# syntax=docker/dockerfile:1

## ---- builder: full Go toolchain, source, module cache ----
FROM golang:1.26-alpine AS builder
WORKDIR /src

# Download modules before copying source so a source-only change doesn't
# bust this layer's cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: every dependency in this module (razorpay-go, lib/pq, the
# razorpay-mcp-server/mark3labs/mcp-go composition) is pure Go — no cgo,
# no libc dependency at runtime. That's what makes a distroless "static"
# final stage below possible at all: these binaries need nothing but the
# kernel underneath them.
#
# cmd/ contains exactly three binaries, confirmed against source, not
# assumed:
#   - mandate-cli: the propose/confirm policy CLI.
#   - mandate-gateway: the transport-layer enforcement process — also the
#     binary that serves the composed MCP toolset (internal/mcpserver)
#     over stdio. There is no separate MCP-server binary; mandate-gateway
#     is it.
#   - mandate-verify: standalone audit hash-chain verifier.
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/mandate-cli ./cmd/mandate-cli && \
    CGO_ENABLED=0 GOOS=linux go build -o /out/mandate-gateway ./cmd/mandate-gateway && \
    CGO_ENABLED=0 GOOS=linux go build -o /out/mandate-verify ./cmd/mandate-verify

## ---- final: no Go toolchain, no source, just the binaries ----
FROM gcr.io/distroless/static-debian12:nonroot AS final
# distroless/static-debian12 ships nothing but a minimal libc-free
# runtime, /etc/passwd's nonroot user, and — the reason it's chosen here
# over scratch — a real CA bundle at /etc/ssl/certs/ca-certificates.crt,
# which every outbound HTTPS call this project makes to Razorpay's API
# needs to verify TLS. No shell, no package manager, no Go toolchain, no
# copied source tree: only the three statically-linked binaries below.
WORKDIR /app
COPY --from=builder /out/mandate-cli /out/mandate-gateway /out/mandate-verify ./

# No ENTRYPOINT/CMD is set here on purpose: this one image holds three
# independent binaries used three different ways, so there is no single
# correct default. docker-compose.yml's services each pick their own
# binary explicitly via `entrypoint:`.
#
# mandate-gateway specifically speaks MCP's JSON-RPC protocol over
# stdin/stdout — running it containerized needs a real interactive stdin
# attached (`docker run -i` / `docker compose run -i`, not `docker compose
# up`, which attaches none). See docker-compose.yml's mandate-gateway
# service comment for the current status of that path.
