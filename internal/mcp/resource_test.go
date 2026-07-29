package mcp

import (
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestResourceDeploymentRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resource := Resource{
		ID:      "sc_alpha",
		UserID:  "u1",
		Name:    "github",
		Enabled: true,
		Config:  config.MCPServerConfig{Type: "stdio", Command: "alpha"},
		Deployment: &ResourceDeployment{
			Status:    "failed",
			Message:   "部署失败",
			Error:     "command exited",
			UpdatedAt: &now,
		},
	}
	rec := store.ConfigRecord{ID: resource.ID}
	resource.ApplyToRecord(&rec)

	got, err := ResourceFromRecord(rec)
	if err != nil {
		t.Fatalf("round trip resource: %v", err)
	}
	if got.Deployment == nil || got.Deployment.Status != "failed" || got.Deployment.Error != "command exited" {
		t.Fatalf("deployment = %#v", got.Deployment)
	}
	if got.Deployment.UpdatedAt == nil || !got.Deployment.UpdatedAt.Equal(now) {
		t.Fatalf("deployment updatedAt = %#v, want %v", got.Deployment.UpdatedAt, now)
	}
}
