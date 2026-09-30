package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// L1: network values supplied by the upstream remain associated with their own
// request/session, regardless of model or completion order. Classification is
// explicitly specified in the user's frozen requirements (292/780/other/missing).
func sessionRequest(id, model string) *http.Request {
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(fmt.Sprintf(`{"model":%q}`, model)))
	q.Header.Set("Session-Id", id)
	return q
}
func TestSessionResponseClassificationAndDedup(t *testing.T) {
	r := testRelay(t)
	length, status := 292, 200
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		h := http.Header{}
		if length > 0 {
			h.Set("X-Codex-Turn-State", strings.Repeat("x", length))
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("original"))}, nil
	})
	for _, tc := range []struct {
		n, code int
		want    string
	}{{292, 200, sessionPassed}, {312, 200, sessionFailed}, {0, 200, sessionPending}, {292, 429, sessionFailed}, {123, 200, sessionFailed}} {
		length, status = tc.n, tc.code
		w := httptest.NewRecorder()
		r.handleForward(w, sessionRequest("same", "model"))
		s := r.status()
		if len(s.Sessions) != 1 || s.Sessions[0].Status != tc.want || s.Sessions[0].Length != length || w.Body.String() != "original" || w.Code != status {
			t.Fatalf("classification/forwarding mismatch: %+v", s.Sessions)
		}
	}
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) { return nil, errors.New("controlled outage") })
	r.handleForward(httptest.NewRecorder(), sessionRequest("outage", "model"))
	s := r.status()
	if s.Failed != len(s.Sessions) || s.Sessions[0].HTTP != 502 {
		t.Fatal("network outage not accounted for")
	}
	r.handleForward(httptest.NewRecorder(), sessionRequest("", "model"))
	if r.status().Unidentified == 0 {
		t.Fatal("missing identity fabricated")
	}
}
func TestSessionLatestRequestWinsAndBounded(t *testing.T) {
	r := testRelay(t)
	q := sessionRequest("same", "m")
	key, older := r.beginSession(q, requestMetadata{Model: "old"})
	_, newer := r.beginSession(q, requestMetadata{Model: "new"})
	r.finishSession(key, newer, 200, 292, "")
	r.finishSession(key, older, 503, 312, "")
	r.endSession(key, older, context.Background())
	r.endSession(key, newer, context.Background())
	if s := r.status().Sessions[0]; s.Status != sessionPassed || s.Model != "new" || s.Active != 0 {
		t.Fatal("older request overwrote latest")
	}
	for i := 0; i < maxSessions+20; i++ {
		k, seq := r.beginSession(sessionRequest(fmt.Sprint(i), "m"), requestMetadata{})
		r.endSession(k, seq, context.Background())
	}
	if len(r.status().Sessions) > maxSessions {
		t.Fatal("unbounded session memory")
	}
}
func TestSharedTicketCrossModelAndAccountBoundary(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	original := ticket{State: "paired", Cookie: "__cflb=pair", GotAt: time.Now()}
	r.pool.Ticket = original
	r.pool.Account = "account:owner"
	seen := map[string]bool{}
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		if q.Header.Get("X-Codex-Turn-State") != original.State || q.Header.Get("Cookie") != original.Cookie {
			t.Error("shared pair not injected")
		}
		seen[q.Header.Get("Session-Id")] = true
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	for _, model := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6"} {
		q := sessionRequest(model, model)
		q.Header.Set("Chatgpt-Account-Id", "owner")
		r.handleForward(httptest.NewRecorder(), q)
		if !seen[model] {
			t.Fatal("model did not share ticket")
		}
	}
	for _, owner := range []string{"other", ""} {
		q := sessionRequest("blocked", "gpt-6-astra")
		q.Header.Set("Chatgpt-Account-Id", owner)
		w := httptest.NewRecorder()
		r.handleForward(w, q)
		if w.Code != 503 || seen["blocked"] {
			t.Fatal("ticket leaked across account boundary")
		}
	}
	if !r.pool.Ticket.GotAt.Equal(original.GotAt) {
		t.Fatal("business reuse extended TTL")
	}
}
func TestRepeatedHarvestKeepsOriginalIssuanceTime(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	state := "gAAAAA" + strings.Repeat("x", 286)
	original := ticket{state, "__cflb=original", time.Now().Add(-stateTTL + time.Second)}
	r.pool.LastIssued = original // cached ticket may already have been invalidated by 312
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("X-Codex-Turn-State", state)
		w.Header().Set("Set-Cookie", "__cflb=other")
	}))
	defer u.Close()
	r.upstream = u.URL
	r.attempts["one"] = &harvestAttempt{Model: "m", Cancel: func() {}, Result: make(chan error, 1)}
	r.handleForward(httptest.NewRecorder(), httptest.NewRequest("POST", "/_harvest/one/responses", strings.NewReader(`{"model":"m"}`)))
	if !r.pool.Ticket.GotAt.Equal(original.GotAt) || r.pool.Ticket.Cookie != original.Cookie {
		t.Fatal("duplicate issuance renewed time or broke pair")
	}
	if len(r.status().Sessions) != 0 || r.status().Requests != 0 {
		t.Fatal("harvest contaminated user observations")
	}
}
func TestLocalTitleReadOnlyDatabaseAndIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state_5.sqlite")
	name, _ := syscall.BytePtrFromString(path)
	var db uintptr
	if sqlcall("sqlite3_open_v2", uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&db)), 6, 0) != 0 {
		t.Fatal("cannot create fixture DB")
	}
	query, _ := syscall.BytePtrFromString("CREATE TABLE threads(id TEXT,title TEXT,name TEXT); INSERT INTO threads VALUES('db-session','original title','Renamed session');")
	rc := sqlcall("sqlite3_exec", db, uintptr(unsafe.Pointer(query)), 0, 0, 0)
	sqlcall("sqlite3_close", db)
	if rc != 0 {
		t.Fatal("fixture SQL failed")
	}
	before, _ := os.ReadFile(path)
	index := `{"id":"db-session","thread_name":"stale index"}` + "\n" + `{"id":"index-session","thread_name":"old"}` + "\n" + `{"id":"index-session","thread_name":"Latest title 中文"}` + "\n" + `{"id":"unobserved","thread_name":"private"}`
	if e := os.WriteFile(filepath.Join(dir, "session_index.jsonl"), []byte(index), 0600); e != nil {
		t.Fatal(e)
	}
	titles := readSessionTitles(dir, map[string]bool{"db-session": true, "index-session": true})
	after, _ := os.ReadFile(path)
	if titles["db-session"] != "Renamed session" || titles["index-session"] != "Latest title 中文" || titles["unobserved"] != "" || string(before) != string(after) {
		t.Fatalf("read-only title lookup mismatch: %v", titles)
	}
}

type testBrokenBody struct{ cancel context.CancelFunc }

func (b testBrokenBody) Read([]byte) (int, error) {
	if b.cancel != nil {
		b.cancel()
	}
	return 0, io.ErrUnexpectedEOF
}
func (testBrokenBody) Close() error { return nil }
func TestStreamFailureVersusClientCancellation(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelClient), func(t *testing.T) {
			r := testRelay(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
				body := testBrokenBody{}
				if cancelClient {
					body.cancel = cancel
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"X-Codex-Turn-State": []string{strings.Repeat("x", 292)}}, Body: body}, nil
			})
			q := sessionRequest("stream", "m").WithContext(ctx)
			func() {
				defer func() {
					if v := recover(); v != nil && v != http.ErrAbortHandler {
						panic(v)
					}
				}()
				r.handleForward(httptest.NewRecorder(), q)
			}()
			row := r.status().Sessions[0]
			if cancelClient && row.Status != sessionPassed {
				t.Fatal("client close overwrote observed 292")
			}
			if !cancelClient && row.Status != sessionFailed {
				t.Fatal("upstream truncation not reported")
			}
		})
	}
}
