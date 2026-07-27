package store

import (
	"context"
	"testing"
	"time"
)

func TestMCPGatewayRuntimeCRUD(t *testing.T) {
	st, err := NewDBStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := &MCPGatewayRuntimeRecord{
		ID:                  "mgr_u1",
		UserID:              "u1",
		Status:              "running",
		DockerContainerID:   "container-1",
		ContainerName:       "bkcrab-mcp-u1",
		Image:               "ghcr.io/lucky-aeon/mcp-gateway:latest",
		InternalPort:        8080,
		ExternalPort:        39123,
		BaseURL:             "http://127.0.0.1:39123",
		APIKey:              "secret",
		DeployedServersJSON: `{"time":{"command":"uvx"}}`,
		LastAccessedAt:      now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := st.SaveMCPGatewayRuntime(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetMCPGatewayRuntime(ctx, "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BaseURL != rec.BaseURL || got.Status != "running" {
		t.Fatalf("got runtime = %#v", got)
	}
	got.Status = "stopped"
	got.ErrorMessage = "idle"
	if err := st.SaveMCPGatewayRuntime(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, err := st.ListMCPGatewayRuntimesByStatus(ctx, "stopped")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].UserID != "u1" {
		t.Fatalf("list = %#v", list)
	}
}
