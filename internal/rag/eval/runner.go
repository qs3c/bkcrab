package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
	"golang.org/x/sync/errgroup"
)

var (
	ErrRunnerFenceLost  = errors.New("evaluation runner lease lost")
	ErrRunBudgetReached = errors.New("evaluation run budget reached")
)

type RunStore interface {
	CreateRAGEvalRun(context.Context, *store.RAGEvalRunRecord) error
	GetRAGEvalRun(context.Context, string) (*store.RAGEvalRunRecord, error)
	GetRAGEvalDatasetVersion(context.Context, string) (*store.RAGEvalDatasetVersionRecord, error)
	GetRAGEvalProfile(context.Context, string) (*store.RAGEvalProfileRecord, error)
	GetRAGEvalGeneration(context.Context, string) (*store.RAGEvalGenerationRecord, error)
	AttachReadyRAGEvalGenerationForRun(context.Context, string, string) (*store.RAGEvalGenerationRecord, error)
	ListRAGEvalCases(context.Context, string, string, int) ([]store.RAGEvalCaseRecord, error)
	ClaimNextRAGEvalRun(context.Context, string, time.Duration) (*store.RAGEvalRunFence, bool, error)
	ClaimRAGEvalRun(context.Context, string, string, time.Duration) (*store.RAGEvalRunFence, bool, error)
	HeartbeatRAGEvalRun(context.Context, store.RAGEvalRunFence, time.Duration) (bool, error)
	UpdateRAGEvalRunProgress(context.Context, store.RAGEvalRunFence, string, string) (bool, error)
	FinishRAGEvalRun(context.Context, store.RAGEvalRunFence, string, string, string) (bool, error)
	GetRAGEvalCaseResult(context.Context, string, string) (*store.RAGEvalCaseResultRecord, error)
	PutRAGEvalCaseResult(context.Context, store.RAGEvalRunFence, store.RAGEvalCaseResultRecord) (bool, error)
	PutRAGEvalMetricResult(context.Context, store.RAGEvalRunFence, store.RAGEvalMetricResultRecord) (bool, error)
	RecordRAGEvalUsageFenced(context.Context, store.RAGEvalRunFence, *store.RAGEvalUsageRecord) (bool, error)
	RAGEvalUsageTotals(context.Context, string) (int64, float64, error)
}

type GenerationProvider interface {
	Ensure(context.Context, *store.RAGEvalRunRecord, ExecutionSnapshot) (*store.RAGEvalGenerationRecord, error)
	Release(context.Context, string) error
}

type CaseExecutionRequest struct {
	RunID, OwnerID string
	Generation     *store.RAGEvalGenerationRecord
	Profile        config.RAGEvalProfileData
	Case           Case
}

type CaseExecutionResult struct {
	Response, ErrorCode, ErrorMessage string
	Contexts, ContextIDs, Citations   []string
	SearchTrace, AnswerTrace          any
	Latency                           time.Duration
	Usage                             Usage
	Abstained                         bool
}

type Usage struct {
	Stage, Provider, Model    string
	InputTokens, OutputTokens int64
	EstimatedCostUSD          float64
	ActualCostUSD             float64
}

// CasePipeline is implemented by the parent rag package so the runner uses
// the shared retrieval and GenerateAnswer boundaries without creating a
// package cycle. Evaluation requests contain no chat-history persistence hook.
type CasePipeline interface {
	Execute(context.Context, CaseExecutionRequest) (CaseExecutionResult, error)
}

type BatchScorer interface {
	Evaluate(context.Context, EvaluateRequest) (EvaluateResponse, error)
}

type RunBudgets struct {
	MaxCases       int     `json:"maxCases"`
	MaxTokens      int64   `json:"maxTokens"`
	MaxCostUSD     float64 `json:"maxCostUsd"`
	MaxDurationSec int     `json:"maxDurationSec"`
}

type ExecutionSnapshot struct {
	Version             int                               `json:"version"`
	DatasetVersion      store.RAGEvalDatasetVersionRecord `json:"datasetVersion"`
	Profile             config.RAGEvalProfileData         `json:"profile"`
	ProfileFingerprint  string                            `json:"profileFingerprint"`
	Metrics             []string                          `json:"metrics"`
	MetricBundleVersion string                            `json:"metricBundleVersion"`
	IndexGenerationID   string                            `json:"indexGenerationId,omitempty"`
	Budgets             RunBudgets                        `json:"budgets"`
	CreatedAt           time.Time                         `json:"createdAt"`
}

type CreateRunRequest struct {
	ID, DatasetVersionID, BaselineRunID, ProfileID, IndexGenerationID, CreatedBy string
	Mode                                                                         RunMode
	Metrics                                                                      []string
}

type Progress struct {
	Total                int     `json:"total"`
	Completed            int     `json:"completed"`
	Failed               int     `json:"failed"`
	Scored               int     `json:"scored"`
	Tokens               int64   `json:"tokens"`
	CostUSD              float64 `json:"costUsd"`
	ParserEngine         string  `json:"parserEngine,omitempty"`
	GenerationDurationMS int64   `json:"generationDurationMs,omitempty"`
	GenerationReused     bool    `json:"generationReused"`
}

type Runner struct {
	store       RunStore
	generations GenerationProvider
	pipeline    CasePipeline
	scorer      BatchScorer
	cfg         config.RAGEvaluationCfg
	workerID    string
	lease, poll time.Duration
	startOnce   sync.Once
}

func NewRunner(st RunStore, generations GenerationProvider, pipeline CasePipeline, scorer BatchScorer, cfg config.RAGEvaluationCfg, workerID string) (*Runner, error) {
	cfg.ApplyDefaults()
	if st == nil || generations == nil || pipeline == nil || scorer == nil || strings.TrimSpace(workerID) == "" || cfg.WorkerConcurrency < 1 {
		return nil, errors.New("evaluation runner dependencies are incomplete")
	}
	return &Runner{store: st, generations: generations, pipeline: pipeline, scorer: scorer, cfg: cfg,
		workerID: strings.TrimSpace(workerID), lease: 30 * time.Second, poll: time.Second}, nil
}

func allowedMetric(name string) bool {
	if _, ok := RagasMetrics[name]; ok {
		return true
	}
	switch name {
	case "hit_at_k", "recall_at_k", "mrr", "ndcg", "citation_precision", "citation_coverage", "abstention_accuracy":
		return true
	default:
		return false
	}
}

func canonicalMetrics(metrics []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		metric = strings.TrimSpace(metric)
		if !allowedMetric(metric) {
			return nil, fmt.Errorf("unsupported evaluation metric %q", metric)
		}
		if _, ok := seen[metric]; ok {
			continue
		}
		seen[metric] = struct{}{}
		out = append(out, metric)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one metric is required")
	}
	sort.Strings(out)
	return out, nil
}

func (r *Runner) CreateRun(ctx context.Context, request CreateRunRequest) (*store.RAGEvalRunRecord, error) {
	mode := string(request.Mode)
	if mode != store.RAGEvalRunModeFullPipeline && mode != store.RAGEvalRunModeOnlineOnly {
		return nil, errors.New("invalid run mode")
	}
	dataset, err := r.store.GetRAGEvalDatasetVersion(ctx, request.DatasetVersionID)
	if err != nil {
		return nil, err
	}
	if dataset.Status != store.RAGEvalDatasetReady {
		return nil, errors.New("dataset version must be READY")
	}
	profileRecord, err := r.store.GetRAGEvalProfile(ctx, request.ProfileID)
	if err != nil {
		return nil, err
	}
	profile, err := config.DecodeRAGEvalProfile([]byte(profileRecord.ProfileJSON))
	if err != nil {
		return nil, err
	}
	metrics, err := canonicalMetrics(request.Metrics)
	if err != nil {
		return nil, err
	}
	if dataset.CaseCount > int64(r.cfg.MaxRunCases) {
		return nil, errors.New("dataset exceeds run case budget")
	}
	if mode == store.RAGEvalRunModeFullPipeline && request.IndexGenerationID != "" {
		return nil, errors.New("full pipeline run cannot select a generation")
	}
	if mode == store.RAGEvalRunModeOnlineOnly && request.IndexGenerationID == "" {
		return nil, errors.New("online-only run requires an explicit READY generation")
	}
	if mode == store.RAGEvalRunModeOnlineOnly {
		generation, generationErr := r.store.GetRAGEvalGeneration(ctx, request.IndexGenerationID)
		if generationErr != nil {
			return nil, generationErr
		}
		if generation.Status != store.RAGEvalGenerationReady || generation.DatasetVersionID != dataset.ID {
			return nil, errors.New("online-only generation must be READY and belong to the dataset version")
		}
	}
	snapshot := ExecutionSnapshot{Version: 1, DatasetVersion: *dataset, Profile: profile,
		ProfileFingerprint: profileRecord.Fingerprint, Metrics: metrics, MetricBundleVersion: MetricBundleV1,
		IndexGenerationID: request.IndexGenerationID, CreatedAt: time.Now().UTC(), Budgets: RunBudgets{
			MaxCases: r.cfg.MaxRunCases, MaxTokens: r.cfg.MaxRunTokens, MaxCostUSD: r.cfg.MaxRunCostUSD, MaxDurationSec: r.cfg.MaxRunDurationSec,
		}}
	snapshotJSON, _ := json.Marshal(snapshot)
	metricsJSON, _ := json.Marshal(metrics)
	record := &store.RAGEvalRunRecord{ID: request.ID, DatasetVersionID: dataset.ID, BaselineRunID: request.BaselineRunID, Mode: mode,
		ProfileID: profileRecord.ID, IndexGenerationID: request.IndexGenerationID, RequestedMetricsJSON: string(metricsJSON), ExecutionSnapshotJSON: string(snapshotJSON), CreatedBy: strings.TrimSpace(request.CreatedBy)}
	if err := r.store.CreateRAGEvalRun(ctx, record); err != nil {
		return nil, err
	}
	return record, nil
}

func (r *Runner) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		for i := 0; i < r.cfg.WorkerConcurrency; i++ {
			go r.worker(ctx, fmt.Sprintf("%s-%d", r.workerID, i))
		}
	})
}

func (r *Runner) worker(ctx context.Context, worker string) {
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	for {
		if err := r.RunNext(ctx, worker); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("rag eval runner", "worker", worker, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) RunNext(ctx context.Context, worker string) error {
	fence, ok, err := r.store.ClaimNextRAGEvalRun(ctx, worker, r.lease)
	if err != nil || !ok {
		return err
	}
	return r.runClaimed(ctx, *fence)
}

func (r *Runner) Run(ctx context.Context, runID string) error {
	fence, ok, err := r.store.ClaimRAGEvalRun(ctx, runID, r.workerID, r.lease)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("run is not claimable")
		}
		return err
	}
	return r.runClaimed(ctx, *fence)
}

func (r *Runner) runClaimed(parent context.Context, fence store.RAGEvalRunFence) error {
	ctx, cancel := context.WithCancel(parent)
	leaseLost := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(r.lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := r.store.HeartbeatRAGEvalRun(ctx, fence, r.lease)
				if err != nil || !ok {
					if err == nil {
						err = ErrRunnerFenceLost
					}
					select {
					case leaseLost <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	err := r.execute(ctx, fence)
	cancel()
	<-heartbeatDone
	select {
	case lost := <-leaseLost:
		return errors.Join(err, lost)
	default:
	}
	return err
}

func (r *Runner) execute(ctx context.Context, fence store.RAGEvalRunFence) (retErr error) {
	run, err := r.store.GetRAGEvalRun(ctx, fence.RunID)
	if err != nil {
		return err
	}
	var snapshot ExecutionSnapshot
	if err = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot); err != nil || snapshot.Version != 1 {
		return r.finishFailure(fence, "invalid_snapshot", "invalid frozen execution snapshot")
	}
	deadline := snapshot.CreatedAt.Add(time.Duration(snapshot.Budgets.MaxDurationSec) * time.Second)
	if snapshot.Budgets.MaxDurationSec > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	generationStarted := time.Now()
	generation, err := r.generations.Ensure(ctx, run, snapshot)
	if err != nil {
		return r.finishFailure(fence, "generation_failed", err.Error())
	}
	generationDuration := time.Since(generationStarted)
	defer func() {
		if releaseErr := r.generations.Release(context.Background(), run.ID); retErr == nil && releaseErr != nil {
			retErr = releaseErr
		}
	}()
	cases, err := r.loadCases(ctx, run.DatasetVersionID, snapshot.Budgets.MaxCases)
	if err != nil {
		return r.finishFailure(fence, "load_cases_failed", err.Error())
	}
	progress := Progress{Total: len(cases), ParserEngine: snapshot.Profile.Ingestion.ParserEngine,
		GenerationDurationMS: max(int64(1), generationDuration.Milliseconds()), GenerationReused: generation.OwnerRunID != "" && generation.OwnerRunID != run.ID}
	if err = r.progress(ctx, fence, "answering", progress); err != nil {
		return err
	}
	samples := make([]EvaluationSample, 0, len(cases))
	caseByID := make(map[string]Case, len(cases))
	caseConcurrency := max(1, r.cfg.CaseConcurrency)
	for start := 0; start < len(cases); start += caseConcurrency {
		if err = ctx.Err(); err != nil {
			return r.finishContext(fence, run, err)
		}
		current, getErr := r.store.GetRAGEvalRun(ctx, run.ID)
		if getErr != nil {
			return getErr
		}
		if current.CancelRequestedAt.Valid {
			return r.finish(fence, store.RAGEvalRunCancelled, "cancelled", "evaluation cancelled")
		}
		tokens, cost, totalErr := r.store.RAGEvalUsageTotals(ctx, run.ID)
		if totalErr != nil {
			return totalErr
		}
		progress.Tokens, progress.CostUSD = tokens, cost
		if (snapshot.Budgets.MaxTokens > 0 && tokens >= snapshot.Budgets.MaxTokens) || (snapshot.Budgets.MaxCostUSD > 0 && cost >= snapshot.Budgets.MaxCostUSD) {
			_ = r.progress(ctx, fence, "budget_exceeded", progress)
			return r.finish(fence, store.RAGEvalRunBudgetExceeded, "budget_exceeded", "evaluation budget exceeded")
		}
		end := min(start+caseConcurrency, len(cases))
		outcomes := make([]caseExecutionOutcome, end-start)
		group, groupCtx := errgroup.WithContext(ctx)
		group.SetLimit(caseConcurrency)
		for index := start; index < end; index++ {
			index := index
			group.Go(func() error {
				outcome, executeCaseErr := r.executeCase(groupCtx, fence, run, generation, snapshot.Profile, cases[index])
				outcomes[index-start] = outcome
				return executeCaseErr
			})
		}
		if groupErr := group.Wait(); groupErr != nil {
			if ctx.Err() != nil {
				return r.finishContext(fence, run, ctx.Err())
			}
			return groupErr
		}
		for _, outcome := range outcomes {
			progress.Completed++
			if outcome.Failed {
				progress.Failed++
			}
			if outcome.Item.ID != "" {
				caseByID[outcome.Item.ID] = outcome.Item
			}
			if outcome.Sample != nil {
				samples = append(samples, *outcome.Sample)
			}
		}
		if err = r.progress(ctx, fence, "answering", progress); err != nil {
			return err
		}
		if exceeded, budgetErr := r.refreshBudget(ctx, run.ID, snapshot.Budgets, &progress); budgetErr != nil {
			return budgetErr
		} else if exceeded {
			_ = r.progress(ctx, fence, "budget_exceeded", progress)
			return r.finish(fence, store.RAGEvalRunBudgetExceeded, "budget_exceeded", "evaluation budget exceeded")
		}
	}
	if err = r.score(ctx, fence, run.ID, run.CreatedBy, snapshot, samples, caseByID, &progress); err != nil {
		if errors.Is(err, ErrRunBudgetReached) {
			_ = r.progress(ctx, fence, "budget_exceeded", progress)
			return r.finish(fence, store.RAGEvalRunBudgetExceeded, "budget_exceeded", "evaluation budget exceeded")
		}
		// Pipeline results are already durable. Leave the run RUNNING so its
		// expired lease can be reclaimed and scoring retried without answering
		// completed cases again.
		_ = r.progress(ctx, fence, "scoring_retry", progress)
		return err
	}
	return r.finish(fence, store.RAGEvalRunSucceeded, "", "")
}

type caseExecutionOutcome struct {
	Item   Case
	Sample *EvaluationSample
	Failed bool
}

func (r *Runner) executeCase(ctx context.Context, fence store.RAGEvalRunFence, run *store.RAGEvalRunRecord, generation *store.RAGEvalGenerationRecord, profile config.RAGEvalProfileData, record store.RAGEvalCaseRecord) (caseExecutionOutcome, error) {
	item, err := decodeCase(record)
	if err != nil {
		return caseExecutionOutcome{Failed: true}, nil
	}
	existing, err := r.store.GetRAGEvalCaseResult(ctx, run.ID, item.ID)
	if err == nil && existing.Status == store.RAGEvalCaseOK {
		if sample, sampleErr := sampleFromStored(item, existing); sampleErr == nil {
			return caseExecutionOutcome{Item: item, Sample: &sample}, nil
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return caseExecutionOutcome{}, err
	}
	started := time.Now()
	result, executeErr := r.pipeline.Execute(ctx, CaseExecutionRequest{RunID: run.ID, OwnerID: run.CreatedBy, Generation: generation, Profile: profile, Case: item})
	if result.Latency <= 0 {
		result.Latency = time.Since(started)
	}
	stored := storedCaseResult(run.ID, item.ID, result, executeErr)
	ok, err := r.store.PutRAGEvalCaseResult(ctx, fence, stored)
	if err != nil {
		return caseExecutionOutcome{}, err
	}
	if !ok {
		return caseExecutionOutcome{}, ErrRunnerFenceLost
	}
	if executeErr != nil {
		return caseExecutionOutcome{Item: item, Failed: true}, nil
	}
	usage := result.Usage
	if usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.EstimatedCostUSD > 0 || usage.ActualCostUSD > 0 {
		_, err = r.store.RecordRAGEvalUsageFenced(ctx, fence, &store.RAGEvalUsageRecord{RunID: run.ID, CaseID: item.ID, Stage: usage.Stage, Provider: usage.Provider, Model: usage.Model,
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, EstimatedCostUSD: usage.EstimatedCostUSD, ActualCostUSD: usage.ActualCostUSD, IdempotencyKey: run.ID + ":" + item.ID + ":answer"})
		if err != nil {
			return caseExecutionOutcome{}, err
		}
	}
	sample, err := sampleFromStored(item, &stored)
	if err != nil {
		return caseExecutionOutcome{Item: item}, nil
	}
	return caseExecutionOutcome{Item: item, Sample: &sample}, nil
}

func (r *Runner) loadCases(ctx context.Context, datasetID string, maxCases int) ([]store.RAGEvalCaseRecord, error) {
	out := []store.RAGEvalCaseRecord{}
	cursor := ""
	for len(out) < maxCases {
		batch, err := r.store.ListRAGEvalCases(ctx, datasetID, cursor, min(200, maxCases-len(out)))
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		out = append(out, batch...)
		cursor = batch[len(batch)-1].ID
	}
	return out, nil
}

func decodeCase(record store.RAGEvalCaseRecord) (Case, error) {
	item := Case{ID: record.ID, UserInput: record.UserInput, Reference: record.ReferenceAnswer, ExpectedAbstention: record.ExpectedAbstention}
	if err := json.Unmarshal([]byte(record.ReferenceContextsJSON), &item.ReferenceContexts); err != nil {
		return Case{}, err
	}
	if err := json.Unmarshal([]byte(record.ReferenceContextIDsJSON), &item.ReferenceContextIDs); err != nil {
		return Case{}, err
	}
	if err := json.Unmarshal([]byte(record.TagsJSON), &item.Tags); err != nil {
		return Case{}, err
	}
	if err := json.Unmarshal([]byte(record.MetadataJSON), &item.Metadata); err != nil {
		return Case{}, err
	}
	// Dataset history is intentionally excluded: evaluation answers have no
	// conversation history and therefore cannot write ordinary chat records.
	return item, nil
}

func storedCaseResult(runID, caseID string, result CaseExecutionResult, executeErr error) store.RAGEvalCaseResultRecord {
	contexts, _ := json.Marshal(result.Contexts)
	ids, _ := json.Marshal(result.ContextIDs)
	search, _ := json.Marshal(result.SearchTrace)
	answer, _ := json.Marshal(result.AnswerTrace)
	usage, _ := json.Marshal(result.Usage)
	status := store.RAGEvalCaseOK
	code, message := result.ErrorCode, result.ErrorMessage
	if executeErr != nil {
		status = store.RAGEvalCaseError
		if code == "" {
			code = "pipeline_error"
		}
		if message == "" {
			message = executeErr.Error()
		}
	}
	return store.RAGEvalCaseResultRecord{RunID: runID, CaseID: caseID, Response: result.Response, ContextsJSON: string(contexts), CitationsJSON: string(ids), SearchTraceJSON: string(search), AnswerTraceJSON: string(answer), Status: status, ErrorCode: code, ErrorMessage: message, LatencyMS: result.Latency.Milliseconds(), UsageJSON: string(usage)}
}

func sampleFromStored(item Case, result *store.RAGEvalCaseResultRecord) (EvaluationSample, error) {
	var contexts, ids []string
	if err := json.Unmarshal([]byte(result.ContextsJSON), &contexts); err != nil {
		return EvaluationSample{}, err
	}
	if err := json.Unmarshal([]byte(result.CitationsJSON), &ids); err != nil {
		return EvaluationSample{}, err
	}
	return EvaluationSample{CaseID: item.ID, UserInput: item.UserInput, RetrievedContexts: contexts, RetrievedContextIDs: ids, Response: result.Response, Reference: item.Reference, ReferenceContexts: item.ReferenceContexts}, nil
}

func (r *Runner) score(ctx context.Context, fence store.RAGEvalRunFence, runID, ownerID string, snapshot ExecutionSnapshot, samples []EvaluationSample, cases map[string]Case, progress *Progress) error {
	requestedRagas := []string{}
	deterministic := []string{}
	for _, metric := range snapshot.Metrics {
		if _, ok := RagasMetrics[metric]; ok {
			requestedRagas = append(requestedRagas, metric)
		} else {
			deterministic = append(deterministic, metric)
		}
	}
	for _, sample := range samples {
		all := DeterministicMetrics(DeterministicInput{RetrievedContextIDs: sample.RetrievedContextIDs, ReferenceContextIDs: cases[sample.CaseID].ReferenceContextIDs, Response: sample.Response, ExpectedAbstention: cases[sample.CaseID].ExpectedAbstention, Abstained: strings.TrimSpace(sample.Response) == ""}, snapshot.Profile.Runtime.TopN)
		for _, metric := range deterministic {
			if result, ok := all[metric]; ok {
				if err := r.putMetric(ctx, fence, sample.CaseID, metric, result); err != nil {
					return err
				}
			}
		}
	}
	batchSize := max(1, r.cfg.MaxBatchSize)
	scoreConcurrency := max(1, r.cfg.ScoreConcurrency)
	batchCount := (len(samples) + batchSize - 1) / batchSize
	for wave := 0; wave < batchCount && len(requestedRagas) > 0; wave += scoreConcurrency {
		waveEnd := min(wave+scoreConcurrency, batchCount)
		scored := make([]int, waveEnd-wave)
		group, groupCtx := errgroup.WithContext(ctx)
		group.SetLimit(scoreConcurrency)
		for batchIndex := wave; batchIndex < waveEnd; batchIndex++ {
			batchIndex := batchIndex
			group.Go(func() error {
				start := batchIndex * batchSize
				end := min(start+batchSize, len(samples))
				count, scoreErr := r.scoreBatch(groupCtx, fence, runID, ownerID, snapshot.MetricBundleVersion, requestedRagas, samples[start:end], start)
				scored[batchIndex-wave] = count
				return scoreErr
			})
		}
		if err := group.Wait(); err != nil {
			return err
		}
		for _, count := range scored {
			progress.Scored += count
		}
		if err := r.progress(ctx, fence, "scoring", *progress); err != nil {
			return err
		}
		if exceeded, budgetErr := r.refreshBudget(ctx, runID, snapshot.Budgets, progress); budgetErr != nil {
			return budgetErr
		} else if exceeded {
			return ErrRunBudgetReached
		}
	}
	return nil
}

func (r *Runner) scoreBatch(ctx context.Context, fence store.RAGEvalRunFence, runID, ownerID, metricBundleVersion string, metrics []string, samples []EvaluationSample, offset int) (int, error) {
	requestID := runID + ":" + fmt.Sprint(offset)
	response, err := r.scorer.Evaluate(ctx, EvaluateRequest{OwnerID: ownerID, RequestID: requestID, MetricBundleVersion: metricBundleVersion, Metrics: metrics, Samples: samples})
	if err != nil {
		return 0, err
	}
	if err := r.recordEvaluatorUsage(ctx, fence, requestID, response.Usage); err != nil {
		return 0, err
	}
	for _, item := range response.Results {
		for metric, result := range item.Metrics {
			if err := r.putMetric(ctx, fence, item.CaseID, metric, result); err != nil {
				return 0, err
			}
		}
	}
	return len(response.Results), nil
}

func (r *Runner) recordEvaluatorUsage(ctx context.Context, fence store.RAGEvalRunFence, requestID string, usage EvaluatorUsage) error {
	records := []store.RAGEvalUsageRecord{
		{RunID: fence.RunID, Stage: "judge", Provider: r.cfg.Sidecar.LLMProvider, Model: r.cfg.Sidecar.LLMModel, InputTokens: usage.LLMInputTokens, OutputTokens: usage.LLMOutputTokens, EstimatedCostUSD: usage.LLMEstimatedCostUSD, IdempotencyKey: requestID + ":judge"},
		{RunID: fence.RunID, Stage: "judge_embedding", Provider: r.cfg.Sidecar.EmbeddingProvider, Model: r.cfg.Sidecar.EmbeddingModel, InputTokens: usage.EmbeddingInputTokens, EstimatedCostUSD: usage.EmbeddingEstimatedCostUSD, IdempotencyKey: requestID + ":embedding"},
	}
	for i := range records {
		record := &records[i]
		if record.InputTokens == 0 && record.OutputTokens == 0 && record.EstimatedCostUSD == 0 {
			continue
		}
		if _, err := r.store.RecordRAGEvalUsageFenced(ctx, fence, record); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) refreshBudget(ctx context.Context, runID string, budgets RunBudgets, progress *Progress) (bool, error) {
	tokens, cost, err := r.store.RAGEvalUsageTotals(ctx, runID)
	if err != nil {
		return false, err
	}
	progress.Tokens, progress.CostUSD = tokens, cost
	return (budgets.MaxTokens > 0 && tokens >= budgets.MaxTokens) || (budgets.MaxCostUSD > 0 && cost >= budgets.MaxCostUSD), nil
}

func (r *Runner) putMetric(ctx context.Context, fence store.RAGEvalRunFence, caseID, metric string, result MetricResult) error {
	details, _ := json.Marshal(result.Details)
	value := sql.NullFloat64{}
	if result.Value != nil {
		value = sql.NullFloat64{Float64: *result.Value, Valid: true}
	}
	ok, err := r.store.PutRAGEvalMetricResult(ctx, fence, store.RAGEvalMetricResultRecord{RunID: fence.RunID, CaseID: caseID, MetricName: metric, MetricVersion: MetricBundleV1, Status: string(result.Status), Value: value, Reason: result.Reason, DetailsJSON: string(details)})
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunnerFenceLost
	}
	return nil
}

func (r *Runner) progress(ctx context.Context, fence store.RAGEvalRunFence, stage string, progress Progress) error {
	raw, _ := json.Marshal(progress)
	ok, err := r.store.UpdateRAGEvalRunProgress(ctx, fence, stage, string(raw))
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunnerFenceLost
	}
	return nil
}
func (r *Runner) finishFailure(fence store.RAGEvalRunFence, code, message string) error {
	err := r.finish(fence, store.RAGEvalRunFailed, code, message)
	if err != nil {
		return err
	}
	return errors.New(code)
}
func (r *Runner) finishContext(fence store.RAGEvalRunFence, run *store.RAGEvalRunRecord, err error) error {
	if run.CancelRequestedAt.Valid {
		return r.finish(fence, store.RAGEvalRunCancelled, "cancelled", "evaluation cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return r.finish(fence, store.RAGEvalRunBudgetExceeded, "duration_budget_exceeded", "evaluation duration budget exceeded")
	}
	return err
}
func (r *Runner) finish(fence store.RAGEvalRunFence, status, code, message string) error {
	ok, err := r.store.FinishRAGEvalRun(context.Background(), fence, status, code, message)
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunnerFenceLost
	}
	return nil
}

// Public mode aliases keep HTTP and callers from depending on store strings.
func FullPipelineMode() RunMode { return RunModeFull }
func OnlineOnlyMode() RunMode   { return RunModeOnlineOnly }
