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
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/scope"
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

func TestRAGCapabilitiesUseCachedSnapshotAndKeepIndependentGates(t *testing.T) {
	server, resolver, _, regular, _ := newRAGAPITestServer(t)
	cfg := config.RAGCfg{
		Features: config.RAGFeatureCfg{
			AdvancedParsingEnabled: true,
			OfficeParsingEnabled:   true,
			TextEnrichmentEnabled:  true,
		},
		DocumentAI: config.RAGDocumentAICfg{
			APIType:              "openai-compatible",
			Endpoint:             "http://127.0.0.1:1/v1",
			APIKey:               "must-not-leak",
			VisionModel:          "vision-test",
			TextModel:            "text-test",
			AllowedEndpointHosts: []string{"127.0.0.1"},
			AllowPrivateEndpoint: true,
		},
		ParserSidecar: config.RAGParserSidecarCfg{Endpoint: "http://127.0.0.1:1"},
		Limits: config.RAGLimitsCfg{
			MaxFileMB:                     50,
			MaxDocumentAIRequests:         300,
			MaxDocumentAITokens:           200_000,
			MaxEstimatedDocumentAICostUSD: 1,
		},
	}
	cfg.ApplyDefaults()
	server.SetRAGConfig(cfg)
	checkedAt := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	server.SetRAGParserHealthSnapshot(config.RAGParserHealthSnapshot{
		ProtocolVersion: "rag-parser/v1",
		Healthy:         true,
		CheckedAt:       checkedAt,
		ExpiresAt:       time.Now().Add(time.Minute),
		MaxInputBytes:   10 * 1024 * 1024,
		Office: config.RAGParserOfficeSnapshot{
			Enabled:    true,
			Formats:    []string{"docx", "pptx", "xlsx"},
			DOCXGolden: true,
			PPTXGolden: true,
			XLSXGolden: true,
		},
		PDF: config.RAGParserPDFSnapshot{Enabled: false, LicenseApproved: false},
	})

	request := authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/capabilities", regular.ID)
	recorder := callRAGHandler(t, server, server.handleRAGCapabilities, request, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("capabilities status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		SupportedExtensions     []string         `json:"supportedExtensions"`
		MaxFileBytes            int64            `json:"maxFileBytes"`
		MaxFileBytesByExtension map[string]int64 `json:"maxFileBytesByExtension"`
		Advanced                struct {
			Available bool      `json:"available"`
			CheckedAt time.Time `json:"checkedAt"`
		} `json:"advanced"`
		Office struct {
			Available bool `json:"available"`
		} `json:"office"`
		PDFAuto struct {
			Available bool `json:"available"`
		} `json:"pdfAuto"`
		OfficeVision struct {
			Available bool `json:"available"`
		} `json:"officeVision"`
		Enrichment struct {
			Available bool `json:"available"`
		} `json:"enrichment"`
		DocumentAIBudget struct {
			MaxRequestsPerDocument         int     `json:"maxRequestsPerDocument"`
			MaxTokensPerDocument           int64   `json:"maxTokensPerDocument"`
			MaxEstimatedCostUSDPerDocument float64 `json:"maxEstimatedCostUSDPerDocument"`
		} `json:"documentAIBudget"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	wantExtensions := []string{".md", ".markdown", ".txt", ".pdf", ".docx", ".pptx", ".xlsx"}
	if fmt.Sprint(response.SupportedExtensions) != fmt.Sprint(wantExtensions) {
		t.Fatalf("supportedExtensions=%v want=%v", response.SupportedExtensions, wantExtensions)
	}
	if response.MaxFileBytes != 50*1024*1024 || response.MaxFileBytesByExtension[".md"] != 50*1024*1024 ||
		response.MaxFileBytesByExtension[".pdf"] != 10*1024*1024 ||
		response.MaxFileBytesByExtension[".docx"] != 10*1024*1024 {
		t.Fatalf("file limits = max:%d byExt:%v", response.MaxFileBytes, response.MaxFileBytesByExtension)
	}
	if !response.Advanced.Available || !response.Office.Available || response.PDFAuto.Available ||
		!response.OfficeVision.Available || !response.Enrichment.Available {
		t.Fatalf("independent capability gates decoded from body=%s", recorder.Body.String())
	}
	if !response.Advanced.CheckedAt.Equal(checkedAt) || response.DocumentAIBudget.MaxRequestsPerDocument != 300 ||
		response.DocumentAIBudget.MaxTokensPerDocument != 200_000 || response.DocumentAIBudget.MaxEstimatedCostUSDPerDocument != 1 {
		t.Fatalf("snapshot/budget mismatch: %+v body=%s", response, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "must-not-leak") || strings.Contains(recorder.Body.String(), "127.0.0.1") {
		t.Fatalf("capability DTO leaked credentials or internal endpoint: %s", recorder.Body.String())
	}
}

func TestRAGCapabilitiesRouteRegisteredAndExpiredSnapshotIsUnavailable(t *testing.T) {
	server := NewServer(0)
	mux := http.NewServeMux()
	server.registerRAGRoutes(mux, func(handler http.HandlerFunc) http.HandlerFunc { return handler })
	request := httptest.NewRequest(http.MethodGet, "/api/rag/capabilities", nil)
	_, pattern := mux.Handler(request)
	if pattern != "GET /api/rag/capabilities" {
		t.Fatalf("capability route pattern=%q", pattern)
	}

	cfg := config.RAGCfg{
		Features:      config.RAGFeatureCfg{OfficeParsingEnabled: true},
		ParserSidecar: config.RAGParserSidecarCfg{Endpoint: "http://rag-parser:8080"},
	}
	cfg.ApplyDefaults()
	server.SetRAGConfig(cfg)
	server.SetRAGParserHealthSnapshot(config.RAGParserHealthSnapshot{
		ProtocolVersion: "rag-parser/v1",
		Healthy:         true,
		CheckedAt:       time.Now().Add(-2 * time.Minute),
		ExpiresAt:       time.Now().Add(-time.Minute),
		Office: config.RAGParserOfficeSnapshot{
			Enabled: true, Formats: []string{"docx", "pptx", "xlsx"},
			DOCXGolden: true, PPTXGolden: true, XLSXGolden: true,
		},
	})
	recorder := httptest.NewRecorder()
	server.handleRAGCapabilities(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "parser_health_stale") {
		t.Fatalf("expired snapshot status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRAGCapabilitiesSnapshotWithoutTTLIsUnavailable(t *testing.T) {
	server := NewServer(0)
	cfg := config.RAGCfg{
		Features:      config.RAGFeatureCfg{OfficeParsingEnabled: true},
		ParserSidecar: config.RAGParserSidecarCfg{Endpoint: "http://rag-parser:8080"},
	}
	cfg.ApplyDefaults()
	server.SetRAGConfig(cfg)
	server.SetRAGParserHealthSnapshot(config.RAGParserHealthSnapshot{
		ProtocolVersion: "rag-parser/v1",
		Healthy:         true,
		CheckedAt:       time.Now().UTC(),
		MaxInputBytes:   1024,
		Office: config.RAGParserOfficeSnapshot{
			Enabled: true, Formats: []string{"docx", "pptx", "xlsx"},
			DOCXGolden: true, PPTXGolden: true, XLSXGolden: true,
		},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/rag/capabilities", nil)
	server.handleRAGCapabilities(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "parser_health_stale") {
		t.Fatalf("TTL-less snapshot status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), `".docx":1024`) {
		t.Fatalf("TTL-less sidecar limit leaked into response: %s", recorder.Body.String())
	}
}

func TestRAGCapabilitiesOfficeGateFailureDoesNotDisableBaseRAG(t *testing.T) {
	server, resolver, _, regular, _ := newRAGAPITestServer(t)
	cfg := config.RAGCfg{
		Features:      config.RAGFeatureCfg{OfficeParsingEnabled: true},
		ParserSidecar: config.RAGParserSidecarCfg{Endpoint: "http://rag-parser:8080"},
	}
	cfg.ApplyDefaults()
	server.SetRAGConfig(cfg)
	server.SetRAGParserHealthSnapshot(config.RAGParserHealthSnapshot{
		ProtocolVersion: "rag-parser/v1",
		Healthy:         true,
		CheckedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().Add(time.Minute),
		Office: config.RAGParserOfficeSnapshot{
			Enabled: true,
			Formats: []string{"docx", "pptx", "xlsx"},
			// Golden checks intentionally remain false.
		},
	})

	request := authTestRequest(t, context.Background(), resolver, http.MethodGet, "/api/rag/capabilities", regular.ID)
	recorder := callRAGHandler(t, server, server.handleRAGCapabilities, request, nil)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"office"`) ||
		!strings.Contains(recorder.Body.String(), `"available":false`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// The ordinary RAG service remains installed and usable despite sidecar
	// capability failure.
	if !server.requireRAG(httptest.NewRecorder()) {
		t.Fatal("Office capability failure disabled base RAG")
	}
}

func TestRAGDTOUsesCamelCaseAndOmitsObjectStorageKeys(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	kb, err := service.CreateKB(context.Background(), regular.ID, "DTO", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	response := callRAGHandler(t, server, server.handleUploadRAGDocument,
		ragMultipartUploadRequest(t, resolver, kb.ID, regular.ID, "dto.md", []byte("# DTO\n\nbody")),
		map[string]string{"id": kb.ID})
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["id"] == nil || body["fileName"] != "dto.md" {
		t.Fatalf("camelCase DTO missing expected fields: %v", body)
	}
	for _, forbidden := range []string{"ID", "FileName", "ObjectKey", "objectKey", "artifactKey"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("DTO exposed forbidden/internal field %q: %v", forbidden, body)
		}
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

func TestGenerateRAGKBMetadataViaAPI(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	ctx := context.Background()

	var receivedPrompt string
	var receivedSystemPrompt string
	var receivedMaxTokens int
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode LLM request: %v", err)
		}
		for _, message := range request.Messages {
			switch message.Role {
			case "system":
				receivedSystemPrompt = message.Content
			case "user":
				receivedPrompt = message.Content
			}
		}
		receivedMaxTokens = request.MaxTokens
		content := `{"name":"产品安装与故障处理","description":"包含产品安装步骤、权限要求和常见故障处理流程，主要用于支持部署实施与售后排障。"}`
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": content}}},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	t.Cleanup(llmServer.Close)
	if err := scope.SaveSetting(ctx, server.dataStore, regular.ID, "", "agents.defaults", map[string]any{
		"model": "test/metadata-model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.SaveProvider(ctx, server.dataStore, regular.ID, "", "test", config.ProviderConfig{
		APIKey: "test-key", APIBase: llmServer.URL, APIType: "openai-chat",
	}); err != nil {
		t.Fatal(err)
	}

	kb, err := service.CreateKB(ctx, regular.ID, "临时名称", "临时描述", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	content := "# 安装与排障\n\n安装需要管理员权限。遇到启动失败时检查配置与服务日志。"
	document, err := service.UploadDocument(ctx, regular.ID, kb.ID, "guide.md", strings.NewReader(content), int64(len([]byte(content))))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		indexed, getErr := service.GetDocument(ctx, regular.ID, kb.ID, document.ID)
		if getErr == nil && indexed.Status == "DONE" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	request := authTestRequest(t, ctx, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/generate-metadata", regular.ID)
	response := callRAGHandler(t, server, server.handleGenerateRAGKBMetadata, request, map[string]string{"id": kb.ID})
	if response.Code != http.StatusOK {
		t.Fatalf("generate status=%d body=%s", response.Code, response.Body.String())
	}
	var generated struct {
		Name                 string `json:"name"`
		Description          string `json:"description"`
		DocumentCount        int    `json:"documentCount"`
		SampledDocumentCount int    `json:"sampledDocumentCount"`
	}
	if err := json.NewDecoder(response.Body).Decode(&generated); err != nil {
		t.Fatal(err)
	}
	if generated.Name != "产品安装与故障处理" || !strings.Contains(generated.Description, "售后排障") {
		t.Fatalf("generated metadata = %+v", generated)
	}
	if generated.DocumentCount != 1 || generated.SampledDocumentCount != 1 {
		t.Fatalf("generated document counts = %+v", generated)
	}
	if !strings.Contains(receivedPrompt, "guide.md") || !strings.Contains(receivedPrompt, "管理员权限") {
		t.Fatalf("LLM prompt missing indexed source: %q", receivedPrompt)
	}
	if receivedMaxTokens != ragMetadataMaxOutputTokens {
		t.Fatalf("metadata max_tokens = %d, want %d", receivedMaxTokens, ragMetadataMaxOutputTokens)
	}
	if !strings.Contains(receivedSystemPrompt, "只能包含 name 和 description 两个字段") ||
		!strings.Contains(receivedSystemPrompt, `{"name":"知识库名称","description":"说明包含哪些内容，以及主要用途是什么"}`) {
		t.Fatalf("metadata system prompt missing strict JSON contract: %q", receivedSystemPrompt)
	}
	persisted, err := service.GetKB(ctx, regular.ID, kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Name != "临时名称" || persisted.Description != "临时描述" {
		t.Fatalf("generation unexpectedly saved metadata: %+v", persisted)
	}
}

func TestRAGChatUsesQuestionOnlyHistoryAndReturnsSources(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	ctx := context.Background()

	var callCount int
	var receivedUserPrompt string
	var receivedTools bool
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode LLM request: %v", err)
		}
		_, receivedTools = raw["tools"]
		var messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(raw["messages"], &messages); err != nil {
			t.Errorf("decode LLM messages: %v", err)
		}
		for _, message := range messages {
			if message.Role == "user" {
				receivedUserPrompt = message.Content
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": "默认端口是 8080。[1]"}}},
			"usage":   map[string]any{"prompt_tokens": 120, "completion_tokens": 18},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	t.Cleanup(llmServer.Close)
	if err := scope.SaveSetting(ctx, server.dataStore, regular.ID, "", "agents.defaults", map[string]any{
		"model": "test/qa-model",
	}); err != nil {
		t.Fatal(err)
	}
	if err := scope.SaveProvider(ctx, server.dataStore, regular.ID, "", "test", config.ProviderConfig{
		APIKey: "test-key", APIBase: llmServer.URL, APIType: "openai-chat",
	}); err != nil {
		t.Fatal(err)
	}

	kb, err := service.CreateKB(ctx, regular.ID, "部署手册", "产品部署与端口说明", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	content := "# 默认端口\n\n服务默认监听 8080 端口。"
	document, err := service.UploadDocument(ctx, regular.ID, kb.ID, "deploy.md", strings.NewReader(content), int64(len([]byte(content))))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		indexed, getErr := service.GetDocument(ctx, regular.ID, kb.ID, document.ID)
		if getErr == nil && indexed.Status == "DONE" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	history := make([]string, 22)
	sessionID := "kbc_test_history"
	historyStart := time.Now().UTC().Add(-time.Hour)
	for index := range history {
		history[index] = fmt.Sprintf("history-%02d", index)
		if err := server.dataStore.AppendRAGChatTurn(ctx, &store.RAGChatTurnRecord{
			ID: fmt.Sprintf("history-turn-%02d", index), UserID: regular.ID, KBID: kb.ID,
			SessionID: sessionID, Title: "历史问题", Question: history[index],
			Answer: "这条历史回答不应进入下一轮上下文", Sources: json.RawMessage("[]"),
			CreatedAt: historyStart.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := json.Marshal(map[string]any{
		"question":  "它默认使用哪个端口？",
		"sessionId": sessionID,
	})
	request := ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/chat", regular.ID, string(body))
	response := callRAGHandler(t, server, server.handleRAGChat, request, map[string]string{"id": kb.ID})
	if response.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		ID        string    `json:"id"`
		SessionID string    `json:"sessionId"`
		Answer    string    `json:"answer"`
		Hits      []rag.Hit `json:"hits"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("answer LLM calls = %d, want 1", callCount)
	}
	if receivedTools {
		t.Fatal("knowledge-base answer request unexpectedly included tools")
	}
	if strings.Contains(receivedUserPrompt, "history-00") || strings.Contains(receivedUserPrompt, "history-01") {
		t.Fatalf("prompt retained history older than 20 questions: %q", receivedUserPrompt)
	}
	if !strings.Contains(receivedUserPrompt, "history-02") || !strings.Contains(receivedUserPrompt, "history-21") {
		t.Fatalf("prompt missing recent question history: %q", receivedUserPrompt)
	}
	if strings.Contains(receivedUserPrompt, "这条历史回答不应进入下一轮上下文") {
		t.Fatalf("prompt unexpectedly included historical answers: %q", receivedUserPrompt)
	}
	if !strings.Contains(receivedUserPrompt, "它默认使用哪个端口？") ||
		!strings.Contains(receivedUserPrompt, "deploy.md") ||
		!strings.Contains(receivedUserPrompt, "默认监听 8080") {
		t.Fatalf("prompt missing current question or retrieved source: %q", receivedUserPrompt)
	}
	if result.ID == "" || result.SessionID != sessionID || result.Answer != "默认端口是 8080。[1]" || len(result.Hits) == 0 || result.Hits[0].DocName != "deploy.md" {
		t.Fatalf("chat response = %+v", result)
	}
	persisted, err := server.dataStore.ListRAGChatTurns(ctx, regular.ID, kb.ID, sessionID)
	if err != nil || len(persisted) != 23 || persisted[len(persisted)-1].Answer != result.Answer {
		t.Fatalf("persisted turns = %d, last=%+v, err=%v", len(persisted), persisted[len(persisted)-1], err)
	}

	sessionsResponse := callRAGHandler(t, server, server.handleListRAGChatSessions,
		authTestRequest(t, ctx, resolver, http.MethodGet, "/api/rag/kbs/"+kb.ID+"/chat/sessions", regular.ID),
		map[string]string{"id": kb.ID})
	if sessionsResponse.Code != http.StatusOK || !strings.Contains(sessionsResponse.Body.String(), sessionID) {
		t.Fatalf("list chat sessions status=%d body=%s", sessionsResponse.Code, sessionsResponse.Body.String())
	}
	turnsResponse := callRAGHandler(t, server, server.handleListRAGChatTurns,
		authTestRequest(t, ctx, resolver, http.MethodGet, "/api/rag/kbs/"+kb.ID+"/chat/sessions/"+sessionID, regular.ID),
		map[string]string{"id": kb.ID, "sessionId": sessionID})
	if turnsResponse.Code != http.StatusOK || !strings.Contains(turnsResponse.Body.String(), result.Answer) || !strings.Contains(turnsResponse.Body.String(), "deploy.md") {
		t.Fatalf("list chat turns status=%d body=%s", turnsResponse.Code, turnsResponse.Body.String())
	}
}

func TestNormalizeRAGChatHistoryUsesRecentQuestionAndRuneBudgets(t *testing.T) {
	history := make([]string, 25)
	for index := range history {
		history[index] = fmt.Sprintf("question-%02d", index)
	}
	got := normalizeRAGChatHistory(history)
	if len(got) != ragChatMaxHistoryQuestions || got[0] != "question-05" || got[len(got)-1] != "question-24" {
		t.Fatalf("normalized count window = %#v", got)
	}

	longHistory := []string{strings.Repeat("旧", ragChatMaxHistoryRunes), strings.Repeat("新", 100)}
	got = normalizeRAGChatHistory(longHistory)
	var runes int
	for _, question := range got {
		runes += utf8.RuneCountInString(question)
	}
	if runes > ragChatMaxHistoryRunes || got[len(got)-1] != strings.Repeat("新", 100) {
		t.Fatalf("normalized rune window = %d, history lengths=%v", runes, []int{utf8.RuneCountInString(got[0]), utf8.RuneCountInString(got[len(got)-1])})
	}
}

func TestGenerateRAGKBMetadataRequiresDoneDocument(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	kb, err := service.CreateKB(context.Background(), regular.ID, "empty", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := authTestRequest(t, context.Background(), resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/generate-metadata", regular.ID)
	response := callRAGHandler(t, server, server.handleGenerateRAGKBMetadata, request, map[string]string{"id": kb.ID})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), rag.ErrNoReadyDocuments.Error()) {
		t.Fatalf("generate without documents status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestParseGeneratedKBMetadataAcceptsFenceAndBoundsOutput(t *testing.T) {
	longName := strings.Repeat("名", 40)
	longDescription := strings.Repeat("描述", 180)
	content, _ := json.Marshal(map[string]string{"name": "《" + longName + "》", "description": longDescription})
	name, description, err := parseGeneratedKBMetadata("```json\n" + string(content) + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(name)); got != 30 {
		t.Fatalf("name rune length = %d, want 30", got)
	}
	if got := len([]rune(description)); got != 300 {
		t.Fatalf("description rune length = %d, want 300", got)
	}
}

func TestParseGeneratedKBMetadataAcceptsCommonModelOutput(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantName    string
		wantKeyword string
	}{
		{
			name:        "reasoning before final object",
			content:     `<think>可以使用 {"name":"示例","description":"示例"} 格式。</think>\n{"name":"部署运维指南","description":"包含安装、配置与故障排查说明，主要用于部署和运维支持。"}`,
			wantName:    "部署运维指南",
			wantKeyword: "故障排查",
		},
		{
			name:        "trailing comma",
			content:     `{"name":"接口参考","description":"包含接口参数与示例，主要用于开发集成。",}`,
			wantName:    "接口参考",
			wantKeyword: "开发集成",
		},
		{
			name:        "nested Chinese keys",
			content:     `{"result":{"知识库名称":"员工制度","知识库描述":"包含考勤和休假制度，主要用于员工查询公司规定。"}}`,
			wantName:    "员工制度",
			wantKeyword: "公司规定",
		},
		{
			name:        "labeled text",
			content:     "**名称**：产品手册\n**描述**：包含产品功能和操作步骤，主要用于用户使用指导。",
			wantName:    "产品手册",
			wantKeyword: "使用指导",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, description, err := parseGeneratedKBMetadata(test.content)
			if err != nil {
				t.Fatal(err)
			}
			if name != test.wantName || !strings.Contains(description, test.wantKeyword) {
				t.Fatalf("generated metadata = (%q, %q)", name, description)
			}
		})
	}
}

func TestParseGeneratedKBMetadataRejectsMissingFields(t *testing.T) {
	for _, content := range []string{"", `{"name":"只有名称"}`, "名称：只有名称"} {
		if _, _, err := parseGeneratedKBMetadata(content); err == nil {
			t.Fatalf("parseGeneratedKBMetadata(%q) unexpectedly succeeded", content)
		}
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
