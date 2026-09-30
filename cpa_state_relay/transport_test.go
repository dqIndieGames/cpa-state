package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// L1: the user's original bytes and headers survive every retry; different
// sessions/accounts must not share an upstream connection.
func TestUpstreamConnectionsAreSessionIsolated(t *testing.T) {
	body := "{ \"model\": \"test\", \"input\": \"same retry bytes\" }\n"
	peers := make(chan string, 8)
	u := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, err := io.ReadAll(q.Body)
		if err != nil || string(b) != body || q.Header.Get("Cookie") != "client=original" || q.Header.Get("X-Codex-Turn-State") != "original-state" {
			t.Error("request changed")
		}
		peers <- q.RemoteAddr
		w.Header().Add("Set-Cookie", "upstream=original; Secure")
		_, _ = io.WriteString(w, "unchanged response")
	}))
	defer u.Close()
	r := testRelay(t)
	r.upstream = u.URL
	transport := newTransport(false)
	transport.Proxy = nil
	transport.TLSClientConfig = u.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	r.transport = transport
	s := httptest.NewServer(r.handler())
	defer s.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	send := func(session, auth string) string {
		t.Helper()
		q, _ := http.NewRequest("POST", s.URL+"/responses", strings.NewReader(body))
		q.Header.Set("Session-Id", session)
		q.Header.Set("Authorization", auth)
		q.Header.Set("Cookie", "client=original")
		q.Header.Set("X-Codex-Turn-State", "original-state")
		resp, err := client.Do(q)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(b) != "unchanged response" || resp.Header.Get("Set-Cookie") != "upstream=original; Secure" {
			t.Fatal("response changed")
		}
		select {
		case peer := <-peers:
			return peer
		case <-time.After(time.Second):
			t.Fatal("upstream not reached")
			return ""
		}
	}
	a := send("a", "Bearer owner")
	b := send("b", "Bearer owner")
	if a == b {
		t.Fatal("different sessions shared one upstream connection")
	}
	if send("a", "Bearer owner") != a {
		t.Fatal("same session lost its connection pool")
	}
	if send("a", "Bearer other-owner") == a {
		t.Fatal("different accounts shared a session connection")
	}
}

func TestWebSocketUpgradePreservesDuplexBytes(t *testing.T) {
	clientFrame := []byte{0x81, 0x82, 1, 2, 3, 4, 'h' ^ 1, 'i' ^ 2}
	serverFrame := []byte{0x81, 2, 'o', 'k'}
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer original" {
			t.Error("upgrade auth changed")
		}
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		digest := sha1.Sum([]byte(q.Header.Get("Sec-Websocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(digest[:]))
		_ = rw.Flush()
		got := make([]byte, len(clientFrame))
		if _, err := io.ReadFull(rw, got); err != nil || !bytes.Equal(got, clientFrame) {
			t.Error("client websocket frame lost or changed")
			return
		}
		_, _ = rw.Write(serverFrame)
		_ = rw.Flush()
	}))
	defer u.Close()
	r := testRelay(t)
	r.upstream = u.URL
	s := httptest.NewServer(r.handler())
	defer s.Close()
	c, err := net.DialTimeout("tcp", strings.TrimPrefix(s.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(4 * time.Second))
	_, _ = fmt.Fprintf(c, "GET /responses HTTP/1.1\r\nHost: local\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer original\r\nSession-Id: websocket-test\r\n\r\n") // gitleaks:allow -- RFC 6455 public sample nonce; synthetic bearer value.
	reader := bufio.NewReader(c)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade failed: %s", resp.Status)
	}
	_, _ = c.Write(clientFrame)
	got := make([]byte, len(serverFrame))
	if _, err := io.ReadFull(reader, got); err != nil || !bytes.Equal(got, serverFrame) {
		t.Fatal("upgraded response is not duplex")
	}
}
