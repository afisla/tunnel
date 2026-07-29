package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type pendingRequest struct {
	ch chan *HttpResponseMsg
}

type tunnelClient struct {
	encoder   *json.Encoder
	decoder   *json.Decoder
	ctrl      net.Conn
	domain    string
	relayPort int
}

type Server struct {
	httpPort   string
	ctrlPort   string
	relayPort  string
	baseDomain string
	relayStart int
	relayEnd   int
	staticDir  string

	mu         sync.RWMutex
	tunnels    map[string]*tunnelClient
	pending    map[string]*pendingRequest
	pendingTCP map[string]net.Conn
	nextPort   int
}

func NewServer(httpPort, ctrlPort, relayPort, baseDomain string, relayStart, relayEnd int, staticDir string) *Server {
	return &Server{
		httpPort:   httpPort,
		ctrlPort:   ctrlPort,
		relayPort:  relayPort,
		baseDomain: baseDomain,
		relayStart: relayStart,
		relayEnd:   relayEnd,
		staticDir:  staticDir,
		tunnels:    make(map[string]*tunnelClient),
		pending:    make(map[string]*pendingRequest),
		pendingTCP: make(map[string]net.Conn),
		nextPort:   relayStart,
	}
}

func (s *Server) Start() error {
	errCh := make(chan error, 4)

	go func() {
		log.Printf("HTTP proxy on :%s", s.httpPort)
		mux := http.NewServeMux()
		mux.HandleFunc("/", s.handleHTTP)
		if err := http.ListenAndServe(":"+s.httpPort, mux); err != nil {
			errCh <- fmt.Errorf("http: %w", err)
		}
	}()

	go func() {
		log.Printf("Control on :%s", s.ctrlPort)
		ln, err := net.Listen("tcp", ":"+s.ctrlPort)
		if err != nil {
			errCh <- err
			return
		}
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("ctrl accept: %v", err)
				continue
			}
			go s.handleControl(conn)
		}
	}()

	go func() {
		log.Printf("Relay data on :%s", s.relayPort)
		ln, err := net.Listen("tcp", ":"+s.relayPort)
		if err != nil {
			errCh <- err
			return
		}
		defer ln.Close()
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("relay accept: %v", err)
				continue
			}
			go s.handleRelayConn(conn)
		}
	}()

	return <-errCh
}

func (s *Server) allocatePort() (int, error) {
	if s.nextPort > s.relayEnd {
		s.nextPort = s.relayStart
	}
	port := s.nextPort
	s.nextPort++
	return port, nil
}

func (s *Server) handleControl(conn net.Conn) {
	defer conn.Close()
	addr := conn.RemoteAddr().String()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var reg RegisterRequest
	if err := dec.Decode(&reg); err != nil {
		return
	}
	if reg.Type != "register" {
		enc.Encode(RegisterResponse{Type: "registered", Success: false, Error: "bad msg type"})
		return
	}

	domain := strings.TrimSpace(reg.Domain)
	if domain == "" {
		var err error
		domain, err = randomSubdomain()
		if err != nil {
			enc.Encode(RegisterResponse{Type: "registered", Success: false, Error: "internal"})
			return
		}
	} else if !isValidSubdomain(domain) {
		enc.Encode(RegisterResponse{Type: "registered", Success: false, Error: "invalid subdomain"})
		return
	}

	s.mu.Lock()
	if _, ok := s.tunnels[domain]; ok {
		s.mu.Unlock()
		enc.Encode(RegisterResponse{Type: "registered", Success: false, Error: fmt.Sprintf("'%s' taken", domain)})
		return
	}
	relayPort, err := s.allocatePort()
	if err != nil {
		s.mu.Unlock()
		enc.Encode(RegisterResponse{Type: "registered", Success: false, Error: "no ports"})
		return
	}

	client := &tunnelClient{
		encoder:   enc,
		decoder:   dec,
		ctrl:      conn,
		domain:    domain,
		relayPort: relayPort,
	}
	s.tunnels[domain] = client
	s.mu.Unlock()

	fullDomain := fmt.Sprintf("%s.%s", domain, s.baseDomain)
	httpURL := fmt.Sprintf("https://%s", fullDomain)

	enc.Encode(RegisterResponse{
		Type:      "registered",
		URL:       httpURL,
		RelayPort: relayPort,
		Success:   true,
	})

	log.Printf("TUNNEL %s -> %s (relay port %d)", fullDomain, addr, relayPort)

	go s.listenTCPRelay(client, relayPort)

	defer func() {
		s.mu.Lock()
		delete(s.tunnels, domain)
		s.mu.Unlock()
		log.Printf("CLOSE %s", fullDomain)
	}()

	for {
		var resp HttpResponseMsg
		if err := dec.Decode(&resp); err != nil {
			return
		}
		if resp.Type == "response" {
			s.mu.Lock()
			p, ok := s.pending[resp.ID]
			if ok {
				delete(s.pending, resp.ID)
			}
			s.mu.Unlock()
			if ok {
				select {
				case p.ch <- &resp:
				default:
				}
			}
		}
	}
}

func (s *Server) listenTCPRelay(client *tunnelClient, port int) {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Printf("relay listen %d: %v", port, err)
		return
	}
	defer ln.Close()

	addr := fmt.Sprintf("%s:%d", s.baseDomain, port)
	log.Printf("TCP relay active: %s -> tunnel %s", addr, client.domain)

	for {
		extConn, err := ln.Accept()
		if err != nil {
			return
		}

		connID := generateID()
		fromAddr := extConn.RemoteAddr().String()

		s.mu.Lock()
		s.pendingTCP[connID] = extConn
		s.mu.Unlock()

		msg := TcpOpenMsg{
			Type:     "tcp_open",
			ConnID:   connID,
			FromAddr: fromAddr,
		}
		if err := client.encoder.Encode(msg); err != nil {
			s.mu.Lock()
			delete(s.pendingTCP, connID)
			s.mu.Unlock()
			extConn.Close()
			return
		}
	}
}

func (s *Server) handleRelayConn(conn net.Conn) {
	defer conn.Close()

	var msg TcpAcceptMsg
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		return
	}
	if msg.Type != "tcp_accept" {
		return
	}

	s.mu.Lock()
	extConn, ok := s.pendingTCP[msg.ConnID]
	if ok {
		delete(s.pendingTCP, msg.ConnID)
	}
	s.mu.Unlock()

	if !ok {
		return
	}

	log.Printf("TCP relay connected: %s", msg.ConnID[:12])

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(extConn, conn)
		extConn.Close()
		wg.Done()
	}()
	go func() {
		io.Copy(conn, extConn)
		conn.Close()
		wg.Done()
	}()
	wg.Wait()
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "" {
		http.Error(w, "no host", 400)
		return
	}

	domain := extractSubdomain(host, s.baseDomain)
	baseOnly := (domain == "")

	if baseOnly {
		s.serveStatic(w, r)
		return
	}

	s.mu.RLock()
	client, ok := s.tunnels[domain]
	s.mu.RUnlock()
	if !ok {
		http.Error(w, fmt.Sprintf("tunnel '%s' not found", domain), 404)
		return
	}

	body, _ := io.ReadAll(r.Body)
	id := generateID()

	headers := make(map[string]string)
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}

	ch := make(chan *HttpResponseMsg, 1)
	s.mu.Lock()
	s.pending[id] = &pendingRequest{ch: ch}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	req := &HttpRequestMsg{
		Type: "request", ID: id, Method: r.Method,
		Path: r.URL.RequestURI(), Host: host,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(body),
	}
	if err := client.encoder.Encode(req); err != nil {
		http.Error(w, "tunnel down", 502)
		return
	}

	select {
	case resp := <-ch:
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(resp.Status)
		b, _ := base64.StdEncoding.DecodeString(resp.Body)
		w.Write(b)
	case <-time.After(60 * time.Second):
		http.Error(w, "timeout", 504)
	}
}



func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" || path == "index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Afisla - Expose Your Local Server to the Internet</title>
<meta name="description" content="Afisla - HTTP & TCP tunnel. Expose your local server to the internet via a public URL. Free, fast, and secure.">
<meta name="keywords" content="tunnel, ngrok alternative, expose localhost, tcp tunnel, http tunnel, free tunnel, afisla">
<meta property="og:title" content="Afisla - Expose Your Local Server">
<meta property="og:description" content="HTTP & TCP tunnel. Free, fast, secure. Expose localhost to internet.">
<meta property="og:type" content="website">
<meta property="og:url" content="https://afisla.web.id">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>⚡</text></svg>">
<style>
*{margin:0;padding:0;box-sizing:border-box}
:root{
--blue-50:#eff6ff;--blue-100:#dbeafe;--blue-200:#bfdbfe;--blue-300:#93c5fd;
--blue-400:#60a5fa;--blue-500:#3b82f6;--blue-600:#2563eb;--blue-700:#1d4ed8;
--blue-800:#1e40af;--blue-900:#1e3a5f;
--dark-900:#0a0e1a;--dark-800:#0f1629;--dark-700:#151d33;
--glass:rgba(255,255,255,0.05);--glass-border:rgba(255,255,255,0.08);
}
body{font-family:'Inter',-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:var(--dark-900);color:#e2e8f0;line-height:1.7;overflow-x:hidden}
a{color:var(--blue-400);text-decoration:none;transition:color .2s}
a:hover{color:var(--blue-300)}

/* Animated gradient bg */
.bg-glow{position:fixed;top:0;left:0;width:100%;height:100%;z-index:-1;overflow:hidden}
.bg-glow::before,.bg-glow::after{content:'';position:absolute;border-radius:50%;filter:blur(120px);opacity:.15;animation:float 20s ease-in-out infinite}
.bg-glow::before{width:600px;height:600px;background:var(--blue-600);top:-200px;right:-100px}
.bg-glow::after{width:500px;height:500px;background:var(--blue-400);bottom:-150px;left:-100px;animation-delay:-10s}
@keyframes float{0%,100%{transform:translate(0,0)}50%{transform:translate(40px,-40px)}}

/* Grid overlay */
.grid-overlay{position:fixed;top:0;left:0;width:100%;height:100%;z-index:-1;background-image:linear-gradient(rgba(59,130,246,.03) 1px,transparent 1px),linear-gradient(90deg,rgba(59,130,246,.03) 1px,transparent 1px);background-size:60px 60px}

.container{max-width:1100px;margin:0 auto;padding:0 24px}

/* Nav */
nav{position:fixed;top:0;left:0;right:0;z-index:100;backdrop-filter:blur(20px);background:rgba(10,14,26,.8);border-bottom:1px solid var(--glass-border)}
.nav-inner{max-width:1100px;margin:0 auto;padding:16px 24px;display:flex;align-items:center;justify-content:space-between}
.logo{display:flex;align-items:center;gap:10px;font-size:1.25rem;font-weight:700;color:#fff}
.logo svg{width:32px;height:32px}
.nav-links{display:flex;gap:28px;align-items:center}
.nav-links a{color:#94a3b8;font-size:.9rem;font-weight:500;transition:color .2s}
.nav-links a:hover{color:#fff}
.nav-cta{background:linear-gradient(135deg,var(--blue-600),var(--blue-500));color:#fff !important;padding:8px 20px;border-radius:8px;font-weight:600 !important;box-shadow:0 0 20px rgba(59,130,246,.3);transition:all .3s !important}
.nav-cta:hover{box-shadow:0 0 30px rgba(59,130,246,.5);transform:translateY(-1px)}

/* Hero */
.hero{padding:140px 0 80px;text-align:center}
.hero-badge{display:inline-flex;align-items:center;gap:8px;padding:6px 16px;border-radius:999px;background:rgba(59,130,246,.1);border:1px solid rgba(59,130,246,.2);color:var(--blue-400);font-size:.8rem;font-weight:600;margin-bottom:24px;animation:fadeInUp .6s ease}
.hero h1{font-size:clamp(2.5rem,6vw,4rem);font-weight:800;color:#fff;line-height:1.1;margin-bottom:20px;animation:fadeInUp .6s ease .1s both}
.hero h1 span{background:linear-gradient(135deg,var(--blue-400),var(--blue-300));-webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text}
.hero p{font-size:1.15rem;color:#94a3b8;max-width:600px;margin:0 auto 40px;animation:fadeInUp .6s ease .2s both}
.hero-actions{display:flex;gap:16px;justify-content:center;flex-wrap:wrap;animation:fadeInUp .6s ease .3s both}
.btn{padding:14px 32px;border-radius:12px;font-size:1rem;font-weight:600;cursor:pointer;border:none;transition:all .3s;display:inline-flex;align-items:center;gap:8px}
.btn-primary{background:linear-gradient(135deg,var(--blue-600),var(--blue-500));color:#fff;box-shadow:0 4px 24px rgba(59,130,246,.35)}
.btn-primary:hover{box-shadow:0 4px 32px rgba(59,130,246,.55);transform:translateY(-2px)}
.btn-secondary{background:var(--glass);color:#e2e8f0;border:1px solid var(--glass-border);backdrop-filter:blur(10px)}
.btn-secondary:hover{background:rgba(255,255,255,.1);border-color:rgba(255,255,255,.15)}

/* Code block */
.code-hero{margin-top:48px;animation:fadeInUp .6s ease .4s both}
.code-block{background:rgba(15,22,41,.9);border:1px solid var(--glass-border);border-radius:16px;padding:24px 28px;text-align:left;max-width:650px;margin:0 auto;backdrop-filter:blur(10px);position:relative;overflow:hidden}
.code-block::before{content:'';position:absolute;top:0;left:0;right:0;height:1px;background:linear-gradient(90deg,transparent,rgba(59,130,246,.5),transparent)}
.code-header{display:flex;align-items:center;gap:8px;margin-bottom:16px}
.code-dot{width:10px;height:10px;border-radius:50%}
.code-dot:nth-child(1){background:#ff5f57}
.code-dot:nth-child(2){background:#febc2e}
.code-dot:nth-child(3){background:#28c840}
.code-block code{font-family:'JetBrains Mono','Fira Code',monospace;font-size:.9rem;color:var(--blue-300);display:block;white-space:pre-wrap;word-break:break-all}
.code-block .prompt{color:#64748b}
.code-block .cmd{color:var(--blue-300)}
.code-block .flag{color:#a78bfa}
.code-block .val{color:#34d399}
.code-block .output{color:#fbbf24}

/* Sections */
section{padding:80px 0}
.section-label{display:inline-flex;align-items:center;gap:6px;padding:6px 14px;border-radius:999px;background:rgba(59,130,246,.08);border:1px solid rgba(59,130,246,.15);color:var(--blue-400);font-size:.75rem;font-weight:600;text-transform:uppercase;letter-spacing:.05em;margin-bottom:16px}
.section-title{font-size:clamp(1.8rem,4vw,2.5rem);font-weight:800;color:#fff;margin-bottom:12px}
.section-desc{color:#94a3b8;font-size:1.05rem;max-width:600px}

/* Architecture */
.arch-card{background:var(--glass);border:1px solid var(--glass-border);border-radius:20px;padding:40px;margin-top:48px;backdrop-filter:blur(10px);position:relative;overflow:hidden}
.arch-card::before{content:'';position:absolute;top:0;left:0;right:0;height:1px;background:linear-gradient(90deg,transparent,rgba(59,130,246,.4),transparent)}
.arch-diagram{background:rgba(10,14,26,.8);border:1px solid rgba(59,130,246,.15);border-radius:12px;padding:24px;margin-top:24px;font-family:'JetBrains Mono',monospace;font-size:.8rem;color:var(--blue-300);line-height:1.8;overflow-x:auto;white-space:pre}

/* Features grid */
.features-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:20px;margin-top:40px}
.feature-card{background:var(--glass);border:1px solid var(--glass-border);border-radius:16px;padding:28px;transition:all .3s;position:relative;overflow:hidden}
.feature-card::before{content:'';position:absolute;top:0;left:0;right:0;height:1px;background:linear-gradient(90deg,transparent,rgba(59,130,246,.3),transparent);opacity:0;transition:opacity .3s}
.feature-card:hover{border-color:rgba(59,130,246,.25);transform:translateY(-4px);box-shadow:0 12px 40px rgba(0,0,0,.3)}
.feature-card:hover::before{opacity:1}
.feature-icon{width:44px;height:44px;border-radius:12px;background:rgba(59,130,246,.1);border:1px solid rgba(59,130,246,.15);display:flex;align-items:center;justify-content:center;font-size:1.3rem;margin-bottom:16px}
.feature-card h3{font-size:1.05rem;font-weight:700;color:#fff;margin-bottom:8px}
.feature-card p{font-size:.9rem;color:#94a3b8;line-height:1.6}

/* Quick Start */
.steps{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:24px;margin-top:40px}
.step{background:var(--glass);border:1px solid var(--glass-border);border-radius:16px;padding:32px;position:relative}
.step-num{position:absolute;top:20px;right:20px;width:36px;height:36px;border-radius:10px;background:rgba(59,130,246,.1);border:1px solid rgba(59,130,246,.2);display:flex;align-items:center;justify-content:center;font-size:.85rem;font-weight:700;color:var(--blue-400)}
.step h3{font-size:1.15rem;font-weight:700;color:#fff;margin-bottom:12px;display:flex;align-items:center;gap:10px}
.step pre{background:rgba(10,14,26,.8);border:1px solid rgba(59,130,246,.1);border-radius:10px;padding:16px;font-family:'JetBrains Mono',monospace;font-size:.82rem;color:var(--blue-300);overflow-x:auto;margin-top:12px;line-height:1.6}

/* TCP section */
.tcp-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(320px,1fr));gap:24px;margin-top:40px}
.tcp-card{background:var(--glass);border:1px solid var(--glass-border);border-radius:16px;padding:32px}
.tcp-card h3{font-size:1.1rem;font-weight:700;color:#fff;margin-bottom:6px;display:flex;align-items:center;gap:8px}
.tcp-card .subtitle{font-size:.85rem;color:#64748b;margin-bottom:16px}
.tcp-card pre{background:rgba(10,14,26,.8);border:1px solid rgba(59,130,246,.1);border-radius:10px;padding:14px;font-family:'JetBrains Mono',monospace;font-size:.78rem;color:var(--blue-300);overflow-x:auto;line-height:1.7}

/* Options */
.options-wrapper{display:grid;grid-template-columns:repeat(auto-fit,minmax(380px,1fr));gap:24px;margin-top:40px}
.option-table{background:var(--glass);border:1px solid var(--glass-border);border-radius:16px;overflow:hidden}
.option-table-header{padding:20px 24px;border-bottom:1px solid var(--glass-border);font-weight:700;color:#fff;font-size:1rem}
.option-row{display:flex;padding:12px 24px;border-bottom:1px solid rgba(255,255,255,.03);transition:background .2s}
.option-row:last-child{border-bottom:none}
.option-row:hover{background:rgba(59,130,246,.04)}
.option-name{font-family:'JetBrains Mono',monospace;font-size:.82rem;color:var(--blue-400);min-width:160px;flex-shrink:0}
.option-desc{font-size:.85rem;color:#94a3b8}

/* Install */
.install-section{text-align:center}
.install-methods{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:20px;margin-top:40px;max-width:800px;margin-left:auto;margin-right:auto}
.install-card{background:var(--glass);border:1px solid var(--glass-border);border-radius:16px;padding:32px;text-align:left}
.install-card h3{font-size:1rem;font-weight:700;color:#fff;margin-bottom:12px}
.install-card pre{background:rgba(10,14,26,.8);border:1px solid rgba(59,130,246,.1);border-radius:10px;padding:14px;font-family:'JetBrains Mono',monospace;font-size:.8rem;color:var(--blue-300);overflow-x:auto;line-height:1.6}
.install-links{margin-top:16px;display:flex;gap:12px;flex-wrap:wrap}
.install-links a{display:inline-flex;align-items:center;gap:6px;padding:8px 16px;border-radius:8px;background:rgba(59,130,246,.08);border:1px solid rgba(59,130,246,.15);font-size:.85rem;color:var(--blue-400);font-weight:500;transition:all .2s}
.install-links a:hover{background:rgba(59,130,246,.15);border-color:rgba(59,130,246,.3)}

/* Footer */
footer{padding:40px 0;border-top:1px solid var(--glass-border);text-align:center;color:#64748b;font-size:.85rem}
footer a{color:var(--blue-400)}

/* Animations */
@keyframes fadeInUp{from{opacity:0;transform:translateY(24px)}to{opacity:1;transform:translateY(0)}}

/* Responsive */
@media(max-width:768px){
  .nav-links a:not(.nav-cta){display:none}
  .hero{padding:120px 0 60px}
  .hero h1{font-size:2rem}
  .features-grid,.steps,.tcp-grid,.options-wrapper,.install-methods{grid-template-columns:1fr}
  .arch-diagram{font-size:.65rem}
  section{padding:60px 0}
}

/* Scroll reveal */
.reveal{opacity:0;transform:translateY(30px);transition:all .6s ease}
.reveal.visible{opacity:1;transform:translateY(0)}

/* Copy button */
.copy-btn{position:absolute;top:12px;right:12px;background:rgba(59,130,246,.15);border:1px solid rgba(59,130,246,.25);color:var(--blue-400);padding:6px 12px;border-radius:8px;font-size:.75rem;cursor:pointer;font-weight:600;transition:all .2s;z-index:2}
.copy-btn:hover{background:rgba(59,130,246,.25)}
.copy-btn.copied{background:rgba(52,211,153,.15);border-color:rgba(52,211,153,.25);color:#34d399}
</style>
</head>
<body>

<div class="bg-glow"></div>
<div class="grid-overlay"></div>

<nav>
  <div class="nav-inner">
    <div class="logo">
      <svg viewBox="0 0 32 32" fill="none"><rect width="32" height="32" rx="8" fill="url(#g1)"/><path d="M8 16l5-8h6l5 8-5 8h-6z" fill="#fff" fill-opacity=".9"/><defs><linearGradient id="g1" x1="0" y1="0" x2="32" y2="32"><stop stop-color="#2563eb"/><stop offset="1" stop-color="#3b82f6"/></linearGradient></defs></svg>
      Afisla
    </div>
    <div class="nav-links">
      <a href="#features">Features</a>
      <a href="#quickstart">Quick Start</a>
      <a href="#tcp">TCP Relay</a>
      <a href="#install" class="nav-cta">Get Started</a>
    </div>
  </div>
</nav>

<section class="hero">
  <div class="container">
    <div class="hero-badge">&#9889; Free &amp; Open Source Tunnel</div>
    <h1>Expose Your Local Server<br>to the <span>Internet</span></h1>
    <p>Afisla is an HTTP &amp; TCP tunnel that exposes your local server to the internet via a public URL. No signup required.</p>
    <div class="hero-actions">
      <a href="#install" class="btn btn-primary">&#9889; Install Now</a>
      <a href="#quickstart" class="btn btn-secondary">&#9654; Quick Start</a>
    </div>
    <div class="code-hero">
      <div class="code-block">
        <button class="copy-btn" onclick="copyCode(this,'afisla client --port-local 8080 --domain testing')">Copy</button>
        <div class="code-header"><span class="code-dot"></span><span class="code-dot"></span><span class="code-dot"></span></div>
        <code><span class="prompt">$</span> <span class="cmd">afisla client</span> <span class="flag">--port-local</span> <span class="val">8080</span> <span class="flag">--domain</span> <span class="val">testing</span>
<span class="output">&#10132; https://testing.afisla.web.id</span>   <span style="color:#64748b">(HTTP via Cloudflare)</span>
<span class="output">&#10132; relay.afisla.web.id:30000</span>      <span style="color:#64748b">(TCP relay direct)</span></code>
      </div>
    </div>
  </div>
</section>

<section id="features">
  <div class="container">
    <div class="section-label">&#10024; Features</div>
    <h2 class="section-title">Everything You Need</h2>
    <p class="section-desc">Full-featured tunnel with HTTP &amp; TCP support, powered by Cloudflare.</p>
    <div class="features-grid">
      <div class="feature-card reveal">
        <div class="feature-icon">&#128274;</div>
        <h3>HTTP / HTTPS</h3>
        <p>Route by subdomain via Cloudflare + Apache. All HTTP methods, headers, and bodies supported.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#128268;</div>
        <h3>TCP Relay</h3>
        <p>Raw TCP forwarding for SSH, RDP, databases, game servers, and more.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#127760;</div>
        <h3>Custom Domain</h3>
        <p>Request a specific subdomain with <code>--domain</code>. Your brand, your tunnel.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#128171;</div>
        <h3>Random Domain</h3>
        <p>Auto-generated 6-char subdomain if no custom domain specified.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#9881;</div>
        <h3>Concurrent</h3>
        <p>Multiple HTTP requests handled concurrently per tunnel. No bottlenecks.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#128272;</div>
        <h3>TLS Encryption</h3>
        <p>HTTPS via Cloudflare edge + wildcard Let's Encrypt certificate.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#128737;</div>
        <h3>Cloudflare Proxy</h3>
        <p>HTTP/HTTPS proxied through Cloudflare for DDoS protection and caching.</p>
      </div>
      <div class="feature-card reveal">
        <div class="feature-icon">&#128279;</div>
        <h3>Dual-Domain</h3>
        <p>HTTP via <code>*.afisla.web.id</code> (proxied), tunnel via <code>relay.afisla.web.id</code> (direct).</p>
      </div>
    </div>
  </div>
</section>

<section id="architecture" style="background:rgba(30,58,95,.08)">
  <div class="container">
    <div class="section-label">&#128736; Architecture</div>
    <h2 class="section-title">How It Works</h2>
    <p class="section-desc">Dual-domain architecture with Cloudflare proxy for HTTP and direct connection for TCP.</p>
    <div class="arch-card reveal">
      <div class="arch-diagram">
Browser &#9654;&#9654; testing.afisla.web.id &#9654;&#9654; Cloudflare (orange cloud)
          (port 443)                       &#9484;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9488;
                                       &#9474; *.afisla.web.id      &#9474;
                                       &#9474; proxied via CF       &#9474;
                                       &#9492;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9496;
                                                  &#9474;
 Client (NAT)                    Apache :80/443  &#9660;  Server (public)
&#9484;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9488;              &#9484;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9488;
&#9474;  localhost:8080   &#9474;              &#9474;  afisla server           &#9474;
&#9474;  (app/service)    &#9474;   control   &#9474;                           &#9474;
&#9474;         &#9492;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9524;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472; &#9664;&#9472;&#9472;&#9472;6673 &#9472;&#9472;&#9472;&#9472;&#9472; &#9474;  HTTP :3376 &#8592; Apache      &#9474;
&#9474;                   &#9474;   relay     &#9474;  Ctrl :6673               &#9474;
&#9474;         &#9492;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9524;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472; &#9664;&#9472;&#9472;&#9472;6674 &#9472;&#9472;&#9472;&#9472;&#9472; &#9474;  Relay:6674               &#9474;
&#9474;                   &#9474;             &#9474;  TCP  :30000-40000        &#9474;
&#9492;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9496;             &#9492;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9496;
         &#9650;                                &#9474;
         &#9474;  relay.afisla.web.id:30000     &#9474;
         &#9492;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472; direct (no proxy) &#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9472;&#9496;</div>
    </div>
  </div>
</section>

<section id="quickstart">
  <div class="container">
    <div class="section-label">&#9654; Quick Start</div>
    <h2 class="section-title">Up and Running in Seconds</h2>
    <p class="section-desc">Start tunneling your local server with a single command.</p>
    <div class="steps">
      <div class="step reveal">
        <span class="step-num">1</span>
        <h3>&#128187; Client</h3>
        <p style="color:#94a3b8;font-size:.9rem;margin-bottom:8px">Basic - random subdomain:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">afisla client</span> <span style="color:#a78bfa">--port-local</span> <span style="color:#34d399">3000</span></pre>
        <p style="color:#94a3b8;font-size:.9rem;margin:12px 0 8px">Custom subdomain:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">afisla client</span> <span style="color:#a78bfa">--port-local</span> <span style="color:#34d399">8080</span> <span style="color:#a78bfa">--domain</span> <span style="color:#34d399">testing</span>
<span style="color:#fbbf24">&#10132; https://testing.afisla.web.id</span></pre>
      </div>
      <div class="step reveal">
        <span class="step-num">2</span>
        <h3>&#9729;&#65039; Server (Already Running)</h3>
        <p style="color:#94a3b8;font-size:.9rem;margin-bottom:8px">The server is already running at afisla.web.id:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">afisla server</span> <span style="color:#a78bfa">--base-domain</span> <span style="color:#34d399">afisla.web.id</span></pre>
      </div>
    </div>
  </div>
</section>

<section id="tcp" style="background:rgba(30,58,95,.08)">
  <div class="container">
    <div class="section-label">&#128268; TCP Relay</div>
    <h2 class="section-title">Raw TCP Forwarding</h2>
    <p class="section-desc">Each tunnel gets an assigned TCP relay port (30000-40000) on relay.afisla.web.id.</p>
    <div class="tcp-grid">
      <div class="tcp-card reveal">
        <h3>&#128274; SSH</h3>
        <p class="subtitle">Expose and access SSH from anywhere</p>
        <p style="color:#94a3b8;font-size:.85rem;margin-bottom:8px">On the machine behind NAT:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">afisla client</span> <span style="color:#a78bfa">--port-local</span> <span style="color:#34d399">22</span> <span style="color:#a78bfa">--domain</span> <span style="color:#34d399">myserver</span>
<span style="color:#fbbf24">&#10132; TCP: relay.afisla.web.id:30000</span></pre>
        <p style="color:#94a3b8;font-size:.85rem;margin:12px 0 8px">SSH from anywhere:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">ssh</span> <span style="color:#a78bfa">-o ProxyCommand='nc relay.afisla.web.id 30000'</span> <span style="color:#34d399">user@localhost</span> <span style="color:#a78bfa">-p</span> <span style="color:#34d399">22</span></pre>
      </div>
      <div class="tcp-card reveal">
        <h3>&#128421; Any TCP Service</h3>
        <p class="subtitle">Databases, game servers, RDP, and more</p>
        <p style="color:#94a3b8;font-size:.85rem;margin-bottom:8px">Expose any local port:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">afisla client</span> <span style="color:#a78bfa">--port-local</span> <span style="color:#34d399">5432</span> <span style="color:#a78bfa">--domain</span> <span style="color:#34d399">postgres</span></pre>
        <p style="color:#94a3b8;font-size:.85rem;margin:12px 0 8px">Connect remotely via relay:</p>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">psql</span> <span style="color:#a78bfa">-h</span> <span style="color:#34d399">relay.afisla.web.id</span> <span style="color:#a78bfa">-p</span> <span style="color:#34d399">30000</span> <span style="color:#a78bfa">-U</span> <span style="color:#34d399">user</span> <span style="color:#34d399">db</span></pre>
      </div>
    </div>
  </div>
</section>

<section id="options">
  <div class="container">
    <div class="section-label">&#9881; Options</div>
    <h2 class="section-title">Configuration</h2>
    <p class="section-desc">Fine-tune your tunnel with these options.</p>
    <div class="options-wrapper">
      <div class="option-table reveal">
        <div class="option-table-header">&#127760; Server Options</div>
        <div class="option-row"><span class="option-name">--http-port</span><span class="option-desc">HTTP proxy port (default 3376)</span></div>
        <div class="option-row"><span class="option-name">--ctrl-port</span><span class="option-desc">Control port (default 6673)</span></div>
        <div class="option-row"><span class="option-name">--relay-port</span><span class="option-desc">Relay data port (default 6674)</span></div>
        <div class="option-row"><span class="option-name">--base-domain</span><span class="option-desc">Base domain (default afisla.web.id)</span></div>
        <div class="option-row"><span class="option-name">--relay-start</span><span class="option-desc">TCP port range start (default 30000)</span></div>
        <div class="option-row"><span class="option-name">--relay-end</span><span class="option-desc">TCP port range end (default 40000)</span></div>
      </div>
      <div class="option-table reveal">
        <div class="option-table-header">&#128187; Client Options</div>
        <div class="option-row"><span class="option-name">--port-local</span><span class="option-desc">Local port to forward (default 8000)</span></div>
        <div class="option-row"><span class="option-name">--domain</span><span class="option-desc">Requested subdomain (random if empty)</span></div>
        <div class="option-row"><span class="option-name">--host-tunnel</span><span class="option-desc">Tunnel relay host (default relay.afisla.web.id)</span></div>
        <div class="option-row"><span class="option-name">--tunnel-http-port</span><span class="option-desc">Server HTTP port (default 443)</span></div>
        <div class="option-row"><span class="option-name">--ctrl-port</span><span class="option-desc">Server control port (default 6673)</span></div>
        <div class="option-row"><span class="option-name">--relay-port</span><span class="option-desc">Server relay data port (default 6674)</span></div>
      </div>
    </div>
  </div>
</section>

<section id="install" class="install-section" style="background:rgba(30,58,95,.08)">
  <div class="container">
    <div class="section-label">&#10024; Install</div>
    <h2 class="section-title">Get Started Now</h2>
    <p class="section-desc" style="margin:0 auto">One command to install, one command to tunnel.</p>
    <div class="install-methods">
      <div class="install-card reveal">
        <h3>&#128187; Quick Install (Recommended)</h3>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">curl</span> <span style="color:#a78bfa">-fsSL</span> <span style="color:#34d399">https://afisla.web.id/install.sh</span> | <span style="color:var(--blue-300)">bash</span></pre>
      </div>
      <div class="install-card reveal">
        <h3>&#128230; Manual Install</h3>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">sudo curl</span> <span style="color:#a78bfa">-fsSLo</span> <span style="color:#34d399">/usr/local/bin/afisla</span> \
    <span style="color:#34d399">https://github.com/afisla/tunnel/releases/latest/download/afisla-linux-amd64</span>
<span style="color:#64748b">$</span> <span style="color:var(--blue-300)">sudo chmod</span> <span style="color:#a78bfa">+x</span> <span style="color:#34d399">/usr/local/bin/afisla</span></pre>
        <div class="install-links">
          <a href="/afisla-linux-amd64">&#8595; Linux AMD64</a>
          <a href="/afisla-linux-arm64">&#8595; Linux ARM64</a>
        </div>
      </div>
      <div class="install-card reveal" style="grid-column:1/-1;max-width:400px;margin:0 auto">
        <h3>&#128640; Build from Source</h3>
        <pre><span style="color:#64748b">$</span> <span style="color:var(--blue-300)">git clone</span> <span style="color:#34d399">https://github.com/afisla/tunnel</span> <span style="color:#34d399">afisla</span>
<span style="color:#64748b">$</span> <span style="color:var(--blue-300)">cd</span> <span style="color:#34d399">afisla</span>
<span style="color:#64748b">$</span> <span style="color:var(--blue-300)">go build</span> <span style="color:#a78bfa">-o</span> <span style="color:#34d399">afisla</span> <span style="color:#34d399">.</span></pre>
        <p style="color:#64748b;font-size:.8rem;margin-top:10px">Requires Go 1.21+</p>
      </div>
    </div>
  </div>
</section>

<footer>
  <div class="container">
    <p>Afisla &mdash; HTTP &amp; TCP Tunnel &mdash; <a href="https://github.com/afisla/tunnel">GitHub</a></p>
  </div>
</footer>

<script>
// Copy button
function copyCode(btn, text) {
  navigator.clipboard.writeText(text).then(() => {
    btn.textContent = 'Copied!';
    btn.classList.add('copied');
    setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('copied'); }, 2000);
  });
}

// Scroll reveal
const observer = new IntersectionObserver((entries) => {
  entries.forEach(e => { if (e.isIntersecting) { e.target.classList.add('visible'); } });
}, { threshold: 0.1 });
document.querySelectorAll('.reveal').forEach(el => observer.observe(el));

// Smooth scroll
document.querySelectorAll('a[href^="#"]').forEach(a => {
  a.addEventListener('click', e => {
    e.preventDefault();
    const t = document.querySelector(a.getAttribute('href'));
    if (t) t.scrollIntoView({ behavior: 'smooth', block: 'start' });
  });
});
</script>
</body>
</html>`))
		return
	}

	if path == "install.sh" {
		data, err := os.ReadFile(s.staticDir + "/install.sh")
		if err == nil {
			w.Header().Set("Content-Type", "text/x-sh; charset=utf-8")
			w.Write(data)
			return
		}
		http.NotFound(w, r)
		return
	}

	if path == "README.md" {
		data, err := os.ReadFile(s.staticDir + "/README.md")
		if err == nil {
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Write(data)
			return
		}
		http.NotFound(w, r)
		return
	}

	if strings.HasPrefix(path, "afisla-") {
		arch := strings.TrimPrefix(path, "afisla-")
		data, err := os.ReadFile(s.staticDir + "/afisla-" + arch)
		if err == nil {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="afisla"`)
			w.Write(data)
			return
		}
		http.NotFound(w, r)
		return
	}

	http.NotFound(w, r)
}
