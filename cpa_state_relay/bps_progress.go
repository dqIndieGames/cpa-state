package main

import "strings"

// Track only JSON lexical state, never retain or transform argument contents.
// Formatting whitespace outside strings is not tool-generation progress.
// Whitespace inside a string remains real payload, including long scripts.
type bpsArgumentProgress struct{ inString, escaped bool }

func (p *bpsArgumentProgress) advance(delta string) bool {
	meaningful := false
	for i := 0; i < len(delta); i++ {
		c := delta[i]
		if p.inString {
			meaningful = true
			if p.escaped {
				p.escaped = false
			} else if c == 92 {
				p.escaped = true
			} else if c == 34 {
				p.inString = false
			}
		} else if c == 34 {
			p.inString = true
			meaningful = true
		} else if c != 32 && c != 9 && c != 10 && c != 13 {
			meaningful = true
		}
	}
	return meaningful
}

func (s *bpsStream) contentProgress(event map[string]any) bool {
	kind := str(event, "type")
	if kind == "response.output_item.added" {
		item, _ := event["item"].(map[string]any)
		if isToolItem(item) && str(item, "id") != "" {
			if s.argumentProgress == nil {
				s.argumentProgress = map[string]*bpsArgumentProgress{}
			}
			id := str(item, "id")
			if s.argumentProgress[id] == nil && len(s.argumentProgress) < 512 {
				p := &bpsArgumentProgress{}
				p.advance(str(item, "arguments"))
				s.argumentProgress[id] = p
			}
		}
		return true
	}
	if kind == "response.function_call_arguments.delta" {
		delta := str(event, "delta")
		if p := s.argumentProgress[str(event, "item_id")]; p != nil {
			return p.advance(delta)
		}
		return strings.TrimSpace(delta) != ""
	}
	return (strings.HasSuffix(kind, ".delta") && str(event, "delta") != "") || kind == "response.output_item.done" || kind == "response.completed"
}
