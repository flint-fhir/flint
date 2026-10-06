# Server

> 22 nodes · cohesion 0.18

## Key Concepts

- **Server** (16 connections) — `server/server.go`
- **net/http.ResponseWriter** (7 connections)
- **writeOperationOutcome()** (7 connections) — `server/server.go`
- **net/http.Request** (6 connections)
- **.handleBundle()** (6 connections) — `server/bundle.go`
- **.handleCreate()** (5 connections) — `server/server.go`
- **.handleRead()** (5 connections) — `server/server.go`
- **.handleSearch()** (5 connections) — `server/server.go`
- **.handleSMARTConfig()** (4 connections) — `server/smart.go`
- **generateID()** (3 connections) — `server/server.go`
- **.handleMetadata()** (3 connections) — `server/server.go`
- **.protoToJSON()** (3 connections) — `server/server.go`
- **SMARTConfig** (3 connections) — `server/smart.go`
- **.Handler()** (2 connections) — `server/server.go`
- **.RegisterResourceType()** (2 connections) — `server/server.go`
- **.SetSMARTConfig()** (2 connections) — `server/smart.go`
- **Server** (2 connections) — `server/smart.go`
- **google.golang.org/protobuf/proto.Message** (1 connections)
- **google.golang.org/protobuf/reflect/protoreflect.MessageDescriptor** (1 connections)
- **net/http.Handler** (1 connections)
- **Server** (1 connections) — `server/bundle.go`
- **smart.go** (1 connections) — `server/smart.go`

## Relationships

- [store.go](store.go.md) (2 shared connections)
- [Producer](Producer.md) (1 shared connections)
- [bundle.go](bundle.go.md) (1 shared connections)

## Source Files

- `server/bundle.go`
- `server/server.go`
- `server/smart.go`

## Audit Trail

- EXTRACTED: 42 (93%)
- INFERRED: 3 (7%)
- AMBIGUOUS: 0 (0%)

---

*Part of the graphify knowledge wiki. See [index](index.md) to navigate.*