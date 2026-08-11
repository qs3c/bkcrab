package eval

import (
	"context"
	"errors"

	"github.com/qs3c/bkcrab/internal/store"
)

type AnalysisStore interface {
	GetRAGEvalRun(context.Context, string) (*store.RAGEvalRunRecord, error)
	ListRAGEvalMetricResults(context.Context, string, string, int) ([]store.RAGEvalMetricResultRecord, error)
}

type AnalysisService struct{ Store AnalysisStore }
type pairedMetric struct {
	CaseID string
	Result MetricResult
}

func (s AnalysisService) AggregateRunMetric(ctx context.Context, runID, metricName string) (Aggregate, error) {
	if s.Store == nil || runID == "" || metricName == "" {
		return Aggregate{}, errors.New("analysis request is incomplete")
	}
	if _, err := s.Store.GetRAGEvalRun(ctx, runID); err != nil {
		return Aggregate{}, err
	}
	items, err := s.metricResults(ctx, runID, metricName)
	if err != nil {
		return Aggregate{}, err
	}
	values := make([]MetricResult, len(items))
	for index, item := range items {
		values[index] = item.Result
	}
	return AggregateMetric(values), nil
}

func (s AnalysisService) CompareRunMetric(ctx context.Context, baselineID, candidateID, metricName string) (PairedDelta, error) {
	if s.Store == nil || baselineID == "" || candidateID == "" || metricName == "" {
		return PairedDelta{}, errors.New("comparison request is incomplete")
	}
	baselineRun, err := s.Store.GetRAGEvalRun(ctx, baselineID)
	if err != nil {
		return PairedDelta{}, err
	}
	candidateRun, err := s.Store.GetRAGEvalRun(ctx, candidateID)
	if err != nil {
		return PairedDelta{}, err
	}
	if err = ValidateComparableRuns(runComparisonIdentity(baselineRun), runComparisonIdentity(candidateRun)); err != nil {
		return PairedDelta{}, err
	}
	baseline, err := s.metricResults(ctx, baselineID, metricName)
	if err != nil {
		return PairedDelta{}, err
	}
	candidate, err := s.metricResults(ctx, candidateID, metricName)
	if err != nil {
		return PairedDelta{}, err
	}
	baselineByCase := map[string]MetricResult{}
	for _, item := range baseline {
		baselineByCase[item.CaseID] = item.Result
	}
	candidateByCase := map[string]MetricResult{}
	for _, item := range candidate {
		candidateByCase[item.CaseID] = item.Result
	}
	return ComparePaired(baselineByCase, candidateByCase), nil
}

func (s AnalysisService) metricResults(ctx context.Context, runID, metricName string) ([]pairedMetric, error) {
	out := []pairedMetric{}
	cursor := ""
	for {
		items, err := s.Store.ListRAGEvalMetricResults(ctx, runID, cursor, 200)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			if item.MetricName == metricName {
				result := MetricResult{Status: MetricStatus(item.Status), Reason: item.Reason}
				if item.Value.Valid {
					value := item.Value.Float64
					result.Value = &value
				}
				out = append(out, pairedMetric{CaseID: item.CaseID, Result: result})
			}
		}
		last := items[len(items)-1]
		cursor = last.CaseID + ":" + last.MetricName
	}
	return out, nil
}

func runComparisonIdentity(run *store.RAGEvalRunRecord) RunComparisonIdentity {
	return RunComparisonIdentity{DatasetVersionID: run.DatasetVersionID, Mode: RunMode(run.Mode), GenerationID: run.IndexGenerationID}
}
