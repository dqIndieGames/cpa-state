package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestBPSLocalValidationErrorVisible(t *testing.T) {
	r := testRelay(t)
	r.settings.BPS = true
	token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"gpt-6-astra","input":[{"type":"custom_tool_call_output","call_id":"orphan","output":"result"}]}`))
	q.Header.Set("Authorization", "Bearer "+token)
	q.Header.Set("Session-Id", "12345678-1234-1234-1234-123456789abc")
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != 400 {
		t.Fatal("invalid history not rejected")
	}
	var body struct {
		Error struct{ Type, Message string }
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	sessions := r.singleStatus().Sessions
	if len(sessions) != 1 || len(sessions[0].Errors) != 1 {
		t.Fatal("local error invisible")
	}
	diagnostic := sessions[0].Errors[0]
	if diagnostic.Type != body.Error.Type || diagnostic.Message != body.Error.Message || diagnostic.HTTP != 400 {
		t.Fatal("display differs from client error")
	}
}

func asyncFixture() []any {
	meta := map[string]any{"turn_id": "turn-original"}
	return []any{
		map[string]any{"type": "custom_tool_call", "call_id": "original", "name": "exec", "namespace": "functions", "input": "notify('progress')", "internal_chat_message_metadata_passthrough": meta},
		map[string]any{"type": "custom_tool_call_output", "call_id": "original", "id": "result-1", "output": "Script running with cell ID 29\nWall time 1.0 seconds\nOutput:\n", "internal_chat_message_metadata_passthrough": meta},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "original", "id": "result-2", "name": "exec", "output": "progress notice", "internal_chat_message_metadata_passthrough": meta},
		map[string]any{"type": "custom_tool_call_output", "call_id": "original", "id": "result-3", "name": "exec", "output": "progress notice", "internal_chat_message_metadata_passthrough": meta},
	}
}

func TestBPSAsyncHistoryAndCompaction(t *testing.T) {
	r := testRelay(t)
	source := bridgeSource()
	history := asyncFixture()
	source["input"] = history
	before, _ := json.Marshal(history)
	q, err := bridgeRequest(t, r, "12345678-1234-1234-1234-123456789abc", source)
	if err != nil {
		t.Fatal(err)
	}
	x := exchange(q)
	if len(x.Async) != 2 {
		t.Fatal("notifications not represented")
	}
	after, _ := json.Marshal(history)
	if string(before) != string(after) {
		t.Fatal("client history mutated")
	}
	var sent map[string]any
	json.NewDecoder(q.Body).Decode(&sent)
	items := sent["input"].([]any)
	calls := map[string]bool{}
	results := map[string]bool{}
	var payloads []any
	start := -1
	for i, v := range items {
		m := v.(map[string]any)
		id := str(m, "call_id")
		if str(m, "type") == "function_call" {
			if start < 0 {
				start = i
			}
			if calls[id] {
				t.Fatal("duplicate call")
			}
			calls[id] = true
		}
		if str(m, "type") == "function_call_output" {
			if !calls[id] || results[id] {
				t.Fatal("orphan or duplicate result")
			}
			results[id] = true
			payloads = append(payloads, m["output"])
		}
	}
	if !reflect.DeepEqual(payloads, []any{history[1].(map[string]any)["output"], "progress notice", "progress notice"}) {
		t.Fatal("notification contents/order lost")
	}
	if len(r.bpsCache.Entries) != 0 {
		t.Fatal("history entered execution cache")
	}
	opaque := map[string]any{"type": "compaction", "encrypted_content": "server-opaque"}
	compactItems := append(items[start:], opaque)
	body, _ := json.Marshal(map[string]any{"id": "cmp", "object": "response.compaction", "output": compactItems})
	resp := &http.Response{Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}
	if err = x.compactResponse(resp); err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	json.NewDecoder(resp.Body).Decode(&restored)
	if !reflect.DeepEqual(restored["output"], append(history, opaque)) {
		t.Fatal("compaction failed to restore original async history")
	}
}

func TestBPSAsyncRejectsAmbiguousNotifications(t *testing.T) {
	for _, kind := range []string{"duplicate-event", "wrong-turn", "not-yielded", "other-tool"} {
		t.Run(kind, func(t *testing.T) {
			history := asyncFixture()
			history = history[:4]
			call := history[0].(map[string]any)
			first := history[1].(map[string]any)
			notice := history[3].(map[string]any)
			switch kind {
			case "duplicate-event":
				notice["id"] = first["id"]
			case "wrong-turn":
				notice["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "different"}
			case "not-yielded":
				first["output"] = "Completed"
			case "other-tool":
				call["name"] = "delete_file"
			}
			source := bridgeSource()
			source["input"] = history
			if _, err := bridgeRequest(t, testRelay(t), "12345678-1234-1234-1234-123456789abc", source); err == nil {
				t.Fatal("ambiguous duplicate accepted")
			}
		})
	}
}

// core/src/client.rs clears internal metadata for custom providers before
// serializing the request. This reproduces that external wire contract.
func TestBPSAsyncCustomProviderWireHistory(t *testing.T) {
	history := asyncFixture()
	for _, v := range history {
		delete(v.(map[string]any), "internal_chat_message_metadata_passthrough")
	}
	src := bridgeSource()
	src["input"] = history
	q, err := bridgeRequest(t, testRelay(t), "12345678-1234-1234-1234-123456789abc", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(exchange(q).Async) != 2 {
		t.Fatal("wire notifications not carried")
	}
}
