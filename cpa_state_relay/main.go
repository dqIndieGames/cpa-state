package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	relayAddr    = "127.0.0.1:17992"
	probeAddr    = "127.0.0.1:17993"
	stateTTL     = 230 * time.Second
	defaultModel = "gpt-6-astra"
	waitTimeout  = 90 * time.Second
)

type relaySettings struct {
	PanelMode        string `json:"panel_mode,omitempty"`
	OwnerAccount     string `json:"owner_account,omitempty"`
	IndependentProxy bool   `json:"independent_proxy"`
	TicketEnabled    bool   `json:"ticket_enabled"`
	TicketWrite      bool   `json:"ticket_write"`
	BPS              bool   `json:"bps_enabled"`
	WindowX          int32  `json:"window_x"`
	WindowY          int32  `json:"window_y"`
	PositionSaved    bool   `json:"position_saved"`
}
type ticket struct {
	State, Cookie string
	GotAt         time.Time
}

func (t ticket) valid(now time.Time) bool { return t.State != "" && now.Before(t.GotAt.Add(stateTTL)) }

type observation struct {
	Length int
	At     time.Time
	Source string
}
type poolState struct {
	SourceModel, FetchModel, Account string
	Ticket                           ticket
	LastIssued                       ticket
	Observed                         observation
	LastAttempt, NextAttempt         time.Time
	LastError                        string
	LastHTTP, Attempts, Reused       int
	Running                          bool
}
type modelState struct {
	Observed observation
	LastHTTP int
}
type harvestAttempt struct {
	Model       string
	Cancel      context.CancelFunc
	Result      chan error
	Responded   bool
	Independent bool
	Epoch       uint64
}
type relay struct {
	titleCache            bpsTitleCache
	titleSlots            chan struct{}
	bpsCache              bpsCallCache
	bpsFailures           bpsFailureStore
	accountsMu            sync.RWMutex
	harvestMu             sync.Mutex
	accounts              map[string]*relay
	parent                *relay
	account               accountInfo
	authFile, accountHome string
	lastTransfer          transferView
	transferSequence      uint64
	headless              bool
	restartPending        bool

	mu                                   sync.RWMutex
	settingsMu                           sync.Mutex
	routeMu                              sync.Mutex
	settings                             relaySettings
	models                               map[string]*modelState
	pool                                 poolState
	sessions                             map[string]*sessionRecord
	errorHistory                         map[string][]sessionError
	sequence                             uint64
	unidentified                         int
	sessionHomes                         []string
	attempts                             map[string]*harvestAttempt
	changed                              chan struct{}
	epoch                                uint64
	active, waiting, requests, forwarded int
	lastModel, globalError               string
	ctx                                  context.Context
	cancel                               context.CancelFunc
	jobs                                 sync.WaitGroup
	mihomo                               *exec.Cmd
	runtimeDir, settingsFile, address    string
	upstream                             string
	transport                            http.RoundTripper
	transports                           sessionTransports
	harvestTransport                     func(bool) *http.Transport
	launchCLI                            func(context.Context, string, string) error
}

func newRelay() *relay {
	ctx, cancel := context.WithCancel(context.Background())
	return &relay{ctx: ctx, cancel: cancel, address: relayAddr, upstream: "https://chatgpt.com", transport: newTransport(false), models: map[string]*modelState{}, sessions: map[string]*sessionRecord{}, attempts: map[string]*harvestAttempt{}, changed: make(chan struct{}), lastModel: defaultModel}
}
func (r *relay) modelLocked(model string) *modelState {
	m := r.models[model]
	if m == nil {
		m = &modelState{}
		r.models[model] = m
	}
	return m
}
func (r *relay) signalLocked() { close(r.changed); r.changed = make(chan struct{}) }
func (r *relay) settingsPath() string {
	if r.settingsFile != "" {
		return r.settingsFile
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "cpa-state-relay", "settings.json")
}
func (r *relay) loadSettings() {
	b, e := os.ReadFile(r.settingsPath())
	if e == nil {
		_ = json.Unmarshal(b, &r.settings)
	}
}
func (r *relay) saveSettings() error {
	r.settingsMu.Lock()
	defer r.settingsMu.Unlock()
	r.mu.RLock()
	b, e := json.MarshalIndent(r.settings, "", "  ")
	r.mu.RUnlock()
	if e != nil {
		return e
	}
	path := r.settingsPath()
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	temp := path + ".tmp"
	if e = os.WriteFile(temp, b, 0600); e != nil {
		return e
	}
	return os.Rename(temp, path)
}
func (r *relay) report(e error) {
	if e != nil {
		r.mu.Lock()
		r.globalError = e.Error()
		r.mu.Unlock()
	}
}
func (r *relay) setEnabled(enabled bool) error {
	r.mu.RLock()
	allowed := r.harvestAllowedLocked()
	r.mu.RUnlock()
	if enabled && !allowed {
		return fmt.Errorf("请先关闭 BPS 并绑定登录资料，再启用打票")
	}
	r.mu.Lock()
	if r.settings.TicketEnabled != enabled {
		r.settings.TicketEnabled = enabled
		r.epoch++
		for _, a := range r.attempts {
			a.Cancel()
		}
		r.pool.Ticket = ticket{}
		r.pool.NextAttempt = time.Time{}
		r.signalLocked()
	}
	r.mu.Unlock()
	return r.saveSettings()
}
func (r *relay) setTicketWrite(enabled bool) error {
	r.mu.Lock()
	if r.settings.TicketWrite != enabled {
		r.settings.TicketWrite = enabled
		r.signalLocked()
	}
	r.mu.Unlock()
	return r.saveSettings()
}
func (r *relay) setIndependent(enabled bool) error {
	if r.parent != nil {
		return r.parent.setIndependent(enabled)
	}
	r.routeMu.Lock()
	defer r.routeMu.Unlock()
	r.mu.RLock()
	previous := r.settings.IndependentProxy
	r.mu.RUnlock()
	if enabled && !previous {
		if e := r.startMihomo(); e != nil {
			return e
		}
	}
	r.mu.Lock()
	r.settings.IndependentProxy = enabled
	r.epoch++
	for _, a := range r.attempts {
		a.Cancel()
	}
	r.pool.Ticket = ticket{}
	r.pool.NextAttempt = time.Time{}
	r.signalLocked()
	r.mu.Unlock()
	for _, child := range r.accountRelays() {
		if child == r {
			continue
		}
		child.mu.Lock()
		child.settings.IndependentProxy = enabled
		child.epoch++
		for _, a := range child.attempts {
			a.Cancel()
		}
		child.pool.Ticket = ticket{}
		child.pool.NextAttempt = time.Time{}
		child.signalLocked()
		child.mu.Unlock()
	}
	if !enabled {
		r.stopMihomo()
	}
	return r.saveSettings()
}
func (r *relay) stop() {
	r.cancel()
	if r.parent == nil {
		for _, child := range r.accountRelays() {
			if child != r {
				child.stop()
			}
		}
	}
	r.mu.Lock()
	for _, a := range r.attempts {
		a.Cancel()
	}
	r.signalLocked()
	r.mu.Unlock()
	r.jobs.Wait()
	r.routeMu.Lock()
	r.stopMihomo()
	r.routeMu.Unlock()
	if t, ok := r.transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	r.transports.closeIdle()
}
func (r *relay) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", r.handleStatus)
	mux.HandleFunc("/api/toggle", r.handleToggle)
	mux.HandleFunc("/api/probe", r.handleProbe)
	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, req *http.Request) {
		if !localMutation(w, req) {
			return
		}
		if err := r.startRestart(); err != nil {
			r.report(err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, req *http.Request) {
		if localMutation(w, req) {
			w.WriteHeader(http.StatusAccepted)
			r.cancel()
		}
	})
	mux.HandleFunc("/", r.dispatchForward)
	return mux
}
func localMutation(w http.ResponseWriter, req *http.Request) bool {
	if req.Method != "POST" {
		http.Error(w, "POST required", 405)
		return false
	}
	if o := req.Header.Get("Origin"); o != "" && o != "http://"+req.Host {
		http.Error(w, "Origin rejected", 403)
		return false
	}
	if req.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "Cross-site rejected", 403)
		return false
	}
	return true
}
func (r *relay) handleToggle(w http.ResponseWriter, req *http.Request) {
	target := r.apiAccount(w, req)
	if target == nil {
		return
	}
	if !localMutation(w, req) {
		return
	}
	var in struct {
		Independent *bool `json:"independent_proxy"`
		Enabled     *bool `json:"ticket_enabled"`
		Write       *bool `json:"ticket_write"`
		BPS         *bool `json:"bps_enabled"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1024)).Decode(&in); e != nil {
		http.Error(w, "Invalid settings", 400)
		return
	}
	var e error
	if in.BPS != nil {
		e = target.setBPS(*in.BPS)
	}
	if e == nil && in.Independent != nil {
		e = r.setIndependent(*in.Independent)
	}
	if e == nil && in.Enabled != nil {
		e = target.setEnabled(*in.Enabled)
	}
	if e == nil && in.Write != nil {
		e = target.setTicketWrite(*in.Write)
	}
	if e != nil {
		r.report(e)
		http.Error(w, e.Error(), 500)
		return
	}
	r.handleStatus(w, req)
}
func (r *relay) handleProbe(w http.ResponseWriter, req *http.Request) {
	target := r.apiAccount(w, req)
	if target == nil {
		return
	}
	if !localMutation(w, req) {
		return
	}
	target.mu.RLock()
	model := target.lastModel
	enabled := target.settings.TicketEnabled && target.harvestAllowedLocked()
	target.mu.RUnlock()
	if !enabled {
		http.Error(w, "票据功能已关闭，请先开启", 409)
		return
	}
	target.startHarvest(model, true)
	w.WriteHeader(http.StatusAccepted)
}

func main() {
	r := newRelay()
	r.titleSlots = make(chan struct{}, 2)

	flag.StringVar(&r.address, "listen", relayAddr, "Local listen address")
	flag.StringVar(&r.settingsFile, "settings", "", "Settings file")
	flag.StringVar(&r.accountHome, "account-home", "", "Read-only authentication directory")
	flag.BoolVar(&r.headless, "headless", false, "Run without windows or tray")
	flag.Parse()
	host, _, err := net.SplitHostPort(r.address)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		fmt.Fprintln(os.Stderr, "Loopback address required")
		return
	}
	r.loadSettings()
	r.discoverAccounts()
	listener, e := net.Listen("tcp", r.address)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		return
	}
	if r.settings.IndependentProxy {
		if e = r.startMihomo(); e != nil {
			for _, a := range r.accountRelays() {
				a.mu.Lock()
				a.settings.IndependentProxy = false
				a.mu.Unlock()
			}
			r.report(e)
		}
	}
	if e := r.saveSettings(); e != nil {
		r.report(e)
	}
	sig, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	server := &http.Server{Handler: r.handler(), ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 90 * time.Second}
	go func() {
		if e := server.Serve(listener); e != nil && !errors.Is(e, http.ErrServerClosed) {
			r.report(e)
			r.cancel()
		}
	}()
	go r.refreshLoop()
	go r.refreshSessionTitles()
	go func() {
		select {
		case <-sig.Done():
			r.cancel()
		case <-r.ctx.Done():
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = server.Close()
		closeNativeWindow()
	}()
	if r.headless {
		<-r.ctx.Done()
	} else {
		go r.runTray()
		r.runWindow()
	}
	r.stop()
}
