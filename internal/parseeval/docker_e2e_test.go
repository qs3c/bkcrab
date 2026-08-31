package parseeval

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/store"
)

// TestDockerOfficeParsersEndToEnd is deliberately opt-in because it requires
// the three parser-evaluation Docker sidecars. See docs/parser-evaluation.md.
func TestDockerOfficeParsersEndToEnd(t *testing.T) {
	if os.Getenv("PARSER_EVAL_E2E") != "1" {
		t.Skip("set PARSER_EVAL_E2E=1 after starting the parser-evaluation Docker profile")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixtureDir := filepath.Join(t.TempDir(), "fixtures")
	generate := exec.CommandContext(ctx, "uv", "run", "--extra", "dev", "python", "tests/fixtures/generate_minimal.py", "--output", fixtureDir)
	generate.Dir = filepath.Join(repositoryRoot, "services", "rag-parser")
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate Office fixtures: %v\n%s", err, output)
	}

	database, err := store.NewDBStore("sqlite", filepath.Join(t.TempDir(), "parser-eval-e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ragRowsBefore := map[string]int64{
		"rag_documents": countRows(t, database.DB(), "rag_documents"),
		"rag_chunks":    countRows(t, database.DB(), "rag_chunks"),
	}

	cfg := config.DefaultParserEvaluationCfg()
	cfg.Enabled = true
	cfg.RendererEndpoint = envOrDefault("PARSER_EVAL_RENDERER_E2E_ENDPOINT", "http://127.0.0.1:18081")
	cfg.MarkItDownEndpoint = envOrDefault("PARSER_EVAL_MARKITDOWN_E2E_ENDPOINT", "http://127.0.0.1:18082")
	cfg.AnyDocEndpoint = envOrDefault("PARSER_EVAL_ANYDOC_E2E_ENDPOINT", "http://127.0.0.1:18083")
	cfg.RenderTimeoutMS = 120_000
	cfg.ParseTimeoutMS = 120_000
	cfg.JudgeTimeoutMS = 10_000

	rendererConfig := DefaultRendererClientConfig(cfg.RendererEndpoint)
	rendererConfig.RequestTimeout = time.Duration(cfg.RenderTimeoutMS) * time.Millisecond
	rendererConfig.ExpectedMaxPages = cfg.MaxPages
	rendererConfig.ExpectedRenderDPI = cfg.RenderDPI
	rendererConfig.MaxInputBytes = cfg.MaxFileBytes
	rendererConfig.TempDir = t.TempDir()
	renderer, err := NewHTTPRendererClient(rendererConfig, &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	rendererHealth := waitForRenderer(t, ctx, renderer)

	markitdown := newE2EParserClient(t, cfg.MarkItDownEndpoint, sidecar.OfficeEngineMarkItDown, cfg)
	anydoc := newE2EParserClient(t, cfg.AnyDocEndpoint, sidecar.OfficeEngineAnyDoc, cfg)
	waitForParser(t, ctx, markitdown, sidecar.OfficeEngineMarkItDown)
	waitForParser(t, ctx, anydoc, sidecar.OfficeEngineAnyDoc)
	pool := sidecar.NewPool(sidecar.OfficeEngineMarkItDown, map[string]*sidecar.Client{
		sidecar.OfficeEngineMarkItDown: markitdown,
		sidecar.OfficeEngineAnyDoc:     anydoc,
	})
	parser := parse.NewLocalParser(pool, 300, 200<<20)
	objectStore := objects.NewLocalFS(filepath.Join(t.TempDir(), "objects"))
	service, err := NewService(database, objectStore, cfg)
	if err != nil {
		t.Fatal(err)
	}

	const actor = "parser-eval-e2e-admin"
	const judgeID = "parser-eval-e2e-judge"
	run, err := service.CreateDraft(ctx, CreateDraftRequest{CreatedBy: actor, JudgeModelBindingID: judgeID})
	if err != nil {
		t.Fatal(err)
	}
	mediaTypes := map[string]string{"docx": MediaTypeDOCX, "pptx": MediaTypePPTX, "xlsx": MediaTypeXLSX}
	for _, format := range []string{"docx", "pptx", "xlsx"} {
		fixturePath := filepath.Join(fixtureDir, "minimal."+format)
		fixture, err := os.Open(fixturePath)
		if err != nil {
			t.Fatal(err)
		}
		info, err := fixture.Stat()
		if err != nil {
			_ = fixture.Close()
			t.Fatal(err)
		}
		_, uploadErr := service.UploadDocument(ctx, UploadDocumentRequest{
			RunID: run.ID, CreatedBy: actor, FileName: info.Name(), MediaType: mediaTypes[format],
			DeclaredSizeBytes: info.Size(), Reader: fixture,
		})
		closeErr := fixture.Close()
		if uploadErr != nil || closeErr != nil {
			t.Fatalf("upload %s: %v", format, fmt.Errorf("%w; close: %v", uploadErr, closeErr))
		}
	}

	snapshot := ExecutionSnapshot{
		MarkItDown:         evalParserDescriptor(t, sidecar.OfficeEngineMarkItDown),
		AnyDoc:             evalParserDescriptor(t, sidecar.OfficeEngineAnyDoc),
		Renderer:           rendererHealth.Descriptor,
		Judge:              JudgeBindingSnapshot{ID: judgeID, Provider: "e2e", Model: "deterministic", Fingerprint: fmt.Sprintf("%x", sha256.Sum256([]byte("parser-eval-e2e-judge"))), ModelDisplayName: "E2E deterministic judge"},
		RenderDPI:          cfg.RenderDPI,
		MaxPages:           cfg.MaxPages,
		MarkdownJudgeChars: cfg.MarkdownJudgeChars,
		JudgePromptVersion: JudgePromptVersion,
		AppVersion:         "docker-e2e",
		CreatedBy:          actor,
		ParserConcurrency:  1,
	}
	if _, err := service.StartRun(ctx, run.ID, actor, snapshot); err != nil {
		t.Fatal(err)
	}
	resolver := func(context.Context, string, JudgeBindingSnapshot) (JudgeModel, error) {
		return deterministicE2EJudge{}, nil
	}
	runner, err := NewRunner(database, objectStore, renderer, parser, resolver, cfg, RunnerOptions{WorkerID: "docker-e2e", LeaseDuration: 2 * time.Minute, HeartbeatEvery: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := runner.RunOnce(ctx)
	if err != nil || !claimed {
		t.Fatalf("run evaluation: claimed=%v err=%v", claimed, err)
	}

	completed, err := service.GetRun(ctx, run.ID, actor)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Run.Status != store.ParserEvalRunSucceeded || len(completed.Documents) != 3 {
		t.Fatalf("unexpected completed run: status=%s documents=%d", completed.Run.Status, len(completed.Documents))
	}
	summary, err := DecodeClosedJSON[RunSummary]([]byte(completed.Run.SummaryJSON), MaxSummaryJSONBytes, func(value *RunSummary) error { return value.Validate() })
	if err != nil || len(summary.Formats) != 3 || summary.MarkItDown.Successes != 3 || summary.AnyDoc.Successes != 3 {
		t.Fatalf("unexpected summary: %#v err=%v", summary, err)
	}
	for _, document := range completed.Documents {
		assertSuccessfulE2EDocument(t, ctx, service, completed.Run.ID, document)
	}
	for table, before := range ragRowsBefore {
		if after := countRows(t, database.DB(), table); after != before {
			t.Fatalf("parser-only evaluation changed %s rows: before=%d after=%d", table, before, after)
		}
	}
}

type deterministicE2EJudge struct{}

func (deterministicE2EJudge) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return &provider.Response{
		Content: `{"a":{"completeness":4,"structure":4,"formatting":4,"cleanliness":4},"b":{"completeness":4,"structure":4,"formatting":4,"cleanliness":4},"winner":"tie","reason":"Both candidates preserve the fixture."}`,
		Usage:   provider.Usage{InputTokens: 100, OutputTokens: 40},
	}, nil
}

func newE2EParserClient(t *testing.T, endpoint, engine string, cfg config.ParserEvaluationCfg) *sidecar.Client {
	t.Helper()
	client, err := sidecar.NewClient(sidecar.ClientConfig{
		Endpoint: endpoint, OfficeEngine: engine, Timeout: time.Duration(cfg.ParseTimeoutMS) * time.Millisecond,
		HealthTTL: 30 * time.Second, HealthProbeInterval: 15 * time.Second, PDFLicenseApproved: true,
		Limits:  sidecar.ClientLimits{MaxInputBytes: cfg.MaxFileBytes, MaxOutputBytes: 200 << 20, MaxExtractedBytes: 200 << 20},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func waitForRenderer(t *testing.T, ctx context.Context, client *HTTPRendererClient) RendererHealthSnapshot {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, err := client.ProbeHealth(probeCtx)
		cancel()
		if err == nil && snapshot.Healthy {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("renderer did not become healthy: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitForParser(t *testing.T, ctx context.Context, client *sidecar.Client, engine string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, err := client.ProbeHealth(probeCtx)
		cancel()
		if err == nil && snapshot.Healthy && snapshot.Office.Enabled {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s parser did not become healthy: %v", engine, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func evalParserDescriptor(t *testing.T, engine string) ParserDescriptor {
	t.Helper()
	descriptor, err := sidecar.OfficeParserDescriptor(engine)
	if err != nil {
		t.Fatal(err)
	}
	return ParserDescriptor{Name: descriptor.Name, Version: descriptor.Version, WrapperVersion: descriptor.WrapperVersion}
}

func assertSuccessfulE2EDocument(t *testing.T, ctx context.Context, service *Service, runID string, document store.ParserEvalDocumentRecord) {
	t.Helper()
	markitdown, markitdownOK := parserResultFor(document, EngineMarkItDown)
	anydoc, anydocOK := parserResultFor(document, EngineAnyDoc)
	judge, judgeOK := decodeJudgeResult(document.JudgeResultJSON)
	if document.Status != store.ParserEvalDocumentSucceeded || !markitdownOK || !anydocOK || !judgeOK ||
		markitdown.ParseDurationMS == nil || markitdown.EndToEndDurationMS == nil || anydoc.ParseDurationMS == nil || anydoc.EndToEndDurationMS == nil ||
		judge.Status != StepSucceeded || !judge.MarkItDownA.Successful() || !judge.AnyDocA.Successful() {
		t.Fatalf("incomplete document result for %s", document.Format)
	}
	requests := []ArtifactRequest{
		{Kind: ArtifactSource},
		{Kind: ArtifactTruthPage, Page: 1},
		{Kind: ArtifactMarkdown, Engine: EngineMarkItDown},
		{Kind: ArtifactMarkdown, Engine: EngineAnyDoc},
		{Kind: ArtifactJudgeRaw, Order: JudgeMarkItDownA},
		{Kind: ArtifactJudgeRaw, Order: JudgeAnyDocA},
	}
	for _, request := range requests {
		artifact, err := service.OpenArtifact(ctx, runID, document.ID, request)
		if err != nil {
			t.Fatalf("open %s artifact for %s: %v", request.Kind, document.Format, err)
		}
		body, readErr := io.ReadAll(artifact.Reader)
		closeErr := artifact.Reader.Close()
		if readErr != nil || closeErr != nil || len(body) == 0 || int64(len(body)) != artifact.SizeBytes {
			t.Fatalf("invalid %s artifact for %s: bytes=%d size=%d read=%v close=%v", request.Kind, document.Format, len(body), artifact.SizeBytes, readErr, closeErr)
		}
	}
}

func countRows(t *testing.T, database *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
