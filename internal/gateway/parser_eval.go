package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/parseeval"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

var ErrParserEvaluationJudgeUnavailable = errors.New("parser evaluation judge binding is unavailable")

type parserEvaluationRuntime struct {
	config   config.ParserEvaluationCfg
	service  *parseeval.Service
	runner   *parseeval.Runner
	cleanup  *parseeval.Cleanup
	renderer *parseeval.HTTPRendererClient
	parsers  *sidecar.Pool
}

func buildParserEvaluationRuntime(cfg config.ParserEvaluationCfg, database store.Store, objectStore objects.Store, objectCfg config.ObjectStoreCfg, homeDir string) (*parserEvaluationRuntime, error) {
	cfg.ApplyDefaults()
	runtime := &parserEvaluationRuntime{config: cfg}
	if !cfg.Enabled {
		return runtime, nil
	}
	if err := cfg.Validate(); err != nil {
		return runtime, err
	}
	if database == nil {
		return runtime, errors.New("parser evaluation database is required")
	}
	if objectStore == nil {
		var err error
		objectStore, err = newRAGObjectStore(objectCfg, homeDir)
		if err != nil {
			return runtime, err
		}
	}
	rendererCfg := parseeval.DefaultRendererClientConfig(cfg.RendererEndpoint)
	rendererCfg.RequestTimeout = time.Duration(cfg.RenderTimeoutMS) * time.Millisecond
	rendererCfg.ExpectedMaxPages = cfg.MaxPages
	rendererCfg.ExpectedRenderDPI = cfg.RenderDPI
	rendererCfg.MaxInputBytes = cfg.MaxFileBytes
	renderer, err := parseeval.NewHTTPRendererClient(rendererCfg, nil)
	if err != nil {
		return runtime, err
	}
	clients := make(map[string]*sidecar.Client, 2)
	for engine, endpoint := range map[string]string{
		sidecar.OfficeEngineMarkItDown: cfg.MarkItDownEndpoint,
		sidecar.OfficeEngineAnyDoc:     cfg.AnyDocEndpoint,
	} {
		client, clientErr := sidecar.NewClient(sidecar.ClientConfig{
			Endpoint: endpoint, OfficeEngine: engine, Timeout: time.Duration(cfg.ParseTimeoutMS) * time.Millisecond,
			Limits: sidecar.ClientLimits{
				MaxInputBytes: cfg.MaxFileBytes, MaxOutputBytes: 200 << 20, MaxExtractedBytes: 200 << 20,
				MaxEntryBytes: cfg.MaxFileBytes, MaxAssetBytes: 20 << 20, MaxRenderBytes: 8 << 20,
				MaxPages: 300, MaxAssets: 500, MaxImagePixels: 40_000_000,
			},
			PDFLicenseApproved: true,
		})
		if clientErr != nil {
			return runtime, fmt.Errorf("%s parser client: %w", engine, clientErr)
		}
		clients[engine] = client
	}
	parserPool := sidecar.NewPool(sidecar.OfficeEngineMarkItDown, clients)
	if parserPool == nil {
		return runtime, errors.New("parser evaluation parser pool is unavailable")
	}
	localParser := parse.NewLocalParser(parserPool, 300, 200<<20)
	service, err := parseeval.NewService(database, objectStore, cfg)
	if err != nil {
		return runtime, err
	}
	cleanup, err := parseeval.NewCleanup(database, objectStore)
	if err != nil {
		return runtime, err
	}
	resolver := parserEvaluationJudgeResolver(database)
	runner, err := parseeval.NewRunner(database, objectStore, renderer, localParser, resolver, cfg, parseeval.RunnerOptions{WorkerID: "parser-eval-" + uuid.NewString()})
	if err != nil {
		return runtime, err
	}
	runtime.service, runtime.runner, runtime.cleanup, runtime.renderer, runtime.parsers = service, runner, cleanup, renderer, parserPool
	return runtime, nil
}

func listParserEvaluationJudgeBindings(cfg *config.Config) []parseeval.JudgeBindingSnapshot {
	if cfg == nil {
		return nil
	}
	providerNames := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	bindings := make([]parseeval.JudgeBindingSnapshot, 0)
	seen := make(map[string]struct{})
	for _, providerName := range providerNames {
		providerCfg := cfg.Providers[providerName]
		if strings.TrimSpace(providerName) == "" || strings.TrimSpace(providerCfg.APIBase) == "" || strings.TrimSpace(providerCfg.APIKey) == "" {
			continue
		}
		for _, model := range providerCfg.Models {
			if !modelSupportsImage(model) || strings.TrimSpace(model.ID) == "" {
				continue
			}
			binding, err := parserEvaluationBinding(providerName, providerCfg, model)
			if err != nil {
				continue
			}
			if _, duplicate := seen[binding.ID]; duplicate {
				continue
			}
			seen[binding.ID] = struct{}{}
			bindings = append(bindings, binding)
		}
	}
	return bindings
}

func parserEvaluationBinding(providerName string, providerCfg config.ProviderConfig, model config.ModelEntry) (parseeval.JudgeBindingSnapshot, error) {
	identity := struct {
		Provider string            `json:"provider"`
		APIBase  string            `json:"apiBase"`
		APIType  string            `json:"apiType"`
		AuthType string            `json:"authType"`
		Model    config.ModelEntry `json:"model"`
	}{
		Provider: strings.TrimSpace(providerName), APIBase: strings.TrimSpace(providerCfg.APIBase),
		APIType: strings.TrimSpace(providerCfg.APIType), AuthType: strings.TrimSpace(providerCfg.AuthType), Model: model,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return parseeval.JudgeBindingSnapshot{}, err
	}
	digest := sha256.Sum256(encoded)
	displayName := strings.TrimSpace(model.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(model.ID)
	}
	binding := parseeval.JudgeBindingSnapshot{
		ID: strings.TrimSpace(providerName) + "/" + strings.TrimSpace(model.ID), Provider: strings.TrimSpace(providerName),
		Model: strings.TrimSpace(model.ID), ModelDisplayName: displayName, Fingerprint: hex.EncodeToString(digest[:]),
		PricingKnown:        model.Cost.Input > 0 || model.Cost.Output > 0,
		InputCostPerMillion: model.Cost.Input, OutputCostPerMillion: model.Cost.Output,
	}
	return binding, binding.Validate()
}

func modelSupportsImage(model config.ModelEntry) bool {
	for _, input := range model.Input {
		if strings.EqualFold(strings.TrimSpace(input), "image") {
			return true
		}
	}
	return false
}

func parserEvaluationJudgeResolver(database store.Store) parseeval.JudgeResolver {
	return func(ctx context.Context, ownerID string, snapshot parseeval.JudgeBindingSnapshot) (parseeval.JudgeModel, error) {
		if database == nil || strings.TrimSpace(ownerID) == "" {
			return nil, ErrParserEvaluationJudgeUnavailable
		}
		owner, err := database.GetUser(ctx, ownerID)
		if err != nil {
			return nil, err
		}
		if owner.Role != users.RoleSuperAdmin || owner.Status != users.StatusActive {
			return nil, ErrParserEvaluationJudgeUnavailable
		}
		cfg, err := assembleConfig(ctx, database, ownerID, "")
		if err != nil {
			return nil, err
		}
		config.LoadEnv().ApplyToConfig(cfg)
		config.ApplyDefaults(cfg)
		bindings := listParserEvaluationJudgeBindings(cfg)
		for _, current := range bindings {
			if current.ID != snapshot.ID {
				continue
			}
			if current != snapshot {
				return nil, fmt.Errorf("%w: binding fingerprint or metadata changed", ErrParserEvaluationJudgeUnavailable)
			}
			providerCfg, ok := cfg.Providers[current.Provider]
			if !ok || strings.TrimSpace(providerCfg.APIKey) == "" || strings.TrimSpace(providerCfg.APIBase) == "" {
				return nil, ErrParserEvaluationJudgeUnavailable
			}
			return provider.NewProvider(providerCfg.APIKey, providerCfg.APIBase, providerCfg.APIType), nil
		}
		return nil, ErrParserEvaluationJudgeUnavailable
	}
}

func (g *Gateway) ListParserEvaluationJudgeBindings(ctx context.Context, ownerID string) ([]parseeval.JudgeBindingSnapshot, error) {
	if g == nil || g.store == nil {
		return nil, ErrParserEvaluationJudgeUnavailable
	}
	owner, err := g.store.GetUser(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	if owner.Role != users.RoleSuperAdmin || owner.Status != users.StatusActive {
		return nil, ErrParserEvaluationJudgeUnavailable
	}
	cfg, err := assembleConfig(ctx, g.store, ownerID, "")
	if err != nil {
		return nil, err
	}
	config.LoadEnv().ApplyToConfig(cfg)
	config.ApplyDefaults(cfg)
	return listParserEvaluationJudgeBindings(cfg), nil
}

func (g *Gateway) ResolveParserEvaluationJudge(ctx context.Context, ownerID string, snapshot parseeval.JudgeBindingSnapshot) (parseeval.JudgeModel, error) {
	if g == nil {
		return nil, ErrParserEvaluationJudgeUnavailable
	}
	return parserEvaluationJudgeResolver(g.store)(ctx, ownerID, snapshot)
}

func (g *Gateway) ParserEvaluationCapabilities(ctx context.Context, ownerID string) (parseeval.Capabilities, error) {
	cfg := config.DefaultParserEvaluationCfg()
	cfg.Enabled = false
	if g != nil && g.parserEval != nil {
		cfg = g.parserEval.config
	} else if g != nil && g.envCfg != nil {
		cfg = g.envCfg.ParserEvaluation
		cfg.ApplyDefaults()
	}
	capabilities := parseeval.Capabilities{
		Enabled: cfg.Enabled, WorkerEnabled: cfg.WorkerEnabled,
		SupportedFormats: []parseeval.Format{parseeval.FormatDOCX, parseeval.FormatPPTX, parseeval.FormatXLSX},
		MaxFiles:         cfg.MaxFiles, MaxFileBytes: cfg.MaxFileBytes, MaxBatchBytes: cfg.MaxBatchBytes,
		MaxPages: cfg.MaxPages, RenderDPI: cfg.RenderDPI, MarkdownJudgeChars: cfg.MarkdownJudgeChars,
		ScoringDimensions: []string{"completeness", "structure", "formatting", "cleanliness"},
	}
	if !cfg.Enabled {
		capabilities.Reason = "disabled"
		return capabilities, nil
	}
	if g == nil || g.parserEval == nil || g.parserEval.service == nil || g.parserEval.renderer == nil || g.parserEval.parsers == nil {
		capabilities.Reason = "runtime_unavailable"
		if g != nil && g.parserEvalReason != "" {
			capabilities.Reason = g.parserEvalReason
		}
		return capabilities, nil
	}
	rendererHealth := g.parserEval.renderer.HealthSnapshot()
	rendererHealthy := rendererHealth.Healthy && !rendererHealth.ExpiresAt.IsZero() && time.Now().Before(rendererHealth.ExpiresAt)
	rendererReason := rendererHealth.Reason
	if rendererHealth.Healthy && !rendererHealthy && rendererReason == "" {
		rendererReason = "health_stale"
	}
	capabilities.Renderer = parseeval.RendererCapability{
		Health:     parseeval.CachedDependencyHealth{Healthy: rendererHealthy, Reason: rendererReason, CheckedAt: rendererHealth.CheckedAt, ExpiresAt: rendererHealth.ExpiresAt},
		Descriptor: rendererHealth.Descriptor,
	}
	if rendererHealthy && rendererHealth.Limits.MaxInputBytes > 0 {
		capabilities.MaxFileBytes = minPositiveInt64(capabilities.MaxFileBytes, rendererHealth.Limits.MaxInputBytes)
	}
	parserHealth := g.parserEval.parsers.HealthSnapshots()
	parsersHealthy := true
	for _, engine := range []parseeval.Engine{parseeval.EngineMarkItDown, parseeval.EngineAnyDoc} {
		snapshot := parserHealth[string(engine)]
		expected, _ := sidecar.OfficeParserDescriptor(string(engine))
		healthy := snapshot.Healthy && !snapshot.ExpiresAt.IsZero() && time.Now().Before(snapshot.ExpiresAt) && snapshot.Office.Enabled && supportsParserEvaluationFormats(snapshot.Office.Formats)
		reason := snapshot.Reason
		if snapshot.Healthy && !healthy && reason == "" {
			if snapshot.ExpiresAt.IsZero() || !time.Now().Before(snapshot.ExpiresAt) {
				reason = "health_stale"
			} else {
				reason = "office_capability_mismatch"
			}
		}
		capabilities.Parsers = append(capabilities.Parsers, parseeval.ParserCapability{
			Engine:     engine,
			Health:     parseeval.CachedDependencyHealth{Healthy: healthy, Reason: reason, CheckedAt: snapshot.CheckedAt, ExpiresAt: snapshot.ExpiresAt},
			Descriptor: parseeval.ParserDescriptor{Name: expected.Name, Version: expected.Version, WrapperVersion: expected.WrapperVersion},
		})
		parsersHealthy = parsersHealthy && healthy
		if healthy && snapshot.MaxInputBytes > 0 {
			capabilities.MaxFileBytes = minPositiveInt64(capabilities.MaxFileBytes, snapshot.MaxInputBytes)
		}
	}
	bindings, err := g.ListParserEvaluationJudgeBindings(ctx, ownerID)
	if err != nil {
		return parseeval.Capabilities{}, err
	}
	capabilities.JudgeModelBindings = bindings
	switch {
	case !cfg.WorkerEnabled:
		capabilities.Reason = "worker_disabled"
	case !rendererHealthy:
		capabilities.Reason = "renderer_unhealthy"
	case !parsersHealthy:
		capabilities.Reason = "parser_unhealthy"
	case len(bindings) == 0:
		capabilities.Reason = "no_vision_judge_model"
	default:
		capabilities.Available = true
	}
	return capabilities, nil
}

func supportsParserEvaluationFormats(formats []string) bool {
	wanted := map[string]bool{"docx": false, "pptx": false, "xlsx": false}
	for _, format := range formats {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(format))]; ok {
			wanted[strings.ToLower(strings.TrimSpace(format))] = true
		}
	}
	return wanted["docx"] && wanted["pptx"] && wanted["xlsx"]
}

func minPositiveInt64(a, b int64) int64 {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}
