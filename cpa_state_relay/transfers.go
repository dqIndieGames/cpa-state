package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type transferView struct {
	LastEvent, LastEventAt                        string
	PendingTools                                  int
	InputLength, SentLength, ResponseLength, HTTP int
	Injected, Dispatched, Responded               bool
	BPS                                           bool
	RequestedEffort, ActualEffort                 string
	At, Error                                     string
	sequence                                      uint64
}

func (r *relay) recordBPSEvent(key string, seq, transferSeq uint64, event string, pending int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	update := func(t *transferView) {
		t.LastEvent = boundedText(event, 100)
		t.LastEventAt = time.Now().Format(time.RFC3339)
		t.PendingTools = pending
	}
	if r.lastTransfer.sequence == transferSeq {
		update(&r.lastTransfer)
	}
	if s := r.sessions[key]; s != nil && s.sequence == seq {
		update(&s.Transfer)
		if s.Status == sessionPending {
			s.Reason = fmt.Sprintf("BPS 接收中 · %s · 待校验工具 %d", boundedText(event, 100), pending)
		}
	}
}
func (t transferView) text() string {
	if !t.Dispatched {
		return "尚无转发"
	}
	if t.BPS {
		if t.Error != "" {
			if strings.HasPrefix(t.Error, "BPS ") {
				return t.Error
			}
			return "BPS · " + t.Error
		}
		if t.Responded {
			return fmt.Sprintf("BPS · HTTP %d", t.HTTP)
		}
		return "BPS · 等待响应"
	}
	mode := "透传"
	if t.Injected {
		mode = "写入"
	}
	response := "等待响应"
	if t.Responded {
		if t.ResponseLength == 0 {
			response = "无票"
		} else {
			response = fmt.Sprint(t.ResponseLength)
		}
	}
	if t.Error != "" {
		response = t.Error
	}
	sent := "无票"
	if t.SentLength > 0 {
		sent = fmt.Sprint(t.SentLength)
	}
	return fmt.Sprintf("%s %s → %s", mode, sent, response)
}
func (t transferView) effortText() string {
	if !t.BPS || t.ActualEffort == "" {
		return ""
	}
	if t.RequestedEffort != t.ActualEffort {
		return fmt.Sprintf("BPS 不支持 %s，已自动降为 %s（实际发送）", t.RequestedEffort, t.ActualEffort)
	}
	return "推理档位：" + t.ActualEffort
}
func (r *relay) recordDispatch(key string, seq uint64, req *http.Request, used ticket) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transferSequence++
	t := transferView{InputLength: len(req.Header.Get("X-Codex-Turn-State")), Injected: used.State != "", Dispatched: true, At: time.Now().Format("15:04:05"), sequence: r.transferSequence}
	t.SentLength = t.InputLength
	t.BPS = bpsRequest(req)
	if t.BPS {
		t.SentLength = 0
		if x := exchange(req); x != nil {
			t.RequestedEffort, t.ActualEffort = x.Requested, x.Actual
		}
	}
	if t.Injected {
		t.SentLength = len(used.State)
	}
	r.lastTransfer = t
	if s := r.sessions[key]; s != nil && s.sequence == seq {
		s.Transfer = t
	}
	return t.sequence
}
func (r *relay) recordResponse(key string, seq, transferSeq uint64, status, length int, reason string) {
	if transferSeq == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	update := func(t *transferView) {
		t.HTTP = status
		t.ResponseLength = length
		t.Responded = status > 0
		t.Error = reason
	}
	if r.lastTransfer.sequence == transferSeq {
		update(&r.lastTransfer)
	}
	if s := r.sessions[key]; s != nil && s.sequence == seq {
		update(&s.Transfer)
	}
}
