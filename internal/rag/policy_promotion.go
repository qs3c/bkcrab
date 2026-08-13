package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/store"
)

var (
	ErrPromotionGatesUnconfigured = errors.New("RAG policy promotion gates are not configured")
	ErrPromotionGateFailed        = errors.New("RAG policy promotion gate failed")
	ErrPolicyActivationConflict   = errors.New("RAG policy active revision changed concurrently")
)

type RuntimePromotionStore interface {
	GetRAGEvalRun(context.Context, string) (*store.RAGEvalRunRecord, error)
	ListRAGEvalMetricResults(context.Context, string, string, int) ([]store.RAGEvalMetricResultRecord, error)
	ListRAGEvalCaseResults(context.Context, string, string, int) ([]store.RAGEvalCaseResultRecord, error)
	RAGEvalUsageTotals(context.Context, string) (int64, float64, error)
	CreateRAGPolicy(context.Context, *store.RAGPolicyRecord) error
	ActivateRAGPolicy(context.Context, string, int64, int64, string, string, string, string) (bool, error)
	ActiveRAGPolicy(context.Context, string) (*store.RAGPolicyRecord, error)
	GetRAGPolicy(context.Context, string, int64) (*store.RAGPolicyRecord, error)
}

type PromotionGates struct {
	MinimumMetricMean    map[string]float64 `json:"minimumMetricMean"`
	MinimumScoredCases   int                `json:"minimumScoredCases"`
	MaximumCaseErrorRate float64            `json:"maximumCaseErrorRate"`
	MaximumP95LatencyMS  int64              `json:"maximumP95LatencyMs"`
	MaximumCostUSD       float64            `json:"maximumCostUsd"`
}

func (g PromotionGates) Validate() error {
	if g.MinimumScoredCases < 1 || len(g.MinimumMetricMean) == 0 {
		return ErrPromotionGatesUnconfigured
	}
	for metric, value := range g.MinimumMetricMean {
		if strings.TrimSpace(metric) == "" || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
			return errors.New("invalid quality promotion gate")
		}
	}
	if math.IsNaN(g.MaximumCaseErrorRate) || math.IsInf(g.MaximumCaseErrorRate, 0) || g.MaximumCaseErrorRate < 0 || g.MaximumCaseErrorRate > 1 || g.MaximumP95LatencyMS < 0 || math.IsNaN(g.MaximumCostUSD) || math.IsInf(g.MaximumCostUSD, 0) || g.MaximumCostUSD < 0 {
		return errors.New("invalid performance/cost promotion gate")
	}
	return nil
}

type RuntimePromotionEnvironment struct {
	RewriteEnabled, HyDEEnabled, RerankerEnabled bool
	AnswerModel                                  string
}
type RuntimePromotionRequest struct {
	RunID, ProfileID, ConfirmationRunID, ActorID, Note string
	Fields                                             []string
}
type RuntimePromotionResult struct {
	Revision   *store.RAGPolicyRecord
	GateReport PromotionGateReport
}
type IngestionPromotionRequest struct {
	RunID, ProfileID, ConfirmationRunID, ActorID, Note string
}
type IngestionPromotionResult struct {
	Revision   *store.RAGPolicyRecord
	GateReport PromotionGateReport
}
type PromotionGateReport struct {
	Passed        bool               `json:"passed"`
	Reasons       []string           `json:"reasons,omitempty"`
	MetricMeans   map[string]float64 `json:"metricMeans"`
	ScoredCases   int                `json:"scoredCases"`
	CaseErrorRate float64            `json:"caseErrorRate"`
	P95LatencyMS  int64              `json:"p95LatencyMs"`
	CostUSD       float64            `json:"costUsd"`
}

type PolicyPromotionService struct {
	Store              RuntimePromotionStore
	Snapshot           *RuntimePolicySnapshot
	Gates              PromotionGates
	Environment        RuntimePromotionEnvironment
	ResolveEnvironment func(context.Context, string) (RuntimePromotionEnvironment, error)
}

var runtimePublishFields = map[string]func(*config.RAGRuntimePolicyData, config.RAGRuntimePolicyData){
	"topN": func(dst *config.RAGRuntimePolicyData, src config.RAGRuntimePolicyData) { dst.TopN = src.TopN },
	"candidateTopK": func(dst *config.RAGRuntimePolicyData, src config.RAGRuntimePolicyData) {
		dst.CandidateTopK = src.CandidateTopK
	},
	"minScore": func(dst *config.RAGRuntimePolicyData, src config.RAGRuntimePolicyData) { dst.MinScore = src.MinScore },
	"temperature": func(dst *config.RAGRuntimePolicyData, src config.RAGRuntimePolicyData) {
		dst.Temperature = src.Temperature
	},
	"maxTokens": func(dst *config.RAGRuntimePolicyData, src config.RAGRuntimePolicyData) { dst.MaxTokens = src.MaxTokens },
	"ragPromptBundleVersion": func(dst *config.RAGRuntimePolicyData, src config.RAGRuntimePolicyData) {
		dst.RAGPromptBundleVersion = src.RAGPromptBundleVersion
	},
}

func RuntimePublishFields() []string {
	out := make([]string, 0, len(runtimePublishFields))
	for field := range runtimePublishFields {
		out = append(out, field)
	}
	sort.Strings(out)
	return out
}

func (s *PolicyPromotionService) PromoteRuntime(ctx context.Context, request RuntimePromotionRequest) (RuntimePromotionResult, error) {
	if s == nil || s.Store == nil || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.ProfileID) == "" || strings.TrimSpace(request.ConfirmationRunID) == "" {
		return RuntimePromotionResult{}, errors.New("complete runtime promotion references are required")
	}
	if err := s.Gates.Validate(); err != nil {
		return RuntimePromotionResult{}, err
	}
	fields, err := canonicalRuntimeFields(request.Fields)
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	source, sourceSnapshot, err := s.loadSuccessfulRun(ctx, request.RunID, request.ProfileID)
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	confirmation, confirmationSnapshot, err := s.loadSuccessfulRun(ctx, request.ConfirmationRunID, request.ProfileID)
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	if confirmation.Mode != store.RAGEvalRunModeOnlineOnly || source.DatasetVersionID != confirmation.DatasetVersionID || source.IndexGenerationID == "" || source.IndexGenerationID != confirmation.IndexGenerationID {
		return RuntimePromotionResult{}, errors.New("runtime confirmation must use the same dataset and generation in ONLINE_ONLY mode")
	}
	active, err := s.Store.ActiveRAGPolicy(ctx, store.RAGPolicyRuntime)
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	activeData, err := config.DecodeRAGRuntimePolicy([]byte(active.PolicyJSON))
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	next := activeData
	for _, field := range fields {
		runtimePublishFields[field](&next, sourceSnapshot.Profile.Runtime)
	}
	next.Version = active.Version + 1
	environment := s.Environment
	if s.ResolveEnvironment != nil {
		environment, err = s.ResolveEnvironment(ctx, request.ActorID)
		if err != nil {
			return RuntimePromotionResult{}, err
		}
	}
	if !sameRuntimeExceptVersion(next, confirmationSnapshot.Profile.Runtime) || !environmentMatches(environment, confirmationSnapshot.Profile) {
		return RuntimePromotionResult{}, errors.New("confirmation run includes unpublished parameter differences")
	}
	report, err := s.evaluateGates(ctx, confirmation.ID)
	if err != nil {
		return RuntimePromotionResult{GateReport: report}, err
	}
	if !report.Passed {
		return RuntimePromotionResult{GateReport: report}, fmt.Errorf("%w: %s", ErrPromotionGateFailed, strings.Join(report.Reasons, "; "))
	}
	encoded, _ := jsonMarshal(next)
	fingerprint, err := rageval.Fingerprint(next)
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	revision := &store.RAGPolicyRecord{Kind: store.RAGPolicyRuntime, Version: next.Version, PolicyJSON: string(encoded), Fingerprint: fingerprint, SourceEvalRunID: confirmation.ID, CreatedBy: request.ActorID, Note: request.Note}
	if err = s.Store.CreateRAGPolicy(ctx, revision); err != nil {
		return RuntimePromotionResult{}, err
	}
	activated, err := s.Store.ActivateRAGPolicy(ctx, store.RAGPolicyRuntime, active.Version, revision.Version, request.ActorID, confirmation.ID, request.Note, store.RAGPolicyAuditPublish)
	if err != nil {
		return RuntimePromotionResult{}, err
	}
	if !activated {
		return RuntimePromotionResult{}, ErrPolicyActivationConflict
	}
	revision.Status = store.RAGPolicyActive
	if s.Snapshot != nil {
		if err = s.Snapshot.Publish(next); err != nil {
			return RuntimePromotionResult{}, err
		}
	}
	return RuntimePromotionResult{Revision: revision, GateReport: report}, nil
}

func (s *PolicyPromotionService) RollbackRuntime(ctx context.Context, expected, target int64, actor, note string) error {
	if s == nil || s.Store == nil || expected <= 0 || target <= 0 || actor == "" {
		return errors.New("complete rollback request is required")
	}
	record, err := s.Store.GetRAGPolicy(ctx, store.RAGPolicyRuntime, target)
	if err != nil {
		return err
	}
	data, err := config.DecodeRAGRuntimePolicy([]byte(record.PolicyJSON))
	if err != nil {
		return err
	}
	ok, err := s.Store.ActivateRAGPolicy(ctx, store.RAGPolicyRuntime, expected, target, actor, "", note, store.RAGPolicyAuditRollback)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPolicyActivationConflict
	}
	if s.Snapshot != nil {
		return s.Snapshot.Publish(data)
	}
	return nil
}

// PromoteIngestion publishes the complete immutable ingestion contract from a
// confirmed FULL_PIPELINE experiment. The caller supplies references only;
// publish-time configuration overrides are deliberately impossible.
func (s *PolicyPromotionService) PromoteIngestion(ctx context.Context, request IngestionPromotionRequest) (IngestionPromotionResult, error) {
	if s == nil || s.Store == nil || strings.TrimSpace(request.ActorID) == "" || strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(request.ProfileID) == "" || strings.TrimSpace(request.ConfirmationRunID) == "" {
		return IngestionPromotionResult{}, errors.New("complete ingestion promotion references are required")
	}
	if err := s.Gates.Validate(); err != nil {
		return IngestionPromotionResult{}, err
	}
	source, sourceSnapshot, err := s.loadSuccessfulRun(ctx, request.RunID, request.ProfileID)
	if err != nil {
		return IngestionPromotionResult{}, err
	}
	confirmation, confirmationSnapshot, err := s.loadSuccessfulRun(ctx, request.ConfirmationRunID, request.ProfileID)
	if err != nil {
		return IngestionPromotionResult{}, err
	}
	if source.Mode != store.RAGEvalRunModeFullPipeline || confirmation.Mode != store.RAGEvalRunModeFullPipeline || source.DatasetVersionID != confirmation.DatasetVersionID || strings.TrimSpace(confirmation.IndexGenerationID) == "" {
		return IngestionPromotionResult{}, errors.New("ingestion confirmation must use the same dataset in FULL_PIPELINE mode")
	}
	candidate := sourceSnapshot.Profile.Ingestion
	confirmed := confirmationSnapshot.Profile.Ingestion
	candidate.Version, confirmed.Version = 0, 0
	if candidate != confirmed {
		return IngestionPromotionResult{}, errors.New("confirmation run does not match the complete ingestion policy")
	}
	report, err := s.evaluateGates(ctx, confirmation.ID)
	if err != nil {
		return IngestionPromotionResult{GateReport: report}, err
	}
	if !report.Passed {
		return IngestionPromotionResult{GateReport: report}, fmt.Errorf("%w: %s", ErrPromotionGateFailed, strings.Join(report.Reasons, "; "))
	}
	active, err := s.Store.ActiveRAGPolicy(ctx, store.RAGPolicyIngestion)
	if err != nil {
		return IngestionPromotionResult{}, err
	}
	next := sourceSnapshot.Profile.Ingestion
	next.Version = active.Version + 1
	if err = next.Validate(); err != nil {
		return IngestionPromotionResult{}, err
	}
	encoded, _ := jsonMarshal(next)
	fingerprint, err := rageval.Fingerprint(next)
	if err != nil {
		return IngestionPromotionResult{}, err
	}
	revision := &store.RAGPolicyRecord{Kind: store.RAGPolicyIngestion, Version: next.Version, PolicyJSON: string(encoded), Fingerprint: fingerprint, SourceEvalRunID: confirmation.ID, CreatedBy: request.ActorID, Note: request.Note}
	if err = s.Store.CreateRAGPolicy(ctx, revision); err != nil {
		return IngestionPromotionResult{}, err
	}
	activated, err := s.Store.ActivateRAGPolicy(ctx, store.RAGPolicyIngestion, active.Version, revision.Version, request.ActorID, confirmation.ID, request.Note, store.RAGPolicyAuditPublish)
	if err != nil {
		return IngestionPromotionResult{}, err
	}
	if !activated {
		return IngestionPromotionResult{}, ErrPolicyActivationConflict
	}
	revision.Status = store.RAGPolicyActive
	return IngestionPromotionResult{Revision: revision, GateReport: report}, nil
}

func (s *PolicyPromotionService) loadSuccessfulRun(ctx context.Context, runID, profileID string) (*store.RAGEvalRunRecord, rageval.ExecutionSnapshot, error) {
	run, err := s.Store.GetRAGEvalRun(ctx, runID)
	if err != nil {
		return nil, rageval.ExecutionSnapshot{}, err
	}
	if run.Status != store.RAGEvalRunSucceeded || run.ProfileID != profileID {
		return nil, rageval.ExecutionSnapshot{}, errors.New("promotion requires a successful run for the referenced profile")
	}
	var snapshot rageval.ExecutionSnapshot
	if err = jsonUnmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot); err != nil || snapshot.Version != 1 {
		return nil, rageval.ExecutionSnapshot{}, errors.New("run execution snapshot is invalid")
	}
	return run, snapshot, nil
}
func canonicalRuntimeFields(fields []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := []string{}
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if _, ok := runtimePublishFields[field]; !ok {
			return nil, fmt.Errorf("runtime field %q is not publishable", field)
		}
		if _, ok := seen[field]; ok {
			continue
		}
		seen[field] = struct{}{}
		out = append(out, field)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one runtime field is required")
	}
	sort.Strings(out)
	return out, nil
}
func sameRuntimeExceptVersion(left, right config.RAGRuntimePolicyData) bool {
	left.Version = 0
	right.Version = 0
	return left == right
}
func environmentMatches(environment RuntimePromotionEnvironment, profile config.RAGEvalProfileData) bool {
	return profile.RewriteEnabled == environment.RewriteEnabled && profile.HyDEEnabled == environment.HyDEEnabled && profile.RerankerEnabled == environment.RerankerEnabled && strings.TrimSpace(profile.AnswerModel) == strings.TrimSpace(environment.AnswerModel)
}

func (s *PolicyPromotionService) evaluateGates(ctx context.Context, runID string) (PromotionGateReport, error) {
	report := PromotionGateReport{Passed: true, MetricMeans: map[string]float64{}}
	metrics := map[string][]rageval.MetricResult{}
	cursor := ""
	caseSet := map[string]struct{}{}
	for {
		items, err := s.Store.ListRAGEvalMetricResults(ctx, runID, cursor, 200)
		if err != nil {
			return report, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			if _, needed := s.Gates.MinimumMetricMean[item.MetricName]; !needed {
				continue
			}
			result := rageval.MetricResult{Status: rageval.MetricStatus(item.Status)}
			if item.Value.Valid {
				value := item.Value.Float64
				result.Value = &value
			}
			metrics[item.MetricName] = append(metrics[item.MetricName], result)
			caseSet[item.CaseID] = struct{}{}
		}
		last := items[len(items)-1]
		cursor = store.RAGEvalMetricCursor(last)
	}
	report.ScoredCases = len(caseSet)
	if report.ScoredCases < s.Gates.MinimumScoredCases {
		report.Reasons = append(report.Reasons, fmt.Sprintf("scored cases %d < %d", report.ScoredCases, s.Gates.MinimumScoredCases))
	}
	for metric, minimum := range s.Gates.MinimumMetricMean {
		aggregate := rageval.AggregateMetric(metrics[metric])
		if aggregate.Mean == nil {
			report.Reasons = append(report.Reasons, metric+" has no scored values")
			continue
		}
		report.MetricMeans[metric] = *aggregate.Mean
		if *aggregate.Mean < minimum {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%s mean %.4f < %.4f", metric, *aggregate.Mean, minimum))
		}
	}
	latencies := []int64{}
	total, failed := 0, 0
	cursor = ""
	for {
		items, err := s.Store.ListRAGEvalCaseResults(ctx, runID, cursor, 200)
		if err != nil {
			return report, err
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			total++
			latencies = append(latencies, item.LatencyMS)
			if item.Status == store.RAGEvalCaseError {
				failed++
			}
		}
		cursor = items[len(items)-1].CaseID
	}
	if total > 0 {
		report.CaseErrorRate = float64(failed) / float64(total)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		report.P95LatencyMS = latencies[int(math.Ceil(.95*float64(len(latencies))))-1]
	}
	_, report.CostUSD, _ = s.Store.RAGEvalUsageTotals(ctx, runID)
	if s.Gates.MaximumCaseErrorRate > 0 && report.CaseErrorRate > s.Gates.MaximumCaseErrorRate {
		report.Reasons = append(report.Reasons, "case error rate exceeds gate")
	}
	if s.Gates.MaximumP95LatencyMS > 0 && report.P95LatencyMS > s.Gates.MaximumP95LatencyMS {
		report.Reasons = append(report.Reasons, "p95 latency exceeds gate")
	}
	if s.Gates.MaximumCostUSD > 0 && report.CostUSD > s.Gates.MaximumCostUSD {
		report.Reasons = append(report.Reasons, "cost exceeds gate")
	}
	report.Passed = len(report.Reasons) == 0
	return report, nil
}

// RuntimePolicyRefresher provides eventual multi-pod convergence. Invalid or
// fingerprint-mismatched revisions never replace the last good snapshot.
type RuntimePolicyRefresher struct {
	Store    RuntimePromotionStore
	Snapshot *RuntimePolicySnapshot
	Interval time.Duration
}

func (r *RuntimePolicyRefresher) RefreshOnce(ctx context.Context) error {
	if r == nil || r.Store == nil || r.Snapshot == nil {
		return errors.New("runtime policy refresher dependencies are incomplete")
	}
	record, err := r.Store.ActiveRAGPolicy(ctx, store.RAGPolicyRuntime)
	if err != nil {
		return err
	}
	next, err := config.DecodeRAGRuntimePolicy([]byte(record.PolicyJSON))
	if err != nil {
		return err
	}
	if next.Version != record.Version {
		return errors.New("runtime policy revision version mismatch")
	}
	fingerprint, err := rageval.Fingerprint(next)
	if err != nil {
		return err
	}
	if fingerprint != record.Fingerprint {
		return errors.New("runtime policy fingerprint mismatch")
	}
	if r.Snapshot.Current().Version == next.Version {
		return nil
	}
	return r.Snapshot.Publish(next)
}
func (r *RuntimePolicyRefresher) Start(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.RefreshOnce(ctx); err != nil {
					slog.Warn("rag policy: runtime refresh rejected", "error", err)
				}
			}
		}
	}()
}

func BootstrapRuntimePolicy(ctx context.Context, st RuntimePromotionStore, snapshot *RuntimePolicySnapshot, initial config.RAGRuntimePolicyData) error {
	if st == nil || snapshot == nil {
		return errors.New("runtime policy bootstrap dependencies are incomplete")
	}
	active, err := st.ActiveRAGPolicy(ctx, store.RAGPolicyRuntime)
	if err == nil {
		data, decodeErr := config.DecodeRAGRuntimePolicy([]byte(active.PolicyJSON))
		if decodeErr != nil {
			return decodeErr
		}
		fingerprint, _ := rageval.Fingerprint(data)
		if data.Version != active.Version || fingerprint != active.Fingerprint {
			return errors.New("active runtime policy failed bootstrap validation")
		}
		return snapshot.Publish(data)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	initial.Version = 1
	encoded, _ := json.Marshal(initial)
	fingerprint, _ := rageval.Fingerprint(initial)
	record := &store.RAGPolicyRecord{Kind: store.RAGPolicyRuntime, Version: 1, PolicyJSON: string(encoded), Fingerprint: fingerprint, CreatedBy: "system:runtime-policy-bootstrap", Note: "production defaults before runtime policy promotion"}
	if err = st.CreateRAGPolicy(ctx, record); err != nil {
		// A peer may have completed the same bootstrap concurrently. Re-read
		// the active pointer and validate it instead of failing this pod.
		active, reloadErr := st.ActiveRAGPolicy(ctx, store.RAGPolicyRuntime)
		if reloadErr != nil {
			return err
		}
		data, decodeErr := config.DecodeRAGRuntimePolicy([]byte(active.PolicyJSON))
		if decodeErr != nil {
			return decodeErr
		}
		storedFingerprint, _ := rageval.Fingerprint(data)
		if data.Version != active.Version || storedFingerprint != active.Fingerprint {
			return errors.New("concurrent runtime policy bootstrap produced an invalid revision")
		}
		return snapshot.Publish(data)
	}
	ok, err := st.ActivateRAGPolicy(ctx, store.RAGPolicyRuntime, 0, 1, record.CreatedBy, "", record.Note, store.RAGPolicyAuditPublish)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPolicyActivationConflict
	}
	return snapshot.Publish(initial)
}

func BootstrapIngestionPolicy(ctx context.Context, st RuntimePromotionStore, initial config.RAGIngestionPolicyData) error {
	if st == nil {
		return errors.New("ingestion policy bootstrap store is incomplete")
	}
	validate := func(record *store.RAGPolicyRecord) error {
		data, err := config.DecodeRAGIngestionPolicy([]byte(record.PolicyJSON))
		if err != nil {
			return err
		}
		fingerprint, _ := rageval.Fingerprint(data)
		if data.Version != record.Version || fingerprint != record.Fingerprint {
			return errors.New("active ingestion policy failed bootstrap validation")
		}
		return nil
	}
	active, err := st.ActiveRAGPolicy(ctx, store.RAGPolicyIngestion)
	if err == nil {
		return validate(active)
	}
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	initial.Version = 1
	if err = initial.Validate(); err != nil {
		return err
	}
	encoded, _ := json.Marshal(initial)
	fingerprint, _ := rageval.Fingerprint(initial)
	record := &store.RAGPolicyRecord{Kind: store.RAGPolicyIngestion, Version: 1, PolicyJSON: string(encoded), Fingerprint: fingerprint, CreatedBy: "system:ingestion-policy-bootstrap", Note: "production defaults before ingestion policy promotion"}
	if err = st.CreateRAGPolicy(ctx, record); err != nil {
		active, reloadErr := st.ActiveRAGPolicy(ctx, store.RAGPolicyIngestion)
		if reloadErr != nil {
			return err
		}
		return validate(active)
	}
	ok, err := st.ActivateRAGPolicy(ctx, store.RAGPolicyIngestion, 0, 1, record.CreatedBy, "", record.Note, store.RAGPolicyAuditPublish)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPolicyActivationConflict
	}
	return nil
}

// Small wrappers keep the policy file's dependency surface explicit.
var jsonMarshal = json.Marshal
var jsonUnmarshal = json.Unmarshal
