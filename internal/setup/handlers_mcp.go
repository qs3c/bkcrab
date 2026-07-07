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
	"github.com/qs3c/bkcrab/internal/store"
)

const mcpSecretMask = "********"

var mcpServerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type agentMCPUpdateRequest struct {
	MCPServers     map[string]config.MCPServerConfig `json:"mcpServers"`
	ShareMCPConfig bool                              `json:"shareMcpConfig"`
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
