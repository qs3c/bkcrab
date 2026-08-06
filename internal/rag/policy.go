package rag

import (
	"errors"
	"sync/atomic"

	"github.com/qs3c/bkcrab/internal/config"
)

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
	if err := next.Validate(); err != nil {
		return err
	}
	copy := next
	s.current.Store(&copy)
	return nil
}

func DefaultRuntimePolicy(cfg config.RAGCfg) config.RAGRuntimePolicyData {
	cfg.ApplyDefaults()
	return config.RAGRuntimePolicyData{Version: 1, TopN: 5, CandidateTopK: cfg.Reranker.CandidateTopK, MinScore: cfg.Reranker.MinScore, Temperature: .2, MaxTokens: 4096, RAGPromptBundleVersion: "rag-answer-v1"}
}
