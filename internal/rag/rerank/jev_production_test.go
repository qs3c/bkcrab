package rerank

import (
	"context"
	"errors"
	"fmt"
	"github.com/qs3c/bkcrab/internal/jev"
	"strings"
	"testing"
	"time"
)

type fakeDecider struct {
	calls int
	fail  int
}

func (f *fakeDecider) Decide(ctx context.Context, state any, q map[string]jev.Question) (jev.Result, error) {
	f.calls++
	if f.calls == f.fail {
		return jev.Result{}, errors.New("unavailable")
	}
	r := jev.Result{Model: "m", Probabilities: map[string]float64{}}
	for id := range q {
		r.Probabilities[id] = .7
	}
	return r, nil
}
func TestProductionJevPreservesIndexesAndSplits(t *testing.T) {
	f := &fakeDecider{}
	c, _ := NewProductionJev(f, "m", 20, time.Second)
	docs := make([]string, 20)
	for i := range docs {
		docs[i] = fmt.Sprintf("%d %s", i, strings.Repeat("x", 4000))
	}
	r, e := c.Rerank(context.Background(), "query", docs, 5)
	if e != nil || len(r) != 5 || f.calls < 2 {
		t.Fatalf("%v %+v calls=%d", e, r, f.calls)
	}
	for i, v := range r {
		if v.Index != i {
			t.Fatal("unstable tie")
		}
	}
}
func TestProductionJevNeverReturnsPartialRank(t *testing.T) {
	f := &fakeDecider{fail: 2}
	c, _ := NewProductionJev(f, "m", 1, time.Second)
	r, e := c.Rerank(context.Background(), "query", []string{"a", "b"}, 2)
	if e == nil || len(r) != 0 {
		t.Fatal("partial rank escaped")
	}
}
