package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDefaultNetworkIgnoresExplicitAndEnvironmentProxies(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(key, "http://127.0.0.1:1")
	}
	transport := newTransport(false)
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil {
		t.Fatal("ordinary requests must use OS routing without an HTTP proxy")
	}
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		_, _ = io.WriteString(w, "connected")
	}))
	defer u.Close()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	resp, err := client.Get(u.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	independent := newTransport(true)
	defer independent.CloseIdleConnections()
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	if independent.Proxy == nil {
		t.Fatal("independent harvest proxy lost")
	}
	proxy, err := independent.Proxy(req)
	if err != nil || proxy == nil || proxy.Host != probeAddr || proxy.Scheme != "http" {
		t.Fatal("independent harvest must retain its dedicated proxy")
	}
}

func TestNetworkErrorDiagnosticsPreserveCauseWithoutSecrets(t *testing.T) {
	r := testRelay(t)
	secret := "private-credential-and-prompt"
	cause := &url.Error{Op: "Post", URL: "https://user:" + secret + "@example.com/?token=" + secret,
		Err: &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}}
	r.transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, cause })
	q := sessionRequest("diagnostic-session", "test")
	q.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != http.StatusBadGateway {
		t.Fatal("transport failure must remain HTTP 502")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(r.settingsPath()), "network-errors.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), "example.com") || strings.Contains(w.Body.String(), secret) {
		t.Fatal("diagnostics leaked private request data")
	}
	var entry map[string]any
	if err := json.Unmarshal(b, &entry); err != nil {
		t.Fatal(err)
	}
	if entry["errno"] != float64(syscall.ECONNRESET) || entry["operation"] != "read" || entry["elapsed_ms"] == nil {
		t.Fatal("typed transport cause or elapsed time lost")
	}
	if networkErrorDetails(fmt.Errorf("wrapped: %w", context.DeadlineExceeded))["timeout"] != true {
		t.Fatal("wrapped timeout not identified")
	}
}
