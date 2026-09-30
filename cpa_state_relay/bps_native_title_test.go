package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Contract fixture: captured from local3-stream-idle-20260924 TUI on
// 2026-09-29. Only account-free metadata and isolated test messages retained.
func nativeTitleFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/codex_native_title.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNativeBPSTitleRecognitionBoundary(t *testing.T) {
	b := nativeTitleFixture(t)
	prompt, ok := nativeBPSTitlePrompt(b)
	if !ok || !strings.HasPrefix(prompt, "Read challenge.txt") || strings.Contains(prompt, "Generate a concise") {
		t.Fatal("real TUI title contract was not recognized")
	}
	mutations := map[string]func(map[string]any){
		"ordinary user request": func(v map[string]any) { delete(v, "client_metadata") },
		"other feature": func(v map[string]any) {
			v["client_metadata"].(map[string]any)["x-codex-turn-metadata"] = `{"thread_source":"automation"}`
		},
		"ordinary structured output": func(v map[string]any) { v["input"] = "Summarize a book" },
		"tools":                      func(v map[string]any) { v["tools"] = []any{map[string]any{"type": "function", "name": "shell"}} },
		"nonstream":                  func(v map[string]any) { v["stream"] = false },
		"different schema": func(v map[string]any) {
			v["text"].(map[string]any)["format"].(map[string]any)["schema"].(map[string]any)["additionalProperties"] = true
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			_ = json.Unmarshal(b, &v)
			mutate(v)
			raw, _ := json.Marshal(v)
			if _, ok := nativeBPSTitlePrompt(raw); ok {
				t.Fatal("unrelated contract intercepted")
			}
		})
	}
}

// Ground truth: Codex 0.159.1 local3 tui/src/app/thread_title.rs starts
// ThreadSource::Feature("thread_title"); protocol/src/protocol.rs serializes
// Feature as its label. Keep the captured legacy fixture as a separate contract.
func TestNativeBPSTitleCurrentFeatureSource(t *testing.T) {
	var v map[string]any
	_ = json.Unmarshal(nativeTitleFixture(t), &v)
	v["client_metadata"].(map[string]any)["x-codex-turn-metadata"] = `{"thread_source":"thread_title"}`
	raw, _ := json.Marshal(v)
	prompt, ok := nativeBPSTitlePrompt(raw)
	if !ok || !strings.HasPrefix(prompt, "Read challenge.txt") {
		t.Fatal("current Codex title request was not recognized")
	}
}

func TestBPSCodexTitlePrefixInvariant(t *testing.T) {
	for _, input := range []string{"Test title", "[bps] Test title", "[BPS] [bps] Test title", strings.Repeat("界", 120)} {
		title := bpsCodexTitle(input)
		if !strings.HasPrefix(title, "[bps] ") || strings.Count(strings.ToLower(title), "[bps]") != 1 || len([]rune(title)) > 36 || bpsCodexTitle(title) != title {
			t.Fatalf("prefix or length invariant failed: %q", title)
		}
	}
	if bpsCodexTitle("[bps] ") != "" {
		t.Fatal("empty upstream title accepted")
	}
}

func TestNativeBPSTitleRealRoutingAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{true: "upstream failure", false: "success"}[failure], func(t *testing.T) {
			r, q, meta, key := titleFixture(t)
			defer r.cancel()
			r.mu.RLock()
			seq := r.sessions[key].sequence
			r.mu.RUnlock()
			called := false
			serviceTitle := "Independent title result"
			r.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				if req.URL.Path != "/basispoints/api/task-title" || req.Header.Get("Authorization") != q.Header.Get("Authorization") || req.Header.Get("Chatgpt-Account-Id") != "second" {
					t.Error("wrong route or account")
				}
				var data map[string]string
				_ = json.NewDecoder(req.Body).Decode(&data)
				if data["user_message"] != "Read the test file" {
					t.Error("title input changed")
				}
				code := 200
				body := `{"title":"` + serviceTitle + `"}`
				if failure {
					code = 503
					body = "unavailable"
				}
				return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			w := httptest.NewRecorder()
			r.handleNativeBPSTitle(w, q, meta, key, seq, "Read the test file")
			if !called {
				t.Fatal("service not called")
			}
			if failure {
				if w.Code < 400 || strings.Contains(w.Body.String(), "response.completed") {
					t.Fatal("failure fabricated success")
				}
				return
			}
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			var final map[string]any
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if strings.HasPrefix(line, "data: ") {
					var e map[string]any
					_ = json.Unmarshal([]byte(line[6:]), &e)
					if e["type"] == "response.completed" {
						final = e
					}
				}
			}
			if final == nil {
				t.Fatal("missing completion")
			}
			response := final["response"].(map[string]any)
			item := response["output"].([]any)[0].(map[string]any)
			body := item["content"].([]any)[0].(map[string]any)["text"].(string)
			var generated map[string]string
			if json.Unmarshal([]byte(body), &generated) != nil || generated["title"] != "[bps] "+serviceTitle {
				t.Fatal("upstream title not delivered to Codex")
			}
			if _, ok := response["usage"]; ok {
				t.Fatal("invented token usage")
			}
		})
	}
}
