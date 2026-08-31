package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestParserEvaluationJudgeCatalogFiltersVisionAndIncompleteProviders(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"vision": {
			APIKey: "secret", APIBase: "https://example.test/v1", APIType: "openai",
			Models: []config.ModelEntry{
				{ID: "text-only", Name: "Text", Input: []string{"text"}},
				{ID: "vision-model", Name: "Vision", Input: []string{"text", "IMAGE"}, Cost: config.ModelCost{Input: 2, Output: 4}},
			},
		},
		"no-secret": {APIBase: "https://example.test/v1", Models: []config.ModelEntry{{ID: "vision", Input: []string{"image"}}}},
	}}
	bindings := listParserEvaluationJudgeBindings(cfg)
	if len(bindings) != 1 || bindings[0].ID != "vision/vision-model" || bindings[0].ModelDisplayName != "Vision" || !bindings[0].PricingKnown {
		t.Fatalf("bindings=%+v", bindings)
	}
	if strings.Contains(bindings[0].Fingerprint, "secret") {
		t.Fatal("fingerprint exposed provider secret")
	}
}

func TestParserEvaluationBindingFingerprintIsSecretFreeAndSensitive(t *testing.T) {
	model := config.ModelEntry{ID: "vision", Name: "Vision", Input: []string{"text", "image"}, Cost: config.ModelCost{Input: 1, Output: 2}}
	providerCfg := config.ProviderConfig{APIKey: "first", APIBase: "https://one.test/v1", APIType: "openai", AuthType: "bearer"}
	first, err := parserEvaluationBinding("provider", providerCfg, model)
	if err != nil {
		t.Fatal(err)
	}
	providerCfg.APIKey = "rotated"
	rotated, _ := parserEvaluationBinding("provider", providerCfg, model)
	if first.Fingerprint != rotated.Fingerprint {
		t.Fatal("API key rotation changed secret-free fingerprint")
	}
	providerCfg.APIBase = "https://two.test/v1"
	changedEndpoint, _ := parserEvaluationBinding("provider", providerCfg, model)
	if first.Fingerprint == changedEndpoint.Fingerprint {
		t.Fatal("endpoint change did not change fingerprint")
	}
	providerCfg.APIBase = "https://one.test/v1"
	model.Input = []string{"text"}
	changedModel, _ := parserEvaluationBinding("provider", providerCfg, model)
	if first.Fingerprint == changedModel.Fingerprint {
		t.Fatal("model metadata change did not change fingerprint")
	}
}

func TestParserEvaluationJudgeResolverRequiresActiveSuperAdminAndFrozenBinding(t *testing.T) {
	database, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, user := range []store.UserRecord{
		{ID: "u_admin", Username: "admin", Email: "admin@example.test", Role: users.RoleSuperAdmin, Status: users.StatusActive},
		{ID: "u_user", Username: "user", Email: "user@example.test", Role: users.RoleUser, Status: users.StatusActive},
	} {
		copy := user
		if err := database.CreateUser(ctx, &copy); err != nil {
			t.Fatal(err)
		}
	}
	providerCfg := config.ProviderConfig{
		APIKey: "secret", APIBase: "https://example.test/v1", APIType: "openai",
		Models: []config.ModelEntry{{ID: "vision", Name: "Vision", Input: []string{"image"}}},
	}
	if err := scope.SaveProvider(ctx, database, "u_admin", "", "judge", providerCfg); err != nil {
		t.Fatal(err)
	}
	cfg, err := assembleConfig(ctx, database, "u_admin", "")
	if err != nil {
		t.Fatal(err)
	}
	bindings := listParserEvaluationJudgeBindings(cfg)
	if len(bindings) != 1 {
		t.Fatalf("bindings=%+v", bindings)
	}
	resolve := parserEvaluationJudgeResolver(database)
	if model, err := resolve(ctx, "u_admin", bindings[0]); err != nil || model == nil {
		t.Fatalf("resolve model=%T err=%v", model, err)
	}
	if _, err := resolve(ctx, "u_user", bindings[0]); !errors.Is(err, ErrParserEvaluationJudgeUnavailable) {
		t.Fatalf("ordinary user err=%v", err)
	}
	changed := bindings[0]
	changed.Fingerprint = strings.Repeat("f", 64)
	if _, err := resolve(ctx, "u_admin", changed); !errors.Is(err, ErrParserEvaluationJudgeUnavailable) {
		t.Fatalf("changed binding err=%v", err)
	}
}

func TestParserEvaluationDisabledAssemblyHasNoDependencies(t *testing.T) {
	runtime, err := buildParserEvaluationRuntime(config.ParserEvaluationCfg{Enabled: false}, nil, nil, config.ObjectStoreCfg{Type: "unsupported"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if runtime == nil || runtime.service != nil || runtime.runner != nil || runtime.renderer != nil || runtime.parsers != nil {
		t.Fatalf("disabled runtime=%+v", runtime)
	}
}

func TestParserEvaluationEnabledAssemblyBuildsIsolatedRuntime(t *testing.T) {
	database, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultParserEvaluationCfg()
	cfg.Enabled = true
	cfg.WorkerEnabled = false
	runtime, err := buildParserEvaluationRuntime(cfg, database, nil, config.ObjectStoreCfg{Type: "local", Local: config.ObjectStoreLocalCfg{Root: t.TempDir()}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.service == nil || runtime.runner == nil || runtime.cleanup == nil || runtime.renderer == nil || runtime.parsers == nil {
		t.Fatalf("incomplete runtime=%+v", runtime)
	}
	if runtime.config.WorkerEnabled {
		t.Fatal("workerEnabled=false was not preserved")
	}
}
