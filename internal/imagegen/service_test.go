package imagegen

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type generationServiceResolver struct {
	mu      sync.Mutex
	calls   int
	configs map[string]string
}

func (r *generationServiceResolver) Snapshot(context.Context, ExecutionIdentity) (SafeProviderPlan, error) {
	return SafeProviderPlan{}, errors.New("unused")
}

func (r *generationServiceResolver) Resolve(_ context.Context, _ ExecutionIdentity, plan SafeProviderPlan) (ResolvedProviderPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	out := ResolvedProviderPlan{Version: plan.Version, AutoFallback: plan.AutoFallback}
	for _, candidate := range plan.Candidates {
		out.Candidates = append(out.Candidates, ResolvedProviderCandidate{
			Provider: candidate.Provider, Model: candidate.Model, Configured: r.configs[candidate.Provider] != "",
			Config: ResolvedProviderConfig{APIKey: r.configs[candidate.Provider]},
		})
	}
	return out, nil
}

type generationServiceBackend struct {
	name       string
	capability Capability
	generate   func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error)
}

type generationServiceGate struct {
	mu        sync.Mutex
	denied    map[string]bool
	attempted []ProviderLease
	acquired  []ProviderLease
	renewed   []ProviderLease
	released  []ProviderLease
}

func (g *generationServiceGate) Acquire(_ context.Context, provider, model, token string, _ int, _ time.Duration) (ProviderLease, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	lease := ProviderLease{Provider: provider, Model: model, Key: provider + ":" + model, Token: token}
	g.attempted = append(g.attempted, lease)
	if g.denied[provider] {
		return lease, false, nil
	}
	g.acquired = append(g.acquired, lease)
	return lease, true, nil
}

func (g *generationServiceGate) Renew(_ context.Context, lease ProviderLease, _ time.Duration) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.renewed = append(g.renewed, lease)
	return nil
}

func (g *generationServiceGate) Release(_ context.Context, lease ProviderLease) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.released = append(g.released, lease)
	return nil
}

func (g *generationServiceGate) counts() (int, int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.acquired), len(g.renewed), len(g.released)
}

func (b *generationServiceBackend) Category() string { return "image_gen" }
func (b *generationServiceBackend) Name() string     { return b.name }
func (b *generationServiceBackend) Execute(context.Context, toolproviders.Request) (toolproviders.Response, error) {
	return toolproviders.Response{}, errors.New("unused")
}
func (b *generationServiceBackend) Capability(string) Capability { return b.capability }
func (b *generationServiceBackend) Generate(ctx context.Context, cfg ResolvedProviderConfig, req GenerateRequest) (ProviderResult, error) {
	return b.generate(ctx, cfg, req)
}

func generationServicePlan(auto bool, names ...string) SafeProviderPlan {
	plan := SafeProviderPlan{
		Version: ProviderPlanSchemaVersion, ConfigUserID: "config", AgentOwnerUserID: "owner", AgentID: "agent",
		AutoFallback: auto,
	}
	for _, name := range names {
		plan.Candidates = append(plan.Candidates, ProviderCandidateRef{Provider: name, Model: "model"})
	}
	return plan
}

func generationServiceIdentity() ExecutionIdentity {
	return ExecutionIdentity{UserID: "tenant", ConfigUserID: "config", AgentOwnerUserID: "owner", AgentID: "agent"}
}

func TestGenerationServiceFallbackRefreshesConfigAndRequiresExactCount(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"alpha": "alpha-secret", "beta": "beta-secret"}}
	registry := toolproviders.NewRegistry()
	var calls []string
	registry.Register(&generationServiceBackend{name: "alpha", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeSquare}}, generate: func(_ context.Context, cfg ResolvedProviderConfig, _ GenerateRequest) (ProviderResult, error) {
		calls = append(calls, "alpha:"+cfg.APIKey)
		return ProviderResult{}, NewProviderError(ErrorRateLimited, "alpha", 429, errors.New("secret response body"))
	}})
	registry.Register(&generationServiceBackend{name: "beta", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeSquare}}, generate: func(_ context.Context, cfg ResolvedProviderConfig, req GenerateRequest) (ProviderResult, error) {
		calls = append(calls, "beta:"+cfg.APIKey)
		return ProviderResult{Images: make([]GeneratedImage, req.Count), Provider: "beta", Model: "model"}, nil
	}})

	service := NewGenerationService(resolver, registry, nil, time.Second)
	result, err := service.Generate(context.Background(), generationServiceIdentity(), generationServicePlan(true, "alpha", "beta"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 2})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if result.Provider != "beta" || len(result.Images) != 2 || len(result.Attempts) != 2 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if resolver.calls != 2 {
		t.Fatalf("current config should resolve per candidate, calls=%d", resolver.calls)
	}
	if len(calls) != 2 || calls[0] != "alpha:alpha-secret" || calls[1] != "beta:beta-secret" {
		t.Fatalf("fallback/config order: %#v", calls)
	}
	for _, attempt := range result.Attempts {
		if len(attempt.Message) > 256 || attempt.Message == "secret response body" {
			t.Fatalf("attempt summary leaked or unbounded: %#v", attempt)
		}
	}
}

func TestGenerationServiceSafetyStopsFallback(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"alpha": "key", "beta": "key"}}
	registry := toolproviders.NewRegistry()
	secondCalled := false
	registry.Register(&generationServiceBackend{name: "alpha", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
		return ProviderResult{}, NewProviderError(ErrorSafetyRejected, "alpha", 400, errors.New("policy"))
	}})
	registry.Register(&generationServiceBackend{name: "beta", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
		secondCalled = true
		return ProviderResult{Images: []GeneratedImage{{SourceURL: "https://example.invalid/image.png"}}}, nil
	}})
	_, err := NewGenerationService(resolver, registry, nil, time.Second).Generate(context.Background(), generationServiceIdentity(), generationServicePlan(true, "alpha", "beta"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
	if ProviderErrorKind(err) != ErrorSafetyRejected || secondCalled {
		t.Fatalf("safety fallback contract broken: kind=%s second=%v err=%v", ProviderErrorKind(err), secondCalled, err)
	}
}

func TestGenerationServiceSkipsCapabilityAndFallsBackOnIncomplete(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"small": "key", "short": "key", "good": "key"}}
	registry := toolproviders.NewRegistry()
	smallCalled := false
	registry.Register(&generationServiceBackend{name: "small", capability: Capability{MaxImagesPerCall: 1, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
		smallCalled = true
		return ProviderResult{}, nil
	}})
	registry.Register(&generationServiceBackend{name: "short", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeLandscape}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
		return ProviderResult{Images: []GeneratedImage{{SourceURL: "https://example.invalid/one.png"}}}, nil
	}})
	registry.Register(&generationServiceBackend{name: "good", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeLandscape}}, generate: func(_ context.Context, _ ResolvedProviderConfig, req GenerateRequest) (ProviderResult, error) {
		return ProviderResult{Images: make([]GeneratedImage, req.Count), Provider: "good"}, nil
	}})
	result, err := NewGenerationService(resolver, registry, nil, time.Second).Generate(context.Background(), generationServiceIdentity(), generationServicePlan(true, "small", "short", "good"), GenerateRequest{Prompt: "prompt", Size: SizeLandscape, Count: 2})
	if err != nil || result.Provider != "good" {
		t.Fatalf("fallback result=%#v err=%v", result, err)
	}
	if smallCalled {
		t.Fatal("backend called despite unsupported count/size capability")
	}
	if len(result.Attempts) != 3 || result.Attempts[0].Kind != ErrorModelUnavailable || result.Attempts[1].Kind != ErrorIncompleteResult {
		t.Fatalf("attempt classification: %#v", result.Attempts)
	}
}

func TestGenerationServiceValidatesRequestAndHonorsCancellation(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"slow": "key"}}
	registry := toolproviders.NewRegistry()
	registry.Register(&generationServiceBackend{name: "slow", capability: Capability{MaxImagesPerCall: 4, SupportedSizes: []string{SizeSquare}}, generate: func(ctx context.Context, _ ResolvedProviderConfig, _ GenerateRequest) (ProviderResult, error) {
		<-ctx.Done()
		return ProviderResult{}, ctx.Err()
	}})
	service := NewGenerationService(resolver, registry, nil, 20*time.Millisecond)
	if _, err := service.Generate(context.Background(), generationServiceIdentity(), generationServicePlan(false, "slow"), GenerateRequest{Prompt: "", Size: SizeSquare, Count: 1}); ProviderErrorKind(err) != ErrorInvalidRequest {
		t.Fatalf("invalid request kind=%s err=%v", ProviderErrorKind(err), err)
	}
	_, err := service.Generate(context.Background(), generationServiceIdentity(), generationServicePlan(false, "slow"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
	if ProviderErrorKind(err) != ErrorUpstreamTransient {
		t.Fatalf("timeout kind=%s err=%v", ProviderErrorKind(err), err)
	}
}

func TestGenerationServiceLimiterDenialFallsBackAndUsesFreshToken(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"alpha": "key", "beta": "key"}}
	registry := toolproviders.NewRegistry()
	var calls []string
	for _, name := range []string{"alpha", "beta"} {
		name := name
		registry.Register(&generationServiceBackend{name: name, capability: Capability{MaxImagesPerCall: 1, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
			calls = append(calls, name)
			return ProviderResult{Images: []GeneratedImage{{SourceURL: "https://example.invalid/image.png"}}}, nil
		}})
	}
	gate := &generationServiceGate{denied: map[string]bool{"alpha": true}}
	service := NewGenerationService(resolver, registry, gate, time.Second)
	result, err := service.Generate(context.Background(), generationServiceIdentity(), generationServicePlan(true, "alpha", "beta"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
	if err != nil || result.Provider != "beta" || len(calls) != 1 || calls[0] != "beta" {
		t.Fatalf("limiter fallback result=%#v calls=%#v err=%v", result, calls, err)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].Kind != ErrorRateLimited {
		t.Fatalf("limiter attempt classification: %#v", result.Attempts)
	}
	gate.mu.Lock()
	if len(gate.attempted) != 2 || gate.attempted[0].Token == gate.attempted[1].Token {
		t.Fatalf("each physical fallback call needs a fresh token: %#v", gate.attempted)
	}
	gate.mu.Unlock()
	acquired, _, released := gate.counts()
	if acquired != 1 || released != 1 {
		t.Fatalf("lease lifecycle acquired=%d released=%d", acquired, released)
	}
}

func TestGenerationServiceLimiterAllDeniedReturnsRateLimitedWithoutWaiting(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"alpha": "key", "beta": "key"}}
	registry := toolproviders.NewRegistry()
	called := false
	for _, name := range []string{"alpha", "beta"} {
		registry.Register(&generationServiceBackend{name: name, capability: Capability{MaxImagesPerCall: 1, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
			called = true
			return ProviderResult{}, nil
		}})
	}
	gate := &generationServiceGate{denied: map[string]bool{"alpha": true, "beta": true}}
	started := time.Now()
	_, err := NewGenerationService(resolver, registry, gate, time.Second).Generate(context.Background(), generationServiceIdentity(), generationServicePlan(true, "alpha", "beta"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
	if ProviderErrorKind(err) != ErrorRateLimited || called || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("all denied kind=%s called=%v elapsed=%s err=%v", ProviderErrorKind(err), called, time.Since(started), err)
	}
}

func TestGenerationServiceLimiterReleasesOnCancelAndPanic(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		resolver := &generationServiceResolver{configs: map[string]string{"slow": "key"}}
		registry := toolproviders.NewRegistry()
		registry.Register(&generationServiceBackend{name: "slow", capability: Capability{MaxImagesPerCall: 1, SupportedSizes: []string{SizeSquare}}, generate: func(ctx context.Context, _ ResolvedProviderConfig, _ GenerateRequest) (ProviderResult, error) {
			<-ctx.Done()
			return ProviderResult{}, ctx.Err()
		}})
		gate := &generationServiceGate{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = NewGenerationService(resolver, registry, gate, time.Second).Generate(ctx, generationServiceIdentity(), generationServicePlan(false, "slow"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
		acquired, _, released := gate.counts()
		if acquired != 1 || released != 1 {
			t.Fatalf("cancel lease lifecycle acquired=%d released=%d", acquired, released)
		}
	})

	t.Run("panic", func(t *testing.T) {
		resolver := &generationServiceResolver{configs: map[string]string{"panic": "key"}}
		registry := toolproviders.NewRegistry()
		registry.Register(&generationServiceBackend{name: "panic", capability: Capability{MaxImagesPerCall: 1, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
			panic("provider panic")
		}})
		gate := &generationServiceGate{}
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("expected provider panic")
				}
			}()
			_, _ = NewGenerationService(resolver, registry, gate, time.Second).Generate(context.Background(), generationServiceIdentity(), generationServicePlan(false, "panic"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
		}()
		acquired, _, released := gate.counts()
		if acquired != 1 || released != 1 {
			t.Fatalf("panic lease lifecycle acquired=%d released=%d", acquired, released)
		}
	})
}

func TestGenerationServiceLimiterRenewsLongProviderCall(t *testing.T) {
	resolver := &generationServiceResolver{configs: map[string]string{"slow": "key"}}
	registry := toolproviders.NewRegistry()
	registry.Register(&generationServiceBackend{name: "slow", capability: Capability{MaxImagesPerCall: 1, SupportedSizes: []string{SizeSquare}}, generate: func(context.Context, ResolvedProviderConfig, GenerateRequest) (ProviderResult, error) {
		time.Sleep(45 * time.Millisecond)
		return ProviderResult{Images: []GeneratedImage{{SourceURL: "https://example.invalid/image.png"}}}, nil
	}})
	gate := &generationServiceGate{}
	service := NewGenerationService(resolver, registry, gate, time.Second)
	service.providerLeaseTTL = 30 * time.Millisecond
	_, err := service.Generate(context.Background(), generationServiceIdentity(), generationServicePlan(false, "slow"), GenerateRequest{Prompt: "prompt", Size: SizeSquare, Count: 1})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	acquired, renewed, released := gate.counts()
	if acquired != 1 || renewed < 1 || released != 1 {
		t.Fatalf("long-call lease lifecycle acquired=%d renewed=%d released=%d", acquired, renewed, released)
	}
}
