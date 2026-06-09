package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// Assemble the leaked-XML fixtures from fragments so this source file
// never contains a literal <function_calls>...</function_calls> block
// (some harnesses scan source for tool-call XML and try to dispatch it).
const (
	openFC  = "<" + "function_calls>"
	closeFC = "<" + "/function_calls>"
	openInv = "<" + "invoke name="
	closInv = "<" + "/invoke>"
	openP   = "<" + "parameter name="
	closP   = "<" + "/parameter>"
)

func TestExtractLeakedToolCalls_NoLeak(t *testing.T) {
	in := "Sure, here is the answer to your question."
	cleaned, calls := extractLeakedToolCalls(in)
	if cleaned != in {
		t.Errorf("expected unchanged text, got %q", cleaned)
	}
	if len(calls) != 0 {
		t.Errorf("expected no calls, got %d", len(calls))
	}
}

func TestExtractLeakedToolCalls_SingleInvoke(t *testing.T) {
	in := "Let me check the docs.\n" +
		openFC + "\n" +
		openInv + `"web_fetch">` + "\n" +
		openP + `"url" string="true">https://docs.e2b.dev/` + closP + "\n" +
		openP + `"max_length" string="false">5000` + closP + "\n" +
		closInv + "\n" +
		closeFC

	cleaned, calls := extractLeakedToolCalls(in)
	if strings.Contains(cleaned, "function_calls") || strings.Contains(cleaned, "invoke name") {
		t.Errorf("xml not stripped: %q", cleaned)
	}
	if !strings.Contains(cleaned, "Let me check the docs") {
		t.Errorf("preface text lost: %q", cleaned)
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Function.Name != "web_fetch" {
		t.Errorf("name=%q", calls[0].Function.Name)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("args not valid json: %v\n%s", err, calls[0].Function.Arguments)
	}
	if args["url"] != "https://docs.e2b.dev/" {
		t.Errorf("url=%v", args["url"])
	}
	if v, ok := args["max_length"].(float64); !ok || v != 5000 {
		t.Errorf("max_length=%v (%T)", args["max_length"], args["max_length"])
	}
}

func TestExtractLeakedToolCalls_AntmlPrefix(t *testing.T) {
	openFCns := "<" + "antml:function_calls>"
	closeFCns := "<" + "/antml:function_calls>"
	openInvns := "<" + "antml:invoke name="
	closInvns := "<" + "/antml:invoke>"
	openPns := "<" + "antml:parameter name="
	closPns := "<" + "/antml:parameter>"

	in := openFCns +
		openInvns + `"bash">` +
		openPns + `"command">ls -la` + closPns +
		closInvns +
		closeFCns

	cleaned, calls := extractLeakedToolCalls(in)
	if strings.TrimSpace(cleaned) != "" {
		t.Errorf("expected empty cleaned, got %q", cleaned)
	}
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("expected bash call, got %+v", calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("args not valid json: %v", err)
	}
	if args["command"] != "ls -la" {
		t.Errorf("command=%v", args["command"])
	}
}

// DeepSeek-style "DSML" marker format observed in the wild from a model
// served via an anthropic-compat endpoint:
//
//	<｜｜DSML｜｜tool_calls>
//	  <｜｜DSML｜｜invoke name="web_search">
//	    <｜｜DSML｜｜parameter name="query" string="true">solo business...</｜｜DSML｜｜parameter>
//	  </｜｜DSML｜｜invoke>
//	</｜｜DSML｜｜tool_calls>
//
// Note: the pipes are U+FF5C (FULLWIDTH VERTICAL LINE), not ASCII `|`,
// and the outer wrapper is `tool_calls` (not `function_calls`).
func TestExtractLeakedToolCalls_DSMLFullwidthPrefix(t *testing.T) {
	openTC := "<" + "｜｜DSML｜｜tool_calls>"
	closeTC := "<" + "/｜｜DSML｜｜tool_calls>"
	openInvDSML := "<" + "｜｜DSML｜｜invoke name="
	closInvDSML := "<" + "/｜｜DSML｜｜invoke>"
	openPDSML := "<" + "｜｜DSML｜｜parameter name="
	closPDSML := "<" + "/｜｜DSML｜｜parameter>"

	in := openTC + "\n" +
		openInvDSML + `"web_search">` + "\n" +
		openPDSML + `"query" string="true">solo business consultant` + closPDSML + "\n" +
		closInvDSML + "\n" +
		openInvDSML + `"web_search">` + "\n" +
		openPDSML + `"query" string="true">independent management consultant` + closPDSML + "\n" +
		closInvDSML + "\n" +
		closeTC

	cleaned, calls := extractLeakedToolCalls(in)
	if strings.Contains(cleaned, "DSML") || strings.Contains(cleaned, "tool_calls") || strings.Contains(cleaned, "invoke name") {
		t.Errorf("xml not stripped: %q", cleaned)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	for i, want := range []string{"solo business consultant", "independent management consultant"} {
		if calls[i].Function.Name != "web_search" {
			t.Errorf("call[%d] name=%q", i, calls[i].Function.Name)
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(calls[i].Function.Arguments), &args); err != nil {
			t.Fatalf("args not valid json: %v", err)
		}
		if args["query"] != want {
			t.Errorf("call[%d] query=%q, want %q", i, args["query"], want)
		}
	}
}

func TestExtractLeakedToolCalls_MultipleInvokes(t *testing.T) {
	in := openFC +
		openInv + `"a">` + openP + `"x">1` + closP + closInv +
		openInv + `"b">` + openP + `"y">2` + closP + closInv +
		closeFC

	_, calls := extractLeakedToolCalls(in)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[0].Function.Name != "a" || calls[1].Function.Name != "b" {
		t.Errorf("names=%q,%q", calls[0].Function.Name, calls[1].Function.Name)
	}
}
