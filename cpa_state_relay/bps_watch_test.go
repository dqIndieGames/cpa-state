package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestBPSWatchHTTPSourceCloses(t *testing.T) {
	disconnected := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.in_progress\"}\n\n")
		w.(http.Flusher).Flush()
		<-q.Context().Done()
		close(disconnected)
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	r := testRelay(t)
	stream := newBPSStreamTimeouts(response.Body, &bpsExchange{relay: r, Choice: "auto"}, 100*time.Millisecond, 2*time.Second)
	defer stream.Close()
	raw, err := io.ReadAll(stream)
	if err != nil || !strings.Contains(string(raw), "idle timeout") {
		t.Fatalf("socket stall not detected: %v %s", err, raw)
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("timeout left upstream socket active")
	}
	t.Log("real HTTP upstream observed connection cancellation after idle timeout")
}

// L1 invariants: incomplete calls never execute, quiet or heartbeat-only streams
// terminate, active content can outlive the deadlines, and cancellation closes IO.
func TestBPSWatchStalledStreams(t *testing.T) {
	for _, scenario := range []string{"idle", "heartbeat", "pending-tool", "partial-event"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := testRelay(t)
				reader, writer := io.Pipe()
				defer writer.Close()
				idle, progress := 3*time.Second, 7*time.Second
				stream := newBPSStreamTimeouts(reader, &bpsExchange{relay: r, Choice: "auto"}, idle, progress)
				defer stream.Close()
				producerDone := make(chan struct{})
				if scenario != "idle" {
					go func() {
						defer close(producerDone)
						if scenario == "pending-tool" {
							raw, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "pending"}})
							if _, err := io.WriteString(writer, "data: "+string(raw)+"\n\n"); err != nil {
								return
							}
						}
						for {
							time.Sleep(time.Second)
							frame := "data: {\"type\":\"response.in_progress\"}\n\n"
							if scenario == "partial-event" {
								frame = " "
							}
							if _, err := io.WriteString(writer, frame); err != nil {
								return
							}
						}
					}()
				} else {
					close(producerDone)
				}
				start := time.Now()
				raw, err := io.ReadAll(stream)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(raw), "timeout") {
					t.Fatalf("stall did not report timeout: %s", raw)
				}
				deadline := progress
				if scenario == "idle" {
					deadline = idle
				}
				if time.Since(start) != deadline {
					t.Fatalf("wrong stall boundary: elapsed=%s deadline=%s", time.Since(start), deadline)
				}
				for _, event := range streamEvents(t, string(raw)) {
					if event["type"] == "response.output_item.done" || event["type"] == "response.completed" {
						t.Fatal("unfinished response released output")
					}
				}
				<-producerDone
				synctest.Wait()
			})
		})
	}
}

func TestBPSWatchSlowContentAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		idle, progress := 2*time.Second, 3*time.Second
		stream := newBPSStreamTimeouts(reader, &bpsExchange{Choice: "auto"}, idle, progress)
		go func() {
			for elapsed := time.Duration(0); elapsed < progress*3; elapsed += time.Second {
				time.Sleep(time.Second)
				_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"x\"}\n\n")
			}
			_, _ = io.WriteString(writer, completeStream(map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Complete"}}}))
		}()
		start := time.Now()
		raw, err := io.ReadAll(stream)
		if err != nil || !strings.Contains(string(raw), "response.completed") || strings.Contains(string(raw), "timeout") || time.Since(start) <= progress {
			t.Fatalf("slow valid response failed: %s %v", raw, err)
		}
		if _, err = writer.Write([]byte("after completion")); err == nil {
			t.Fatal("completed response left upstream open")
		}
		_ = stream.Close()
	})
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		r := testRelay(t)
		x := &bpsExchange{relay: r, RequestKey: "cancelled"}
		stream := newBPSStream(reader, x)
		done := make(chan []byte, 1)
		go func() { raw, _ := io.ReadAll(stream); done <- raw }()
		synctest.Wait()
		_ = stream.Close()
		raw := <-done
		if len(raw) != 0 {
			t.Fatal("client cancellation emitted retry failure")
		}
	})
}

func TestBPSStreamRetriesRemainClientOwned(t *testing.T) {
	r := testRelay(t)
	src := bridgeSource()
	for attempt := 0; attempt < 5; attempt++ {
		q, err := bridgeRequest(t, r, "retry", src)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader("")), exchange(q)))
		events := streamEvents(t, string(raw))
		last := events[len(events)-1]
		if last["type"] != "response.failed" {
			t.Fatal("transport failure not retryable")
		}
		for _, e := range events {
			item, _ := e["item"].(map[string]any)
			if isToolItem(item) || e["type"] == "response.completed" {
				t.Fatal("retry delivered a tool")
			}
		}
	}
	if _, err := bridgeRequest(t, r, "retry", src); err != nil {
		t.Fatalf("client retry blocked: %v", err)
	}
	if _, err := bridgeRequest(t, r, "other-session", src); err != nil {
		t.Fatal("retry budget crossed sessions")
	}
	src["input"] = append(src["input"].([]any), map[string]any{"role": "user", "content": "Retry in a new turn"})
	if _, err := bridgeRequest(t, r, "retry", src); err != nil {
		t.Fatal("new turn remained blocked")
	}
}
