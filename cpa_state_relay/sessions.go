package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxSessions = 500

// Accepted local ticket classes, as specified by the user (292 and 780).
func goodTicketLength(length int) bool { return length == 292 || length == 780 }

const (
	sessionPending = "待判定"
	sessionPassed  = "通过"
	sessionFailed  = "未通过"
)

type requestMetadata struct {
	Input  json.RawMessage `json:"input"`
	Model  string          `json:"model"`
	Client map[string]any  `json:"client_metadata"`
}
type sessionRecord struct {
	ImageLadder                                   imageLadderState
	ImageOptimization                             string
	Errors                                        []sessionError
	TitleState, TitleError                        string
	titleAttempted                                bool
	titleLocalNamed                               bool
	Transfer                                      transferView
	ID, Title, TitleSource, Model, Status, Reason string
	At                                            time.Time
	Length, HTTP, Requests, Active                int
	sequence                                      uint64
}
type sessionView struct {
	ImageOptimization                                         string
	Errors                                                    []sessionError
	TitleState, TitleError                                    string
	AccountID, AccountName, AccountEmail                      string
	Transfer                                                  transferView
	ID, Title, TitleSource, Model, Status, Reason, ObservedAt string
	Length, HTTP, Requests, Active                            int
	updated                                                   time.Time
}

func boundedText(s string, limit int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\x00", "")
	rr := []rune(s)
	if len(rr) > limit {
		return string(rr[:limit])
	}
	return s
}
func sessionIdentity(req *http.Request, meta requestMetadata) (string, string) {
	// Actual 0.155.1 CLI carries Thread-Id / Session-Id and client_metadata.thread_id.
	id := ""
	for _, key := range []string{"Thread-Id", "Session-Id", "session_id", "X-Codex-Thread-Id"} {
		if id = req.Header.Get(key); id != "" {
			break
		}
	}
	if id == "" {
		for _, key := range []string{"thread_id", "session_id"} {
			if value, ok := meta.Client[key].(string); ok && value != "" {
				id = value
				break
			}
		}
	}
	if len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") {
		id = ""
	}
	title, _ := meta.Client["title"].(string)
	return strings.TrimSpace(id), boundedText(title, 4096)
}
func (r *relay) beginSession(req *http.Request, meta requestMetadata) (string, uint64) {
	id, title := sessionIdentity(req, meta)
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" {
		r.unidentified++
		return "", 0
	}
	key := accountIdentity(req) + "\x00" + id
	s := r.sessions[key]
	if s == nil {
		if len(r.sessions) >= maxSessions {
			oldestKey := ""
			var oldest time.Time
			for k, v := range r.sessions {
				if v.Active == 0 && (oldestKey == "" || v.At.Before(oldest)) {
					oldestKey = k
					oldest = v.At
				}
			}
			if oldestKey == "" {
				r.unidentified++
				return "", 0
			}
			delete(r.sessions, oldestKey)
		}
		s = &sessionRecord{ID: id, Title: "未提供标题"}
		r.loadSessionErrorsLocked()
		s.Errors = append([]sessionError(nil), r.errorHistory[id]...)
		r.sessions[key] = s
	}
	r.sequence++
	s.sequence = r.sequence
	s.Active++
	s.Requests++
	s.Model = meta.Model
	s.At = time.Now()
	s.Status = sessionPending
	s.Reason = "等待上游响应"
	s.Transfer = transferView{}
	s.Length = 0
	s.HTTP = 0
	if title != "" {
		s.Title = title
		s.TitleSource = "请求"
	}
	return key, s.sequence
}
func (r *relay) finishSession(key string, seq uint64, status, length int, reason string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	// A response from an older request must not overwrite the latest turn's verdict.
	if s == nil || s.sequence != seq {
		return
	}
	s.At = time.Now()
	s.HTTP = status
	s.Length = length
	switch {
	case reason != "":
		s.Status = sessionFailed
		s.Reason = reason
	case status != http.StatusSwitchingProtocols && (status < 200 || status >= 300):
		s.Status = sessionFailed
		s.Reason = fmt.Sprintf("HTTP %d", status)
	case s.Transfer.BPS:
		s.Status = sessionPassed
		s.Reason = "BPS 响应完成（不按票据判定）"
	case goodTicketLength(length):
		s.Status = sessionPassed
		s.Reason = fmt.Sprintf("响应票据%d", length)
	case length == 0:
		s.Status = sessionPending
		s.Reason = "响应未携带票据"
	default:
		s.Status = sessionFailed
		s.Reason = fmt.Sprintf("响应票据%d", length)
	}
}
func (r *relay) endSession(key string, seq uint64, ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[key]
	if s == nil {
		return
	}
	if s.Active > 0 {
		s.Active--
	}
	if s.sequence == seq && s.Status == sessionPending && (s.Reason == "等待上游响应" || s.Transfer.BPS) && ctx.Err() != nil {
		s.Reason = "客户端已取消"
		s.At = time.Now()
	}
}
func (r *relay) sessionViewsLocked() ([]sessionView, int, int, int) {
	views := make([]sessionView, 0, len(r.sessions))
	passed, failed, pending := 0, 0, 0
	for _, s := range r.sessions {
		switch s.Status {
		case sessionPassed:
			passed++
		case sessionFailed:
			failed++
		default:
			pending++
		}
		views = append(views, sessionView{ImageOptimization: s.ImageOptimization, Errors: append([]sessionError(nil), s.Errors...), TitleState: s.TitleState, TitleError: s.TitleError, AccountID: r.account.ID, AccountName: r.account.Name, AccountEmail: r.account.Email, Transfer: s.Transfer, ID: s.ID, Title: s.Title, TitleSource: s.TitleSource, Model: s.Model, Status: s.Status, Reason: s.Reason, ObservedAt: s.At.Format("2006-01-02 15:04:05"), Length: s.Length, HTTP: s.HTTP, Requests: s.Requests, Active: s.Active, updated: s.At})
	}
	rank := func(s string) int {
		switch s {
		case sessionFailed:
			return 0
		case sessionPending:
			return 1
		default:
			return 2
		}
	}
	sort.Slice(views, func(i, j int) bool {
		a, b := views[i], views[j]
		if rank(a.Status) != rank(b.Status) {
			return rank(a.Status) < rank(b.Status)
		}
		if !a.updated.Equal(b.updated) {
			return a.updated.After(b.updated)
		}
		return a.ID < b.ID
	})
	return views, passed, failed, pending
}
func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}
func (r *relay) refreshSessionTitles() {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
			for _, a := range r.accountRelays() {
				a.resolveSessionTitles()
			}
		}
	}
}
func (r *relay) resolveSessionTitles() {
	r.mu.RLock()
	ids := map[string]bool{}
	for _, s := range r.sessions {
		ids[s.ID] = true
	}
	homes := append([]string(nil), r.sessionHomes...)
	r.mu.RUnlock()
	if len(ids) == 0 {
		return
	}
	if len(homes) == 0 {
		homes = []string{codexHome()}
	}
	for _, home := range homes {
		titles := readSessionTitleInfo(home, ids)
		r.mu.Lock()
		for _, s := range r.sessions {
			if title, ok := titles[s.ID]; ok && title.Text != "" {
				applyLocalTitle(s, title.Text, title.Named)
			}
		}
		r.mu.Unlock()
	}
}
func (s sessionView) detailText() string {
	return s.detailTextExpanded(true)
}
func (s sessionView) detailTextExpanded(expanded bool) string {
	text := fmt.Sprintf("标题（%s）：\r\n%s", s.TitleSource, s.Title)
	if s.ImageOptimization != "" {
		text += "\r\n\r\n" + s.ImageOptimization
	}
	if len(s.Errors) > 0 {
		text += "\r\n\r\n最近错误（最多 3 种，同类合并）："
		for _, e := range s.Errors {
			text += "\r\n" + e.detail(expanded) + "\r\n"
		}
	}
	if s.TitleState != "" {
		text += "\r\n标题状态：" + s.TitleState
	}
	if s.TitleError != "" {
		text += "\r\n标题原因：" + s.TitleError
	}
	text += fmt.Sprintf("\r\n\r\n账号：%s · %s\r\n转发：%s\r\n客户端票：%d\r\n\r\n状态：%s\r\n原因：%s\r\n模型：%s\r\n观测：%s\r\nHTTP：%d    票据长度：%d\r\n请求：%d    当前进行中：%d\r\n\r\nSession：\r\n%s", s.AccountName, s.AccountEmail, s.Transfer.text(), s.Transfer.InputLength, s.Status, s.Reason, s.Model, s.ObservedAt, s.HTTP, s.Length, s.Requests, s.Active, s.ID)
	return text
}
