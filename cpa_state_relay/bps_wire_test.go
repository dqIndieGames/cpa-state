package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func thinNativeCall(name string, payload map[string]any) map[string]any {
	code, custom := payload["input"].(string)
	if !custom {
		raw, _ := json.Marshal(payload["arguments"])
		code = string(raw)
	}
	args, _ := json.Marshal(map[string]any{"summary": "Call the declared client tool", "extended_summary": "Deliver the original client payload with its declared tool identity.", "destructive": false, "references": []any{"codex:" + name}, "code": code})
	return map[string]any{"type": "function_call", "id": "fc_thin", "call_id": "call_thin", "name": "run_officejs", "arguments": string(args), "status": "completed"}
}

// External truth is the actual BPS response.created schema in testdata.
// The negative sample reproduces the old empty-reference instruction conflict.
func TestBPSThinWrapperMatchesLiveSchema(t *testing.T) {
	raw, err := os.ReadFile("testdata/bps-run-officejs-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("urn:bps:live-wrapper", fixture["tool"].(map[string]any)["parameters"]); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("urn:bps:live-wrapper")
	if err != nil {
		t.Fatal(err)
	}
	call := thinNativeCall("functions.apply_patch", map[string]any{"input": "original payload"})
	var args map[string]any
	_ = json.Unmarshal([]byte(str(call, "arguments")), &args)
	if err = schema.Validate(args); err != nil {
		t.Fatal(err)
	}
	args["references"] = []any{}
	if schema.Validate(args) == nil {
		t.Fatal("captured upstream schema unexpectedly accepts empty references")
	}
}

// L1: independently supplied input bytes survive the complete transport/history
// path. In particular, JSON-looking custom input is still raw custom input.
func TestBPSThinPayloadFidelityAndHistory(t *testing.T) {
	slash := string(rune(92))
	nl := string(rune(10))
	quote := string(rune(34))
	samples := []string{"*** Begin Patch" + nl + "*** End Patch" + nl, "const r = /" + slash + "d+" + slash + "s/;" + nl + "text(" + quote + "C:" + slash + "temp" + slash + "new" + quote + ");", "quotes: '" + quote + " café Ω 🧪" + nl, fmt.Sprintf("{%q:%q,%q:%q}", "name", "do_not_dispatch", "input", "still raw")}
	for _, input := range samples {
		t.Run(fmt.Sprintf("bytes-%d", len(input)), func(t *testing.T) {
			r := testRelay(t)
			src := bridgeSource()
			q, err := bridgeRequest(t, r, "thin-history", src)
			if err != nil {
				t.Fatal(err)
			}
			native := thinNativeCall("functions.apply_patch", map[string]any{"input": input})
			raw, err := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(native))), exchange(q)))
			if err != nil {
				t.Fatal(err)
			}
			var client map[string]any
			for _, e := range streamEvents(t, string(raw)) {
				if e["type"] == "response.output_item.done" {
					client = e["item"].(map[string]any)
				}
			}
			if client == nil || client["input"] != input || client["call_id"] != native["call_id"] {
				t.Fatal("custom payload or identity changed", string(raw))
			}
			output := map[string]any{"type": "custom_tool_call_output", "call_id": client["call_id"], "output": "independently supplied result"}
			src["input"] = []any{client, output}
			next, err := bridgeRequest(t, r, "thin-history", src)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			_ = json.NewDecoder(next.Body).Decode(&body)
			items := body["input"].([]any)
			restored := items[len(items)-2].(map[string]any)
			if !reflect.DeepEqual(restored, native) || items[len(items)-1].(map[string]any)["output"] != output["output"] {
				t.Fatal("history changed call or result")
			}
		})
	}
	r := testRelay(t)
	q, err := bridgeRequest(t, r, "thin-function", bridgeSource())
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"cmd": "Write-Output 'exact'"}
	client, err := exchange(q).convertTool(thinNativeCall("functions.exec_command", map[string]any{"arguments": args}))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	_ = json.Unmarshal([]byte(str(client, "arguments")), &decoded)
	if !reflect.DeepEqual(decoded, args) {
		t.Fatal("function arguments changed")
	}
}

func TestBPSThinInvalidBatchNeverDelivers(t *testing.T) {
	for _, scenario := range []string{"unknown", "ambiguous", "schema", "nontext", "choice"} {
		t.Run(scenario, func(t *testing.T) {
			r := testRelay(t)
			src := bridgeSource()
			if scenario == "choice" {
				src["tool_choice"] = "none"
			}
			q, err := bridgeRequest(t, r, "bad-thin", src)
			if err != nil {
				t.Fatal(err)
			}
			valid := thinNativeCall("functions.apply_patch", map[string]any{"input": "must not execute"})
			bad := thinNativeCall("functions.exec_command", map[string]any{"arguments": map[string]any{"cmd": true}})
			bad["id"], bad["call_id"] = "fc_bad", "call_bad"
			var args map[string]any
			_ = json.Unmarshal([]byte(str(bad, "arguments")), &args)
			switch scenario {
			case "unknown":
				args["references"] = []any{"codex:undeclared"}
			case "ambiguous":
				args["references"] = []any{"codex:functions.exec_command", "codex:functions.apply_patch"}
			case "nontext":
				args["code"] = true
			}
			encoded, _ := json.Marshal(args)
			bad["arguments"] = string(encoded)
			raw, _ := io.ReadAll(newBPSStream(io.NopCloser(strings.NewReader(completeStream(valid, bad))), exchange(q)))
			events := streamEvents(t, string(raw))
			if events[len(events)-1]["type"] != "response.failed" {
				t.Fatal("invalid batch passed", string(raw))
			}
			for _, e := range events {
				item, _ := e["item"].(map[string]any)
				if isToolItem(item) {
					t.Fatal("invalid response leaked executable tool")
				}
			}
			if _, ok := r.bpsCache.get(exchange(q).Scope + str(valid, "call_id")); ok {
				t.Fatal("partial batch committed")
			}
		})
	}
}

// L1: formatting noise cannot extend the existing progress deadline; identical
// spaces inside a real string can. Virtual time avoids waiting minutes.
func TestBPSArgumentWhitespaceProgress(t *testing.T) {
	for _, inside := range []bool{false, true} {
		t.Run(fmt.Sprintf("inside-%v", inside), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := testRelay(t)
				q, err := bridgeRequest(t, r, "whitespace", bridgeSource())
				if err != nil {
					t.Fatal(err)
				}
				reader, writer := io.Pipe()
				defer writer.Close()
				idle, progress := 3*time.Second, 7*time.Second
				stream := newBPSStreamTimeouts(reader, exchange(q), idle, progress)
				defer stream.Close()
				send := func(event map[string]any) error {
					raw, _ := json.Marshal(event)
					_, err := io.WriteString(writer, "data: "+string(raw)+strings.Repeat(string(rune(10)), 2))
					return err
				}
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					prefix := fmt.Sprintf("{%q:[", "references")
					if inside {
						prefix = fmt.Sprintf("{%q:%c", "code", rune(34))
					}
					item := map[string]any{"type": "function_call", "id": "fc_thin", "name": "run_officejs", "call_id": "call_thin", "arguments": prefix}
					if send(map[string]any{"type": "response.output_item.added", "item": item}) != nil {
						return
					}
					for i := 0; i < 10; i++ {
						time.Sleep(time.Second)
						if send(map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_thin", "delta": " "}) != nil {
							return
						}
					}
					_, _ = io.WriteString(writer, completeStream(thinNativeCall("functions.apply_patch", map[string]any{"input": strings.Repeat(" ", 10)})))
				}()
				start := time.Now()
				raw, err := io.ReadAll(stream)
				if err != nil {
					t.Fatal(err)
				}
				events := streamEvents(t, string(raw))
				last := events[len(events)-1]
				if inside {
					if last["type"] != "response.completed" || time.Since(start) <= progress {
						t.Fatal("legitimate string payload timed out", string(raw))
					}
				} else {
					if last["type"] != "response.failed" || time.Since(start) != progress {
						t.Fatal("formatting whitespace renewed deadline", string(raw))
					}
					for _, e := range events {
						item, _ := e["item"].(map[string]any)
						if isToolItem(item) {
							t.Fatal("unfinished tool executed")
						}
					}
				}
				<-finished
			})
		})
	}
}
