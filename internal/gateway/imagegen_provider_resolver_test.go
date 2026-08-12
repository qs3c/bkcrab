package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	imagegendomain "github.com/qs3c/bkcrab/internal/imagegen"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type imagegenResolverTestProvider struct {
	name string
}

func (p imagegenResolverTestProvider) Category() string { return "image_gen" }
func (p imagegenResolverTestProvider) Name() string     { return p.name }
func (p imagegenResolverTestProvider) Execute(context.Context, toolproviders.Request) (toolproviders.Response, error) {
	return toolproviders.Response{}, nil
}

func newImagegenResolverTestStore(t *testing.T, agentConfig map[string]interface{}) (*store.DBStore, *ImagegenProviderResolver) {
	t.Helper()
	db, err := store.NewDBStore("sqlite", "file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SaveAgent(context.Background(), &store.AgentRecord{
		ID: "agent-a", UserID: "owner-a", Name: "agent", Config: agentConfig,
	}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	registry := toolproviders.NewRegistry()
	registry.Register(imagegenResolverTestProvider{name: "alpha"})
	registry.Register(imagegenResolverTestProvider{name: "beta"})
	return db, NewImagegenProviderResolver(db, registry)
}

func saveImagegenToolLayer(t *testing.T, db store.Store, userID, agentID, provider, key, model string) {
	t.Helper()
	ctx := context.Background()
	if provider != "" {
		if err := scope.SaveSetting(ctx, db, userID, agentID, NSToolProviders, map[string]interface{}{
			provider: map[string]interface{}{"apiKey": key, "endpoint": "https://" + provider + ".invalid"},
		}); err != nil {
			t.Fatalf("save provider layer (%q,%q): %v", userID, agentID, err)
		}
	}
	if model != "" {
		if err := scope.SaveSetting(ctx, db, userID, agentID, NSToolCategories, map[string]interface{}{
			"image_gen": map[string]interface{}{"primary": provider + "/" + model, "autoFallback": true},
		}); err != nil {
			t.Fatalf("save category layer (%q,%q): %v", userID, agentID, err)
		}
	}
}

func imagegenResolverIdentity(configUser string) imagegendomain.ExecutionIdentity {
	return imagegendomain.ExecutionIdentity{
		UserID: "tenant-runtime", ConfigUserID: configUser, AgentOwnerUserID: "owner-a", AgentID: "agent-a",
	}
}

func TestImagegenProviderResolverOwnedScopeOrder(t *testing.T) {
	db, resolver := newImagegenResolverTestStore(t, nil)
	saveImagegenToolLayer(t, db, "", "", "alpha", "system-key", "system-model")
	saveImagegenToolLayer(t, db, "owner-a", "", "alpha", "owner-key", "owner-model")
	saveImagegenToolLayer(t, db, "", "agent-a", "alpha", "agent-key", "agent-model")
	saveImagegenToolLayer(t, db, "owner-a", "agent-a", "alpha", "owner-agent-key", "owner-agent-model")

	identity := imagegenResolverIdentity("owner-a")
	plan, err := resolver.Snapshot(context.Background(), identity)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Provider != "alpha" || plan.Candidates[0].Model != "owner-agent-model" {
		t.Fatalf("system -> user -> agent -> user-agent order not honored: %#v", plan)
	}
	resolved, err := resolver.Resolve(context.Background(), identity, plan)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := resolved.Candidates[0].Config.APIKey; got != "owner-agent-key" {
		t.Fatalf("resolved key: want owner-agent-key, got %q", got)
	}
}

func TestImagegenProviderResolverForeignSharingViewerPriorityAndRotation(t *testing.T) {
	db, resolver := newImagegenResolverTestStore(t, map[string]interface{}{"shareModelConfig": true})
	saveImagegenToolLayer(t, db, "", "", "alpha", "system-key", "system-model")
	saveImagegenToolLayer(t, db, "owner-a", "", "alpha", "owner-secret", "owner-model")
	saveImagegenToolLayer(t, db, "", "agent-a", "beta", "agent-secret", "agent-model")

	identity := imagegenResolverIdentity("viewer-a")
	ownerPlan, err := resolver.Snapshot(context.Background(), identity)
	if err != nil {
		t.Fatalf("owner/agent shared snapshot: %v", err)
	}
	if got := ownerPlan.Candidates[0]; got.Provider != "beta" || got.Model != "agent-model" {
		t.Fatalf("agent overlay should win owner fallback: %#v", got)
	}

	// A viewer's explicit configuration is the innermost layer for a foreign agent.
	saveImagegenToolLayer(t, db, "viewer-a", "", "alpha", "viewer-secret-v1", "viewer-model")
	viewerPlan, err := resolver.Snapshot(context.Background(), identity)
	if err != nil {
		t.Fatalf("viewer snapshot: %v", err)
	}
	if got := viewerPlan.Candidates[0]; got.Provider != "alpha" || got.Model != "viewer-model" {
		t.Fatalf("viewer explicit chain did not win: %#v", got)
	}

	encoded, err := json.Marshal(viewerPlan)
	if err != nil {
		t.Fatalf("marshal safe plan: %v", err)
	}
	lower := strings.ToLower(string(encoded))
	for _, forbidden := range []string{"apikey", "authorization", "bearer", "secretkey", "access token", "viewer-secret", "owner-secret", "agent-secret"} {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			t.Fatalf("safe plan leaked %q: %s", forbidden, encoded)
		}
	}

	// Change both the current chain and credential. The old plan keeps its
	// candidate order/model but resolves the rotated credential.
	saveImagegenToolLayer(t, db, "viewer-a", "", "alpha", "viewer-secret-v2", "replacement-model")
	resolved, err := resolver.Resolve(context.Background(), identity, viewerPlan)
	if err != nil {
		t.Fatalf("resolve rotated plan: %v", err)
	}
	if got := resolved.Candidates[0]; got.Provider != "alpha" || got.Model != "viewer-model" || got.Config.APIKey != "viewer-secret-v2" {
		t.Fatalf("snapshot order/current secret contract broken: %#v", got)
	}
	if viewerPlan.ConfigUserID == identity.UserID {
		t.Fatal("test did not exercise config_user_id != tenant_user_id")
	}
}

func TestImagegenProviderResolverForeignSharingDisabled(t *testing.T) {
	db, resolver := newImagegenResolverTestStore(t, map[string]interface{}{"shareModelConfig": false})
	saveImagegenToolLayer(t, db, "owner-a", "", "alpha", "owner-secret", "owner-model")
	saveImagegenToolLayer(t, db, "", "agent-a", "beta", "agent-secret", "agent-model")
	identity := imagegenResolverIdentity("viewer-a")

	if _, err := resolver.Snapshot(context.Background(), identity); err == nil {
		t.Fatal("foreign viewer inherited owner/agent tool providers with sharing disabled")
	}
	saveImagegenToolLayer(t, db, "viewer-a", "", "alpha", "viewer-key", "viewer-model")
	plan, err := resolver.Snapshot(context.Background(), identity)
	if err != nil {
		t.Fatalf("viewer-owned config should remain usable: %v", err)
	}
	resolved, err := resolver.Resolve(context.Background(), identity, plan)
	if err != nil {
		t.Fatalf("resolve viewer config: %v", err)
	}
	if got := resolved.Candidates[0].Config.APIKey; got != "viewer-key" {
		t.Fatalf("owner secret leaked through disabled sharing: %q", got)
	}
}

func TestImagegenProviderResolverRejectsIdentityAndPlanScopeMismatch(t *testing.T) {
	db, resolver := newImagegenResolverTestStore(t, nil)
	saveImagegenToolLayer(t, db, "owner-a", "", "alpha", "owner-key", "model")
	identity := imagegenResolverIdentity("owner-a")
	plan, err := resolver.Snapshot(context.Background(), identity)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	wrongOwner := identity
	wrongOwner.AgentOwnerUserID = "attacker"
	if _, err := resolver.Resolve(context.Background(), wrongOwner, plan); err == nil {
		t.Fatal("owner mismatch accepted")
	}
	wrongConfig := identity
	wrongConfig.ConfigUserID = "another-user"
	if _, err := resolver.Resolve(context.Background(), wrongConfig, plan); err == nil {
		t.Fatal("plan config scope mismatch accepted")
	}
}
