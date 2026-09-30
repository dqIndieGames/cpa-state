package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type bpsContextKey struct{}

func bpsRequest(req *http.Request) bool {
	on, _ := req.Context().Value(bpsContextKey{}).(bool)
	return on
}

func responsesPath(path string) string {
	for _, prefix := range []string{"", "/v1", "/backend-api/codex"} {
		for _, suffix := range []string{"/responses", "/responses/compact"} {
			if path == prefix+suffix {
				return suffix
			}
		}
	}
	return ""
}

// Freeze the route for this request. Toggling never redirects an existing stream.
func (r *relay) selectBPS(req *http.Request) (*http.Request, error) {
	r.mu.RLock()
	on := r.settings.BPS && responsesPath(req.URL.Path) != ""
	boundID := r.account.ID
	r.mu.RUnlock()
	if !on {
		return req, nil
	}
	if req.Method != http.MethodPost || req.Header.Get("Upgrade") != "" {
		return nil, fmt.Errorf("BPS requires HTTP POST; set supports_websockets=false")
	}
	auth := req.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("BPS requires a ChatGPT access token")
	}
	claimID := strings.TrimPrefix(tokenInfo(token).ID, "account:")
	id := req.Header.Get("Chatgpt-Account-Id")
	if id == "" {
		id = claimID
	}
	if id == "" || (claimID != "" && claimID != id) || (boundID != "" && boundID != "account:"+id) {
		return nil, fmt.Errorf("BPS account identity missing or mismatched")
	}
	if other := req.Header.Get("X-Openai-Account-Id"); other != "" && other != id {
		return nil, fmt.Errorf("BPS account headers disagree")
	}
	q := req.Clone(context.WithValue(req.Context(), bpsContextKey{}, true))
	q.Header.Set("Chatgpt-Account-Id", id)
	q.Header.Set("X-Openai-Account-Id", id)
	q.Header.Set("X-Basispoints-Auth-Mode", "chatgpt")
	return q, nil
}

func (r *relay) setBPS(on bool) error {
	r.mu.Lock()
	if r.settings.BPS != on {
		r.settings.BPS = on
		r.epoch++
		for _, task := range r.attempts {
			task.Cancel()
		}
		r.pool.Ticket = ticket{}
		r.pool.NextAttempt = time.Time{}
		r.signalLocked()
	}
	r.mu.Unlock()
	return r.saveSettings()
}

// Observe bounded SSE event metadata while forwarding every byte unchanged.
// A 200 header alone is not proof of a completed model response.
type bpsBody struct {
	io.ReadCloser
	line                               []byte
	sse, completed, finished, overflow bool
	failure                            string
	done                               func(string)
}

func (b *bpsBody) eventLine() {
	line := bytes.TrimSpace(b.line)
	name := ""
	if bytes.HasPrefix(line, []byte("event:")) {
		name = strings.TrimSpace(string(line[6:]))
	} else if bytes.HasPrefix(line, []byte("data:")) {
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) == nil {
			name = event.Type
			if (name == "response.failed" || name == "response.incomplete") && event.Response.Error.Message != "" {
				b.failure = "BPS: " + boundedText(event.Response.Error.Message, 2048)
				return
			}
		}
	}
	switch name {
	case "response.completed":
		b.completed = true
	case "response.failed", "response.incomplete", "error":
		b.failure = "BPS " + name
	}
}

func (b *bpsBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.sse {
		for _, c := range p[:n] {
			if c == '\n' {
				if !b.overflow {
					b.eventLine()
				}
				b.line = b.line[:0]
				b.overflow = false
			} else if len(b.line) < 65536 {
				b.line = append(b.line, c)
			} else {
				b.overflow = true
			}
		}
	}
	if err != nil && !b.finished {
		b.finished = true
		if b.sse && len(b.line) > 0 && !b.overflow {
			b.eventLine()
		}
		reason := b.failure
		if err != io.EOF {
			reason = "BPS 上游响应中断"
		}
		if reason == "" && b.sse && !b.completed {
			reason = "BPS 响应未完成"
		}
		b.done(reason)
	}
	return n, err
}

func (b *bpsBody) Close() error {
	if !b.finished {
		b.finished = true
		b.done("BPS 响应未读完")
	}
	return b.ReadCloser.Close()
}
