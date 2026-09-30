package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const buildVersion = "multi-account-20260929-async-image-recovery"

type accountInfo struct{ ID, Name, Email string }
type authDocument struct {
	Tokens struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

// Claims are display hints only. Upstream remains responsible for authentication.
func tokenInfo(token string) accountInfo {
	var claims struct {
		Name    string `json:"name"`
		Email   string `json:"email"`
		Profile struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
		Auth struct {
			ID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if len(token) > 65536 {
		return accountInfo{}
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return accountInfo{}
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil || json.Unmarshal(b, &claims) != nil {
		return accountInfo{}
	}
	if claims.Name == "" {
		claims.Name = claims.Profile.Name
	}
	if claims.Email == "" {
		claims.Email = claims.Profile.Email
	}
	id := ""
	if claims.Auth.ID != "" {
		id = "account:" + claims.Auth.ID
	}
	return accountInfo{id, boundedText(strings.Join(strings.Fields(claims.Name), " "), 120), boundedText(strings.Join(strings.Fields(claims.Email), " "), 200)}
}
func authInfo(b []byte) (accountInfo, error) {
	var doc authDocument
	if json.Unmarshal(b, &doc) != nil || doc.Tokens.AccountID == "" || doc.Tokens.AccessToken == "" {
		return accountInfo{}, fmt.Errorf("登录资料缺少账号或访问令牌")
	}
	info := tokenInfo(doc.Tokens.IDToken)
	access := tokenInfo(doc.Tokens.AccessToken)
	id := "account:" + doc.Tokens.AccountID
	if (info.ID != "" && info.ID != id) || (access.ID != "" && access.ID != id) {
		return accountInfo{}, fmt.Errorf("登录资料账号不一致")
	}
	info.ID = id
	if info.Name == "" {
		info.Name = access.Name
	}
	if info.Email == "" {
		info.Email = access.Email
	}
	return info, nil
}
func (r *relay) root() *relay {
	if r.parent != nil {
		return r.parent
	}
	return r
}
func (r *relay) accountRelays() []*relay {
	root := r.root()
	root.accountsMu.RLock()
	defer root.accountsMu.RUnlock()
	out := []*relay{root}
	for _, a := range root.accounts {
		if a != root {
			out = append(out, a)
		}
	}
	sort.Slice(out[1:], func(i, j int) bool {
		a, b := out[i+1], out[j+1]
		a.mu.RLock()
		idA := a.account.ID
		a.mu.RUnlock()
		b.mu.RLock()
		idB := b.account.ID
		b.mu.RUnlock()
		return idA < idB
	})
	return out
}
func (r *relay) addAccountLocked(info accountInfo) *relay {
	if a := r.accounts[info.ID]; a != nil {
		return a
	}
	a := newRelay()
	a.parent = r
	a.account = info
	a.address = r.address
	a.upstream = r.upstream
	a.harvestTransport = r.harvestTransport
	if base, ok := r.transport.(*http.Transport); ok {
		a.transport = base.Clone()
	} else {
		a.transport = r.transport
	}
	sum := sha256.Sum256([]byte(info.ID))
	a.settingsFile = filepath.Join(filepath.Dir(r.settingsPath()), "accounts", fmt.Sprintf("%x.json", sum[:12]))
	a.loadSettings()
	r.mu.RLock()
	a.settings.IndependentProxy = r.settings.IndependentProxy
	r.mu.RUnlock()
	r.accounts[info.ID] = a
	return a
}
func (r *relay) discoverAccounts() {
	home := r.accountHome
	if home == "" {
		home = codexHome()
	}
	paths := []string{filepath.Join(home, "auth.json")}
	nested, _ := filepath.Glob(filepath.Join(home, "accounts", "*", "auth.json"))
	paths = append(paths, nested...)
	r.accountsMu.Lock()
	defer r.accountsMu.Unlock()
	if r.accounts == nil {
		r.accounts = map[string]*relay{}
	}
	bound := map[*relay]bool{}
	for _, p := range paths {
		b, e := os.ReadFile(p)
		if e != nil {
			continue
		}
		info, e := authInfo(b)
		if e != nil {
			continue
		}
		var a *relay
		if r.account.ID == "" && len(r.accounts) == 0 {
			a = r
			r.mu.Lock()
			r.account = info
			if r.settings.OwnerAccount != "" && r.settings.OwnerAccount != info.ID {
				r.settings.TicketEnabled = false
				r.settings.TicketWrite = false
				r.settings.BPS = false
			}
			r.settings.OwnerAccount = info.ID
			r.mu.Unlock()
			r.accounts[info.ID] = r
		} else {
			a = r.addAccountLocked(info)
		}
		if bound[a] {
			continue
		}
		bound[a] = true
		a.mu.Lock()
		a.account = info
		a.authFile = p
		a.sessionHomes = []string{filepath.Dir(p), home}
		a.mu.Unlock()
	}
	for _, a := range r.accounts {
		if !bound[a] {
			a.mu.Lock()
			if a.authFile != "" {
				a.authFile = ""
				a.pool.Ticket = ticket{}
				a.epoch++
				for _, task := range a.attempts {
					task.Cancel()
				}
				a.signalLocked()
			}
			a.mu.Unlock()
		}
	}
}
func (r *relay) dispatchForward(w http.ResponseWriter, req *http.Request) {
	r.accountsMu.RLock()
	enabled := r.accounts != nil
	r.accountsMu.RUnlock()
	if !enabled || req.URL.Path == "/" {
		r.handleForward(w, req)
		return
	}
	if strings.HasPrefix(req.URL.Path, "/_harvest/") {
		key, _, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, "/_harvest/"), "/")
		for _, a := range r.accountRelays() {
			a.mu.RLock()
			found := a.attempts[key] != nil
			a.mu.RUnlock()
			if found {
				a.handleForward(w, req)
				return
			}
		}
		http.Error(w, "Unknown harvest task", 403)
		return
	}
	id := accountIdentity(req)
	if id == "" {
		id = "unidentified"
	}
	info := tokenInfo(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	if info.ID != "" && info.ID != id {
		http.Error(w, "Account identity mismatch", 503)
		return
	}
	if info.ID != id {
		info = accountInfo{ID: id}
	} // Never attach another workspace's display identity.
	r.accountsMu.Lock()
	a := r.addAccountLocked(info)
	r.accountsMu.Unlock()
	a.mu.Lock()
	if a.authFile == "" {
		if info.Name != "" {
			a.account.Name = info.Name
		}
		if info.Email != "" {
			a.account.Email = info.Email
		}
	}
	a.mu.Unlock()
	a.handleForward(w, req)
}
func (r *relay) apiAccount(w http.ResponseWriter, req *http.Request) *relay {
	id := req.URL.Query().Get("account")
	if id == "" {
		return r
	}
	r.accountsMu.RLock()
	a := r.accounts[id]
	r.accountsMu.RUnlock()
	if a == nil {
		http.Error(w, "Unknown account", 404)
	}
	return a
}
func (r *relay) harvestAllowedLocked() bool {
	return !r.settings.BPS && (r.account.ID == "" || r.authFile != "")
}

func (r *relay) scheduleAccounts() {
	type candidate struct {
		r       *relay
		waiting int
		at      time.Time
	}
	var candidates []candidate
	for _, a := range r.accountRelays() {
		a.mu.RLock()
		if a.settings.TicketEnabled && a.harvestAllowedLocked() && !time.Now().Before(a.pool.NextAttempt) {
			candidates = append(candidates, candidate{a, a.waiting, a.pool.LastAttempt})
		}
		a.mu.RUnlock()
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if (candidates[i].waiting > 0) != (candidates[j].waiting > 0) {
			return candidates[i].waiting > 0
		}
		return candidates[i].at.Before(candidates[j].at)
	})
	for _, c := range candidates {
		c.r.mu.RLock()
		model := c.r.lastModel
		c.r.mu.RUnlock()
		c.r.startHarvest(model, false)
	}
}
