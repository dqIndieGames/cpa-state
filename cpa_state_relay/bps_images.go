package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
)

type bpsImageHistory struct{ Original, Message, Call, Result map[string]any }

// Live BPS rejects input_image in user messages (422), but accepts the same
// bytes in function results. This request-local envelope carries attachments;
// it does not execute a tool, change the client's history, or cache image bytes.
func (x *bpsExchange) userImages(item map[string]any, index int, used map[string]bool) []any {
	if str(item, "role") != "user" {
		return []any{item}
	}
	content, ok := item["content"].([]any)
	if !ok {
		return []any{item}
	}
	count := 0
	for _, v := range content {
		p, _ := v.(map[string]any)
		if str(p, "type") == "input_image" {
			count++
		}
	}
	if count == 0 {
		return []any{item}
	}
	raw, _ := json.Marshal(item)
	sum := sha256.Sum256(append([]byte(fmt.Sprintf("%s/%d/", x.Scope, index)), raw...))
	id := fmt.Sprintf("call_cpa_attachment_%x", sum[:12])
	for used[id] {
		id += "_"
	}
	used[id] = true
	text := make([]any, 0, len(content))
	images := []any{map[string]any{"type": "input_text", "text": "Original user-provided images, in message order. This is an attachment transport envelope only; no tool was executed. Follow the user's text message."}}
	for _, v := range content {
		p, _ := v.(map[string]any)
		if str(p, "type") == "input_image" {
			text = append(text, map[string]any{"type": "input_text", "text": fmt.Sprintf("[User image %d: supplied unchanged in attachment result %s below]", len(images), id)})
			images = append(images, p)
		} else {
			text = append(text, v)
		}
	}
	message := cloneObject(item)
	message["content"] = text
	args, _ := json.Marshal(map[string]any{"code": "// User attachment transport only. No tool execution.", "summary": "Receive user image attachments", "extended_summary": "Receive original user images through the compatible attachment transport envelope", "destructive": false, "references": []any{"user-attachments"}})
	call := map[string]any{"type": "function_call", "call_id": id, "name": "run_officejs", "arguments": string(args), "status": "completed"}
	result := map[string]any{"type": "function_call_output", "call_id": id, "output": images}
	if x.Images == nil {
		x.Images = map[string]bpsImageHistory{}
	}
	x.Images[id] = bpsImageHistory{item, message, call, result}
	return []any{message, call, result}
}

// Compaction may retain any subset. Never return relay-only tool envelopes to
// Codex; restore the original user message once, preserving its image bytes.
func (x *bpsExchange) restoreUserImages(output []any) ([]any, error) {
	if len(x.Images) == 0 {
		return output, nil
	}
	out := make([]any, 0, len(output))
	seen := map[string]bool{}
	for _, v := range output {
		item, ok := v.(map[string]any)
		if !ok {
			out = append(out, v)
			continue
		}
		id := str(item, "call_id")
		attachment, found := x.Images[id]
		if found {
			expected := attachment.Call
			if str(item, "type") == "function_call_output" {
				expected = attachment.Result
			}
			for k, val := range expected {
				if k != "status" && !reflect.DeepEqual(item[k], val) {
					return nil, fmt.Errorf("BPS compaction changed user attachment envelope")
				}
			}
		} else if str(item, "role") == "user" {
			for key, a := range x.Images {
				if reflect.DeepEqual(item["content"], a.Message["content"]) {
					id, attachment, found = key, a, true
					break
				}
			}
		}
		if found {
			if !seen[id] {
				out = append(out, attachment.Original)
				seen[id] = true
			}
		} else {
			out = append(out, v)
		}
	}
	return out, nil
}
