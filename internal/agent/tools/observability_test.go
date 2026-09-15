package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qs3c/bkcrab/internal/observability"
)

func TestExecuteResultOperationalMetrics(t *testing.T) {
	m := observability.New()
	observability.SetDefault(m)
	defer observability.SetDefault(nil)
	r := &Registry{tools: make(map[string]registeredTool)}
	r.Register("custom_private_tool", "test", nil, func(context.Context, json.RawMessage) (string, error) { return "", errors.New("private error body") })
	if _, err := r.ExecuteResult(context.Background(), "custom_private_tool", "{}"); err == nil {
		t.Fatal("tool error lost")
	}
	if _, err := r.ExecuteResult(context.Background(), "unknown_private_tool", "{}"); err == nil {
		t.Fatal("unknown tool error lost")
	}
	if n := testutil.ToFloat64(m.Requests.WithLabelValues("tool", "other", "call", "error")); n != 2 {
		t.Fatal(n)
	}
	if n := testutil.ToFloat64(m.Inflight.WithLabelValues("tool", "other", "call")); n != 0 {
		t.Fatal(n)
	}
}
