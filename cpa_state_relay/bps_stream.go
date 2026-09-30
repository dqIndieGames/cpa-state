package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Read-driven transformation: closing the downstream closes upstream immediately.
// Native tool deltas are held until a successful complete response validates them.
type bpsStream struct {
	argumentProgress map[string]*bpsArgumentProgress
	watch            *bpsWatch
	source           io.ReadCloser
	reader           *bufio.Scanner
	x                *bpsExchange
	pending          []byte
	ended            bool
	sequence         int
	held             map[string]bool
	badTool          map[string]any
	responseID       string
}

func newBPSStream(source io.ReadCloser, x *bpsExchange) io.ReadCloser {
	return newBPSStreamTimeouts(source, x, bpsIdleTimeout, bpsProgressTimeout)
}
func newBPSStreamTimeouts(source io.ReadCloser, x *bpsExchange, idle, progress time.Duration) io.ReadCloser {
	watch := newBPSWatch(source, idle, progress)
	s := bufio.NewScanner(watch)
	s.Buffer(make([]byte, 4096), 4<<20)
	return &bpsStream{source: watch, watch: watch, reader: s, x: x, held: map[string]bool{}}
}
func (s *bpsStream) Close() error { return s.source.Close() }
func (s *bpsStream) emit(event map[string]any) {
	event["sequence_number"] = s.sequence
	s.sequence++
	raw, _ := json.Marshal(event)
	s.pending = append(s.pending, []byte("event: "+str(event, "type")+"\ndata: "+string(raw)+"\n\n")...)
}
func (s *bpsStream) fail(err error) {
	_, logErr := s.x.recordBPSFailure(err, s.badTool, true)
	if logErr != nil && s.x != nil && s.x.relay != nil {
		s.x.relay.report(fmt.Errorf("BPS error log could not be saved: %w", logErr))
	}
	// Upstream formatting errors are retryable. Only the client owns the retry
	// budget; never turn a failed generation into a completed assistant message.
	s.emitFailure(err, "server_error")
}
func (s *bpsStream) failCode(err error, code string) {
	if _, logErr := s.x.recordBPSFailure(err, s.badTool, false); logErr != nil && s.x != nil && s.x.relay != nil {
		s.x.relay.report(fmt.Errorf("BPS error log could not be saved: %w", logErr))
	}
	s.emitFailure(err, code)
}
func (s *bpsStream) emitFailure(err error, code string) {
	s.emit(map[string]any{"type": "response.failed", "response": map[string]any{"status": "failed", "error": map[string]any{"type": "bps_bridge_error", "code": code, "message": err.Error()}}})
	s.ended = true
	_ = s.source.Close()
}
func isToolItem(m map[string]any) bool { t := str(m, "type"); return strings.Contains(t, "call") }
func (s *bpsStream) handle(event map[string]any) error {
	kind := str(event, "type")
	if s.x != nil {
		s.x.StreamState.LastEvent = boundedText(kind, 100)
		s.x.StreamState.LastEventAt = time.Now()
	}
	if s.contentProgress(event) {
		s.watch.Progress()
	}
	defer func() {
		if s.x != nil {
			s.x.StreamState.PendingTools = len(s.held)
		}
		if s.x != nil && s.x.Observe != nil {
			s.x.Observe(kind, len(s.held))
		}
		if s.ended {
			_ = s.source.Close()
		}
	}()
	if response, ok := event["response"].(map[string]any); ok && str(response, "id") != "" {
		s.responseID = str(response, "id")
	}
	if kind == "response.failed" || kind == "response.incomplete" || kind == "error" {
		if _, err := s.x.recordBPSFailure(fmt.Errorf("BPS upstream stream failure: %s", kind), nil, false); err != nil && s.x != nil && s.x.relay != nil {
			s.x.relay.report(err)
		}
		s.emit(event)
		s.ended = true
		return nil
	}
	if kind == "response.output_item.added" || kind == "response.output_item.done" {
		item, _ := event["item"].(map[string]any)
		if isToolItem(item) {
			if str(item, "id") == "" || len(s.held) >= 512 && !s.held[str(item, "id")] {
				return fmt.Errorf("BPS pending tool identity missing or response exceeds limit")
			}
			s.held[str(item, "id")] = true
			s.emit(map[string]any{"type": "response.in_progress"})
			return nil
		}
	}
	if s.held[str(event, "item_id")] || strings.Contains(kind, "function_call") || strings.Contains(kind, "custom_tool_call") {
		// Report actual upstream activity while withholding executable payloads.
		// This is not a timer heartbeat: a stalled upstream remains a real stall.
		s.emit(map[string]any{"type": "response.in_progress"})
		return nil
	}
	if kind != "response.completed" {
		s.emit(event)
		return nil
	}
	response, ok := event["response"].(map[string]any)
	if !ok {
		return fmt.Errorf("BPS completion has no response")
	}
	if status := str(response, "status"); status != "completed" {
		return fmt.Errorf("BPS completion status is not completed")
	}
	output, ok := response["output"].([]any)
	if !ok {
		return fmt.Errorf("BPS completion has no output")
	}
	converted := make([]any, len(output))
	copy(converted, output)
	count := 0
	pairs := map[string]bpsCallPair{}
	ids := map[string]bool{}
	indexes := []int{}
	for i, v := range output {
		item, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid BPS output item")
		}
		if !isToolItem(item) {
			continue
		}
		count++
		if count > 1 && !s.x.Parallel {
			return fmt.Errorf("BPS returned multiple calls with parallel_tool_calls=false")
		}
		client, err := s.x.convertTool(item)
		if err != nil {
			s.badTool = item
			return err
		}
		key := s.x.Scope + str(client, "call_id")
		if _, exists := s.x.Restored[str(client, "call_id")]; exists {
			return fmt.Errorf("BPS repeated a historical call_id; execution suppressed")
		}
		if _, exists := pairs[key]; exists || ids[str(item, "id")] {
			return fmt.Errorf("BPS repeated a tool identity in one response")
		}
		ids[str(item, "id")] = true
		pairs[key] = bpsCallPair{Native: item, Client: client}
		indexes = append(indexes, i)
		converted[i] = client
	}
	for id := range s.held {
		if !ids[id] {
			return fmt.Errorf("BPS completed without its pending tool")
		}
	}
	if count == 0 && s.x.Choice != "auto" && s.x.Choice != "none" {
		return fmt.Errorf("BPS did not satisfy tool_choice")
	}
	if count > 0 {
		// Codex parses completion after receiving tool items. Validate its required
		// completion fields before delivering any executable tool event.
		if err := validateBPSCompletion(response); err != nil {
			return err
		}
		if err := s.x.relay.bpsCache.putBatch(pairs); err != nil {
			return err
		}
	}
	for _, toolIndex := range indexes {
		client := converted[toolIndex].(map[string]any)
		added := cloneObject(client)
		added["status"] = "in_progress"
		field, eventType := "arguments", "function_call_arguments"
		if str(client, "type") == "custom_tool_call" {
			field, eventType = "input", "custom_tool_call_input"
		}
		added[field] = ""
		if str(client, "type") == "tool_search_call" {
			added["arguments"] = map[string]any{}
			s.emit(map[string]any{"type": "response.output_item.added", "output_index": toolIndex, "item": added})
			s.emit(map[string]any{"type": "response.output_item.done", "output_index": toolIndex, "item": client})
			continue
		}
		s.emit(map[string]any{"type": "response.output_item.added", "output_index": toolIndex, "item": added})
		s.emit(map[string]any{"type": "response." + eventType + ".delta", "output_index": toolIndex, "item_id": client["id"], "delta": client[field]})
		s.emit(map[string]any{"type": "response." + eventType + ".done", "output_index": toolIndex, "item_id": client["id"], field: client[field]})
		s.emit(map[string]any{"type": "response.output_item.done", "output_index": toolIndex, "item": client})
	}
	result := cloneObject(response)
	result["output"] = converted
	event["response"] = result
	s.emit(event)
	s.ended = true
	clear(s.held)
	return nil
}

func validateBPSCompletion(response map[string]any) error {
	if str(response, "id") == "" {
		return fmt.Errorf("BPS completion identity missing")
	}
	var parsed struct {
		EndTurn *bool `json:"end_turn"`
		Usage   *struct {
			Input        *int64 `json:"input_tokens"`
			Output       *int64 `json:"output_tokens"`
			Total        *int64 `json:"total_tokens"`
			InputDetails *struct {
				Cached *int64 `json:"cached_tokens"`
				Write  int64  `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails *struct {
				Reasoning *int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	raw, _ := json.Marshal(response)
	if json.Unmarshal(raw, &parsed) != nil {
		return fmt.Errorf("invalid BPS completion metadata")
	}
	if u := parsed.Usage; u != nil {
		if u.Input == nil || u.Output == nil || u.Total == nil || (u.InputDetails != nil && u.InputDetails.Cached == nil) || (u.OutputDetails != nil && u.OutputDetails.Reasoning == nil) {
			return fmt.Errorf("incomplete BPS token usage")
		}
	}
	return nil
}
func (s *bpsStream) Read(p []byte) (int, error) {
	for len(s.pending) == 0 && !s.ended {
		var data []string
		name := ""
		size := 0
		frameComplete := false
		for s.reader.Scan() {
			line := s.reader.Text()
			size += len(line)
			if size > 4<<20 {
				s.fail(fmt.Errorf("BPS event exceeds size limit"))
				break
			}
			if line == "" {
				if len(data) > 0 || name != "" {
					frameComplete = true
					break
				}
				continue
			}
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
			if strings.HasPrefix(line, "event:") {
				name = strings.TrimSpace(line[6:])
			}
		}
		if s.ended {
			break
		}
		if err := s.watch.Err(); err != nil {
			s.failCode(err, "server_error")
			break
		}
		if s.watch.Canceled() {
			s.ended = true
			break
		}
		if errors.Is(s.reader.Err(), bufio.ErrTooLong) {
			s.fail(fmt.Errorf("BPS event exceeds size limit"))
			break
		}
		if len(data) > 0 && !frameComplete {
			s.failCode(fmt.Errorf("BPS stream ended during an event"), "server_error")
			break
		}
		if len(data) == 0 {
			if name != "" && s.reader.Err() == nil {
				continue
			}
			if err := s.reader.Err(); err != nil {
				s.failCode(fmt.Errorf("BPS stream read failed"), "server_error")
			} else {
				// Calls are withheld until a validated completion, so retrying this
				// request cannot execute any partially received tool a second time.
				s.failCode(fmt.Errorf("BPS stream ended before completion"), "server_error")
			}
			break
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &event); err != nil || event == nil {
			s.fail(fmt.Errorf("invalid BPS SSE JSON"))
			break
		}
		if str(event, "type") == "" {
			event["type"] = name
		}
		if err := s.handle(event); err != nil {
			s.fail(err)
		}
	}
	if len(s.pending) > 0 {
		n := copy(p, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	return 0, io.EOF
}
