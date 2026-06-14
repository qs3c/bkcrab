package setup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/store"
)

// loadAgentFileConfig 从 agents.config 列返回 agent 的每行覆盖配置 JSON。
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

// saveAgentFileConfig 将每个 agent 的覆盖配置持久化到 agents.config 中。
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

// isStoreNotFound 识别跨后端的"未找到"信号。
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
