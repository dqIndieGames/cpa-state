package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Contract source: Nonary/ghcp_proxy, ad23ce2db3b5212c0355762d981c3877322fb160,
// excel_upstream.py prepare_responses_body and extract_native_client_tool_call.
// Invariants: model unchanged, effort bounded, call identity and actual results preserved,
// account/session isolation, no tool delivery before validated completion.
func bridgeRequest(t *testing.T, r *relay, session string, src map[string]any) (*http.Request, error) {
	t.Helper()
	raw, _ := json.Marshal(src)
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(string(raw)))
	q.Header.Set("Session-Id", session)
	q.Header.Set("Chatgpt-Account-Id", "second")
	return r.prepareBPS(q, requestMetadata{})
}
func bridgeSource() map[string]any {
	return map[string]any{"model": "gpt-6-sol", "reasoning": map[string]any{"effort": "max"}, "instructions": "Client instructions", "input": []any{map[string]any{"role": "user", "content": "Run a tool"}}, "tools": []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}, "additionalProperties": false}}, map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "text"}}}}}}
}
func nativeCall(name string, payload map[string]any) map[string]any {
	inner := map[string]any{"name": name}
	for k, v := range payload {
		inner[k] = v
	}
	code, _ := json.Marshal(inner)
	args, _ := json.Marshal(map[string]any{"code": string(code), "summary": "test", "extended_summary": "test", "destructive": false, "references": []any{}})
	return map[string]any{"type": "function_call", "id": "fc_abc", "call_id": "call_abc", "name": "run_officejs", "arguments": string(args), "status": "completed"}
}
func completeStream(items ...any) string {
	raw, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_fixture", "status": "completed", "output": items, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 10 + 5}}})
	return "data: " + string(raw) + "\n\n"
}
func streamEvents(t *testing.T, s string) []map[string]any {
	t.Helper()
	events := []map[string]any{}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var e map[string]any
			if err := json.Unmarshal([]byte(line[6:]), &e); err != nil {
				t.Fatal(err)
			}
			events = append(events, e)
		}
	}
	return events
}
func TestBPSBridgeEffortAndBody(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			r := testRelay(t)
			src := bridgeSource()
			src["reasoning"] = map[string]any{"effort": effort}
			q, err := bridgeRequest(t, r, "session", src)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			_ = json.NewDecoder(q.Body).Decode(&out)
			actual := out["reasoning_effort"]
			if effort == "max" {
				if actual != "xhigh" {
					t.Fatal("user-specified downgrade missing")
				}
			} else if actual != effort {
				t.Fatal("supported effort changed")
			}
			if out["model"] != src["model"] || out["tools"] != nil || out["reasoning"] != nil || out["model_selection"] != "explicit" || out["store"] != false || out["stream"] != true {
				t.Fatal("BPS body contract")
			}
			input := out["input"].([]any)
			if input[0].(map[string]any)["content"] != src["instructions"] {
				t.Fatal("instructions lost")
			}
		})
	}
	r := testRelay(t)
	src := bridgeSource()
	src["reasoning_effort"] = "invalid"
	delete(src, "reasoning")
	if _, err := bridgeRequest(t, r, "s", src); err == nil {
		t.Fatal("unknown effort silently accepted")
	}
}
func TestBPSBridgeToolAndHistory(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "function", true: "custom"}[custom], func(t *testing.T) {
			r := testRelay(t)
			src := bridgeSource()
			q, err := bridgeRequest(t, r, "session", src)
			if err != nil {
				t.Fatal(err)
			}
			x := exchange(q)
			name := "functions.exec_command"
			payload := map[string]any{"arguments": map[string]any{"cmd": "echo test"}}
			if custom {
				name = "functions.apply_patch"
				payload = map[string]any{"input": "*** Begin Patch\n*** End Patch\n"}
			}
			native := nativeCall(name, payload)
			raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), x))
			events := streamEvents(t, string(raw))
			last := events[len(events)-1]
			if last["type"] != "response.completed" {
				t.Fatal(string(raw))
			}
			response := last["response"].(map[string]any)
			client := response["output"].([]any)[0].(map[string]any)
			if client["call_id"] != native["call_id"] || client["namespace"] != "functions" {
				t.Fatal("identity lost")
			}
			if custom && client["input"] != payload["input"] {
				t.Fatal("custom input changed")
			}
			outputType := "function_call_output"
			if custom {
				outputType = "custom_tool_call_output"
			}
			failure := "Process exited with code 7\ncommand failed"
			result := map[string]any{"type": outputType, "call_id": native["call_id"], "output": failure}
			src["input"] = append(src["input"].([]any), client, result)
			q2, err := bridgeRequest(t, r, "session", src)
			if err != nil {
				t.Fatal(err)
			}
			var replay map[string]any
			_ = json.NewDecoder(q2.Body).Decode(&replay)
			input := replay["input"].([]any)
			call := input[len(input)-2].(map[string]any)
			output := input[len(input)-1].(map[string]any)
			if call["arguments"] != native["arguments"] || call["name"] != native["name"] || output["output"] != failure || output["type"] != "function_call_output" {
				t.Fatal("history/results modified")
			}
			if _, err := bridgeRequest(t, r, "other-session", src); err == nil {
				t.Fatal("cross-session history accepted")
			}
			x.Scope = "other-account\x00session\x00"
			if _, ok := r.bpsCache.get(x.Scope + str(client, "call_id")); ok {
				t.Fatal("cross-account cache leak")
			}
		})
	}
}
func TestBPSBridgeRejectsUnsafeCalls(t *testing.T) {
	for _, kind := range []string{"unknown", "schema", "native", "none", "choice", "parallel", "required", "duplicate", "truncated", "failed", "malformed-usage"} {
		t.Run(kind, func(t *testing.T) {
			r := testRelay(t)
			src := bridgeSource()
			if kind == "none" {
				src["tool_choice"] = "none"
			}
			if kind == "choice" {
				src["tool_choice"] = map[string]any{"type": "custom", "name": "apply_patch", "namespace": "functions"}
			}
			if kind == "required" {
				src["tool_choice"] = "required"
			}
			q, err := bridgeRequest(t, r, "s", src)
			if err != nil {
				t.Fatal(err)
			}
			x := exchange(q)
			native := nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "pwd"}})
			if kind == "unknown" {
				native = nativeCall("absent", map[string]any{"arguments": map[string]any{}})
			}
			if kind == "schema" {
				native = nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": true}})
			}
			if kind == "native" {
				native["name"] = "update_plan"
			}
			stream := completeStream(native)
			if kind == "malformed-usage" {
				stream = strings.Replace(stream, `"total_tokens":15`, `"wrong_field":15`, 1)
			}
			if kind == "parallel" {
				stream = completeStream(native, native)
			}
			if kind == "required" {
				stream = completeStream()
			}
			if kind == "truncated" {
				stream = "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_abc\"}}\n\n"
			}
			if kind == "failed" {
				stream = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n"
			}
			if kind == "duplicate" {
				_, _ = io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(stream)), x))
			}
			raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(stream)), x))
			for _, event := range streamEvents(t, string(raw)) {
				if event["type"] == "response.output_item.added" || event["type"] == "response.completed" {
					t.Fatal("invalid tool delivered: " + string(raw))
				}
			}
			if !strings.Contains(string(raw), "error") && !strings.Contains(string(raw), "response.failed") {
				t.Fatal("failure hidden")
			}
		})
	}
}
func TestBPSBridgeToolHeldUntilCompletion(t *testing.T) {
	r := testRelay(t)
	q, err := bridgeRequest(t, r, "s", bridgeSource())
	if err != nil {
		t.Fatal(err)
	}
	native := nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "pwd"}})
	added, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native})
	stream := "data: " + string(added) + "\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_abc\",\"delta\":\"raw native data\"}\n\n" + completeStream(native)
	raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(stream)), exchange(q)))
	if strings.Contains(string(raw), "run_officejs") || strings.Contains(string(raw), "raw native data") {
		t.Fatal("native tool leaked")
	}
	events := streamEvents(t, string(raw))
	if events[len(events)-1]["type"] != "response.completed" {
		t.Fatal(string(raw))
	}
}

func TestBPSBridgeCancellationClosesUpstream(t *testing.T) {
	r := testRelay(t)
	q, err := bridgeRequest(t, r, "s", bridgeSource())
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := newBPSStream(reader, exchange(q))
	done := make(chan []byte, 1)
	go func() { raw, _ := io.ReadAll(stream); done <- raw }()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-done:
		if strings.Contains(string(raw), "response.output_item.added") {
			t.Fatal("cancelled tool delivered")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing client did not unblock upstream")
	}
}

// External contract: Codex client.rs build_responses_request emits
// ResponseItem::AdditionalTools when model_info.use_responses_lite is enabled.
func TestBPSResponsesLiteNativeCustomHistory(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	declared := src["tools"]
	delete(src, "tools")
	src["input"] = append([]any{map[string]any{"type": "additional_tools", "role": "developer", "tools": declared}}, src["input"].([]any)...)
	q, err := bridgeRequest(t, r, "lite", src)
	if err != nil {
		t.Fatal(err)
	}
	x := exchange(q)
	if len(x.Tools) == 0 {
		t.Fatal("Responses Lite tool declaration lost")
	}
	native := map[string]any{"type": "custom_tool_call", "id": "ctc_native", "call_id": "native_custom", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch\n", "status": "completed"}
	raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), x))
	events := streamEvents(t, string(raw))
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatal(string(raw))
	}
	client := last["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	if client["namespace"] != "functions" || client["input"] != native["input"] {
		t.Fatal("native custom identity/input changed")
	}
	src["input"] = append(src["input"].([]any), client, map[string]any{"type": "custom_tool_call_output", "call_id": native["call_id"], "output": ""})
	replay, err := bridgeRequest(t, r, "lite", src)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(replay.Body).Decode(&body)
	input := body["input"].([]any)
	result := input[len(input)-1].(map[string]any)
	if result["type"] != "custom_tool_call_output" || result["output"] != "" {
		t.Fatal("native custom output was rewritten")
	}
	native["name"] = "read_ranges"
	if _, err := x.directTool(native); err == nil {
		t.Fatal("undeclared Excel tool accepted")
	}
}
