package imagegen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

func integrationMySQLStore(t *testing.T) *store.DBStore {
	t.Helper()
	dsn := os.Getenv("BKCRAB_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("BKCRAB_TEST_MYSQL_DSN is not set")
	}
	opened, err := store.New(&store.StorageConfig{Type: store.StorageMySQL, DSN: dsn, AutoMigrate: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	db, ok := opened.(*store.DBStore)
	if !ok {
		t.Fatal("integration store is not DBStore")
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func integrationID(t *testing.T, prefix string) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(raw[:])
}

func cleanupIntegrationBatch(t *testing.T, db *store.DBStore, batchIDs ...string) {
	t.Helper()
	for _, id := range batchIDs {
		_, _ = db.DB().Exec(`DELETE FROM image_generation_tasks WHERE batch_id=?`, id)
		_, _ = db.DB().Exec(`DELETE FROM image_generation_batches WHERE id=?`, id)
	}
}

type integrationPlanResolver struct{}

func (integrationPlanResolver) Snapshot(_ context.Context, identity ExecutionIdentity) (SafeProviderPlan, error) {
	return SafeProviderPlan{Version: ProviderPlanSchemaVersion, ConfigUserID: identity.ConfigUserID,
		AgentOwnerUserID: identity.AgentOwnerUserID, AgentID: identity.AgentID,
		AutoFallback: true, Candidates: []ProviderCandidateRef{{Provider: "openai", Model: "gpt-image-1"}}}, nil
}

func (integrationPlanResolver) Resolve(context.Context, ExecutionIdentity, SafeProviderPlan) (ResolvedProviderPlan, error) {
	return ResolvedProviderPlan{}, nil
}

func integrationIdentity(user, agent string) ExecutionIdentity {
	return ExecutionIdentity{UserID: user, ConfigUserID: user, AgentOwnerUserID: "owner-" + agent, AgentID: agent,
		WorkspaceProjectID: "project-origin", WorkspaceSessionID: "session-origin", MessageChannel: "web"}
}

func integrationRequest(t *testing.T, raw string) NormalizedRequest {
	t.Helper()
	request, err := NormalizeRequest([]byte(raw), RequestLimits{MaxImagesPerBatch: 16, MaxItems: 16, PromptMaxRunes: 8000, RequestMaxBytes: 128 << 10, WaitMaxSeconds: 240})
	if err != nil {
		t.Fatal(err)
	}
	return request
}
