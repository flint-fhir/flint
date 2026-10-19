# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /src

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/flintd ./cmd/flintd \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/certify ./cmd/certify \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker

# Optional Add-On target: Temporal ingestion worker (`docker build --target worker ...`)
FROM alpine:3.21 AS worker
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 flint
COPY --from=builder /out/worker /usr/local/bin/worker
USER flint
ENTRYPOINT ["/usr/local/bin/worker"]

# Default target: Standalone Flint FHIR R4 Server (`flintd` + `seed` + `certify`)
FROM alpine:3.21 AS flint
LABEL org.opencontainers.image.title="Flint FHIR R4 Server" \
      org.opencontainers.image.description="High-Performance Open Source FHIR R4 Server on PostgreSQL (linux/amd64, linux/arm64)" \
      org.opencontainers.image.source="https://github.com/flint-fhir/flint" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 flint

COPY --from=builder /out/flintd /usr/local/bin/flintd
COPY --from=builder /out/seed /usr/local/bin/seed
COPY --from=builder /out/certify /usr/local/bin/certify

USER flint
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/flintd"]
