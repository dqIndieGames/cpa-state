package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// L1 / Responses input contract: supplied calls and results retain their identity
// and payload; recovering history never inserts an executable response/cache item.
func TestBPSResumeCompleteHistory(t *testing.T) {
	for _, kind := range []string{"function_call", "custom_tool_call", "tool_search_call"} {
		t.Run(kind, func(t *testing.T) {
			r := testRelay(t)
			call := map[string]any{"type": kind, "call_id": "old_call", "name": "old_tool", "arguments": `{"value":"original"}`, "input": "original custom input"}
			result := map[string]any{"type": kind + "_output", "call_id": "old_call", "output": "original result"}
			if kind == "tool_search_call" {
				call["execution"] = "client"
				call["arguments"] = map[string]any{"query": "original"}
				result = map[string]any{"type": "tool_search_output", "call_id": "old_call", "execution": "client", "status": "completed", "tools": []any{}}
			}
			src := bridgeSource()
			src["input"] = []any{call, result}
			q, err := bridgeRequest(t, r, "12345678-1234-1234-1234-123456789abc", src)
			if err != nil {
				t.Fatal(err)
			}
			x := exchange(q)
			pair, ok := x.historyPair("old_call")
			if !ok || !reflect.DeepEqual(pair.Client, call) {
				t.Fatal("client history changed")
			}
			if _, ok = r.bpsCache.get(x.Scope + "old_call"); ok {
				t.Fatal("history polluted execution cache")
			}
			other := &bpsExchange{relay: r, Scope: "other-account\x00other-session\x00"}
			if _, ok = other.historyPair("old_call"); ok {
				t.Fatal("history crossed request/account scope")
			}
			var sent map[string]any
			if err = json.NewDecoder(q.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			input := sent["input"].([]any)
			output := input[len(input)-1].(map[string]any)
			if kind != "tool_search_call" && !reflect.DeepEqual(output["output"], result["output"]) {
				t.Fatal("result changed")
			}
			opaque := map[string]any{"type": "compaction", "encrypted_content": "opaque-from-upstream"}
			raw, _ := json.Marshal(map[string]any{"id": "cmp_fixture", "object": "response.compaction", "output": []any{pair.Native, output, opaque}})
			response := &http.Response{Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}
			if err = x.compactResponse(response); err != nil {
				t.Fatal(err)
			}
			var compact map[string]any
			_ = json.NewDecoder(response.Body).Decode(&compact)
			if !reflect.DeepEqual(compact["output"], []any{call, result, opaque}) {
				t.Fatal("compaction changed submitted history")
			}
		})
	}
}

func TestBPSResumeRejectsIncompleteHistory(t *testing.T) {
	call := map[string]any{"type": "function_call", "call_id": "old", "name": "old", "arguments": "{}"}
	result := map[string]any{"type": "function_call_output", "call_id": "old", "output": "result"}
	for _, history := range [][]any{{call}, {result}, {result, call}, {call, call, result}, {call, result, result}, {call, map[string]any{"type": "custom_tool_call_output", "call_id": "old", "output": "wrong kind"}}, {call, map[string]any{"type": "function_call_output", "call_id": "old"}}} {
		src := bridgeSource()
		src["input"] = history
		if _, err := bridgeRequest(t, testRelay(t), "12345678-1234-1234-1234-123456789abc", src); err == nil {
			t.Fatal("incomplete/ambiguous history accepted")
		}
	}
}

func TestBPSResumeHistoryBeyondExecutionCache(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	history := []any{}
	for i := 0; i < 600; i++ {
		id := fmt.Sprint("old_", i)
		history = append(history, map[string]any{"type": "function_call", "name": "retired", "call_id": id, "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": id, "output": id})
	}
	src["input"] = history
	q, err := bridgeRequest(t, r, "12345678-1234-1234-1234-123456789abc", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range history {
		item := value.(map[string]any)
		if _, ok := exchange(q).historyPair(str(item, "call_id")); !ok {
			t.Fatal("long history evicted itself")
		}
	}
	if len(r.bpsCache.Entries) != 0 {
		t.Fatal("history entered shared cache")
	}
}

func TestBPSResumeSuppressesHistoricalExecution(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	src["input"] = []any{map[string]any{"type": "function_call", "call_id": "call_abc", "name": "exec_command", "namespace": "functions", "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": "call_abc", "output": "already executed"}}
	q, err := bridgeRequest(t, r, "12345678-1234-1234-1234-123456789abc", src)
	if err != nil {
		t.Fatal(err)
	}
	native := nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "echo must-not-run"}})
	raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), exchange(q)))
	failed := false
	for _, event := range streamEvents(t, string(raw)) {
		item, _ := event["item"].(map[string]any)
		if isToolItem(item) || event["type"] == "response.completed" {
			t.Fatal("historical identity released for execution")
		}
		if event["type"] == "response.failed" {
			failed = true
		}
	}
	if !failed {
		t.Fatal("missing explicit failure")
	}
}
