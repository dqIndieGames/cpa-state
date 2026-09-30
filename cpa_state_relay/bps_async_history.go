package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

type bpsAsyncHistory struct{ Original map[string]any }

// Codex exec may yield once and later notify with additional output items for
// the same call. BPS needs one output per call. Carry each notification at its
// original position in a completed, request-local transport pair, never replay
// the original code. Strictly scope this to distinct exec notification events.
func (x *bpsExchange) asyncHistory(history []any) []any {
	calls := map[string]map[string]any{}
	first := map[string]map[string]any{}
	ids := map[string]bool{}
	used := map[string]bool{}
	for _, v := range history {
		m, _ := v.(map[string]any)
		used[str(m, "call_id")] = true
	}
	out := make([]any, 0, len(history))
	for index, v := range history {
		m, _ := v.(map[string]any)
		callID := str(m, "call_id")
		if str(m, "type") == "custom_tool_call" {
			calls[callID] = m
		}
		if str(m, "type") == "custom_tool_call_output" {
			previous := first[callID]
			call := calls[callID]
			eventID := str(m, "id")
			cm, _ := call["internal_chat_message_metadata_passthrough"].(map[string]any)
			pm, _ := previous["internal_chat_message_metadata_passthrough"].(map[string]any)
			mm, _ := m["internal_chat_message_metadata_passthrough"].(map[string]any)
			turn := str(cm, "turn_id")
			// Codex removes internal metadata for non-OpenAI providers. The
			// wire form still has stable call/event IDs and the yielded exec.
			matchingTurn := cm == nil && pm == nil && mm == nil || turn != "" && str(pm, "turn_id") == turn && str(mm, "turn_id") == turn
			if previous != nil && call != nil && str(call, "name") == "exec" && str(call, "namespace") == "functions" && str(m, "name") == "exec" && strings.HasPrefix(str(previous, "output"), "Script running with cell ID ") && matchingTurn && eventID != "" && !ids[eventID] {
				digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", x.Scope, index, eventID)))
				id := fmt.Sprintf("call_cpa_async_%x", digest[:12])
				for used[id] {
					id += "_"
				}
				used[id] = true
				transport := cloneObject(call)
				delete(transport, "id")
				transport["call_id"] = id
				transport["input"] = "// Historical asynchronous output transport only; no tool was executed. The following result is a later progress notification from call " + callID + "."
				result := cloneObject(m)
				result["call_id"] = id
				if x.Async == nil {
					x.Async = map[string]bpsAsyncHistory{}
				}
				x.Async[id] = bpsAsyncHistory{Original: m}
				out = append(out, transport, result)
				ids[eventID] = true
				continue
			}
			if previous == nil {
				first[callID] = m
			}
			if eventID != "" {
				ids[eventID] = true
			}
		}
		out = append(out, v)
	}
	return out
}

// Called only after normal compaction validation/restoration of every pair.
func (x *bpsExchange) restoreAsyncHistory(items []any) []any {
	if len(x.Async) == 0 {
		return items
	}
	out := make([]any, 0, len(items))
	for _, v := range items {
		m, _ := v.(map[string]any)
		if a, ok := x.Async[str(m, "call_id")]; ok {
			if strings.HasSuffix(str(m, "type"), "_output") {
				out = append(out, a.Original)
			}
		} else {
			out = append(out, v)
		}
	}
	return out
}
