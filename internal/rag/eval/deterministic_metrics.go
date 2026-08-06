package eval

import (
	"math"
	"sort"
	"strings"
)

type DeterministicInput struct {
	RetrievedContextIDs []string
	ReferenceContextIDs []string
	Citations           []string
	Response            string
	ExpectedAbstention  bool
	Abstained           bool
}

func DeterministicMetrics(input DeterministicInput, k int) map[string]MetricResult {
	if k <= 0 || k > len(input.RetrievedContextIDs) {
		k = len(input.RetrievedContextIDs)
	}
	results := map[string]MetricResult{}
	if len(input.ReferenceContextIDs) == 0 {
		for _, name := range []string{"hit_at_k", "recall_at_k", "mrr", "ndcg"} {
			results[name] = MetricResult{Status: MetricSkippedMissingInput, Reason: "reference_context_ids is required"}
		}
	} else {
		relevant := make(map[string]struct{}, len(input.ReferenceContextIDs))
		for _, id := range input.ReferenceContextIDs {
			relevant[id] = struct{}{}
		}
		hits, first, dcg := 0, 0, 0.0
		seen := map[string]struct{}{}
		for i, id := range input.RetrievedContextIDs[:k] {
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			if _, ok := relevant[id]; ok {
				hits++
				if first == 0 {
					first = i + 1
				}
				dcg += 1 / math.Log2(float64(i+2))
			}
		}
		hit := 0.0
		if hits > 0 {
			hit = 1
		}
		recall := float64(hits) / float64(len(relevant))
		mrr := 0.0
		if first > 0 {
			mrr = 1 / float64(first)
		}
		idealCount := len(relevant)
		if idealCount > k {
			idealCount = k
		}
		idcg := 0.0
		for i := 0; i < idealCount; i++ {
			idcg += 1 / math.Log2(float64(i+2))
		}
		ndcg := 0.0
		if idcg > 0 {
			ndcg = dcg / idcg
		}
		results["hit_at_k"] = MetricResult{Status: MetricOK, Value: score(hit)}
		results["recall_at_k"] = MetricResult{Status: MetricOK, Value: score(recall)}
		results["mrr"] = MetricResult{Status: MetricOK, Value: score(mrr)}
		results["ndcg"] = MetricResult{Status: MetricOK, Value: score(ndcg)}
	}
	if input.Citations == nil {
		results["citation_precision"] = MetricResult{Status: MetricSkippedMissingInput, Reason: "citations are required"}
	} else {
		retrieved := map[string]struct{}{}
		for _, id := range input.RetrievedContextIDs {
			retrieved[id] = struct{}{}
		}
		valid := 0
		unique := map[string]struct{}{}
		for _, id := range input.Citations {
			if _, ok := unique[id]; ok {
				continue
			}
			unique[id] = struct{}{}
			if _, ok := retrieved[id]; ok {
				valid++
			}
		}
		value := 1.0
		if len(unique) > 0 {
			value = float64(valid) / float64(len(unique))
		}
		results["citation_precision"] = MetricResult{Status: MetricOK, Value: score(value)}
	}
	abstention := 0.0
	if input.ExpectedAbstention == input.Abstained {
		abstention = 1
	}
	results["abstention_accuracy"] = MetricResult{Status: MetricOK, Value: score(abstention)}
	return results
}

func LooksLikeAbstention(response string) bool {
	lower := strings.ToLower(strings.TrimSpace(response))
	for _, marker := range []string{"无法回答", "资料不足", "不知道", "cannot answer", "insufficient information", "don't know"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func SortedMetricNames(results map[string]MetricResult) []string {
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
