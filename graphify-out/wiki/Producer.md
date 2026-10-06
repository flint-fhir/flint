# Producer

> 24 nodes · cohesion 0.13

## Key Concepts

- **Producer** (11 connections) — `automq/producer.go`
- **Activities** (8 connections) — `ingest/activity/activities.go`
- **log/slog.Logger** (5 connections)
- **activities.go** (5 connections) — `ingest/activity/activities.go`
- **NewProducer()** (4 connections) — `automq/producer.go`
- **.Publish()** (4 connections) — `automq/producer.go`
- **main()** (4 connections) — `cmd/worker/main.go`
- **.PublishToAutoMQ()** (3 connections) — `ingest/activity/activities.go`
- **.WriteMedplumBatch()** (3 connections) — `ingest/activity/activities.go`
- **.WritePostgresBatch()** (3 connections) — `ingest/activity/activities.go`
- **Config** (3 connections) — `automq/producer.go`
- **Message** (3 connections) — `automq/producer.go`
- **topicSet()** (3 connections) — `automq/producer.go`
- **main()** (3 connections) — `cmd/e2e/test_delete.go`
- **IndexExtractorFunc** (2 connections) — `ingest/activity/activities.go`
- **PublishToAutoMQInput** (2 connections) — `ingest/activity/activities.go`
- **WriteMedplumBatchInput** (2 connections) — `ingest/activity/activities.go`
- **WritePostgresBatchInput** (2 connections) — `ingest/activity/activities.go`
- **test_delete.go** (2 connections) — `cmd/e2e/test_delete.go`
- **envOrDefault()** (2 connections) — `cmd/e2e/test_delete.go`
- **worker/main.go** (2 connections) — `cmd/worker/main.go`
- **envOrDefault()** (2 connections) — `cmd/worker/main.go`
- **.Close()** (1 connections) — `automq/producer.go`
- **github.com/twmb/franz-go/pkg/kgo.Client** (1 connections)

## Relationships

- [store.go](store.go.md) (7 shared connections)
- [Server](Server.md) (1 shared connections)

## Source Files

- `automq/producer.go`
- `cmd/e2e/test_delete.go`
- `cmd/worker/main.go`
- `ingest/activity/activities.go`

## Audit Trail

- EXTRACTED: 44 (100%)
- INFERRED: 0 (0%)
- AMBIGUOUS: 0 (0%)

---

*Part of the graphify knowledge wiki. See [index](index.md) to navigate.*