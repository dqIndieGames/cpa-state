package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Independently authored payloads: nested object punctuation, comments and
// quoted script bodies must survive transport repair without changing bytes.
func TestBPSNestedScriptBoundaryCompatibility(t *testing.T) {
	for _, script := range []string{
		`const x = {"name":"a", "input":{"k":"v"}}; text(x);`,
		"const x = {\"items\":[{\"k\":\"v\"}, {\"k\":\"w\"}]};\ntext(x);",
		"text(await tools.exec_command({cmd: `@'\npackage main\nvar x = map[string]string{\"name\":\"x\", \"input\":\"y\"}\n'@`, shell: \"pwsh.exe\"}));",
		"$code = @'\nvar x = map[string]string{\"a\":\"b\"}\nvar y = 'quote'\n'@\nWrite-Output $code",
		"// \"}, \"name\": \"comment\"\ntext({\"a\":\"b\"});",
		`const x = /["}]/; text({"a":"b"});`,
		`const x = "}"; text(x);`,
	} {
		// Missing escapes for script quotes are the observed transport defect.
		encoded, _ := json.Marshal(script)
		payload := strings.ReplaceAll(string(encoded[1:len(encoded)-1]), `\"`, `"`)
		envelope := `{"name":"functions.exec","input":"` + payload + `"}`
		got, err := bpsToolEnvelope(envelope)
		if err != nil || got["input"] != script {
			t.Fatalf("nested script changed: err=%v got=%q want=%q", err, got["input"], script)
		}
	}
}

// L1: transport repair retains the independently supplied script bytes;
// malformed structure never becomes a tool. Valid JSON keeps JSON semantics.
func TestBPSCustomScriptCompatibility(t *testing.T) {
	for _, sample := range []struct{ envelope, input string }{
		{`{"name":"functions.exec","input":"text("hello");"}`, `text("hello");`},
		{`{"name":"functions.exec","input":"const r = /\d+\s/;"}`, `const r = /\d+\s/;`},
		{`{"name":"functions.exec","input":"text('it\'s');"}`, `text('it\'s');`},
		{`{"name":"functions.exec","input":"text(\"hello\");\n"}`, "text(\"hello\");\n"},
	} {
		got, err := bpsToolEnvelope(sample.envelope)
		if err != nil || got["input"] != sample.input {
			t.Fatalf("script changed: got=%q want=%q err=%v", got["input"], sample.input, err)
		}
	}
	for _, sample := range []string{
		`{"name":"functions.exec","input":"text("hello");","name":"other"}`,
		`{"name":"functions.exec","input":"text("hello");"} {"name":"other","input":"x"}`,
		`{"name":"functions.exec","input":"truncated`,
	} {
		if _, err := bpsToolEnvelope(sample); err == nil {
			t.Fatal("ambiguous or truncated envelope accepted")
		}
	}
}
