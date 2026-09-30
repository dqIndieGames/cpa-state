package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func randomN(n int64) int64 {
	x, e := rand.Int(rand.Reader, big.NewInt(n))
	if e != nil {
		return 0
	}
	return x.Int64()
}
func (r *relay) refreshLoop() {
	tick := time.NewTicker(time.Second)
	scans := 0
	defer tick.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
			scans++
			if scans%15 == 0 {
				r.discoverAccounts()
			}
			r.scheduleAccounts()
		}
	}
}
func (r *relay) startHarvest(model string, force bool) {
	if model == "" {
		return
	}
	root := r.root()
	root.harvestMu.Lock()
	defer root.harvestMu.Unlock()
	if force {
		r.mu.Lock()
		r.pool.NextAttempt = time.Time{}
		r.mu.Unlock()
	}
	for _, other := range root.accountRelays() {
		other.mu.RLock()
		busy := other.pool.Running
		other.mu.RUnlock()
		if busy {
			return
		}
	}
	r.mu.Lock()
	if !r.harvestAllowedLocked() {
		r.mu.Unlock()
		return
	}
	if !r.settings.TicketEnabled || r.ctx.Err() != nil {
		r.mu.Unlock()
		return
	}
	m := &r.pool
	if m.Running || (!force && time.Now().Before(m.NextAttempt)) {
		r.mu.Unlock()
		return
	}
	keyBytes := make([]byte, 24)
	_, _ = rand.Read(keyBytes)
	key := hex.EncodeToString(keyBytes)
	ctx, cancel := context.WithTimeout(r.ctx, 75*time.Second)
	a := &harvestAttempt{Model: model, Cancel: cancel, Result: make(chan error, 1), Independent: r.settings.IndependentProxy, Epoch: r.epoch}
	r.attempts[key] = a
	m.Running = true
	m.LastAttempt = time.Now()
	m.FetchModel = model
	m.Attempts++
	m.LastError = ""
	r.jobs.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.jobs.Done()
		defer cancel()
		runner := r.launchCLI
		if runner == nil {
			runner = r.runCLI
		}
		done := make(chan error, 1)
		go func() { done <- runner(ctx, model, key) }()
		var result error
		select {
		case result = <-a.Result:
			cancel()
			<-done
		case result = <-done:
			select {
			case result = <-a.Result:
			default:
				if result == nil {
					result = fmt.Errorf("CLI结束，未收到有效票据")
				}
			}
		case <-ctx.Done():
			result = ctx.Err()
			cancel()
			<-done
		}
		r.mu.Lock()
		delete(r.attempts, key)
		m := &r.pool
		m.Running = false
		if a.Epoch == r.epoch {
			if result == nil {
				m.NextAttempt = time.Now().Add(time.Duration(30+randomN(31)) * time.Second)
				m.LastError = ""
			} else {
				m.NextAttempt = time.Now().Add(30 * time.Second)
				if r.ctx.Err() == nil {
					m.LastError = result.Error()
				}
			}
		}
		r.signalLocked()
		r.mu.Unlock()
	}()
}

// A separate home isolates plugins, MCPs, project instructions and transient CLI files.
// Authentication is copied only for this short-lived process and removed afterwards.
func (r *relay) runCLI(ctx context.Context, model, key string) error {
	exe, e := exec.LookPath("codex.exe")
	if e != nil {
		return fmt.Errorf("找不到真实 codex.exe: %w", e)
	}
	root := filepath.Join(filepath.Dir(r.settingsPath()), "cli-tasks")
	if e = os.MkdirAll(root, 0700); e != nil {
		return e
	}
	dir, e := os.MkdirTemp(root, "harvest-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	r.mu.RLock()
	authPath := r.authFile
	expected := r.account.ID
	r.mu.RUnlock()
	if authPath == "" {
		if expected != "" {
			return fmt.Errorf("该账号尚未绑定登录资料")
		}
		authPath = filepath.Join(codexHome(), "auth.json")
	}
	auth, e := os.ReadFile(authPath)
	if e != nil {
		return fmt.Errorf("读取该账号登录资料失败")
	}
	if expected != "" {
		info, err := authInfo(auth)
		if err != nil || info.ID != expected {
			return fmt.Errorf("登录资料已切换账号，等待重新识别")
		}
	}
	if e = os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0600); e != nil {
		return e
	}
	work := filepath.Join(dir, "work")
	if e = os.Mkdir(work, 0700); e != nil {
		return e
	}
	config := `model_provider = "harvest"
model_reasoning_effort = "low"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
[features]
shell_tool = false
apply_patch_freeform = false
multi_agent = false
[model_providers.harvest]
name = "CPA CLI Harvest"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
request_max_retries = 0
stream_max_retries = 0
base_url = ` + strconv.Quote("http://"+r.address+"/_harvest/"+key+"/backend-api/codex") + "\n"
	if e = os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); e != nil {
		return e
	}
	questions := []string{"What is 7 plus 8?", "Name one primary color.", "What is the capital of France?", "How many sides does a triangle have?", "Name one planet in our solar system."}
	prompt := "Answer briefly in one sentence. Do not use tools, access files, or delegate. " + questions[randomN(int64(len(questions)))]
	cmd := exec.CommandContext(ctx, exe, "exec", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--color", "never", "--json", "-m", model, "-C", work, prompt)
	cmd.Dir = work
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		if !strings.EqualFold(name, "CODEX_HOME") && !strings.EqualFold(name, "OPENAI_API_KEY") && !strings.EqualFold(name, "OPENAI_BASE_URL") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	// No raw CLI output or credentials enter logs. The gateway reports safe HTTP outcomes.
	return runInJob(cmd)
}
