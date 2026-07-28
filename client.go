package main

import (
	"bytes"
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

type Client struct {
	tunnelHost   string
	tunnelHTTP   string
	tunnelCtrl   string
	tunnelRelay  string
	localPort    string
	domain       string
}

func NewClient(tunnelHost, tunnelHTTP, tunnelCtrl, tunnelRelay, localPort, domain string) *Client {
	return &Client{
		tunnelHost:  tunnelHost,
		tunnelHTTP:  tunnelHTTP,
		tunnelCtrl:  tunnelCtrl,
		tunnelRelay: tunnelRelay,
		localPort:   localPort,
		domain:      domain,
	}
}

func (c *Client) Start() error {
	ctrlAddr := net.JoinHostPort(c.tunnelHost, c.tunnelCtrl)
	log.Printf("Connecting to %s", ctrlAddr)

	ctrl, err := net.DialTimeout("tcp", ctrlAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer ctrl.Close()

	enc := json.NewEncoder(ctrl)
	dec := json.NewDecoder(ctrl)

	if err := enc.Encode(RegisterRequest{Type: "register", Domain: c.domain}); err != nil {
		return fmt.Errorf("register send: %w", err)
	}

	var reg RegisterResponse
	if err := dec.Decode(&reg); err != nil {
		return fmt.Errorf("register response: %w", err)
	}
	if !reg.Success {
		return fmt.Errorf("register: %s", reg.Error)
	}

	tunnelURL := reg.URL
	if c.tunnelHTTP != "" && c.tunnelHTTP != "443" {
		tunnelURL = fmt.Sprintf("https://%s.%s:%s", c.domain, c.tunnelHost, c.tunnelHTTP)
	} else if strings.HasPrefix(tunnelURL, "http://") {
		tunnelURL = "https://" + strings.TrimPrefix(tunnelURL, "http://")
		parts := strings.SplitN(tunnelURL, "/", 2)
		hostPort := strings.Split(parts[0], ":")
		if len(hostPort) == 2 {
			tunnelURL = "https://" + hostPort[0]
		} else {
			tunnelURL = "https://" + parts[0]
		}
	}

	fmt.Printf("\n  Tunnel: %s\n", tunnelURL)
	fmt.Printf("  Local:  localhost:%s\n", c.localPort)
	if reg.RelayPort > 0 {
		fmt.Printf("  TCP:    %s:%d (raw TCP relay)\n", c.tunnelHost, reg.RelayPort)
	}
	fmt.Println()

	go c.readControl(ctrl, tunnelURL)

	select {}
}

func (c *Client) readControl(ctrl net.Conn, tunnelURL string) {
	dec := json.NewDecoder(ctrl)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			log.Printf("Control lost: %v", err)
			os.Exit(1)
			return
		}

		var msgType struct{ Type string `json:"type"` }
		json.Unmarshal(raw, &msgType)

		switch msgType.Type {
		case "request":
			var req HttpRequestMsg
			json.Unmarshal(raw, &req)
			go c.handleRequest(ctrl, &req)
		case "tcp_open":
			var msg TcpOpenMsg
			json.Unmarshal(raw, &msg)
			go c.handleTCPOpen(&msg)
		}
	}
}

func (c *Client) handleRequest(ctrl net.Conn, req *HttpRequestMsg) {
	bodyBytes, _ := base64.StdEncoding.DecodeString(req.Body)

	targetURL := fmt.Sprintf("http://127.0.0.1:%s%s", c.localPort, req.Path)
	httpReq, err := http.NewRequest(req.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	httpReq.Host = req.Host

	httpClient := &http.Client{Timeout: 55 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		log.Printf("Proxy error %s %s: %v", req.Method, req.Path, err)
		enc := json.NewEncoder(ctrl)
		enc.Encode(HttpResponseMsg{
			Type: "response", ID: req.ID, Status: 502,
			Headers: map[string]string{"Content-Type": "text/plain"},
			Body:    base64.StdEncoding.EncodeToString([]byte(err.Error())),
		})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	headers := make(map[string]string)
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ", ")
	}

	json.NewEncoder(ctrl).Encode(HttpResponseMsg{
		Type: "response", ID: req.ID, Status: resp.StatusCode,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(respBody),
	})
}

func (c *Client) handleTCPOpen(msg *TcpOpenMsg) {
	localAddr := fmt.Sprintf("127.0.0.1:%s", c.localPort)
	local, err := net.DialTimeout("tcp", localAddr, 10*time.Second)
	if err != nil {
		log.Printf("TCP relay local connect failed (%s): %v", msg.ConnID[:12], err)
		return
	}
	defer local.Close()

	relayAddr := net.JoinHostPort(c.tunnelHost, c.tunnelRelay)
	relay, err := net.DialTimeout("tcp", relayAddr, 10*time.Second)
	if err != nil {
		log.Printf("TCP relay connect %s failed: %v", relayAddr, err)
		return
	}
	defer relay.Close()

	acceptMsg := TcpAcceptMsg{Type: "tcp_accept", ConnID: msg.ConnID}
	if err := json.NewEncoder(relay).Encode(acceptMsg); err != nil {
		return
	}

	log.Printf("TCP relay active: %s -> 127.0.0.1:%s", msg.FromAddr, c.localPort)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		io.Copy(local, relay)
		local.Close()
		wg.Done()
	}()
	go func() {
		io.Copy(relay, local)
		relay.Close()
		wg.Done()
	}()
	wg.Wait()
}
