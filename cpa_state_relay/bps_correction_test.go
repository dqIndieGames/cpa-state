package main

import (
	"bytes"

	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L1: live-observed literal string controls retain their decoded bytes.
func TestBPSLiteralJSONControlsPreservePayload(t *testing.T) {
	for _, control := range []byte{10, 13, 9, 0, 8, 12} {
		r := testRelay(t)
		q, err := bridgeRequest(t, r, "controls", bridgeSource())
		if err != nil {
			t.Fatal(err)
		}
		code := fmt.Sprintf(`{"name":"functions.apply_patch","input":"first%slast"}`, string(control))
		args, _ := json.Marshal(map[string]any{"code": code})
		native := nativeCall("unused", nil)
		native["arguments"] = string(args)
		client, err := exchange(q).convertTool(native)
		if err != nil || client["input"] != "first"+string(control)+"last" {
			t.Fatalf("control character changed: %v", err)
		}
	}
	ambiguous := `{"name":"functions.apply_patch","input":"first` + string(rune(92)) + string(rune(10)) + `last"}`
	if _, err := bpsToolEnvelope(ambiguous); err == nil {
		t.Fatal("ambiguous line continuation accepted")
	}
}

// L1: envelope normalization preserves the caller's exact custom input, and
// malformed or ambiguous data never produces an executable tool event.
func TestBPSFormatNormalizationPreservesInput(t *testing.T) {
	input := "*** Begin Patch\n*** Add File: C:\\example.txt\n+literal \"quote\" and \n*** End Patch\n"
	inner := map[string]any{"name": "functions.apply_patch", "input": input}
	raw, _ := json.Marshal(inner)
	encoded, _ := json.Marshal(string(raw))
	fence := strings.Repeat(string(rune(96)), 3)
	for _, value := range []any{inner, string(raw), string(encoded), fence + "json\n" + string(raw) + "\n" + fence} {
		r := testRelay(t)
		q, err := bridgeRequest(t, r, "normalize", bridgeSource())
		if err != nil {
			t.Fatal(err)
		}
		native := nativeCall("unused", nil)
		outer, _ := json.Marshal(map[string]any{"code": value})
		native["arguments"] = string(outer)
		got, err := exchange(q).convertTool(native)
		if err != nil || got["input"] != input {
			t.Fatalf("input changed or valid envelope rejected: %v", err)
		}
	}
	for _, value := range []any{nil, "", "null", "[]", "{}{}", "{\"name\":", "text(await tools.exec_command({cmd:'echo unsafe'}));"} {
		if _, err := bpsToolEnvelope(value); err == nil {
			t.Fatal("ambiguous envelope accepted")
		}
	}
}

// L1, user contract: failures never execute tools or complete a turn; old
// persisted counts only supply feedback and cannot block a client retry.
func TestBPSCorrectionFeedbackAndRedaction(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	const failedAttempts = 5 // Exercise retries beyond the removed two-retry gate.
	for attempt := 0; attempt < failedAttempts; attempt++ {
		q, err := bridgeRequest(t, r, "budget", src)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := io.ReadAll(q.Body)
		if attempt > 0 && !strings.Contains(string(request), "previous response failed") {
			t.Fatal("correction feedback missing")
		}
		native := nativeCall("unused", nil)
		args, _ := json.Marshal(map[string]any{"code": "await tools.exec_command({cmd:'SECRET_TEST_TOKEN_12345'});"})
		native["arguments"] = string(args)
		body, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), exchange(q)))
		for _, event := range streamEvents(t, string(body)) {
			item, _ := event["item"].(map[string]any)
			if isToolItem(item) || event["type"] == "response.completed" {
				t.Fatal("failed response released a tool")
			}
		}
		{
			reason := ""
			observer := &bpsBody{ReadCloser: io.NopCloser(bytes.NewReader(body)), sse: true, done: func(value string) { reason = value }}
			_, _ = io.ReadAll(observer)
			if reason == "" || !strings.Contains(string(body), "response.failed") {
				t.Fatal("failure was not reported")
			}
		}
	}
	if _, err := bridgeRequest(t, r, "budget", src); err != nil {
		t.Fatal("client retry blocked", err)
	}
	fresh := testRelay(t)
	fresh.settingsFile = r.settingsFile
	q, err := bridgeRequest(t, fresh, "budget", src)
	if err != nil {
		t.Fatal("persisted failure count blocked a retry", err)
	}
	request, _ := io.ReadAll(q.Body)
	if !strings.Contains(string(request), "previous response failed") {
		t.Fatal("restart lost correction feedback")
	}
	log, err := os.ReadFile(filepath.Join(r.bpsFailureDirectory(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "SECRET_TEST_TOKEN_12345") || strings.Contains(string(log), "exec_command") {
		t.Fatal("tool content leaked into log")
	}
	if len(strings.Split(strings.TrimSpace(string(log)), "\n")) != failedAttempts {
		t.Fatal("failure log count differs from attempts")
	}
	src["input"] = append(src["input"].([]any), map[string]any{"role": "user", "content": "A new independent request"})
	if _, err := bridgeRequest(t, fresh, "budget", src); err != nil {
		t.Fatal("previous turn blocked a new user turn")
	}
}

// L1: only a restart-authorized account/session and exact locally recorded,
func TestBPSCorrectionCanRecover(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	q, err := bridgeRequest(t, r, "recovery", src)
	if err != nil {
		t.Fatal(err)
	}
	native := nativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "echo recovered"}})
	bad := cloneObject(native)
	bad["arguments"] = "{}"
	_, _ = io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(bad))), exchange(q)))
	q, err = bridgeRequest(t, r, "recovery", src)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), exchange(q)))
	events := streamEvents(t, string(body))
	last := events[len(events)-1]["response"].(map[string]any)
	if last["status"] != "completed" || last["bps_bridge_error"] != nil {
		t.Fatal("corrected response did not recover")
	}
	client := last["output"].([]any)[0].(map[string]any)
	src["input"] = append(src["input"].([]any), client, map[string]any{"type": "function_call_output", "call_id": client["call_id"], "output": "recovered"})
	if _, err := bridgeRequest(t, r, "recovery", src); err != nil {
		t.Fatal("successful corrected history could not continue")
	}
}
