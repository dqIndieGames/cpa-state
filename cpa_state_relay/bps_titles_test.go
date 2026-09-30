package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// L1 contracts: match independent service values, isolate accounts, preserve
// local names, and never block or fail model forwarding for a title failure.
// Wire keys come from the official THr() frontend and live title-protocol.json.
func titleFixture(t *testing.T) (*relay, *http.Request, requestMetadata, string) {
	t.Helper()
	r := testRelay(t)
	r.settings.BPS = true
	r.titleSlots = make(chan struct{}, 2)
	r.account = accountInfo{ID: "account:second"}
	r.sessionHomes = []string{t.TempDir()}
	q := sessionRequest("title-session", "gpt-6-astra")
	q.Header.Set("Authorization", "Bearer "+fixtureAuth(t, t.TempDir(), "auth.json", "second"))
	q.Header.Set("Chatgpt-Account-Id", "second")
	q.Header.Set("X-Openai-Account-Id", "second")
	q.Header.Set("X-Basispoints-Auth-Mode", "chatgpt")
	meta := requestMetadata{Model: "gpt-6-astra", Input: json.RawMessage(`[{"role":"developer","content":"private system instructions"},{"role":"user","content":[{"type":"input_text","text":"Read the local file"}]}]`)}
	key, _ := r.beginSession(q, meta)
	return r, q, meta, key
}
func awaitTitle(t *testing.T, r *relay, key string) sessionView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows := r.singleStatus().Sessions
		if len(rows) > 0 && rows[0].TitleState != "生成中" {
			return rows[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("title worker did not finish")
	return sessionView{}
}
func TestBPSTitleProtocolAsyncCacheAndLocalPriority(t *testing.T) {
	r, q, meta, key := titleFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	want := "独立测试服务返回的标题"
	r.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(started)
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if req.URL.Path != "/basispoints/api/task-title" || req.Header.Get("Accept") != "application/json" || req.Header.Get("Chatgpt-Account-Id") != "second" || req.Header.Get("Authorization") != q.Header.Get("Authorization") {
			t.Error("title route or account mismatch")
		}
		if len(body) != 3 || body["user_message"] != "Read the local file" || body["locale"] != "zh-CN" || !bpsSessionUUID.MatchString(body["task_id"]) {
			t.Error("title wire contract mismatch")
		}
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		raw, _ := json.Marshal(map[string]string{"title": want})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	r.queueBPSTitle(q, meta, key)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker not started")
	}
	r.queueBPSTitle(q, meta, key)
	if r.singleStatus().Sessions[0].TitleSource != "用户消息摘录" {
		t.Fatal("pending fallback missing")
	}
	close(release)
	row := awaitTitle(t, r, key)
	if !strings.HasPrefix(row.Title, "[bps] ") || strings.TrimPrefix(row.Title, "[bps] ") != want || row.TitleSource != "BPS自动标题" || calls.Load() != 1 {
		t.Fatal("generated title or dedup failed")
	}
	other := testRelay(t)
	other.settingsFile = r.settingsFile
	cacheKey := titleCacheKey(r.account.ID, row.ID)
	if other.cachedTitle(cacheKey, "") != row.Title || other.cachedTitle(titleCacheKey("account:other", row.ID), "") != "" {
		t.Fatal("persistent cache crossed account boundary")
	}
	index := filepath.Join(r.sessionHomes[0], "session_index.jsonl")
	raw, _ := json.Marshal(map[string]string{"id": row.ID, "thread_name": "用户重新命名"})
	if err := os.WriteFile(index, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	r.resolveSessionTitles()
	if got := r.singleStatus().Sessions[0]; got.Title != "用户重新命名" || got.TitleSource != "Codex本地记录" {
		t.Fatal("local rename did not take priority")
	}
}

func TestBPSTitleLegacyCachePrefix(t *testing.T) {
	r := testRelay(t)
	key := titleCacheKey("account:second", "old-session")
	upstreamTitle := "旧缓存标题"
	raw, _ := json.Marshal(map[string]cachedBPSTitle{key: {Title: upstreamTitle, At: time.Now()}})
	path := filepath.Join(filepath.Dir(r.settingsPath()), "bps-titles.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	title := r.cachedTitle(key, "")
	if !strings.HasPrefix(title, "[bps] ") || strings.TrimPrefix(title, "[bps] ") != upstreamTitle || strings.Count(strings.ToLower(title), "[bps]") != 1 {
		t.Fatal("legacy cache title lost text or prefix")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatal("reading a legacy cache rewrote the file")
	}
	for _, input := range []string{upstreamTitle, "[bps] " + upstreamTitle, "[BPS] [bps] " + upstreamTitle, strings.Repeat("界", 140)} {
		got := r.cachedTitle(key, input)
		if !strings.HasPrefix(got, "[bps] ") || strings.Count(strings.ToLower(got), "[bps]") != 1 || len([]rune(got)) > 120 || r.cachedTitle(key, "") != got {
			t.Fatal("cache prefix, length or persistence invariant failed")
		}
	}
}
func TestBPSTitleFailuresAndExistingNames(t *testing.T) {
	for _, scenario := range []string{"http", "json", "empty", "local", "request"} {
		t.Run(scenario, func(t *testing.T) {
			r, q, meta, key := titleFixture(t)
			var calls atomic.Int32
			r.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := `{"title":""}`
				code := http.StatusOK
				if scenario == "http" {
					code = http.StatusServiceUnavailable
				}
				if scenario == "json" {
					body = "not json"
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			if scenario == "local" {
				raw, _ := json.Marshal(map[string]string{"id": "title-session", "thread_name": "本地标题"})
				_ = os.WriteFile(filepath.Join(r.sessionHomes[0], "session_index.jsonl"), append(raw, '\n'), 0600)
			}
			if scenario == "request" {
				r.sessions[key].Title = "客户端标题"
				r.sessions[key].TitleSource = "请求"
			}
			r.queueBPSTitle(q, meta, key)
			row := awaitTitle(t, r, key)
			if scenario == "local" || scenario == "request" {
				if calls.Load() != 0 || row.TitleError != "" {
					t.Fatal("existing title triggered remote request")
				}
				return
			}
			if row.TitleError == "" || row.TitleSource != "用户消息摘录" || row.Status != sessionPending {
				t.Fatal("title failure affected model state or lost fallback")
			}
			r.queueBPSTitle(q, meta, key)
			if calls.Load() != 1 {
				t.Fatal("failed title repeated within session")
			}
		})
	}
}
func TestBPSTitleLateReplyCannotOverwriteLocalOrReplacement(t *testing.T) {
	r, q, meta, key := titleFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	r.transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, context.Canceled
		}
		return &http.Response{StatusCode: http.StatusOK, Body: &titleNotifyBody{Reader: strings.NewReader(`{"title":"remote"}`), done: finished}}, nil
	})
	r.queueBPSTitle(q, meta, key)
	<-started
	r.mu.Lock()
	applyLocalTitle(r.sessions[key], "Local renamed title")
	r.mu.Unlock()
	close(release)
	<-finished
	row := awaitTitle(t, r, key)
	if row.Title != "Local renamed title" {
		t.Fatal("late title replaced local name")
	}
}

type titleNotifyBody struct {
	io.Reader
	done chan struct{}
}

func (b *titleNotifyBody) Close() error { close(b.done); return nil }

func TestBPSLongExplicitLocalTitleWins(t *testing.T) {
	r, q, meta, key := titleFixture(t)
	name := strings.Repeat("Human chosen long title ", 12)
	raw, _ := json.Marshal(map[string]string{"id": "title-session", "thread_name": name})
	if err := os.WriteFile(filepath.Join(r.sessionHomes[0], "session_index.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	r.transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("explicit long name must not trigger generation")
		return nil, context.Canceled
	})
	r.queueBPSTitle(q, meta, key)
	row := awaitTitle(t, r, key)
	if row.Title != strings.TrimSpace(name) || row.TitleSource != "Codex本地记录" {
		t.Fatal("long explicit name lost")
	}
}
