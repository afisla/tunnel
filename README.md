# Afisla — HTTP & TCP Tunnel

Afisla exposes your local server to the internet via a public URL. Supports **HTTP/HTTPS** and **raw TCP** (SSH, database, etc.).

```
afisla client --port-local 8080 --domain testing
→ https://testing.afisla.web.id   (HTTP)
→ afisla.web.id:30000             (TCP relay)
```

## Architecture

```
Client (NAT)                          Server (public)
┌─────────────────┐     control       ┌──────────────────────┐
│  localhost:8080  │◄──── port 6673 ──►│  afisla server       │
│  (app/service)   │                  │                      │
│         │        │    relay data    │  HTTP :3376 ← Apache │
│         │        │◄──── port 6674 ──►  Ctrl :6673          │
│         │        │                  │  Relay:6674          │
│         │        │                  │  TCP  :30000-40000   │
└─────────────────┘                  └──────────────────────┘
                                              │
                                    HTTP ──► testing.afisla.web.id
                                    TCP  ──► afisla.web.id:30000
```

## Quick Start

### Server (already running on afisla.web.id)

```bash
afisla server --base-domain afisla.web.id
```

### Client

```bash
# Basic — random subdomain
afisla client --port-local 3000

# Custom subdomain
afisla client --port-local 8080 --domain testing
# → https://testing.afisla.web.id forwards to localhost:8080

# Specific server
afisla client --host-tunnel afisla.web.id --port-local 3000 --domain myapp
```

## Install on Client Machine

```bash
curl -fsSL https://afisla.web.id/install.sh | bash
```

Or manually:

```bash
# Download binary
sudo curl -fsSLo /usr/local/bin/afisla https://afisla.web.id/afisla-linux-amd64
sudo chmod +x /usr/local/bin/afisla

# Run
afisla client --port-local 8080 --domain myapp
```

## Features

| Feature | Description |
|---|---|
| **HTTP/HTTPS** | Route by subdomain via Apache. All HTTP methods, headers, bodies. |
| **TCP Relay** | Raw TCP forwarding for SSH, RDP, databases, game servers. |
| **Custom Domain** | Request a specific subdomain with `--domain`. |
| **Random Domain** | Auto-generated 6-char subdomain if `--domain` omitted. |
| **Concurrent** | Multiple HTTP requests handled concurrently per tunnel. |
| **TLS** | HTTPS via wildcard Let's Encrypt cert on afisla.web.id. |

## TCP Relay Usage

Each tunnel gets an assigned TCP relay port (30000-40000). Use it for any protocol: SSH, RDP, databases, game servers.

### SSH

On the machine behind NAT:
```bash
afisla client --port-local 22 --domain myserver
# → TCP: afisla.web.id:30000 (raw TCP relay)
```

From anywhere, SSH through the relay:
```bash
ssh -o ProxyCommand='nc afisla.web.id 30000' user@localhost -p 22
```

The `ProxyCommand` opens a raw TCP connection to the relay port — no `%h` needed.  
SSH protocol bytes flow through the tunnel to the client machine's port 22.

### Any TCP Service

```bash
# Local: expose any port
afisla client --port-local 5432 --domain postgres

# Remote: connect via relay port (shown after client connects)
psql -h afisla.web.id -p RELAY_PORT -U user db
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
--port-local        Local port to forward     (default 8000)
--domain            Requested subdomain       (random if empty)
--host-tunnel       Server host               (default afisla.web.id)
--tunnel-http-port  Server HTTP port          (default 443)
--ctrl-port         Server control port       (default 6673)
--relay-port        Server relay data port    (default 6674)
```

## Build from Source

```bash
git clone https://github.com/afisla/tunnel afisla
cd afisla
go build -o afisla .
```

Requires Go 1.21+.
