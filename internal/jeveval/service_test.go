package jeveval

import (
	"context"
	"errors"
	"testing"

	"github.com/qs3c/bkcrab/internal/jev"
)

type decider struct{ calls int }

func (d *decider) Decide(c context.Context, s any, q map[string]jev.Question) (jev.Result, error) {
	d.calls++
	return jev.Result{Usage: jev.Usage{Attempts: 1, EstimatedCostUSD: .001, Unknown: true}}, errors.New("provider failure")
}
func TestFixedRubricsAndFailureUsage(t *testing.T) {
	d := &decider{}
	s := Service{Client: d, Model: "m"}
	r := Request{Kind: "support", Model: "m", Contexts: []string{"data"}, Claims: []string{"claim"}}
	out := s.Evaluate(context.Background(), r)
	if out.Error == "" || out.Result.Usage.EstimatedCostUSD == 0 {
		t.Fatal("lost error usage")
	}
	r.Model = "other"
	s.Evaluate(context.Background(), r)
	r.Model = "m"
	r.Kind = "arbitrary"
	s.Evaluate(context.Background(), r)
	if d.calls != 1 {
		t.Fatal("invalid request dispatched")
	}
}
