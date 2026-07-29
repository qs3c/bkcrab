package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/mcp"
	"github.com/qs3c/bkcrab/internal/store"
)

const mcpSecretMask = "********"

var mcpServerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type agentMCPUpdateRequest struct {
	MCPServers     map[string]config.MCPServerConfig `json:"mcpServers"`
	ShareMCPConfig bool                              `json:"shareMcpConfig"`
}

type mcpResourceWriteRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Enabled     *bool                  `json:"enabled,omitempty"`
	Config      config.MCPServerConfig `json:"config"`
}

func pendingMCPDeployment(enabled bool) *mcp.ResourceDeployment {
	now := time.Now().UTC()
	if !enabled {
		return &mcp.ResourceDeployment{
			Status:    mcp.ResourceDeploymentDisabled,
			Message:   "服务已停用",
			UpdatedAt: &now,
		}
	}
	return &mcp.ResourceDeployment{
		Status:    mcp.ResourceDeploymentPending,
		Message:   "等待首次部署或连接测试",
		UpdatedAt: &now,
	}
}

func maskMCPServers(src map[string]config.MCPServerConfig) map[string]config.MCPServerConfig {
	out := make(map[string]config.MCPServerConfig, len(src))
	for name, cfg := range src {
		cp := cfg
		if cfg.Headers != nil {
			cp.Headers = make(map[string]string, len(cfg.Headers))
			for k, v := range cfg.Headers {
				if v != "" {
					cp.Headers[k] = mcpSecretMask
				} else {
					cp.Headers[k] = v
				}
			}
		}
		if cfg.Env != nil {
			cp.Env = make(map[string]string, len(cfg.Env))
			for k, v := range cfg.Env {
				if v != "" {
					cp.Env[k] = mcpSecretMask
				} else {
					cp.Env[k] = v
				}
			}
		}
		out[name] = cp
	}
	return out
}

func mergeMaskedMCPSecrets(old, next map[string]config.MCPServerConfig) map[string]config.MCPServerConfig {
	out := make(map[string]config.MCPServerConfig, len(next))
	for name, cfg := range next {
		cp := cfg
		oldCfg := old[name]
		if cfg.Headers != nil {
			cp.Headers = make(map[string]string, len(cfg.Headers))
			for k, v := range cfg.Headers {
				if v == mcpSecretMask {
					if oldCfg.Headers != nil {
						cp.Headers[k] = oldCfg.Headers[k]
					}
					continue
				}
				cp.Headers[k] = v
			}
		}
		if cfg.Env != nil {
			cp.Env = make(map[string]string, len(cfg.Env))
			for k, v := range cfg.Env {
				if v == mcpSecretMask {
					if oldCfg.Env != nil {
						cp.Env[k] = oldCfg.Env[k]
					}
					continue
				}
				cp.Env[k] = v
			}
		}
		out[name] = cp
	}
	return out
}

func validateMCPServers(servers map[string]config.MCPServerConfig) error {
	for name, cfg := range servers {
		if !mcpServerNameRE.MatchString(name) {
			return fmt.Errorf("invalid MCP server name %q", name)
		}
		typ := strings.ToLower(strings.TrimSpace(cfg.Type))
		if typ != "stdio" && typ != "http" {
			return fmt.Errorf("MCP server %q type must be stdio or http", name)
		}
		if cfg.Transport != "" && !config.MCPTransportValid(cfg.Transport) {
			return fmt.Errorf("MCP server %q transport must be sse or streamable-http", name)
		}
		for k, v := range cfg.Headers {
			if !strings.EqualFold(k, "Authorization") {
				return fmt.Errorf("MCP server %q only supports Authorization bearer headers", name)
			}
			if v == "" || !strings.HasPrefix(v, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(v, "Bearer ")) == "" {
				return fmt.Errorf("MCP server %q Authorization header must be a Bearer token", name)
			}
		}
		switch typ {
		case "stdio":
			if strings.TrimSpace(cfg.URL) != "" {
				return fmt.Errorf("MCP server %q stdio config must not set url", name)
			}
			if config.MCPServerEnabled(cfg) && strings.TrimSpace(cfg.Command) == "" {
				return fmt.Errorf("MCP server %q stdio config requires command", name)
			}
		case "http":
			if strings.TrimSpace(cfg.Command) != "" {
				return fmt.Errorf("MCP server %q http config must not set command", name)
			}
			if config.MCPServerEnabled(cfg) && strings.TrimSpace(cfg.URL) == "" {
				return fmt.Errorf("MCP server %q http config requires url", name)
			}
		}
	}
	return nil
}

func (s *Server) handleGetAgentMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	cfg := agentMCPConfigFromRecord(rec)
	jsonResponse(w, http.StatusOK, s.agentMCPResponse(r, rec, cfg))
}

func (s *Server) handlePutAgentMCP(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req agentMCPUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg := agentMCPConfigFromRecord(rec)
	next := mergeMaskedMCPSecrets(cfg.MCPServers, req.MCPServers)
	if err := validateMCPServers(next); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cfg.MCPServers = next
	cfg.ShareMCPConfig = req.ShareMCPConfig
	applyAgentMCPConfigToRecord(rec, cfg)
	rec.UpdatedAt = time.Now().UTC()
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.invalidateAgent(rec.ID)
	jsonResponse(w, http.StatusOK, s.agentMCPResponse(r, rec, cfg))
}

func (s *Server) handleGetAgentMCPStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"gateway": s.mcpGatewayStatus(r, rec.UserID)})
}

func (s *Server) handleTestAgentMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if s.mcpRuntime == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "mcp gateway runtime is not configured"})
		return
	}
	cfg := agentMCPConfigFromRecord(rec)
	tools, err := s.mcpRuntime.TestServers(r.Context(), rec.UserID, cfg.MCPServers)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "tools": tools})
}

func (s *Server) agentMCPResponse(r *http.Request, rec *store.AgentRecord, cfg config.AgentFileConfig) map[string]any {
	return map[string]any{
		"mcpServers":     maskMCPServers(cfg.MCPServers),
		"shareMcpConfig": cfg.ShareMCPConfig,
		"gateway":        s.mcpGatewayStatus(r, rec.UserID),
	}
}

func (s *Server) mcpGatewayStatus(r *http.Request, userID string) map[string]any {
	out := map[string]any{"status": "stopped"}
	if s.mcpRuntime == nil {
		return out
	}
	rec, err := s.mcpRuntime.Status(r.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out
		}
		return map[string]any{"status": "error", "errorMessage": err.Error()}
	}
	if rec == nil {
		return out
	}
	if rec.Status != "" {
		out["status"] = rec.Status
	}
	if rec.BaseURL != "" {
		out["baseUrl"] = rec.BaseURL
	}
	if rec.Image != "" {
		out["image"] = rec.Image
	}
	if !rec.LastAccessedAt.IsZero() {
		out["lastAccessedAt"] = rec.LastAccessedAt
	}
	if rec.ErrorMessage != "" {
		out["errorMessage"] = rec.ErrorMessage
	}
	return out
}

func agentMCPConfigFromRecord(rec *store.AgentRecord) config.AgentFileConfig {
	var cfg config.AgentFileConfig
	if rec == nil || len(rec.Config) == 0 {
		return cfg
	}
	blob, _ := json.Marshal(rec.Config)
	_ = json.Unmarshal(blob, &cfg)
	return cfg
}

func applyAgentMCPConfigToRecord(rec *store.AgentRecord, cfg config.AgentFileConfig) {
	if rec.Config == nil {
		rec.Config = map[string]interface{}{}
	}
	if len(cfg.MCPServers) > 0 {
		rec.Config["mcpServers"] = cfg.MCPServers
	} else {
		delete(rec.Config, "mcpServers")
	}
	if cfg.ShareMCPConfig {
		rec.Config["shareMcpConfig"] = true
	} else {
		delete(rec.Config, "shareMcpConfig")
	}
}

func (s *Server) handleListMCPResources(w http.ResponseWriter, r *http.Request) {
	userID := s.effectiveUserID(r)
	rows, err := s.dataStore.ListConfigs(r.Context(), store.KindMCPServer, userID, "")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resources := make([]mcp.Resource, 0, len(rows))
	for _, row := range rows {
		resource, err := mcp.ResourceFromRecord(row)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		resources = append(resources, maskMCPResource(resource))
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"servers": resources,
		"gateway": s.mcpGatewayStatus(r, userID),
	})
}

func (s *Server) handleGetMCPResource(w http.ResponseWriter, r *http.Request) {
	resource := s.requireOwnedMCPResource(w, r)
	if resource == nil {
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"server": maskMCPResource(*resource)})
}

func (s *Server) handleCreateMCPResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	userID := s.effectiveUserID(r)
	var req mcpResourceWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !mcpServerNameRE.MatchString(req.Name) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name must contain only letters, numbers, underscore, or hyphen (max 64)"})
		return
	}
	if existing, err := s.dataStore.GetConfigByName(r.Context(), store.KindMCPServer, userID, "", req.Name); err == nil && existing != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": "an MCP server with this name already exists"})
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	req.Config.Enabled = &enabled
	if err := validateMCPServers(map[string]config.MCPServerConfig{req.Name: req.Config}); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	resource := mcp.Resource{
		UserID:      userID,
		Name:        req.Name,
		Description: strings.TrimSpace(req.Description),
		Enabled:     enabled,
		Config:      req.Config,
		Deployment:  pendingMCPDeployment(enabled),
	}
	rec := &store.ConfigRecord{}
	resource.ApplyToRecord(rec)
	if err := s.dataStore.SaveConfig(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	resource, err := mcp.ResourceFromRecord(*rec)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if s.mcpRuntime != nil {
		if err := s.mcpRuntime.SyncUserResources(r.Context(), userID); err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"error": "MCP resource was saved, but the running gateway could not be synchronized: " + err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"server": maskMCPResource(resource)})
}

func (s *Server) handleUpdateMCPResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	current := s.requireOwnedMCPResource(w, r)
	if current == nil {
		return
	}
	var req mcpResourceWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if name := strings.TrimSpace(req.Name); name != "" && name != current.Name {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "MCP server name cannot be changed"})
		return
	}
	enabled := current.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	merged := mergeMaskedMCPSecrets(
		map[string]config.MCPServerConfig{current.Name: current.Config},
		map[string]config.MCPServerConfig{current.Name: req.Config},
	)[current.Name]
	merged.Enabled = &enabled
	if err := validateMCPServers(map[string]config.MCPServerConfig{current.Name: merged}); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	rec, err := s.dataStore.GetConfig(r.Context(), current.ID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	next := *current
	next.Description = strings.TrimSpace(req.Description)
	next.Enabled = enabled
	next.Config = merged
	next.Deployment = pendingMCPDeployment(enabled)
	next.ApplyToRecord(rec)
	if err := s.dataStore.SaveConfig(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	next, err = mcp.ResourceFromRecord(*rec)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateMCPResourceAgents(r, next.UserID, next.ID)
	if s.mcpRuntime != nil {
		if err := s.mcpRuntime.SyncUserResources(r.Context(), next.UserID); err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"error": "MCP resource was saved, but the running gateway could not be synchronized: " + err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"server": maskMCPResource(next)})
}

func (s *Server) handleDeleteMCPResource(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	resource := s.requireOwnedMCPResource(w, r)
	if resource == nil {
		return
	}
	if err := s.revokeMCPResourceFromAgents(r, resource.UserID, resource.ID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.dataStore.DeleteConfig(r.Context(), resource.ID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if s.mcpRuntime != nil {
		if err := s.mcpRuntime.SyncUserResources(r.Context(), resource.UserID); err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"error": "MCP resource was deleted, but the running gateway could not be synchronized: " + err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestMCPResource(w http.ResponseWriter, r *http.Request) {
	resource := s.requireOwnedMCPResource(w, r)
	if resource == nil {
		return
	}
	if s.mcpRuntime == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "mcp gateway runtime is not configured"})
		return
	}
	tools, err := s.mcpRuntime.TestResources(r.Context(), resource.UserID, []string{resource.ID})
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "tools": tools})
}

func (s *Server) handleGetMCPStatus(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{"gateway": s.mcpGatewayStatus(r, s.effectiveUserID(r))})
}

func (s *Server) requireOwnedMCPResource(w http.ResponseWriter, r *http.Request) *mcp.Resource {
	rec, err := s.dataStore.GetConfig(r.Context(), r.PathValue("id"))
	if err != nil || rec == nil || rec.Kind != store.KindMCPServer ||
		rec.UserID != s.effectiveUserID(r) || rec.AgentID != "" {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return nil
	}
	resource, err := mcp.ResourceFromRecord(*rec)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return nil
	}
	return &resource
}

func maskMCPResource(resource mcp.Resource) mcp.Resource {
	resource.Config = maskMCPServers(map[string]config.MCPServerConfig{
		resource.Name: resource.Config,
	})[resource.Name]
	return resource
}

func (s *Server) invalidateMCPResourceAgents(r *http.Request, userID, resourceID string) {
	s.invalidateUser(userID)
	agents, err := s.dataStore.ListAgents(r.Context(), userID)
	if err != nil {
		return
	}
	for i := range agents {
		cfg := agentMCPConfigFromRecord(&agents[i])
		if cfg.MCP == nil {
			continue
		}
		for _, grantedID := range cfg.MCP.Servers {
			if grantedID == resourceID {
				s.invalidateAgent(agents[i].ID)
				break
			}
		}
	}
}

func (s *Server) revokeMCPResourceFromAgents(r *http.Request, userID, resourceID string) error {
	agents, err := s.dataStore.ListAgents(r.Context(), userID)
	if err != nil {
		return err
	}
	for i := range agents {
		cfg := agentMCPConfigFromRecord(&agents[i])
		if cfg.MCP == nil {
			continue
		}
		kept := make([]string, 0, len(cfg.MCP.Servers))
		changed := false
		for _, grantedID := range cfg.MCP.Servers {
			if grantedID == resourceID {
				changed = true
				continue
			}
			kept = append(kept, grantedID)
		}
		if !changed {
			continue
		}
		if len(kept) == 0 {
			delete(agents[i].Config, "mcp")
		} else {
			agents[i].Config["mcp"] = map[string]interface{}{"servers": kept}
		}
		agents[i].UpdatedAt = time.Now().UTC()
		if err := s.dataStore.SaveAgent(r.Context(), &agents[i]); err != nil {
			return err
		}
		s.invalidateAgent(agents[i].ID)
	}
	return nil
}
