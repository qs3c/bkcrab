package parseeval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeRunnerStore struct {
	mu            sync.Mutex
	run           store.ParserEvalRunRecord
	documents     []store.ParserEvalDocumentRecord
	claimed       bool
	finish        *store.ParserEvalRunFinish
	failPutFence  bool
	heartbeatLive bool
	cancelOnGet   int
	gets          int
}

func (f *fakeRunnerStore) ClaimParserEvalRun(_ context.Context, worker string, now time.Time, lease time.Duration) (*store.ParserEvalLease, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed {
		return nil, false, nil
	}
	f.claimed = true
	f.run.Status = store.ParserEvalRunRunning
	f.run.LeaseOwner = worker
	f.run.FenceToken++
	f.run.LeaseUntil = sql.NullTime{Time: now.Add(lease), Valid: true}
	return &store.ParserEvalLease{RunID: f.run.ID, LeaseOwner: worker, FenceToken: f.run.FenceToken}, true, nil
}

func (f *fakeRunnerStore) HeartbeatParserEvalRun(_ context.Context, _ store.ParserEvalLease, now time.Time, lease time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.heartbeatLive {
		return false, nil
	}
	f.run.LeaseUntil = sql.NullTime{Time: now.Add(lease), Valid: true}
	return true, nil
}

func (f *fakeRunnerStore) UpdateParserEvalRunProgress(_ context.Context, _ store.ParserEvalLease, stage, progress string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPutFence {
		return false, nil
	}
	f.run.Stage, f.run.ProgressJSON = stage, progress
	return true, nil
}

func (f *fakeRunnerStore) GetParserEvalRun(_ context.Context, _ string) (*store.ParserEvalRunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.cancelOnGet > 0 && f.gets >= f.cancelOnGet {
		f.run.CancelRequestedAt = sql.NullTime{Time: time.Now(), Valid: true}
	}
	copy := f.run
	return &copy, nil
}

func (f *fakeRunnerStore) ListParserEvalDocuments(context.Context, string) ([]store.ParserEvalDocumentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ParserEvalDocumentRecord(nil), f.documents...), nil
}

func (f *fakeRunnerStore) PutParserEvalDocumentResults(_ context.Context, _ store.ParserEvalLease, update store.ParserEvalDocumentUpdate) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPutFence {
		return false, nil
	}
	for index := range f.documents {
		if f.documents[index].ID != update.DocumentID {
			continue
		}
		document := &f.documents[index]
		mergeTestResult(&document.TruthJSON, update.TruthJSON)
		mergeTestResult(&document.MarkItDownResultJSON, update.MarkItDownResultJSON)
		mergeTestResult(&document.AnyDocResultJSON, update.AnyDocResultJSON)
		mergeTestResult(&document.JudgeResultJSON, update.JudgeResultJSON)
		if update.Status != "" {
			document.Status = update.Status
		}
		if update.Stage != "" {
			document.Stage = update.Stage
		}
		document.ErrorCode, document.ErrorMessage = update.ErrorCode, update.ErrorMessage
		return true, nil
	}
	return false, nil
}

func mergeTestResult(current *string, next string) {
	if next == "" {
		return
	}
	var status struct {
		Status string `json:"status"`
	}
	_, _ = fmtSscanStatus(*current, &status.Status)
	if status.Status != string(StepSucceeded) {
		*current = next
	}
}

func fmtSscanStatus(raw string, target *string) (bool, error) {
	value, err := DecodeClosedJSON[struct {
		Status string `json:"status"`
	}]([]byte(raw), MaxResultJSONBytes, nil)
	if err == nil {
		*target = value.Status
	}
	return err == nil, err
}

func (f *fakeRunnerStore) FinishParserEvalRun(_ context.Context, _ store.ParserEvalLease, finish store.ParserEvalRunFinish, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := finish
	f.finish = &copy
	f.run.Status = finish.Status
	return true, nil
}

type fakeTruthRenderer struct {
	t      *testing.T
	events *[]string
	errors []error
	calls  int
}

func (f *fakeTruthRenderer) Render(context.Context, document.Source) (*RenderedTruth, error) {
	*f.events = append(*f.events, "render")
	index := f.calls
	f.calls++
	if index < len(f.errors) && f.errors[index] != nil {
		return nil, f.errors[index]
	}
	root := f.t.TempDir()
	page := []byte("png-page")
	local := root + string(os.PathSeparator) + "page.png"
	if err := os.WriteFile(local, page, 0o600); err != nil {
		f.t.Fatal(err)
	}
	digest := sha256.Sum256(page)
	return &RenderedTruth{
		Descriptor: RendererDescriptor{ProtocolVersion: "parser-eval-renderer/v1", ServiceVersion: "1", LibreOfficeVersion: "1", PyMuPDFVersion: "1"},
		TotalPages: 1, CoveredPages: 1, RenderDurationMS: 7,
		Pages: []RenderedPage{{Page: 1, Width: 10, Height: 20, SHA256: hex.EncodeToString(digest[:]), ByteSize: int64(len(page))}},
		root:  root, entries: map[int]string{1: local},
	}, nil
}

type fakeOfficeParser struct {
	events    *[]string
	errors    map[Engine][]error
	calls     map[Engine]int
	active    atomic.Int32
	maxActive atomic.Int32
	block     bool
}

func (f *fakeOfficeParser) Parse(ctx context.Context, source document.Source, options parse.ParseOptions) (*document.ParsedDocument, error) {
	engine := Engine(source.ParserEngine)
	*f.events = append(*f.events, "parse:"+string(engine))
	active := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		maximum := f.maxActive.Load()
		if active <= maximum || f.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	index := f.calls[engine]
	f.calls[engine] = index + 1
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if index < len(f.errors[engine]) && f.errors[engine][index] != nil {
		return nil, f.errors[engine][index]
	}
	parseDuration := 5 * time.Millisecond
	if options.SidecarTimings != nil {
		options.SidecarTimings(sidecar.BundleTimings{ParseDuration: &parseDuration, EndToEndDuration: 8 * time.Millisecond})
	}
	return document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser:        document.ParserInfo{Name: string(engine), Version: options.ParserVersion},
		Units: []document.MarkdownUnit{{
			ID: "unit_1", Location: document.SourceLocation{Kind: document.LocationDocument}, Markdown: "# " + string(engine),
		}},
	}, nil, nil), nil
}

type fakeRunnerJudgeModel struct{ events *[]string }

func (f *fakeRunnerJudgeModel) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	*f.events = append(*f.events, "judge")
	return &provider.Response{Content: validJudgeJSON, Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

func validRunnerSnapshot(t *testing.T) string {
	t.Helper()
	snapshot := ExecutionSnapshot{
		MarkItDown: ParserDescriptor{Name: "markitdown", Version: "1", WrapperVersion: "1"},
		AnyDoc:     ParserDescriptor{Name: "anydoc", Version: "1", WrapperVersion: "1"},
		Renderer:   RendererDescriptor{ProtocolVersion: "parser-eval-renderer/v1", ServiceVersion: "1", LibreOfficeVersion: "1", PyMuPDFVersion: "1"},
		Judge:      testJudgeBinding(),
		RenderDPI:  100, MaxPages: 6, MarkdownJudgeChars: 40000, JudgePromptVersion: JudgePromptVersion,
		AppVersion: "test", CreatedBy: "admin", ParserConcurrency: 1,
	}
	raw, err := EncodeBoundedJSON(snapshot, MaxSnapshotJSONBytes)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newRunnerFixture(t *testing.T, digest string) (*Runner, *fakeRunnerStore, *fakeObjects, *fakeTruthRenderer, *fakeOfficeParser, *[]string) {
	t.Helper()
	now := time.Now().UTC()
	events := &[]string{}
	documentID := "ped_doc"
	runID := "per_run"
	storeFake := &fakeRunnerStore{
		run: store.ParserEvalRunRecord{ID: runID, Status: store.ParserEvalRunQueued, ExecutionSnapshotJSON: validRunnerSnapshot(t)},
		documents: []store.ParserEvalDocumentRecord{{
			ID: documentID, RunID: runID, Ordinal: 1, FileName: "sample.docx", Format: "docx", MediaType: MediaTypeDOCX,
			SizeBytes: 6, SHA256: digest, SourceObjectKey: "parser-eval/runs/per_run/documents/ped_doc/source.bin",
			Status: store.ParserEvalDocumentUploaded, Stage: store.ParserEvalDocumentStageValidating,
			TruthJSON: `{"status":"PENDING"}`, MarkItDownResultJSON: `{"status":"PENDING"}`, AnyDocResultJSON: `{"status":"PENDING"}`, JudgeResultJSON: `{"status":"PENDING"}`,
		}},
		heartbeatLive: true,
	}
	objectsFake := newFakeObjects()
	objectsFake.data[storeFake.documents[0].SourceObjectKey] = []byte("source")
	renderer := &fakeTruthRenderer{t: t, events: events}
	parserFake := &fakeOfficeParser{events: events, errors: map[Engine][]error{}, calls: map[Engine]int{}}
	judgeModel := &fakeRunnerJudgeModel{events: events}
	cfg := config.DefaultParserEvaluationCfg()
	runner, err := NewRunner(storeFake, objectsFake, renderer, parserFake,
		func(context.Context, string, JudgeBindingSnapshot) (JudgeModel, error) { return judgeModel, nil }, cfg,
		RunnerOptions{WorkerID: "worker", LeaseDuration: time.Hour, HeartbeatEvery: 30 * time.Minute, Now: func() time.Time { return now }, RetryDelay: time.Millisecond},
	)
	if err != nil {
		t.Fatal(err)
	}
	return runner, storeFake, objectsFake, renderer, parserFake, events
}

func TestRunnerExecutesStrictSerialPipeline(t *testing.T) {
	runner, database, _, _, parserFake, events := newRunnerFixture(t, strings.Repeat("0", 64))
	claimed, err := runner.RunOnce(context.Background())
	if err != nil || !claimed {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	want := "render,parse:markitdown,parse:anydoc,judge,judge"
	if got := strings.Join(*events, ","); got != want {
		t.Fatalf("events=%q want=%q", got, want)
	}
	if parserFake.maxActive.Load() != 1 {
		t.Fatalf("parser max concurrency=%d", parserFake.maxActive.Load())
	}
	if database.finish == nil || database.finish.Status != store.ParserEvalRunSucceeded {
		t.Fatalf("finish=%+v", database.finish)
	}
	stored := database.documents[0]
	markitdown, ok := decodeParserResult(stored.MarkItDownResultJSON)
	if !ok || markitdown.Order != 1 || markitdown.ParseDurationMS == nil || *markitdown.ParseDurationMS != 5 || markitdown.EndToEndDurationMS == nil || *markitdown.EndToEndDurationMS != 8 {
		t.Fatalf("markitdown result=%+v ok=%v", markitdown, ok)
	}
	anydoc, ok := decodeParserResult(stored.AnyDocResultJSON)
	if !ok || anydoc.Order != 2 {
		t.Fatalf("anydoc result=%+v ok=%v", anydoc, ok)
	}
}

func TestParserOrderUsesDigestLowestBit(t *testing.T) {
	if got := parserOrder(strings.Repeat("0", 64)); got[0] != EngineMarkItDown {
		t.Fatalf("even order=%v", got)
	}
	if got := parserOrder(strings.Repeat("0", 63) + "1"); got[0] != EngineAnyDoc {
		t.Fatalf("odd order=%v", got)
	}
}

func TestRunnerContinuesParsersAfterRenderFailureAndSkipsJudge(t *testing.T) {
	runner, database, _, renderer, _, events := newRunnerFixture(t, strings.Repeat("0", 64))
	renderer.errors = []error{errors.New("invalid document")}
	if _, err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*events, ","); got != "render,parse:markitdown,parse:anydoc" {
		t.Fatalf("events=%q", got)
	}
	if database.finish == nil || database.finish.Status != store.ParserEvalRunPartial || database.documents[0].Status != store.ParserEvalDocumentPartial {
		t.Fatalf("finish=%+v document=%+v", database.finish, database.documents[0])
	}
}

func TestRunnerSkipsJudgeWhenOneParserFails(t *testing.T) {
	runner, database, _, _, parserFake, events := newRunnerFixture(t, strings.Repeat("0", 64))
	parserFake.errors[EngineMarkItDown] = []error{errors.New("unsupported content")}
	if _, err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*events, ","); got != "render,parse:markitdown,parse:anydoc" {
		t.Fatalf("events=%q", got)
	}
	if database.documents[0].Status != store.ParserEvalDocumentPartial || database.finish == nil || database.finish.Status != store.ParserEvalRunPartial {
		t.Fatalf("document=%+v finish=%+v", database.documents[0], database.finish)
	}
}

func TestRunnerResumesSuccessfulTruthParsersAndJudgeSlot(t *testing.T) {
	runner, database, objectStore, renderer, parserFake, events := newRunnerFixture(t, strings.Repeat("0", 64))
	storedArtifact := func(key, mediaType string, data []byte) StoredArtifact {
		digest := sha256.Sum256(data)
		objectStore.data[key] = data
		return StoredArtifact{ObjectKey: key, SHA256: hex.EncodeToString(digest[:]), MediaType: mediaType, ByteSize: int64(len(data))}
	}
	page := storedArtifact("parser-eval/runs/per_run/documents/ped_doc/truth/page-0001.png", "image/png", []byte("page"))
	truth := TruthResult{
		Status: StepSucceeded, Descriptor: RendererDescriptor{ProtocolVersion: "parser-eval-renderer/v1", ServiceVersion: "1", LibreOfficeVersion: "1", PyMuPDFVersion: "1"},
		TotalPages: 1, CoveredPages: 1, Pages: []TruthPage{{Page: 1, Width: 10, Height: 20, Artifact: page}}, RenderDurationMS: pointer(int64(1)),
	}
	encode := func(value any) string {
		raw, err := EncodeBoundedJSON(value, MaxResultJSONBytes)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	parserResult := func(engine Engine) ParserResult {
		markdown := []byte("# " + string(engine))
		artifact := storedArtifact("parser-eval/runs/per_run/documents/ped_doc/outputs/"+string(engine)+".md", "text/markdown; charset=utf-8", markdown)
		return ParserResult{Status: StepSucceeded, Descriptor: ParserDescriptor{Name: string(engine), Version: "1", WrapperVersion: "1"}, Order: 1, Markdown: artifact, Stats: ComputeStructureStats(string(markdown), 0)}
	}
	rawArtifact := storedArtifact("parser-eval/runs/per_run/documents/ped_doc/judge/markitdown-a.json", "application/json", []byte(validJudgeJSON))
	existingJudge := JudgeResult{
		Status: StepFailed,
		MarkItDownA: JudgeSlot{Order: JudgeMarkItDownA, Status: StepSucceeded, Verdict: BlindVerdict{
			A: BlindScores{5, 5, 5, 5}, B: BlindScores{1, 1, 1, 1}, Winner: PositionWinnerA, Reason: "valid",
		}, Raw: rawArtifact},
		AnyDocA: JudgeSlot{Order: JudgeAnyDocA, Status: StepFailed, Error: ErrorDetail{Code: "judge_call_failed", Message: "failed"}},
		Error:   ErrorDetail{Code: "judge_incomplete", Message: "failed"},
	}
	database.documents[0].TruthJSON = encode(truth)
	database.documents[0].MarkItDownResultJSON = encode(parserResult(EngineMarkItDown))
	database.documents[0].AnyDocResultJSON = encode(parserResult(EngineAnyDoc))
	database.documents[0].JudgeResultJSON = encode(existingJudge)
	database.documents[0].Status = store.ParserEvalDocumentPartial

	if _, err := runner.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if renderer.calls != 0 || len(parserFake.calls) != 0 || strings.Join(*events, ",") != "judge" {
		t.Fatalf("renderer=%d parser=%v events=%v", renderer.calls, parserFake.calls, *events)
	}
	if database.documents[0].Status != store.ParserEvalDocumentSucceeded {
		t.Fatalf("document=%+v", database.documents[0])
	}
}

type temporaryTestError struct{}

func (temporaryTestError) Error() string   { return "temporary" }
func (temporaryTestError) Timeout() bool   { return false }
func (temporaryTestError) Temporary() bool { return true }

func TestRunnerRetriesOnlyTransientSteps(t *testing.T) {
	t.Run("transient twice", func(t *testing.T) {
		runner, _, _, renderer, _, _ := newRunnerFixture(t, strings.Repeat("0", 64))
		renderer.errors = []error{temporaryTestError{}, temporaryTestError{}}
		if _, err := runner.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if renderer.calls != 3 {
			t.Fatalf("render calls=%d want=3", renderer.calls)
		}
	})
	t.Run("deterministic once", func(t *testing.T) {
		runner, _, _, renderer, _, _ := newRunnerFixture(t, strings.Repeat("0", 64))
		renderer.errors = []error{errors.New("bad input")}
		if _, err := runner.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if renderer.calls != 1 {
			t.Fatalf("render calls=%d want=1", renderer.calls)
		}
	})
}

func TestRunnerStopsOnCancellationAndFenceLoss(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		runner, database, _, _, _, events := newRunnerFixture(t, strings.Repeat("0", 64))
		database.cancelOnGet = 2
		if _, err := runner.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if database.finish == nil || database.finish.Status != store.ParserEvalRunCancelled || len(*events) != 0 {
			t.Fatalf("finish=%+v events=%v", database.finish, *events)
		}
	})
	t.Run("fence", func(t *testing.T) {
		runner, database, _, _, _, _ := newRunnerFixture(t, strings.Repeat("0", 64))
		database.failPutFence = true
		if _, err := runner.RunOnce(context.Background()); !errors.Is(err, ErrRunnerFenceLost) {
			t.Fatalf("err=%v", err)
		}
		if database.finish != nil {
			t.Fatalf("stale worker finished run: %+v", database.finish)
		}
	})
	t.Run("heartbeat fence loss cancels active step", func(t *testing.T) {
		runner, database, _, _, parserFake, _ := newRunnerFixture(t, strings.Repeat("0", 64))
		runner.options.HeartbeatEvery = 5 * time.Millisecond
		database.heartbeatLive = false
		parserFake.block = true
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := runner.RunOnce(ctx); !errors.Is(err, ErrRunnerFenceLost) {
			t.Fatalf("err=%v", err)
		}
		if database.finish != nil {
			t.Fatalf("stale worker finished run: %+v", database.finish)
		}
	})
}
