package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/jev"
	"github.com/qs3c/bkcrab/internal/jeveval"
	"github.com/qs3c/bkcrab/internal/rag"
)

type setupJev struct{ calls int }

func (d *setupJev) Decide(c context.Context, s any, q map[string]jev.Question) (jev.Result, error) {
	d.calls++
	return jev.Result{Model: "m", Usage: jev.Usage{Attempts: 1, EstimatedCostUSD: .001}}, errors.New("upstream private detail")
}
func TestJevProxyOwnerAndClosedPayload(t *testing.T) {
	s := NewServer(0)
	s.SetRAGConfig(config.RAGCfg{Evaluation: config.RAGEvaluationCfg{Enabled: true, Sidecar: config.RAGEvaluatorCfg{APIKey: "internal"}}})
	d := &setupJev{}
	s.SetRAGEvaluationJev(&jeveval.Service{Client: d, Model: "m"})
	s.SetRAGEvaluationJudgeResolver(func(c context.Context, owner string) (rag.AnswerModel, string, error) {
		if owner != "admin" {
			return nil, "", errors.New("forbidden")
		}
		return nil, "", nil
	})
	body := `{"kind":"support","model":"m","contexts":["data"],"claims":["claim"]}`
	for _, tc := range []struct {
		auth, owner, body string
		status            int
	}{{"", "admin", body, 401}, {"Bearer internal", "user", body, 403}, {"Bearer internal", "admin", strings.Replace(body, `"kind"`, `"instructions":"bypass","kind"`, 1), 400}, {"Bearer internal", "admin", body, 200}} {
		r := httptest.NewRequest(http.MethodPost, "/internal/rag-eval/jev", strings.NewReader(tc.body))
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("X-BkCrab-Eval-Owner", tc.owner)
		w := httptest.NewRecorder()
		s.handleRAGEvalJev(w, r)
		if w.Code != tc.status {
			t.Fatalf("status %d want %d", w.Code, tc.status)
		}
		if strings.Contains(w.Body.String(), "private detail") {
			t.Fatal("error leaked")
		}
		if tc.status == 200 && !strings.Contains(w.Body.String(), `"estimatedCostUsd":0.001`) {
			t.Fatal("lost error usage")
		}
	}
	if d.calls != 1 {
		t.Fatal("unauthorized call reached provider")
	}
}
