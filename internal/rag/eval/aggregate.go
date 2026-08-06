package eval

import (
	"math"
	"sort"
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
		} else {
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
