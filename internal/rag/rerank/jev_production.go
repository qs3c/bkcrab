package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/jev"
)

const JevRelevancePolicyVersion = "rerank-jev-v1"

// ProductionJev uses request-scoped budgets and bounded retries. Errors are
// returned intact to the search service's RRF fallback; partial ranks never escape.
type ProductionJev struct {
	client    jev.Decider
	model     string
	batchSize int
	timeout   time.Duration
}

func NewProductionJev(client jev.Decider, model string, batchSize int, timeout time.Duration) (*ProductionJev, error) {
	if client == nil || model == "" || batchSize < 1 || batchSize > 20 || timeout <= 0 {
		return nil, errors.New("invalid production Jev reranker configuration")
	}
	return &ProductionJev{client, model, batchSize, timeout}, nil
}
func (c *ProductionJev) Rerank(parent context.Context, query string, docs []string, topN int) (ranked []Result, err error) {
	if strings.TrimSpace(query) == "" || len(docs) > 100 {
		return nil, errors.New("invalid Jev rerank input")
	}
	if len(docs) == 0 || topN <= 0 {
		return []Result{}, nil
	}
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	started := time.Now()
	var usage jev.Usage
	actualModel := ""
	defer func() {
		slog.Info("rag: Jev rerank", "policy", JevRelevancePolicyVersion, "model", actualModel, "candidates", len(docs), "attempts", usage.Attempts, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens, "estimated_cost_usd", usage.EstimatedCostUSD, "usage_unknown", usage.Unknown, "duration_ms", time.Since(started).Milliseconds(), "success", err == nil)
	}()
	// One batch at a time per query; the shared client bounds concurrent queries.
	// Adaptive splitting preserves full passages and the caller's total deadline.
	for start := 0; start < len(docs); {
		end := min(start+c.batchSize, len(docs))
		var state any
		var questions map[string]jev.Question
		for {
			passages := map[string]string{}
			questions = map[string]jev.Question{}
			for i := start; i < end; i++ {
				id := fmt.Sprintf("d%d", i)
				passages[id] = docs[i]
				questions[id] = jev.Noul(fmt.Sprintf("Evaluate ONLY `passages.%s` against `query`. %s", id, JevRelevanceInstruction), "Concrete evidence for at least part of the query or correcting its premise.", "Only shared keywords or topic, without useful evidence.")
			}
			state = map[string]any{"query": query, "passages": passages}
			body, _ := json.Marshal(map[string]any{"model": c.model, "state": state, "questions": questions})
			if len(body) <= jev.MaxRequestBytes {
				break
			}
			if end-start == 1 {
				return nil, errors.New("Jev candidate exceeds local payload limit")
			}
			end = start + (end-start)/2
		}
		result, e := c.client.Decide(ctx, state, questions)
		usage.Add(result.Usage)
		if e != nil {
			return nil, e
		}
		if actualModel != "" && actualModel != result.Model {
			return nil, errors.New("Jev model changed between batches")
		}
		actualModel = result.Model
		if len(result.Probabilities) != end-start {
			return nil, errors.New("Jev rerank answer coverage mismatch")
		}
		for i := start; i < end; i++ {
			v, ok := result.Probabilities[fmt.Sprintf("d%d", i)]
			if !ok {
				return nil, errors.New("Jev rerank missing candidate")
			}
			ranked = append(ranked, Result{Index: i, Score: v})
		}
		start = end
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })
	return ranked[:min(topN, len(ranked))], nil
}

var _ Reranker = (*ProductionJev)(nil)
