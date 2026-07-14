package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

func newRAGAPITestServer(t *testing.T) (*Server, *auth.Resolver, *users.Account, *users.Account, *rag.Service) {
	return newRAGAPITestServerWithMaxFileMB(t, 0)
}

func newRAGAPITestServerWithMaxFileMB(t *testing.T, maxFileMB int) (*Server, *auth.Resolver, *users.Account, *users.Account, *rag.Service) {
	t.Helper()
	ctx := context.Background()
	server, resolver, admin, regular := newAuthTestServer(t, ctx)
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		data := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(embedServer.Close)
	cfg := config.RAGCfg{
		Milvus:    config.MilvusCfg{Address: "fake"},
		Embedding: config.RAGEmbeddingCfg{Endpoint: embedServer.URL, Model: "test", Dims: 4},
		Limits:    config.RAGLimitsCfg{MaxFileMB: maxFileMB},
	}
	service := rag.New(rag.Deps{
		Store:   server.dataStore,
		Vector:  vector.NewFake(),
		Objects: objects.NewLocalFS(t.TempDir()),
		Cfg:     cfg,
		Workers: 1,
	})
	workerCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.Start(workerCtx)
	server.SetRAGService(service)
	return server, resolver, admin, regular, service
}

func ragMultipartUploadRequest(t *testing.T, resolver *auth.Resolver, kbID, userID, fileName string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := authTestRequest(t, context.Background(), resolver, http.MethodPost, "/api/rag/kbs/"+kbID+"/documents", userID)
	request.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func ragJSONRequest(t *testing.T, resolver *auth.Resolver, method, path, userID, body string) *http.Request {
	t.Helper()
	request := authTestRequest(t, context.Background(), resolver, method, path, userID)
	request.Body = io.NopCloser(strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func callRAGHandler(t *testing.T, server *Server, handler http.HandlerFunc, request *http.Request, pathValues map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	for key, value := range pathValues {
		request.SetPathValue(key, value)
	}
	recorder := httptest.NewRecorder()
	server.authMiddleware(handler)(recorder, request)
	return recorder
}

func saveRAGTestAgent(t *testing.T, server *Server, ownerID string, cfg map[string]interface{}) *store.AgentRecord {
	t.Helper()
	rec := &store.AgentRecord{ID: "agt_rag_auth", UserID: ownerID, Name: "RAG Agent", Config: cfg}
	if err := server.dataStore.SaveAgent(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func updateRAGTestAgent(t *testing.T, server *Server, resolver *auth.Resolver, actorID, agentID, body string) *httptest.ResponseRecorder {
	t.Helper()
	return callRAGHandler(t, server, server.handleUpdateAgent,
		ragJSONRequest(t, resolver, http.MethodPut, "/api/agents/"+agentID, actorID, body),
		map[string]string{"id": agentID})
}

func loadRAGTestAgentConfig(t *testing.T, server *Server, agentID string) (*store.AgentRecord, config.AgentFileConfig) {
	t.Helper()
	rec, err := server.dataStore.GetAgent(context.Background(), agentID)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(rec.Config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.AgentFileConfig
	if err := json.Unmarshal(blob, &cfg); err != nil {
		t.Fatal(err)
	}
	return rec, cfg
}

func TestRAGDisabledReturnsServiceUnavailable(t *testing.T) {
	ctx := context.Background()
	server, resolver, _, regular := newAuthTestServer(t, ctx)
	recorder := callRAGHandler(t, server, server.handleListRAGKBs,
		authTestRequest(t, ctx, resolver, http.MethodGet, "/api/rag/kbs", regular.ID), nil)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "RAG 未配置") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRAGKBCRUDViaAPI(t *testing.T) {
	server, resolver, _, regular, _ := newRAGAPITestServer(t)
	create := callRAGHandler(t, server, server.handleCreateRAGKB,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs", regular.ID, `{"name":"产品手册","description":"说明"}`), nil)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	var kb store.RAGKBRecord
	if err := json.NewDecoder(create.Body).Decode(&kb); err != nil || kb.ID == "" {
		t.Fatalf("create body=%s err=%v", create.Body.String(), err)
	}

	list := callRAGHandler(t, server, server.handleListRAGKBs,
		authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/kbs", regular.ID), nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), kb.ID) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}

	update := callRAGHandler(t, server, server.handleUpdateRAGKB,
		ragJSONRequest(t, resolver, http.MethodPatch, "/api/rag/kbs/"+kb.ID, regular.ID, `{"name":"新版手册"}`),
		map[string]string{"id": kb.ID})
	if update.Code != http.StatusOK || !strings.Contains(update.Body.String(), "新版手册") {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}

	deleteResponse := callRAGHandler(t, server, server.handleDeleteRAGKB,
		authTestRequest(t, context.Background(), resolver, http.MethodDelete, "/api/rag/kbs/"+kb.ID, regular.ID),
		map[string]string{"id": kb.ID})
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	get := callRAGHandler(t, server, server.handleGetRAGKB,
		authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/kbs/"+kb.ID, regular.ID),
		map[string]string{"id": kb.ID})
	if get.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status=%d body=%s", get.Code, get.Body.String())
	}
}

func TestRAGKBOwnershipAndAdminAccess(t *testing.T) {
	server, resolver, admin, owner, service := newRAGAPITestServer(t)
	other := createAuthTestUser(t, context.Background(), server.accounts, "rag-other", users.RoleUser)
	kb, err := service.CreateKB(context.Background(), owner.ID, "私有库", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	pathValues := map[string]string{"id": kb.ID}
	otherGet := callRAGHandler(t, server, server.handleGetRAGKB,
		authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/kbs/"+kb.ID, other.ID), pathValues)
	if otherGet.Code != http.StatusForbidden {
		t.Fatalf("other status=%d body=%s", otherGet.Code, otherGet.Body.String())
	}
	adminGet := callRAGHandler(t, server, server.handleGetRAGKB,
		authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/kbs/"+kb.ID, admin.ID), pathValues)
	if adminGet.Code != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", adminGet.Code, adminGet.Body.String())
	}
}

func TestRAGKBDeleteRejectsOtherOwnerAndPreservesKB(t *testing.T) {
	server, resolver, _, owner, service := newRAGAPITestServer(t)
	other := createAuthTestUser(t, context.Background(), server.accounts, "rag-delete-other", users.RoleUser)
	kb, err := service.CreateKB(context.Background(), owner.ID, "Private KB", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	response := callRAGHandler(t, server, server.handleDeleteRAGKB,
		authTestRequest(t, context.Background(), resolver, http.MethodDelete, "/api/rag/kbs/"+kb.ID, other.ID),
		map[string]string{"id": kb.ID})
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-owner delete status=%d body=%s", response.Code, response.Body.String())
	}

	persisted, err := service.GetKB(context.Background(), owner.ID, kb.ID)
	if err != nil {
		t.Fatalf("owner could not read KB after rejected delete: %v", err)
	}
	if persisted.Status != "active" {
		t.Fatalf("rejected delete mutated KB status: %+v", persisted)
	}
}

func TestRAGDocumentUploadListAndSearch(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	kb, err := service.CreateKB(context.Background(), regular.ID, "手册", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "guide.md")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("# 安装\n\n安装需要管理员权限。"))
	_ = writer.Close()
	request := authTestRequest(t, context.Background(), resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/documents", regular.ID)
	request.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	request.Header.Set("Content-Type", writer.FormDataContentType())
	upload := callRAGHandler(t, server, server.handleUploadRAGDocument, request, map[string]string{"id": kb.ID})
	if upload.Code != http.StatusAccepted {
		t.Fatalf("upload status=%d body=%s", upload.Code, upload.Body.String())
	}
	var document store.RAGDocumentRecord
	if err := json.NewDecoder(upload.Body).Decode(&document); err != nil || document.ID == "" {
		t.Fatalf("upload body=%s err=%v", upload.Body.String(), err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		doc, getErr := service.GetDocument(context.Background(), regular.ID, kb.ID, document.ID)
		if getErr == nil && doc.Status == "DONE" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	indexed, _ := service.GetDocument(context.Background(), regular.ID, kb.ID, document.ID)
	if indexed == nil || indexed.Status != "DONE" {
		t.Fatalf("document not indexed: %+v", indexed)
	}

	list := callRAGHandler(t, server, server.handleListRAGDocuments,
		authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/kbs/"+kb.ID+"/documents", regular.ID),
		map[string]string{"id": kb.ID})
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), "guide.md") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	search := callRAGHandler(t, server, server.handleRAGSearch,
		ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/search", regular.ID, `{"query":"安装权限","topN":3}`),
		map[string]string{"id": kb.ID})
	if search.Code != http.StatusOK || !strings.Contains(search.Body.String(), "guide.md") {
		t.Fatalf("search status=%d body=%s", search.Code, search.Body.String())
	}
}

func TestRAGDocumentUploadRejectsUnsupportedExtension(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	kb, err := service.CreateKB(context.Background(), regular.ID, "Upload validation", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	request := ragMultipartUploadRequest(t, resolver, kb.ID, regular.ID, "payload.exe", []byte("not a document"))
	response := callRAGHandler(t, server, server.handleUploadRAGDocument, request, map[string]string{"id": kb.ID})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unsupported extension status=%d body=%s", response.Code, response.Body.String())
	}
	docs, err := service.ListDocuments(context.Background(), regular.ID, kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatalf("unsupported upload persisted documents: %+v", docs)
	}
}

func TestRAGDocumentUploadRejectsOversizeAtBothLimits(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServerWithMaxFileMB(t, 1)
	kb, err := service.CreateKB(context.Background(), regular.ID, "Upload limits", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	const mebibyte = 1024 * 1024
	tests := []struct {
		name             string
		fileSize         int
		wantServiceQuota bool
	}{
		{name: "service_file_size_limit", fileSize: mebibyte + 1, wantServiceQuota: true},
		{name: "http_request_body_limit", fileSize: 2 * mebibyte, wantServiceQuota: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := ragMultipartUploadRequest(t, resolver, kb.ID, regular.ID, "oversize.md", bytes.Repeat([]byte{'x'}, test.fileSize))
			response := callRAGHandler(t, server, server.handleUploadRAGDocument, request, map[string]string{"id": kb.ID})
			if response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversize status=%d body=%s", response.Code, response.Body.String())
			}
			gotServiceQuota := strings.Contains(response.Body.String(), rag.ErrQuota.Error())
			if gotServiceQuota != test.wantServiceQuota {
				t.Fatalf("wrong validation layer: service quota marker=%v body=%s", gotServiceQuota, response.Body.String())
			}
		})
	}

	docs, err := service.ListDocuments(context.Background(), regular.ID, kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 0 {
		t.Fatalf("oversize uploads persisted documents: %+v", docs)
	}
}

func TestAgentUpdatePersistsOwnedRAGAuthorization(t *testing.T) {
	server, resolver, _, owner, service := newRAGAPITestServer(t)
	agent := saveRAGTestAgent(t, server, owner.ID, map[string]interface{}{"description": "kept"})
	kb, err := service.CreateKB(context.Background(), owner.ID, "Owner KB", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	response := updateRAGTestAgent(t, server, resolver, owner.ID, agent.ID,
		fmt.Sprintf(`{"rag":{"kbs":[%q,%q],"topN":7}}`, kb.ID, kb.ID))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	rec, cfg := loadRAGTestAgentConfig(t, server, agent.ID)
	if cfg.RAG == nil || len(cfg.RAG.KBs) != 1 || cfg.RAG.KBs[0] != kb.ID || cfg.RAG.TopN != 7 {
		t.Fatalf("persisted AgentFileConfig RAG = %+v; raw config=%v", cfg.RAG, rec.Config)
	}
	if rec.Config["description"] != "kept" {
		t.Fatalf("unrelated config was not preserved: %v", rec.Config)
	}
}

func TestAgentUpdateRAGUsesAgentOwnerAsAuthorizationBoundary(t *testing.T) {
	server, resolver, admin, owner, service := newRAGAPITestServer(t)
	agent := saveRAGTestAgent(t, server, owner.ID, nil)
	ownerKB, err := service.CreateKB(context.Background(), owner.ID, "Owner KB", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	adminKB, err := service.CreateKB(context.Background(), admin.ID, "Admin KB", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A super-admin may manage the agent, and may grant a KB owned by that
	// agent's owner.
	allowed := updateRAGTestAgent(t, server, resolver, admin.ID, agent.ID,
		fmt.Sprintf(`{"rag":{"kbs":[%q]}}`, ownerKB.ID))
	if allowed.Code != http.StatusOK {
		t.Fatalf("admin owner-KB grant status=%d body=%s", allowed.Code, allowed.Body.String())
	}

	// The same administrator cannot grant their own KB to somebody else's
	// agent. Validation happens before SaveAgent, so the name and prior grant
	// must remain unchanged.
	forbidden := updateRAGTestAgent(t, server, resolver, admin.ID, agent.ID,
		fmt.Sprintf(`{"name":"must-not-save","rag":{"kbs":[%q]}}`, adminKB.ID))
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("cross-owner grant status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	rec, cfg := loadRAGTestAgentConfig(t, server, agent.ID)
	if rec.Name != "RAG Agent" || cfg.RAG == nil || len(cfg.RAG.KBs) != 1 || cfg.RAG.KBs[0] != ownerKB.ID {
		t.Fatalf("forbidden update partially persisted: rec=%+v rag=%+v", rec, cfg.RAG)
	}

	missing := updateRAGTestAgent(t, server, resolver, owner.ID, agent.ID,
		`{"name":"also-must-not-save","rag":{"kbs":["kb_missing"]}}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("dangling grant status=%d body=%s", missing.Code, missing.Body.String())
	}
	rec, cfg = loadRAGTestAgentConfig(t, server, agent.ID)
	if rec.Name != "RAG Agent" || cfg.RAG == nil || cfg.RAG.KBs[0] != ownerKB.ID {
		t.Fatalf("dangling update partially persisted: rec=%+v rag=%+v", rec, cfg.RAG)
	}
}

func TestAgentUpdateClearsRAGAuthorization(t *testing.T) {
	server, resolver, _, owner, service := newRAGAPITestServer(t)
	agent := saveRAGTestAgent(t, server, owner.ID, nil)
	kb, err := service.CreateKB(context.Background(), owner.ID, "Owner KB", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	set := updateRAGTestAgent(t, server, resolver, owner.ID, agent.ID,
		fmt.Sprintf(`{"rag":{"kbs":[%q],"topN":9}}`, kb.ID))
	if set.Code != http.StatusOK {
		t.Fatalf("set status=%d body=%s", set.Code, set.Body.String())
	}

	clear := updateRAGTestAgent(t, server, resolver, owner.ID, agent.ID, `{"rag":{"kbs":[]}}`)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", clear.Code, clear.Body.String())
	}
	rec, cfg := loadRAGTestAgentConfig(t, server, agent.ID)
	if cfg.RAG != nil {
		t.Fatalf("RAG authorization not cleared: %+v", cfg.RAG)
	}
	if _, exists := rec.Config["rag"]; exists {
		t.Fatalf("empty RAG config should be removed from agents.config: %v", rec.Config)
	}
}

func TestAgentUpdateRAGWhenServiceDisabled(t *testing.T) {
	server, resolver, _, owner := newAuthTestServer(t, context.Background())
	agent := saveRAGTestAgent(t, server, owner.ID, map[string]interface{}{
		"rag": map[string]interface{}{"kbs": []string{"kb_old"}, "topN": 4},
	})

	unverifiable := updateRAGTestAgent(t, server, resolver, owner.ID, agent.ID,
		`{"name":"must-not-save","rag":{"kbs":["kb_new"]}}`)
	if unverifiable.Code != http.StatusServiceUnavailable || !strings.Contains(unverifiable.Body.String(), "RAG 未配置") {
		t.Fatalf("disabled non-empty update status=%d body=%s", unverifiable.Code, unverifiable.Body.String())
	}
	rec, cfg := loadRAGTestAgentConfig(t, server, agent.ID)
	if rec.Name != "RAG Agent" || cfg.RAG == nil || len(cfg.RAG.KBs) != 1 || cfg.RAG.KBs[0] != "kb_old" {
		t.Fatalf("disabled update partially persisted: rec=%+v rag=%+v", rec, cfg.RAG)
	}

	// Revocation is safe without the service because it introduces no KB
	// reference that needs ownership validation.
	clear := updateRAGTestAgent(t, server, resolver, owner.ID, agent.ID, `{"rag":{"kbs":[]}}`)
	if clear.Code != http.StatusOK {
		t.Fatalf("disabled clear status=%d body=%s", clear.Code, clear.Body.String())
	}
	_, cfg = loadRAGTestAgentConfig(t, server, agent.ID)
	if cfg.RAG != nil {
		t.Fatalf("disabled clear left authorization behind: %+v", cfg.RAG)
	}
}
