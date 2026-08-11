package rag

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/qs3c/bkcrab/internal/config"
)

// RuntimePolicyProvider is the process-wide read boundary. A database polling
// or notification implementation can replace RuntimePolicySnapshot without
// changing request code; every request still calls Current exactly once.
type RuntimePolicyProvider interface {
	Current() config.RAGRuntimePolicyData
}

// RuntimePolicySnapshot provides request-scoped immutable reads and lock-free
// atomic publication. Callers capture Current once at request start.
type RuntimePolicySnapshot struct {
	current atomic.Pointer[config.RAGRuntimePolicyData]
}

func NewRuntimePolicySnapshot(initial config.RAGRuntimePolicyData) (*RuntimePolicySnapshot, error) {
	snapshot := &RuntimePolicySnapshot{}
	if err := snapshot.Publish(initial); err != nil {
		return nil, err
	}
	return snapshot, nil
}
func (s *RuntimePolicySnapshot) Current() config.RAGRuntimePolicyData {
	if s == nil {
		return config.RAGRuntimePolicyData{}
	}
	value := s.current.Load()
	if value == nil {
		return config.RAGRuntimePolicyData{}
	}
	return *value
}
func (s *RuntimePolicySnapshot) Publish(next config.RAGRuntimePolicyData) error {
	if s == nil {
		return errors.New("nil runtime policy snapshot")
	}
	if next.Version <= 0 {
		return errors.New("runtime policy version must be positive")
	}
	next.RAGPromptBundleVersion = strings.TrimSpace(next.RAGPromptBundleVersion)
	if err := next.Validate(); err != nil {
		return err
	}
	if _, ok := answerPromptBundles[next.RAGPromptBundleVersion]; !ok {
		return errors.New("runtime policy references unknown answer prompt bundle")
	}
	copy := next
	s.current.Store(&copy)
	return nil
}

func DefaultRuntimePolicy(cfg config.RAGCfg) config.RAGRuntimePolicyData {
	cfg.ApplyDefaults()
	candidateTopK := cfg.Reranker.CandidateTopK
	if candidateTopK < 5 {
		candidateTopK = 5
	}
	return config.RAGRuntimePolicyData{Version: 1, TopN: 5, CandidateTopK: candidateTopK, MinScore: cfg.Reranker.MinScore, Temperature: .2, MaxTokens: 4096, RAGPromptBundleVersion: RAGAnswerPromptBundleV1}
}

// DefaultIngestionPolicy converts executable provider configuration into a
// credential-free bootstrap revision. Endpoints participate only in the
// opaque contract fingerprint and are never persisted in policy JSON.
func DefaultIngestionPolicy(cfg config.RAGCfg) config.RAGIngestionPolicyData {
	cfg.ApplyDefaults()
	return config.RAGIngestionPolicyData{
		Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard,
		DocumentAI: config.RAGPolicyDocumentAIData{
			VisionModel:             strings.TrimSpace(cfg.DocumentAI.VisionModel),
			TextModel:               strings.TrimSpace(cfg.DocumentAI.TextModel),
			VisionPromptVersion:     strings.TrimSpace(cfg.DocumentAI.VisionPromptVersion),
			EnrichmentPromptVersion: strings.TrimSpace(cfg.DocumentAI.EnrichmentPromptVersion),
		},
		Embedding: config.RAGPolicyEmbeddingData{
			ContractFingerprint: embeddingContractFingerprint("system", cfg.Embedding, cfg.Embedding.Model, cfg.Embedding.Dims),
			Model:               strings.TrimSpace(cfg.Embedding.Model), Dims: cfg.Embedding.Dims,
		},
	}
}

type runtimePolicyContextKey struct{}

// CaptureRuntimePolicy attaches one immutable value snapshot to ctx. Repeated
// calls on the same request return the already captured revision, even if a
// concurrent publisher swaps the process-wide provider in the meantime.
func (s *Service) CaptureRuntimePolicy(ctx context.Context) (context.Context, config.RAGRuntimePolicyData) {
	if captured, ok := ctx.Value(runtimePolicyContextKey{}).(config.RAGRuntimePolicyData); ok {
		return ctx, captured
	}
	cfg := config.RAGCfg{}
	if s != nil {
		cfg = s.cfg
	}
	policy := DefaultRuntimePolicy(cfg)
	if s != nil && s.runtimePolicy != nil {
		policy = s.runtimePolicy.Current()
	}
	return context.WithValue(ctx, runtimePolicyContextKey{}, policy), policy
}

func (s *Service) RuntimePolicyProvider() RuntimePolicyProvider {
	if s == nil {
		return nil
	}
	return s.runtimePolicy
}
