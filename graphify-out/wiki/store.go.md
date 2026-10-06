# store.go

> 35 nodes · cohesion 0.13

## Key Concepts

- **store.go** (18 connections) — `store/postgres/store.go`
- **context.Context** (17 connections)
- **insertSearchIndexes()** (12 connections) — `store/postgres/store.go`
- **Store** (10 connections) — `store/postgres/store.go`
- **SearchIndexes** (9 connections) — `store/postgres/store.go`
- **database/sql.Tx** (8 connections)
- **New()** (7 connections) — `store/postgres/store.go`
- **deleteSearchIndexes()** (6 connections) — `store/postgres/store.go`
- **.WriteBatch()** (5 connections) — `store/postgres/store.go`
- **.WriteResource()** (5 connections) — `store/postgres/store.go`
- **New()** (5 connections) — `server/server.go`
- **bulkInsertDates()** (5 connections) — `store/postgres/store.go`
- **bulkInsertQuantities()** (5 connections) — `store/postgres/store.go`
- **bulkInsertReferences()** (5 connections) — `store/postgres/store.go`
- **bulkInsertStrings()** (5 connections) — `store/postgres/store.go`
- **bulkInsertTokens()** (5 connections) — `store/postgres/store.go`
- **bulkInsertURIs()** (5 connections) — `store/postgres/store.go`
- **ResourceInput** (4 connections) — `store/postgres/store.go`
- **TestServer_Integration()** (4 connections) — `server/server_test.go`
- **main()** (3 connections) — `cmd/flintd/main.go`
- **SpidxDate** (3 connections) — `store/postgres/store.go`
- **SpidxQuantity** (3 connections) — `store/postgres/store.go`
- **SpidxURI** (3 connections) — `store/postgres/store.go`
- **.DeleteResource()** (3 connections) — `store/postgres/store.go`
- **database/sql.DB** (2 connections)
- *... and 10 more nodes in this community*

## Relationships

- [Producer](Producer.md) (7 shared connections)
- [Server](Server.md) (2 shared connections)
- [testing.T](testing.T.md) (2 shared connections)
- [.Search](Search.md) (1 shared connections)
- [helpers.go](helpers.go.md) (1 shared connections)

## Source Files

- `cmd/flintd/main.go`
- `server/server.go`
- `server/server_test.go`
- `store/postgres/store.go`

## Audit Trail

- EXTRACTED: 92 (100%)
- INFERRED: 0 (0%)
- AMBIGUOUS: 0 (0%)

---

*Part of the graphify knowledge wiki. See [index](index.md) to navigate.*