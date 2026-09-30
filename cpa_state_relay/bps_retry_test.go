package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type failedBPSReader struct{}

func (failedBPSReader) Read([]byte) (int, error) { return 0, errors.New("connection interrupted") }
func (failedBPSReader) Close() error             { return nil }

// Codex codex-api/src/sse/responses.rs maps invalid_prompt to InvalidRequest;
// other response.failed codes (including server_error) map to Retryable.
// A broken transport must not suppress that retry or deliver an incomplete call.
func TestBPSStreamFailureRetryContract(t *testing.T) {
	for _, scenario := range []string{"eof", "read-error", "pending-tool", "partial-json", "malformed-json", "oversize", "oversize-after-line"} {
		t.Run(scenario, func(t *testing.T) {
			var source io.ReadCloser = io.NopCloser(strings.NewReader(""))
			switch scenario {
			case "read-error":
				source = failedBPSReader{}
			case "pending-tool":
				source = io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"pending\",\"name\":\"run_officejs\"}}\n\n"))
			case "partial-json":
				source = io.NopCloser(strings.NewReader("data: {\"type\":\"response."))
			case "malformed-json":
				source = io.NopCloser(strings.NewReader("data: not-json\n\n"))
			case "oversize":
				source = io.NopCloser(strings.NewReader("data: " + strings.Repeat("x", 4<<20)))
			case "oversize-after-line":
				source = io.NopCloser(strings.NewReader("data: {}\ndata: " + strings.Repeat("x", 4<<20)))
			}
			raw, err := io.ReadAll(newBPSStream(source, &bpsExchange{}))
			if err != nil {
				t.Fatal(err)
			}
			events := streamEvents(t, string(raw))
			last := events[len(events)-1]
			if last["type"] != "response.failed" {
				t.Fatal("lost failure event", last)
			}
			failure := last["response"].(map[string]any)["error"].(map[string]any)
			if failure["code"] != "server_error" {
				t.Fatal("wrong Codex retry classification", failure)
			}
			for _, event := range events {
				if event["type"] == "response.output_item.done" || event["type"] == "response.completed" {
					t.Fatal("failed stream delivered a tool or success", event)
				}
			}
		})
	}
}
