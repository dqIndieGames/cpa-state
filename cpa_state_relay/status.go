package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

type modelView struct {
	TicketLength                                                                      int
	FetchModel                                                                        string
	Model, Phase, Error, ObservationSource, ObservedAt                                string
	Remaining, ObservedLength, FetchSeconds, RetrySeconds, Attempts, Reused, LastHTTP int
	Running, Valid                                                                    bool
}
type statusView struct {
	UIReady                               bool
	Version                               string
	PID                                   int
	Accounts                              []accountView
	Enabled, Independent, TicketWrite     bool
	BPS                                   bool
	Active, Waiting, Requests, Forwarded  int
	Models                                []modelView
	Shared                                modelView
	Sessions                              []sessionView
	Passed, Failed, Pending, Unidentified int
	CurrentModel, Proxy, Error            string
}

func (r *relay) singleStatus() statusView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := statusView{Enabled: r.settings.TicketEnabled, TicketWrite: r.settings.TicketWrite, Independent: r.settings.IndependentProxy, Active: r.active, Waiting: r.waiting, Requests: r.requests, Forwarded: r.forwarded, CurrentModel: r.lastModel, Proxy: "系统网络", Error: r.globalError}
	s.BPS = r.settings.BPS
	if s.Independent {
		s.Proxy = "独立代理"
	}
	now := time.Now()
	{
		model, m := r.pool.SourceModel, &r.pool
		v := modelView{Model: model, Running: m.Running, Valid: m.Ticket.valid(now), ObservedLength: m.Observed.Length, ObservationSource: m.Observed.Source, Attempts: m.Attempts, Reused: m.Reused, LastHTTP: m.LastHTTP, Error: m.LastError}
		v.FetchModel = m.FetchModel
		if v.Valid {
			v.TicketLength = len(m.Ticket.State)
		}
		if !m.Observed.At.IsZero() {
			v.ObservedAt = m.Observed.At.Format("15:04:05")
		}
		if v.Valid {
			v.Remaining = int((m.Ticket.GotAt.Add(stateTTL).Sub(now) + time.Second - 1) / time.Second)
		}
		if v.Running {
			v.FetchSeconds = int(now.Sub(m.LastAttempt) / time.Second)
		}
		if m.NextAttempt.After(now) {
			v.RetrySeconds = int((m.NextAttempt.Sub(now) + time.Second - 1) / time.Second)
		}
		switch {
		case s.BPS:
			v.Phase = "BPS · 打票与写入暂停"
		case !r.harvestAllowedLocked():
			v.Phase = "未绑定登录资料"
		case !s.Enabled:
			v.Phase = "透明转发"
		case v.Valid && v.Running:
			v.Phase = fmt.Sprintf("%d 可用 · 补票中", len(m.Ticket.State))
		case v.Valid:
			v.Phase = fmt.Sprintf("%d 可用", len(m.Ticket.State))
		case v.Running:
			v.Phase = "获取有效票据"
		case v.Error != "":
			v.Phase = "获取失败"
		default:
			v.Phase = "等待有效票据"
		}
		s.Shared = v
	}
	for model, m := range r.models {
		v := s.Shared
		v.Model = model
		v.ObservedLength = m.Observed.Length
		v.ObservationSource = m.Observed.Source
		v.LastHTTP = m.LastHTTP
		v.ObservedAt = ""
		if !m.Observed.At.IsZero() {
			v.ObservedAt = m.Observed.At.Format("15:04:05")
		}
		s.Models = append(s.Models, v)
	}
	s.Sessions, s.Passed, s.Failed, s.Pending = r.sessionViewsLocked()
	s.Unidentified = r.unidentified
	sort.Slice(s.Models, func(i, j int) bool { return s.Models[i].Model < s.Models[j].Model })
	return s
}
func (s statusView) selected() modelView {
	for _, m := range s.Models {
		if m.Model == s.CurrentModel {
			return m
		}
	}
	return s.Shared
}
func (s statusView) lines() (string, string, string, string, uint32) {
	m := s.selected()
	color := uint32(0xc6a66a)
	top := m.Phase
	if s.BPS {
		top = "BPS · 打票与写入暂停"
	} else if s.Enabled {
		switch {
		case m.Valid:
			top += fmt.Sprintf("  剩余 %d秒", m.Remaining)
			color = 0x91d46e
		case m.Running:
			top += fmt.Sprintf("  已等 %d秒", m.FetchSeconds)
			color = 0x68c8f3
		case m.Error != "":
			top += fmt.Sprintf("  %d秒后重试", m.RetrySeconds)
			color = 0x8585f5
		}
	} else {
		if m.ObservedAt != "" {
			if m.ObservedLength > 0 {
				top += fmt.Sprintf("  最近 %d", m.ObservedLength)
			} else {
				top += "  最近无票"
			}
		} else {
			top += "  尚未观测"
		}
	}
	bottom := fmt.Sprintf("%s · 并发 %d · 等待 %d", s.Proxy, s.Active, s.Waiting)
	detail := fmt.Sprintf("会话  通过 %d  ·  未通过 %d  ·  待判定 %d", s.Passed, s.Failed, s.Pending)
	extra := fmt.Sprintf("共享池 · 来源 %s · 取票 %d次 · 复用 %d次", s.Shared.Model, m.Attempts, m.Reused)
	if m.Error != "" {
		extra = m.Error
	}
	if s.Error != "" {
		extra = s.Error
	}
	return top, bottom, detail, extra, color
}
func (r *relay) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(r.status())
}

type accountView struct {
	ID, Name, Email string
	Bound           bool
	State           statusView
	Latest          transferView
}

func (a accountView) displayName() string {
	if a.Name != "" {
		return a.Name
	}
	if a.Email != "" {
		return a.Email
	}
	if len(a.ID) > 8 {
		return "未识别账号 · " + a.ID[len(a.ID)-8:]
	}
	return "未识别账号"
}
func (r *relay) status() statusView {
	s := r.singleStatus()
	s.Version = buildVersion
	s.PID = os.Getpid()
	s.UIReady = r.headless || nativeWindow.Load() != 0
	if r.parent != nil {
		return s
	}
	s.Active = 0
	s.Waiting = 0
	s.Requests = 0
	s.Forwarded = 0
	s.Passed = 0
	s.Failed = 0
	s.Pending = 0
	s.Unidentified = 0
	s.Sessions = nil
	for _, child := range r.accountRelays() {
		cs := child.singleStatus()
		child.mu.RLock()
		a := accountView{ID: child.account.ID, Name: child.account.Name, Email: child.account.Email, Bound: child.authFile != "", State: cs, Latest: child.lastTransfer}
		child.mu.RUnlock()
		s.Accounts = append(s.Accounts, a)
		s.Active += cs.Active
		s.Waiting += cs.Waiting
		s.Requests += cs.Requests
		s.Forwarded += cs.Forwarded
		s.Passed += cs.Passed
		s.Failed += cs.Failed
		s.Pending += cs.Pending
		s.Unidentified += cs.Unidentified
		s.Sessions = append(s.Sessions, cs.Sessions...)
	}
	return s
}
