package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Failure counts supply correction feedback, never a relay-owned retry limit.
type bpsFailure struct {
	Count  int
	At     time.Time
	Reason string
}

// Return fixed diagnostic categories, never model-controlled tool names or text.
func bpsFailureReason(err error) string {
	message := err.Error()
	for _, reason := range []string{"BPS stream idle timeout", "BPS stream progress timeout"} {
		if strings.Contains(message, reason) {
			return reason
		}
	}
	var status int
	if _, e := fmt.Sscanf(message, "BPS upstream HTTP %d", &status); e == nil && status >= 100 && status <= 599 {
		return fmt.Sprintf("BPS upstream HTTP %d", status)
	}
	for _, word := range []string{"tool code", "transport arguments", "schema", "arguments", "catalog", "tool_choice", "parallel_tool_calls", "identity", "history", "completion", "SSE JSON", "stream", "size limit"} {
		if strings.Contains(message, word) {
			reason := "BPS validation failed: " + word
			if start := strings.Index(message, "invalid JSON at byte "); start >= 0 {
				var offset int
				if _, e := fmt.Sscanf(message[start:], "invalid JSON at byte %d", &offset); e == nil {
					reason += fmt.Sprintf(" (invalid JSON at byte %d)", offset)
				}
			}
			return reason
		}
	}
	return "BPS protocol or upstream failure"
}

func (r *relay) logBPSRequestError(req *http.Request, cause error) {
	x := exchange(req)
	if x == nil {
		id, _ := sessionIdentity(req, requestMetadata{})
		x = &bpsExchange{relay: r, FailureKey: fmt.Sprintf("%x", sha256.Sum256([]byte(accountIdentity(req)+"\x00"+id+"\x00"+req.URL.Path)))}
	}
	if _, err := x.recordBPSFailure(cause, nil, false); err != nil {
		r.report(fmt.Errorf("BPS error log could not be saved: %w", err))
	}
}

type bpsFailureStore struct {
	sync.Mutex
	loaded  bool
	entries map[string]bpsFailure
}

func (r *relay) bpsFailureDirectory() string {
	path := r.settingsPath()
	return filepath.Join(filepath.Dir(path), "bps-errors", strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
}
func (r *relay) loadBPSFailuresLocked() {
	s := &r.bpsFailures
	if s.loaded {
		return
	}
	s.loaded = true
	s.entries = map[string]bpsFailure{}
	path := filepath.Join(r.bpsFailureDirectory(), "retry-state.json")
	if stat, err := os.Stat(path); err == nil && stat.Size() <= 256<<10 {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, &s.entries)
		}
	}
	if s.entries == nil {
		s.entries = map[string]bpsFailure{}
	}
}
func (x *bpsExchange) applyBPSCorrection(input *[]any) error {
	r := x.relay
	r.bpsFailures.Lock()
	defer r.bpsFailures.Unlock()
	r.loadBPSFailuresLocked()
	f := r.bpsFailures.entries[x.FailureKey]
	if f.Count > 0 {
		*input = append(*input, developerMessage("Previous local validation result: "+f.Reason))
		*input = append(*input, developerMessage(fmt.Sprintf(bpsCorrectionProtocol, f.Count)))
	}
	return nil
}

// A syntax skeleton, not source text: remove every payload character except
// JSON punctuation/whitespace. Store a hash and size to correlate full samples
// without putting prompts, command strings or credentials into plaintext logs.
func bpsSample(value string) map[string]any {
	sum := sha256.Sum256([]byte(value))
	var shape strings.Builder
	masked := false
	for _, ch := range value {
		if shape.Len() >= 1024 {
			break
		}
		if strings.ContainsRune("{}[]:,\"\\ \t\r\n", ch) {
			shape.WriteRune(ch)
			masked = false
		} else if !masked {
			shape.WriteByte('?')
			masked = true
		}
	}
	return map[string]any{"bytes": len(value), "sha256": fmt.Sprintf("%x", sum), "syntax": shape.String(), "has_code_mode": strings.Contains(value, "tools."), "has_officejs": strings.Contains(value, "ctx."), "fenced": strings.HasPrefix(strings.TrimSpace(value), strings.Repeat(string(rune(96)), 3))}
}

func (x *bpsExchange) recordBPSFailure(cause error, native map[string]any, correction bool) (int, error) {
	if x == nil || x.relay == nil {
		return 0, nil
	}
	r := x.relay
	r.bpsFailures.Lock()
	defer r.bpsFailures.Unlock()
	r.loadBPSFailuresLocked()
	now := time.Now().UTC()
	f := r.bpsFailures.entries[x.FailureKey]
	if correction && x.FailureKey != "" {
		f.Count++
		f.At = now
		f.Reason = bpsFailureReason(cause)
		r.bpsFailures.entries[x.FailureKey] = f
	}
	for len(r.bpsFailures.entries) > 512 {
		oldest := ""
		var at time.Time
		for key, item := range r.bpsFailures.entries {
			if key != x.FailureKey && (oldest == "" || item.At.Before(at)) {
				oldest, at = key, item.At
			}
		}
		delete(r.bpsFailures.entries, oldest)
	}
	dir := r.bpsFailureDirectory()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return f.Count, err
	}
	if correction {
		raw, _ := json.Marshal(r.bpsFailures.entries)
		path := filepath.Join(dir, "retry-state.json")
		if err := os.WriteFile(path+".tmp", raw, 0600); err != nil {
			return f.Count, err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return f.Count, err
		}
	}
	entry := map[string]any{"time": now, "request": x.FailureKey, "protocol_error": correction, "failures": f.Count, "retry_owner": "client", "error": bpsSample(cause.Error())}
	entry["reason"] = bpsFailureReason(cause)
	entry["stream"] = x.StreamState
	if native != nil {
		entry["tool_type"] = str(native, "type")
		entry["arguments"] = bpsSample(str(native, "arguments"))
		var outer map[string]any
		if json.Unmarshal([]byte(str(native, "arguments")), &outer) == nil {
			entry["code_type"] = fmt.Sprintf("%T", outer["code"])
			if code, ok := outer["code"].(string); ok {
				entry["code"] = bpsSample(code)
			}
		}
	}
	path := filepath.Join(dir, "events.jsonl")
	if stat, err := os.Stat(path); err == nil && stat.Size() >= 2<<20 {
		if err := os.Remove(path + ".1"); err != nil && !os.IsNotExist(err) {
			return f.Count, err
		}
		if err := os.Rename(path, path+".1"); err != nil {
			return f.Count, err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return f.Count, err
	}
	err = json.NewEncoder(file).Encode(entry)
	closeErr := file.Close()
	if err != nil {
		return f.Count, err
	}
	return f.Count, closeErr
}
