package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// L0: fixture transcribed from the user-supplied Excel capture, sections
// 6.3/6.4, ChatGPTExcel发送报文_后台请求头与正文_2026-09-29.md.
// L1: identity comes from this request, body length matches transmitted bytes,
// and setting headers never alters the request body.
func TestBPSExcelCapturedHeaders(t *testing.T) {
	raw, err := os.ReadFile("testdata/bps-excel-headers-20260929.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	claim, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{
		"chatgpt_account_id": "second", "chatgpt_account_user_id": "user-fixture__second",
	}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(claim) + ".fixture"
	r := testRelay(t)
	r.settings.BPS = true
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		for key, value := range want {
			if q.Header.Get(key) != value {
				t.Errorf("capture mismatch: %s", key)
			}
		}
		if q.Header.Get("X-Openai-Account-User-Id") != "user-fixture__second" || q.Header.Get("X-Openai-Account-Id") != "second" || q.Header.Get("Authorization") != "Bearer "+token {
			t.Error("account identity changed")
		}
		for _, key := range []string{"Cookie", "X-Codex-Turn-State", "Sentry-Trace", "Baggage"} {
			if q.Header.Get(key) != "" {
				t.Errorf("stale header forwarded: %s", key)
			}
		}
		body, _ := io.ReadAll(q.Body)
		if q.ContentLength != int64(len(body)) {
			t.Error("incorrect dynamic body length")
		}
		q.Body = io.NopCloser(strings.NewReader(string(body)))
		setBPSHeaders(q)
		after, _ := io.ReadAll(q.Body)
		if string(body) != string(after) {
			t.Error("headers changed body")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))}, nil
	})
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-6-sol","input":[{"role":"user","content":"hello"}]}`))
	q.Header.Set("Authorization", "Bearer "+token)
	for _, key := range []string{"Cookie", "X-Codex-Turn-State", "X-Openai-Account-User-Id", "Sentry-Trace", "Baggage"} {
		q.Header.Set(key, "stale-other-account")
	}
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != http.StatusOK {
		t.Fatalf("relay returned %d", w.Code)
	}

	// An opaque token or one missing the claim cannot inherit another user ID.
	q.Header.Set("Authorization", "Bearer opaque")
	q.Header.Set("X-Openai-Account-User-Id", "stale-other-account")
	setBPSHeaders(q)
	if q.Header.Get("X-Openai-Account-User-Id") != "" {
		t.Error("stale identity survived")
	}
}
