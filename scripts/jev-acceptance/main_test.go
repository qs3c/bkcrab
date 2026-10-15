package main

import (
	"context"
	"errors"
	"testing"

	"github.com/qs3c/bkcrab/internal/jev"
)

type chargedFailure struct{ calls int }

func (f *chargedFailure) Decide(context.Context, any, map[string]jev.Question) (jev.Result, error) {
	f.calls++
	return jev.Result{Usage: jev.Usage{Attempts: 1, EstimatedCostUSD: .01, Unknown: true}}, errors.New("no response")
}
func TestAcceptanceBudgetIncludesFailedAttempts(t *testing.T) {
	provider := &chargedFailure{}
	m := &metered{client: provider, budget: .02}
	for i := 0; i < 3; i++ {
		_, _ = m.Decide(context.Background(), "", nil)
	}
	if provider.calls != 2 || m.used != .02 || len(m.calls) != 2 {
		t.Fatalf("failed attempts bypassed cap: %+v", m)
	}
}
