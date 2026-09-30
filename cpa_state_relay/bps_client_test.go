package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// User contract: Codex owns retry policy. External truth is the actual installed
// client running against an isolated relay and local fault-injecting upstream.
// No model calls, production settings or real credentials are involved.
func TestBPSRealClientRetryRecovery(t *testing.T) {
	for _, failure := range []string{"format", "disconnect", "http"} {
		t.Run(failure, func(t *testing.T) { verifyBPSClientRetries(t, failure, false) })
	}
}

func TestBPSRealClientRetryExhaustion(t *testing.T) {
	for _, failure := range []string{"format", "disconnect"} {
		t.Run(failure, func(t *testing.T) { verifyBPSClientRetries(t, failure, true) })
	}
}

func TestBPSThinRealClientRecovery(t *testing.T) {
	verifyBPSClientRetries(t, "thin", false)
}

func TestBPSRealClientNestedScriptCompatibility(t *testing.T) {
	verifyBPSClientRetries(t, "nested", false)
}

func verifyBPSClientRetries(t *testing.T, failure string, exhaust bool) {
	t.Helper()
	thin := failure == "thin"
	if thin {
		failure = "format"
	}
	cli := os.Getenv("BPS_VERIFY_CLIENT")
	if cli == "" {
		t.Skip("set BPS_VERIFY_CLIENT to the installed Codex executable")
	}
	r := testRelay(t)
	r.settings.BPS = true
	token := fixtureAuth(t, t.TempDir(), "auth.json", "second")
	injectedFailures := int32(3) // Must recover beyond the old relay cutoff.
	if failure == "nested" {
		injectedFailures = 0
	}
	runsTool := failure == "format" || failure == "nested"
	const clientRetries = 4
	var upstream, requests atomic.Int32
	var toolResults atomic.Int32
	r.transport = roundTripFunc(func(q *http.Request) (*http.Response, error) {
		attempt := upstream.Add(1)
		request, _ := io.ReadAll(q.Body)
		if failure == "format" && attempt > 1 && (exhaust || attempt <= injectedFailures+1) && !strings.Contains(string(request), "previous response failed") {
			t.Error("client retry lost correction feedback")
		}
		if !exhaust && attempt > injectedFailures {
			if runsTool && attempt == injectedFailures+1 {
				x := exchange(q)
				for name, tool := range x.Tools {
					if failure == "nested" && tool.Name != "exec" {
						continue
					}
					var payload map[string]any
					switch tool.Name {
					case "exec":
						if tool.Kind == "custom" {
							payload = map[string]any{"input": `text("BPS_TOOL_EXECUTED");`}
						}
					case "exec_command":
						payload = map[string]any{"arguments": map[string]any{"cmd": "Write-Output BPS_TOOL_EXECUTED", "shell": "pwsh.exe"}}
					case "shell_command":
						payload = map[string]any{"arguments": map[string]any{"command": "Write-Output BPS_TOOL_EXECUTED"}}
					}
					if payload != nil {
						if failure == "nested" {
							script := "const data = {\"name\":\"BPS_TOOL_EXECUTED\",\"items\":[{\"k\":\"v\"}]};\ntext(data.name);"
							native := nativeCall(name, payload)
							encodedName, _ := json.Marshal(name)
							args, _ := json.Marshal(map[string]any{"code": `{"name":` + string(encodedName) + `,"input":"` + script + `"}`})
							native["arguments"] = string(args)
							return fixtureSSE(completeStream(native)), nil
						}
						native := nativeCall(name, payload)
						if thin {
							native = thinNativeCall(name, payload)
						}
						return fixtureSSE(completeStream(native)), nil
					}
				}
				t.Error("fixture client exposes no supported read-only execution tool")
			}
			if runsTool {
				var body map[string]any
				_ = json.Unmarshal(request, &body)
				for _, value := range body["input"].([]any) {
					item, _ := value.(map[string]any)
					if str(item, "type") == "function_call_output" {
						output, _ := json.Marshal(item["output"])
						if strings.Contains(string(output), "BPS_TOOL_EXECUTED") {
							toolResults.Add(1)
						} else {
							t.Logf("fixture tool output: %s", output)
						}
					}
				}
			}
			item := map[string]any{"id": "msg_recovered", "type": "message", "role": "assistant", "phase": "final_answer", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "RECOVERED_AFTER_RETRIES", "annotations": []any{}}}}
			done, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			return fixtureSSE("data: " + string(done) + "\n\n" + completeStream(item)), nil
		}
		switch failure {
		case "disconnect":
			return fixtureSSE("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"pending\",\"name\":\"MUST_NOT_EXECUTE\"}}\n\n"), nil
		case "http":
			return &http.Response{StatusCode: http.StatusBadGateway, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"fixture upstream unavailable"}}`))}, nil
		default:
			native := nativeCall("unused", nil)
			args, _ := json.Marshal(map[string]any{"code": `{"name":"functions.exec","input":"TRUNCATED_MUST_NOT_EXECUTE`})
			native["arguments"] = string(args)
			return fixtureSSE(completeStream(native)), nil
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		requests.Add(1)
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Chatgpt-Account-Id", "second")
		r.handler().ServeHTTP(w, q)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--json", "--color", "never", "-m", "gpt-6-astra", "-c", "model_provider=\"bps_fixture\"", "-c", "model_providers.bps_fixture.name=\"BPS fixture\"", "-c", fmt.Sprintf("model_providers.bps_fixture.base_url=%q", server.URL+"/v1"), "-c", "model_providers.bps_fixture.wire_api=\"responses\"", "-c", "model_providers.bps_fixture.env_key=\"BPS_FIXTURE_KEY\"", "-c", fmt.Sprintf("model_providers.bps_fixture.stream_max_retries=%d", clientRetries), "-c", "model_providers.bps_fixture.request_max_retries=0", "-c", "model_providers.bps_fixture.supports_websockets=false", "-c", "features.enable_request_compression=false", "Use a tool only if the server returns a valid tool call.")
	cmd.Dir = t.TempDir()
	cmd.Args = append(cmd.Args, "-c", "service_tier=\"default\"")
	cmd.Args = append(cmd.Args, "-c", "force_service_tier_priority=false", "-c", "features.unbounded_connection_retries=false")
	if failure == "nested" {
		cmd.Args = append(cmd.Args, "-c", "features.code_mode=true")
	}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "CODEX_HOME=") && !strings.HasPrefix(strings.ToUpper(value), "CODEX_INTERNAL_RETRY_MODE=") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+t.TempDir(), "BPS_FIXTURE_KEY=local-test-key")
	if exhaust {
		cmd.Env = append(cmd.Env, "CODEX_INTERNAL_RETRY_MODE=bounded")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := runInJob(cmd)
	if ctx.Err() != nil {
		t.Fatalf("client timed out: %s %s", stdout.String(), stderr.String())
	}
	if requests.Load() != upstream.Load() {
		t.Fatal("relay blocked client retries before upstream")
	}
	if ((exhaust || !runsTool) && strings.Contains(stdout.String(), "command_execution")) || strings.Contains(stdout.String(), "msg_cpa_bps_stopped") {
		t.Fatal("failed response executed a tool or synthesized completion")
	}
	if exhaust {
		if err == nil || upstream.Load() != clientRetries+1 || strings.Contains(stdout.String(), `"type":"turn.completed"`) {
			t.Fatalf("client budget not respected: attempts=%d err=%v %s %s", upstream.Load(), err, stdout.String(), stderr.String())
		}
	} else {
		wantRequests := int32(injectedFailures + 1)
		if runsTool {
			wantRequests++
			if toolResults.Load() != 1 {
				t.Errorf("successful tool result not received exactly once: %d", toolResults.Load())
			}
		}
		if err != nil || upstream.Load() != wantRequests || !strings.Contains(stdout.String(), "RECOVERED_AFTER_RETRIES") {
			t.Fatalf("retry did not recover: attempts=%d err=%v %s %s", upstream.Load(), err, stdout.String(), stderr.String())
		}
	}
	if r.singleStatus().Active != 0 {
		t.Fatal("retry leaked a connection")
	}
	t.Logf("DEV-assisted installed client: scenario=%s exhausted=%v upstream=%d; failed tools never executed", failure, exhaust, upstream.Load())
}

func fixtureSSE(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
}
