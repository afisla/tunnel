# Afisla — HTTP & TCP Tunnel

Afisla exposes your local server to the internet via a public URL. Supports **HTTP/HTTPS** and **raw TCP** (SSH, database, etc.).

```
afisla client --port-local 8080 --domain testing
→ https://testing.afisla.web.id      (HTTP — via Cloudflare proxy)
→ relay.afisla.web.id:30000          (TCP relay — direct)
```

## Dual-Domain Architecture

Cloudflare **proxies** HTTP/HTTPS for `*.afisla.web.id`, while tunnel control traffic uses a separate DNS-only domain `relay.afisla.web.id` (direct IP, no proxy).

```
                                     Cloudflare (orange cloud)
                                     ┌──────────────────────┐
Browser ──► testing.afisla.web.id ──►│  *.afisla.web.id     │
          (port 443)                  │  proxied via CF      │
                                     └──────────┬───────────┘
                                                │
Client (NAT)                    Apache :80/443  ▼  Server (public)
┌──────────────────┐              ┌──────────────────────────┐
│  localhost:8080   │              │  afisla server           │
│  (app/service)    │   control   │                           │
│         │         │◄───6673 ────│  HTTP :3376 ← Apache      │
│         │         │   relay     │  Ctrl :6673               │
│         │         │◄───6674 ────│  Relay:6674               │
│         │         │             │  TCP  :30000-40000        │
└──────────────────┘             └──────────────────────────┘
         ▲                                │
         │  relay.afisla.web.id:30000     │
         └────── direct (no proxy) ───────┘
```

## Quick Start

### Client

```bash
# Basic — random subdomain
afisla client --port-local 3000

# Custom subdomain
afisla client --port-local 8080 --domain testing
# → https://testing.afisla.web.id forwards to localhost:8080
```

### Server (already running)

```bash
afisla server --base-domain afisla.web.id
```

## Install on Client Machine

```bash
curl -fsSL https://afisla.web.id/install.sh | bash
```

Or manually:

```bash
# Download binary from GitHub releases
sudo curl -fsSLo /usr/local/bin/afisla https://github.com/afisla/tunnel/releases/download/v0.4.1/afisla-linux-amd64
sudo chmod +x /usr/local/bin/afisla

# Run
afisla client --port-local 8080 --domain myapp
```

## Features

| Feature | Description |
|---|---|
| **HTTP/HTTPS** | Route by subdomain via Cloudflare + Apache. All HTTP methods, headers, bodies. |
| **TCP Relay** | Raw TCP forwarding for SSH, RDP, databases, game servers. |
| **Custom Domain** | Request a specific subdomain with `--domain`. |
| **Random Domain** | Auto-generated 6-char subdomain if `--domain` omitted. |
| **Concurrent** | Multiple HTTP requests handled concurrently per tunnel. |
| **TLS** | HTTPS via Cloudflare edge + wildcard Let's Encrypt cert. |
| **Cloudflare Proxy** | HTTP/HTTPS proxied through Cloudflare (DDoS protection, caching). |
| **Dual-Domain** | HTTP via `*.afisla.web.id` (proxied), tunnel via `relay.afisla.web.id` (direct). |

## TCP Relay Usage

Each tunnel gets an assigned TCP relay port (30000-40000) on `relay.afisla.web.id`.

### SSH

On the machine behind NAT:
```bash
afisla client --port-local 22 --domain myserver
# → TCP: relay.afisla.web.id:30000 (raw TCP relay)
```

From anywhere, SSH through the relay:
```bash
ssh -o ProxyCommand='nc relay.afisla.web.id 30000' user@localhost -p 22
```

### Any TCP Service

```bash
# Local: expose any port
afisla client --port-local 5432 --domain postgres

# Remote: connect via relay port
psql -h relay.afisla.web.id -p 30000 -U user db
```

## Server Options

```
--http-port     HTTP proxy port      (default 3376)
--ctrl-port     Control port         (default 6673)
--relay-port    Relay data port      (default 6674)
--base-domain   Base domain          (default afisla.web.id)
--relay-start   TCP port range start (default 30000)
--relay-end     TCP port range end   (default 40000)
```

## Client Options

```
--port-local        Local port to forward         (default 8000)
--domain            Requested subdomain           (random if empty)
--host-tunnel       Tunnel relay host             (default relay.afisla.web.id)
--tunnel-http-port  Server HTTP port              (default 443)
--ctrl-port         Server control port           (default 6673)
--relay-port        Server relay data port        (default 6674)
```

## Build from Source

```bash
git clone https://github.com/afisla/tunnel afisla
cd afisla
go build -o afisla .
```

Requires Go 1.21+.
