package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exact message captured from BPS while reproducing the reported PNG failure.
const bps422Error = `{"error":{"message":"422: Invalid request body.","type":"server_error","param":null,"code":null}}`

func TestUpstreamErrorsPerSessionAndRecovery(t *testing.T) {
	for _, bps := range []bool{false, true} {
		t.Run(fmt.Sprint(bps), func(t *testing.T) {
			r := testRelay(t)
			r.settings.BPS = bps
			token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
			status, body := 422, bps422Error
			r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"request-varies"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			request := func(id string) {
				q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[]}`))
				q.Header.Set("Authorization", "Bearer "+token)
				q.Header.Set("Session-Id", id)
				w := httptest.NewRecorder()
				r.handler().ServeHTTP(w, q)
				if w.Code != status || status >= 400 && w.Body.String() != body {
					t.Fatal("status/body not transparently forwarded")
				}
			}
			request("conversation-A")
			request("conversation-A")
			request("conversation-B")
			find := func(id string) sessionView {
				for _, s := range r.singleStatus().Sessions {
					if s.ID == id {
						return s
					}
				}
				t.Fatal("missing conversation")
				return sessionView{}
			}
			a, b := find("conversation-A"), find("conversation-B")
			if len(a.Errors) != 1 || a.Errors[0].Count != 2 || b.Errors[0].Count != 1 || a.Errors[0].Message != "422: Invalid request body." {
				t.Fatal("error cause/grouping lost")
			}
			status = 200
			if bps {
				body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
				r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
			} else {
				body = "{}"
			}
			request("conversation-A")
			if find("conversation-A").Errors[0].RecoveredAt == "" || find("conversation-B").Errors[0].RecoveredAt != "" {
				t.Fatal("recovery crossed session or was omitted")
			}
			if raw, err := os.ReadFile(r.sessionErrorPath()); err != nil || len(raw) > errorHistoryLimit {
				t.Fatal("bounded snapshot missing")
			}
			r2 := testRelay(t)
			r2.settingsFile = r.settingsFile
			r2.mu.Lock()
			r2.loadSessionErrorsLocked()
			r2.mu.Unlock()
			if r2.errorHistory["conversation-A"][0].Count != 2 {
				t.Fatal("restart lost aggregate")
			}
		})
	}
}

func TestUpstreamErrorDiagnosticsDoNotPersistSecretsOrInput(t *testing.T) {
	q := httptest.NewRequest("POST", "/responses", nil)
	q.Header.Set("Authorization", "Bearer opaque-login-value")
	q.Header.Set("Cookie", "session=opaque-cookie-value")
	raw := []byte(`{"detail":[{"loc":["body","input",0,"content"],"msg":"Invalid attachment. token=opaque-login-value; cookie=opaque-cookie-value; sk-secret00000000","type":"value_error","input":"private chat text","ctx":{"password":"private-password"}}]}`)
	e := parseUpstreamError(raw, 422, q, http.Header{}, true)
	encoded, _ := json.Marshal(e)
	for _, secret := range []string{"opaque-login-value", "opaque-cookie-value", "sk-secret00000000", "private chat text", "private-password"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("secret/request echo retained")
		}
	}
	if e.Param != "body.input.0.content" || !strings.Contains(e.Message, "Invalid attachment") {
		t.Fatal("useful diagnostic removed")
	}
}

func TestUpstreamErrorBodyBoundedAndUnchanged(t *testing.T) {
	raw := strings.Repeat("x", upstreamErrorLimit*3)
	calls := 0
	b := &upstreamErrorBody{ReadCloser: io.NopCloser(strings.NewReader(raw)), report: func(prefix []byte) {
		calls++
		if len(prefix) != upstreamErrorLimit {
			t.Fatal("unbounded capture")
		}
	}}
	got, err := io.ReadAll(b)
	b.Close()
	if err != nil || string(got) != raw || calls != 1 {
		t.Fatal("body changed or duplicate observation")
	}
}

func TestSessionErrorsBoundedIsolatedAndStale(t *testing.T) {
	r := testRelay(t)
	q := httptest.NewRequest("POST", "/responses", nil)
	q.Header.Set("Session-Id", "one")
	key, seq := r.beginSession(q, requestMetadata{})
	for i := 0; i < 5; i++ {
		r.recordSessionError(key, seq, sessionError{Mode: "普通", HTTP: 422, Message: fmt.Sprint("different ", i)})
	}
	if len(r.sessions[key].Errors) != 3 || r.sessions[key].Errors[0].Message != "different 4" {
		t.Fatal("history not bounded/newest first")
	}
	_, newSeq := r.beginSession(q, requestMetadata{})
	r.recordSessionError(key, seq, sessionError{Message: "stale"})
	r.recoverSessionErrors(key, seq)
	if r.sessions[key].Errors[0].Message == "stale" || r.sessions[key].Errors[0].RecoveredAt != "" {
		t.Fatal("old response changed current verdict")
	}
	r.recoverSessionErrors(key, newSeq)
	a, b := testRelay(t), testRelay(t)
	a.settingsFile = filepath.Join(t.TempDir(), "accounts", "a.json")
	b.settingsFile = filepath.Join(filepath.Dir(a.settingsFile), "b.json")
	if a.sessionErrorPath() == b.sessionErrorPath() {
		t.Fatal("account histories share a file")
	}
}

func TestNormalStreamFailureNotRecoveredAtEOF(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"Quota exhausted\",\"code\":\"quota\"}}}\n\n"
	failed, recovered := 0, 0
	b := &successBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), sse: true, done: func() { recovered++ }, onFailure: func(raw []byte) {
		failed++
		if !bytes.Contains(raw, []byte("Quota exhausted")) {
			t.Fatal("stream cause lost")
		}
	}}
	got, _ := io.ReadAll(b)
	if string(got) != stream || failed != 1 || recovered != 0 {
		t.Fatal("failed SSE marked recovered or changed")
	}
}
