package imagegen

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type ProviderCallGate interface {
	Call(ctx context.Context, provider, model string, call func(context.Context) (ProviderResult, error)) (ProviderResult, error)
}

type passThroughProviderCallGate struct{}

func (passThroughProviderCallGate) Call(ctx context.Context, _, _ string, call func(context.Context) (ProviderResult, error)) (ProviderResult, error) {
	return call(ctx)
}

type GenerationService struct {
	resolver        ProviderPlanResolver
	registry        *toolproviders.Registry
	gate            ProviderCallGate
	providerTimeout time.Duration
}

func NewGenerationService(resolver ProviderPlanResolver, registry *toolproviders.Registry, gate ProviderCallGate, providerTimeout time.Duration) *GenerationService {
	if gate == nil {
		gate = passThroughProviderCallGate{}
	}
	if providerTimeout <= 0 {
		providerTimeout = 120 * time.Second
	}
	return &GenerationService{resolver: resolver, registry: registry, gate: gate, providerTimeout: providerTimeout}
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
				callCtx, cancel := context.WithTimeout(ctx, s.providerTimeout)
				result, callErr := s.gate.Call(callCtx, reference.Provider, reference.Model, func(gatedCtx context.Context) (ProviderResult, error) {
					return backend.Generate(gatedCtx, candidate.Config, request)
				})
				cancel()
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
