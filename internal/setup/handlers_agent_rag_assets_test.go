package setup

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/api"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

const (
	agentRAGAssetAgentID   = "agt_session_rag_assets"
	agentRAGAssetSessionID = "session_with_rag_assets"
)

type agentRAGAssetFixture struct {
	*ragAssetFixture
	attacker *users.Account
	mux      *http.ServeMux
}

type agentRAGAssetResolver struct {
	spaces map[string]*api.UserSpaceView
}

func (r *agentRAGAssetResolver) UserSpaceFor(userID string) (*api.UserSpaceView, error) {
	return r.spaces[userID], nil
}

func (r *agentRAGAssetResolver) LocalAgentManager() *agent.Manager { return nil }

func (r *agentRAGAssetResolver) IsCloudMode() bool { return false }

type agentRAGAssetNoopProvider struct{}

func (agentRAGAssetNoopProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("unexpected Chat call")
}

func (agentRAGAssetNoopProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, errors.New("unexpected ChatStream call")
}

func newAgentRAGAssetFixture(t *testing.T) *agentRAGAssetFixture {
	t.Helper()
	ctx := context.Background()
	base := newRAGAssetFixture(t)
	attacker, err := base.server.accounts.Create(ctx, users.CreateInput{
		Username: "asset-attacker", Email: "asset-attacker@example.test", Password: "password", Role: users.RoleUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.server.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: agentRAGAssetAgentID, UserID: base.owner.ID, Name: "Public RAG agent", IsPublic: true,
	}); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	t.Setenv("BKCRAB_HOME", root)
	spaces := make(map[string]*api.UserSpaceView)
	for _, account := range []*users.Account{base.owner, base.other, attacker, base.admin} {
		spaces[account.ID] = &api.UserSpaceView{
			UserID: account.ID,
			Agents: newAgentRAGAssetManager(t, root, account.ID, base.owner.ID),
		}
	}
	base.server.SetUserResolver(&agentRAGAssetResolver{spaces: spaces})
	mux := http.NewServeMux()
	base.server.registerRAGRoutes(mux, base.server.authMiddleware)
	return &agentRAGAssetFixture{ragAssetFixture: base, attacker: attacker, mux: mux}
}

func newAgentRAGAssetManager(t *testing.T, root, userID, ownerID string) *agent.Manager {
	t.Helper()
	manager, err := agent.NewManager([]config.ResolvedAgent{{
		ID:            agentRAGAssetAgentID,
		UserID:        ownerID,
		Home:          filepath.Join(root, "agents", userID, agentRAGAssetAgentID),
		Workspace:     filepath.Join(root, "workspaces", userID, agentRAGAssetAgentID),
		Model:         "fake/model",
		MaxTokens:     128,
		ContextWindow: 4096,
	}}, agentRAGAssetNoopProvider{}, nil, agent.WithUserID(userID))
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func (f *agentRAGAssetFixture) saveSession(t *testing.T, userID, sessionID string, workingMessages []store.SessionMessage) {
	t.Helper()
	if err := f.server.dataStore.SaveSession(context.Background(), userID, agentRAGAssetAgentID, sessionID, &store.SessionRecord{
		Channel: "web", ChatID: sessionID, Messages: workingMessages, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *agentRAGAssetFixture) appendGrant(t *testing.T, userID, sessionID, role, kbID, docID, assetID string) {
	t.Helper()
	ref := rag.RAGResourceRef{
		Asset: document.AssetRef{
			ID: assetID, Kind: document.AssetKindImage, Caption: "Gateway diagram",
			Location: document.SourceLocation{Kind: document.LocationPage, Index: 2, Label: "Page 2"},
		},
		KBID: kbID, KBName: "asset KB", DocID: docID, DocName: "diagram.pdf",
		ChunkIndex: 0, SourceLocation: document.SourceLocation{Kind: document.LocationPage, Index: 2, Label: "Page 2"},
	}
	if err := f.server.dataStore.AppendSessionMessage(context.Background(), userID, agentRAGAssetAgentID, sessionID, store.SessionMessage{
		Role: role, Content: "answer", Metadata: map[string]interface{}{"ragResources": []rag.RAGResourceRef{ref}},
		Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *agentRAGAssetFixture) requestAgentAsset(
	t *testing.T,
	actorID, agentID, sessionID, assetID string,
	thumbnail bool,
	query string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/agents/" + agentID + "/chat/" + sessionID + "/rag-assets/" + assetID
	if thumbnail {
		path += "/thumbnail"
	}
	path += query
	request := authTestRequest(t, context.Background(), f.resolver, http.MethodGet, path, actorID)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	f.mux.ServeHTTP(recorder, request)
	return recorder
}

func (f *agentRAGAssetFixture) saveGoodGrant(t *testing.T) {
	t.Helper()
	f.saveSession(t, f.other.ID, agentRAGAssetSessionID, nil)
	f.appendGrant(t, f.other.ID, agentRAGAssetSessionID, "assistant", f.kb.ID, f.doc.ID, f.asset.ID)
}

func TestAgentRAGAssetRequiresArchivedAssistantGrantAndServesVariants(t *testing.T) {
	fixture := newAgentRAGAssetFixture(t)
	workingRef := rag.RAGResourceRef{
		Asset: document.AssetRef{ID: fixture.asset.ID}, KBID: fixture.kb.ID, DocID: fixture.doc.ID,
	}
	fixture.saveSession(t, fixture.other.ID, agentRAGAssetSessionID, []store.SessionMessage{{
		Role: "assistant", Metadata: map[string]interface{}{"ragResources": []rag.RAGResourceRef{workingRef}},
	}})

	fixture.objects.resetReads()
	workingSetOnly := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil)
	if workingSetOnly.Code != http.StatusNotFound || fixture.objects.reads() != 0 {
		t.Fatalf("working-set-only status=%d reads=%d", workingSetOnly.Code, fixture.objects.reads())
	}

	fixture.appendGrant(t, fixture.other.ID, agentRAGAssetSessionID, "user", fixture.kb.ID, fixture.doc.ID, fixture.asset.ID)
	fixture.objects.resetReads()
	userMetadata := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil)
	if userMetadata.Code != http.StatusNotFound || fixture.objects.reads() != 0 {
		t.Fatalf("user metadata status=%d reads=%d", userMetadata.Code, fixture.objects.reads())
	}

	fixture.appendGrant(t, fixture.other.ID, agentRAGAssetSessionID, "assistant", fixture.kb.ID, fixture.doc.ID, fixture.asset.ID)
	display := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil)
	if display.Code != http.StatusOK || !bytes.Equal(display.Body.Bytes(), fixture.display) {
		t.Fatalf("display status=%d body=%q", display.Code, display.Body.Bytes())
	}
	for name, want := range map[string]string{
		"Content-Type": "image/png", "Content-Disposition": "inline", "Cache-Control": "private, no-cache",
		"X-Content-Type-Options": "nosniff", "Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := display.Header().Get(name); got != want {
			t.Fatalf("%s=%q want=%q", name, got, want)
		}
	}

	thumbnail := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, true, "", nil)
	if thumbnail.Code != http.StatusOK || !bytes.Equal(thumbnail.Body.Bytes(), fixture.thumb) {
		t.Fatalf("thumbnail status=%d body=%q", thumbnail.Code, thumbnail.Body.Bytes())
	}
	if display.Header().Get("ETag") == thumbnail.Header().Get("ETag") {
		t.Fatalf("display and thumbnail unexpectedly share ETag %q", display.Header().Get("ETag"))
	}

	fixture.objects.resetReads()
	notModified := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", map[string]string{
		"If-None-Match": display.Header().Get("ETag"),
	})
	if notModified.Code != http.StatusNotModified || fixture.objects.reads() != 0 {
		t.Fatalf("conditional status=%d reads=%d", notModified.Code, fixture.objects.reads())
	}

	actAs := fixture.requestAgentAsset(t, fixture.admin.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "?actAs="+fixture.other.ID, nil)
	if actAs.Code != http.StatusOK || !bytes.Equal(actAs.Body.Bytes(), fixture.display) {
		t.Fatalf("admin actAs status=%d body=%q", actAs.Code, actAs.Body.Bytes())
	}
}

func TestAgentRAGAssetRejectsCrossScopeAndMismatchedReferencesBeforeRead(t *testing.T) {
	fixture := newAgentRAGAssetFixture(t)
	fixture.saveSession(t, fixture.other.ID, "wrong_doc", nil)
	fixture.appendGrant(t, fixture.other.ID, "wrong_doc", "assistant", fixture.kb.ID, "doc_other", fixture.asset.ID)
	fixture.saveSession(t, fixture.other.ID, "wrong_kb", nil)
	fixture.appendGrant(t, fixture.other.ID, "wrong_kb", "assistant", "kb_other", fixture.doc.ID, fixture.asset.ID)
	fixture.saveGoodGrant(t)
	fixture.saveSession(t, fixture.attacker.ID, "attacker_session", nil)
	fixture.appendGrant(t, fixture.attacker.ID, "attacker_session", "assistant", fixture.kb.ID, fixture.doc.ID, fixture.asset.ID)

	unknownAssetID := "ast_ffffffffffffffffffffffffffffffff"
	tests := []struct {
		name                        string
		actorID, agentID, sessionID string
		assetID                     string
	}{
		{name: "wrong document provenance", actorID: fixture.other.ID, agentID: agentRAGAssetAgentID, sessionID: "wrong_doc", assetID: fixture.asset.ID},
		{name: "wrong knowledge base provenance", actorID: fixture.other.ID, agentID: agentRAGAssetAgentID, sessionID: "wrong_kb", assetID: fixture.asset.ID},
		{name: "cross user session", actorID: fixture.attacker.ID, agentID: agentRAGAssetAgentID, sessionID: agentRAGAssetSessionID, assetID: fixture.asset.ID},
		{name: "other session", actorID: fixture.other.ID, agentID: agentRAGAssetAgentID, sessionID: "attacker_session", assetID: fixture.asset.ID},
		{name: "guessed asset id", actorID: fixture.other.ID, agentID: agentRAGAssetAgentID, sessionID: agentRAGAssetSessionID, assetID: unknownAssetID},
		{name: "other agent", actorID: fixture.other.ID, agentID: "agt_other", sessionID: agentRAGAssetSessionID, assetID: fixture.asset.ID},
	}
	fixture.objects.resetReads()
	var canonicalBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.requestAgentAsset(t, test.actorID, test.agentID, test.sessionID, test.assetID, false, "", map[string]string{
				"If-None-Match": "*",
			})
			if response.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if canonicalBody == "" {
				canonicalBody = response.Body.String()
			} else if response.Body.String() != canonicalBody {
				t.Fatalf("denial body=%q want=%q", response.Body.String(), canonicalBody)
			}
		})
	}
	if fixture.objects.reads() != 0 {
		t.Fatalf("authorization denials performed %d object reads", fixture.objects.reads())
	}
}

func TestAgentRAGAssetRevokesDeletedGrantAndCurrentTombstonesBefore304(t *testing.T) {
	t.Run("assistant message deleted", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		db := fixture.server.dataStore.(*store.DBStore)
		if _, err := db.DB().ExecContext(context.Background(), `DELETE FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND role='assistant'`, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("session deleted", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		if err := fixture.server.dataStore.DeleteSession(context.Background(), fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("agent deleted", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		db := fixture.server.dataStore.(*store.DBStore)
		if _, err := db.DB().ExecContext(context.Background(), `DELETE FROM agents WHERE id=?`, agentRAGAssetAgentID); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("public sharing revoked", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		record, err := fixture.server.dataStore.GetAgent(context.Background(), agentRAGAssetAgentID)
		if err != nil {
			t.Fatal(err)
		}
		record.IsPublic = false
		if err := fixture.server.dataStore.SaveAgent(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("document deleting", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		db := fixture.server.dataStore.(*store.DBStore)
		if _, err := db.DB().ExecContext(context.Background(), `UPDATE rag_documents SET status='DELETING' WHERE id=?`, fixture.doc.ID); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("asset display unavailable", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		db := fixture.server.dataStore.(*store.DBStore)
		if _, err := db.DB().ExecContext(context.Background(), `UPDATE rag_assets SET display_status='unavailable' WHERE id=?`, fixture.asset.ID); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("knowledge base deleting", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		if _, err := fixture.server.dataStore.MarkRAGKBDeleting(context.Background(), fixture.kb.ID); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})

	t.Run("asset owner inactive", func(t *testing.T) {
		fixture := newAgentRAGAssetFixture(t)
		fixture.saveGoodGrant(t)
		etag := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", nil).Header().Get("ETag")
		owner, err := fixture.server.dataStore.GetUser(context.Background(), fixture.owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		owner.Status = "disabled"
		if err := fixture.server.dataStore.UpdateUser(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
		assertAgentRAGAssetRevoked(t, fixture, etag)
	})
}

func assertAgentRAGAssetRevoked(t *testing.T, fixture *agentRAGAssetFixture, etag string) {
	t.Helper()
	fixture.objects.resetReads()
	response := fixture.requestAgentAsset(t, fixture.other.ID, agentRAGAssetAgentID, agentRAGAssetSessionID, fixture.asset.ID, false, "", map[string]string{
		"If-None-Match": etag,
	})
	if response.Code != http.StatusNotFound || fixture.objects.reads() != 0 {
		t.Fatalf("revoked status=%d body=%s reads=%d", response.Code, response.Body.String(), fixture.objects.reads())
	}
}
