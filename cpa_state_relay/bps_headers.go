package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// Browser/product values follow the captured Excel request (2026-09-29,
// sections 6.3-6.4). Auth belongs to the incoming account, never that capture.
func setBPSHeaders(q *http.Request) {
	for _, key := range []string{"Cookie", "X-Codex-Turn-State", "X-Openai-Account-User-Id", "Sentry-Trace", "Baggage"} {
		q.Header.Del(key)
	}
	for key, value := range map[string]string{
		"Origin":       "https://bps.openai.com",
		"Referer":      "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/",
		"Accept":       "text/event-stream",
		"Content-Type": "application/json",
		// The SSE parser consumes decoded bytes; do not advertise unsupported
		// br/zstd encodings just because a browser supports them.
		"Accept-Encoding":             "identity",
		"Accept-Language":             "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7",
		"User-Agent":                  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36",
		"Priority":                    "u=1, i",
		"Sec-Fetch-Dest":              "empty",
		"Sec-Fetch-Mode":              "cors",
		"Sec-Fetch-Site":              "same-origin",
		"Sec-Fetch-Storage-Access":    "active",
		"X-Stainless-Os":              "Unknown",
		"X-Stainless-Runtime":         "browser:chrome",
		"X-Stainless-Arch":            "unknown",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": "6.31.0",
		"X-Stainless-Runtime-Version": "153.0.0",
		"X-Stainless-Retry-Count":     "0",
	} {
		q.Header.Set(key, value)
	}
	if responsesPath(q.URL.Path) == "/responses/compact" || strings.HasSuffix(q.URL.Path, "/responses/compact") {
		q.Header.Set("Accept", "application/json")
	}
	for key, value := range map[string]string{
		"office-platform": "OfficeOnline", "office-host": "Excel",
		"browser-ua-mobile": "false", "browser-name": "chrome",
		"client-platform-class": "OfficeOnline", "client-platform": "excel",
		"client-runtime": "web", "client-product": "basispoints-excel-plugin",
		"client-agent-profile": "excel", "client-host": "office", "client-editor": "excel",
	} {
		q.Header.Set("X-Openai-Internal-Basispoints-"+key, value)
	}
	_, token, _ := strings.Cut(q.Header.Get("Authorization"), " ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 65536 {
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Auth struct {
			AccountID     string `json:"chatgpt_account_id"`
			AccountUserID string `json:"chatgpt_account_user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err == nil && json.Unmarshal(payload, &claims) == nil && claims.Auth.AccountID == q.Header.Get("Chatgpt-Account-Id") && claims.Auth.AccountUserID != "" {
		q.Header.Set("X-Openai-Account-User-Id", claims.Auth.AccountUserID)
	}
}
