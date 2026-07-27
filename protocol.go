package main

type RegisterRequest struct {
	Type   string `json:"type"`
	Domain string `json:"domain"`
}

type RegisterResponse struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	RelayPort int    `json:"relay_port,omitempty"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

type HttpRequestMsg struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Host    string            `json:"host"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type HttpResponseMsg struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type TcpOpenMsg struct {
	Type     string `json:"type"`
	ConnID   string `json:"conn_id"`
	FromAddr string `json:"from_addr"`
}

type TcpAcceptMsg struct {
	Type   string `json:"type"`
	ConnID string `json:"conn_id"`
}

type TcpCloseMsg struct {
	Type   string `json:"type"`
	ConnID string `json:"conn_id"`
}
