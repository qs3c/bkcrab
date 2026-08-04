package imagegen

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const ProviderPlanSchemaVersion = 1

// ProviderCandidateRef is safe to persist. It identifies an adapter and model,
// never a credential, resolved endpoint, response URL, or in-memory backend.
type ProviderCandidateRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SafeProviderPlan freezes the submission-time chain while retaining only the
// identity references needed to reconstruct current credentials at execution.
type SafeProviderPlan struct {
	Version          int                    `json:"version"`
	ConfigUserID     string                 `json:"config_user_id"`
	AgentOwnerUserID string                 `json:"agent_owner_user_id"`
	AgentID          string                 `json:"agent_id"`
	AutoFallback     bool                   `json:"auto_fallback"`
	Candidates       []ProviderCandidateRef `json:"candidates"`
}

func (p SafeProviderPlan) Validate() error {
	if p.Version != ProviderPlanSchemaVersion {
		return fmt.Errorf("imagegen: unsupported provider plan version %d", p.Version)
	}
	for name, value := range map[string]string{
		"config user ID": p.ConfigUserID, "agent owner user ID": p.AgentOwnerUserID, "agent ID": p.AgentID,
	} {
		if strings.TrimSpace(value) == "" || len(value) > 120 {
			return fmt.Errorf("imagegen: provider plan %s is required and bounded", name)
		}
	}
	if len(p.Candidates) < 1 || len(p.Candidates) > 16 {
		return errors.New("imagegen: provider plan candidates must be in [1,16]")
	}
	seen := make(map[ProviderCandidateRef]struct{}, len(p.Candidates))
	for index, candidate := range p.Candidates {
		if strings.TrimSpace(candidate.Provider) != candidate.Provider || candidate.Provider == "" || len(candidate.Provider) > 120 ||
			strings.TrimSpace(candidate.Model) != candidate.Model || len(candidate.Model) > 240 ||
			strings.ContainsAny(candidate.Provider+candidate.Model, "\r\n\x00") {
			return fmt.Errorf("imagegen: invalid provider candidate at index %d", index)
		}
		if _, exists := seen[candidate]; exists {
			return fmt.Errorf("imagegen: duplicate provider candidate at index %d", index)
		}
		seen[candidate] = struct{}{}
	}
	return nil
}

func (p SafeProviderPlan) MatchesIdentity(identity ExecutionIdentity) bool {
	return p.ConfigUserID == identity.ConfigUserID &&
		p.AgentOwnerUserID == identity.AgentOwnerUserID && p.AgentID == identity.AgentID
}

// ResolvedProviderConfig exists only for the duration of an execution attempt.
// JSON exclusions make accidental serialization fail closed for its secrets.
type ResolvedProviderConfig struct {
	APIKey   string            `json:"-"`
	Endpoint string            `json:"-"`
	Options  map[string]string `json:"-"`
}

type ResolvedProviderCandidate struct {
	Provider   string                 `json:"provider"`
	Model      string                 `json:"model"`
	Configured bool                   `json:"configured"`
	Config     ResolvedProviderConfig `json:"-"`
}

type ResolvedProviderPlan struct {
	Version      int                         `json:"version"`
	AutoFallback bool                        `json:"auto_fallback"`
	Candidates   []ResolvedProviderCandidate `json:"candidates"`
}

// ProviderPlanResolver is the narrow domain dependency used by batch creation
// and workers. Snapshot output is durable; Resolve output is memory-only.
type ProviderPlanResolver interface {
	Snapshot(ctx context.Context, identity ExecutionIdentity) (SafeProviderPlan, error)
	Resolve(ctx context.Context, identity ExecutionIdentity, plan SafeProviderPlan) (ResolvedProviderPlan, error)
}
