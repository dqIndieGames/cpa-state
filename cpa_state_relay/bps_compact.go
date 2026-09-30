package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
)

// Standalone compaction returns a canonical replacement window, not a generated
// assistant answer. Preserve opaque compaction items and all retained history.
func (x *bpsExchange) compactResponse(resp *http.Response) error {
	defer resp.Body.Close()
	const limit = 32 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("BPS compaction read failed: %w", err)
	}
	if len(raw) > limit {
		return fmt.Errorf("BPS compaction response exceeds size limit")
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || str(result, "object") != "response.compaction" || str(result, "id") == "" {
		return fmt.Errorf("BPS returned invalid compaction response")
	}
	output, ok := result["output"].([]any)
	if !ok || len(output) == 0 {
		return fmt.Errorf("BPS compaction output missing")
	}
	output, err = x.restoreUserImages(output)
	if err != nil {
		return err
	}
	result["output"] = output
	compacted, changed := false, len(x.Images) > 0
	for i, value := range output {
		item, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("BPS compaction output item invalid")
		}
		kind := str(item, "type")
		if kind == "compaction" {
			if str(item, "encrypted_content") == "" {
				return fmt.Errorf("BPS compaction encrypted content missing")
			}
			compacted = true
		}
		if !isToolItem(item) {
			continue
		}
		// Only previously submitted, scoped history may occur here. Compaction
		// cannot introduce executable calls, alter arguments, or invent results.
		call := str(item, "call_id")
		pair, found := x.historyPair(call)
		if !found {
			return fmt.Errorf("BPS compaction returned unknown tool history")
		}
		clientKind := str(pair.Client, "type")
		if strings.HasSuffix(kind, "_output") {
			if clientKind == "tool_search_call" {
				clientKind = "tool_search_output"
			} else {
				clientKind += "_output"
			}
		}
		original := x.History[call+"/"+clientKind]
		if original == nil {
			return fmt.Errorf("BPS compaction returned unsubmitted tool history")
		}
		if strings.HasSuffix(kind, "_output") {
			expected := original["output"]
			if clientKind == "tool_search_output" {
				if str(pair.Native, "type") == "tool_search_call" {
					if !reflect.DeepEqual(item["tools"], original["tools"]) || !reflect.DeepEqual(item["status"], original["status"]) {
						return fmt.Errorf("BPS compaction changed discovery history")
					}
				} else {
					payload, _ := json.Marshal(map[string]any{"tools": original["tools"], "status": original["status"]})
					expected = string(payload)
				}
			}
			if !reflect.DeepEqual(item["output"], expected) {
				return fmt.Errorf("BPS compaction changed a tool result")
			}
		} else {
			for _, key := range []string{"type", "name", "namespace", "arguments", "input", "execution"} {
				if !reflect.DeepEqual(item[key], pair.Native[key]) {
					return fmt.Errorf("BPS compaction changed a tool call")
				}
			}
		}
		output[i] = original
		changed = true
	}
	if !compacted {
		return fmt.Errorf("BPS response contains no encrypted compaction item")
	}
	if changed {
		result["output"] = x.restoreAsyncHistory(output)
		raw, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Type", "application/json")
	return nil
}
