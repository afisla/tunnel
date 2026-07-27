package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

func randomSubdomain() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		b[i] = charset[n.Int64()]
	}
	return string(b), nil
}

func isValidSubdomain(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func extractSubdomain(host, baseDomain string) string {
	host = strings.Split(host, ":")[0]
	base := strings.TrimPrefix(baseDomain, ".")
	if !strings.HasSuffix(host, "."+base) && host != base {
		return ""
	}
	if host == base {
		return ""
	}
	return strings.TrimSuffix(host, "."+base)
}

var idMu sync.Mutex
var idCounter int64

func generateID() string {
	idMu.Lock()
	idCounter++
	counter := idCounter
	idMu.Unlock()
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%d-%x", time.Now().UnixNano()+counter, b)
}
