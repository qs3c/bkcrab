package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// loadAgentFileConfig returns an agent's per-row override JSON from the
// agents.config column.
func (s *Server) loadAgentFileConfig(r *http.Request, agentID string) (*config.AgentFileConfig, error) {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return &config.AgentFileConfig{}, nil
		}
		return nil, err
	}
	cfg := &config.AgentFileConfig{}
	if len(rec.Config) > 0 {
		blob, _ := json.Marshal(rec.Config)
		_ = json.Unmarshal(blob, cfg)
	}
	return cfg, nil
}

// saveAgentFileConfig persists per-agent overrides into agents.config.
func (s *Server) saveAgentFileConfig(r *http.Request, agentID string, cfg *config.AgentFileConfig) error {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			rec = &store.AgentRecord{ID: agentID, UserID: s.effectiveUserID(r), Name: agentID}
		} else {
			return err
		}
	}
	blob, _ := json.Marshal(cfg)
	var asMap map[string]interface{}
	if err := json.Unmarshal(blob, &asMap); err != nil {
		return err
	}
	rec.Config = asMap
	rec.UpdatedAt = time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = rec.UpdatedAt
	}
	return s.dataStore.SaveAgent(r.Context(), rec)
}

// isStoreNotFound recognises the "not found" signal across backends.
func isStoreNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no rows in result set") || strings.Contains(msg, "not found")
}

var _ = context.Background
