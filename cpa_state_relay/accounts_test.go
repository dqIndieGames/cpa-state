package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// L1 invariants: user-requested per-account separation, source identity,
// unchanged client payloads, and actual sent/returned ticket attribution.
func fixtureAuth(t *testing.T, home, relative, id string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"name": "Person " + id, "email": id + "@example.test", "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": id}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture"
	b, _ := json.Marshal(map[string]any{"tokens": map[string]string{"account_id": id, "access_token": token, "id_token": token, "refresh_token": "fixture-refresh"}})
	path := filepath.Join(home, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return token
}
func accountFixture(t *testing.T) (*relay, *relay) {
	t.Helper()
	r := testRelay(t)
	r.accountHome = t.TempDir()
	fixtureAuth(t, r.accountHome, "auth.json", "a")
	fixtureAuth(t, r.accountHome, "accounts/second/auth.json", "b")
	r.discoverAccounts()
	return r, r.accounts["account:b"]
}
func TestAccountDiscoveryDedupAndChange(t *testing.T) {
	r, b := accountFixture(t)
	fixtureAuth(t, r.accountHome, "accounts/duplicate/auth.json", "a")
	r.discoverAccounts()
	if len(r.accounts) != len(map[string]bool{"a": true, "b": true}) || b.account.Name != "Person b" || b.account.Email != "b@example.test" {
		t.Fatal("identity or dedup failed")
	}
	b.pool.Ticket = ticket{State: "old", GotAt: time.Now()}
	b.settings.TicketEnabled = true
	fixtureAuth(t, r.accountHome, "accounts/second/auth.json", "c")
	r.discoverAccounts()
	if b.authFile != "" || b.pool.Ticket.State != "" || r.accounts["account:c"].authFile == "" {
		t.Fatal("credential reassignment retained old binding")
	}
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"m"}`))
	q.Header.Set("Chatgpt-Account-Id", "b")
	b.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		if q.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("unbound account injected")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	r.handler().ServeHTTP(httptest.NewRecorder(), q)
}
func TestConcurrentAccountsKeepTicketCookieSessionAndObservationSeparate(t *testing.T) {
	r, b := accountFixture(t)
	accounts := []*relay{r, b}
	replies := map[string]string{"account:a": "reply-a", "account:b": "reply-b-longer"}
	for _, a := range accounts {
		a.settings.TicketEnabled = true
		a.settings.TicketWrite = true
		a.pool.Account = a.account.ID
		a.pool.Ticket = ticket{State: "ticket-" + a.account.ID, Cookie: "__cflb=" + a.account.ID, GotAt: time.Now()}
		a.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
			id := accountIdentity(q)
			body, _ := io.ReadAll(q.Body)
			if q.Header.Get("X-Codex-Turn-State") != "ticket-"+id || q.Header.Get("Cookie") != "client=keep; __cflb="+id || string(body) != `{"model":"same-model"}` {
				t.Error("cross-account transfer or request mutation")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Codex-Turn-State": []string{replies[id]}}, Body: io.NopCloser(strings.NewReader(id))}, nil
		})
	}
	var wg sync.WaitGroup
	for _, a := range accounts {
		a := a
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"same-model"}`))
			q.Header.Set("Chatgpt-Account-Id", strings.TrimPrefix(a.account.ID, "account:"))
			q.Header.Set("Session-Id", "same-session")
			q.Header.Set("Cookie", "client=keep")
			q.Header.Set("X-Codex-Turn-State", "client-ticket")
			w := httptest.NewRecorder()
			r.handler().ServeHTTP(w, q)
			if w.Body.String() != a.account.ID {
				t.Error("wrong response")
			}
		}()
	}
	wg.Wait()
	for _, a := range accounts {
		s := a.singleStatus()
		v := s.Sessions[0]
		if v.AccountID != a.account.ID || v.Transfer.InputLength != len("client-ticket") || v.Transfer.SentLength != len(a.pool.Ticket.State) || v.Transfer.ResponseLength != len(replies[a.account.ID]) || !v.Transfer.Injected || s.Models[0].ObservedLength != len(replies[a.account.ID]) {
			t.Fatal("wrong attribution")
		}
	}
	if len(r.status().Sessions) != len(accounts) {
		t.Fatal("same session ID collapsed across accounts")
	}
	before := b.pool.Ticket
	if err := r.setEnabled(false); err != nil {
		t.Fatal(err)
	}
	if b.pool.Ticket != before || !b.settings.TicketEnabled {
		t.Fatal("toggle affected another account")
	}
}
func TestAccountAPISettingsPersistAndDoNotLeakCredentials(t *testing.T) {
	r, b := accountFixture(t)
	q := httptest.NewRequest("POST", "/api/toggle?account=account:b", strings.NewReader(`{"ticket_enabled":true,"ticket_write":true}`))
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, q)
	if w.Code != http.StatusOK || r.settings.TicketEnabled || !b.settings.TicketEnabled {
		t.Fatal("toggle target incorrect")
	}
	reloaded := testRelay(t)
	reloaded.settingsFile = r.settingsFile
	reloaded.accountHome = r.accountHome
	reloaded.discoverAccounts()
	if !reloaded.accounts[b.account.ID].settings.TicketWrite {
		t.Fatal("account preferences lost")
	}
	raw := w.Body.String()
	for _, secret := range []string{"fixture-refresh", "e30.", "__cflb="} {
		if strings.Contains(raw, secret) {
			t.Fatal("secret exposed")
		}
	}
	token := fixtureAuth(t, r.accountHome, "accounts/extra/auth.json", "other")
	mismatch := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"m"}`))
	mismatch.Header.Set("Authorization", "Bearer "+token)
	mismatch.Header.Set("Chatgpt-Account-Id", "b")
	denied := httptest.NewRecorder()
	r.handler().ServeHTTP(denied, mismatch)
	if denied.Code != http.StatusServiceUnavailable {
		t.Fatal("ambiguous identity accepted")
	}
}
func TestAccountSchedulerSerializesAndContinuesAfterFailure(t *testing.T) {
	r, b := accountFixture(t)
	var active atomic.Int32
	started := make(chan *relay, 4)
	release := make(chan struct{})
	for _, a := range []*relay{r, b} {
		a := a
		a.settings.TicketEnabled = true
		a.launchCLI = func(ctx context.Context, model, key string) error {
			if !active.CompareAndSwap(0, 1) {
				t.Error("parallel harvest")
			}
			defer active.Store(0)
			started <- a
			select {
			case <-release:
				return context.DeadlineExceeded
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	r.scheduleAccounts()
	select {
	case first := <-started:
		if first != r {
			t.Fatal("initial ordering")
		}
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	b.startHarvest(defaultModel, true)
	select {
	case <-started:
		t.Fatal("second concurrent task")
	default:
	}
	release <- struct{}{}
	r.jobs.Wait()
	r.scheduleAccounts()
	select {
	case next := <-started:
		if next != b {
			t.Fatal("failed account starved next")
		}
	case <-time.After(time.Second):
		t.Fatal("next not started")
	}
	release <- struct{}{}
	b.jobs.Wait()
}
func TestStaleResponseCannotReplaceLatestTransfer(t *testing.T) {
	r, _ := accountFixture(t)
	q := httptest.NewRequest("POST", "/responses", nil)
	old := r.recordDispatch("", 0, q, ticket{State: "old"})
	current := r.recordDispatch("", 0, q, ticket{State: "new-ticket"})
	r.recordResponse("", 0, current, http.StatusOK, len("new-reply"), "")
	before := r.lastTransfer
	r.recordResponse("", 0, old, http.StatusBadGateway, 0, "old failure")
	if r.lastTransfer != before {
		t.Fatal("stale result overwrote latest")
	}
}

func TestAuxiliaryRequestsDoNotEraseLatestTurn(t *testing.T) {
	r, _ := accountFixture(t)
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"m"}`))
	q.Header.Set("Chatgpt-Account-Id", "a")
	q.Header.Set("X-Codex-Turn-State", "turn-ticket")
	r.handler().ServeHTTP(httptest.NewRecorder(), q)
	before := r.lastTransfer
	q = httptest.NewRequest("GET", "/backend-api/wham/usage", nil)
	q.Header.Set("Chatgpt-Account-Id", "a")
	r.handler().ServeHTTP(httptest.NewRecorder(), q)
	if r.lastTransfer != before {
		t.Fatal("auxiliary request erased turn observation")
	}
}
