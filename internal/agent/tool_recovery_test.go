package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
)

// TestMaybeRecoverToolCallsPreservesThinking pins the other half of the
// django__django-14122__kpsnf5L failure. Recovery clears RawAssistant because
// the leaked-XML turn carried no usable structured message; the provider then
// rebuilds the wire message from the persisted fields. If recovery also
// dropped the reasoning text, DeepSeek thinking mode rejects the next request
// with `The reasoning_content in the thinking mode must be passed back to the
// API.` The recovered call must keep Thinking so the serializer can re-emit it.
func TestMaybeRecoverToolCallsPreservesThinking(t *testing.T) {
	a := &Agent{name: "eval", model: "deepseek-flash"}
	resp := &provider.Response{
		Content: "Let me inspect the field.\n\n<invoke name=\"exec\">" +
			"<parameter name=\"command\" string=\"true\">grep -rn _meta_ordering django</parameter>" +
			"</invoke>",
		Thinking:     "The GROUP BY clause wrongly includes Meta.ordering fields.",
		RawAssistant: json.RawMessage(`{"role":"assistant","content":"stale leaked XML"}`),
	}

	a.maybeRecoverToolCalls(resp)

	if !resp.HasToolCalls() {
		t.Fatalf("expected a recovered tool call, got none (content=%q)", resp.Content)
	}
	if resp.ToolCalls[0].Function.Name != "exec" {
		t.Errorf("recovered tool = %q, want exec", resp.ToolCalls[0].Function.Name)
	}
	if resp.Thinking != "The GROUP BY clause wrongly includes Meta.ordering fields." {
		t.Errorf("recovery dropped Thinking: got %q", resp.Thinking)
	}
	if len(resp.RawAssistant) != 0 {
		t.Errorf("recovery must clear the stale RawAssistant, still have %s", resp.RawAssistant)
	}
	if strings.Contains(resp.Content, "<invoke") {
		t.Errorf("residual content still carries recovered XML: %q", resp.Content)
	}
}

func TestRecoverToolCallsFromContent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		in         string
		wantCalls  int
		wantName   string         // first call only (or "" to skip)
		wantArgs   map[string]any // first call only (or nil to skip)
		wantResLen int            // residual content length (-1 = skip check)
	}{
		{
			name:       "no invoke — passthrough",
			in:         "Here's a plain answer.",
			wantCalls:  0,
			wantResLen: len("Here's a plain answer."),
		},
		{
			name: "single invoke with string+raw params",
			in: `<invoke name="exec">` +
				`<parameter name="command" string="true">echo hi</parameter>` +
				`<parameter name="timeout" string="false">15</parameter>` +
				`</invoke>`,
			wantCalls: 1,
			wantName:  "exec",
			wantArgs:  map[string]any{"command": "echo hi", "timeout": float64(15)},
		},
		{
			name: "DSML wrapper stripped",
			in: `<DSML><tool_calls><invoke name="read_file">` +
				`<parameter name="path" string="true">IDENTITY.md</parameter>` +
				`</invoke></tool_calls></DSML>`,
			wantCalls:  1,
			wantName:   "read_file",
			wantArgs:   map[string]any{"path": "IDENTITY.md"},
			wantResLen: 0,
		},
		{
			name: "multi-line command body preserved",
			in: "<invoke name=\"exec\">" +
				"<parameter name=\"command\" string=\"true\">line1\nline2\nline3</parameter>" +
				"</invoke>",
			wantCalls: 1,
			wantName:  "exec",
			wantArgs:  map[string]any{"command": "line1\nline2\nline3"},
		},
		{
			name: "two invokes both recovered",
			in: `<invoke name="a"><parameter name="x" string="true">1</parameter></invoke>` +
				`<invoke name="b"><parameter name="y" string="false">2</parameter></invoke>`,
			wantCalls: 2,
		},
		{
			name: "missing string attribute defaults to string",
			in: `<invoke name="exec">` +
				`<parameter name="command">echo hi</parameter>` +
				`</invoke>`,
			wantCalls: 1,
			wantArgs:  map[string]any{"command": "echo hi"},
		},
		{
			name: "raw param with invalid JSON falls back to string",
			in: `<invoke name="t">` +
				`<parameter name="weird" string="false">not-json{</parameter>` +
				`</invoke>`,
			wantCalls: 1,
			wantArgs:  map[string]any{"weird": "not-json{"},
		},
		{
			name: "preamble text preserved, invoke stripped",
			in: `Let me check the file. <invoke name="read_file">` +
				`<parameter name="path" string="true">a.md</parameter>` +
				`</invoke>`,
			wantCalls:  1,
			wantResLen: len("Let me check the file."),
		},
		{
			name:       "unclosed invoke — no match, pass through",
			in:         `<invoke name="exec"><parameter name="x" string="true">y`,
			wantCalls:  0,
			wantResLen: -1, // residual is original content; just want 0 calls
		},
		{
			name:       "tag-shaped but no name attribute — skip",
			in:         `<invoke><parameter name="x">y</parameter></invoke>`,
			wantCalls:  0,
			wantResLen: -1, // residual is unchanged input; not asserting length
		},
		{
			// The shape from a real DeepSeek/Qwen detokenization leak:
			// special tokens like `<｜tool_calls｜>` get rendered as ASCII
			// with extra spacing. Recovery must normalize them first.
			name: "leaked DSML-pipe tokens — recovered",
			in: `< | | DSML | | tool_calls>` +
				`< | | DSML | | invoke name="exec">` +
				`<parameter name="command" string="true">which node &amp;&amp; which npx 2&gt;&amp;1</parameter>` +
				`<parameter name="timeout" string="false">10</parameter>` +
				`</ | | DSML | | invoke>` +
				`</ | | DSML | | tool_calls>`,
			wantCalls:  1,
			wantName:   "exec",
			wantArgs:   map[string]any{"command": "which node &amp;&amp; which npx 2&gt;&amp;1", "timeout": float64(10)},
			wantResLen: 0,
		},
		{
			name: "leaked fullwidth-pipe tokens — recovered",
			in: `<｜tool_calls｜>` +
				`<｜invoke name="read_file"｜>` +
				`<parameter name="path" string="true">IDENTITY.md</parameter>` +
				`<｜/invoke｜>` +
				`<｜/tool_calls｜>`,
			wantCalls:  1,
			wantName:   "read_file",
			wantArgs:   map[string]any{"path": "IDENTITY.md"},
			wantResLen: 0,
		},
		{
			// No recoverable invoke inside, but leaked wrappers must still
			// be scrubbed so the UI doesn't render `< | | DSML | …>` text.
			name:       "leaked wrappers without invoke — scrubbed to empty",
			in:         `< | | DSML | | tool_calls></ | | DSML | | tool_calls>`,
			wantCalls:  0,
			wantResLen: 0,
		},
		{
			name:       "leaked wrappers with preamble — preamble preserved",
			in:         `Let me check. <|tool_calls|></|tool_calls|>`,
			wantCalls:  0,
			wantResLen: len("Let me check."),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, residual := recoverToolCallsFromContent(tc.in)
			if len(calls) != tc.wantCalls {
				t.Fatalf("got %d calls, want %d (calls=%+v)", len(calls), tc.wantCalls, calls)
			}
			if tc.wantCalls == 0 {
				// When the test pins a residual length, the case is
				// asserting on the scrub path (leaked-token noise was
				// removed even though no calls were recovered). Otherwise
				// the no-recovery path must leave content untouched.
				if tc.wantResLen >= 0 {
					if len(residual) != tc.wantResLen {
						t.Fatalf("0-call scrub: residual length = %d (%q), want %d", len(residual), residual, tc.wantResLen)
					}
				} else if residual != tc.in {
					t.Fatalf("no-recovery path should leave content untouched: got %q, want %q", residual, tc.in)
				}
				return
			}
			if tc.wantName != "" && calls[0].Function.Name != tc.wantName {
				t.Errorf("first call name = %q, want %q", calls[0].Function.Name, tc.wantName)
			}
			if tc.wantArgs != nil {
				var got map[string]any
				if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &got); err != nil {
					t.Fatalf("parse recovered args: %v (raw=%q)", err, calls[0].Function.Arguments)
				}
				for k, v := range tc.wantArgs {
					if got[k] != v {
						t.Errorf("args[%q] = %v (%T), want %v (%T)", k, got[k], got[k], v, v)
					}
				}
			}
			// Recovered calls must have synthetic IDs so the loop can pair
			// the tool_result message back to this assistant turn.
			if !strings.HasPrefix(calls[0].ID, "recovered_") {
				t.Errorf("expected synthetic id (recovered_*), got %q", calls[0].ID)
			}
			if tc.wantResLen >= 0 && len(residual) != tc.wantResLen {
				t.Errorf("residual length = %d (%q), want %d", len(residual), residual, tc.wantResLen)
			}
			// Residual must never still contain the raw XML — that would
			// double-bill the call in the UI on the next render.
			if strings.Contains(residual, "<invoke") || strings.Contains(residual, "</invoke") {
				t.Errorf("residual still has invoke tags: %q", residual)
			}
		})
	}
}
