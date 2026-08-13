package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

func float(value float64) *float64 { return &value }
func boolean(value bool) *bool     { return &value }

func TestAggregateExcludesSkippedAndErrorsFromScoredDenominator(t *testing.T) {
	results := []MetricResult{{Status: MetricOK, Value: float(.2)}, {Status: MetricOK, Value: float(.8)}, {Status: MetricSkippedMissingInput}, {Status: MetricError}}
	aggregate := AggregateMetric(results)
	if aggregate.Count != 4 || aggregate.ScoredCount != 2 || aggregate.SkippedCount != 1 || aggregate.ErrorCount != 1 {
		t.Fatalf("aggregate=%+v", aggregate)
	}
	if aggregate.Mean == nil || *aggregate.Mean != .5 || aggregate.Median == nil || *aggregate.Median != .5 {
		t.Fatalf("aggregate=%+v", aggregate)
	}
	allSkipped := AggregateMetric([]MetricResult{{Status: MetricSkippedMissingInput}, {Status: MetricSkippedMissingInput}})
	if allSkipped.Mean != nil || allSkipped.ScoredCount != 0 || allSkipped.SkippedCount != 2 {
		t.Fatalf("all skipped=%+v", allSkipped)
	}
	single := AggregateMetric([]MetricResult{{Status: MetricOK, Value: float(.7)}})
	if single.P95 == nil || *single.P95 != .7 {
		t.Fatalf("single=%+v", single)
	}
}

func TestPairedComparisonListsMissingCasesAndValidatesIdentity(t *testing.T) {
	baseline := map[string]MetricResult{"a": {Status: MetricOK, Value: float(.4)}, "only-base": {Status: MetricOK, Value: float(.5)}}
	candidate := map[string]MetricResult{"a": {Status: MetricOK, Value: float(.7)}, "only-candidate": {Status: MetricOK, Value: float(.8)}}
	delta := ComparePaired(baseline, candidate)
	if delta.Pairs != 1 || len(delta.MissingBaseline) != 1 || delta.MissingBaseline[0] != "only-candidate" || len(delta.MissingCandidate) != 1 || delta.MissingCandidate[0] != "only-base" {
		t.Fatalf("delta=%+v", delta)
	}
	identity := RunComparisonIdentity{DatasetVersionID: "dataset", Mode: RunModeOnlineOnly, GenerationID: "generation"}
	if err := ValidateComparableRuns(identity, identity); err != nil {
		t.Fatal(err)
	}
	changed := identity
	changed.GenerationID = "other"
	if err := ValidateComparableRuns(identity, changed); err == nil {
		t.Fatal("different online generation accepted")
	}
	changed = identity
	changed.DatasetVersionID = "other"
	if err := ValidateComparableRuns(identity, changed); err == nil {
		t.Fatal("different dataset accepted")
	}
}

func TestThresholdCurveDoesNotInventUnlabeledPrecisionRecall(t *testing.T) {
	curve, err := ThresholdCurve([]ThresholdObservation{{CaseID: "unlabeled", RerankScore: float(.9)}}, []float64{.5})
	if err != nil {
		t.Fatal(err)
	}
	if curve[0].Precision != nil || curve[0].Recall != nil || curve[0].Selected != 1 {
		t.Fatalf("curve=%+v", curve)
	}
	curve, err = ThresholdCurve([]ThresholdObservation{{CaseID: "tp", RerankScore: float(.9), Relevant: boolean(true)}, {CaseID: "fp", RerankScore: float(.8), Relevant: boolean(false)}, {CaseID: "fn", RerankScore: float(.2), Relevant: boolean(true)}}, []float64{.5})
	if err != nil {
		t.Fatal(err)
	}
	point := curve[0]
	if point.Precision == nil || *point.Precision != .5 || point.Recall == nil || *point.Recall != .5 || point.TruePositive != 1 || point.FalsePositive != 1 || point.FalseNegative != 1 {
		t.Fatalf("point=%+v", point)
	}
}

func TestThresholdObservationsUseSavedHitScoresAndReferenceLabels(t *testing.T) {
	results := []store.RAGEvalCaseResultRecord{{CaseID: "case", SearchTraceJSON: `{"hits":[{"contextId":"doc:1","rerankScore":0.9},{"contextId":"doc:2","rerankScore":0.3}]}`}}
	observations, err := ThresholdObservationsFromSavedResults(results, map[string]Case{"case": {ID: "case", ReferenceContextIDs: []string{"doc:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 || observations[0].RerankScore == nil || *observations[0].RerankScore != .9 || observations[0].Relevant == nil || !*observations[0].Relevant || observations[1].Relevant == nil || *observations[1].Relevant {
		t.Fatalf("observations=%+v", observations)
	}
	unlabeled, err := ThresholdObservationsFromSavedResults(results, map[string]Case{"case": {ID: "case"}})
	if err != nil || unlabeled[0].Relevant != nil {
		t.Fatalf("unlabeled=%+v err=%v", unlabeled, err)
	}
}

func TestBoundedAllowlistedSlicesAndConfirmationEligibility(t *testing.T) {
	cases := []SliceCase{{CaseID: "a", Tags: []string{"finance"}, Metadata: map[string]any{"region": "cn", "request_id": "high-cardinality"}}, {CaseID: "b", Tags: []string{"support"}, Metadata: map[string]any{"region": "cn"}}}
	slices, err := BuildSlices(cases, []SliceSpec{{Key: "tag", Value: "finance"}, {Key: "metadata.region", Value: "cn"}}, map[string]struct{}{"region": {}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(slices[SliceSpec{Key: "tag", Value: "finance"}]) != 1 || len(slices[SliceSpec{Key: "metadata.region", Value: "cn"}]) != 2 {
		t.Fatalf("slices=%v", slices)
	}
	if _, err = BuildSlices(cases, []SliceSpec{{Key: "metadata.request_id", Value: "x"}}, map[string]struct{}{"region": {}}, 10); err == nil {
		t.Fatal("non-allowlisted metadata accepted")
	}
	if _, err = BuildSlices(cases, []SliceSpec{{Key: "tag", Value: "a"}, {Key: "tag", Value: "b"}}, nil, 1); err == nil {
		t.Fatal("cardinality limit ignored")
	}
	identity := RunComparisonIdentity{DatasetVersionID: "dataset", Mode: RunModeFull}
	if got := CheckConfirmationEligibility(identity, true, 20, 10); !got.Eligible {
		t.Fatalf("eligible=%+v", got)
	}
	if got := CheckConfirmationEligibility(identity, false, 2, 10); got.Eligible || len(got.Reasons) != 2 {
		t.Fatalf("ineligible=%+v", got)
	}
}

type exportAuthorizer struct {
	called int
	err    error
}

func (a *exportAuthorizer) AuthorizeRAGEvalExport(_ context.Context, actor string, _ *store.RAGEvalRunRecord) error {
	a.called++
	if actor != "admin" {
		return errors.New("forbidden")
	}
	return a.err
}

type exportTraceSink struct{ calls int }

func (s *exportTraceSink) PutRAGEvalTrace(_ context.Context, runID, caseID, kind string, _ json.RawMessage) (string, error) {
	s.calls++
	return "trace://" + runID + "/" + caseID + "/" + kind, nil
}

func TestExportReauthorizesStreamsJSONAndReferencesLargeTraces(t *testing.T) {
	runner, st, _, _, runID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
	if err := runner.Run(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	authorizer := &exportAuthorizer{}
	sink := &exportTraceSink{}
	service := ExportService{Store: st, Authorizer: authorizer, TraceSink: sink, InlineTraceBytes: 1}
	var output bytes.Buffer
	if err := service.Export(context.Background(), "admin", runID, ExportJSON, true, &output); err != nil {
		t.Fatal(err)
	}
	if authorizer.called != 1 || sink.calls != 2 {
		t.Fatalf("auth=%d sink=%d", authorizer.called, sink.calls)
	}
	var records []ExportRecord
	if err := json.Unmarshal(output.Bytes(), &records); err != nil {
		t.Fatalf("invalid streaming JSON: %v\n%s", err, output.String())
	}
	if len(records) != 2 || records[0].SearchTraceRef == "" || records[0].SearchTrace != nil {
		t.Fatalf("records=%+v", records)
	}
	output.Reset()
	if err := service.Export(context.Background(), "admin", runID, ExportCSV, false, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "run_id,case_id") || !strings.Contains(output.String(), "faithfulness") {
		t.Fatalf("csv=%s", output.String())
	}
	if err := service.Export(context.Background(), "user", runID, ExportJSON, false, &output); err == nil {
		t.Fatal("export authorization was not rechecked")
	}
	analysis := AnalysisService{Store: st}
	aggregate, err := analysis.AggregateRunMetric(context.Background(), runID, "faithfulness")
	if err != nil || aggregate.ScoredCount != 1 {
		t.Fatalf("aggregate=%+v err=%v", aggregate, err)
	}
	delta, err := analysis.CompareRunMetric(context.Background(), runID, runID, "faithfulness")
	if err != nil || delta.Pairs != 1 {
		t.Fatalf("delta=%+v err=%v", delta, err)
	}
	if got := safeCSVCell("=HYPERLINK(\"bad\")"); !strings.HasPrefix(got, "'") {
		t.Fatalf("unsafe csv cell=%q", got)
	}
}
