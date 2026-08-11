package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/store"
)

type Aggregate struct {
	Count        int      `json:"count"`
	ScoredCount  int      `json:"scoredCount"`
	SkippedCount int      `json:"skippedCount"`
	ErrorCount   int      `json:"errorCount"`
	Mean         *float64 `json:"mean,omitempty"`
	Median       *float64 `json:"median,omitempty"`
	P50          *float64 `json:"p50,omitempty"`
	P95          *float64 `json:"p95,omitempty"`
}

func AggregateMetric(results []MetricResult) Aggregate {
	aggregate := Aggregate{Count: len(results)}
	values := make([]float64, 0, len(results))
	for _, result := range results {
		switch result.Status {
		case MetricOK:
			if result.Value != nil && !math.IsNaN(*result.Value) && !math.IsInf(*result.Value, 0) {
				values = append(values, *result.Value)
			} else {
				aggregate.ErrorCount++
			}
		case MetricError:
			aggregate.ErrorCount++
		default:
			aggregate.SkippedCount++
		}
	}
	aggregate.ScoredCount = len(values)
	if len(values) == 0 {
		return aggregate
	}
	sort.Float64s(values)
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	median := percentile(values, .5)
	p50 := median
	p95 := percentile(values, .95)
	aggregate.Mean = &mean
	aggregate.Median = &median
	aggregate.P50 = &p50
	aggregate.P95 = &p95
	return aggregate
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := p * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	weight := position - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

type PairedDelta struct {
	Pairs            int       `json:"pairs"`
	MissingBaseline  []string  `json:"missingBaseline"`
	MissingCandidate []string  `json:"missingCandidate"`
	Baseline         Aggregate `json:"baseline"`
	Candidate        Aggregate `json:"candidate"`
	AbsoluteDelta    *float64  `json:"absoluteDelta,omitempty"`
	RelativeDelta    *float64  `json:"relativeDelta,omitempty"`
}

func ComparePaired(baseline, candidate map[string]MetricResult) PairedDelta {
	ids := make([]string, 0)
	for id := range baseline {
		if _, ok := candidate[id]; ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	b := make([]MetricResult, 0, len(ids))
	c := make([]MetricResult, 0, len(ids))
	out := PairedDelta{}
	for id := range baseline {
		if _, ok := candidate[id]; !ok {
			out.MissingCandidate = append(out.MissingCandidate, id)
		}
	}
	for id := range candidate {
		if _, ok := baseline[id]; !ok {
			out.MissingBaseline = append(out.MissingBaseline, id)
		}
	}
	sort.Strings(out.MissingBaseline)
	sort.Strings(out.MissingCandidate)
	for _, id := range ids {
		if baseline[id].Status == MetricOK && baseline[id].Value != nil && candidate[id].Status == MetricOK && candidate[id].Value != nil {
			b = append(b, baseline[id])
			c = append(c, candidate[id])
		}
	}
	out.Pairs = len(b)
	out.Baseline = AggregateMetric(b)
	out.Candidate = AggregateMetric(c)
	if out.Baseline.Mean != nil && out.Candidate.Mean != nil {
		delta := *out.Candidate.Mean - *out.Baseline.Mean
		out.AbsoluteDelta = &delta
		if *out.Baseline.Mean != 0 {
			relative := delta / math.Abs(*out.Baseline.Mean)
			out.RelativeDelta = &relative
		}
	}
	return out
}

type RunComparisonIdentity struct {
	DatasetVersionID string
	Mode             RunMode
	GenerationID     string
}

// ValidateComparableRuns prevents attractive but invalid deltas. All paired
// comparisons share one immutable dataset version; ONLINE_ONLY runs also share
// the exact physical generation.
func ValidateComparableRuns(baseline, candidate RunComparisonIdentity) error {
	if strings.TrimSpace(baseline.DatasetVersionID) == "" || baseline.DatasetVersionID != candidate.DatasetVersionID {
		return errors.New("paired comparison requires the same dataset version")
	}
	if !baseline.Mode.Valid() || !candidate.Mode.Valid() || baseline.Mode != candidate.Mode {
		return errors.New("paired comparison requires the same run mode")
	}
	if baseline.Mode == RunModeOnlineOnly && (baseline.GenerationID == "" || baseline.GenerationID != candidate.GenerationID) {
		return errors.New("online-only comparison requires the same READY generation")
	}
	return nil
}

type ThresholdObservation struct {
	CaseID      string
	RerankScore *float64
	Relevant    *bool
}

type ThresholdPoint struct {
	Threshold     float64  `json:"threshold"`
	Selected      int      `json:"selected"`
	Labeled       int      `json:"labeled"`
	TruePositive  int      `json:"truePositive"`
	FalsePositive int      `json:"falsePositive"`
	FalseNegative int      `json:"falseNegative"`
	Precision     *float64 `json:"precision,omitempty"`
	Recall        *float64 `json:"recall,omitempty"`
}

// ThresholdCurve uses saved reranker scores only. Precision/recall stay nil
// when no relevance labels exist; missing labels are never treated as false.
func ThresholdCurve(observations []ThresholdObservation, thresholds []float64) ([]ThresholdPoint, error) {
	if len(thresholds) == 0 {
		seen := map[float64]struct{}{0: {}}
		for _, item := range observations {
			if item.RerankScore != nil {
				seen[*item.RerankScore] = struct{}{}
			}
		}
		for value := range seen {
			thresholds = append(thresholds, value)
		}
	}
	for _, threshold := range thresholds {
		if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
			return nil, fmt.Errorf("invalid threshold %v", threshold)
		}
	}
	sort.Float64s(thresholds)
	out := make([]ThresholdPoint, 0, len(thresholds))
	for _, threshold := range thresholds {
		point := ThresholdPoint{Threshold: threshold}
		for _, item := range observations {
			selected := item.RerankScore != nil && *item.RerankScore >= threshold
			if selected {
				point.Selected++
			}
			if item.Relevant == nil {
				continue
			}
			point.Labeled++
			if selected && *item.Relevant {
				point.TruePositive++
			}
			if selected && !*item.Relevant {
				point.FalsePositive++
			}
			if !selected && *item.Relevant {
				point.FalseNegative++
			}
		}
		if point.Labeled > 0 {
			if denominator := point.TruePositive + point.FalsePositive; denominator > 0 {
				value := float64(point.TruePositive) / float64(denominator)
				point.Precision = &value
			}
			if denominator := point.TruePositive + point.FalseNegative; denominator > 0 {
				value := float64(point.TruePositive) / float64(denominator)
				point.Recall = &value
			}
		}
		out = append(out, point)
	}
	return out, nil
}

// ThresholdObservationsFromSavedResults reconstructs the curve input from the
// bounded hit projection saved by the runner. Reference IDs are the only
// relevance labels; absent labels remain nil.
func ThresholdObservationsFromSavedResults(results []store.RAGEvalCaseResultRecord, cases map[string]Case) ([]ThresholdObservation, error) {
	out := []ThresholdObservation{}
	for _, result := range results {
		var trace struct {
			Hits []struct {
				ContextID   string   `json:"contextId"`
				RerankScore *float64 `json:"rerankScore"`
			} `json:"hits"`
		}
		if err := json.Unmarshal([]byte(result.SearchTraceJSON), &trace); err != nil {
			return nil, fmt.Errorf("decode saved search trace for %s: %w", result.CaseID, err)
		}
		item, exists := cases[result.CaseID]
		labels := map[string]struct{}{}
		if exists {
			for _, id := range item.ReferenceContextIDs {
				labels[id] = struct{}{}
			}
		}
		for index, hit := range trace.Hits {
			observation := ThresholdObservation{CaseID: fmt.Sprintf("%s:%d", result.CaseID, index), RerankScore: hit.RerankScore}
			if exists && len(item.ReferenceContextIDs) > 0 {
				_, relevant := labels[hit.ContextID]
				observation.Relevant = &relevant
			}
			out = append(out, observation)
		}
	}
	return out, nil
}

const (
	MaxSliceKeyBytes           = 64
	MaxSliceValueBytes         = 128
	DefaultMaxSliceCardinality = 100
)

type SliceCase struct {
	CaseID   string
	Tags     []string
	Metadata map[string]any
}
type SliceSpec struct{ Key, Value string }

// BuildSlices supports the closed "tag" key plus explicitly allowlisted
// metadata.<key> fields, and rejects requests that would create high-cardinality
// response/DB work.
func BuildSlices(cases []SliceCase, specs []SliceSpec, allowedMetadata map[string]struct{}, maxCardinality int) (map[SliceSpec][]string, error) {
	if maxCardinality <= 0 {
		maxCardinality = DefaultMaxSliceCardinality
	}
	if len(specs) > maxCardinality {
		return nil, errors.New("slice cardinality limit exceeded")
	}
	out := make(map[SliceSpec][]string, len(specs))
	seen := map[SliceSpec]struct{}{}
	for _, spec := range specs {
		spec.Key = strings.TrimSpace(spec.Key)
		spec.Value = strings.TrimSpace(spec.Value)
		if spec.Key == "" || spec.Value == "" || len(spec.Key) > MaxSliceKeyBytes || len(spec.Value) > MaxSliceValueBytes {
			return nil, errors.New("slice key/value is invalid")
		}
		if spec.Key != "tag" {
			if !strings.HasPrefix(spec.Key, "metadata.") {
				return nil, errors.New("slice key is not allowlisted")
			}
			key := strings.TrimPrefix(spec.Key, "metadata.")
			if _, ok := allowedMetadata[key]; !ok {
				return nil, errors.New("metadata slice key is not allowlisted")
			}
		}
		if _, duplicate := seen[spec]; duplicate {
			continue
		}
		seen[spec] = struct{}{}
		for _, item := range cases {
			matched := false
			if spec.Key == "tag" {
				for _, tag := range item.Tags {
					if tag == spec.Value {
						matched = true
						break
					}
				}
			} else {
				key := strings.TrimPrefix(spec.Key, "metadata.")
				value, ok := item.Metadata[key]
				matched = ok && fmt.Sprint(value) == spec.Value
			}
			if matched {
				out[spec] = append(out[spec], item.CaseID)
			}
		}
	}
	return out, nil
}

type ConfirmationEligibility struct {
	Eligible bool     `json:"eligible"`
	Reasons  []string `json:"reasons,omitempty"`
}

func CheckConfirmationEligibility(candidate RunComparisonIdentity, successful bool, completedCases, minimumSamples int) ConfirmationEligibility {
	out := ConfirmationEligibility{Eligible: true}
	if err := ValidateComparableRuns(candidate, candidate); err != nil {
		out.Reasons = append(out.Reasons, err.Error())
	}
	if !successful {
		out.Reasons = append(out.Reasons, "candidate run is not successful")
	}
	if minimumSamples < 1 {
		minimumSamples = 1
	}
	if completedCases < minimumSamples {
		out.Reasons = append(out.Reasons, fmt.Sprintf("requires at least %d completed cases", minimumSamples))
	}
	out.Eligible = len(out.Reasons) == 0
	return out
}
