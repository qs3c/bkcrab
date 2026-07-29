package mcp

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

// Resource is an MCP server owned by a user. Agents only store Resource.IDs
// as grants; connection details and secrets remain in this user-level record.
type Resource struct {
	ID          string                 `json:"id"`
	UserID      string                 `json:"userId"`
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Enabled     bool                   `json:"enabled"`
	Config      config.MCPServerConfig `json:"config"`
	Deployment  *ResourceDeployment    `json:"deployment,omitempty"`
	CreatedAt   time.Time              `json:"createdAt"`
	UpdatedAt   time.Time              `json:"updatedAt"`
}

const (
	ResourceDeploymentPending  = "pending"
	ResourceDeploymentDisabled = "disabled"
)

type ResourceDeployment struct {
	Status    string     `json:"status"`
	Message   string     `json:"message,omitempty"`
	Error     string     `json:"error,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type resourceData struct {
	Description string              `json:"description,omitempty"`
	Type        string              `json:"type"`
	URL         string              `json:"url,omitempty"`
	Headers     map[string]string   `json:"headers,omitempty"`
	Command     string              `json:"command,omitempty"`
	Args        []string            `json:"args,omitempty"`
	Env         map[string]string   `json:"env,omitempty"`
	Transport   string              `json:"transport,omitempty"`
	Deployment  *ResourceDeployment `json:"deployment,omitempty"`
}

func ResourceFromRecord(rec store.ConfigRecord) (Resource, error) {
	if rec.Kind != store.KindMCPServer {
		return Resource{}, fmt.Errorf("config %q is not an MCP resource", rec.ID)
	}
	blob, err := json.Marshal(rec.Data)
	if err != nil {
		return Resource{}, fmt.Errorf("marshal MCP resource %q: %w", rec.ID, err)
	}
	var data resourceData
	if err := json.Unmarshal(blob, &data); err != nil {
		return Resource{}, fmt.Errorf("decode MCP resource %q: %w", rec.ID, err)
	}
	enabled := rec.Enabled
	return Resource{
		ID:          rec.ID,
		UserID:      rec.UserID,
		Name:        rec.Name,
		Description: data.Description,
		Enabled:     rec.Enabled,
		Config: config.MCPServerConfig{
			Type:      data.Type,
			URL:       data.URL,
			Headers:   data.Headers,
			Command:   data.Command,
			Args:      data.Args,
			Env:       data.Env,
			Transport: data.Transport,
			Enabled:   &enabled,
		},
		Deployment: data.Deployment,
		CreatedAt:  rec.CreatedAt,
		UpdatedAt:  rec.UpdatedAt,
	}, nil
}

func (r Resource) ApplyToRecord(rec *store.ConfigRecord) {
	if rec == nil {
		return
	}
	rec.Kind = store.KindMCPServer
	rec.UserID = r.UserID
	rec.AgentID = ""
	rec.Name = r.Name
	rec.Enabled = r.Enabled
	data := map[string]interface{}{
		"description": r.Description,
		"type":        r.Config.Type,
		"url":         r.Config.URL,
		"headers":     r.Config.Headers,
		"command":     r.Config.Command,
		"args":        r.Config.Args,
		"env":         r.Config.Env,
		"transport":   r.Config.Transport,
	}
	if r.Deployment != nil {
		data["deployment"] = r.Deployment
	}
	rec.Data = data
}
