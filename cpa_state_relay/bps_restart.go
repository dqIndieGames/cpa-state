package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
)

var bpsSessionUUID = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

// Full client-supplied history is input, never an instruction to execute a tool.
// Restored pairs are request-local: no disk access, cross-account lookup, or cache
// eviction can supply or replace history. New model output uses the usual checks.
func (x *bpsExchange) restoreRestartHistory(session string, history []any) error {
	calls := map[string]map[string]any{}
	results := map[string]map[string]any{}
	for _, value := range history {
		item, _ := value.(map[string]any)
		kind := str(item, "type")
		switch kind {
		case "function_call", "custom_tool_call", "tool_search_call":
			id := str(item, "call_id")
			if id == "" || calls[id] != nil {
				return fmt.Errorf("BPS history has missing or duplicate call_id")
			}
			calls[id] = item
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			id := str(item, "call_id")
			call := calls[id]
			if call == nil || results[id] != nil {
				return fmt.Errorf("BPS history has orphan, reordered or duplicate tool result")
			}
			expected := str(call, "type") + "_output"
			if str(call, "type") == "tool_search_call" {
				expected = "tool_search_output"
			}
			if kind != expected {
				return fmt.Errorf("BPS history result type differs from call")
			}
			if kind == "tool_search_output" {
				if str(item, "execution") != "client" || str(item, "status") != "completed" {
					return fmt.Errorf("BPS history has incomplete tool discovery")
				}
				if _, ok := item["tools"].([]any); !ok {
					return fmt.Errorf("BPS history discovery tools missing")
				}
			} else if _, ok := item["output"]; !ok {
				return fmt.Errorf("BPS history tool result missing output")
			}
			results[id] = item
		}
	}
	x.Restored = map[string]bpsCallPair{}
	for id, client := range calls {
		if results[id] == nil {
			return fmt.Errorf("BPS history tool result missing; no tool was replayed")
		}
		if pair, ok := x.relay.bpsCache.get(x.Scope + id); ok {
			for _, key := range []string{"type", "name", "namespace", "arguments", "input", "execution"} {
				if !reflect.DeepEqual(client[key], pair.Client[key]) {
					return fmt.Errorf("BPS tool history mismatch")
				}
			}
			x.Restored[id] = pair
			continue
		}
		if !bpsSessionUUID.MatchString(session) {
			continue
		}
		name := str(client, "name")
		if ns := str(client, "namespace"); ns != "" {
			name = ns + "." + name
		}
		code := ""
		switch str(client, "type") {
		case "function_call":
			var args map[string]any
			code = str(client, "arguments")
			if json.Unmarshal([]byte(code), &args) != nil || args == nil {
				return fmt.Errorf("BPS history has invalid function arguments")
			}
		case "custom_tool_call":
			var ok bool
			code, ok = client["input"].(string)
			if !ok {
				return fmt.Errorf("BPS history has invalid custom input")
			}
		case "tool_search_call":
			if str(client, "execution") != "client" {
				return fmt.Errorf("BPS history has unsupported tool search execution")
			}
			if _, ok := client["arguments"].(map[string]any); !ok {
				return fmt.Errorf("BPS history has invalid search arguments")
			}
			name = "tool_search"
			raw, _ := json.Marshal(client["arguments"])
			code = string(raw)
		}
		if name == "" {
			return fmt.Errorf("BPS history has missing tool name")
		}
		args, _ := json.Marshal(map[string]any{"code": code, "summary": "Restored completed tool history", "extended_summary": "Previously executed client tool supplied in request history", "destructive": false, "references": []any{bpsToolReferencePrefix + name}})
		native := map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_restart_" + id, "call_id": id, "arguments": string(args), "status": "completed"}
		x.Restored[id] = bpsCallPair{Native: native, Client: client}
	}
	return nil
}

func (x *bpsExchange) historyPair(call string) (bpsCallPair, bool) {
	if pair, ok := x.Restored[call]; ok {
		return pair, true
	}
	return x.relay.bpsCache.get(x.Scope + call)
}
