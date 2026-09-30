package main

import (
	"net/http"
	"sync"
	"time"
)

type sessionTransportEntry struct {
	transport *http.Transport
	active    int
	used      time.Time
}

type sessionTransports struct {
	mu      sync.Mutex
	entries map[string]*sessionTransportEntry
}

// Only connections are session-scoped. No body, cookie, ticket, or response is
// cached or replayed; each incoming client attempt is forwarded once.
func (r *relay) sessionTransport(req *http.Request, meta requestMetadata) (http.RoundTripper, func()) {
	base, ok := r.transport.(*http.Transport)
	if !ok {
		return r.transport, func() {}
	}
	id, _ := sessionIdentity(req, meta)
	if id == "" {
		id = "connection:" + req.RemoteAddr
	}
	key := accountIdentity(req) + "\x00" + id
	p := &r.transports
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = make(map[string]*sessionTransportEntry)
	}
	e := p.entries[key]
	if e == nil {
		if len(p.entries) >= maxSessions {
			oldestKey := ""
			var oldest time.Time
			for k, candidate := range p.entries {
				if candidate.active == 0 && (oldestKey == "" || candidate.used.Before(oldest)) {
					oldestKey, oldest = k, candidate.used
				}
			}
			if oldestKey != "" {
				p.entries[oldestKey].transport.CloseIdleConnections()
				delete(p.entries, oldestKey)
			}
		}
		e = &sessionTransportEntry{transport: base.Clone()}
		if len(p.entries) >= maxSessions {
			return e.transport, e.transport.CloseIdleConnections
		}
		p.entries[key] = e
	}
	e.active++
	e.used = time.Now()
	return e.transport, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		e.active--
		e.used = time.Now()
	}
}

func (p *sessionTransports) closeIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		e.transport.CloseIdleConnections()
	}
}
