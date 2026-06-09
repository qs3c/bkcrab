package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// extractLeakedToolCalls scavenges Claude-style tool-call XML that some
// non-Anthropic models (notably MiMo via xiaomimimo's anthropic-compat
// endpoint, and DeepSeek-derivatives that use the `<｜｜DSML｜｜...>`
// fullwidth-pipe marker style) emit as plain text instead of returning a
// structured content_block of type "tool_use". The model has clearly seen
// Claude's training format `<function_calls><invoke name="X"><parameter
// name="P">v</parameter></invoke></function_calls>` (or a close cousin)
// and reproduces it verbatim, but the upstream gateway never converts it
// back to a tool_use block, so it leaks into the assistant's text content.
//
// When detected, we strip the XML from the text and synthesize ToolCall
// entries the agent loop can dispatch normally. Returns the cleaned
// content and any synthesized calls. If no XML pattern is found,
// returns the input text unchanged and a nil slice.
//
// The tag prefix is optional and tolerant: Claude uses `antml:`, and
// DeepSeek-style models wrap tags in fullwidth pipes like
// `｜｜DSML｜｜` (note: U+FF5C, not ASCII `|`). The outer wrapper tag is
// either `function_calls` (Claude) or `tool_calls` (DSML/OpenAI-ish).
// The `string="true|false"` attribute on parameters controls JSON
// decoding: when string="false" the value is parsed as raw JSON
// (numbers, booleans, arrays); otherwise it's encoded as a string.
const tagPrefixPattern = `(?:antml:|｜｜[^｜<>]+｜｜)?`

var (
	leakedFunctionCallsRe = regexp.MustCompile(`(?s)<` + tagPrefixPattern + `(?:function|tool)_calls>(.*?)</` + tagPrefixPattern + `(?:function|tool)_calls>`)
	leakedInvokeRe        = regexp.MustCompile(`(?s)<` + tagPrefixPattern + `invoke\s+name="([^"]+)"\s*>(.*?)</` + tagPrefixPattern + `invoke>`)
	leakedParameterRe     = regexp.MustCompile(`(?s)<` + tagPrefixPattern + `parameter\s+name="([^"]+)"([^>]*)>(.*?)</` + tagPrefixPattern + `parameter>`)
	leakedStringAttrRe    = regexp.MustCompile(`string="(true|false)"`)
)

func extractLeakedToolCalls(text string) (cleaned string, calls []ToolCall) {
	if text == "" || (!strings.Contains(text, "function_calls") && !strings.Contains(text, "tool_calls")) {
		return text, nil
	}

	matches := leakedFunctionCallsRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	var b strings.Builder
	prev := 0
	for _, m := range matches {
		b.WriteString(text[prev:m[0]])
		prev = m[1]

		body := text[m[2]:m[3]]
		for _, inv := range leakedInvokeRe.FindAllStringSubmatch(body, -1) {
			name := inv[1]
			args := map[string]json.RawMessage{}
			for _, p := range leakedParameterRe.FindAllStringSubmatch(inv[2], -1) {
				pname := p[1]
				attrs := p[2]
				val := p[3]

				asString := true
				if sa := leakedStringAttrRe.FindStringSubmatch(attrs); len(sa) == 2 && sa[1] == "false" {
					asString = false
				}

				if asString {
					raw, _ := json.Marshal(val)
					args[pname] = raw
				} else {
					trimmed := strings.TrimSpace(val)
					if json.Valid([]byte(trimmed)) {
						args[pname] = json.RawMessage(trimmed)
					} else {
						raw, _ := json.Marshal(val)
						args[pname] = raw
					}
				}
			}

			argsJSON, err := json.Marshal(args)
			if err != nil {
				continue
			}
			calls = append(calls, ToolCall{
				ID:   "tooluse_xml_" + randomToolID(),
				Type: "function",
				Function: FunctionCall{
					Name:      name,
					Arguments: string(argsJSON),
				},
			})
		}
	}
	b.WriteString(text[prev:])

	cleaned = strings.TrimSpace(b.String())
	return cleaned, calls
}

func randomToolID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf[:])
}
