package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type bpsExchangeKey struct{}
type bpsTool struct {
	Name, Namespace, Kind string
	Schema                *jsonschema.Schema
}
type bpsExchange struct {
	Async                            map[string]bpsAsyncHistory
	Images                           map[string]bpsImageHistory
	StreamState                      bpsStreamState
	RequestKey                       string
	Observe                          func(string, int)
	relay                            *relay
	Scope, Requested, Actual, Choice string
	Tools                            map[string]bpsTool
	Parallel                         bool
	Compact                          bool
	History                          map[string]map[string]any
	Restored                         map[string]bpsCallPair
	FailureKey                       string
}
type bpsCallPair struct{ Native, Client map[string]any }

// Bounded, process-local history. Never reconstruct missing calls or persist inputs.
type bpsCallCache struct {
	sync.Mutex
	Entries map[string]bpsCallPair
	Order   []string
	Bytes   int
}

func (c *bpsCallCache) put(key string, pair bpsCallPair) error {
	return c.putBatch(map[string]bpsCallPair{key: pair})
}

// Reserve an entire response before exposing any executable item to Codex.
func (c *bpsCallCache) putBatch(pairs map[string]bpsCallPair) error {
	sizes, total := map[string]int{}, 0
	for key, pair := range pairs {
		raw, _ := json.Marshal(pair)
		if len(raw) > 2<<20 {
			return fmt.Errorf("BPS tool call exceeds history limit")
		}
		sizes[key] = len(raw)
		total += len(raw)
	}
	if len(pairs) > 512 || total > 16<<20 {
		return fmt.Errorf("BPS tool response exceeds history limit")
	}
	c.Lock()
	defer c.Unlock()
	if c.Entries == nil {
		c.Entries = map[string]bpsCallPair{}
	}
	for key := range pairs {
		if _, ok := c.Entries[key]; ok {
			return fmt.Errorf("BPS duplicate call_id; execution suppressed")
		}
	}
	for len(c.Order)+len(pairs) > 512 || c.Bytes+total > 16<<20 {
		oldest := c.Order[0]
		c.Order = c.Order[1:]
		b, _ := json.Marshal(c.Entries[oldest])
		c.Bytes -= len(b)
		delete(c.Entries, oldest)
	}
	for key, pair := range pairs {
		c.Entries[key] = pair
		c.Order = append(c.Order, key)
		c.Bytes += sizes[key]
	}
	return nil
}
func (c *bpsCallCache) get(key string) (bpsCallPair, bool) {
	c.Lock()
	defer c.Unlock()
	p, ok := c.Entries[key]
	return p, ok
}
func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func cloneObject(m map[string]any) map[string]any {
	n := make(map[string]any, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}
func exchange(req *http.Request) *bpsExchange {
	x, _ := req.Context().Value(bpsExchangeKey{}).(*bpsExchange)
	return x
}
func developerMessage(s string) map[string]any {
	return map[string]any{"role": "developer", "content": s}
}

func toolCatalog(items []any, namespace string, out map[string]bpsTool, catalog *[]any) error {
	for _, v := range items {
		tool, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid tool definition")
		}
		name, kind := str(tool, "name"), str(tool, "type")
		if kind == "function" && name == "" {
			if nested, ok := tool["function"].(map[string]any); ok {
				tool = cloneObject(nested)
				tool["type"] = kind
				name = str(tool, "name")
			}
		}
		if kind == "tool_search" && str(tool, "execution") == "client" {
			name = "tool_search"
		}
		if kind != "namespace" && kind != "function" && kind != "custom" && kind != "tool_search" {
			return fmt.Errorf("BPS unsupported client tool type: %s; disable this hosted tool for the BPS provider", kind)
		}
		if name == "" {
			return fmt.Errorf("BPS tools require names")
		}
		full := name
		if namespace != "" {
			full = namespace + "." + name
		}
		if kind == "namespace" {
			children, _ := tool["tools"].([]any)
			if err := toolCatalog(children, full, out, catalog); err != nil {
				return err
			}
			continue
		}
		if _, ok := out[full]; ok {
			return fmt.Errorf("duplicate tool: %s", full)
		}
		entry := cloneObject(tool)
		entry["name"] = full
		t := bpsTool{Name: name, Namespace: namespace, Kind: kind}
		if kind == "function" || kind == "tool_search" {
			schema := tool["parameters"]
			if schema == nil {
				schema = tool["inputSchema"]
			}
			if schema == nil {
				schema = tool["input_schema"]
			}
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			// Only embedded schemas are allowed; compilation cannot fetch remote resources.
			c := jsonschema.NewCompiler()
			c.UseLoader(noSchemaNetwork{})
			if err := c.AddResource("urn:bps:tool", schema); err != nil {
				return err
			}
			s, err := c.Compile("urn:bps:tool")
			if err != nil {
				return fmt.Errorf("invalid schema for %s: %w", full, err)
			}
			t.Schema = s
			entry["parameters"] = schema
		}
		out[full] = t
		*catalog = append(*catalog, entry)
	}
	return nil
}

type noSchemaNetwork struct{}

func (noSchemaNetwork) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema resource not allowed: %s", url)
}

func (r *relay) prepareBPS(req *http.Request, meta requestMetadata) (*http.Request, error) {
	compact := responsesPath(req.URL.Path) == "/responses/compact"
	if enc := req.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil, fmt.Errorf("BPS requires enable_request_compression=false")
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var src map[string]any
	if err = json.Unmarshal(raw, &src); err != nil {
		return nil, fmt.Errorf("invalid BPS JSON request")
	}
	if tier, exists := src["service_tier"]; exists && tier != nil {
		// Live BPS rejects even service_tier=default with HTTP 422. Omission
		// requests the upstream default; priority/flex cannot be promised.
		if tier != "auto" && tier != "default" {
			return nil, fmt.Errorf("BPS does not support service_tier priority/flex or unknown tiers; use default or auto")
		}
	}
	x := &bpsExchange{relay: r, Tools: map[string]bpsTool{}, Choice: "auto", Parallel: true, Compact: compact, History: map[string]map[string]any{}}
	if parallel, ok := src["parallel_tool_calls"].(bool); ok {
		x.Parallel = parallel
	}
	x.Requested = str(src, "reasoning_effort")
	if reason, ok := src["reasoning"].(map[string]any); ok && str(reason, "effort") != "" {
		x.Requested = str(reason, "effort")
	}
	if x.Requested == "" {
		x.Requested = "medium"
	}
	x.Actual = x.Requested
	switch x.Actual {
	case "max", "ultra":
		x.Actual = "xhigh"
	case "low", "medium", "high", "xhigh":
	default:
		return nil, fmt.Errorf("unsupported BPS reasoning effort: %s", x.Actual)
	}
	id, _ := sessionIdentity(req, meta)
	if id == "" {
		id = str(src, "prompt_cache_key")
	}
	tools, _ := src["tools"].([]any)
	// Responses Lite carries explicit client tool declarations in input items.
	// These are part of the request contract, not model-authored instructions.
	if items, ok := src["input"].([]any); ok {
		for _, v := range items {
			if item, ok := v.(map[string]any); ok && str(item, "type") == "additional_tools" {
				declared, ok := item["tools"].([]any)
				if !ok {
					return nil, fmt.Errorf("invalid additional_tools declaration")
				}
				tools = append(tools, declared...)
			}
		}
	}
	if id == "" && len(tools) > 0 {
		return nil, fmt.Errorf("BPS tools require a stable session ID")
	}
	x.Scope = accountIdentity(req) + "\x00" + id + "\x00"
	if items, ok := src["input"].([]any); ok {
		src["input"] = x.asyncHistory(items)
	}
	catalog := []any{}
	if err = toolCatalog(tools, "", x.Tools, &catalog); err != nil {
		return nil, err
	}
	if items, ok := src["input"].([]any); ok {
		if err = x.restoreRestartHistory(id, items); err != nil {
			return nil, err
		}
	}
	// Deferred tools are declared in client tool_search_output, not necessarily
	// in the next request's top-level tools. Prefer current declarations, then
	// the newest scoped discovery result; never infer schemas from prose.
	if items, ok := src["input"].([]any); ok {
		for i := len(items) - 1; i >= 0; i-- {
			item, ok := items[i].(map[string]any)
			if !ok || str(item, "type") != "tool_search_output" || str(item, "execution") != "client" || str(item, "status") != "completed" {
				continue
			}
			pair, known := x.historyPair(str(item, "call_id"))
			if !known || str(pair.Client, "type") != "tool_search_call" {
				return nil, fmt.Errorf("BPS discovery result has no matching scoped client call")
			}
			declared, ok := item["tools"].([]any)
			if !ok {
				return nil, fmt.Errorf("invalid BPS discovered tool declarations")
			}
			found, entries := map[string]bpsTool{}, []any{}
			if err = toolCatalog(declared, "", found, &entries); err != nil {
				return nil, err
			}
			for _, entry := range entries {
				name := str(entry.(map[string]any), "name")
				if _, exists := x.Tools[name]; !exists {
					x.Tools[name] = found[name]
					catalog = append(catalog, entry)
				}
			}
		}
	}
	switch choice := src["tool_choice"].(type) {
	case string:
		x.Choice = choice
	case map[string]any:
		name := str(choice, "name")
		if name == "" && str(choice, "type") == "function" {
			if nested, ok := choice["function"].(map[string]any); ok {
				name = str(nested, "name")
			}
		}
		if ns := str(choice, "namespace"); ns != "" {
			name = ns + "." + name
		}
		if _, ok := x.Tools[name]; !ok {
			return nil, fmt.Errorf("unknown selected tool")
		}
		x.Choice = name
	case nil:
	default:
		return nil, fmt.Errorf("unsupported tool_choice")
	}
	if x.Choice != "auto" && x.Choice != "none" && x.Choice != "required" {
		if _, ok := x.Tools[x.Choice]; !ok {
			return nil, fmt.Errorf("unsupported tool_choice")
		}
	}
	if x.Choice == "required" && len(x.Tools) == 0 {
		return nil, fmt.Errorf("required tool_choice has no tools")
	}
	input := []any{}
	if instructions := str(src, "instructions"); instructions != "" {
		input = append(input, developerMessage(instructions))
	}
	catalogJSON, _ := json.Marshal(catalog)
	protocol := bpsClientProtocol(catalogJSON, x.Choice, x.Parallel)
	if !compact {
		input = append(input, developerMessage(protocol))
	}
	var history []any
	switch v := src["input"].(type) {
	case []any:
		history = v
	case string:
		history = []any{map[string]any{"role": "user", "content": v}}
	case nil:
	default:
		return nil, fmt.Errorf("invalid BPS input")
	}
	turnBoundary := []any{}
	iteration := 0
	usedCalls := map[string]bool{}
	for _, v := range history {
		if item, ok := v.(map[string]any); ok {
			usedCalls[str(item, "call_id")] = true
		}
	}
	for historyIndex, v := range history {
		item, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid BPS input item")
		}
		kind := str(item, "type")
		if str(item, "role") == "user" {
			turnBoundary = append(turnBoundary, item)
			iteration = 0
		}
		switch kind {
		case "additional_tools":
			// Consumed into the client catalog; not an upstream input item.
			continue
		case "function_call", "custom_tool_call", "tool_search_call", "function_call_output", "custom_tool_call_output", "tool_search_output":
			call := str(item, "call_id")
			pair, ok := x.historyPair(call)
			if call == "" || !ok {
				return nil, fmt.Errorf("BPS tool history missing; start a new session (no tool was replayed)")
			}
			x.History[call+"/"+kind] = item
			if strings.HasSuffix(kind, "_output") {
				n := cloneObject(item)
				n["type"] = "function_call_output"
				if str(pair.Native, "type") == "custom_tool_call" {
					n["type"] = "custom_tool_call_output"
				}
				if kind == "tool_search_output" {
					if str(pair.Native, "type") == "tool_search_call" {
						n["type"] = kind
					} else {
						payload, _ := json.Marshal(map[string]any{"tools": item["tools"], "status": item["status"]})
						n = map[string]any{"type": "function_call_output", "call_id": call, "output": string(payload)}
					}
				}
				delete(n, "id")
				input = append(input, n)
				iteration++
			} else {
				for _, k := range []string{"type", "name", "namespace", "arguments", "input", "execution"} {
					if !reflect.DeepEqual(item[k], pair.Client[k]) {
						return nil, fmt.Errorf("BPS tool history mismatch")
					}
				}
				input = append(input, pair.Native)
			}
		case "item_reference":
			return nil, fmt.Errorf("BPS cannot replay item references; start a new session")
		case "reasoning":
			if str(item, "encrypted_content") != "" {
				input = append(input, item)
			}
		default:
			input = append(input, x.userImages(item, historyIndex, usedCalls)...)
		}
	}
	codeName, codeTool, hasCodeMode := x.resolveTool("exec", "")
	if !compact && hasCodeMode && codeTool.Kind == "custom" {
		reminder := developerMessage(bpsCodeModeReminder(codeName))
		index := len(input)
		if index > 0 {
			if last, ok := input[index-1].(map[string]any); ok && str(last, "type") == "compaction_trigger" {
				index--
			}
		}
		input = append(input, nil)
		copy(input[index+1:], input[index:])
		input[index] = reminder
	}
	boundary, _ := json.Marshal(turnBoundary)
	turn := fmt.Sprintf("%x", sha256.Sum256(append([]byte(x.Scope), boundary...)))
	x.FailureKey = fmt.Sprintf("%x", sha256.Sum256([]byte(turn+fmt.Sprint(iteration))))
	x.RequestKey = x.FailureKey
	if !compact {
		if err := x.applyBPSCorrection(&input); err != nil {
			return nil, err
		}
	}
	metadata := map[string]any{}
	if m, ok := src["metadata"].(map[string]any); ok {
		for k, v := range m {
			switch v.(type) {
			case string, float64, bool:
				metadata[k] = fmt.Sprint(v)
			}
		}
	}
	metadata["task_id"] = fmt.Sprintf("%x", sha256.Sum256([]byte(x.Scope)))
	metadata["turn_id"] = turn
	metadata["agent_iteration"] = fmt.Sprint(iteration)
	body := map[string]any{"model": src["model"], "model_selection": "explicit", "stream": true, "store": false, "input": input, "reasoning_effort": x.Actual, "metadata": metadata}
	if v, ok := src["context_management"].([]any); ok && len(v) > 0 {
		body["context_management"] = v
	}
	if v := str(src, "prompt_cache_key"); v != "" {
		body["prompt_cache_key"] = v
	}
	if compact {
		body = map[string]any{"model": src["model"], "input": input, "metadata": metadata}
		x.Requested, x.Actual = "", ""
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	q := req.Clone(context.WithValue(req.Context(), bpsExchangeKey{}, x))
	q.Body = io.NopCloser(bytes.NewReader(encoded))
	q.ContentLength = int64(len(encoded))
	q.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	q.Header.Del("Content-Length")
	return q, nil
}

func (x *bpsExchange) convertTool(native map[string]any) (map[string]any, error) {
	if str(native, "name") != "run_officejs" && str(native, "name") != "functions.run_officejs" {
		return x.directTool(native)
	}
	if str(native, "type") != "function_call" || (str(native, "name") != "run_officejs" && str(native, "name") != "functions.run_officejs") {
		return nil, fmt.Errorf("BPS returned an unsupported native tool")
	}
	var outer map[string]any
	if json.Unmarshal([]byte(str(native, "arguments")), &outer) != nil {
		return nil, fmt.Errorf("invalid BPS transport arguments")
	}
	inner, err := x.toolPayload(outer)
	if err != nil {
		return nil, err
	}
	name := str(inner, "name")
	if name == "" {
		name = str(inner, "tool")
	}
	name, t, ok := x.resolveTool(name, "")
	if !ok || x.Choice == "none" || (x.Choice != "auto" && x.Choice != "required" && x.Choice != name) {
		return nil, fmt.Errorf("BPS returned a tool outside the allowed catalog/choice: %s", name)
	}
	call := str(native, "call_id")
	if call == "" || str(native, "id") == "" {
		return nil, fmt.Errorf("BPS tool identity missing")
	}
	client := map[string]any{"type": t.Kind + "_call", "id": native["id"], "call_id": call, "name": t.Name, "status": "completed"}
	if t.Namespace != "" {
		client["namespace"] = t.Namespace
	}
	if t.Kind == "function" || t.Kind == "tool_search" {
		args, ok := inner["arguments"].(map[string]any)
		if !ok {
			args, ok = inner["args"].(map[string]any)
		}
		if !ok {
			return nil, fmt.Errorf("BPS function arguments must be an object")
		}
		if err := t.Schema.Validate(args); err != nil {
			return nil, fmt.Errorf("BPS tool arguments fail the declared JSON schema")
		}
		raw, _ := json.Marshal(args)
		client["arguments"] = string(raw)
		if t.Kind == "tool_search" {
			client["id"] = "tsc_" + call
			client["execution"] = "client"
			client["arguments"] = args
			delete(client, "name")
		}
	} else {
		input, ok := inner["input"].(string)
		if !ok {
			input, ok = inner["args"].(string)
		}
		if !ok {
			return nil, fmt.Errorf("BPS custom input must be text")
		}
		client["type"] = "custom_tool_call"
		client["id"] = "ctc_" + call
		client["input"] = input
	}
	return client, nil
}

// Newer BPS responses can use native client-shaped calls directly. Accept only
// an unambiguous declared tool of the same type; never expose Excel-only tools.
func (x *bpsExchange) directTool(native map[string]any) (map[string]any, error) {
	kind := str(native, "type")
	if kind != "function_call" && kind != "custom_tool_call" && kind != "tool_search_call" {
		return nil, fmt.Errorf("BPS returned an unsupported native tool: %s %s", kind, str(native, "name"))
	}
	name := str(native, "name")
	if kind == "tool_search_call" && str(native, "execution") == "client" {
		name = "tool_search"
	}
	qualified, t, ok := x.resolveTool(name, str(native, "namespace"))
	if !ok || x.Choice == "none" || (x.Choice != "auto" && x.Choice != "required" && x.Choice != qualified) {
		return nil, fmt.Errorf("BPS native tool is not uniquely declared or allowed: %s", name)
	}
	want := "function_call"
	if t.Kind == "custom" {
		want = "custom_tool_call"
	} else if t.Kind == "tool_search" {
		want = "tool_search_call"
	}
	if kind != want {
		return nil, fmt.Errorf("BPS native tool type mismatch")
	}
	if str(native, "call_id") == "" || str(native, "id") == "" {
		return nil, fmt.Errorf("BPS native tool identity missing")
	}
	client := cloneObject(native)
	client["name"] = t.Name
	client["status"] = "completed"
	delete(client, "namespace")
	if t.Namespace != "" {
		client["namespace"] = t.Namespace
	}
	if t.Kind == "tool_search" {
		if t.Schema.Validate(native["arguments"]) != nil {
			return nil, fmt.Errorf("BPS tool search arguments fail the declared JSON schema")
		}
		delete(client, "name")
	} else if t.Kind == "function" {
		var args map[string]any
		if json.Unmarshal([]byte(str(native, "arguments")), &args) != nil || args == nil {
			return nil, fmt.Errorf("BPS native function arguments must be an object")
		}
		if t.Schema.Validate(args) != nil {
			return nil, fmt.Errorf("BPS native arguments fail the declared JSON schema")
		}
	} else {
		if _, ok := native["input"].(string); !ok {
			return nil, fmt.Errorf("BPS native custom input must be text")
		}
	}
	return client, nil
}

func (x *bpsExchange) resolveTool(name, namespace string) (string, bpsTool, bool) {
	qualified := name
	if namespace != "" {
		qualified = namespace + "." + name
	}
	t, ok := x.Tools[qualified]
	if !ok && strings.HasPrefix(qualified, "functions.") {
		// BPS can add its presentation namespace to a flat client tool name.
		// Never strip arbitrary namespaces, nested names, or resolve collisions.
		flat := strings.TrimPrefix(qualified, "functions.")
		candidate, declared := x.Tools[flat]
		if declared && candidate.Namespace == "" && !strings.Contains(flat, ".") {
			matches := 0
			for _, tool := range x.Tools {
				if tool.Name == flat {
					matches++
				}
			}
			if matches == 1 {
				return flat, candidate, true
			}
		}
	}
	if !ok && namespace == "" {
		matches := 0
		for key, tool := range x.Tools {
			if tool.Name == name {
				qualified, t = key, tool
				matches++
			}
		}
		ok = matches == 1
	}
	return qualified, t, ok
}
