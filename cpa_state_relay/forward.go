package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func accountIdentity(req *http.Request) string {
	if id := req.Header.Get("Chatgpt-Account-Id"); id != "" {
		return "account:" + id
	}
	if auth := req.Header.Get("Authorization"); auth != "" {
		if info := tokenInfo(strings.TrimPrefix(auth, "Bearer ")); info.ID != "" {
			return info.ID
		}
		return fmt.Sprintf("auth:%x", sha256.Sum256([]byte(auth)))
	}
	return ""
}

type observedBody struct {
	io.ReadCloser
	failed func()
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if e != nil && e != io.EOF {
		b.failed()
	}
	return n, e
}

func newTransport(independent bool) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// Separate HTTP/1.1 connections avoid cross-session HTTP/2 flow control and
	// preserve the bidirectional connection required by WebSocket upgrades.
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	t.DisableCompression = true
	t.MaxIdleConns = 64
	t.MaxIdleConnsPerHost = 32
	// The client owns response-header waiting and cancellation; do not cut it short.
	t.ResponseHeaderTimeout = 0
	// Ordinary traffic follows OS routing (including TUN), not environment proxies.
	t.Proxy = nil
	if independent {
		u, _ := url.Parse("http://" + probeAddr)
		t.Proxy = http.ProxyURL(u)
	}
	return t
}
func mergedCookies(original, paired string) string {
	if paired == "" {
		return original
	}
	replacements := map[string]bool{}
	for _, p := range strings.Split(paired, ";") {
		k, _, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok {
			replacements[k] = true
		}
	}
	var parts []string
	for _, p := range strings.Split(original, ";") {
		p = strings.TrimSpace(p)
		k, _, ok := strings.Cut(p, "=")
		if p != "" && (!ok || !replacements[k]) {
			parts = append(parts, p)
		}
	}
	return strings.Join(append(parts, paired), "; ")
}
func responseCookies(h http.Header) string {
	var parts []string
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == "__oailb" || c.Name == "__cflb" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}
func (r *relay) observeLocked(model, state, source string) {
	if model == "" {
		return
	}
	m := r.modelLocked(model)
	m.Observed = observation{Length: len(state), At: time.Now(), Source: source}
	r.lastModel = model
}
func (r *relay) waitTicket(ctx context.Context, model string) (ticket, error) {
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	r.mu.Lock()
	r.waiting++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.waiting--; r.mu.Unlock() }()
	for {
		r.mu.Lock()
		if !r.settings.TicketEnabled || !r.settings.TicketWrite || !r.harvestAllowedLocked() {
			r.mu.Unlock()
			return ticket{}, nil
		}
		t := r.pool.Ticket
		changed := r.changed
		r.mu.Unlock()
		if t.valid(time.Now()) {
			return t, nil
		}
		r.startHarvest(model, false)
		select {
		case <-ctx.Done():
			return ticket{}, ctx.Err()
		case <-r.ctx.Done():
			return ticket{}, r.ctx.Err()
		case <-timer.C:
			return ticket{}, fmt.Errorf("等待有效票据超时，请稍后重试")
		case <-changed:
		}
	}
}
func (r *relay) handleForward(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "CPA State · 请使用托盘中的原生状态窗口。状态接口：/api/status")
		return
	}
	token := ""
	if strings.HasPrefix(req.URL.Path, "/_harvest/") {
		rest := strings.TrimPrefix(req.URL.Path, "/_harvest/")
		var ok bool
		token, rest, ok = strings.Cut(rest, "/")
		if !ok {
			http.NotFound(w, req)
			return
		}
		r.mu.RLock()
		a := r.attempts[token]
		r.mu.RUnlock()
		if a == nil {
			http.Error(w, "Unknown harvest task", 403)
			return
		}
		clone := req.Clone(req.Context())
		clone.URL.Path = "/" + rest
		clone.URL.RawPath = ""
		req = clone
	}
	isResponse := req.Method == "POST" && (strings.HasSuffix(req.URL.Path, "/responses") || strings.HasSuffix(req.URL.Path, "/responses/compact"))
	isUpgrade := req.Method == "GET" && strings.EqualFold(req.Header.Get("Upgrade"), "websocket") && strings.HasSuffix(req.URL.Path, "/responses")
	model := ""
	var meta requestMetadata
	var originalBody []byte
	if isResponse {
		// Inspect only the model; send the original bytes unchanged, including whitespace.
		raw, e := io.ReadAll(req.Body)
		if e != nil {
			http.Error(w, "Cannot read request", 400)
			return
		}
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(raw))
		originalBody = raw
		_ = json.Unmarshal(raw, &meta)
		model = meta.Model
	}
	if token != "" {
		r.handleHarvestForward(w, req, token, model)
		return
	}
	var routeErr error
	req, routeErr = r.selectBPS(req)
	if routeErr != nil {
		http.Error(w, routeErr.Error(), http.StatusBadRequest)
		return
	}
	bps := bpsRequest(req)
	key, seq := "", uint64(0)
	if isResponse || isUpgrade {
		key, seq = r.beginSession(req, meta)
		defer r.endSession(key, seq, req.Context())
	}
	if bps {
		if prompt, ok := nativeBPSTitlePrompt(originalBody); ok && responsesPath(req.URL.Path) == "/responses" {
			r.handleNativeBPSTitle(w, req, meta, key, seq, prompt)
			return
		}
		req = r.prepareImageLadder(req, originalBody, key, seq, bps)
		prepared, err := r.prepareBPS(req, meta)
		if err != nil {
			r.finishSession(key, seq, 400, 0, err.Error())
			r.recordSessionError(key, seq, sessionError{Mode: "BPS", HTTP: 400, Type: "bps_request_error", Message: cleanDiagnostic(err.Error(), req, 2048)})
			r.logBPSRequestError(req, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "bps_request_error", "message": err.Error()}})
			return
		}
		req = prepared
		r.queueBPSTitle(req, meta, key)
	}
	if !bps && isResponse {
		req = r.prepareImageLadder(req, originalBody, key, seq, false)
	}
	r.mu.Lock()
	r.active++
	r.requests++
	if model != "" {
		r.observeLocked(model, req.Header.Get("X-Codex-Turn-State"), "请求")
	}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
	var used ticket
	if model != "" && !bps {
		var e error
		used, e = r.waitTicket(req.Context(), model)
		if e != nil {
			if req.Context().Err() == nil {
				r.finishSession(key, seq, 503, 0, e.Error())
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "state_unavailable", "message": e.Error()}})
			}
			return
		}
	}
	// Recheck mode/expiry at dispatch; toggles do not change an already-dispatched stream.
	r.mu.Lock()
	if bps || !r.settings.TicketEnabled || !r.settings.TicketWrite || !r.harvestAllowedLocked() {
		used = ticket{}
	} else if model != "" {
		// A route switch or replacement may have happened while this waiter woke up.
		used = r.pool.Ticket
		if accountIdentity(req) != r.pool.Account {
			r.mu.Unlock()
			r.finishSession(key, seq, 503, 0, "票据账号不匹配")
			http.Error(w, "Ticket account mismatch", 503)
			return
		}
		if !used.valid(time.Now()) {
			r.mu.Unlock()
			r.finishSession(key, seq, 503, 0, "票据已到期或出口已切换")
			http.Error(w, "Ticket expired or route changed before dispatch", 503)
			return
		}
	}
	if used.State != "" {
		r.pool.Reused++
	}
	r.mu.Unlock()
	transferSeq := uint64(0)
	if isResponse || isUpgrade {
		transferSeq = r.recordDispatch(key, seq, req, used)
	}
	transport, release := r.sessionTransport(req, meta)
	defer release()
	proxy := r.proxy(transport, used)
	dispatchedAt := time.Now()
	proxy.ErrorHandler = func(w http.ResponseWriter, q *http.Request, e error) {
		if q.Context().Err() == nil {
			r.logUpstreamError(q, e, time.Since(dispatchedAt))
			if bps {
				r.logBPSRequestError(q, fmt.Errorf("BPS upstream stream transport failed"))
			}
			r.recordResponse(key, seq, transferSeq, 0, 0, "上游网络错误")
			r.finishSession(key, seq, 502, 0, "上游网络错误")
			http.Error(w, "Upstream connection failed", 502)
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		length := len(resp.Header.Get("X-Codex-Turn-State"))
		if resp.StatusCode >= 400 {
			resp.Body = &upstreamErrorBody{ReadCloser: resp.Body, report: func(raw []byte) {
				e := parseUpstreamError(raw, resp.StatusCode, req, resp.Header, bps)
				r.recordResponse(key, seq, transferSeq, resp.StatusCode, length, e.summary())
				r.recordSessionError(key, seq, e)
			}}
		}
		r.recordResponse(key, seq, transferSeq, resp.StatusCode, length, "")
		if bps && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if exchange(req).Compact {
				if err := exchange(req).compactResponse(resp); err != nil {
					return err
				}
			}
			sse := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
			if sse {
				exchange(req).Observe = func(event string, pending int) { r.recordBPSEvent(key, seq, transferSeq, event, pending) }
				resp.Body = newBPSStream(resp.Body, exchange(req))
				resp.ContentLength = -1
				resp.Header.Del("Content-Length")
			}
			resp.Body = &bpsBody{ReadCloser: resp.Body, sse: sse, done: func(reason string) {
				if !sse && responsesPath(req.URL.Path) == "/responses" && reason == "" {
					reason = "BPS 返回非流式响应，未验证完成"
				}
				r.recordResponse(key, seq, transferSeq, resp.StatusCode, length, reason)
				r.finishSession(key, seq, resp.StatusCode, length, reason)
				if reason == "" {
					r.recoverSessionErrors(key, seq)
				} else {
					r.recordSessionError(key, seq, sessionError{Mode: "BPS", HTTP: resp.StatusCode, Type: "stream_error", Message: cleanDiagnostic(reason, req, 2048)})
				}
			}}
		} else {
			r.finishSession(key, seq, resp.StatusCode, length, "")
		}
		// A 101 body is an io.ReadWriteCloser. Wrapping it as a read-only body
		// prevents ReverseProxy from tunnelling the upgraded connection.
		if !bps && resp.StatusCode != http.StatusSwitchingProtocols {
			resp.Body = &observedBody{ReadCloser: resp.Body, failed: func() {
				if req.Context().Err() == nil {
					r.recordResponse(key, seq, transferSeq, resp.StatusCode, length, "上游响应中断")
					r.finishSession(key, seq, resp.StatusCode, length, "上游响应中断")
				}
			}}
		}
		if !bps && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body = &successBody{ReadCloser: resp.Body, sse: strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"), done: func() { r.recoverSessionErrors(key, seq) }, onFailure: func(raw []byte) {
				e := parseUpstreamError(raw, resp.StatusCode, req, resp.Header, false)
				r.recordResponse(key, seq, transferSeq, resp.StatusCode, length, e.summary())
				r.recordSessionError(key, seq, e)
			}}
		}
		if model != "" {
			r.mu.Lock()
			r.observeLocked(model, resp.Header.Get("X-Codex-Turn-State"), "响应")
			m := r.modelLocked(model)
			m.LastHTTP = resp.StatusCode
			if used.State != "" && length == 312 && r.pool.Ticket.State == used.State && r.pool.Ticket.GotAt.Equal(used.GotAt) {
				r.pool.Ticket = ticket{}
				r.pool.NextAttempt = time.Time{}
				r.signalLocked()
			}
			r.mu.Unlock()
		}
		return nil
	}
	proxy.ServeHTTP(w, req)
	r.mu.Lock()
	r.forwarded++
	r.mu.Unlock()
}
func (r *relay) proxy(transport http.RoundTripper, t ticket) *httputil.ReverseProxy {
	target, _ := url.Parse(r.upstream)
	return &httputil.ReverseProxy{
		Transport: transport, FlushInterval: -1,
		Rewrite: func(p *httputil.ProxyRequest) {
			if bpsRequest(p.In) {
				p.Out.URL.Scheme = "https"
				p.Out.URL.Host = "bps.openai.com"
				p.Out.Host = "bps.openai.com"
				p.Out.URL.Path = "/basispoints/api" + responsesPath(p.In.URL.Path)
				p.Out.URL.RawPath = ""
				p.Out.URL.RawQuery = p.In.URL.RawQuery
				setBPSHeaders(p.Out)
				return
			}
			p.Out.URL.Scheme = target.Scheme
			p.Out.URL.Host = target.Host
			p.Out.Host = target.Host
			if p.Out.URL.Path == "/responses" || strings.HasPrefix(p.Out.URL.Path, "/responses/") {
				p.Out.URL.Path = "/backend-api/codex" + p.Out.URL.Path
				p.Out.URL.RawPath = ""
			}
			// Rewrite preserves the original query, without ReverseProxy's normalization.
			p.Out.URL.RawQuery = p.In.URL.RawQuery
			for _, key := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
				if values, ok := p.In.Header[key]; ok {
					p.Out.Header[key] = append([]string(nil), values...)
				}
			}
			if t.State != "" {
				p.Out.Header.Set("X-Codex-Turn-State", t.State)
				if t.Cookie != "" {
					p.Out.Header.Set("Cookie", mergedCookies(strings.Join(p.Out.Header.Values("Cookie"), "; "), t.Cookie))
				}
			}
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, e error) {
			if req.Context().Err() == nil {
				http.Error(w, "Upstream connection failed", 502)
			}
		},
	}
}
func (r *relay) handleHarvestForward(w http.ResponseWriter, req *http.Request, key, model string) {
	r.mu.Lock()
	a := r.attempts[key]
	if a == nil || a.Responded || a.Model != model || !strings.HasSuffix(req.URL.Path, "/responses") || (r.account.ID != "" && accountIdentity(req) != r.account.ID) {
		r.mu.Unlock()
		http.Error(w, "Harvest request rejected", 403)
		return
	}
	// One upstream request per dedicated CLI process; retries cannot multiply probes.
	a.Responded = true
	r.mu.Unlock()
	factory := r.harvestTransport
	if factory == nil {
		factory = newTransport
	}
	transport := factory(a.Independent)
	defer transport.CloseIdleConnections()
	p := r.proxy(transport, ticket{})
	p.ModifyResponse = func(resp *http.Response) error {
		state := resp.Header.Get("X-Codex-Turn-State")
		var outcome error
		success := (resp.StatusCode == 200 || resp.StatusCode == 429) && goodTicketLength(len(state)) && strings.HasPrefix(state, "gAAAAA")
		if !success {
			outcome = fmt.Errorf("上游 HTTP %d · 票据 %d", resp.StatusCode, len(state))
		}
		r.mu.Lock()
		if a.Epoch == r.epoch && r.settings.TicketEnabled {
			r.observeLocked(model, state, "CLI取票响应")
			m := &r.pool
			m.LastHTTP = resp.StatusCode
			m.Observed = observation{Length: len(state), At: time.Now(), Source: "CLI取票响应"}
			if success {
				owner := accountIdentity(req)
				if m.Account != owner {
					m.Ticket = ticket{}
					m.LastIssued = ticket{}
				}
				m.Account = owner
				if m.LastIssued.State != state {
					m.LastIssued = ticket{state, responseCookies(resp.Header), time.Now()}
					m.SourceModel = model
				}
				m.Ticket = m.LastIssued
				m.LastError = ""
				r.signalLocked()
			}
		} else {
			outcome = context.Canceled
		}
		r.mu.Unlock()
		// Only this disposable CLI request is cut short; business streams are untouched.
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(strings.NewReader(""))
		resp.ContentLength = 0
		resp.Header.Del("Content-Length")
		select {
		case a.Result <- outcome:
		default:
		}
		return nil
	}
	p.ServeHTTP(w, req)
}
