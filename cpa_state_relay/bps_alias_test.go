package main

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Regression truth: 2026-09-29 real second CLI declared exec_command while BPS
// returned codex:functions.exec_command (DEV tool-names.json). Expected identity
// and arguments come from the independent client catalog, not converted output.
func TestBPSFlatAliasAllEnvelopesAndHistory(t *testing.T) {
	for _, variant := range []string{"thin", "legacy", "native-name", "native-namespace"} {
		t.Run(variant, func(t *testing.T) {
			r := testRelay(t)
			src := bridgeSource()
			tool := src["tools"].([]any)[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
			src["tools"] = []any{tool}
			q, err := bridgeRequest(t, r, "alias-history", src)
			if err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"cmd": "read a known local file"}
			native := thinNativeCall("functions."+str(tool, "name"), map[string]any{"arguments": args})
			switch variant {
			case "legacy":
				native = nativeCall("functions."+str(tool, "name"), map[string]any{"arguments": args})
			case "native-name", "native-namespace":
				encoded, _ := json.Marshal(args)
				native["name"], native["arguments"] = "functions."+str(tool, "name"), string(encoded)
				if variant == "native-namespace" {
					native["name"], native["namespace"] = tool["name"], "functions"
				}
			}
			raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), exchange(q)))
			var client map[string]any
			for _, event := range streamEvents(t, string(raw)) {
				if event["type"] == "response.output_item.done" {
					client, _ = event["item"].(map[string]any)
				}
			}
			if client == nil || client["name"] != tool["name"] || client["namespace"] != nil {
				t.Fatalf("client identity lost: %s", raw)
			}
			var actual map[string]any
			_ = json.Unmarshal([]byte(str(client, "arguments")), &actual)
			if !reflect.DeepEqual(actual, args) {
				t.Fatal("arguments changed")
			}
			output := map[string]any{"type": "function_call_output", "call_id": client["call_id"], "output": "independent result"}
			src["input"] = []any{client, output}
			next, err := bridgeRequest(t, r, "alias-history", src)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			_ = json.NewDecoder(next.Body).Decode(&body)
			items := body["input"].([]any)
			if !reflect.DeepEqual(items[len(items)-2], native) || items[len(items)-1].(map[string]any)["output"] != output["output"] {
				t.Fatal("history not preserved")
			}
		})
	}
}

func TestBPSAliasBoundaries(t *testing.T) {
	x := &bpsExchange{Tools: map[string]bpsTool{"exec_command": {Name: "exec_command", Kind: "function"}}}
	for _, name := range []string{"other.exec_command", "functions.functions.exec_command", "functions.unknown", "functions.exec"} {
		if _, _, ok := x.resolveTool(name, ""); ok {
			t.Fatalf("undeclared alias accepted: %s", name)
		}
	}
	x.Tools["other.exec_command"] = bpsTool{Name: "exec_command", Namespace: "other", Kind: "function"}
	if _, _, ok := x.resolveTool("functions.exec_command", ""); ok {
		t.Fatal("ambiguous fallback accepted")
	}
	x.Tools["functions.exec_command"] = bpsTool{Name: "exec_command", Namespace: "functions", Kind: "function"}
	if name, tool, ok := x.resolveTool("functions.exec_command", ""); !ok || name != "functions.exec_command" || tool.Namespace != "functions" {
		t.Fatal("exact namespace did not win")
	}
	for _, scenario := range []string{"none", "schema", "type"} {
		r := testRelay(t)
		src := bridgeSource()
		src["tools"] = src["tools"].([]any)[0].(map[string]any)["tools"]
		if scenario == "none" {
			src["tool_choice"] = "none"
		}
		q, err := bridgeRequest(t, r, "reject-alias", src)
		if err != nil {
			t.Fatal(err)
		}
		native := thinNativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": "read"}})
		if scenario == "schema" {
			native = thinNativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": true}})
		}
		if scenario == "type" {
			native["type"], native["name"], native["input"] = "custom_tool_call", "functions.exec_command", "read"
		}
		if _, err := exchange(q).convertTool(native); err == nil {
			t.Fatalf("alias bypassed %s validation", scenario)
		}
	}
}

func TestBPSCodeModePromptUsesDeclaredName(t *testing.T) {
	for _, namespace := range []string{"", "functions", "client"} {
		r := testRelay(t)
		src := bridgeSource()
		tool := map[string]any{"type": "custom", "name": "exec", "format": map[string]any{"type": "text"}}
		src["tools"] = []any{tool}
		name := "exec"
		if namespace != "" {
			name = namespace + ".exec"
			src["tools"] = []any{map[string]any{"type": "namespace", "name": namespace, "tools": []any{tool}}}
		}
		q, err := bridgeRequest(t, r, "prompt-names", src)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(q.Body)
		if !strings.Contains(string(raw), "codex:"+name) {
			t.Fatal("actual declared Code Mode name missing")
		}
		if namespace != "functions" && strings.Contains(string(raw), "codex:functions.exec") {
			t.Fatal("nonexistent Code Mode name advertised")
		}
	}
}
