package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// L1 invariants: original request/response preservation, isolation, and cancellation.
// TTL contract source: user explicitly required a 230-second hard ceiling.
func testRelay(t *testing.T) *relay {
	t.Helper()
	r := newRelay()
	// Local fixtures must not depend on the user's live Clash rules.
	direct := newTransport(false)
	direct.Proxy = nil
	r.transport = direct
	r.harvestTransport = func(independent bool) *http.Transport { t := newTransport(independent); t.Proxy = nil; return t }
	r.settingsFile = filepath.Join(t.TempDir(), "settings.json")
	t.Cleanup(r.stop)
	return r
}
func TestMissingPreferenceDefaultsToTransparentVerge(t *testing.T) {
	r := testRelay(t)
	r.loadSettings()
	if r.status().Independent || r.status().Enabled {
		t.Fatal("fresh install must transparently follow Verge")
	}
}
func TestDisableRouteCancelsAndInvalidates(t *testing.T) {
	r := testRelay(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.settings.IndependentProxy = true
	r.attempts["own"] = &harvestAttempt{Cancel: cancel}
	r.pool.Ticket = ticket{State: "old", Cookie: "old", GotAt: time.Now()}
	if e := r.setIndependent(false); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("old probe not cancelled")
	}
	if r.pool.Ticket.State != "" || r.status().Independent {
		t.Fatal("old route ticket retained")
	}
	loaded := newRelay()
	defer loaded.stop()
	loaded.settingsFile = r.settingsFile
	loaded.loadSettings()
	if loaded.settings.IndependentProxy {
		t.Fatal("preference lost")
	}
}
func TestSettingsFailureReleasesLock(t *testing.T) {
	r := testRelay(t)
	block := filepath.Join(t.TempDir(), "block")
	if e := os.WriteFile(block, []byte("block"), 0600); e != nil {
		t.Fatal(e)
	}
	r.settingsFile = filepath.Join(block, "settings.json")
	if r.setIndependent(false) == nil {
		t.Fatal("save failure hidden")
	}
	r.settingsFile = filepath.Join(t.TempDir(), "settings.json")
	done := make(chan error, 1)
	go func() { done <- r.setIndependent(false) }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock leak")
	}
}
func TestTransparentPreservesRequestAndResponse(t *testing.T) {
	body := "{ \"model\": \"test-model\", \"input\": [\"你好\"], \"tools\": [] }\n"
	query := "a=1&a=2&escaped=%2f%2B&empty=&semi=x;y"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		if string(b) != body || q.URL.RawQuery != query || q.URL.Path != "/backend-api/codex/responses/compact" {
			t.Error("request payload/path/query changed")
		}
		if q.Header.Get("Authorization") != "Bearer test" || q.Header.Get("Cookie") != "other=keep; __cflb=client" || q.Header.Get("X-Codex-Turn-State") != "client-state" || q.Header.Get("X-Custom") != "keep" {
			t.Error("business headers changed")
		}
		w.Header().Add("Set-Cookie", "one=1")
		w.Header().Add("Set-Cookie", "two=2")
		w.Header().Set("X-Codex-Turn-State", strings.Repeat("x", 312))
		w.Header().Set("Trailer", "X-Final")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, "upstream-error-unchanged")
		w.Header().Set("X-Final", "done")
	}))
	defer upstream.Close()
	r := testRelay(t)
	r.upstream = upstream.URL
	s := httptest.NewServer(r.handler())
	defer s.Close()
	q, _ := http.NewRequest("POST", s.URL+"/backend-api/codex/responses/compact?"+query, strings.NewReader(body))
	q.Header.Set("Authorization", "Bearer test")
	q.Header.Set("Cookie", "other=keep; __cflb=client")
	q.Header.Set("X-Codex-Turn-State", "client-state")
	q.Header.Set("X-Custom", "keep")
	resp, e := http.DefaultClient.Do(q)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || string(b) != "upstream-error-unchanged" || len(resp.Header.Values("Set-Cookie")) != 2 || resp.Trailer.Get("X-Final") != "done" {
		t.Fatal("response changed")
	}
	if r.status().selected().ObservedLength != 312 {
		t.Fatal("passive response observation missing")
	}
}
func TestTicketLifetimeBoundary(t *testing.T) {
	if stateTTL != 230*time.Second {
		t.Fatal("user's 230-second contract violated")
	}
	now := time.Now()
	tick := ticket{State: "ticket", GotAt: now}
	if !tick.valid(now.Add(230*time.Second-time.Nanosecond)) || tick.valid(now.Add(230*time.Second)) {
		t.Fatal("hard cutoff incorrect")
	}
}
func TestInjectionPreservesOtherCookiesAndBody(t *testing.T) {
	body := `{"model":"model-a","input":"unchanged"}`
	seen := make(chan string, 1)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		if string(b) != body {
			t.Error("body rewritten")
		}
		if q.Header.Get("X-Codex-Turn-State") != "paired-state" {
			t.Error("ticket not injected")
		}
		seen <- q.Header.Get("Cookie")
		_, _ = io.WriteString(w, "ok")
	}))
	defer u.Close()
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	r.upstream = u.URL
	got := time.Now()
	r.pool.Ticket = ticket{"paired-state", "__cflb=new; __oailb=new2", got}
	q := httptest.NewRequest("POST", "/responses", strings.NewReader(body))
	q.Header.Set("Cookie", "other=keep; __cflb=old; third=value")
	r.handler().ServeHTTP(httptest.NewRecorder(), q)
	cookies := <-seen
	if cookies != "other=keep; third=value; __cflb=new; __oailb=new2" {
		t.Fatalf("unrelated cookies lost: %s", cookies)
	}
	if !r.pool.Ticket.GotAt.Equal(got) {
		t.Fatal("reuse extended TTL")
	}
}
func TestConcurrentStreamsAndIndependentCancellation(t *testing.T) {
	started := make(chan string, 16)
	firstReceived := make(chan struct{}, 1)
	release := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		id := q.Header.Get("X-Session")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s-first\n\n", id)
		w.(http.Flusher).Flush()
		started <- id
		select {
		case <-q.Context().Done():
			cancelled <- struct{}{}
		case <-release:
			_, _ = fmt.Fprintf(w, "data: %s-last\n\n", id)
		}
	}))
	defer u.Close()
	r := testRelay(t)
	r.upstream = u.URL
	s := httptest.NewServer(r.handler())
	defer s.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	cancels := make([]context.CancelFunc, 12)
	for i := 0; i < 12; i++ {
		ctx, c := context.WithCancel(context.Background())
		cancels[i] = c
		defer c()
		wg.Add(1)
		go func(i int, ctx context.Context) {
			defer wg.Done()
			id := fmt.Sprint(i)
			q, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/responses", strings.NewReader(`{"model":"m"}`))
			q.Header.Set("X-Session", id)
			resp, e := http.DefaultClient.Do(q)
			if e != nil {
				errs <- e
				return
			}
			defer resp.Body.Close()
			br := bufio.NewReader(resp.Body)
			first, e := br.ReadString('\n')
			if e != nil || first != "data: "+id+"-first\n" {
				errs <- fmt.Errorf("first chunk/mixed stream %d", i)
				return
			}
			if i == 0 {
				firstReceived <- struct{}{}
			}
			rest, e := io.ReadAll(br)
			if i != 0 && (e != nil || !strings.Contains(string(rest), id+"-last")) {
				errs <- fmt.Errorf("other stream interrupted %d", i)
			}
		}(i, ctx)
	}
	for i := 0; i < 12; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("streams serialized or buffered")
		}
	}
	select {
	case <-firstReceived:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel target did not receive first chunk")
	}
	cancels[0]()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel not propagated")
	}
	close(release)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
func TestToggleReleasesWaitersAndDeduplicates(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	started := make(chan struct{}, 4)
	r.launchCLI = func(ctx context.Context, model, key string) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := r.waitTicket(context.Background(), "m")
			if e != nil {
				t.Error(e)
			}
		}()
	}
	<-started
	r.mu.RLock()
	n := len(r.attempts)
	r.mu.RUnlock()
	if n != 1 {
		t.Fatal("duplicate harvest")
	}
	if e := r.setEnabled(false); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("disable did not release waiters")
	}
}
func TestOldResponseCannotInvalidateReplacement(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	old := ticket{"old", "", time.Now()}
	fresh := ticket{"new", "", time.Now()}
	r.pool.Ticket = old
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.mu.Lock()
		r.pool.Ticket = fresh
		r.mu.Unlock()
		w.Header().Set("X-Codex-Turn-State", strings.Repeat("x", 312))
	}))
	defer u.Close()
	r.upstream = u.URL
	r.handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/responses", strings.NewReader(`{"model":"m"}`)))
	if r.pool.Ticket.State != fresh.State {
		t.Fatal("old response invalidated newer ticket")
	}
}
func TestHarvestUsesSameGatewayAndStopsAfterHeaders(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	state := "gAAAAA" + strings.Repeat("x", 286)
	closed := make(chan struct{})
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("X-Codex-Turn-State") != "" {
			t.Error("harvest injected old ticket")
		}
		w.Header().Set("X-Codex-Turn-State", state)
		w.Header().Set("Set-Cookie", "__cflb=paired; Path=/")
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-q.Context().Done()
		close(closed)
	}))
	defer u.Close()
	r.upstream = u.URL
	s := httptest.NewServer(r.handler())
	defer s.Close()
	r.address = strings.TrimPrefix(s.URL, "http://")
	r.launchCLI = func(ctx context.Context, model, key string) error {
		q, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/_harvest/"+key+"/backend-api/codex/responses", strings.NewReader(`{"model":"m"}`))
		resp, e := http.DefaultClient.Do(q)
		if e == nil {
			resp.Body.Close()
		}
		return e
	}
	r.startHarvest("m", true)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream generation was not cancelled")
	}
	r.jobs.Wait()
	r.mu.RLock()
	m := r.pool
	r.mu.RUnlock()
	if m.Ticket.State != state || m.Ticket.Cookie != "__cflb=paired" || m.Running {
		t.Fatal("harvest result not retained or CLI still running")
	}
	if m.NextAttempt.Before(m.Ticket.GotAt.Add(30*time.Second)) || m.NextAttempt.After(time.Now().Add(60*time.Second)) {
		t.Fatal("refresh schedule out of range")
	}
	if r.models["other"] != nil {
		t.Fatal("model ticket leaked")
	}
}
func TestWaitingClientCancellation(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	r.launchCLI = func(ctx context.Context, model, key string) error { <-ctx.Done(); return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := r.waitTicket(ctx, "m"); e == nil {
		t.Fatal("cancelled waiter did not return")
	}
	if r.status().Waiting != 0 {
		t.Fatal("waiter leaked")
	}
}
func TestDisableDoesNotInterruptDispatchedStream(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	r.pool.Ticket = ticket{"existing", "", time.Now()}
	first := make(chan struct{})
	release := make(chan struct{})
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(first)
		<-release
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer u.Close()
	r.upstream = u.URL
	s := httptest.NewServer(r.handler())
	defer s.Close()
	result := make(chan string, 1)
	go func() {
		resp, e := http.Post(s.URL+"/responses", "application/json", strings.NewReader(`{"model":"m"}`))
		if e != nil {
			result <- e.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		result <- string(b)
	}()
	<-first
	if e := r.setEnabled(false); e != nil {
		t.Fatal(e)
	}
	close(release)
	select {
	case b := <-result:
		if b != "data: first\n\ndata: last\n\n" {
			t.Fatal("toggle interrupted original response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream hung")
	}
}

func TestWriteOffDoesNotInterruptDispatchedStream(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	r.settings.TicketWrite = true
	r.pool.Ticket = ticket{"existing", "", time.Now()}
	first := make(chan struct{})
	release := make(chan struct{})
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		if q.Header.Get("X-Codex-Turn-State") != "existing" {
			t.Error("stream was not dispatched with the ticket")
		}
		bodyReader, bodyWriter := io.Pipe()
		go func() {
			_, _ = io.WriteString(bodyWriter, "data: first\n\n")
			close(first)
			<-release
			_, _ = io.WriteString(bodyWriter, "data: last\n\n")
			_ = bodyWriter.Close()
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: bodyReader}, nil
	})
	server := httptest.NewServer(r.handler())
	defer server.Close()
	result := make(chan string, 1)
	go func() {
		q, _ := http.NewRequest("POST", server.URL+"/responses", strings.NewReader(`{"model":"m"}`))
		resp, err := http.DefaultClient.Do(q)
		if err != nil {
			result <- err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		result <- string(b)
	}()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not start")
	}
	if err := r.setTicketWrite(false); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case body := <-result:
		if body != "data: first\n\ndata: last\n\n" {
			t.Fatal("write toggle interrupted original stream")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream hung after write toggle")
	}
}

// User contract: all models share one acquisition task, including forced requests.
func TestDifferentModelsShareSingleHarvest(t *testing.T) {
	r := testRelay(t)
	r.settings.TicketEnabled = true
	started := make(chan string, 8)
	r.launchCLI = func(ctx context.Context, model, key string) error { started <- model; <-ctx.Done(); return ctx.Err() }
	r.startHarvest("model-a", true)
	r.startHarvest("model-b", true)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("harvest did not start")
	}
	r.mu.RLock()
	n := len(r.attempts)
	r.mu.RUnlock()
	if n != 1 {
		t.Fatal("parallel models created duplicate harvests")
	}
	select {
	case <-started:
		t.Fatal("second CLI launched")
	default:
	}
}

func BenchmarkTransparentForward(b *testing.B) {
	r := newRelay()
	defer r.stop()
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		_, _ = io.Copy(io.Discard, q.Body)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: result\n\n")), Request: q}, nil
	})
	body := `{"model":"benchmark","input":"` + strings.Repeat("x", 32*1024) + `"}`
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q := httptest.NewRequest("POST", "/backend-api/codex/responses", strings.NewReader(body))
			r.handleForward(httptest.NewRecorder(), q)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(q *http.Request) (*http.Response, error) { return f(q) }
