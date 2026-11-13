# Embedded Pure-Go FHIR R4 Server (`CGO_ENABLED=0` + SQLite WAL)

Flint's HTTP/clinical engine ([`server`](../../server/)) is strictly decoupled from storage via [`store.Store`](../../store/store.go) and [`store.AnalyticalStore`](../../store/store.go). That lets you embed a complete **HL7 FHIR R4, US Core v6.1.0 & v7.0.0, HL7 IPA v1.0.0, SMART v2, and SQL-on-FHIR v2** server directly inside any Go binary, CLI, desktop clinical app, or integration test with **`CGO_ENABLED=0`** and **zero external infrastructure**.

## Run the Example

```bash
# Ephemeral in-memory mode (:memory:)
CGO_ENABLED=0 go run ./examples/embedded-sqlite

# Durable single-file SQLite Write-Ahead Logging (WAL) mode
CGO_ENABLED=0 go run ./examples/embedded-sqlite \
  -dsn flint-embedded.db \
  -addr 127.0.0.1:8080 \
  -referential-integrity strict
```

Once running, open:
- **CapabilityStatement**: `http://127.0.0.1:8080/fhir/r4/demo/metadata`
- **Interactive SMART v2 & Clinical Sandbox (`$docs`)**: `http://127.0.0.1:8080/fhir/r4/demo/$docs`
- **Live 137-Check Certification Suite (`$certify`)**: `http://127.0.0.1:8080/fhir/r4/demo/$certify`

## Embed in Your Own Go Service

```bash
go get github.com/flint-fhir/flint
```

```go
package main

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"

	_ "modernc.org/sqlite"

	"github.com/flint-fhir/flint/server"
	"github.com/flint-fhir/flint/store/sqlite"
)

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	db, err := sql.Open("sqlite", "clinical.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if err := sqlite.RunMigrations(ctx, db, sqlite.JournalWAL); err != nil {
		panic(err)
	}
	sqStore := sqlite.New(db, sqlite.WithJournalMode(sqlite.JournalWAL), sqlite.WithDSN("clinical.db"))
	if err := sqStore.LoadFromDB(ctx); err != nil {
		panic(err)
	}

	srv := server.New(sqStore, logger)
	srv.SetAnalyticalStore(sqStore)
	srv.SetReferentialIntegrity(server.ReferentialIntegrityStrict)
	server.RegisterDefaultResourceTypes(srv)

	// srv.Handler() is a standard net/http.Handler — mount it anywhere!
	_ = http.ListenAndServe("127.0.0.1:8080", srv.Handler())
}
```
