package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"
)

// Captured from the actual local3 TUI's ephemeral system title request.
// Match the contract AND system feature metadata, never just a user's prose.
const codexTitleInstructions = "Generate a concise, single-line task title of at most 36 characters and under five words where possible. Start with an imperative verb. Capitalize only the first word unless the user's language, proper nouns, acronyms, or code terms require otherwise. Preserve ticket references exactly. Write in the user's language. Do not use quotes, markdown, or trailing punctuation. Do not answer the request."

func nativeBPSTitlePrompt(raw []byte) (string, bool) {
	var src map[string]any
	if json.Unmarshal(raw, &src) != nil || src["stream"] != true {
		return "", false
	}
	if tools, exists := src["tools"]; exists && tools != nil && !reflect.DeepEqual(tools, []any{}) {
		return "", false
	}
	text, _ := src["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	var schema any
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"title":{"type":"string","minLength":1,"maxLength":36}},"required":["title"],"additionalProperties":false}`), &schema)
	if format["type"] != "json_schema" || format["name"] != "codex_output_schema" || format["strict"] != true || !reflect.DeepEqual(format["schema"], schema) {
		return "", false
	}
	client, _ := src["client_metadata"].(map[string]any)
	var metadata map[string]any
	if json.Unmarshal([]byte(str(client, "x-codex-turn-metadata")), &metadata) != nil || metadata["thread_source"] != "system" {
		return "", false
	}
	// This local3 build still includes additional_tools in its system worker.
	// The dedicated title endpoint never executes them; don't mistake their
	// presence for a user turn when the system metadata/schema/prompt match.
	input, _ := json.Marshal(src["input"])
	prompt := titleUserMessage(input)
	user, ok := strings.CutPrefix(prompt, codexTitleInstructions+"\n\nUser prompt:\n")
	return strings.TrimSpace(user), ok && strings.TrimSpace(user) != ""
}

func bpsCodexTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	for strings.HasPrefix(strings.ToLower(title), "[bps]") {
		title = strings.TrimSpace(title[5:])
	}
	if title == "" {
		return ""
	}
	return "[bps] " + boundedText(title, 30)
}

// Answer the real TUI title worker. Codex owns naming, manual-name precedence,
// persistence and live notifications; CPA never edits its database or rollout.
func (r *relay) handleNativeBPSTitle(w http.ResponseWriter, req *http.Request, meta requestMetadata, key string, seq uint64, prompt string) {
	r.mu.Lock()
	r.active++
	r.requests++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
	transfer := r.recordDispatch(key, seq, req, ticket{})
	id, _ := sessionIdentity(req, meta)
	ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()
	var title string
	var err error
	if slots := r.root().titleSlots; slots != nil {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if err == nil {
		title, err = r.fetchBPSTitle(ctx, req.Header, id, prompt)
	}
	title = bpsCodexTitle(title)
	if err == nil && title == "" {
		err = fmt.Errorf("标题服务返回空标题")
	}
	if err != nil {
		reason := "BPS Codex标题失败: " + boundedText(err.Error(), 160)
		r.recordResponse(key, seq, transfer, 502, 0, reason)
		r.finishSession(key, seq, 502, 0, reason)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "bps_title_error", "message": reason}})
		return
	}
	r.mu.Lock()
	if s := r.sessions[key]; s != nil && s.sequence == seq {
		s.Title, s.TitleSource, s.TitleState = title, "BPS自动标题", "已返回Codex标题请求"
	}
	r.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	err = writeNativeTitleSSE(w, title)
	reason := ""
	if err != nil {
		reason = "Codex 标题响应传输中断"
	} else {
		r.recordBPSEvent(key, seq, transfer, "response.completed", 0)
	}
	r.recordResponse(key, seq, transfer, 200, 0, reason)
	r.finishSession(key, seq, 200, 0, reason)
}

func writeNativeTitleSSE(w http.ResponseWriter, title string) error {
	content, _ := json.Marshal(map[string]string{"title": title})
	responseID := fmt.Sprintf("resp_bps_title_%d", time.Now().UnixNano())
	itemID := "msg_" + responseID
	part := map[string]any{"type": "output_text", "text": string(content), "annotations": []any{}}
	item := map[string]any{"type": "message", "id": itemID, "role": "assistant", "status": "completed", "content": []any{part}}
	response := map[string]any{"id": responseID, "object": "response", "status": "completed", "output": []any{item}}
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": itemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
		{"type": "response.content_part.added", "output_index": 0, "item_id": itemID, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
		{"type": "response.output_text.delta", "output_index": 0, "item_id": itemID, "content_index": 0, "delta": string(content)},
		{"type": "response.output_text.done", "output_index": 0, "item_id": itemID, "content_index": 0, "text": string(content)},
		{"type": "response.content_part.done", "output_index": 0, "item_id": itemID, "content_index": 0, "part": part},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": response},
	}
	for i, event := range events {
		event["sequence_number"] = i
		raw, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw); err != nil {
			return err
		}
	}
	return http.NewResponseController(w).Flush()
}
