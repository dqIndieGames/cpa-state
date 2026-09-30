package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// External contract: user-supplied BPS URL/headers and account isolation.
// SSE success requires the Responses protocol's response.completed event.
func TestBPSRequestContractAndOffRestoresOriginal(t *testing.T) {
	r := testRelay(t)
	token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
	if r.singleStatus().BPS {
		t.Fatal("BPS must default off")
	}
	r.settings.TicketEnabled, r.settings.TicketWrite = true, true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.attempts["old"] = &harvestAttempt{Cancel: cancel}
	r.pool.Ticket = ticket{State: "cached", Cookie: "old=secret", GotAt: time.Now()}
	if err := r.setBPS(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("old harvest not cancelled")
	}
	if r.pool.Ticket.State != "" {
		t.Fatal("old ticket retained")
	}
	reloaded := testRelay(t)
	reloaded.settingsFile = r.settingsFile
	reloaded.loadSettings()
	if !reloaded.singleStatus().BPS {
		t.Fatal("BPS preference not persisted")
	}
	body := `{"model":"gpt-6-sol","stream":true,"input":[]}`
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	on := true
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		payload, _ := io.ReadAll(q.Body)
		if (!on && string(payload) != body) || q.URL.RawQuery != "a=1&a=2" || q.Header.Get("Authorization") != "Bearer "+token {
			t.Error("payload/query/token changed")
		}
		if on {
			var adapted map[string]any
			if json.Unmarshal(payload, &adapted) != nil || adapted["model"] != "gpt-6-sol" || adapted["model_selection"] != "explicit" || adapted["store"] != false {
				t.Fatal("BPS request contract violated")
			}
			if q.URL.String() != "https://bps.openai.com/basispoints/api/responses?a=1&a=2" || q.Host != "bps.openai.com" {
				t.Error("wrong BPS destination")
			}
			for name, value := range map[string]string{"Chatgpt-Account-Id": "second", "X-Openai-Account-Id": "second", "X-Basispoints-Auth-Mode": "chatgpt", "Cookie": "", "X-Codex-Turn-State": ""} {
				if q.Header.Get(name) != value {
					t.Errorf("incorrect header %s", name)
				}
			}
		} else if q.URL.Host != "chatgpt.com" || q.URL.Path != "/backend-api/codex/responses" || q.Header.Get("Cookie") != "client=keep" || q.Header.Get("X-Codex-Turn-State") != "client-ticket" || q.Header.Get("X-Basispoints-Auth-Mode") != "" {
			t.Error("OFF altered original route/headers")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	call := func() {
		q := httptest.NewRequest("POST", "/responses?a=1&a=2", strings.NewReader(body))
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Cookie", "client=keep")
		q.Header.Set("X-Codex-Turn-State", "client-ticket")
		q.Header.Set("Session-Id", "bps-contract")
		w := httptest.NewRecorder()
		r.handler().ServeHTTP(w, q)
		if w.Code != 200 || (!on && w.Body.String() != stream) || (on && !strings.Contains(w.Body.String(), `"status":"completed"`)) {
			t.Fatal("response not preserved")
		}
	}
	call()
	s := r.singleStatus()
	if s.Waiting != 0 || s.Shared.Attempts != 0 || s.Passed != 1 || s.Sessions[0].Transfer.SentLength != 0 {
		t.Fatal("BPS waited, harvested, injected, or misclassified")
	}
	r.settings.TicketEnabled, r.settings.TicketWrite = false, false
	if err := r.setBPS(false); err != nil {
		t.Fatal(err)
	}
	on = false
	call()
	if r.singleStatus().Pending != 1 {
		t.Fatal("OFF must restore ticket-based verdict")
	}
}

func TestBPSAccountIsolationAndValidation(t *testing.T) {
	r, second := accountFixture(t)
	if err := second.setBPS(true); err != nil {
		t.Fatal(err)
	}
	if r.singleStatus().BPS {
		t.Fatal("second switch changed main")
	}
	second.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		t.Error("invalid auth reached upstream")
		return nil, context.Canceled
	})
	for _, auth := range []string{"", "Bearer "} {
		q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"m"}`))
		q.Header.Set("Chatgpt-Account-Id", "b")
		q.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		r.handler().ServeHTTP(w, q)
		if w.Code != 400 {
			t.Fatalf("missing token accepted: %d", w.Code)
		}
	}
	token := fixtureAuth(t, t.TempDir(), "auth.json", "b")
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"m"}`))
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("X-Openai-Account-Id", "other")
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != 400 {
		t.Fatal("conflicting account headers accepted")
	}
}

func TestBPSEventCompletionAndErrorTransparency(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		http       int
		passed     bool
	}{
		{"complete", "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n", 200, true},
		{"data-only", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n", 200, true},
		{"failed", "event: response.failed\ndata: {}\n\n", 200, false},
		{"incomplete", "event: response.incomplete\ndata: {}\n\n", 200, false},
		{"error", "event: error\ndata: {}\n\n", 200, false},
		{"truncated", "event: response.created\ndata: {}\n\n", 200, false},
		{"unauthorized", `{"error":"unauthorized"}`, 401, false},
		{"forbidden", "<html>Access denied</html>", 403, false},
		{"rate-limit", `{"error":"rate_limit"}`, 429, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := testRelay(t)
			r.settings.BPS = true
			token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
			r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
				// Switch during dispatch: this response must retain its BPS semantics.
				if err := r.setBPS(false); err != nil {
					t.Error(err)
				}
				return &http.Response{StatusCode: tc.http, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			q := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
			q.Header.Set("Authorization", "Bearer "+token)
			q.Header.Set("Session-Id", "s")
			w := httptest.NewRecorder()
			r.handler().ServeHTTP(w, q)
			s := r.singleStatus()
			if w.Code != tc.http || (tc.http >= 400 && w.Body.String() != tc.body) {
				t.Fatal("upstream response changed")
			}
			if (s.Passed == 1) != tc.passed || (!tc.passed && s.Failed != 1) {
				t.Fatalf("wrong session verdict: %+v", s.Sessions)
			}
		})
	}
}

func TestBPSCompactPathAndStreamingChunkBoundaries(t *testing.T) {
	r := testRelay(t)
	r.settings.BPS = true
	token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
	// Contract: official response.compaction window; BPS endpoint confirmed live
	// on 2026-09-24. Opaque bytes and retained input must survive unchanged.
	compacted := `{"id":"cmp_fixture","object":"response.compaction","output":[{"type":"message","role":"user","content":"retain me"},{"type":"compaction","encrypted_content":"opaque-fixture"}]}`
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		if q.URL.Path != "/basispoints/api/responses/compact" || q.Header.Get("Accept") != "application/json" {
			t.Error("compact routing contract")
		}
		var body map[string]any
		_ = json.NewDecoder(q.Body).Decode(&body)
		if body["metadata"] == nil || body["stream"] != nil || body["tools"] != nil {
			t.Error("compact request contract")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(compacted))}, nil
	})
	q := httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(`{"model":"m"}`))
	q.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != http.StatusOK || w.Body.String() != compacted {
		t.Fatal("canonical compact window changed")
	}
	stream := "event: response.completed\ndata: {}\n\n"
	reason := "not finished"
	b := &bpsBody{ReadCloser: io.NopCloser(strings.NewReader(stream)), sse: true, done: func(s string) { reason = s }}
	var got strings.Builder
	buffer := make([]byte, 3)
	for {
		n, e := b.Read(buffer)
		got.Write(buffer[:n])
		if e != nil {
			break
		}
	}
	if reason != "" || got.String() != stream {
		t.Fatal("chunked SSE not preserved/classified")
	}
}
