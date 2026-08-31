package parseeval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/store"
)

var (
	ErrRunnerFenceLost = errors.New("parser evaluation runner lost its lease fence")
	ErrRunnerCancelled = errors.New("parser evaluation run was cancelled")
)

type RunnerStore interface {
	ClaimParserEvalRun(context.Context, string, time.Time, time.Duration) (*store.ParserEvalLease, bool, error)
	HeartbeatParserEvalRun(context.Context, store.ParserEvalLease, time.Time, time.Duration) (bool, error)
	UpdateParserEvalRunProgress(context.Context, store.ParserEvalLease, string, string, time.Time) (bool, error)
	GetParserEvalRun(context.Context, string) (*store.ParserEvalRunRecord, error)
	ListParserEvalDocuments(context.Context, string) ([]store.ParserEvalDocumentRecord, error)
	PutParserEvalDocumentResults(context.Context, store.ParserEvalLease, store.ParserEvalDocumentUpdate) (bool, error)
	FinishParserEvalRun(context.Context, store.ParserEvalLease, store.ParserEvalRunFinish, time.Time) (bool, error)
}

type TruthRenderer interface {
	Render(context.Context, document.Source) (*RenderedTruth, error)
}

type OfficeParser interface {
	Parse(context.Context, document.Source, parse.ParseOptions) (*document.ParsedDocument, error)
}

type RunnerOptions struct {
	WorkerID       string
	LeaseDuration  time.Duration
	HeartbeatEvery time.Duration
	PollEvery      time.Duration
	RetryDelay     time.Duration
	MaxRetries     int
	Now            func() time.Time
}

type Runner struct {
	store    RunnerStore
	objects  objects.Store
	renderer TruthRenderer
	parser   OfficeParser
	resolve  JudgeResolver
	config   config.ParserEvaluationCfg
	options  RunnerOptions
}

func NewRunner(database RunnerStore, objectStore objects.Store, renderer TruthRenderer, parser OfficeParser, resolve JudgeResolver, cfg config.ParserEvaluationCfg, options RunnerOptions) (*Runner, error) {
	if database == nil || objectStore == nil || renderer == nil || parser == nil || resolve == nil {
		return nil, errors.New("parser evaluation runner dependencies are required")
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.WorkerID) == "" {
		return nil, errors.New("parser evaluation worker ID is required")
	}
	if options.LeaseDuration <= 0 {
		options.LeaseDuration = 2 * time.Minute
	}
	if options.HeartbeatEvery <= 0 {
		options.HeartbeatEvery = 30 * time.Second
	}
	if options.HeartbeatEvery >= options.LeaseDuration {
		return nil, errors.New("heartbeat interval must be shorter than lease duration")
	}
	if options.PollEvery <= 0 {
		options.PollEvery = time.Second
	}
	if options.RetryDelay < 0 {
		return nil, errors.New("retry delay cannot be negative")
	}
	if options.RetryDelay == 0 {
		options.RetryDelay = 100 * time.Millisecond
	}
	if options.MaxRetries < 0 {
		return nil, errors.New("max retries cannot be negative")
	}
	if options.MaxRetries == 0 {
		options.MaxRetries = 2
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Runner{store: database, objects: objectStore, renderer: renderer, parser: parser, resolve: resolve, config: cfg, options: options}, nil
}

// Run keeps claiming work until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	for {
		claimed, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if claimed {
			continue
		}
		timer := time.NewTimer(r.options.PollEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOnce claims and completes at most one run.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	lease, claimed, err := r.store.ClaimParserEvalRun(ctx, r.options.WorkerID, r.now(), r.options.LeaseDuration)
	if err != nil || !claimed {
		return claimed, err
	}
	workCtx, cancelWork := context.WithCancelCause(ctx)
	stopHeartbeat := r.superviseLease(workCtx, cancelWork, *lease)
	defer stopHeartbeat()

	run, err := r.store.GetParserEvalRun(workCtx, lease.RunID)
	if err != nil {
		return true, err
	}
	snapshot, err := DecodeClosedJSON[ExecutionSnapshot]([]byte(run.ExecutionSnapshotJSON), MaxSnapshotJSONBytes, func(value *ExecutionSnapshot) error { return value.Validate() })
	if err != nil {
		return true, r.finishFatal(ctx, *lease, "invalid_execution_snapshot", err)
	}
	documents, err := r.store.ListParserEvalDocuments(workCtx, lease.RunID)
	if err != nil {
		return true, err
	}
	progress := RunProgress{DocumentsTotal: len(documents)}
	for index := range documents {
		if documents[index].Status == store.ParserEvalDocumentSucceeded {
			progress.DocumentsCompleted++
			continue
		}
		progress.CurrentDocumentID = documents[index].ID
		if err := r.checkpoint(workCtx, *lease); err != nil {
			return true, r.handleStoppedRun(ctx, *lease, progress, documents, err)
		}
		if err := r.updateProgress(workCtx, *lease, RunStageValidating, progress); err != nil {
			return true, r.handleStoppedRun(ctx, *lease, progress, documents, err)
		}
		updated, err := r.processDocument(workCtx, *lease, snapshot, documents[index], progress)
		if err != nil {
			return true, r.handleStoppedRun(ctx, *lease, progress, documents, err)
		}
		documents[index] = updated
		progress.DocumentsCompleted++
	}
	progress.CurrentDocumentID = ""
	if err := r.updateProgress(workCtx, *lease, RunStageAggregating, progress); err != nil {
		return true, r.handleStoppedRun(ctx, *lease, progress, documents, err)
	}
	summary := Aggregate(documentViews(documents))
	summaryJSON, err := EncodeBoundedJSON(summary, MaxSummaryJSONBytes)
	if err != nil {
		return true, r.finishFatal(ctx, *lease, "summary_encode_failed", err)
	}
	progressJSON, err := EncodeBoundedJSON(progress, MaxSnapshotJSONBytes)
	if err != nil {
		return true, r.finishFatal(ctx, *lease, "progress_encode_failed", err)
	}
	status := classifyRun(documents)
	ok, err := r.store.FinishParserEvalRun(ctx, *lease, store.ParserEvalRunFinish{
		Status: status, Stage: store.ParserEvalRunStageAggregating, ProgressJSON: progressJSON, SummaryJSON: summaryJSON,
	}, r.now())
	if err != nil {
		return true, err
	}
	if !ok {
		return true, ErrRunnerFenceLost
	}
	return true, nil
}

func (r *Runner) processDocument(ctx context.Context, lease store.ParserEvalLease, snapshot ExecutionSnapshot, record store.ParserEvalDocumentRecord, progress RunProgress) (store.ParserEvalDocumentRecord, error) {
	source := r.documentSource(record, "")
	truth, truthOK := decodeTruthResult(record.TruthJSON)
	if !truthOK || !truth.Successful() {
		if err := r.updateProgress(ctx, lease, RunStageRendering, progress); err != nil {
			return record, err
		}
		var renderResult TruthResult
		err := r.retryExternal(ctx, time.Duration(r.config.RenderTimeoutMS)*time.Millisecond, func(attemptCtx context.Context) error {
			var stepErr error
			renderResult, stepErr = r.renderTruth(attemptCtx, lease.RunID, record.ID, source)
			return stepErr
		})
		if err != nil {
			if ctx.Err() != nil {
				return record, context.Cause(ctx)
			}
			renderResult = TruthResult{Status: StepFailed, Error: boundedErrorDetail(stepErrorCode("render", err), err.Error())}
		}
		encoded, encodeErr := EncodeBoundedJSON(renderResult, MaxResultJSONBytes)
		if encodeErr != nil {
			return record, encodeErr
		}
		if err := r.putDocument(ctx, lease, &record, store.ParserEvalDocumentUpdate{DocumentID: record.ID, Status: store.ParserEvalDocumentRunning, Stage: store.ParserEvalDocumentStageRendering, TruthJSON: encoded}); err != nil {
			return record, err
		}
		truth = renderResult
	}

	orders := parserOrder(record.SHA256)
	for position, engine := range orders {
		current, valid := parserResultFor(record, engine)
		if valid && current.Successful() {
			continue
		}
		if err := r.checkpoint(ctx, lease); err != nil {
			return record, err
		}
		if err := r.updateProgress(ctx, lease, RunStageParsing, progress); err != nil {
			return record, err
		}
		var parserResult ParserResult
		err := r.retryExternal(ctx, time.Duration(r.config.ParseTimeoutMS)*time.Millisecond, func(attemptCtx context.Context) error {
			var stepErr error
			parserResult, stepErr = r.parseDocument(attemptCtx, lease.RunID, snapshot, r.documentSource(record, engine), engine, position+1)
			return stepErr
		})
		if err != nil {
			if ctx.Err() != nil {
				return record, context.Cause(ctx)
			}
			parserResult = ParserResult{Status: StepFailed, Order: position + 1, Error: boundedErrorDetail(stepErrorCode("parse", err), err.Error())}
		}
		encoded, encodeErr := EncodeBoundedJSON(parserResult, MaxResultJSONBytes)
		if encodeErr != nil {
			return record, encodeErr
		}
		update := store.ParserEvalDocumentUpdate{DocumentID: record.ID, Status: store.ParserEvalDocumentRunning, Stage: store.ParserEvalDocumentStageParsing}
		if engine == EngineMarkItDown {
			update.MarkItDownResultJSON = encoded
		} else {
			update.AnyDocResultJSON = encoded
		}
		if err := r.putDocument(ctx, lease, &record, update); err != nil {
			return record, err
		}
	}

	markitdown, markitdownOK := parserResultFor(record, EngineMarkItDown)
	anydoc, anydocOK := parserResultFor(record, EngineAnyDoc)
	judge, judgeOK := decodeJudgeResult(record.JudgeResultJSON)
	if !(judgeOK && judge.Status == StepSucceeded) {
		if truth.Successful() && markitdownOK && markitdown.Successful() && anydocOK && anydoc.Successful() {
			if err := r.checkpoint(ctx, lease); err != nil {
				return record, err
			}
			if err := r.updateProgress(ctx, lease, RunStageScoring, progress); err != nil {
				return record, err
			}
			var pages []JudgePage
			var markitdownText, anydocText []byte
			loadErr := r.retryExternal(ctx, time.Duration(r.config.JudgeTimeoutMS)*time.Millisecond, func(attemptCtx context.Context) error {
				var err error
				pages, err = r.loadJudgePages(attemptCtx, truth)
				if err != nil {
					return err
				}
				markitdownText, err = r.readArtifact(attemptCtx, markitdown.Markdown)
				if err != nil {
					return err
				}
				anydocText, err = r.readArtifact(attemptCtx, anydoc.Markdown)
				return err
			})
			if loadErr != nil {
				if ctx.Err() != nil {
					return record, context.Cause(ctx)
				}
				judge = failedJudgeResult("judge_input_unavailable", loadErr.Error())
			} else {
				var existing *JudgeResult
				if judgeOK {
					existing = &judge
				}
				judge = r.judgeWithRetries(ctx, lease.RunID, record.ID, snapshot, pages, string(markitdownText), string(anydocText), existing)
				if ctx.Err() != nil {
					return record, context.Cause(ctx)
				}
			}
		} else {
			judge = skippedJudgeResult("judge_prerequisite_failed", "truth and both parser results must succeed before scoring")
		}
		encoded, err := EncodeBoundedJSON(judge, MaxResultJSONBytes)
		if err != nil {
			return record, err
		}
		if err := r.putDocument(ctx, lease, &record, store.ParserEvalDocumentUpdate{DocumentID: record.ID, Status: store.ParserEvalDocumentRunning, Stage: store.ParserEvalDocumentStageScoring, JudgeResultJSON: encoded}); err != nil {
			return record, err
		}
	}

	status, code, message := classifyDocument(record)
	if err := r.putDocument(ctx, lease, &record, store.ParserEvalDocumentUpdate{DocumentID: record.ID, Status: status, Stage: store.ParserEvalDocumentStageScoring, ErrorCode: code, ErrorMessage: message}); err != nil {
		return record, err
	}
	return record, nil
}

func (r *Runner) renderTruth(ctx context.Context, runID, documentID string, source document.Source) (_ TruthResult, resultErr error) {
	truth, err := r.renderer.Render(ctx, source)
	if err != nil {
		return TruthResult{}, err
	}
	if truth == nil {
		return TruthResult{}, errors.New("renderer returned no truth bundle")
	}
	defer func() { resultErr = errors.Join(resultErr, truth.Close()) }()
	result := TruthResult{
		Status: StepSucceeded, Descriptor: truth.Descriptor, TotalPages: truth.TotalPages, CoveredPages: truth.CoveredPages,
		RenderDurationMS: &truth.RenderDurationMS, Pages: make([]TruthPage, 0, len(truth.Pages)),
	}
	for _, page := range truth.Pages {
		key, err := TruthPageObjectKey(runID, documentID, page.Page)
		if err != nil {
			return TruthResult{}, err
		}
		reader, err := truth.OpenPage(ctx, page.Page)
		if err != nil {
			return TruthResult{}, err
		}
		putErr := r.objects.Put(ctx, key, reader, page.ByteSize, "image/png")
		closeErr := reader.Close()
		if putErr != nil || closeErr != nil {
			return TruthResult{}, transientStep(errors.Join(putErr, closeErr))
		}
		result.Pages = append(result.Pages, TruthPage{Page: page.Page, Width: page.Width, Height: page.Height, Artifact: StoredArtifact{ObjectKey: key, SHA256: page.SHA256, MediaType: "image/png", ByteSize: page.ByteSize}})
	}
	return result, result.Validate()
}

func (r *Runner) parseDocument(ctx context.Context, runID string, snapshot ExecutionSnapshot, source document.Source, engine Engine, order int) (ParserResult, error) {
	descriptor := snapshot.MarkItDown
	if engine == EngineAnyDoc {
		descriptor = snapshot.AnyDoc
	}
	var timings sidecar.BundleTimings
	var timingSeen bool
	parsed, err := r.parser.Parse(ctx, source, parse.ParseOptions{
		Mode: config.ParseModeStandard, ParserVersion: descriptor.Version,
		SidecarTimings: func(value sidecar.BundleTimings) { timings, timingSeen = value, true },
	})
	if err != nil {
		return ParserResult{}, err
	}
	if parsed == nil {
		return ParserResult{}, errors.New("parser returned no document")
	}
	defer parsed.Close()
	markdown := document.JoinMarkdownUnits(parsed.Units)
	key, err := MarkdownObjectKey(runID, source.DocID, engine)
	if err != nil {
		return ParserResult{}, err
	}
	if err := r.objects.Put(ctx, key, strings.NewReader(markdown), int64(len(markdown)), "text/markdown; charset=utf-8"); err != nil {
		return ParserResult{}, transientStep(err)
	}
	digest := sha256.Sum256([]byte(markdown))
	result := ParserResult{
		Status: StepSucceeded, Descriptor: descriptor, Order: order,
		Markdown: StoredArtifact{ObjectKey: key, SHA256: hex.EncodeToString(digest[:]), MediaType: "text/markdown; charset=utf-8", ByteSize: int64(len(markdown))},
		Stats:    ComputeStructureStats(markdown, len(parsed.Warnings)),
		Warnings: make([]ParseWarning, 0, len(parsed.Warnings)),
	}
	if timingSeen {
		endToEnd := timings.EndToEndDuration.Milliseconds()
		result.EndToEndDurationMS = &endToEnd
		if timings.ParseDuration != nil {
			parseDuration := timings.ParseDuration.Milliseconds()
			result.ParseDurationMS = &parseDuration
		}
	}
	for _, warning := range parsed.Warnings {
		result.Warnings = append(result.Warnings, ParseWarning{Code: truncateRunes(warning.Code, 128), Message: truncateRunes(warning.Message, 2048), Degraded: warning.Degraded})
	}
	return result, result.Validate()
}

func (r *Runner) documentSource(record store.ParserEvalDocumentRecord, engine Engine) document.Source {
	return document.Source{
		DocID: record.ID, FileName: record.FileName, Format: record.Format, ParserEngine: string(engine),
		Size: record.SizeBytes, SHA256: record.SHA256,
		Open: func(ctx context.Context) (io.ReadCloser, error) { return r.objects.Get(ctx, record.SourceObjectKey) },
	}
}

func (r *Runner) judgeWithRetries(ctx context.Context, runID, documentID string, snapshot ExecutionSnapshot, pages []JudgePage, markitdown, anydoc string, existing *JudgeResult) JudgeResult {
	judge := BlindJudge{
		Resolve: r.resolve, Timeout: time.Duration(r.config.JudgeTimeoutMS) * time.Millisecond,
		MaxMarkdownRunes: snapshot.MarkdownJudgeChars,
		WriteRaw: func(ctx context.Context, order JudgeOrder, raw []byte) (StoredArtifact, error) {
			key, err := JudgeRawObjectKey(runID, documentID, order)
			if err != nil {
				return StoredArtifact{}, err
			}
			if err := r.objects.Put(ctx, key, bytes.NewReader(raw), int64(len(raw)), "application/json"); err != nil {
				return StoredArtifact{}, err
			}
			digest := sha256.Sum256(raw)
			return StoredArtifact{ObjectKey: key, SHA256: hex.EncodeToString(digest[:]), MediaType: "application/json", ByteSize: int64(len(raw))}, nil
		},
	}
	result := JudgeResult{}
	prior := existing
	for attempt := 0; attempt <= r.options.MaxRetries; attempt++ {
		result = judge.Evaluate(ctx, JudgeInput{Binding: snapshot.Judge, Pages: pages, MarkItDown: markitdown, AnyDoc: anydoc, Existing: prior})
		if result.Status == StepSucceeded || !transientJudgeResult(result) || attempt == r.options.MaxRetries {
			break
		}
		prior = &result
		if !waitRetry(ctx, r.options.RetryDelay) {
			break
		}
	}
	return result
}

func (r *Runner) loadJudgePages(ctx context.Context, truth TruthResult) ([]JudgePage, error) {
	pages := make([]JudgePage, 0, len(truth.Pages))
	for _, page := range truth.Pages {
		raw, err := r.readArtifact(ctx, page.Artifact)
		if err != nil {
			return nil, err
		}
		pages = append(pages, JudgePage{Page: page.Page, PNG: raw})
	}
	return pages, nil
}

func (r *Runner) readArtifact(ctx context.Context, artifact StoredArtifact) ([]byte, error) {
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	reader, err := r.objects.Get(ctx, artifact.ObjectKey)
	if err != nil {
		return nil, transientStep(err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, artifact.ByteSize+1))
	if err != nil {
		return nil, transientStep(err)
	}
	if int64(len(raw)) != artifact.ByteSize {
		return nil, errors.New("stored artifact size mismatch")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != artifact.SHA256 {
		return nil, errors.New("stored artifact digest mismatch")
	}
	return raw, nil
}

func (r *Runner) retryExternal(ctx context.Context, timeout time.Duration, operation func(context.Context) error) error {
	var last error
	for attempt := 0; attempt <= r.options.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		last = operation(attemptCtx)
		cancel()
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if last == nil || !isTransientStep(last) || attempt == r.options.MaxRetries {
			return last
		}
		if !waitRetry(ctx, r.options.RetryDelay) {
			return context.Cause(ctx)
		}
	}
	return last
}

func waitRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type transientStepError struct{ error }

func transientStep(err error) error {
	if err == nil {
		return nil
	}
	return transientStepError{error: err}
}

func isTransientStep(err error) bool {
	var forced transientStepError
	if errors.As(err, &forced) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRendererUnavailable) {
		return true
	}
	var rendererHTTP *RendererHTTPError
	if errors.As(err, &rendererHTTP) {
		return rendererHTTP.StatusCode == http.StatusTooManyRequests || rendererHTTP.StatusCode >= 500
	}
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		code := status.HTTPStatus()
		return code == http.StatusTooManyRequests || code >= 500
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func transientJudgeResult(result JudgeResult) bool {
	for _, slot := range []JudgeSlot{result.MarkItDownA, result.AnyDocA} {
		if slot.Successful() {
			continue
		}
		switch slot.Error.Code {
		case "judge_call_failed", "judge_timeout", "judge_raw_store_failed":
			continue
		default:
			return false
		}
	}
	return result.Status != StepSucceeded
}

func (r *Runner) superviseLease(ctx context.Context, cancel context.CancelCauseFunc, lease store.ParserEvalLease) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(r.options.HeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				alive, err := r.store.HeartbeatParserEvalRun(ctx, lease, r.now(), r.options.LeaseDuration)
				if err != nil {
					cancel(err)
					return
				}
				if !alive {
					cancel(ErrRunnerFenceLost)
					return
				}
				run, err := r.store.GetParserEvalRun(ctx, lease.RunID)
				if err != nil {
					cancel(err)
					return
				}
				if run.CancelRequestedAt.Valid {
					cancel(ErrRunnerCancelled)
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(stop); <-done }) }
}

func (r *Runner) checkpoint(ctx context.Context, lease store.ParserEvalLease) error {
	if err := ctx.Err(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	run, err := r.store.GetParserEvalRun(ctx, lease.RunID)
	if err != nil {
		return err
	}
	if run.CancelRequestedAt.Valid {
		return ErrRunnerCancelled
	}
	if run.Status != store.ParserEvalRunRunning || run.LeaseOwner != lease.LeaseOwner || run.FenceToken != lease.FenceToken || !run.LeaseUntil.Valid || !run.LeaseUntil.Time.After(r.now()) {
		return ErrRunnerFenceLost
	}
	return nil
}

func (r *Runner) updateProgress(ctx context.Context, lease store.ParserEvalLease, stage RunStage, progress RunProgress) error {
	encoded, err := EncodeBoundedJSON(progress, MaxSnapshotJSONBytes)
	if err != nil {
		return err
	}
	ok, err := r.store.UpdateParserEvalRunProgress(ctx, lease, string(stage), encoded, r.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunnerFenceLost
	}
	return nil
}

func (r *Runner) putDocument(ctx context.Context, lease store.ParserEvalLease, record *store.ParserEvalDocumentRecord, update store.ParserEvalDocumentUpdate) error {
	ok, err := r.store.PutParserEvalDocumentResults(ctx, lease, update)
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunnerFenceLost
	}
	mergeResult := func(current *string, next string) {
		if next == "" {
			return
		}
		currentStatus, _ := DecodeClosedJSON[struct {
			Status StepStatus `json:"status"`
		}]([]byte(*current), MaxResultJSONBytes, nil)
		if currentStatus.Status == StepSucceeded {
			return
		}
		*current = next
	}
	mergeResult(&record.TruthJSON, update.TruthJSON)
	mergeResult(&record.MarkItDownResultJSON, update.MarkItDownResultJSON)
	mergeResult(&record.AnyDocResultJSON, update.AnyDocResultJSON)
	mergeResult(&record.JudgeResultJSON, update.JudgeResultJSON)
	if update.Status != "" {
		record.Status = update.Status
	}
	if update.Stage != "" {
		record.Stage = update.Stage
	}
	record.ErrorCode, record.ErrorMessage = update.ErrorCode, update.ErrorMessage
	return nil
}

func (r *Runner) handleStoppedRun(ctx context.Context, lease store.ParserEvalLease, progress RunProgress, documents []store.ParserEvalDocumentRecord, err error) error {
	if errors.Is(err, ErrRunnerCancelled) {
		summaryJSON, encodeErr := EncodeBoundedJSON(Aggregate(documentViews(documents)), MaxSummaryJSONBytes)
		if encodeErr != nil {
			return encodeErr
		}
		progress.CurrentDocumentID = ""
		progressJSON, encodeErr := EncodeBoundedJSON(progress, MaxSnapshotJSONBytes)
		if encodeErr != nil {
			return encodeErr
		}
		ok, finishErr := r.store.FinishParserEvalRun(ctx, lease, store.ParserEvalRunFinish{Status: store.ParserEvalRunCancelled, Stage: store.ParserEvalRunStageAggregating, ProgressJSON: progressJSON, SummaryJSON: summaryJSON}, r.now())
		if finishErr != nil {
			return finishErr
		}
		if !ok {
			return ErrRunnerFenceLost
		}
		return nil
	}
	return err
}

func (r *Runner) finishFatal(ctx context.Context, lease store.ParserEvalLease, code string, failure error) error {
	progress := RunProgress{}
	progressJSON, _ := EncodeBoundedJSON(progress, MaxSnapshotJSONBytes)
	summaryJSON, _ := EncodeBoundedJSON(RunSummary{}, MaxSummaryJSONBytes)
	ok, err := r.store.FinishParserEvalRun(ctx, lease, store.ParserEvalRunFinish{
		Status: store.ParserEvalRunFailed, Stage: store.ParserEvalRunStageAggregating, ProgressJSON: progressJSON, SummaryJSON: summaryJSON,
		ErrorCode: code, ErrorMessage: truncateRunes(failure.Error(), 2048),
	}, r.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrRunnerFenceLost
	}
	return nil
}

func (r *Runner) now() time.Time { return r.options.Now().UTC() }

func decodeTruthResult(raw string) (TruthResult, bool) {
	result, err := DecodeClosedJSON[TruthResult]([]byte(raw), MaxResultJSONBytes, func(value *TruthResult) error { return value.Validate() })
	return result, err == nil
}

func parserResultFor(record store.ParserEvalDocumentRecord, engine Engine) (ParserResult, bool) {
	raw := record.MarkItDownResultJSON
	if engine == EngineAnyDoc {
		raw = record.AnyDocResultJSON
	}
	return decodeParserResult(raw)
}

func parserOrder(digest string) []Engine {
	decoded, err := hex.DecodeString(digest)
	if err == nil && len(decoded) > 0 && decoded[len(decoded)-1]&1 == 1 {
		return []Engine{EngineAnyDoc, EngineMarkItDown}
	}
	return []Engine{EngineMarkItDown, EngineAnyDoc}
}

func skippedJudgeResult(code, message string) JudgeResult {
	return JudgeResult{
		Status: StepSkipped, MarkItDownA: JudgeSlot{Order: JudgeMarkItDownA, Status: StepSkipped},
		AnyDocA: JudgeSlot{Order: JudgeAnyDocA, Status: StepSkipped}, Error: boundedErrorDetail(code, message),
	}
}

func classifyDocument(record store.ParserEvalDocumentRecord) (status, code, message string) {
	truth, truthOK := decodeTruthResult(record.TruthJSON)
	markitdown, markitdownOK := parserResultFor(record, EngineMarkItDown)
	anydoc, anydocOK := parserResultFor(record, EngineAnyDoc)
	judge, judgeOK := decodeJudgeResult(record.JudgeResultJSON)
	if truthOK && truth.Successful() && markitdownOK && markitdown.Successful() && anydocOK && anydoc.Successful() && judgeOK && judge.Status == StepSucceeded {
		return store.ParserEvalDocumentSucceeded, "", ""
	}
	if markitdownOK && markitdown.Successful() || anydocOK && anydoc.Successful() {
		return store.ParserEvalDocumentPartial, "evaluation_incomplete", "one or more evaluation slots did not succeed"
	}
	return store.ParserEvalDocumentFailed, "parsers_failed", "neither parser completed successfully"
}

func classifyRun(documents []store.ParserEvalDocumentRecord) string {
	allSucceeded := len(documents) > 0
	anyUseful := false
	for _, document := range documents {
		allSucceeded = allSucceeded && document.Status == store.ParserEvalDocumentSucceeded
		anyUseful = anyUseful || document.Status == store.ParserEvalDocumentSucceeded || document.Status == store.ParserEvalDocumentPartial
	}
	if allSucceeded {
		return store.ParserEvalRunSucceeded
	}
	if anyUseful {
		return store.ParserEvalRunPartial
	}
	return store.ParserEvalRunFailed
}

func documentViews(documents []store.ParserEvalDocumentRecord) []DocumentView {
	views := make([]DocumentView, 0, len(documents))
	for _, document := range documents {
		views = append(views, DocumentView{Format: Format(document.Format), MarkItDownResultJSON: document.MarkItDownResultJSON, AnyDocResultJSON: document.AnyDocResultJSON, JudgeResultJSON: document.JudgeResultJSON})
	}
	return views
}

func stepErrorCode(prefix string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return prefix + "_timeout"
	}
	return prefix + "_failed"
}
