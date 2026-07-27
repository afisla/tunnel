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
		w.Write([]byte(`<!DOCTYPE html><html><head><title>Afisla Tunnel</title><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><style>body{font-family:-apple-system,sans-serif;max-width:600px;margin:50px auto;padding:0 20px;line-height:1.6}h1{color:#1a73e8}code{background:#f0f0f0;padding:2px 6px;border-radius:3px}pre{background:#f0f0f0;padding:16px;border-radius:6px;overflow-x:auto}</style></head><body><h1>Afisla Tunnel</h1><p>HTTP &amp; TCP tunnel server running.</p><h2>Install Client</h2><pre>curl -fsSL https://afisla.web.id/install.sh | bash</pre><p>Or download the binary directly:</p><ul><li><a href="/afisla-linux-amd64">afisla-linux-amd64</a></li><li><a href="/afisla-linux-arm64">afisla-linux-arm64</a></li></ul></body></html>`))
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
