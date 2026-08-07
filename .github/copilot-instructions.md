# Copilot instructions for this repository

## Build and test commands

- Build binary (same as CI): `go build -o afisla .`
- Run full test suite: `go test -v ./...`
- Run a single test: `go test -v ./... -run '^TestName$'`
- Run tests for one package: `go test -v .`

CI workflow is in `.github/workflows/go.yml` and currently runs build + `go test -v ./...` on push/PR to `main`.

## High-level architecture

This is a single-binary Go CLI with two modes (`main.go`):

- `afisla server`: runs three listeners in one process:
  - HTTP ingress (`handleHTTP`) on `--http-port` (default `3376`)
  - Control channel for client registration and HTTP response messages on `--ctrl-port` (default `6673`)
  - Relay channel for TCP data bridging on `--relay-port` (default `6674`)
- `afisla client`: opens one long-lived control TCP connection to the server, registers a subdomain, then:
  - handles HTTP proxy requests sent over JSON messages
  - opens per-connection relay sockets for raw TCP forwarding

Core runtime flow spans `server.go`, `client.go`, `protocol.go`, and `util.go`:

1. Client sends `RegisterRequest` over control socket.
2. Server validates/allocates subdomain and relay port, stores tunnel in `Server.tunnels`, and replies with `RegisterResponse`.
3. For HTTP: server receives public request, serializes it as `HttpRequestMsg` (body base64), waits on `pending[id]`, and writes back `HttpResponseMsg`.
4. For TCP: server accepts external TCP on assigned per-tunnel port, sends `TcpOpenMsg` to client, and waits for `tcp_accept` on relay listener to bridge streams.

`README.md` documents the production topology: HTTP is expected to be proxied via Cloudflare wildcard domain, while control/relay traffic uses a direct DNS-only host.

## Key codebase conventions

- JSON message protocol types are string-discriminated (`type` field) and declared centrally in `protocol.go`. Extend these structs first when adding protocol behavior.
- HTTP payloads are tunneled as base64 strings in protocol messages (`HttpRequestMsg.Body`, `HttpResponseMsg.Body`) to keep control traffic JSON-safe.
- Shared server state (`tunnels`, `pending`, `pendingTCP`, `nextPort`) is guarded by `Server.mu`; use lock discipline consistent with existing read/write sections.
- Tunnel IDs and connection IDs are generated with `generateID()` (`util.go`), which combines time + randomness; use it for new request/connection correlation.
- Subdomains are lowercase `[a-z0-9-]`, max 63 chars (`isValidSubdomain`); host routing derives subdomain from Host header using `extractSubdomain(host, baseDomain)`.
- CLI option parsing is manual in `main.go` (supports both legacy and current flag aliases). Keep backward-compatible aliases when adjusting flags.
- Error style in this repo: return wrapped errors from top-level start paths (`fmt.Errorf("context: %w", err)`), and fail fast in `main` via `log.Fatalf`.
