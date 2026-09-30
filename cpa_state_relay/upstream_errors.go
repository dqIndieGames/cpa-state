package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const upstreamErrorLimit = 64 << 10
const errorHistoryLimit = 1 << 20

type sessionError struct {
	Mode                         string
	HTTP                         int
	Message, Type, Code, Param   string
	Count                        int
	FirstAt, LastAt, RecoveredAt string
	RequestID, CFRay             string
}

var diagnosticSecrets = regexp.MustCompile(`(?i)(?:bearer\s+[^\s,;"'<>]+|\b(?:sk-|sess-)[a-z0-9_-]{8,}|\beyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+|(?:authorization|cookie|set-cookie|api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret)["']?\s*[=:]\s*(?:"[^"]*"|'[^']*'|[^\r\n,;]+))`)
var diagnosticData = regexp.MustCompile(`data:[^\s"'<>]+`)
var diagnosticHTML = regexp.MustCompile(`<[^>]*>`)

func cleanDiagnostic(s string, req *http.Request, limit int) string {
	if req != nil {
		for _, key := range []string{"Authorization", "Cookie", "X-Api-Key"} {
			value := req.Header.Get(key)
			if value != "" {
				s = strings.ReplaceAll(s, value, "[凭据已隐藏]")
			}
			if key == "Authorization" {
				if _, token, ok := strings.Cut(value, " "); ok && len(token) > 5 {
					s = strings.ReplaceAll(s, token, "[凭据已隐藏]")
				}
			}
			if key == "Cookie" {
				for _, part := range strings.Split(value, ";") {
					if _, token, ok := strings.Cut(strings.TrimSpace(part), "="); ok && len(token) > 5 {
						s = strings.ReplaceAll(s, token, "[凭据已隐藏]")
					}
				}
			}
		}
	}
	s = diagnosticSecrets.ReplaceAllString(s, "[凭据已隐藏]")
	s = diagnosticData.ReplaceAllString(s, "[附件内容已省略]")
	s = strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
	if len([]rune(s)) > limit {
		return boundedText(s, limit) + "…（已截断）"
	}
	return strings.TrimSpace(s)
}

func parseUpstreamError(raw []byte, status int, req *http.Request, header http.Header, bps bool) sessionError {
	e := sessionError{Mode: "普通", HTTP: status, RequestID: cleanDiagnostic(header.Get("X-Request-Id"), req, 160), CFRay: cleanDiagnostic(header.Get("Cf-Ray"), req, 120)}
	if bps {
		e.Mode = "BPS"
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		// Select diagnostics only; never retain validation input/ctx or request echoes.
		var extract func(any, int)
		extract = func(v any, depth int) {
			if depth > 5 {
				return
			}
			switch m := v.(type) {
			case string:
				if e.Message == "" {
					e.Message = m
				}
			case map[string]any:
				if msg := str(m, "message"); msg != "" {
					e.Message = msg
				} else if msg := str(m, "msg"); msg != "" {
					e.Message = msg
				}
				if e.Type == "" {
					e.Type = str(m, "type")
				}
				if e.Code == "" {
					e.Code = str(m, "code")
				}
				if e.Param == "" {
					e.Param = str(m, "param")
				}
				if loc, ok := m["loc"].([]any); ok && e.Param == "" {
					parts := []string{}
					for _, p := range loc {
						parts = append(parts, fmt.Sprint(p))
					}
					e.Param = strings.Join(parts, ".")
				}
				if e.Message == "" {
					for _, k := range []string{"error", "detail", "errors"} {
						if child, ok := m[k]; ok {
							extract(child, depth+1)
							if e.Message != "" {
								break
							}
						}
					}
				}
			case []any:
				for i, child := range m {
					if i >= 3 {
						break
					}
					previous := e.Message
					e.Message = ""
					extract(child, depth+1)
					if previous != "" {
						if e.Message != "" {
							e.Message = previous + "; " + e.Message
						} else {
							e.Message = previous
						}
					}
				}
			}
		}
		extract(value, 0)
		if e.Message == "" {
			e.Message = "上游返回 JSON，但未提供可显示的错误原因"
		}
	} else {
		// Malformed/oversized JSON may include request echoes; don't dump it as text.
		trimmed := strings.TrimSpace(string(raw))
		if status == 524 && header.Get("Cf-Ray") != "" && strings.Contains(trimmed, "A timeout occurred") {
			e.Message = "Cloudflare 524: A timeout occurred. 上游已连接，但未在网关时限内返回响应。"
		} else if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			e.Message = "上游错误 JSON 不完整或超过 64 KiB；未保存正文"
		} else {
			e.Message = strings.Join(strings.Fields(diagnosticHTML.ReplaceAllString(trimmed, " ")), " ")
		}
		if e.Message == "" {
			e.Message = "上游未返回错误正文"
		}
	}
	e.Message = cleanDiagnostic(e.Message, req, 2048)
	e.Type = cleanDiagnostic(e.Type, req, 120)
	e.Code = cleanDiagnostic(e.Code, req, 120)
	e.Param = cleanDiagnostic(e.Param, req, 240)
	return e
}

// Observe forwarded bytes without pre-reading, delaying headers or changing body.
type upstreamErrorBody struct {
	io.ReadCloser
	prefix []byte
	done   bool
	report func([]byte)
}
type successBody struct {
	io.ReadCloser
	done                             func()
	sse, completed, failed, overflow bool
	line                             []byte
	onFailure                        func([]byte)
}

func (b *successBody) event() {
	if b.overflow {
		return
	}
	line := strings.TrimSpace(string(b.line))
	if !strings.HasPrefix(line, "data:") {
		return
	}
	var event map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &event) != nil {
		return
	}
	switch str(event, "type") {
	case "response.completed":
		b.completed = true
	case "response.failed", "response.incomplete", "error":
		if b.failed {
			return
		}
		b.failed = true
		cause := event
		if response, ok := event["response"].(map[string]any); ok {
			cause = response
		}
		if cause["error"] == nil {
			cause = map[string]any{"message": str(event, "type")}
		}
		raw, _ := json.Marshal(cause)
		if b.onFailure != nil {
			b.onFailure(raw)
		}
	}
}

func (b *successBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.sse {
		for _, c := range p[:n] {
			if c == '\n' {
				b.event()
				b.line = b.line[:0]
				b.overflow = false
			} else if len(b.line) < upstreamErrorLimit {
				b.line = append(b.line, c)
			} else {
				b.overflow = true
			}
		}
		if err == io.EOF && len(b.line) > 0 {
			b.event()
			b.line = nil
		}
	}
	if err == io.EOF && b.done != nil && (!b.sse || b.completed) && !b.failed {
		b.done()
		b.done = nil
	}
	return n, err
}
func (b *upstreamErrorBody) finish() {
	if !b.done {
		b.done = true
		b.report(b.prefix)
		b.prefix = nil
	}
}
func (b *upstreamErrorBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	remaining := upstreamErrorLimit - len(b.prefix)
	if remaining > 0 {
		b.prefix = append(b.prefix, p[:min(n, remaining)]...)
	}
	if err != nil {
		b.finish()
	}
	return n, err
}
func (b *upstreamErrorBody) Close() error { err := b.ReadCloser.Close(); b.finish(); return err }
func (e sessionError) summary() string {
	return fmt.Sprintf("%s HTTP %d · %s", e.Mode, e.HTTP, boundedText(e.Message, 180))
}
func sameSessionError(a, b sessionError) bool {
	return a.Mode == b.Mode && a.HTTP == b.HTTP && a.Message == b.Message && a.Type == b.Type && a.Code == b.Code && a.Param == b.Param
}
func (r *relay) recordSessionError(key string, seq uint64, e sessionError) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil || s.sequence != seq {
		return
	}
	now := time.Now().Format(time.RFC3339)
	if s.ImageLadder.Eligible && imageLadderTrigger(e) && (e.HTTP != 524 || s.ImageLadder.Heavy) {
		s.ImageLadder.Pending = true
	}
	e.Count = 1
	e.FirstAt = now
	e.LastAt = now
	for i, old := range s.Errors {
		if sameSessionError(old, e) {
			e.Count = old.Count + 1
			e.FirstAt = old.FirstAt
			s.Errors = append(s.Errors[:i], s.Errors[i+1:]...)
			break
		}
	}
	s.Errors = append([]sessionError{e}, s.Errors...)
	if len(s.Errors) > 3 {
		s.Errors = s.Errors[:3]
	}
	s.Reason = e.summary()
	s.Status = sessionFailed
	r.saveSessionErrorsLocked(s)
}
func (r *relay) recoverSessionErrors(key string, seq uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil || s.sequence != seq {
		return
	}
	changed := false
	for i := range s.Errors {
		if s.Errors[i].RecoveredAt == "" {
			s.Errors[i].RecoveredAt = time.Now().Format(time.RFC3339)
			changed = true
		}
	}
	if changed {
		r.saveSessionErrorsLocked(s)
	}
}

// One bounded snapshot per account, replacing repeat log lines. No chat history.
func (r *relay) loadSessionErrorsLocked() {
	if r.errorHistory != nil {
		return
	}
	r.errorHistory = map[string][]sessionError{}
	f, err := os.Open(r.sessionErrorPath())
	if err != nil {
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, errorHistoryLimit+1))
	if err != nil || len(raw) > errorHistoryLimit {
		return
	}
	var history map[string][]sessionError
	if json.Unmarshal(raw, &history) != nil || len(history) > maxSessions {
		return
	}
	for id, entries := range history {
		if len(entries) > 3 {
			entries = entries[:3]
		}
		r.errorHistory[id] = entries
	}
}
func (r *relay) saveSessionErrorsLocked(s *sessionRecord) {
	r.loadSessionErrorsLocked()
	r.errorHistory[s.ID] = append([]sessionError(nil), s.Errors...)
	keys := make([]string, 0, len(r.errorHistory))
	for id := range r.errorHistory {
		keys = append(keys, id)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := r.errorHistory[keys[i]], r.errorHistory[keys[j]]
		if len(a) == 0 {
			return true
		}
		if len(b) == 0 {
			return false
		}
		return a[0].LastAt < b[0].LastAt
	})
	raw, _ := json.Marshal(r.errorHistory)
	for len(keys) > maxSessions || len(raw) > errorHistoryLimit {
		delete(r.errorHistory, keys[0])
		keys = keys[1:]
		raw, _ = json.Marshal(r.errorHistory)
	}
	path := r.sessionErrorPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err == nil {
		if err = os.WriteFile(path+".tmp", raw, 0600); err == nil {
			err = os.Rename(path+".tmp", path)
		}
		if err != nil {
			r.globalError = "会话错误摘要保存失败（界面记录仍保留）"
		}
	} else {
		r.globalError = "会话错误摘要目录不可写（界面记录仍保留）"
	}
}
func (r *relay) sessionErrorPath() string {
	return filepath.Join(filepath.Dir(r.settingsPath()), "session-errors", filepath.Base(r.settingsPath()))
}

func (e sessionError) detail(expanded bool) string {
	state := "未恢复"
	if e.RecoveredAt != "" {
		state = "已恢复 · " + e.RecoveredAt
	}
	message := e.Message
	if !expanded && len([]rune(message)) > 180 {
		message = boundedText(message, 180) + "…（点“展开原因”）"
	}
	text := fmt.Sprintf("%s · HTTP %d · 累计 %d 次 · %s\r\n最近：%s\r\n原因：%s", e.Mode, e.HTTP, e.Count, state, e.LastAt, message)
	if expanded {
		if e.Type != "" {
			text += "\r\n类型：" + e.Type
		}
		if e.Code != "" {
			text += "\r\n错误码：" + e.Code
		}
		if e.Param != "" {
			text += "\r\n字段：" + e.Param
		}
		if e.RequestID != "" {
			text += "\r\nRequest ID：" + e.RequestID
		}
	}
	return text
}
