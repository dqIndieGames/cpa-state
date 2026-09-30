package main

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// L1 invariants and Codex wire contracts: protocol/src/models.rs ToolSearchCall /
// ToolSearchOutput; core/src/tools/parallel.rs; tools/src/tool_spec.rs.
// Assertions compare independently supplied calls/results, never generated snapshots.
func TestBPSMultipleToolsAtomicDelivery(t *testing.T) {
	for _, scenario := range []string{"parallel", "explicit-false", "invalid-last", "duplicate-id", "already-delivered", "thin-parallel", "thin-invalid-last"} {
		t.Run(scenario, func(t *testing.T) {
			thin := strings.HasPrefix(scenario, "thin-")
			scenario := strings.TrimPrefix(scenario, "thin-")
			r := testRelay(t)
			src := bridgeSource()
			if scenario == "explicit-false" {
				src["parallel_tool_calls"] = false
			}
			q, err := bridgeRequest(t, r, "parallel", src)
			if err != nil {
				t.Fatal(err)
			}
			a := nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "read-a"}})
			b := nativeCall("functions.apply_patch", map[string]any{"input": "*** Begin Patch\n*** End Patch\n"})
			if thin {
				a = thinNativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "read-a"}})
				b = thinNativeCall("functions.apply_patch", map[string]any{"input": "independent raw patch input"})
			}
			b["id"], b["call_id"] = "fc_b", "call_b"
			if scenario == "invalid-last" {
				b["name"] = "undeclared"
			}
			if scenario == "duplicate-id" {
				b["call_id"] = a["call_id"]
			}
			if scenario == "already-delivered" {
				if err := r.bpsCache.put(exchange(q).Scope+str(b, "call_id"), bpsCallPair{Native: b, Client: b}); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(a, b))), exchange(q)))
			events := streamEvents(t, string(raw))
			var calls []map[string]any
			for _, event := range events {
				if event["type"] == "response.output_item.done" {
					calls = append(calls, event["item"].(map[string]any))
				}
			}
			if scenario != "parallel" {
				if len(calls) != 0 || events[len(events)-1]["type"] != "response.failed" {
					t.Fatal("invalid batch leaked calls", string(raw))
				}
				if _, ok := r.bpsCache.get(exchange(q).Scope + str(a, "call_id")); ok {
					t.Fatal("partial batch committed")
				}
				return
			}
			if len(calls) != len([]any{a, b}) {
				t.Fatal("lost parallel calls", string(raw))
			}
			if calls[0]["call_id"] != a["call_id"] || calls[1]["call_id"] != b["call_id"] {
				t.Fatal("call identity changed")
			}
			// Results may arrive in a different order and contain image/text blocks.
			resultB := map[string]any{"type": "custom_tool_call_output", "call_id": b["call_id"], "output": "patch failed"}
			resultA := map[string]any{"type": "function_call_output", "call_id": a["call_id"], "output": []any{map[string]any{"type": "input_text", "text": "actual result"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,fixture"}}}
			src["input"] = []any{calls[0], calls[1], resultB, resultA}
			replay, err := bridgeRequest(t, r, "parallel", src)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			_ = json.NewDecoder(replay.Body).Decode(&body)
			input := body["input"].([]any)
			if !reflect.DeepEqual(input[len(input)-1].(map[string]any)["output"], resultA["output"]) || input[len(input)-2].(map[string]any)["output"] != resultB["output"] {
				t.Fatal("tool result lost or crossed")
			}
		})
	}
}

func TestBPSClientToolSearchContract(t *testing.T) {
	for _, mode := range []string{"legacy", "native", "thin"} {
		direct := mode == "native"
		r := testRelay(t)
		src := bridgeSource()
		src["tools"] = []any{map[string]any{"type": "tool_search", "execution": "client", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}}}
		q, err := bridgeRequest(t, r, "discovery", src)
		if err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"query": "chrome screenshot"}
		native := nativeCall("tool_search", map[string]any{"arguments": args})
		if mode == "thin" {
			native = thinNativeCall("tool_search", map[string]any{"arguments": args})
		}
		if direct {
			native = map[string]any{"id": "tsc_search", "type": "tool_search_call", "call_id": "discover", "execution": "client", "arguments": args}
		}
		raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), exchange(q)))
		events := streamEvents(t, string(raw))
		last := events[len(events)-1]
		if last["type"] != "response.completed" {
			t.Fatal(string(raw))
		}
		call := last["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
		if call["type"] != "tool_search_call" || call["execution"] != "client" || !reflect.DeepEqual(call["arguments"], args) {
			t.Fatal("invalid client discovery", call)
		}
		tools := []any{map[string]any{"type": "function", "name": "discovered", "parameters": map[string]any{"type": "object"}}}
		src["input"] = []any{call, map[string]any{"type": "tool_search_output", "call_id": native["call_id"], "execution": "client", "status": "completed", "tools": tools}}
		replay, err := bridgeRequest(t, r, "discovery", src)
		if err != nil {
			t.Fatal(err)
		}
		next := nativeCall("discovered", map[string]any{"arguments": map[string]any{}})
		if mode == "thin" {
			next = thinNativeCall("discovered", map[string]any{"arguments": map[string]any{}})
		}
		if _, err := exchange(replay).convertTool(next); err != nil {
			t.Fatal("a client-discovered tool cannot be called", err)
		}
		var body map[string]any
		_ = json.NewDecoder(replay.Body).Decode(&body)
		input := body["input"].([]any)
		out := input[len(input)-1].(map[string]any)
		if direct {
			if out["type"] != "tool_search_output" || !reflect.DeepEqual(out["tools"], tools) {
				t.Fatal("discovery result lost")
			}
		} else {
			var decoded map[string]any
			if json.Unmarshal([]byte(str(out, "output")), &decoded) != nil || !reflect.DeepEqual(decoded["tools"], tools) {
				t.Fatal("wrapped discovery result lost")
			}
		}
	}
}

func TestBPSCompactionPolicyPreserved(t *testing.T) {
	for _, policy := range []any{nil, []any{}, []any{map[string]any{"type": "compaction", "compact_threshold": float64(12345)}}} {
		r := testRelay(t)
		src := bridgeSource()
		src["tools"] = []any{}
		src["context_management"] = policy
		trigger := map[string]any{"type": "compaction_trigger"}
		src["input"] = append(src["input"].([]any), trigger)
		q, err := bridgeRequest(t, r, "compact", src)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		_ = json.NewDecoder(q.Body).Decode(&body)
		if list, ok := policy.([]any); ok && len(list) > 0 {
			if !reflect.DeepEqual(body["context_management"], policy) {
				t.Fatal("client policy changed")
			}
		} else if _, exists := body["context_management"]; exists {
			t.Fatal("unsolicited compaction policy")
		}
		input := body["input"].([]any)
		if !reflect.DeepEqual(input[len(input)-1], trigger) {
			t.Fatal("compaction trigger not last")
		}
	}
}

func TestBPSCodeModeWrapperAndActivity(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	src["tools"] = []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "text"}}}}}
	q, err := bridgeRequest(t, r, "code-mode", src)
	if err != nil {
		t.Fatal(err)
	}
	code := "const values = await Promise.all([tools.delay({label:'a'}), tools.delay({label:'b'})]); text(values);"
	inner, _ := json.Marshal(map[string]any{"tool": "exec", "args": code})
	args, _ := json.Marshal(map[string]any{"code": string(inner)})
	native := nativeCall("functions.exec", map[string]any{"input": code})
	native["arguments"] = string(args)
	added, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "item": native})
	raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader("data: "+string(added)+"\n\n"+completeStream(native))), exchange(q)))
	events := streamEvents(t, string(raw))
	if events[0]["type"] != "response.in_progress" {
		t.Fatal("real upstream activity hidden")
	}
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatal(string(raw))
	}
	client := last["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	if client["input"] != code || client["namespace"] != "functions" {
		t.Fatal("Code Mode input changed")
	}
}
