package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// External user contract: testdata/ticket_contract.json, approved 2026-09-23.
func TestTicketUserContract(t *testing.T) {
	var spec struct {
		Cases []struct {
			Length, HTTP     int
			Prefix           string
			Accepted, Passed bool
		}
	}
	b, err := os.ReadFile("testdata/ticket_contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	for _, tc := range spec.Cases {
		t.Run(fmt.Sprintf("%d_%d_%s", tc.Length, tc.HTTP, tc.Prefix), func(t *testing.T) {
			r := testRelay(t)
			r.settings.TicketEnabled = true // Acquisition works with injection OFF.
			state := tc.Prefix + strings.Repeat("x", tc.Length-len(tc.Prefix))
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
				if q.Header.Get("X-Codex-Turn-State") != "" {
					t.Error("harvest injected a ticket")
				}
				w.Header().Set("X-Codex-Turn-State", state)
				w.Header().Set("Set-Cookie", "__cflb=paired")
				w.WriteHeader(tc.HTTP)
			}))
			defer u.Close()
			r.upstream = u.URL
			a := &harvestAttempt{Model: "m", Cancel: func() {}, Result: make(chan error, 1)}
			r.attempts["fixture"] = a
			r.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/_harvest/fixture/responses", strings.NewReader(`{"model":"m"}`)))
			if got := <-a.Result; (got == nil) != tc.Accepted {
				t.Fatalf("acceptance mismatch: %v", got)
			}
			if (r.pool.Ticket.State != "") != tc.Accepted {
				t.Fatal("pool disagrees with user contract")
			}
			if tc.Accepted && (r.pool.Ticket.State != state || r.pool.Ticket.Cookie != "__cflb=paired") {
				t.Fatal("ticket pair changed")
			}
			q := sessionRequest("observed", "m")
			r.handler().ServeHTTP(httptest.NewRecorder(), q)
			if (r.status().Sessions[0].Status == sessionPassed) != tc.Passed {
				t.Fatal("session classification disagrees with user contract")
			}
		})
	}
}

func TestWriteDefaultAndPersistenceThroughAPI(t *testing.T) {
	r := testRelay(t)
	r.loadSettings()
	if r.status().TicketWrite {
		t.Fatal("new configuration writes tickets")
	}
	if err := os.WriteFile(r.settingsPath(), []byte(`{"ticket_enabled":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	r.loadSettings()
	if r.status().TicketWrite || !r.status().Enabled {
		t.Fatal("legacy config does not default OFF")
	}
	for _, enabled := range []bool{true, false} {
		w := httptest.NewRecorder()
		r.handler().ServeHTTP(w, httptest.NewRequest("POST", "/api/toggle", strings.NewReader(fmt.Sprintf(`{"ticket_write":%t}`, enabled))))
		var status statusView
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.TicketWrite != enabled || !status.Enabled {
			t.Fatal("toggle response inconsistent")
		}
		loaded := testRelay(t)
		loaded.settingsFile = r.settingsFile
		loaded.loadSettings()
		if loaded.status().TicketWrite != enabled || !loaded.status().Enabled {
			t.Fatal("toggle not persisted independently")
		}
	}
}

func TestWriteGateCombinations(t *testing.T) {
	for _, maintain := range []bool{false, true} {
		for _, write := range []bool{false, true} {
			for _, client := range []string{"", "client-ticket"} {
				t.Run(fmt.Sprintf("%t_%t_%s", maintain, write, client), func(t *testing.T) {
					r := testRelay(t)
					r.settings.TicketEnabled, r.settings.TicketWrite = maintain, write
					original := ticket{"cached-ticket", "__cflb=cached", time.Now()}
					r.pool.Ticket = original
					seen := false
					r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
						seen = true
						wantState, wantCookie := client, "__cflb=client; other=keep"
						if maintain && write {
							wantState, wantCookie = original.State, "other=keep; __cflb=cached"
						}
						if q.Header.Get("X-Codex-Turn-State") != wantState || q.Header.Get("Cookie") != wantCookie {
							t.Error("write gate changed wrong headers")
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unchanged"))}, nil
					})
					q := sessionRequest("s", "m")
					q.Header.Set("X-Codex-Turn-State", client)
					q.Header.Set("Cookie", "__cflb=client; other=keep")
					w := httptest.NewRecorder()
					r.handler().ServeHTTP(w, q)
					if !seen || w.Body.String() != "unchanged" {
						t.Fatal("forwarding failed")
					}
					if (r.pool.Reused > 0) != (maintain && write) {
						t.Fatal("incorrect reuse count")
					}
				})
			}
		}
	}
}

func TestWriteOffDoesNotWaitOrHarvest(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.pool.Account = "unrelated-account"
	r.pool.Ticket = ticket{"expired", "paired", time.Now().Add(-stateTTL)}
	r.launchCLI = func(context.Context, string, string) error { t.Error("write OFF started demand harvest"); return nil }
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		if q.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("expired ticket injected")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	r.handler().ServeHTTP(w, sessionRequest("s", "m").WithContext(ctx))
	if w.Body.String() != "ok" || r.status().Waiting != 0 || r.pool.Attempts != 0 {
		t.Fatal("write OFF blocked on unavailable ticket")
	}
}

func TestWriteOffReleasesWaiterPreservesHarvest(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled, r.settings.TicketWrite = true, true
	started := make(chan struct{})
	r.launchCLI = func(ctx context.Context, _, _ string) error { close(started); <-ctx.Done(); return ctx.Err() }
	done := make(chan error, 1)
	go func() { _, err := r.waitTicket(context.Background(), "m"); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("harvest did not start")
	}
	if err := r.setTicketWrite(false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter not released")
	}
	if !r.status().Enabled || !r.status().Shared.Running {
		t.Fatal("write toggle cancelled acquisition")
	}
}
