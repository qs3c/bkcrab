package eval

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type DeterministicInput struct {
	RetrievedContextIDs  []string
	ReferenceContextIDs  []string
	RetrievedDocumentIDs []string
	ReferenceDocumentIDs []string
	Response             string
	ExpectedAbstention   bool
	Abstained            bool
}

func DeterministicMetrics(input DeterministicInput, k int) map[string]MetricResult {
	results := map[string]MetricResult{}
	mergeRankingMetrics(results, input.RetrievedContextIDs, input.ReferenceContextIDs, k,
		[4]string{"hit_at_k", "recall_at_k", "mrr", "ndcg"}, "reference_context_ids is required")
	mergeRankingMetrics(results, input.RetrievedDocumentIDs, input.ReferenceDocumentIDs, k,
		[4]string{"doc_hit_at_k", "doc_recall_at_k", "doc_mrr", "doc_ndcg"}, "reference_document_ids is required")
	precision, coverage := citationMetrics(input.Response, input.RetrievedContextIDs)
	results["citation_precision"] = precision
	results["citation_coverage"] = coverage
	abstention := 0.0
	if input.ExpectedAbstention == input.Abstained {
		abstention = 1
	}
	results["abstention_accuracy"] = MetricResult{Status: MetricOK, Value: score(abstention)}
	return results
}

func mergeRankingMetrics(results map[string]MetricResult, retrieved, references []string, k int, names [4]string, missingReason string) {
	relevant := make(map[string]struct{}, len(references))
	for _, id := range references {
		if id = strings.TrimSpace(id); id != "" {
			relevant[id] = struct{}{}
		}
	}
	if len(relevant) == 0 {
		for _, name := range names {
			results[name] = MetricResult{Status: MetricSkippedMissingInput, Reason: missingReason}
		}
		return
	}
	if k <= 0 || k > len(retrieved) {
		k = len(retrieved)
	}
	hits, first, dcg := 0, 0, 0.0
	seen := map[string]struct{}{}
	for i, id := range retrieved[:k] {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
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
	results[names[0]] = MetricResult{Status: MetricOK, Value: score(hit)}
	results[names[1]] = MetricResult{Status: MetricOK, Value: score(recall)}
	results[names[2]] = MetricResult{Status: MetricOK, Value: score(mrr)}
	results[names[3]] = MetricResult{Status: MetricOK, Value: score(ndcg)}
}

var citationPattern = regexp.MustCompile(`\[([0-9]+)\]`)
var claimSeparator = regexp.MustCompile(`[.!?。！？\n]+`)

// citationMetrics implements the v1 deterministic [n] contract. Precision is
// the fraction of unique numeric citations that address an existing, non-empty
// retrieved context ID. Coverage is the fraction of non-empty sentence-like
// claims containing at least one valid citation. Other syntaxes are ignored.
func citationMetrics(response string, contextIDs []string) (MetricResult, MetricResult) {
	if len(contextIDs) == 0 {
		skipped := MetricResult{Status: MetricSkippedMissingInput, Reason: "retrieved_context_ids is required"}
		return skipped, skipped
	}
	validIndex := func(index int) bool {
		return index > 0 && index <= len(contextIDs) && strings.TrimSpace(contextIDs[index-1]) != ""
	}
	unique, valid, invalid := map[int]struct{}{}, 0, []int{}
	for _, match := range citationPattern.FindAllStringSubmatch(response, -1) {
		index, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if _, duplicate := unique[index]; duplicate {
			continue
		}
		unique[index] = struct{}{}
		if validIndex(index) {
			valid++
		} else {
			invalid = append(invalid, index)
		}
	}
	reason := ""
	if len(invalid) > 0 {
		sort.Ints(invalid)
		reason = fmt.Sprintf("out-of-range citations: %v", invalid)
	}
	precision := MetricResult{Status: MetricSkippedMissingInput, Reason: "response has no [n] citations"}
	if len(unique) > 0 {
		precision = MetricResult{Status: MetricOK, Value: score(float64(valid) / float64(len(unique))), Reason: reason}
	}
	claims, covered := 0, 0
	for _, claim := range claimSeparator.Split(response, -1) {
		claim = strings.TrimSpace(claim)
		if claim == "" || strings.TrimSpace(citationPattern.ReplaceAllString(claim, "")) == "" {
			continue
		}
		claims++
		for _, match := range citationPattern.FindAllStringSubmatch(claim, -1) {
			index, err := strconv.Atoi(match[1])
			if err == nil && validIndex(index) {
				covered++
				break
			}
		}
	}
	coverage := MetricResult{Status: MetricSkippedMissingInput, Reason: "response claims are required"}
	if claims > 0 {
		coverage = MetricResult{Status: MetricOK, Value: score(float64(covered) / float64(claims)), Reason: reason}
	}
	return precision, coverage
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
