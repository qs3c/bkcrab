package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	imagegendomain "github.com/qs3c/bkcrab/internal/imagegen"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/toolproviders"
)

const imagegenToolCategory = "image_gen"

type effectiveToolConfig struct {
	Providers map[string]config.ToolProviderCfg
	Tools     map[string]config.ToolCategoryCfg
}

// ImagegenProviderResolver rebuilds the effective image provider view from the
// canonical config rows on every call. It deliberately owns no user-space
// cache, provider credentials, or mutable chain state.
type ImagegenProviderResolver struct {
	store    store.Store
	registry *toolproviders.Registry
}

func NewImagegenProviderResolver(st store.Store, registry *toolproviders.Registry) *ImagegenProviderResolver {
	return &ImagegenProviderResolver{store: st, registry: registry}
}

func (r *ImagegenProviderResolver) Snapshot(ctx context.Context, identity imagegendomain.ExecutionIdentity) (imagegendomain.SafeProviderPlan, error) {
	if err := r.validate(); err != nil {
		return imagegendomain.SafeProviderPlan{}, err
	}
	effective, err := resolveEffectiveToolConfig(ctx, r.store, identity)
	if err != nil {
		return imagegendomain.SafeProviderPlan{}, err
	}
	category, ok := effective.Tools[imagegenToolCategory]
	if !ok || len(category.Chain()) == 0 {
		return imagegendomain.SafeProviderPlan{}, errors.New("imagegen: no image provider chain configured")
	}
	resolved, err := r.registry.ResolveProviderReferences(imagegenToolCategory, category.Chain(), toolConfigLookup(effective.Providers))
	if err != nil {
		return imagegendomain.SafeProviderPlan{}, fmt.Errorf("imagegen: snapshot provider chain: %w", err)
	}
	plan := imagegendomain.SafeProviderPlan{
		Version: imagegendomain.ProviderPlanSchemaVersion, ConfigUserID: identity.ConfigUserID,
		AgentOwnerUserID: identity.AgentOwnerUserID, AgentID: identity.AgentID,
		AutoFallback: category.FallbackEnabled(), Candidates: make([]imagegendomain.ProviderCandidateRef, 0, len(resolved)),
	}
	available := false
	for _, candidate := range resolved {
		if candidate.Name == "none" {
			return imagegendomain.SafeProviderPlan{}, errors.New("imagegen: image generation is disabled for this agent")
		}
		available = available || candidate.Configured
		plan.Candidates = append(plan.Candidates, imagegendomain.ProviderCandidateRef{Provider: candidate.Name, Model: candidate.Model})
	}
	if !available {
		return imagegendomain.SafeProviderPlan{}, errors.New("imagegen: no configured image provider is available")
	}
	if err := plan.Validate(); err != nil {
		return imagegendomain.SafeProviderPlan{}, err
	}
	return plan, nil
}

func (r *ImagegenProviderResolver) Resolve(ctx context.Context, identity imagegendomain.ExecutionIdentity, plan imagegendomain.SafeProviderPlan) (imagegendomain.ResolvedProviderPlan, error) {
	if err := r.validate(); err != nil {
		return imagegendomain.ResolvedProviderPlan{}, err
	}
	if err := identity.Validate(); err != nil {
		return imagegendomain.ResolvedProviderPlan{}, err
	}
	if err := plan.Validate(); err != nil {
		return imagegendomain.ResolvedProviderPlan{}, err
	}
	if !plan.MatchesIdentity(identity) {
		return imagegendomain.ResolvedProviderPlan{}, errors.New("imagegen: provider plan identity does not match execution identity")
	}
	effective, err := resolveEffectiveToolConfig(ctx, r.store, identity)
	if err != nil {
		return imagegendomain.ResolvedProviderPlan{}, err
	}
	refs := make([]string, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		ref := candidate.Provider
		if candidate.Model != "" {
			ref += "/" + candidate.Model
		}
		refs = append(refs, ref)
	}
	resolved, err := r.registry.ResolveProviderReferences(imagegenToolCategory, refs, toolConfigLookup(effective.Providers))
	if err != nil {
		return imagegendomain.ResolvedProviderPlan{}, fmt.Errorf("imagegen: resolve provider chain: %w", err)
	}
	out := imagegendomain.ResolvedProviderPlan{
		Version: plan.Version, AutoFallback: plan.AutoFallback,
		Candidates: make([]imagegendomain.ResolvedProviderCandidate, 0, len(resolved)),
	}
	for _, candidate := range resolved {
		out.Candidates = append(out.Candidates, imagegendomain.ResolvedProviderCandidate{
			Provider: candidate.Name, Model: candidate.Model, Configured: candidate.Configured,
			Config: imagegendomain.ResolvedProviderConfig{
				APIKey: candidate.Config.APIKey, Endpoint: candidate.Config.Endpoint, Options: candidate.Config.Options,
			},
		})
	}
	return out, nil
}

func (r *ImagegenProviderResolver) validate() error {
	if r == nil || r.store == nil {
		return errors.New("imagegen: provider resolver store is required")
	}
	if r.registry == nil {
		return errors.New("imagegen: provider resolver registry is required")
	}
	return nil
}

func toolConfigLookup(providers map[string]config.ToolProviderCfg) func(string) toolproviders.ProviderConfig {
	return func(name string) toolproviders.ProviderConfig {
		provider := providers[name]
		return toolproviders.ProviderConfig{APIKey: provider.APIKey, Endpoint: provider.Endpoint, Options: provider.Options}
	}
}

// resolveEffectiveToolConfig is shared by synchronous agent registration and
// the durable resolver. For an owned agent it follows system -> user -> agent
// -> user-agent. For a shared foreign agent, owner/official layers provide the
// fallback while explicit viewer layers are reapplied last. shareModelConfig
// disables both owner and official-agent tool configuration inheritance.
func resolveEffectiveToolConfig(ctx context.Context, st store.Store, identity imagegendomain.ExecutionIdentity) (effectiveToolConfig, error) {
	if err := identity.Validate(); err != nil {
		return effectiveToolConfig{}, err
	}
	if st == nil {
		return effectiveToolConfig{}, errors.New("imagegen: config store is required")
	}
	agentRecord, err := st.GetAgent(ctx, identity.AgentID)
	if err != nil {
		return effectiveToolConfig{}, fmt.Errorf("imagegen: load agent: %w", err)
	}
	if agentRecord == nil || agentRecord.UserID != identity.AgentOwnerUserID {
		return effectiveToolConfig{}, errors.New("imagegen: agent owner identity mismatch")
	}
	out := effectiveToolConfig{
		Providers: make(map[string]config.ToolProviderCfg),
		Tools:     make(map[string]config.ToolCategoryCfg),
	}
	applyStoredToolLayer := func(userID, agentID string) error {
		layer, err := readStoredToolLayer(ctx, st, userID, agentID)
		if err != nil {
			return err
		}
		applyEffectiveToolLayer(&out, layer)
		return nil
	}
	if err := applyStoredToolLayer("", ""); err != nil {
		return effectiveToolConfig{}, err
	}

	foreign := identity.ConfigUserID != agentRecord.UserID
	share := true
	if value, ok := agentRecord.Config["shareModelConfig"].(bool); ok {
		share = value
	}
	if !foreign {
		if err := applyStoredToolLayer(identity.ConfigUserID, ""); err != nil {
			return effectiveToolConfig{}, err
		}
		applyLegacyAgentToolLayer(&out, agentRecord.Config)
		if err := applyStoredToolLayer("", identity.AgentID); err != nil {
			return effectiveToolConfig{}, err
		}
		if err := applyStoredToolLayer(identity.ConfigUserID, identity.AgentID); err != nil {
			return effectiveToolConfig{}, err
		}
		return out, nil
	}

	if share {
		if err := applyStoredToolLayer(agentRecord.UserID, ""); err != nil {
			return effectiveToolConfig{}, err
		}
		applyLegacyAgentToolLayer(&out, agentRecord.Config)
		if err := applyStoredToolLayer("", identity.AgentID); err != nil {
			return effectiveToolConfig{}, err
		}
	}
	// The config user is the viewer/configuration principal, which can differ
	// from the fair-queue tenant. Explicit viewer choices are authoritative.
	if err := applyStoredToolLayer(identity.ConfigUserID, ""); err != nil {
		return effectiveToolConfig{}, err
	}
	if err := applyStoredToolLayer(identity.ConfigUserID, identity.AgentID); err != nil {
		return effectiveToolConfig{}, err
	}
	return out, nil
}

func readStoredToolLayer(ctx context.Context, st store.Store, userID, agentID string) (effectiveToolConfig, error) {
	out := effectiveToolConfig{}
	read := func(namespace string, dst any) error {
		record, err := st.GetConfigByName(ctx, store.KindSetting, userID, agentID, namespace)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		if record == nil || len(record.Data) == 0 {
			return nil
		}
		blob, err := json.Marshal(record.Data)
		if err != nil {
			return err
		}
		return json.Unmarshal(blob, dst)
	}
	if err := read(NSToolProviders, &out.Providers); err != nil {
		return effectiveToolConfig{}, fmt.Errorf("imagegen: read tool provider layer: %w", err)
	}
	if err := read(NSToolCategories, &out.Tools); err != nil {
		return effectiveToolConfig{}, fmt.Errorf("imagegen: read tool category layer: %w", err)
	}
	return out, nil
}

func applyLegacyAgentToolLayer(out *effectiveToolConfig, raw map[string]interface{}) {
	if out == nil || len(raw) == 0 {
		return
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return
	}
	var legacy config.AgentFileConfig
	if json.Unmarshal(blob, &legacy) != nil {
		return
	}
	applyEffectiveToolLayer(out, effectiveToolConfig{Providers: legacy.ToolProviders, Tools: legacy.Tools})
}

func applyEffectiveToolLayer(out *effectiveToolConfig, layer effectiveToolConfig) {
	for name, provider := range layer.Providers {
		provider.APIKey = strings.TrimSpace(provider.APIKey)
		provider.Endpoint = strings.TrimSpace(provider.Endpoint)
		if provider.Options != nil {
			options := make(map[string]string, len(provider.Options))
			for key, value := range provider.Options {
				options[key] = value
			}
			provider.Options = options
		}
		out.Providers[name] = provider
	}
	for name, category := range layer.Tools {
		category.Fallbacks = append([]string(nil), category.Fallbacks...)
		out.Tools[name] = category
	}
}
