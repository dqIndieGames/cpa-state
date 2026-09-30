package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type cachedBPSTitle struct {
	Title string
	At    time.Time
}
type bpsTitleCache struct {
	mu      sync.Mutex
	loaded  bool
	entries map[string]cachedBPSTitle
}

func titleCacheKey(account, session string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(account+"\x00"+session)))
}

// Persist only titles, never access tokens or user messages. Account and session
// are both part of the cache identity; the account relay cannot borrow a title.
func (r *relay) cachedTitle(key, value string) string {
	root := r.root()
	c := &root.titleCache
	c.mu.Lock()
	defer c.mu.Unlock()
	path := filepath.Join(filepath.Dir(root.settingsPath()), "bps-titles.json")
	if !c.loaded {
		c.loaded = true
		c.entries = map[string]cachedBPSTitle{}
		if stat, err := os.Stat(path); err == nil && stat.Size() <= 1<<20 {
			if raw, err := os.ReadFile(path); err == nil {
				_ = json.Unmarshal(raw, &c.entries)
			}
		}
		if c.entries == nil {
			c.entries = map[string]cachedBPSTitle{}
		}
	}
	if value != "" {
		c.entries[key] = cachedBPSTitle{value, time.Now()}
		for len(c.entries) > maxSessions {
			oldest := ""
			for k, entry := range c.entries {
				if oldest == "" || entry.At.Before(c.entries[oldest].At) {
					oldest = k
				}
			}
			delete(c.entries, oldest)
		}
		raw, _ := json.Marshal(c.entries)
		if os.MkdirAll(filepath.Dir(path), 0700) == nil {
			tmp := path + ".tmp"
			if os.WriteFile(tmp, raw, 0600) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	return boundedText(c.entries[key].Title, 120)
}

func usableLocalTitle(title string) bool {
	title = strings.TrimSpace(title)
	return title != "" && title != "未提供标题" && !strings.ContainsAny(title, "\r\n") && len([]rune(title)) <= 120
}

func applyLocalTitle(s *sessionRecord, title string, named ...bool) {
	if s.TitleSource == "请求" {
		return
	}
	isNamed := len(named) > 0 && named[0]
	if s.titleLocalNamed && !isNamed {
		return
	}
	if isNamed || usableLocalTitle(title) {
		s.titleLocalNamed = isNamed
		s.Title, s.TitleSource = strings.TrimSpace(title), "Codex本地记录"
		s.TitleState, s.TitleError = "", ""
	} else if s.TitleSource != "BPS自动标题" && !usableLocalTitle(s.Title) {
		s.Title, s.TitleSource = boundedText(title, 4096), "Codex本地记录"
	}
}

func titleUserMessage(input json.RawMessage) string {
	var text string
	if json.Unmarshal(input, &text) == nil {
		return boundedText(text, 4096)
	}
	var items []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(input, &items) != nil {
		return ""
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Role != "user" {
			continue
		}
		if json.Unmarshal(items[i].Content, &text) == nil {
			return boundedText(text, 4096)
		}
		var parts []struct{ Type, Text string }
		if json.Unmarshal(items[i].Content, &parts) == nil {
			var lines []string
			for _, part := range parts {
				if part.Type == "input_text" || part.Type == "text" {
					lines = append(lines, part.Text)
				}
			}
			if len(lines) > 0 {
				return boundedText(strings.Join(lines, "\n"), 4096)
			}
		}
	}
	return ""
}

func (r *relay) queueBPSTitle(req *http.Request, meta requestMetadata, key string) {
	root := r.root()
	if root.titleSlots == nil || key == "" || responsesPath(req.URL.Path) == "/responses/compact" {
		return
	}
	message := titleUserMessage(meta.Input)
	if message == "" {
		return
	}
	r.mu.Lock()
	s := r.sessions[key]
	if r.ctx.Err() != nil || s == nil || s.titleAttempted || s.TitleSource == "请求" || (s.TitleSource == "Codex本地记录" && (s.titleLocalNamed || usableLocalTitle(s.Title))) {
		r.mu.Unlock()
		return
	}
	s.titleAttempted = true
	s.TitleState = "生成中"
	if s.TitleSource == "" {
		s.Title, s.TitleSource = message, "用户消息摘录"
	}
	id, account := s.ID, r.account.ID
	if account == "" {
		account = accountIdentity(req)
	}
	homes := append([]string(nil), r.sessionHomes...)
	headers := req.Header.Clone()
	r.jobs.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.jobs.Done()
		// Resolve existing local titles first, including account-specific homes.
		if len(homes) == 0 {
			homes = []string{codexHome()}
		}
		for _, home := range homes {
			if title := readSessionTitleInfo(home, map[string]bool{id: true})[id]; title.Text != "" {
				r.mu.Lock()
				if current := r.sessions[key]; current == s {
					applyLocalTitle(s, title.Text, title.Named)
				}
				r.mu.Unlock()
			}
		}
		r.mu.RLock()
		local := s.TitleSource == "请求" || (s.TitleSource == "Codex本地记录" && (s.titleLocalNamed || usableLocalTitle(s.Title)))
		r.mu.RUnlock()
		if local {
			return
		}
		cacheKey := titleCacheKey(account, id)
		title := r.cachedTitle(cacheKey, "")
		var err error
		if title == "" {
			ctx, cancel := context.WithTimeout(root.ctx, 20*time.Second)
			defer cancel()
			select {
			case root.titleSlots <- struct{}{}:
				title, err = r.fetchBPSTitle(ctx, headers, id, message)
				<-root.titleSlots
			case <-ctx.Done():
				err = ctx.Err()
			}
			if err == nil {
				r.cachedTitle(cacheKey, title)
			}
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.sessions[key] != s {
			return
		}
		if s.TitleSource == "请求" || (s.TitleSource == "Codex本地记录" && (s.titleLocalNamed || usableLocalTitle(s.Title))) {
			return
		}
		if err != nil {
			s.TitleState, s.TitleError = "生成失败，使用现有标题或消息摘录", boundedText(err.Error(), 160)
			return
		}
		s.Title, s.TitleSource, s.TitleState, s.TitleError = title, "BPS自动标题", "已生成", ""
	}()
}

// Ground truth: official app-DyvH3mJg.js THr() and generateTaskTitle(),
// independently verified on 2026-09-29 with second: HTTP 200 {title, model}.
func (r *relay) fetchBPSTitle(ctx context.Context, headers http.Header, id, message string) (string, error) {
	// Stable UUID for non-UUID client session IDs; never reuse a different account's task.
	task := id
	if !bpsSessionUUID.MatchString(task) {
		hash := titleCacheKey(headers.Get("Chatgpt-Account-Id"), id)
		task = hash[:8] + "-" + hash[8:12] + "-" + hash[12:16] + "-" + hash[16:20] + "-" + hash[20:32]
	}
	raw, _ := json.Marshal(map[string]string{"task_id": task, "locale": "zh-CN", "user_message": message})
	q, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://bps.openai.com/basispoints/api/task-title", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	for _, name := range []string{"Authorization", "Chatgpt-Account-Id", "X-Openai-Account-Id", "X-Basispoints-Auth-Mode"} {
		q.Header.Set(name, headers.Get(name))
	}
	setBPSHeaders(q)
	q.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: r.transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(q)
	if err != nil {
		return "", fmt.Errorf("标题服务连接失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("标题服务 HTTP %d", resp.StatusCode)
	}
	var result struct {
		Title string `json:"title"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result) != nil {
		return "", fmt.Errorf("标题服务返回格式错误")
	}
	title := boundedText(strings.Join(strings.Fields(result.Title), " "), 120)
	if title == "" {
		return "", fmt.Errorf("标题服务返回空标题")
	}
	return title, nil
}
