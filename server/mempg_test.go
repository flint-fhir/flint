package server_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flint-fhir/flint/store/postgres"
)

var (
	memPGOnce   sync.Once
	globalMemPG = newMemPGStore()
)

func canUseMemPGFallback(testName string) bool {
	for _, skipPrefix := range []string{
		"TestServer_Integration",
		"TestServer_CriticalSecurityAndSearchHardening",
		"TestHistoryAtListCompartmentsAndDeleteIfMatchWave12",
		"TestFilterUCUMContentBulkUntilAndGroupEverythingWave13",
	} {
		if strings.HasPrefix(testName, skipPrefix) {
			return false
		}
	}
	return true
}

func openMemPostgres(t *testing.T) *sql.DB {
	t.Helper()
	memPGOnce.Do(func() {
		sql.Register("flintmempg", &memPGDriver{store: globalMemPG})
	})
	db, err := sql.Open("flintmempg", "mem")
	if err != nil {
		t.Fatalf("open flintmempg: %v", err)
	}
	return db
}

type memResRecord struct {
	tenantID    string
	resType     string
	resID       string
	version     int
	lastUpdated time.Time
	protoBytes  []byte
	isDeleted   bool
}

type memExportJobRec struct {
	jobID      string
	tenantID   string
	patientID  string
	requestURL string
	createdAt  time.Time
	typesOrder string
	counts     string
	filesJSON  string
	combined   []byte
}

type memAuditRec struct {
	tenantID      string
	auditID       string
	recorded      time.Time
	action        string
	subtypeCode   string
	outcome       string
	outcomeDesc   string
	httpMethod    string
	httpStatus    int64
	requestURI    string
	agentSubject  string
	agentPatient  string
	clientIP      string
	entityType    string
	entityID      string
	entityVersion string
}

type memPGStore struct {
	mu            sync.RWMutex
	resources     map[string]*memResRecord
	history       []memResRecord
	strings       []postgres.SpidxString
	tokens        []postgres.SpidxToken
	dates         []postgres.SpidxDate
	refs          []postgres.SpidxReference
	quants        []postgres.SpidxQuantity
	uris          []postgres.SpidxURI
	exportJobs    map[string]*memExportJobRec
	authCodes     map[string][]driver.Value
	refreshTokens map[string][]driver.Value
	revokedTokens map[string]bool
	audits        []memAuditRec
}

func newMemPGStore() *memPGStore {
	return &memPGStore{
		resources:     make(map[string]*memResRecord),
		exportJobs:    make(map[string]*memExportJobRec),
		authCodes:     make(map[string][]driver.Value),
		refreshTokens: make(map[string][]driver.Value),
		revokedTokens: make(map[string]bool),
	}
}

func memResKey(tenant, resType, resID string) string {
	return tenant + "|" + resType + "|" + resID
}

type memPGDriver struct {
	store *memPGStore
}

func (d *memPGDriver) Open(_ string) (driver.Conn, error) {
	return &memPGConn{store: d.store}, nil
}

type memPGConn struct {
	store *memPGStore
}

func (c *memPGConn) Prepare(_ string) (driver.Stmt, error) { return nil, errors.New("unsupported") }
func (c *memPGConn) Close() error                          { return nil }
func (c *memPGConn) Begin() (driver.Tx, error)             { return c.store.beginTx(), nil }
func (c *memPGConn) Ping(_ context.Context) error          { return nil }

type memPGTx struct {
	store     *memPGStore
	resources map[string]*memResRecord
	history   []memResRecord
	strings   []postgres.SpidxString
	tokens    []postgres.SpidxToken
	dates     []postgres.SpidxDate
	refs      []postgres.SpidxReference
	quants    []postgres.SpidxQuantity
	uris      []postgres.SpidxURI
	done      bool
}

func (s *memPGStore) beginTx() *memPGTx {
	s.mu.Lock()
	defer s.mu.Unlock()
	resSnap := make(map[string]*memResRecord, len(s.resources))
	for k, v := range s.resources {
		if v != nil {
			cp := *v
			resSnap[k] = &cp
		}
	}
	return &memPGTx{
		store:     s,
		resources: resSnap,
		history:   append([]memResRecord(nil), s.history...),
		strings:   append([]postgres.SpidxString(nil), s.strings...),
		tokens:    append([]postgres.SpidxToken(nil), s.tokens...),
		dates:     append([]postgres.SpidxDate(nil), s.dates...),
		refs:      append([]postgres.SpidxReference(nil), s.refs...),
		quants:    append([]postgres.SpidxQuantity(nil), s.quants...),
		uris:      append([]postgres.SpidxURI(nil), s.uris...),
	}
}

func (t *memPGTx) Commit() error {
	t.done = true
	return nil
}

func (t *memPGTx) Rollback() error {
	if t.done || t.store == nil {
		return nil
	}
	t.done = true
	s := t.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources = t.resources
	s.history = t.history
	s.strings = t.strings
	s.tokens = t.tokens
	s.dates = t.dates
	s.refs = t.refs
	s.quants = t.quants
	s.uris = t.uris
	return nil
}

type memPGRows struct {
	cols []string
	rows [][]driver.Value
	idx  int
}

func (r *memPGRows) Columns() []string { return r.cols }
func (r *memPGRows) Close() error      { return nil }
func (r *memPGRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.idx])
	r.idx++
	return nil
}

func nvStr(args []driver.NamedValue, i int) string {
	if i >= len(args) || args[i].Value == nil {
		return ""
	}
	switch v := args[i].Value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}

func nvBytes(args []driver.NamedValue, i int) []byte {
	if i >= len(args) || args[i].Value == nil {
		return nil
	}
	switch v := args[i].Value.(type) {
	case []byte:
		out := make([]byte, len(v))
		copy(out, v)
		return out
	case string:
		return []byte(v)
	default:
		return nil
	}
}

func nvInt(args []driver.NamedValue, i int) int {
	if i >= len(args) || args[i].Value == nil {
		return 0
	}
	switch v := args[i].Value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	default:
		return 0
	}
}

func nvFloat(args []driver.NamedValue, i int) float64 {
	if i >= len(args) || args[i].Value == nil {
		return 0
	}
	switch v := args[i].Value.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	default:
		return 0
	}
}

func nvTime(args []driver.NamedValue, i int) time.Time {
	if i >= len(args) || args[i].Value == nil {
		return time.Now().UTC()
	}
	if t, ok := args[i].Value.(time.Time); ok {
		return t.UTC()
	}
	return time.Now().UTC()
}

func (c *memPGConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	q := strings.ReplaceAll(strings.TrimSpace(query), "public.", "")
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.HasPrefix(q, "DELETE FROM fhir_resource WHERE tenant_id = $1") && !strings.Contains(q, "res_type"):
		tenant := nvStr(args, 0)
		for k, r := range s.resources {
			if r.tenantID == tenant {
				delete(s.resources, k)
			}
		}
		keptHist := s.history[:0]
		for _, h := range s.history {
			if h.tenantID != tenant {
				keptHist = append(keptHist, h)
			}
		}
		s.history = keptHist
		keptAudits := s.audits[:0]
		for _, a := range s.audits {
			if a.tenantID != tenant {
				keptAudits = append(keptAudits, a)
			}
		}
		s.audits = keptAudits
		for k, j := range s.exportJobs {
			if j.tenantID == tenant {
				delete(s.exportJobs, k)
			}
		}
		s.deleteSpidxForTenant(tenant)
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "DELETE FROM fhir_resource") && strings.Contains(q, "res_type = $2") && strings.Contains(q, "res_id = $3"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		key := memResKey(tenant, resType, resID)
		if _, ok := s.resources[key]; ok {
			delete(s.resources, key)
			return driver.RowsAffected(1), nil
		}
		return driver.RowsAffected(0), nil

	case strings.Contains(q, "DELETE FROM fhir_resource_history") && strings.Contains(q, "res_type = $2") && strings.Contains(q, "res_id = $3"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		removed := 0
		kept := s.history[:0]
		for _, h := range s.history {
			if h.tenantID == tenant && h.resType == resType && h.resID == resID {
				removed++
			} else {
				kept = append(kept, h)
			}
		}
		s.history = kept
		return driver.RowsAffected(removed), nil

	case strings.HasPrefix(q, "DELETE FROM fhir_resource_history h USING fhir_resource r"):
		tenant := nvStr(args, 0)
		removed := 0
		kept := s.history[:0]
		for _, h := range s.history {
			cur := s.resources[memResKey(h.tenantID, h.resType, h.resID)]
			if h.tenantID == tenant && cur != nil && h.version < cur.version {
				removed++
			} else {
				kept = append(kept, h)
			}
		}
		s.history = kept
		return driver.RowsAffected(removed), nil

	case strings.Contains(q, "INSERT INTO fhir_resource_history"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		ver := nvInt(args, 3)
		lu := nvTime(args, 4)
		var pb []byte
		isDel := strings.Contains(q, "TRUE")
		if len(args) >= 6 {
			pb = nvBytes(args, 5)
		} else if cur := s.resources[memResKey(tenant, resType, resID)]; cur != nil {
			pb = cur.protoBytes
		}
		s.history = append(s.history, memResRecord{
			tenantID:    tenant,
			resType:     resType,
			resID:       resID,
			version:     ver,
			lastUpdated: lu,
			protoBytes:  pb,
			isDeleted:   isDel,
		})
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM spidx_string WHERE tenant_id = $1"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		prefix := resType + "/" + resID + "#"
		kept := s.strings[:0]
		for _, r := range s.strings {
			if r.TenantID == tenant && ((r.ResType == resType && r.ResID == resID) || strings.HasPrefix(r.ResID, prefix)) {
				continue
			}
			kept = append(kept, r)
		}
		s.strings = kept
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM spidx_token WHERE tenant_id = $1"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		prefix := resType + "/" + resID + "#"
		kept := s.tokens[:0]
		for _, r := range s.tokens {
			if r.TenantID == tenant && ((r.ResType == resType && r.ResID == resID) || strings.HasPrefix(r.ResID, prefix)) {
				continue
			}
			kept = append(kept, r)
		}
		s.tokens = kept
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM spidx_date WHERE tenant_id = $1"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		prefix := resType + "/" + resID + "#"
		kept := s.dates[:0]
		for _, r := range s.dates {
			if r.TenantID == tenant && ((r.ResType == resType && r.ResID == resID) || strings.HasPrefix(r.ResID, prefix)) {
				continue
			}
			kept = append(kept, r)
		}
		s.dates = kept
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM spidx_reference WHERE tenant_id = $1"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		prefix := resType + "/" + resID + "#"
		kept := s.refs[:0]
		for _, r := range s.refs {
			if r.TenantID == tenant && ((r.ResType == resType && r.ResID == resID) || strings.HasPrefix(r.ResID, prefix)) {
				continue
			}
			kept = append(kept, r)
		}
		s.refs = kept
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM spidx_quantity WHERE tenant_id = $1"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		prefix := resType + "/" + resID + "#"
		kept := s.quants[:0]
		for _, r := range s.quants {
			if r.TenantID == tenant && ((r.ResType == resType && r.ResID == resID) || strings.HasPrefix(r.ResID, prefix)) {
				continue
			}
			kept = append(kept, r)
		}
		s.quants = kept
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "DELETE FROM spidx_uri WHERE tenant_id = $1"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		prefix := resType + "/" + resID + "#"
		kept := s.uris[:0]
		for _, r := range s.uris {
			if r.TenantID == tenant && ((r.ResType == resType && r.ResID == resID) || strings.HasPrefix(r.ResID, prefix)) {
				continue
			}
			kept = append(kept, r)
		}
		s.uris = kept
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO spidx_string"):
		for i := 0; i+4 < len(args); i += 5 {
			s.strings = append(s.strings, postgres.SpidxString{
				TenantID: nvStr(args, i),
				ResType:  nvStr(args, i+1),
				ResID:    nvStr(args, i+2),
				SpName:   nvStr(args, i+3),
				SpValue:  nvStr(args, i+4),
			})
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO spidx_token"):
		for i := 0; i+5 < len(args); i += 6 {
			s.tokens = append(s.tokens, postgres.SpidxToken{
				TenantID: nvStr(args, i),
				ResType:  nvStr(args, i+1),
				ResID:    nvStr(args, i+2),
				SpName:   nvStr(args, i+3),
				SpSystem: nvStr(args, i+4),
				SpValue:  nvStr(args, i+5),
			})
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO spidx_date"):
		for i := 0; i+5 < len(args); i += 6 {
			s.dates = append(s.dates, postgres.SpidxDate{
				TenantID: nvStr(args, i),
				ResType:  nvStr(args, i+1),
				ResID:    nvStr(args, i+2),
				SpName:   nvStr(args, i+3),
				SpLow:    nvTime(args, i+4),
				SpHigh:   nvTime(args, i+5),
			})
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO spidx_reference"):
		for i := 0; i+5 < len(args); i += 6 {
			s.refs = append(s.refs, postgres.SpidxReference{
				TenantID:   nvStr(args, i),
				ResType:    nvStr(args, i+1),
				ResID:      nvStr(args, i+2),
				SpName:     nvStr(args, i+3),
				TargetType: nvStr(args, i+4),
				TargetID:   nvStr(args, i+5),
			})
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO spidx_quantity"):
		for i := 0; i+6 < len(args); i += 7 {
			s.quants = append(s.quants, postgres.SpidxQuantity{
				TenantID: nvStr(args, i),
				ResType:  nvStr(args, i+1),
				ResID:    nvStr(args, i+2),
				SpName:   nvStr(args, i+3),
				SpSystem: nvStr(args, i+4),
				SpCode:   nvStr(args, i+5),
				SpValue:  nvFloat(args, i+6),
			})
		}
		return driver.RowsAffected(1), nil

	case strings.HasPrefix(q, "INSERT INTO spidx_uri"):
		for i := 0; i+4 < len(args); i += 5 {
			s.uris = append(s.uris, postgres.SpidxURI{
				TenantID: nvStr(args, i),
				ResType:  nvStr(args, i+1),
				ResID:    nvStr(args, i+2),
				SpName:   nvStr(args, i+3),
				SpValue:  nvStr(args, i+4),
			})
		}
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "INSERT INTO fhir_export_job"):
		job := &memExportJobRec{
			jobID:      nvStr(args, 0),
			tenantID:   nvStr(args, 1),
			patientID:  nvStr(args, 2),
			requestURL: nvStr(args, 3),
			createdAt:  nvTime(args, 4),
			typesOrder: nvStr(args, 5),
			counts:     nvStr(args, 6),
			filesJSON:  nvStr(args, 7),
			combined:   nvBytes(args, 8),
		}
		s.exportJobs[job.tenantID+"|"+job.jobID] = job
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "DELETE FROM fhir_export_job"):
		key := nvStr(args, 0) + "|" + nvStr(args, 1)
		if _, ok := s.exportJobs[key]; ok {
			delete(s.exportJobs, key)
			return driver.RowsAffected(1), nil
		}
		return driver.RowsAffected(0), nil

	case strings.Contains(q, "INSERT INTO fhir_oauth_auth_code"):
		h := nvStr(args, 0)
		s.authCodes[h] = []driver.Value{
			nvStr(args, 1), nvStr(args, 2), nvStr(args, 3), nvStr(args, 4), nvStr(args, 5),
			nvStr(args, 6), nvStr(args, 7), nvStr(args, 8), nvStr(args, 9), nvTime(args, 10),
		}
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "INSERT INTO fhir_oauth_refresh_token"):
		h := nvStr(args, 0)
		s.refreshTokens[h] = []driver.Value{
			nvStr(args, 1), nvStr(args, 2), nvStr(args, 3), nvStr(args, 4), nvTime(args, 5),
		}
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "DELETE FROM fhir_oauth_refresh_token WHERE token_hash = $1"):
		delete(s.refreshTokens, nvStr(args, 0))
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "INSERT INTO fhir_oauth_revoked_token"):
		s.revokedTokens[nvStr(args, 0)] = true
		return driver.RowsAffected(1), nil

	case strings.Contains(q, "INSERT INTO fhir_audit_event"):
		if len(args) >= 16 {
			s.audits = append(s.audits, memAuditRec{
				tenantID:      nvStr(args, 0),
				auditID:       nvStr(args, 1),
				recorded:      nvTime(args, 2),
				action:        nvStr(args, 3),
				subtypeCode:   nvStr(args, 4),
				outcome:       nvStr(args, 5),
				outcomeDesc:   nvStr(args, 6),
				httpMethod:    nvStr(args, 7),
				httpStatus:    int64(nvInt(args, 8)),
				requestURI:    nvStr(args, 9),
				agentSubject:  nvStr(args, 10),
				agentPatient:  nvStr(args, 11),
				clientIP:      nvStr(args, 12),
				entityType:    nvStr(args, 13),
				entityID:      nvStr(args, 14),
				entityVersion: nvStr(args, 15),
			})
		}
		return driver.RowsAffected(1), nil
	}

	return driver.RowsAffected(1), nil
}

func (s *memPGStore) deleteSpidxForTenant(tenant string) {
	keptS := s.strings[:0]
	for _, r := range s.strings {
		if r.TenantID != tenant {
			keptS = append(keptS, r)
		}
	}
	s.strings = keptS

	keptT := s.tokens[:0]
	for _, r := range s.tokens {
		if r.TenantID != tenant {
			keptT = append(keptT, r)
		}
	}
	s.tokens = keptT

	keptD := s.dates[:0]
	for _, r := range s.dates {
		if r.TenantID != tenant {
			keptD = append(keptD, r)
		}
	}
	s.dates = keptD

	keptR := s.refs[:0]
	for _, r := range s.refs {
		if r.TenantID != tenant {
			keptR = append(keptR, r)
		}
	}
	s.refs = keptR

	keptQ := s.quants[:0]
	for _, r := range s.quants {
		if r.TenantID != tenant {
			keptQ = append(keptQ, r)
		}
	}
	s.quants = keptQ

	keptU := s.uris[:0]
	for _, r := range s.uris {
		if r.TenantID != tenant {
			keptU = append(keptU, r)
		}
	}
	s.uris = keptU
}

func (c *memPGConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q := strings.TrimSpace(query)
	s := c.store
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()

	switch {
	case strings.Contains(q, "SELECT version FROM schema_migrations"):
		return &memPGRows{cols: []string{"version"}}, nil

	case strings.Contains(q, "SELECT res_version, is_deleted") && strings.Contains(q, "FROM fhir_resource"):
		tenant := nvStr(args, 0)
		var resType, resID string
		if strings.Contains(q, "res_type = 'Patient'") {
			resType = "Patient"
			resID = nvStr(args, 1)
		} else {
			resType = nvStr(args, 1)
			resID = nvStr(args, 2)
		}
		rec := s.resources[memResKey(tenant, resType, resID)]
		if rec == nil {
			return &memPGRows{cols: []string{"res_version", "is_deleted"}}, nil
		}
		return &memPGRows{
			cols: []string{"res_version", "is_deleted"},
			rows: [][]driver.Value{{int64(rec.version), rec.isDeleted}},
		}, nil

	case strings.Contains(q, "SELECT res_version, last_updated, is_deleted") && strings.Contains(q, "FOR UPDATE"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		rec := s.resources[memResKey(tenant, resType, resID)]
		if rec == nil {
			return &memPGRows{cols: []string{"res_version", "last_updated", "is_deleted"}}, nil
		}
		return &memPGRows{
			cols: []string{"res_version", "last_updated", "is_deleted"},
			rows: [][]driver.Value{{int64(rec.version), rec.lastUpdated, rec.isDeleted}},
		}, nil

	case strings.Contains(q, "INSERT INTO fhir_resource") && strings.Contains(q, "RETURNING res_version, last_updated"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		pb := nvBytes(args, 3)
		key := memResKey(tenant, resType, resID)
		ver := 1
		if cur := s.resources[key]; cur != nil {
			ver = cur.version + 1
		}
		s.resources[key] = &memResRecord{
			tenantID:    tenant,
			resType:     resType,
			resID:       resID,
			version:     ver,
			lastUpdated: now,
			protoBytes:  pb,
			isDeleted:   false,
		}
		return &memPGRows{
			cols: []string{"res_version", "last_updated"},
			rows: [][]driver.Value{{int64(ver), now}},
		}, nil

	case strings.Contains(q, "UPDATE fhir_resource") && strings.Contains(q, "RETURNING res_version, last_updated"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		newVer := nvInt(args, 3)
		key := memResKey(tenant, resType, resID)
		if cur := s.resources[key]; cur != nil {
			cur.version = newVer
			cur.lastUpdated = now
			cur.isDeleted = true
		}
		return &memPGRows{
			cols: []string{"res_version", "last_updated"},
			rows: [][]driver.Value{{int64(newVer), now}},
		}, nil

	case strings.Contains(q, "SELECT resource_proto FROM fhir_resource"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		rec := s.resources[memResKey(tenant, resType, resID)]
		if rec == nil || rec.isDeleted {
			return &memPGRows{cols: []string{"resource_proto"}}, nil
		}
		return &memPGRows{
			cols: []string{"resource_proto"},
			rows: [][]driver.Value{{rec.protoBytes}},
		}, nil

	case strings.Contains(q, "SELECT resource_proto, res_version, last_updated, is_deleted") && strings.Contains(q, "FROM fhir_resource_history"):
		tenant, resType, resID, ver := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2), nvInt(args, 3)
		for i := len(s.history) - 1; i >= 0; i-- {
			h := s.history[i]
			if h.tenantID == tenant && h.resType == resType && h.resID == resID && h.version == ver {
				return &memPGRows{
					cols: []string{"resource_proto", "res_version", "last_updated", "is_deleted"},
					rows: [][]driver.Value{{h.protoBytes, int64(h.version), h.lastUpdated, h.isDeleted}},
				}, nil
			}
		}
		return &memPGRows{cols: []string{"resource_proto", "res_version", "last_updated", "is_deleted"}}, nil

	case strings.Contains(q, "SELECT resource_proto, res_version, last_updated, is_deleted") && strings.Contains(q, "FROM fhir_resource"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		rec := s.resources[memResKey(tenant, resType, resID)]
		if rec == nil {
			return &memPGRows{cols: []string{"resource_proto", "res_version", "last_updated", "is_deleted"}}, nil
		}
		if len(args) >= 4 && rec.version != nvInt(args, 3) {
			return &memPGRows{cols: []string{"resource_proto", "res_version", "last_updated", "is_deleted"}}, nil
		}
		return &memPGRows{
			cols: []string{"resource_proto", "res_version", "last_updated", "is_deleted"},
			rows: [][]driver.Value{{rec.protoBytes, int64(rec.version), rec.lastUpdated, rec.isDeleted}},
		}, nil

	case strings.Contains(q, "SELECT is_deleted FROM fhir_resource"):
		tenant, resType, resID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2)
		rec := s.resources[memResKey(tenant, resType, resID)]
		if rec == nil {
			return &memPGRows{cols: []string{"is_deleted"}}, nil
		}
		return &memPGRows{
			cols: []string{"is_deleted"},
			rows: [][]driver.Value{{rec.isDeleted}},
		}, nil

	case strings.Contains(q, "SELECT EXISTS ("):
		tenant, resType, resID, patID := nvStr(args, 0), nvStr(args, 1), nvStr(args, 2), nvStr(args, 3)
		if resType == "Patient" && resID == patID {
			return &memPGRows{cols: []string{"exists"}, rows: [][]driver.Value{{true}}}, nil
		}
		for _, rf := range s.refs {
			if rf.TenantID == tenant && rf.ResType == resType && rf.ResID == resID && rf.TargetID == patID && (rf.TargetType == "Patient" || rf.TargetType == "") {
				return &memPGRows{cols: []string{"exists"}, rows: [][]driver.Value{{true}}}, nil
			}
		}
		return &memPGRows{cols: []string{"exists"}, rows: [][]driver.Value{{false}}}, nil

	case strings.Contains(q, "SELECT COUNT(*) FROM fhir_resource_history h"):
		matches := s.filterHistory(q, args)
		return &memPGRows{
			cols: []string{"count"},
			rows: [][]driver.Value{{int64(len(matches))}},
		}, nil

	case strings.Contains(q, "SELECT h.res_type, h.res_id, h.resource_proto, h.res_version, h.last_updated, h.is_deleted FROM fhir_resource_history h"):
		matches := s.filterHistory(q, args)
		rows := make([][]driver.Value, 0, len(matches))
		for _, h := range matches {
			rows = append(rows, []driver.Value{h.resType, h.resID, h.protoBytes, int64(h.version), h.lastUpdated, h.isDeleted})
		}
		return &memPGRows{
			cols: []string{"res_type", "res_id", "resource_proto", "res_version", "last_updated", "is_deleted"},
			rows: rows,
		}, nil

	case strings.Contains(q, "SELECT COUNT(") && strings.Contains(q, "FROM fhir_resource"):
		matches := s.filterResources(q, args, false)
		return &memPGRows{
			cols: []string{"count"},
			rows: [][]driver.Value{{int64(len(matches))}},
		}, nil

	case !strings.Contains(q, "FROM fhir_resource r") && strings.Contains(q, "FROM spidx_reference rf") && strings.Contains(q, "r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto"):
		matches := s.resolveIncludeQuery(q, args)
		rows := make([][]driver.Value, 0, len(matches))
		for _, m := range matches {
			rows = append(rows, []driver.Value{m.resType, m.resID, int64(m.version), m.lastUpdated, m.protoBytes})
		}
		return &memPGRows{
			cols: []string{"res_type", "res_id", "res_version", "last_updated", "resource_proto"},
			rows: rows,
		}, nil

	case strings.Contains(q, "r.res_type, r.res_id, r.res_version, r.last_updated, r.resource_proto"):
		matches := s.filterResources(q, args, true)
		rows := make([][]driver.Value, 0, len(matches))
		for _, m := range matches {
			rows = append(rows, []driver.Value{m.resType, m.resID, int64(m.version), m.lastUpdated, m.protoBytes})
		}
		return &memPGRows{
			cols: []string{"res_type", "res_id", "res_version", "last_updated", "resource_proto"},
			rows: rows,
		}, nil

	case strings.Contains(q, "SELECT r.res_type, r.res_id, r.res_version") && !strings.Contains(q, "r.last_updated"):
		// PurgePatientCompartment query
		tenant, patID := nvStr(args, 0), nvStr(args, 1)
		var rows [][]driver.Value
		for _, r := range s.resources {
			if r.tenantID != tenant || r.resType == "Patient" || r.resType == "Practitioner" || r.resType == "Organization" || r.resType == "Location" {
				continue
			}
			for _, rf := range s.refs {
				if rf.TenantID == tenant && rf.ResType == r.resType && rf.ResID == r.resID && rf.TargetID == patID {
					rows = append(rows, []driver.Value{r.resType, r.resID, int64(r.version)})
					break
				}
			}
		}
		return &memPGRows{
			cols: []string{"res_type", "res_id", "res_version"},
			rows: rows,
		}, nil

	case strings.Contains(q, "SELECT res_type, res_id FROM fhir_resource"):
		tenant := nvStr(args, 0)
		onlyDeleted := strings.Contains(q, "is_deleted = TRUE")
		var rows [][]driver.Value
		for _, r := range s.resources {
			if r.tenantID != tenant {
				continue
			}
			if onlyDeleted && !r.isDeleted {
				continue
			}
			rows = append(rows, []driver.Value{r.resType, r.resID})
		}
		return &memPGRows{
			cols: []string{"res_type", "res_id"},
			rows: rows,
		}, nil

	case strings.Contains(q, "SELECT res_type, res_id, resource_proto FROM fhir_resource"):
		tenant := nvStr(args, 0)
		wantType := ""
		wantID := ""
		if strings.Contains(q, "res_type = $2") {
			wantType = nvStr(args, 1)
			if strings.Contains(q, "res_id = $3") {
				wantID = nvStr(args, 2)
			}
		} else if strings.Contains(q, "res_id = $2") {
			wantID = nvStr(args, 1)
		}
		var rows [][]driver.Value
		for _, r := range s.resources {
			if r.tenantID != tenant || r.isDeleted {
				continue
			}
			if wantType != "" && r.resType != wantType {
				continue
			}
			if wantID != "" && r.resID != wantID {
				continue
			}
			rows = append(rows, []driver.Value{r.resType, r.resID, r.protoBytes})
		}
		return &memPGRows{
			cols: []string{"res_type", "res_id", "resource_proto"},
			rows: rows,
		}, nil

	case strings.Contains(q, "SELECT DISTINCT u.sp_value"):
		tenant := nvStr(args, 0)
		seen := make(map[string]bool)
		var rows [][]driver.Value
		for _, u := range s.uris {
			if u.TenantID == tenant && u.SpName == "_profile" && !seen[u.SpValue] {
				seen[u.SpValue] = true
				rows = append(rows, []driver.Value{u.SpValue})
			}
		}
		return &memPGRows{cols: []string{"sp_value"}, rows: rows}, nil

	case strings.Contains(q, "SELECT DISTINCT t.sp_name, t.sp_system, t.sp_value"):
		tenant := nvStr(args, 0)
		var rows [][]driver.Value
		for _, t := range s.tokens {
			if t.TenantID == tenant && (t.SpName == "_tag" || t.SpName == "_security") {
				rows = append(rows, []driver.Value{t.SpName, t.SpSystem, t.SpValue})
			}
		}
		return &memPGRows{cols: []string{"sp_name", "sp_system", "sp_value"}, rows: rows}, nil

	case strings.Contains(q, "FROM fhir_export_job"):
		key := nvStr(args, 0) + "|" + nvStr(args, 1)
		job := s.exportJobs[key]
		if job == nil {
			return &memPGRows{cols: []string{"job_id", "tenant_id", "patient_id", "request_url", "created_at", "types_order", "counts", "files_json", "combined"}}, nil
		}
		return &memPGRows{
			cols: []string{"job_id", "tenant_id", "patient_id", "request_url", "created_at", "types_order", "counts", "files_json", "combined"},
			rows: [][]driver.Value{{job.jobID, job.tenantID, job.patientID, job.requestURL, job.createdAt, job.typesOrder, job.counts, job.filesJSON, job.combined}},
		}, nil

	case strings.Contains(q, "DELETE FROM fhir_oauth_auth_code"):
		h := nvStr(args, 0)
		vals, ok := s.authCodes[h]
		delete(s.authCodes, h)
		cols := []string{"client_id", "redirect_uri", "scope", "code_challenge", "code_challenge_method", "patient_id", "encounter_id", "tenant_id", "nonce", "expires_at"}
		if !ok {
			return &memPGRows{cols: cols}, nil
		}
		return &memPGRows{cols: cols, rows: [][]driver.Value{vals}}, nil

	case strings.Contains(q, "DELETE FROM fhir_oauth_refresh_token"):
		h := nvStr(args, 0)
		vals, ok := s.refreshTokens[h]
		delete(s.refreshTokens, h)
		cols := []string{"subject", "patient_id", "encounter_id", "scope", "expires_at"}
		if !ok {
			return &memPGRows{cols: cols}, nil
		}
		return &memPGRows{cols: cols, rows: [][]driver.Value{vals}}, nil

	case strings.Contains(q, "SELECT 1 FROM fhir_oauth_revoked_token"):
		h := nvStr(args, 0)
		if s.revokedTokens[h] {
			return &memPGRows{cols: []string{"?column?"}, rows: [][]driver.Value{{int64(1)}}}, nil
		}
		return &memPGRows{cols: []string{"?column?"}}, nil

	case strings.Contains(q, "SELECT COUNT(*) FROM fhir_audit_event"):
		tenant := nvStr(args, 0)
		var cnt int64
		for _, a := range s.audits {
			if a.tenantID == tenant {
				cnt++
			}
		}
		return &memPGRows{
			cols: []string{"count"},
			rows: [][]driver.Value{{cnt}},
		}, nil

	case strings.Contains(q, "FROM fhir_audit_event"):
		tenant := nvStr(args, 0)
		var rows [][]driver.Value
		for i := len(s.audits) - 1; i >= 0; i-- {
			a := s.audits[i]
			if a.tenantID != tenant {
				continue
			}
			rows = append(rows, []driver.Value{
				a.tenantID, a.auditID, a.recorded, a.action, a.subtypeCode, a.outcome, a.outcomeDesc,
				a.httpMethod, a.httpStatus, a.requestURI, a.agentSubject, a.agentPatient,
				a.clientIP, a.entityType, a.entityID, a.entityVersion,
			})
		}
		return &memPGRows{
			cols: []string{
				"tenant_id", "audit_id", "recorded", "action", "subtype_code", "outcome", "outcome_desc",
				"http_method", "http_status", "request_uri", "agent_subject", "agent_patient",
				"client_ip", "entity_type", "entity_id", "entity_version",
			},
			rows: rows,
		}, nil
	}

	return &memPGRows{cols: []string{"col"}}, nil
}

func (s *memPGStore) filterHistory(q string, args []driver.NamedValue) []memResRecord {
	tenant := nvStr(args, 0)
	resType := ""
	resID := ""
	if strings.Contains(q, "h.res_type = $2") {
		resType = nvStr(args, 1)
		if strings.Contains(q, "h.res_id = $3") {
			resID = nvStr(args, 2)
		}
	}
	var out []memResRecord
	for i := len(s.history) - 1; i >= 0; i-- {
		h := s.history[i]
		if h.tenantID != tenant {
			continue
		}
		if resType != "" && h.resType != resType {
			continue
		}
		if resID != "" && h.resID != resID {
			continue
		}
		out = append(out, h)
	}
	return out
}

var (
	reResIDEq      = regexp.MustCompile(`AND r\.res_id = \$(\d+)`)
	reResIDAny     = regexp.MustCompile(`AND r\.res_id = ANY\(\$(\d+)\)`)
	reResTypeAny   = regexp.MustCompile(`r\.res_type = ANY\(\$(\d+)\)`)
	reResTypeNotEq = regexp.MustCompile(`r\.res_type != \$(\d+)`)
	reDateHighGe   = regexp.MustCompile(`d\.sp_high >= \$(\d+)`)
	reDateLowLe    = regexp.MustCompile(`d\.sp_low <= \$(\d+)`)
	reSpNameArg    = regexp.MustCompile(`\.sp_name = \$(\d+)`)
	reArgRef       = regexp.MustCompile(`\$(\d+)`)
)

func parsePGStringArray(raw string) []string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "{")
	s = strings.TrimSuffix(s, "}")
	if s == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		p := strings.Trim(strings.TrimSpace(part), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *memPGStore) filterResources(q string, args []driver.NamedValue, applyPagination bool) []*memResRecord {
	tenant := nvStr(args, 0)
	if strings.Contains(q, "(r.res_type > $") {
		return nil
	}

	isEncEverything := strings.Contains(q, "NOT (r.res_type = 'Encounter' AND r.res_id = $2)")
	isPatEverything := strings.Contains(q, "r.res_type NOT IN ('Patient', 'Group')") && strings.Contains(q, "rf.target_id = $2")

	var wantType string
	if strings.Contains(q, "r.res_type = $2") {
		wantType = nvStr(args, 1)
	}
	var wantTypes []string
	if m := reResTypeAny.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		wantTypes = parsePGStringArray(nvStr(args, idx-1))
	}
	var excludeType string
	if m := reResTypeNotEq.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		excludeType = nvStr(args, idx-1)
	}
	var wantID string
	if !isEncEverything && !isPatEverything {
		if m := reResIDEq.FindStringSubmatch(q); len(m) == 2 && !strings.Contains(q, "OR (r.res_type =") {
			idx, _ := strconv.Atoi(m[1])
			wantID = nvStr(args, idx-1)
		}
	}
	var wantIDs []string
	if m := reResIDAny.FindStringSubmatch(q); len(m) == 2 && !strings.Contains(q, "r.res_type = 'Patient' AND r.res_id = ANY") {
		idx, _ := strconv.Atoi(m[1])
		wantIDs = parsePGStringArray(nvStr(args, idx-1))
	}

	var candidates []*memResRecord
	for _, r := range s.resources {
		if r.tenantID != tenant || r.isDeleted {
			continue
		}
		if isEncEverything {
			encID := nvStr(args, 1)
			if r.resType == "Encounter" && r.resID == encID {
				continue
			}
			if !s.inCompartment(tenant, "Encounter", encID, r) {
				continue
			}
			if !s.matchesClinicalDateBounds(r, q, args) {
				continue
			}
		}
		if isPatEverything {
			patID := nvStr(args, 1)
			if r.resType == "Patient" || r.resType == "Group" {
				continue
			}
			if !s.inCompartment(tenant, "Patient", patID, r) {
				continue
			}
			if !s.matchesClinicalDateBounds(r, q, args) {
				continue
			}
		}
		if wantType != "" && r.resType != wantType {
			continue
		}
		if len(wantTypes) > 0 && !slicesContains(wantTypes, r.resType) {
			continue
		}
		if excludeType != "" && r.resType == excludeType {
			continue
		}
		if wantID != "" && r.resID != wantID {
			continue
		}
		if len(wantIDs) > 0 && !slicesContains(wantIDs, r.resID) {
			continue
		}
		if !s.matchesJoinsAndWhere(r, q, args) {
			continue
		}
		candidates = append(candidates, r)
	}

	s.sortCandidates(candidates, q, args)

	if applyPagination && strings.Contains(q, "OFFSET $") && len(args) >= 2 {
		limit := nvInt(args, len(args)-2)
		offset := nvInt(args, len(args)-1)
		if offset >= len(candidates) {
			return nil
		}
		candidates = candidates[offset:]
		if limit > 0 && limit < len(candidates) {
			candidates = candidates[:limit]
		}
	}
	return candidates
}

var (
	reSortDateSub   = regexp.MustCompile(`\(SELECT (MIN|MAX)\(sd\.(sp_low|sp_high)\) FROM spidx_date sd WHERE [^)]+ sd\.sp_name = \$(\d+)\) (ASC|DESC)`)
	reSortStringSub = regexp.MustCompile(`\(SELECT (MIN|MAX)\(ss\.sp_value\) FROM spidx_string ss WHERE [^)]+ ss\.sp_name = \$(\d+)\) (ASC|DESC)`)
	reRfResTypeEq   = regexp.MustCompile(`AND rf\.res_type = \$(\d+)`)
	reRfSpNameEq    = regexp.MustCompile(`AND rf\.sp_name = \$(\d+)`)
	reRfSpNameAny   = regexp.MustCompile(`AND rf\.sp_name = ANY\(\$(\d+)\)`)
	reRfTargetType  = regexp.MustCompile(`AND \(?rf\.target_type = \$(\d+)`)
)

func (s *memPGStore) resolveIncludeQuery(q string, args []driver.NamedValue) []*memResRecord {
	tenant := nvStr(args, 0)
	idList := parsePGStringArray(nvStr(args, 1))
	if strings.Contains(q, "rf.res_id = ANY($3)") {
		idList = parsePGStringArray(nvStr(args, 2))
	}
	isRevInclude := strings.Contains(q, "rf.target_id = ANY($2)")

	wantResType := ""
	if m := reRfResTypeEq.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		wantResType = nvStr(args, idx-1)
	}
	wantSpName := ""
	if m := reRfSpNameEq.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		wantSpName = nvStr(args, idx-1)
	}
	var wantSpNames []string
	if m := reRfSpNameAny.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		wantSpNames = parsePGStringArray(nvStr(args, idx-1))
	}
	wantTargetType := ""
	if m := reRfTargetType.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		wantTargetType = nvStr(args, idx-1)
	}

	seen := make(map[string]bool)
	var out []*memResRecord
	for _, rf := range s.refs {
		if rf.TenantID != tenant {
			continue
		}
		if wantResType != "" && rf.ResType != wantResType {
			continue
		}
		if wantSpName != "" && rf.SpName != wantSpName {
			continue
		}
		if len(wantSpNames) > 0 && !slicesContains(wantSpNames, rf.SpName) {
			continue
		}
		if wantTargetType != "" && rf.TargetType != "" && rf.TargetType != wantTargetType {
			continue
		}
		if isRevInclude {
			if !slicesContains(idList, rf.TargetID) {
				continue
			}
			key := memResKey(tenant, rf.ResType, rf.ResID)
			if rec := s.resources[key]; rec != nil && !rec.isDeleted && !seen[key] {
				seen[key] = true
				out = append(out, rec)
			}
		} else {
			if !slicesContains(idList, rf.ResID) {
				continue
			}
			key := memResKey(tenant, rf.TargetType, rf.TargetID)
			if rec := s.resources[key]; rec != nil && !rec.isDeleted && !seen[key] {
				seen[key] = true
				out = append(out, rec)
			}
		}
	}
	return out
}

func (s *memPGStore) sortCandidates(candidates []*memResRecord, q string, args []driver.NamedValue) {
	if m := reSortDateSub.FindStringSubmatch(q); len(m) == 5 {
		spIdx, _ := strconv.Atoi(m[3])
		spName := nvStr(args, spIdx-1)
		desc := m[4] == "DESC"
		getDate := func(r *memResRecord) (time.Time, bool) {
			var best time.Time
			found := false
			for _, d := range s.dates {
				if d.TenantID == r.tenantID && d.ResType == r.resType && d.ResID == r.resID && d.SpName == spName {
					t := d.SpLow
					if desc {
						t = d.SpHigh
					}
					if !found || (!desc && t.Before(best)) || (desc && t.After(best)) {
						best = t
						found = true
					}
				}
			}
			return best, found
		}
		sort.Slice(candidates, func(i, j int) bool {
			ti, okI := getDate(candidates[i])
			tj, okJ := getDate(candidates[j])
			if okI != okJ {
				return okI
			}
			if okI && !ti.Equal(tj) {
				if desc {
					return ti.After(tj)
				}
				return ti.Before(tj)
			}
			return candidates[i].resID < candidates[j].resID
		})
		return
	}
	if m := reSortStringSub.FindStringSubmatch(q); len(m) == 4 {
		spIdx, _ := strconv.Atoi(m[2])
		spName := nvStr(args, spIdx-1)
		desc := m[3] == "DESC"
		getStr := func(r *memResRecord) (string, bool) {
			var best string
			found := false
			for _, st := range s.strings {
				if st.TenantID == r.tenantID && st.ResType == r.resType && st.ResID == r.resID && st.SpName == spName {
					if !found || (!desc && st.SpValue < best) || (desc && st.SpValue > best) {
						best = st.SpValue
						found = true
					}
				}
			}
			return best, found
		}
		sort.Slice(candidates, func(i, j int) bool {
			si, okI := getStr(candidates[i])
			sj, okJ := getStr(candidates[j])
			if okI != okJ {
				return okI
			}
			if okI && si != sj {
				if desc {
					return si > sj
				}
				return si < sj
			}
			return candidates[i].resID < candidates[j].resID
		})
		return
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].resType != candidates[j].resType {
			return candidates[i].resType < candidates[j].resType
		}
		return candidates[i].resID < candidates[j].resID
	})
}

func (s *memPGStore) inCompartment(tenant, focalType, focalID string, r *memResRecord) bool {
	for _, rf := range s.refs {
		if rf.TenantID != tenant {
			continue
		}
		if rf.ResType == r.resType && rf.ResID == r.resID && rf.TargetID == focalID && (rf.TargetType == focalType || rf.TargetType == "") {
			return true
		}
		if rf.ResType == focalType && rf.ResID == focalID && rf.TargetType == r.resType && rf.TargetID == r.resID {
			return true
		}
	}
	return false
}

func (s *memPGStore) matchesClinicalDateBounds(r *memResRecord, q string, args []driver.NamedValue) bool {
	var minHigh *time.Time
	var maxLow *time.Time
	if m := reDateHighGe.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		t := nvTime(args, idx-1)
		minHigh = &t
	}
	if m := reDateLowLe.FindStringSubmatch(q); len(m) == 2 {
		idx, _ := strconv.Atoi(m[1])
		t := nvTime(args, idx-1)
		maxLow = &t
	}
	if minHigh == nil && maxLow == nil {
		return true
	}
	hasDateRow := false
	for _, d := range s.dates {
		if d.TenantID == r.tenantID && d.ResType == r.resType && d.ResID == r.resID && d.SpName != "birthdate" {
			hasDateRow = true
			if minHigh != nil && d.SpHigh.Before(*minHigh) {
				continue
			}
			if maxLow != nil && d.SpLow.After(*maxLow) {
				continue
			}
			return true
		}
	}
	return !hasDateRow
}

func slicesContains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}

var (
	reCrfAlias        = regexp.MustCompile(`^JOIN spidx_reference (crf\d+) `)
	reCtgLinkCrf      = regexp.MustCompile(`\.res_id = (crf\d+)\.target_id`)
	reTargetTypeArg   = regexp.MustCompile(`\.target_type = \$(\d+)`)
	reTokenSysVal     = regexp.MustCompile(`sp_system = \$(\d+) AND [a-z0-9_]+\.sp_value = \$(\d+)`)
	reTokenEmptySys   = regexp.MustCompile(`sp_system = '' AND [a-z0-9_]+\.sp_value = \$(\d+)`)
	reTokenSysOnly    = regexp.MustCompile(`sp_system = \$(\d+)`)
	reDateCmp         = regexp.MustCompile(`sp_(low|high)\s*(<=|>=|<|>)\s*\$(\d+)`)
	reQuantBranchCode = regexp.MustCompile(`\(([^()]+)\) AND [a-z0-9_]+\.sp_code = \$(\d+)`)
	reQuantValCmp     = regexp.MustCompile(`sp_value\s*(<=|>=|!=|<|>|=)\s*\$(\d+)`)
	reQuantCodeOnly   = regexp.MustCompile(`sp_code = \$(\d+)`)
	reQuantSysOnly    = regexp.MustCompile(`sp_system = \$(\d+)`)
	reNotExistsToken  = regexp.MustCompile(`NOT EXISTS \(SELECT 1 FROM spidx_token (t\d+) WHERE [^)]+\)`)
	reMissingNot      = regexp.MustCompile(`AND NOT \(EXISTS \(SELECT 1 FROM spidx_[a-z]+ m WHERE m\.tenant_id = r\.tenant_id AND m\.res_type = r\.res_type AND m\.res_id = r\.res_id AND m\.sp_name = \$(\d+)\)`)
	reMissingPresent  = regexp.MustCompile(`AND \(EXISTS \(SELECT 1 FROM spidx_[a-z]+ m WHERE m\.tenant_id = r\.tenant_id AND m\.res_type = r\.res_type AND m\.res_id = r\.res_id AND m\.sp_name = \$(\d+)\)`)
	reHasExists       = regexp.MustCompile(`EXISTS \(SELECT 1 FROM spidx_reference hrf0 JOIN fhir_resource hr0 .+ hsp0\.sp_name = \$(\d+) AND \((.+)\) WHERE hrf0\.tenant_id = r\.tenant_id AND hrf0\.res_type = \$(\d+) AND hrf0\.sp_name = \$(\d+)`)
	reCompQuantOnly   = regexp.MustCompile(`EXISTS \(SELECT 1 FROM spidx_quantity cq WHERE cq\.tenant_id = r\.tenant_id AND cq\.res_type = r\.res_type AND cq\.res_id = r\.res_id AND cq\.sp_name = \$(\d+) AND ([^)]+)\)`)
	reCompTokQuant    = regexp.MustCompile(`EXISTS \(SELECT 1 FROM spidx_token ct JOIN spidx_quantity cq ON [^W]+ WHERE ct\.tenant_id = r\.tenant_id AND ct\.res_type = r\.res_type AND ct\.res_id = r\.res_id AND ct\.sp_name = 'code' AND \(([^)]+)\) AND cq\.sp_name = 'value-quantity' AND ([^)]+)\)`)
	reFilterStrSub    = regexp.MustCompile(`EXISTS \(SELECT 1 FROM spidx_string fs WHERE fs\.tenant_id = r\.tenant_id AND fs\.res_type = r\.res_type AND fs\.res_id = r\.res_id AND fs\.sp_name = \$(\d+) AND fs\.sp_value (?:LIKE|=) \$(\d+)`)
	reFilterTokSub    = regexp.MustCompile(`EXISTS \(SELECT 1 FROM spidx_token ft WHERE ft\.tenant_id = r\.tenant_id AND ft\.res_type = r\.res_type AND ft\.res_id = r\.res_id AND ft\.sp_name = \$(\d+) AND \((.+?)\)\)`)
)

func (s *memPGStore) matchesJoinsAndWhere(r *memResRecord, q string, args []driver.NamedValue) bool {
	joinSection := q
	whereSection := ""
	if idx := strings.Index(joinSection, "FROM fhir_resource r"); idx != -1 {
		joinSection = joinSection[idx+len("FROM fhir_resource r"):]
	}
	if idx := strings.Index(joinSection, "WHERE r.tenant_id = $1"); idx != -1 {
		whereSection = joinSection[idx+len("WHERE r.tenant_id = $1"):]
		joinSection = joinSection[:idx]
	}

	chainedRefs := make(map[string][]postgres.SpidxReference)

	parts := strings.Split(joinSection, "JOIN ")
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		line := "JOIN " + trimmed
		switch {
		case strings.HasPrefix(line, "JOIN spidx_reference "):
			if m := reCrfAlias.FindStringSubmatch(line); len(m) == 2 {
				crfAlias := m[1]
				spName, _ := extractSpNameAndValues(line, args)
				wantTargetType := ""
				if tm := reTargetTypeArg.FindStringSubmatch(line); len(tm) == 2 {
					tIdx, _ := strconv.Atoi(tm[1])
					wantTargetType = nvStr(args, tIdx-1)
				}
				var matchedRefs []postgres.SpidxReference
				if pm := reCtgLinkCrf.FindStringSubmatch(line); len(pm) == 2 {
					parentRefs := chainedRefs[pm[1]]
					for _, prf := range parentRefs {
						for _, rf := range s.refs {
							if rf.TenantID != r.tenantID || rf.ResID != prf.TargetID || rf.SpName != spName {
								continue
							}
							if prf.TargetType != "" && rf.ResType != prf.TargetType {
								continue
							}
							if wantTargetType != "" && rf.TargetType != "" && rf.TargetType != wantTargetType {
								continue
							}
							matchedRefs = append(matchedRefs, rf)
						}
					}
				} else {
					for _, rf := range s.refs {
						if rf.TenantID != r.tenantID || rf.ResType != r.resType || rf.ResID != r.resID || rf.SpName != spName {
							continue
						}
						if wantTargetType != "" && rf.TargetType != "" && rf.TargetType != wantTargetType {
							continue
						}
						matchedRefs = append(matchedRefs, rf)
					}
				}
				if len(matchedRefs) == 0 {
					return false
				}
				chainedRefs[crfAlias] = matchedRefs
				continue
			}
			if !s.matchSpidxRefJoin(r, line, args) {
				return false
			}
		case strings.HasPrefix(line, "JOIN spidx_string "):
			if m := reCtgLinkCrf.FindStringSubmatch(line); len(m) == 2 {
				refs := chainedRefs[m[1]]
				matchedAny := false
				for _, rf := range refs {
					targetRec := &memResRecord{tenantID: r.tenantID, resType: rf.TargetType, resID: rf.TargetID}
					if s.matchSpidxStringForTarget(targetRec, line, args) {
						matchedAny = true
						break
					}
				}
				if !matchedAny {
					return false
				}
				continue
			}
			if !s.matchSpidxStringJoin(r, line, args) {
				return false
			}
		case strings.HasPrefix(line, "JOIN spidx_token "):
			if m := reCtgLinkCrf.FindStringSubmatch(line); len(m) == 2 {
				refs := chainedRefs[m[1]]
				matchedAny := false
				for _, rf := range refs {
					targetRec := &memResRecord{tenantID: r.tenantID, resType: rf.TargetType, resID: rf.TargetID}
					if s.matchSpidxTokenForTarget(targetRec, line, args) {
						matchedAny = true
						break
					}
				}
				if !matchedAny {
					return false
				}
				continue
			}
			if !s.matchSpidxTokenJoin(r, line, args) {
				return false
			}
		case strings.HasPrefix(line, "JOIN spidx_quantity "):
			if !s.matchSpidxQuantJoin(r, line, args) {
				return false
			}
		case strings.HasPrefix(line, "JOIN spidx_date "):
			if !s.matchSpidxDateJoin(r, line, args) {
				return false
			}
		}
	}

	if whereSection != "" && !s.matchesWhereClauses(r, whereSection, args) {
		return false
	}

	return true
}

func (s *memPGStore) hasAnyIndexForParam(r *memResRecord, spName string) bool {
	for _, row := range s.strings {
		if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
			return true
		}
	}
	for _, row := range s.tokens {
		if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
			return true
		}
	}
	for _, row := range s.dates {
		if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
			return true
		}
	}
	for _, row := range s.refs {
		if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
			return true
		}
	}
	for _, row := range s.quants {
		if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
			return true
		}
	}
	for _, row := range s.uris {
		if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
			return true
		}
	}
	return false
}

func (s *memPGStore) matchesWhereClauses(r *memResRecord, whereSection string, args []driver.NamedValue) bool {
	// 1. :not modifier on token
	for _, m := range reNotExistsToken.FindAllStringSubmatch(whereSection, -1) {
		if s.matchSpidxTokenForTarget(r, m[0], args) {
			return false
		}
	}
	// 2. :missing=true
	for _, m := range reMissingNot.FindAllStringSubmatch(whereSection, -1) {
		idx, _ := strconv.Atoi(m[1])
		spName := nvStr(args, idx-1)
		if s.hasAnyIndexForParam(r, spName) {
			return false
		}
	}
	// 3. :missing=false
	for _, m := range reMissingPresent.FindAllStringSubmatch(whereSection, -1) {
		idx, _ := strconv.Atoi(m[1])
		spName := nvStr(args, idx-1)
		if !s.hasAnyIndexForParam(r, spName) {
			return false
		}
	}
	// 4. _has reverse chaining
	for _, m := range reHasExists.FindAllStringSubmatch(whereSection, -1) {
		spIdx, _ := strconv.Atoi(m[1])
		tokCond := m[2]
		srcIdx, _ := strconv.Atoi(m[3])
		refIdx, _ := strconv.Atoi(m[4])
		spName := nvStr(args, spIdx-1)
		srcType := nvStr(args, srcIdx-1)
		refName := nvStr(args, refIdx-1)

		matchedHas := false
		for _, rf := range s.refs {
			if rf.TenantID != r.tenantID || rf.ResType != srcType || rf.SpName != refName || rf.TargetID != r.resID {
				continue
			}
			srcRec := s.resources[memResKey(r.tenantID, rf.ResType, rf.ResID)]
			if srcRec == nil || srcRec.isDeleted {
				continue
			}
			if s.matchTokenCondOnRec(srcRec, spName, tokCond, args) {
				matchedHas = true
				break
			}
		}
		if !matchedHas {
			return false
		}
	}
	// 5. Composite component-code-value-quantity
	for _, m := range reCompQuantOnly.FindAllStringSubmatch(whereSection, -1) {
		spIdx, _ := strconv.Atoi(m[1])
		spName := nvStr(args, spIdx-1)
		qCond := m[2]
		if !s.matchQuantCondOnRec(r, spName, qCond, args) {
			return false
		}
	}
	// 6. Composite code-value-quantity
	for _, m := range reCompTokQuant.FindAllStringSubmatch(whereSection, -1) {
		tokCond := m[1]
		qCond := m[2]
		if !s.matchTokenCondOnRec(r, "code", tokCond, args) || !s.matchQuantCondOnRec(r, "value-quantity", qCond, args) {
			return false
		}
	}
	// 7. _filter string & token leaf conditions
	if strMatches := reFilterStrSub.FindAllStringSubmatch(whereSection, -1); len(strMatches) > 0 {
		isOrGroup := strings.Contains(whereSection, " OR EXISTS (SELECT 1 FROM spidx_string fs")
		anyMatched := false
		for _, m := range strMatches {
			spIdx, _ := strconv.Atoi(m[1])
			patIdx, _ := strconv.Atoi(m[2])
			spName := nvStr(args, spIdx-1)
			pat := nvStr(args, patIdx-1)
			matched := false
			for _, row := range s.strings {
				if row.TenantID == r.tenantID && row.ResType == r.resType && row.ResID == r.resID && row.SpName == spName {
					if matchStringPattern(row.SpValue, pat, true) {
						matched = true
						break
					}
				}
			}
			if matched {
				anyMatched = true
			} else if !isOrGroup {
				return false
			}
		}
		if isOrGroup && !anyMatched {
			return false
		}
	}
	for _, loc := range reFilterTokSub.FindAllStringSubmatchIndex(whereSection, -1) {
		spIdx, _ := strconv.Atoi(whereSection[loc[2]:loc[3]])
		spName := nvStr(args, spIdx-1)
		tokCond := whereSection[loc[4]:loc[5]]
		negated := loc[0] >= 5 && strings.HasSuffix(whereSection[:loc[0]], "NOT (")
		matched := s.matchTokenCondOnRec(r, spName, tokCond, args)
		if negated && matched {
			return false
		}
		if !negated && !matched {
			return false
		}
	}
	return true
}

func extractLineArgs(line string, args []driver.NamedValue) []string {
	matches := reArgRef.FindAllStringSubmatch(line, -1)
	var out []string
	for _, m := range matches {
		idx, _ := strconv.Atoi(m[1])
		out = append(out, nvStr(args, idx-1))
	}
	return out
}

func extractSpNameAndValues(line string, args []driver.NamedValue) (string, []string) {
	locs := reSpNameArg.FindAllStringSubmatchIndex(line, -1)
	if len(locs) > 0 {
		last := locs[len(locs)-1]
		idx, _ := strconv.Atoi(line[last[2]:last[3]])
		spName := nvStr(args, idx-1)
		vals := extractLineArgs(line[last[1]:], args)
		return spName, vals
	}
	lArgs := extractLineArgs(line, args)
	if len(lArgs) == 0 {
		return "", nil
	}
	return lArgs[0], lArgs[1:]
}

func matchStringPattern(spVal, pat string, isLike bool) bool {
	valLower := strings.ToLower(spVal)
	patLower := strings.ToLower(pat)
	if !isLike {
		return valLower == patLower
	}
	if strings.HasPrefix(patLower, "%") && strings.HasSuffix(patLower, "%") {
		sub := strings.Trim(patLower, "%")
		return sub != "" && strings.Contains(valLower, sub)
	}
	if strings.HasSuffix(patLower, "%") {
		pref := strings.TrimSuffix(patLower, "%")
		return pref != "" && strings.HasPrefix(valLower, pref)
	}
	return valLower == patLower
}

func (s *memPGStore) matchSpidxStringJoin(r *memResRecord, line string, args []driver.NamedValue) bool {
	return s.matchSpidxStringForTarget(r, line, args)
}

func (s *memPGStore) matchSpidxStringForTarget(r *memResRecord, line string, args []driver.NamedValue) bool {
	spName, patterns := extractSpNameAndValues(line, args)
	if spName == "" || len(patterns) == 0 {
		return true
	}
	isLike := strings.Contains(line, "LIKE")
	for _, row := range s.strings {
		if row.TenantID != r.tenantID || row.ResID != r.resID {
			continue
		}
		if r.resType != "" && row.ResType != r.resType {
			continue
		}
		if row.SpName != spName && row.SpName != spName+":text" && (!strings.HasSuffix(spName, ":text") || row.SpName != strings.TrimSuffix(spName, ":text")) {
			continue
		}
		for _, pat := range patterns {
			if matchStringPattern(row.SpValue, pat, isLike) {
				return true
			}
		}
	}
	return false
}

func (s *memPGStore) matchSpidxTokenJoin(r *memResRecord, line string, args []driver.NamedValue) bool {
	return s.matchSpidxTokenForTarget(r, line, args)
}

func (s *memPGStore) matchSpidxTokenForTarget(r *memResRecord, line string, args []driver.NamedValue) bool {
	locs := reSpNameArg.FindAllStringSubmatchIndex(line, -1)
	if len(locs) == 0 {
		return true
	}
	last := locs[len(locs)-1]
	idx, _ := strconv.Atoi(line[last[2]:last[3]])
	spName := nvStr(args, idx-1)
	condPart := line[last[1]:]
	return s.matchTokenCondOnRec(r, spName, condPart, args)
}

func (s *memPGStore) matchTokenCondOnRec(r *memResRecord, spName, condPart string, args []driver.NamedValue) bool {
	sysValPairs := reTokenSysVal.FindAllStringSubmatch(condPart, -1)
	emptySysMatches := reTokenEmptySys.FindAllStringSubmatch(condPart, -1)
	sysOnlyMatches := reTokenSysOnly.FindAllStringSubmatch(condPart, -1)

	for _, row := range s.tokens {
		if row.TenantID != r.tenantID || row.ResID != r.resID || row.SpName != spName {
			continue
		}
		if r.resType != "" && row.ResType != r.resType {
			continue
		}
		if len(sysValPairs) > 0 {
			for _, m := range sysValPairs {
				sIdx, _ := strconv.Atoi(m[1])
				vIdx, _ := strconv.Atoi(m[2])
				if row.SpSystem == nvStr(args, sIdx-1) && strings.EqualFold(row.SpValue, nvStr(args, vIdx-1)) {
					return true
				}
			}
			continue
		}
		if len(emptySysMatches) > 0 {
			for _, m := range emptySysMatches {
				vIdx, _ := strconv.Atoi(m[1])
				if row.SpSystem == "" && strings.EqualFold(row.SpValue, nvStr(args, vIdx-1)) {
					return true
				}
			}
			continue
		}
		if len(sysOnlyMatches) > 0 && !strings.Contains(condPart, "sp_value") {
			for _, m := range sysOnlyMatches {
				sIdx, _ := strconv.Atoi(m[1])
				if row.SpSystem == nvStr(args, sIdx-1) {
					return true
				}
			}
			continue
		}
		for _, v := range extractLineArgs(condPart, args) {
			if v != "" && strings.EqualFold(row.SpValue, v) {
				return true
			}
		}
	}
	return false
}

func (s *memPGStore) matchSpidxRefJoin(r *memResRecord, line string, args []driver.NamedValue) bool {
	if strings.Contains(line, "crf") && !strings.Contains(line, "target_id =") {
		return true
	}
	spName, vals := extractSpNameAndValues(line, args)
	if spName == "" {
		return true
	}
	for _, row := range s.refs {
		if row.TenantID != r.tenantID || row.ResType != r.resType || row.ResID != r.resID || row.SpName != spName {
			continue
		}
		if len(vals) == 0 {
			return true
		}
		for _, v := range vals {
			if v != "" && row.TargetID == v {
				return true
			}
		}
	}
	return false
}

func evalSingleQuantCmp(val float64, op string, target float64) bool {
	switch op {
	case "<":
		return val < target
	case "<=":
		return val <= target
	case ">":
		return val > target
	case ">=":
		return val >= target
	case "!=":
		return math.Abs(val-target) > 1e-9
	default:
		return math.Abs(val-target) <= 1e-9
	}
}

func (s *memPGStore) matchSpidxQuantJoin(r *memResRecord, line string, args []driver.NamedValue) bool {
	locs := reSpNameArg.FindAllStringSubmatchIndex(line, -1)
	if len(locs) == 0 {
		return true
	}
	last := locs[len(locs)-1]
	idx, _ := strconv.Atoi(line[last[2]:last[3]])
	spName := nvStr(args, idx-1)
	condPart := line[last[1]:]
	return s.matchQuantCondOnRec(r, spName, condPart, args)
}

func (s *memPGStore) matchQuantCondOnRec(r *memResRecord, spName, condPart string, args []driver.NamedValue) bool {
	branches := reQuantBranchCode.FindAllStringSubmatch(condPart, -1)
	valCmps := reQuantValCmp.FindAllStringSubmatch(condPart, -1)
	codeMatches := reQuantCodeOnly.FindAllStringSubmatch(condPart, -1)
	sysMatches := reQuantSysOnly.FindAllStringSubmatch(condPart, -1)

	for _, row := range s.quants {
		if row.TenantID != r.tenantID || row.ResType != r.resType || row.ResID != r.resID || row.SpName != spName {
			continue
		}
		if len(sysMatches) > 0 {
			sIdx, _ := strconv.Atoi(sysMatches[len(sysMatches)-1][1])
			wantSys := nvStr(args, sIdx-1)
			if row.SpSystem != wantSys && (!strings.Contains(condPart, "sp_system = ''") || row.SpSystem != "") {
				continue
			}
		}
		if len(branches) > 0 {
			matchedBranch := false
			for _, b := range branches {
				cIdx, _ := strconv.Atoi(b[2])
				if row.SpCode != nvStr(args, cIdx-1) {
					continue
				}
				bCmps := reQuantValCmp.FindAllStringSubmatch(b[1], -1)
				allOk := true
				for _, cm := range bCmps {
					vIdx, _ := strconv.Atoi(cm[2])
					if !evalSingleQuantCmp(row.SpValue, cm[1], nvFloat(args, vIdx-1)) {
						allOk = false
						break
					}
				}
				if allOk {
					matchedBranch = true
					break
				}
			}
			if matchedBranch {
				return true
			}
			continue
		}
		if len(codeMatches) > 0 {
			cIdx, _ := strconv.Atoi(codeMatches[0][1])
			if row.SpCode != nvStr(args, cIdx-1) {
				continue
			}
		}
		allOk := true
		for _, cm := range valCmps {
			vIdx, _ := strconv.Atoi(cm[2])
			if !evalSingleQuantCmp(row.SpValue, cm[1], nvFloat(args, vIdx-1)) {
				allOk = false
				break
			}
		}
		if allOk {
			return true
		}
	}
	return false
}

func (s *memPGStore) matchSpidxDateJoin(r *memResRecord, line string, args []driver.NamedValue) bool {
	locs := reSpNameArg.FindAllStringSubmatchIndex(line, -1)
	if len(locs) == 0 {
		return true
	}
	last := locs[len(locs)-1]
	idx, _ := strconv.Atoi(line[last[2]:last[3]])
	spName := nvStr(args, idx-1)
	condPart := line[last[1]:]
	cmps := reDateCmp.FindAllStringSubmatch(condPart, -1)

	for _, row := range s.dates {
		if row.TenantID != r.tenantID || row.ResType != r.resType || row.ResID != r.resID || row.SpName != spName {
			continue
		}
		allOk := true
		for _, cm := range cmps {
			col := cm[1]
			op := cm[2]
			tIdx, _ := strconv.Atoi(cm[3])
			target := nvTime(args, tIdx-1)
			val := row.SpLow
			if col == "high" {
				val = row.SpHigh
			}
			var ok bool
			switch op {
			case "<":
				ok = val.Before(target)
			case "<=":
				ok = !val.After(target)
			case ">":
				ok = val.After(target)
			case ">=":
				ok = !val.Before(target)
			}
			if !ok {
				allOk = false
				break
			}
		}
		if allOk {
			return true
		}
	}
	return false
}
