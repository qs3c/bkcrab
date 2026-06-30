package modelspec

import "testing"

const fixture = `{"entries":[
  {"id":"glm-5.1","name":"GLM 5.1","context":202752,"output":65536,"apiHost":"opencode.ai"},
  {"id":"glm-5.1","name":"GLM 5.1","context":200000,"output":64000,"apiHost":"bigmodel.cn"},
  {"id":"claude-sonnet-4","name":"Claude Sonnet 4","context":200000,"output":64000,"apiHost":"anthropic.com"}
]}`

func mustLoad(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load([]byte(fixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestLookupByIDCaseInsensitive(t *testing.T) {
	c := mustLoad(t)
	spec, ok := c.Lookup("GLM-5.1", "opencode.ai")
	if !ok {
		t.Fatal("expected hit")
	}
	if spec.ContextWindow != 202752 || spec.MaxOutputTokens != 65536 {
		t.Fatalf("got %+v, want {202752 65536}", spec)
	}
}

func TestLookupApiHostPreferred(t *testing.T) {
	c := mustLoad(t)
	spec, _ := c.Lookup("glm-5.1", "bigmodel.cn")
	if spec.ContextWindow != 200000 {
		t.Fatalf("apiHost preference failed: got context %d, want 200000", spec.ContextWindow)
	}
}

func TestLookupConservativeWhenNoHostMatch(t *testing.T) {
	c := mustLoad(t)
	spec, _ := c.Lookup("glm-5.1", "")
	if spec.ContextWindow != 200000 || spec.MaxOutputTokens != 64000 {
		t.Fatalf("conservative tie-break failed: got %+v, want {200000 64000}", spec)
	}
}

func TestLookupByNameWhenIDMisses(t *testing.T) {
	c := mustLoad(t)
	spec, ok := c.Lookup("Claude Sonnet 4", "")
	if !ok || spec.ContextWindow != 200000 {
		t.Fatalf("name lookup failed: ok=%v spec=%+v", ok, spec)
	}
}

func TestLookupUnknownReturnsFalse(t *testing.T) {
	c := mustLoad(t)
	if _, ok := c.Lookup("no-such-model", ""); ok {
		t.Fatal("expected miss")
	}
}

func TestLoadBadJSONErrors(t *testing.T) {
	if _, err := Load([]byte("{not json")); err == nil {
		t.Fatal("expected error on bad json")
	}
}
