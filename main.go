package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "client":
		runClient(os.Args[2:])
	case "version":
		fmt.Println("afisla 0.4.0")
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`afisla - HTTP & TCP tunnel

Usage:
  afisla server [options]
  afisla client [options]
  afisla version`)
}

func runServer(args []string) {
	httpPort := "3376"
	ctrlPort := "6673"
	relayPort := "6674"
	baseDomain := "afisla.web.id"
	relayStart := 30000
	relayEnd := 40000
	staticDir := "/home/ubuntu/afisla"

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--http-port" || args[i] == "-http-port":
			if i+1 < len(args) { i++; httpPort = args[i] }
		case args[i] == "--ctrl-port" || args[i] == "--tcp-port":
			if i+1 < len(args) { i++; ctrlPort = args[i] }
		case args[i] == "--relay-port":
			if i+1 < len(args) { i++; relayPort = args[i] }
		case args[i] == "--base-domain":
			if i+1 < len(args) { i++; baseDomain = args[i] }
		case args[i] == "--relay-start":
			if i+1 < len(args) { i++; fmt.Sscanf(args[i], "%d", &relayStart) }
		case args[i] == "--relay-end":
			if i+1 < len(args) { i++; fmt.Sscanf(args[i], "%d", &relayEnd) }
		case args[i] == "--static-dir":
			if i+1 < len(args) { i++; staticDir = args[i] }
		case args[i] == "-h" || args[i] == "--help":
			fmt.Println(`Server options:
  --http-port     HTTP proxy port (default 3376)
  --ctrl-port     Control port (default 6673)
  --relay-port    Relay data port (default 6674)
  --base-domain   Base domain (default afisla.web.id)
  --relay-start   TCP relay port range start (default 30000)
  --relay-end     TCP relay port range end (default 40000)
  --static-dir    Static file directory for downloads (default /home/ubuntu/afisla)`)
			return
		}
	}

	srv := NewServer(httpPort, ctrlPort, relayPort, baseDomain, relayStart, relayEnd, staticDir)
	log.Printf("Server starting (HTTP:%s Ctrl:%s Relay:%s Domain:%s)", httpPort, ctrlPort, relayPort, baseDomain)
	if err := srv.Start(); err != nil {
		log.Fatalf("Server: %v", err)
	}
}

func runClient(args []string) {
	tunnelHost := "relay.afisla.web.id"
	tunnelHTTP := "443"
	tunnelCtrl := "6673"
	tunnelRelay := "6674"
	localPort := "8000"
	domain := ""

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--host-tunnel" || args[i] == "--tunnel-host":
			if i+1 < len(args) { i++; tunnelHost = args[i] }
		case args[i] == "--tunnel-http-port":
			if i+1 < len(args) { i++; tunnelHTTP = args[i] }
		case args[i] == "--tunnel-tcp-port" || args[i] == "--ctrl-port":
			if i+1 < len(args) { i++; tunnelCtrl = args[i] }
		case args[i] == "--relay-port":
			if i+1 < len(args) { i++; tunnelRelay = args[i] }
		case args[i] == "--local-port" || args[i] == "--port-local":
			if i+1 < len(args) { i++; localPort = args[i] }
		case args[i] == "--domain":
			if i+1 < len(args) { i++; domain = args[i] }
		case args[i] == "-h" || args[i] == "--help":
			fmt.Println(`Client options:
  --host-tunnel      Tunnel relay host (default relay.afisla.web.id)
  --port-local       Local port to forward (default 8000)
  --domain           Requested subdomain (random if empty)
  --tunnel-http-port HTTP port (default 443)
  --ctrl-port        Control port (default 6673)
  --relay-port       Relay data port (default 6674)`)
			return
		}
	}

	client := NewClient(tunnelHost, tunnelHTTP, tunnelCtrl, tunnelRelay, localPort, domain)
	if err := client.Start(); err != nil {
		log.Fatalf("Client: %v", err)
	}
}
