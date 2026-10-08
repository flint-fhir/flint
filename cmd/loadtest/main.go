package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/proto"

	condpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/condition_go_proto"
	encpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/encounter_go_proto"
	obspb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/observation_go_proto"
	patpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/patient_go_proto"
	pracpb "github.com/google/fhir/go/proto/google/fhir/proto/r4/core/resources/practitioner_go_proto"

	"github.com/flint-fhir/flint/ingest/workflow"
	"github.com/flint-fhir/flint/store/postgres"
)

type Config struct {
	Target      string        // "http", "store", "temporal"
	URL         string        // e.g. "http://localhost:8080/fhir/r4/loadtest"
	Rate        int           // target bundles/sec (0 = unthrottled benchmark)
	Duration    time.Duration // test duration
	Workers     int           // number of concurrent workers
	BundleSize  int           // number of entries per bundle (1-5)
	Tenant      string        // tenant ID
	PGDSN       string        // Postgres DSN (for store target)
	TemporalAddr string       // Temporal host:port (for temporal target)
}

type Metrics struct {
	totalBundles   atomic.Int64
	totalResources atomic.Int64
	status2xx      atomic.Int64
	status4xx      atomic.Int64
	status5xx      atomic.Int64
	totalErrors    atomic.Int64

	mu         sync.Mutex
	latencies  []time.Duration
}

func (m *Metrics) recordLatency(d time.Duration) {
	m.mu.Lock()
	if len(m.latencies) < 500000 { // cap in-memory sample size
		m.latencies = append(m.latencies, d)
	}
	m.mu.Unlock()
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.Target, "target", "http", "Target mode: http, store, temporal")
	flag.StringVar(&cfg.URL, "url", "http://localhost:8082/fhir/r4/loadtest", "Flint HTTP server bundle URL")
	flag.IntVar(&cfg.Rate, "rate", 1000, "Target bundles/second (0 for maximum uncapped throughput)")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "Test run duration (e.g. 30s, 5m, 1h)")
	flag.IntVar(&cfg.Workers, "workers", 50, "Number of concurrent workers")
	flag.IntVar(&cfg.BundleSize, "bundle-size", 5, "Number of Big 5 resources per Bundle (1-5)")
	flag.StringVar(&cfg.Tenant, "tenant", "loadtest", "Tenant ID")
	flag.StringVar(&cfg.PGDSN, "pg-dsn", "postgres://flint:flint@localhost:5432/flint?sslmode=disable", "Postgres DSN")
	flag.StringVar(&cfg.TemporalAddr, "temporal-addr", "localhost:7233", "Temporal service address")
	flag.Parse()

	fmt.Println("==================================================")
	fmt.Println("⚡ Flint High-Throughput Load Testing Harness")
	fmt.Printf("   Target:      %s\n", cfg.Target)
	if cfg.Target == "http" {
		fmt.Printf("   Endpoint:    %s\n", cfg.URL)
	}
	fmt.Printf("   Target Rate: %d bundles/sec\n", cfg.Rate)
	fmt.Printf("   Duration:    %s\n", cfg.Duration)
	fmt.Printf("   Workers:     %d\n", cfg.Workers)
	fmt.Printf("   Bundle Size: %d resources/bundle\n", cfg.BundleSize)
	fmt.Println("==================================================")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ctx, runCancel := context.WithTimeout(ctx, cfg.Duration)
	defer runCancel()

	metrics := &Metrics{
		latencies: make([]time.Duration, 0, 100000),
	}

	start := time.Now()

	switch cfg.Target {
	case "http":
		runHTTPLoad(ctx, cfg, metrics)
	case "store":
		runStoreLoad(ctx, cfg, metrics)
	case "temporal":
		runTemporalLoad(ctx, cfg, metrics)
	default:
		fmt.Printf("Unknown target mode: %s\n", cfg.Target)
		os.Exit(1)
	}

	elapsed := time.Since(start)
	printReport(cfg, metrics, elapsed)
}

func runHTTPLoad(ctx context.Context, cfg Config, m *Metrics) {
	transport := &http.Transport{
		MaxIdleConns:        500,
		MaxIdleConnsPerHost: cfg.Workers * 2,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	// Pre-generate raw bundle templates
	templates := generateBundlePayloads(100, cfg.BundleSize)

	jobs := make(chan []byte, cfg.Workers*4)
	var wg sync.WaitGroup

	// Workers
	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case payload, ok := <-jobs:
					if !ok {
						return
					}
					t0 := time.Now()
					req, err := http.NewRequestWithContext(ctx, "POST", cfg.URL, bytes.NewReader(payload))
					if err != nil {
						m.totalErrors.Add(1)
						continue
					}
					req.Header.Set("Content-Type", "application/fhir+json")

					resp, err := client.Do(req)
					lat := time.Since(t0)
					if err != nil {
						if ctx.Err() == nil {
							m.totalErrors.Add(1)
						}
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()

					m.recordLatency(lat)
					m.totalBundles.Add(1)
					m.totalResources.Add(int64(cfg.BundleSize))

					if resp.StatusCode >= 200 && resp.StatusCode < 300 {
						m.status2xx.Add(1)
					} else if resp.StatusCode >= 400 && resp.StatusCode < 500 {
						m.status4xx.Add(1)
					} else {
						m.status5xx.Add(1)
					}
				}
			}
		}(w)
	}

	// Rate limiter generator
	go func() {
		defer close(jobs)
		var limiter *time.Ticker
		if cfg.Rate > 0 {
			limiter = time.NewTicker(time.Second / time.Duration(cfg.Rate))
			defer limiter.Stop()
		}

		idx := 0
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter.C:
					}
				}
				payload := templates[idx%len(templates)]
				idx++
				select {
				case <-ctx.Done():
					return
				case jobs <- payload:
				}
			}
		}
	}()

	// Periodic reporter
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	lastBundles := int64(0)
	lastTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case now := <-ticker.C:
			cur := m.totalBundles.Load()
			delta := cur - lastBundles
			dt := now.Sub(lastTime).Seconds()
			bps := float64(delta) / dt
			rps := bps * float64(cfg.BundleSize)
			fmt.Printf("   [%s] Current: %6.1f bundles/s (%7.1f res/s) | Total: %d bundles | 2xx: %d | 4xx: %d | 5xx: %d | Err: %d\n",
				now.Format("15:04:05"), bps, rps, cur, m.status2xx.Load(), m.status4xx.Load(), m.status5xx.Load(), m.totalErrors.Load())
			lastBundles = cur
			lastTime = now
		}
	}
}

func runStoreLoad(ctx context.Context, cfg Config, m *Metrics) {
	db, err := sql.Open("postgres", cfg.PGDSN)
	if err != nil {
		fmt.Printf("Failed to open Postgres: %v\n", err)
		return
	}
	defer db.Close()
	db.SetMaxOpenConns(cfg.Workers * 2)
	db.SetMaxIdleConns(cfg.Workers)

	store := postgres.New(db)
	extractors := postgres.DefaultIndexExtractors()

	// Pre-create protos
	patProto, _ := proto.Marshal(&patpb.Patient{})
	condProto, _ := proto.Marshal(&condpb.Condition{})
	encProto, _ := proto.Marshal(&encpb.Encounter{})
	obsProto, _ := proto.Marshal(&obspb.Observation{})
	pracProto, _ := proto.Marshal(&pracpb.Practitioner{})

	types := []string{"Patient", "Condition", "Encounter", "Observation", "Practitioner"}
	protos := [][]byte{patProto, condProto, encProto, obsProto, pracProto}

	jobs := make(chan int64, cfg.Workers*4)
	var wg sync.WaitGroup

	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for seq := range jobs {
				bundleID := fmt.Sprintf("bench-b-%d", seq)
				inputs := make([]postgres.ResourceInput, cfg.BundleSize)
				for i := 0; i < cfg.BundleSize; i++ {
					rt := types[i%len(types)]
					resID := fmt.Sprintf("bench-%s-%d-%d", rt, seq, i)
					pBytes := protos[i%len(protos)]
					var idx *postgres.SearchIndexes
					if ext, ok := extractors[rt]; ok {
						idx, _ = ext(cfg.Tenant, resID, pBytes)
					}
					inputs[i] = postgres.ResourceInput{
						TenantID:       cfg.Tenant,
						ResType:        rt,
						ResID:          resID,
						ResourceProto:  pBytes,
						SearchIndexes:  idx,
						IdempotencyKey: bundleID,
					}
				}

				t0 := time.Now()
				err := store.WriteBatch(ctx, inputs)
				lat := time.Since(t0)

				if err != nil {
					if ctx.Err() == nil {
						m.totalErrors.Add(1)
					}
				} else {
					m.recordLatency(lat)
					m.totalBundles.Add(1)
					m.totalResources.Add(int64(cfg.BundleSize))
					m.status2xx.Add(1)
				}
			}
		}(w)
	}

	go func() {
		defer close(jobs)
		var seq int64
		var limiter *time.Ticker
		if cfg.Rate > 0 {
			limiter = time.NewTicker(time.Second / time.Duration(cfg.Rate))
			defer limiter.Stop()
		}
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter.C:
					}
				}
				seq++
				select {
				case <-ctx.Done():
					return
				case jobs <- seq:
				}
			}
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	lastBundles := int64(0)
	lastTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case now := <-ticker.C:
			cur := m.totalBundles.Load()
			delta := cur - lastBundles
			dt := now.Sub(lastTime).Seconds()
			bps := float64(delta) / dt
			rps := bps * float64(cfg.BundleSize)
			fmt.Printf("   [%s] Current: %6.1f bundles/s (%7.1f res/s) | Total: %d bundles | Success: %d | Err: %d\n",
				now.Format("15:04:05"), bps, rps, cur, m.status2xx.Load(), m.totalErrors.Load())
			lastBundles = cur
			lastTime = now
		}
	}
}

func runTemporalLoad(ctx context.Context, cfg Config, m *Metrics) {
	c, err := client.Dial(client.Options{HostPort: cfg.TemporalAddr})
	if err != nil {
		fmt.Printf("Failed to connect to Temporal: %v\n", err)
		return
	}
	defer c.Close()

	jobs := make(chan int64, cfg.Workers*4)
	var wg sync.WaitGroup

	patProto, _ := proto.Marshal(&patpb.Patient{})

	for w := 0; w < cfg.Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for seq := range jobs {
				bundleID := fmt.Sprintf("bench-wf-%d-%d", workerID, seq)
				input := workflow.IngestInput{
					TenantID:       cfg.Tenant,
					BundleID:       bundleID,
					ResourceProtos: [][]byte{patProto},
					ResourceTypes:  []string{"Patient"},
					ResourceIDs:    []string{fmt.Sprintf("bench-pat-%d", seq)},
				}

				t0 := time.Now()
				we, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
					ID:        bundleID,
					TaskQueue: workflow.TaskQueue,
				}, workflow.IngestFHIRBundle, input)
				if err != nil {
					m.totalErrors.Add(1)
					continue
				}

				err = we.Get(ctx, nil)
				lat := time.Since(t0)
				m.recordLatency(lat)
				m.totalBundles.Add(1)
				m.totalResources.Add(1)
				if err != nil {
					m.totalErrors.Add(1)
				} else {
					m.status2xx.Add(1)
				}
			}
		}(w)
	}

	go func() {
		defer close(jobs)
		var seq int64
		var limiter *time.Ticker
		if cfg.Rate > 0 {
			limiter = time.NewTicker(time.Second / time.Duration(cfg.Rate))
			defer limiter.Stop()
		}
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if limiter != nil {
					select {
					case <-ctx.Done():
						return
					case <-limiter.C:
					}
				}
				seq++
				select {
				case <-ctx.Done():
					return
				case jobs <- seq:
				}
			}
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	lastBundles := int64(0)
	lastTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case now := <-ticker.C:
			cur := m.totalBundles.Load()
			delta := cur - lastBundles
			dt := now.Sub(lastTime).Seconds()
			bps := float64(delta) / dt
			fmt.Printf("   [%s] Current: %6.1f wf/s | Total: %d | Success: %d | Err: %d\n",
				now.Format("15:04:05"), bps, cur, m.status2xx.Load(), m.totalErrors.Load())
			lastBundles = cur
			lastTime = now
		}
	}
}

func generateBundlePayloads(count, bundleSize int) [][]byte {
	payloads := make([][]byte, count)
	for i := 0; i < count; i++ {
		entries := make([]map[string]any, 0, bundleSize)
		// 1. Patient
		entries = append(entries, map[string]any{
			"resource": map[string]any{
				"resourceType": "Patient",
				"id":           map[string]string{"value": fmt.Sprintf("pat-%d", i)},
				"name": []map[string]any{{
					"family": map[string]string{"value": fmt.Sprintf("Smith%d", i)},
					"given":  []map[string]string{{"value": "Jane"}},
				}},
				"gender": map[string]string{"value": "female"},
			},
			"request": map[string]string{"method": "PUT", "url": fmt.Sprintf("Patient/pat-%d", i)},
		})

		// 2. Practitioner
		if bundleSize >= 2 {
			entries = append(entries, map[string]any{
				"resource": map[string]any{
					"resourceType": "Practitioner",
					"id":           map[string]string{"value": fmt.Sprintf("prac-%d", i)},
					"name": []map[string]any{{
						"family": map[string]string{"value": "House"},
						"given":  []map[string]string{{"value": "Gregory"}},
					}},
				},
				"request": map[string]string{"method": "PUT", "url": fmt.Sprintf("Practitioner/prac-%d", i)},
			})
		}

		// 3. Encounter
		if bundleSize >= 3 {
			entries = append(entries, map[string]any{
				"resource": map[string]any{
					"resourceType": "Encounter",
					"id":           map[string]string{"value": fmt.Sprintf("enc-%d", i)},
					"status":       map[string]string{"value": "finished"},
					"class": map[string]any{
						"code": map[string]string{"value": "AMB"},
					},
					"subject": map[string]any{
						"patientId": map[string]string{"value": fmt.Sprintf("pat-%d", i)},
					},
				},
				"request": map[string]string{"method": "PUT", "url": fmt.Sprintf("Encounter/enc-%d", i)},
			})
		}

		// 4. Condition
		if bundleSize >= 4 {
			entries = append(entries, map[string]any{
				"resource": map[string]any{
					"resourceType": "Condition",
					"id":           map[string]string{"value": fmt.Sprintf("cond-%d", i)},
					"subject": map[string]any{
						"patientId": map[string]string{"value": fmt.Sprintf("pat-%d", i)},
					},
					"code": map[string]any{
						"coding": []map[string]any{{
							"system": map[string]string{"value": "http://hl7.org/fhir/sid/icd-10"},
							"code":   map[string]string{"value": "I10"},
						}},
					},
				},
				"request": map[string]string{"method": "PUT", "url": fmt.Sprintf("Condition/cond-%d", i)},
			})
		}

		// 5. Observation
		if bundleSize >= 5 {
			entries = append(entries, map[string]any{
				"resource": map[string]any{
					"resourceType": "Observation",
					"id":           map[string]string{"value": fmt.Sprintf("obs-%d", i)},
					"status":       map[string]string{"value": "final"},
					"subject": map[string]any{
						"patientId": map[string]string{"value": fmt.Sprintf("pat-%d", i)},
					},
					"code": map[string]any{
						"coding": []map[string]any{{
							"system": map[string]string{"value": "http://loinc.org"},
							"code":   map[string]string{"value": "8867-4"},
						}},
					},
				},
				"request": map[string]string{"method": "PUT", "url": fmt.Sprintf("Observation/obs-%d", i)},
			})
		}

		bundle := map[string]any{
			"resourceType": "Bundle",
			"type":         "transaction",
			"entry":        entries,
		}
		b, _ := json.Marshal(bundle)
		payloads[i] = b
	}
	return payloads
}

func printReport(cfg Config, m *Metrics, elapsed time.Duration) {
	total := m.totalBundles.Load()
	resTotal := m.totalResources.Load()
	secs := elapsed.Seconds()
	avgBps := float64(total) / secs
	avgRps := float64(resTotal) / secs

	m.mu.Lock()
	lats := make([]time.Duration, len(m.latencies))
	copy(lats, m.latencies)
	m.mu.Unlock()

	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })

	var p50, p90, p95, p99, minLat, maxLat time.Duration
	if len(lats) > 0 {
		minLat = lats[0]
		maxLat = lats[len(lats)-1]
		p50 = lats[len(lats)*50/100]
		p90 = lats[len(lats)*90/100]
		p95 = lats[len(lats)*95/100]
		p99 = lats[len(lats)*99/100]
	}

	fmt.Println("\n==================================================")
	fmt.Println("📊 Flint Load Test Summary Report")
	fmt.Println("==================================================")
	fmt.Printf("Duration:         %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Total Bundles:    %d\n", total)
	fmt.Printf("Total Resources:  %d\n", resTotal)
	fmt.Printf("Avg Throughput:   %.1f bundles/sec (%.1f resources/sec)\n", avgBps, avgRps)
	fmt.Printf("Success (2xx):    %d (%.2f%%)\n", m.status2xx.Load(), ratio(m.status2xx.Load(), total))
	fmt.Printf("Client Err (4xx): %d\n", m.status4xx.Load())
	fmt.Printf("Server Err (5xx): %d\n", m.status5xx.Load())
	fmt.Printf("Transport Errors: %d\n", m.totalErrors.Load())
	fmt.Println("--------------------------------------------------")
	fmt.Println("Latency Distribution:")
	fmt.Printf("  Min:   %s\n", minLat)
	fmt.Printf("  p50:   %s\n", p50)
	fmt.Printf("  p90:   %s\n", p90)
	fmt.Printf("  p95:   %s\n", p95)
	fmt.Printf("  p99:   %s\n", p99)
	fmt.Printf("  Max:   %s\n", maxLat)
	fmt.Println("==================================================")
}

func ratio(num, den int64) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}
