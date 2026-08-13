package imagegen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type passThroughProviderCallGate struct{}

func (passThroughProviderCallGate) Acquire(_ context.Context, provider, model, token string, _ int, _ time.Duration) (ProviderLease, bool, error) {
	return ProviderLease{Provider: provider, Model: model, Key: provider + ":" + model, Token: token}, true, nil
}
func (passThroughProviderCallGate) Renew(context.Context, ProviderLease, time.Duration) error {
	return nil
}
func (passThroughProviderCallGate) Release(context.Context, ProviderLease) error {
	return nil
}

type GenerationService struct {
	resolver         ProviderPlanResolver
	registry         *toolproviders.Registry
	gate             ProviderCallGate
	providerTimeout  time.Duration
	providerLimit    int
	providerLimits   map[string]int
	providerLeaseTTL time.Duration
}

func NewGenerationService(resolver ProviderPlanResolver, registry *toolproviders.Registry, gate ProviderCallGate, providerTimeout time.Duration) *GenerationService {
	if gate == nil {
		gate = passThroughProviderCallGate{}
	}
	if providerTimeout <= 0 {
		providerTimeout = 120 * time.Second
	}
	return &GenerationService{
		resolver: resolver, registry: registry, gate: gate, providerTimeout: providerTimeout,
		providerLimit: 4, providerLimits: make(map[string]int), providerLeaseTTL: 30 * time.Second,
	}
}

// ConfigureProviderLimits applies immutable deployment-level physical call
// limits. It must be called before the service is shared by workers.
func (s *GenerationService) ConfigureProviderLimits(defaultLimit int, overrides map[string]int, leaseTTL time.Duration) *GenerationService {
	if s == nil {
		return s
	}
	if defaultLimit > 0 {
		s.providerLimit = defaultLimit
	}
	s.providerLimits = make(map[string]int, len(overrides))
	for provider, limit := range overrides {
		if provider != "" && limit > 0 {
			s.providerLimits[provider] = limit
		}
	}
	if leaseTTL > 0 {
		s.providerLeaseTTL = leaseTTL
	}
	return s
}

func (s *GenerationService) Generate(ctx context.Context, identity ExecutionIdentity, plan SafeProviderPlan, request GenerateRequest) (ProviderResult, error) {
	if err := request.Validate(); err != nil {
		return ProviderResult{}, err
	}
	if err := identity.Validate(); err != nil {
		return ProviderResult{}, NewProviderError(ErrorInvalidRequest, "", 0, err)
	}
	if err := plan.Validate(); err != nil || !plan.MatchesIdentity(identity) {
		if err == nil {
			err = errors.New("provider plan identity mismatch")
		}
		return ProviderResult{}, NewProviderError(ErrorInvalidRequest, "", 0, err)
	}
	if s == nil || s.resolver == nil || s.registry == nil || s.gate == nil {
		return ProviderResult{}, NewProviderError(ErrorAuthConfig, "", 0, errors.New("generation service is not configured"))
	}

	attempts := make([]AttemptSummary, 0, len(plan.Candidates))
	for index, reference := range plan.Candidates {
		current, err := s.resolver.Resolve(ctx, identity, plan)
		if err != nil {
			providerErr := NewProviderError(ErrorAuthConfig, reference.Provider, 0, err)
			attempts = append(attempts, summarizeAttempt(reference, providerErr))
			return ProviderResult{Attempts: attempts}, providerErr
		}
		candidate, ok := resolvedCandidate(current, reference)
		if !ok || !candidate.Configured {
			err = NewProviderError(ErrorAuthConfig, reference.Provider, 0, errors.New("provider configuration unavailable"))
		} else {
			provider := s.registry.Get("image_gen", reference.Provider)
			backend, backendOK := provider.(Backend)
			if !backendOK || backend.Name() != reference.Provider {
				err = NewProviderError(ErrorModelUnavailable, reference.Provider, 0, errors.New("typed backend unavailable"))
			} else if !backend.Capability(reference.Model).Supports(request.Size, request.Count) {
				err = NewProviderError(ErrorModelUnavailable, reference.Provider, 0, errors.New("provider capability does not support request"))
			} else {
				candidate.Config.Model = reference.Model
				result, callErr := s.callProvider(ctx, backend, reference, candidate.Config, request)
				if callErr == nil {
					switch {
					case len(result.Images) == 0:
						callErr = NewProviderError(ErrorEmptyResult, reference.Provider, 0, errors.New("provider returned no images"))
					case len(result.Images) != request.Count:
						callErr = NewProviderError(ErrorIncompleteResult, reference.Provider, 0, fmt.Errorf("provider returned %d images", len(result.Images)))
					}
				}
				if callErr == nil {
					if result.Provider == "" {
						result.Provider = reference.Provider
					}
					if result.Model == "" {
						result.Model = reference.Model
					}
					attempts = append(attempts, AttemptSummary{Provider: reference.Provider, Model: reference.Model})
					result.Attempts = append(attempts, result.Attempts...)
					return result, nil
				}
				err = normalizeProviderError(reference.Provider, callErr)
			}
		}

		attempts = append(attempts, summarizeAttempt(reference, err))
		kind := ProviderErrorKind(err)
		terminal := kind == ErrorInvalidRequest || kind == ErrorSafetyRejected
		if terminal || !plan.AutoFallback || index == len(plan.Candidates)-1 {
			return ProviderResult{Attempts: attempts}, err
		}
	}
	return ProviderResult{Attempts: attempts}, NewProviderError(ErrorModelUnavailable, "", 0, errors.New("provider chain exhausted"))
}

func (s *GenerationService) callProvider(ctx context.Context, backend Backend, reference ProviderCandidateRef, cfg ResolvedProviderConfig, request GenerateRequest) (result ProviderResult, err error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return ProviderResult{}, NewProviderError(ErrorRateLimited, reference.Provider, 0, fmt.Errorf("create provider limiter token: %w", err))
	}
	limit := s.providerLimit
	if override := s.providerLimits[reference.Provider]; override > 0 {
		limit = override
	}
	lease, acquired, acquireErr := s.gate.Acquire(ctx, reference.Provider, reference.Model, hex.EncodeToString(tokenBytes), limit, s.providerLeaseTTL)
	if acquireErr != nil {
		return ProviderResult{}, NewProviderError(ErrorRateLimited, reference.Provider, 0, acquireErr)
	}
	if !acquired {
		return ProviderResult{}, NewProviderError(ErrorRateLimited, reference.Provider, 0, errors.New("provider physical concurrency limit reached"))
	}

	callCtx, cancel := context.WithTimeout(ctx, s.providerTimeout)
	stopRenew := make(chan struct{})
	renewDone := make(chan struct{})
	renewErr := make(chan error, 1)
	interval := s.providerLeaseTTL / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopRenew:
				return
			case <-callCtx.Done():
				return
			case <-ticker.C:
				if renew := s.gate.Renew(callCtx, lease, s.providerLeaseTTL); renew != nil {
					select {
					case renewErr <- renew:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	renewStopped := false
	stop := func() {
		if renewStopped {
			return
		}
		renewStopped = true
		cancel()
		close(stopRenew)
		<-renewDone
	}
	defer func() {
		stop()
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer releaseCancel()
		_ = s.gate.Release(releaseCtx, lease)
	}()

	result, err = backend.Generate(callCtx, cfg, request)
	stop()
	select {
	case leaseErr := <-renewErr:
		return ProviderResult{}, NewProviderError(ErrorRateLimited, reference.Provider, 0, leaseErr)
	default:
		return result, err
	}
}

func resolvedCandidate(plan ResolvedProviderPlan, reference ProviderCandidateRef) (ResolvedProviderCandidate, bool) {
	for _, candidate := range plan.Candidates {
		if candidate.Provider == reference.Provider && candidate.Model == reference.Model {
			return candidate, true
		}
	}
	return ResolvedProviderCandidate{}, false
}

func normalizeProviderError(provider string, err error) error {
	if ProviderErrorKind(err) != "" {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewProviderError(ErrorUpstreamTransient, provider, 0, err)
	}
	return NewProviderError(ErrorUpstreamTransient, provider, 0, err)
}

func summarizeAttempt(reference ProviderCandidateRef, err error) AttemptSummary {
	summary := AttemptSummary{Provider: reference.Provider, Model: reference.Model, Kind: ProviderErrorKind(err)}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) && providerErr.StatusCode > 0 {
		summary.Message = fmt.Sprintf("status=%d", providerErr.StatusCode)
	}
	return summary
}
