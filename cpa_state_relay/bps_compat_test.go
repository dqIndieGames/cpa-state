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

// Contract sources: user-approved compatibility requirements; OpenAI compaction
// guide (canonical output reused as-is); live BPS compact probe 2026-09-24.
func TestBPSNestedToolTierAndUltra(t *testing.T) {
	r := testRelay(t)
	src := map[string]any{"model": "gpt-6-astra", "reasoning_effort": "ultra", "service_tier": "default",
		"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []any{"key"}, "additionalProperties": false}}}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}, "input": "Look up a key"}
	q, err := bridgeRequest(t, r, "nested", src)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(q.Body).Decode(&body)
	if body["service_tier"] != nil || exchange(q).Requested != src["reasoning_effort"] || exchange(q).Actual != "xhigh" {
		t.Fatal("parameter contract lost")
	}
	x := exchange(q)
	if _, err := x.convertTool(nativeCall("lookup", map[string]any{"arguments": map[string]any{"key": "abc"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := x.convertTool(nativeCall("lookup", map[string]any{"arguments": map[string]any{"key": true}})); err == nil {
		t.Fatal("nested schema not enforced")
	}
	delete(src, "tools")
	src["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}}, map[string]any{"role": "user", "content": "hello"}}
	q, err = bridgeRequest(t, r, "lite", src)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(q.Body).Decode(&body)
	for _, v := range body["input"].([]any) {
		if str(v.(map[string]any), "type") == "additional_tools" {
			t.Fatal("client declaration leaked upstream")
		}
	}
}

func TestBPSServiceTierPolicy(t *testing.T) {
	// Live BPS rejects the field. User explicitly chose default/auto omission
	// and a visible error for priority/flex, rather than a silent downgrade.
	for _, tier := range []string{"auto", "default", "priority", "flex", "unknown"} {
		r := testRelay(t)
		src := map[string]any{"model": "m", "input": "hello", "service_tier": tier}
		q, err := bridgeRequest(t, r, "tier", src)
		allowed := tier == "auto" || tier == "default"
		if (err == nil) != allowed {
			t.Fatalf("unexpected tier decision for %s: %v", tier, err)
		}
		if allowed {
			var body map[string]any
			_ = json.NewDecoder(q.Body).Decode(&body)
			if _, exists := body["service_tier"]; exists {
				t.Fatal("unsupported field forwarded")
			}
		}
	}
}

func TestBPSCompactHistoryAndContinuation(t *testing.T) {
	r := testRelay(t)
	q, err := bridgeRequest(t, r, "compact-session", bridgeSource())
	if err != nil {
		t.Fatal(err)
	}
	x := exchange(q)
	native := nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "echo marker"}})
	client, err := x.convertTool(native)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.bpsCache.put(x.Scope+str(native, "call_id"), bpsCallPair{Native: native, Client: client}); err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"type": "function_call_output", "call_id": native["call_id"], "output": "marker"}
	history := []any{map[string]any{"role": "user", "content": "retain marker"}, client, result}
	raw, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "input": history})
	compactReq := httptest.NewRequest("POST", "/responses/compact", strings.NewReader(string(raw)))
	compactReq.Header = q.Header.Clone()
	prepared, err := r.prepareBPS(compactReq, requestMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	cx := exchange(prepared)
	var sent map[string]any
	_ = json.NewDecoder(prepared.Body).Decode(&sent)
	if !reflect.DeepEqual(sent["input"].([]any)[1], native) {
		t.Fatal("native history not restored for compaction")
	}
	opaque := map[string]any{"type": "compaction", "encrypted_content": "opaque-from-upstream"}
	window := map[string]any{"id": "cmp_fixture", "object": "response.compaction", "output": []any{native, result, opaque}}
	raw, _ = json.Marshal(window)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(string(raw))), Header: http.Header{}}
	if err = cx.compactResponse(resp); err != nil {
		t.Fatal(err)
	}
	var returned map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&returned)
	items := returned["output"].([]any)
	if !reflect.DeepEqual(items, []any{client, result, opaque}) {
		t.Fatal("compaction lost/reordered/changed history")
	}
	src := bridgeSource()
	src["input"] = append(items, map[string]any{"role": "user", "content": "Continue"})
	if _, err = bridgeRequest(t, r, "compact-session", src); err != nil {
		t.Fatal(err)
	}
	if _, err = bridgeRequest(t, r, "other-session", src); err == nil {
		t.Fatal("compaction crossed session scope")
	}
}

func TestBPSCompactRejectsInvalidAndPreservesErrors(t *testing.T) {
	for _, body := range []string{`{}`, `{"id":"x","object":"response.compaction","output":[{"type":"message"}]}`, `{"id":"x","object":"response.compaction","output":[{"type":"compaction","encrypted_content":""}]}`} {
		x := &bpsExchange{relay: testRelay(t)}
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
		if x.compactResponse(resp) == nil {
			t.Fatal("invalid compact accepted")
		}
	}
	r := testRelay(t)
	r.settings.BPS = true
	token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
	upstream := `{"error":{"message":"unavailable"}}`
	r.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstream))}, nil
	})
	q := httptest.NewRequest("POST", "/responses/compact", strings.NewReader(`{"model":"m","input":"hello"}`))
	q.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != http.StatusTooManyRequests || w.Body.String() != upstream {
		t.Fatal("upstream error hidden")
	}
}
