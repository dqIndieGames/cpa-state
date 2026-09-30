package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestImageLadderForwardRetry(t *testing.T) {
	for _, bps := range []bool{false, true} {
		t.Run(fmt.Sprint(bps), func(t *testing.T) {
			r := testRelay(t)
			r.settings.BPS = bps
			token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
			input := []any{}
			for i := 0; i < 7; i++ {
				input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "preserve"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + strings.Repeat("A", 650000), "detail": "high"}}})
			}
			raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "medium"}, "input": input})
			calls := 0
			r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
				var sent map[string]any
				json.NewDecoder(q.Body).Decode(&sent)
				slots := imageSlots(sent["input"].([]any))
				count := 0
				for _, s := range slots {
					if str(s, "image_url") == ladderPlaceholder {
						count++
					}
				}
				if calls == 0 && count != 0 || calls == 1 && count != 2 || calls == 2 && count != 6 {
					t.Fatalf("request %d placeholder count=%d", calls, count)
				}
				if str(sent, "model") != "gpt-6-astra" {
					t.Fatal("model changed")
				}
				calls++
				status, body, kind := 524, "A timeout occurred", "text/plain"
				if calls == 3 {
					status = 200
					kind = "text/event-stream"
					body = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {kind}, "Cf-Ray": {"test-ray"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			for i := 0; i < 3; i++ {
				q := httptest.NewRequest("POST", "/responses", bytes.NewReader(raw))
				q.Header.Set("Authorization", "Bearer "+token)
				q.Header.Set("Session-Id", "retry")
				w := httptest.NewRecorder()
				r.handler().ServeHTTP(w, q)
				if i < 2 && (w.Code != 524 || w.Body.String() != "A timeout occurred") {
					t.Fatal("error forwarding changed", w.Code)
				}
				if i == 2 && w.Code != 200 {
					t.Fatal("retry failed", w.Code, w.Body.String())
				}
			}
			sessions := r.singleStatus().Sessions
			if len(sessions) != 1 || sessions[0].Errors[0].Count != 2 || sessions[0].Errors[0].RecoveredAt == "" || sessions[0].ImageOptimization == "" {
				t.Fatal("recovery/display missing")
			}
		})
	}
}

func TestImageLadderCodexPolicy(t *testing.T) {
	makeInput := func(detail string) []any {
		var in []any
		for i := 0; i < 7; i++ {
			in = append(in, map[string]any{"type": "function_call_output", "call_id": string(rune('a' + i)), "output": []any{map[string]any{"type": "input_text", "text": "keep text"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + string(rune('a'+i)), "detail": detail}}})
		}
		return in
	}
	for _, tc := range []struct {
		tier, keep int
		detail     string
	}{{1, 5, "original"}, {2, 1, "original"}, {3, 5, "high"}, {4, 1, "high"}} {
		input := makeInput(tc.detail)
		before, _ := json.Marshal(input)
		var original []any
		json.Unmarshal(before, &original)
		if got := applyImageTier(input, tc.tier); got != 7-tc.keep {
			t.Fatalf("tier %d changed %d", tc.tier, got)
		}
		for i, v := range input {
			item := v.(map[string]any)
			orig := original[i].(map[string]any)
			if i >= 7-tc.keep && !reflect.DeepEqual(item, orig) {
				t.Fatal("recent evidence changed")
			}
			if item["call_id"] != orig["call_id"] || !reflect.DeepEqual(item["output"].([]any)[0], orig["output"].([]any)[0]) {
				t.Fatal("text/call identity changed")
			}
		}
		if applyImageTier(input, tc.tier) != 0 {
			t.Fatal("not idempotent")
		}
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ladderPlaceholder, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(data))
	if err != nil || im.Bounds().Dx() != 1 || im.Bounds().Dy() != 1 {
		t.Fatal("placeholder must be a valid tiny PNG", err)
	}
}

func TestImageLadderFailureScopeAndNoMutation(t *testing.T) {
	r := testRelay(t)
	input := []any{}
	for i := 0; i < 7; i++ {
		input = append(input, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,x", "detail": "high"}}})
	}
	raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "medium"}, "input": input})
	q := httptest.NewRequest("POST", "http://localhost/responses", bytes.NewReader(raw))
	q.Header.Set("Session-Id", "ladder-session")
	key, seq := r.beginSession(q, requestMetadata{Model: "gpt-6-astra"})
	if r.prepareImageLadder(q, raw, key, seq, false) != q {
		t.Fatal("healthy request rewritten")
	}
	r.recordSessionError(key, seq, sessionError{HTTP: 422, Message: "Invalid request body."})
	if r.prepareImageLadder(q, raw, key, seq, false) != q {
		t.Fatal("generic 422 must not evict images")
	}
	r.recordSessionError(key, seq, sessionError{HTTP: 524})
	if r.prepareImageLadder(q, raw, key, seq, false) != q {
		t.Fatal("small timeout must not evict images")
	}
	r.recordSessionError(key, seq, sessionError{HTTP: 422, Code: "context_length_exceeded"})
	optimized := r.prepareImageLadder(q, raw, key, seq, false)
	body, _ := io.ReadAll(optimized.Body)
	var got map[string]any
	json.Unmarshal(body, &got)
	if got["model"] != "gpt-6-astra" || got["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Fatal("model changed")
	}
	slots := imageSlots(got["input"].([]any))
	n := 0
	for _, s := range slots {
		if str(s, "image_url") == ladderPlaceholder {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("expected Codex tier3 keep5 after no-op original tiers; replaced %d", n)
	}
	original, _ := io.ReadAll(q.Body)
	if !bytes.Equal(original, raw) {
		t.Fatal("original request mutated")
	}
	q.Header.Set("Session-Id", "other")
	other, otherSeq := r.beginSession(q, requestMetadata{})
	if r.prepareImageLadder(q, raw, other, otherSeq, false) != q {
		t.Fatal("cross-session leak")
	}
	changed := bytes.Replace(raw, []byte("gpt-6-astra"), []byte("another-model"), 1)
	if r.prepareImageLadder(q, changed, key, seq, false) != q {
		t.Fatal("state leaked into changed request")
	}
	for _, e := range []sessionError{{HTTP: 401, Message: "context window"}, {HTTP: 403}, {HTTP: 429}, {HTTP: 422, Message: "Invalid image"}} {
		if imageLadderTrigger(e) {
			t.Fatal("unrelated error triggers", e)
		}
	}
}

func TestImageLadderAppendOnlyContinuation(t *testing.T) {
	r := testRelay(t)
	in := []any{}
	for i := 0; i < 7; i++ {
		in = append(in, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "image-original", "detail": "high"}}})
	}
	makeBody := func(items []any) []byte {
		b, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": items})
		return b
	}
	q := httptest.NewRequest("POST", "/responses", nil)
	q.Header.Set("Session-Id", "append-test")
	key, seq := r.beginSession(q, requestMetadata{})
	raw := makeBody(in)
	r.prepareImageLadder(q, raw, key, seq, true)
	r.recordSessionError(key, seq, sessionError{HTTP: 400, Code: "context_length_exceeded"})
	if r.prepareImageLadder(q, raw, key, seq, true) == q {
		t.Fatal("initial recovery not applied")
	}
	in = append(in, map[string]any{"role": "assistant", "content": "next step"})
	if r.prepareImageLadder(q, makeBody(in), key, seq, true) == q {
		t.Fatal("append-only continuation lost recovery")
	}
	in[0] = map[string]any{"role": "user", "content": "changed prefix"}
	if r.prepareImageLadder(q, makeBody(in), key, seq, true) != q {
		t.Fatal("changed history inherited old recovery")
	}
}
