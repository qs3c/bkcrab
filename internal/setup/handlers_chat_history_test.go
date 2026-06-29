package setup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/api"
	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
)

type chatHistoryNoopProvider struct{}

func (chatHistoryNoopProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("unexpected Chat call")
}

func (chatHistoryNoopProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, errors.New("unexpected ChatStream call")
}

type chatHistoryResolver struct {
	spaces map[string]*api.UserSpaceView
}

func (r *chatHistoryResolver) UserSpaceFor(userID string) (*api.UserSpaceView, error) {
	return r.spaces[userID], nil
}

func (r *chatHistoryResolver) LocalAgentManager() *agent.Manager { return nil }

func (r *chatHistoryResolver) IsCloudMode() bool { return false }

type chatHistoryResponse struct {
	History        []map[string]any `json:"history"`
	LatestEventSeq int64            `json:"latestEventSeq"`
	ContextUsage   struct {
		UsedTokens    int `json:"usedTokens"`
		ContextWindow int `json:"contextWindow"`
		TriggerTokens int `json:"triggerTokens"`
	} `json:"contextUsage"`
}

func TestHandleChatHistoryRestoresLatestContextUsage(t *testing.T) {
	ctx := context.Background()
	s, resolver, _, regularUser := newAuthTestServer(t, ctx)
	manager := newChatHistoryTestManager(t, regularUser.ID)
	s.SetUserResolver(&chatHistoryResolver{
		spaces: map[string]*api.UserSpaceView{
			regularUser.ID: {UserID: regularUser.ID, Agents: manager},
		},
	})

	if _, err := s.dataStore.AppendSessionEvent(ctx, regularUser.ID, "ctx-agent", "ctx-session", "usage", []byte(`{"usage":{"usedTokens":111,"contextWindow":64000,"triggerTokens":51200}}`)); err != nil {
		t.Fatalf("AppendSessionEvent usage: %v", err)
	}
	latestSeq, err := s.dataStore.AppendSessionEvent(ctx, regularUser.ID, "ctx-agent", "ctx-session", "done", []byte(`{"usage":{"usedTokens":1234,"contextWindow":64000,"triggerTokens":51200}}`))
	if err != nil {
		t.Fatalf("AppendSessionEvent done: %v", err)
	}

	body := requestChatHistory(t, s, resolver, regularUser.ID, "/api/chat/history?agentId=ctx-agent&sessionId=ctx-session")
	if body.LatestEventSeq != latestSeq {
		t.Fatalf("latestEventSeq = %d, want %d", body.LatestEventSeq, latestSeq)
	}
	if body.ContextUsage.UsedTokens != 1234 {
		t.Fatalf("contextUsage.usedTokens = %d, want 1234", body.ContextUsage.UsedTokens)
	}
	if body.ContextUsage.ContextWindow != 64000 {
		t.Fatalf("contextUsage.contextWindow = %d, want 64000", body.ContextUsage.ContextWindow)
	}
	if body.ContextUsage.TriggerTokens != 51200 {
		t.Fatalf("contextUsage.triggerTokens = %d, want 51200", body.ContextUsage.TriggerTokens)
	}
}

func TestHandleChatHistoryReturnsZeroContextUsageForNewSession(t *testing.T) {
	ctx := context.Background()
	s, resolver, _, regularUser := newAuthTestServer(t, ctx)
	manager := newChatHistoryTestManager(t, regularUser.ID)
	s.SetUserResolver(&chatHistoryResolver{
		spaces: map[string]*api.UserSpaceView{
			regularUser.ID: {UserID: regularUser.ID, Agents: manager},
		},
	})

	body := requestChatHistory(t, s, resolver, regularUser.ID, "/api/chat/history?agentId=ctx-agent&sessionId=new-session")
	if body.ContextUsage.UsedTokens != 0 {
		t.Fatalf("contextUsage.usedTokens = %d, want 0", body.ContextUsage.UsedTokens)
	}
	if body.ContextUsage.ContextWindow != 64000 {
		t.Fatalf("contextUsage.contextWindow = %d, want 64000", body.ContextUsage.ContextWindow)
	}
	if body.ContextUsage.TriggerTokens <= 0 || body.ContextUsage.TriggerTokens > body.ContextUsage.ContextWindow {
		t.Fatalf("contextUsage.triggerTokens = %d, want within (0, %d]", body.ContextUsage.TriggerTokens, body.ContextUsage.ContextWindow)
	}
}

func newChatHistoryTestManager(t *testing.T, userID string) *agent.Manager {
	t.Helper()

	root := t.TempDir()
	t.Setenv("BKCRAB_HOME", root)
	manager, err := agent.NewManager([]config.ResolvedAgent{{
		ID:            "ctx-agent",
		UserID:        userID,
		Home:          filepath.Join(root, "agents", "ctx-agent"),
		Workspace:     filepath.Join(root, "workspace", "ctx-agent"),
		Model:         "fake/model",
		MaxTokens:     100,
		ContextWindow: 64000,
	}}, chatHistoryNoopProvider{}, nil, agent.WithUserID(userID))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

func requestChatHistory(t *testing.T, s *Server, resolver *auth.Resolver, userID, target string) chatHistoryResponse {
	t.Helper()

	rr := httptest.NewRecorder()
	s.authMiddleware(s.handleChatHistory)(rr, authTestRequest(t, context.Background(), resolver, http.MethodGet, target, userID))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var body chatHistoryResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}
