package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Normalize only unambiguous JSON envelopes, never arbitrary script text.
// The custom input inside the envelope is preserved byte for byte.
func bpsToolEnvelope(value any) (map[string]any, error) {
	for depth := 0; depth < 4; depth++ {
		if object, ok := value.(map[string]any); ok && object != nil {
			return object, nil
		}
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("BPS tool code must contain one JSON object (received %T)", value)
		}
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), string(rune(0xfeff))))
		fence := strings.Repeat(string(rune(96)), 3)
		if strings.HasPrefix(text, fence) {
			header, body, found := strings.Cut(text, "\n")
			if found && (header == fence+"json" || header == fence) && strings.HasSuffix(body, fence) {
				text = strings.TrimSpace(strings.TrimSuffix(body, fence))
			}
		}
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			if fixed, changed := bpsEscapeJSONControls(text); changed {
				if json.Unmarshal([]byte(fixed), &value) == nil {
					continue
				}
			}
			if inner, ok := bpsLooseCustomEnvelope(text); ok {
				return inner, nil
			}
			if syntax, ok := err.(*json.SyntaxError); ok {
				return nil, fmt.Errorf("BPS tool code must contain one JSON object (invalid JSON at byte %d)", syntax.Offset)
			}
			return nil, fmt.Errorf("BPS tool code must contain one JSON object")
		}
	}
	return nil, fmt.Errorf("BPS tool code exceeds JSON wrapping limit")
}

// A common upstream variant leaves script quotes or script-only escapes inside
// the final input string. Recover only the exact two-field custom envelope;
// never infer a tool name, missing delimiter, or additional field. Valid JSON
// always takes precedence. Catalog/choice/type validation still runs afterwards.
var bpsCustomPrefix = regexp.MustCompile(`^\{\s*"(name|tool)"\s*:\s*("(?:[^"\\]|\\.)*")\s*,\s*"(input|args)"\s*:\s*"`)
var bpsCustomSuffix = regexp.MustCompile(`"\s*\}$`)
var bpsExtraField = regexp.MustCompile(`"\s*,\s*"[^"\r\n]+"\s*:`)

func bpsLooseCustomEnvelope(text string) (map[string]any, bool) {
	prefix := bpsCustomPrefix.FindStringSubmatchIndex(text)
	suffix := bpsCustomSuffix.FindStringIndex(text)
	if prefix == nil || suffix == nil || suffix[0] < prefix[1] {
		return nil, false
	}
	var name string
	if json.Unmarshal([]byte(text[prefix[4]:prefix[5]]), &name) != nil || name == "" {
		return nil, false
	}
	payload := text[prefix[1]:suffix[0]]
	var out strings.Builder
	out.WriteByte('"')
	for i := 0; i < len(payload); i++ {
		ch := payload[i]
		switch {
		case ch == '\\':
			if i+1 >= len(payload) || payload[i+1] < 32 {
				return nil, false
			}
			next := payload[i+1]
			if strings.ContainsRune(`"\/bfnrt`, rune(next)) {
				out.WriteByte(ch)
				i++
				out.WriteByte(next)
			} else if next == 'u' && i+5 < len(payload) && bpsHex4(payload[i+2:i+6]) {
				out.WriteString(payload[i : i+6])
				i += 5
			} else {
				// Script regex/path escapes such as \d, \s, \P, \' are
				// literal backslashes at this JSON transport layer.
				out.WriteString(`\\`)
			}
		case ch == '"':
			out.WriteString(`\"`)
		case ch < 32:
			fmt.Fprintf(&out, `\u%04x`, ch)
		default:
			out.WriteByte(ch)
		}
	}
	out.WriteByte('"')
	var input string
	if json.Unmarshal([]byte(out.String()), &input) != nil {
		return nil, false
	}
	if !bpsScriptBoundaryIntact(input) {
		return nil, false
	}
	return map[string]any{"name": name, "input": input}, true
}

// Check candidate envelope endings only outside script strings/comments and
// balanced containers. A JSON object inside JavaScript, a template literal or
// a shell here-string is payload, not another transport field. This is a
// boundary check, not a script evaluator; the client still owns execution.
func bpsScriptBoundaryIntact(input string) bool {
	var stack []byte
	var quote, previous byte
	lineComment, blockComment, regex, regexClass := false, false, false, false
	here := ""
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if here != "" {
			if (i == 0 || input[i-1] == '\n') && strings.HasPrefix(input[i:], here) {
				i += len(here) - 1
				here = ""
			}
			continue
		}
		if lineComment {
			if ch == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if ch == '*' && i+1 < len(input) && input[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quote != 0 {
			if ch == '\\' {
				i++
				continue
			}
			if ch == quote {
				quote = 0
				previous = ch
			}
			continue
		}
		if regex {
			if ch == '\\' {
				i++
				continue
			}
			if ch == '[' {
				regexClass = true
			}
			if ch == ']' {
				regexClass = false
			}
			if ch == '/' && !regexClass {
				regex = false
				previous = ch
			}
			continue
		}
		if ch == '/' && i+1 < len(input) {
			if input[i+1] == '/' {
				lineComment = true
				i++
				continue
			}
			if input[i+1] == '*' {
				blockComment = true
				i++
				continue
			}
			if previous == 0 || strings.ContainsRune("=(:,[!&|?;{", rune(previous)) {
				regex = true
				continue
			}
		}
		if ch == '@' && i+2 < len(input) && (input[i+1] == '\'' || input[i+1] == '"') && (input[i+2] == '\r' || input[i+2] == '\n') {
			here = string(input[i+1]) + "@"
			i++
			continue
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			if ch == '"' && len(stack) == 0 && previous != 0 && !strings.ContainsRune("=(:,[!&|?+", rune(previous)) {
				tail := input[i:]
				if match := bpsExtraField.FindStringIndex(tail); match != nil && match[0] == 0 {
					return false
				}
				if strings.HasPrefix(strings.TrimSpace(tail[1:]), "}") {
					return false
				}
			}
			quote = ch
			continue
		}
		switch ch {
		case '(', '[', '{':
			stack = append(stack, ch)
		case ')', ']', '}':
			if len(stack) == 0 {
				return false
			}
			open := stack[len(stack)-1]
			if !(open == '(' && ch == ')' || open == '[' && ch == ']' || open == '{' && ch == '}') {
				return false
			}
			stack = stack[:len(stack)-1]
		}
		if ch != ' ' && ch != '\r' && ch != '\n' && ch != '\t' {
			previous = ch
		}
	}
	return quote == 0 && !regex && !blockComment && here == "" && len(stack) == 0
}

func bpsHex4(value string) bool {
	for _, ch := range value {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
			return false
		}
	}
	return true
}

// Observed live failure: literal newlines inside the envelope's input string.
// Escaping control characters preserves their decoded value. Never guess
// missing quotes, invalid escapes, line continuations or concatenated objects.
func bpsEscapeJSONControls(text string) (string, bool) {
	var out strings.Builder
	inside, escaped, changed := false, false, false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if inside && ch < 32 {
			if escaped {
				return text, false
			}
			out.WriteByte(92)
			out.WriteString(fmt.Sprintf("u%04x", ch))
			changed = true
			continue
		}
		out.WriteByte(ch)
		if inside {
			if escaped {
				escaped = false
			} else if ch == 92 {
				escaped = true
			} else if ch == 34 {
				inside = false
			}
		} else if ch == 34 {
			inside = true
		}
	}
	return out.String(), changed
}
