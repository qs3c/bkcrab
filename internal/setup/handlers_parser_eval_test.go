package setup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/parseeval"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

type staticParserEvalCapabilities struct {
	value parseeval.Capabilities
	err   error
}

func (p staticParserEvalCapabilities) ParserEvaluationCapabilities(context.Context, string) (parseeval.Capabilities, error) {
	return p.value, p.err
}

func TestParserEvaluationRoutesUseStrictSessionGate(t *testing.T) {
	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/admin/parser-evals/capabilities"},
		{http.MethodGet, "/api/admin/parser-evals/runs"},
		{http.MethodPost, "/api/admin/parser-evals/runs"},
		{http.MethodGet, "/api/admin/parser-evals/runs/per_run"},
		{http.MethodDelete, "/api/admin/parser-evals/runs/per_run"},
		{http.MethodPost, "/api/admin/parser-evals/runs/per_run/documents"},
		{http.MethodDelete, "/api/admin/parser-evals/runs/per_run/documents/ped_doc"},
		{http.MethodPost, "/api/admin/parser-evals/runs/per_run/start"},
		{http.MethodPost, "/api/admin/parser-evals/runs/per_run/cancel"},
		{http.MethodPost, "/api/admin/parser-evals/runs/per_run/retry"},
		{http.MethodGet, "/api/admin/parser-evals/runs/per_run/documents/ped_doc/artifacts/source"},
	}
	identities := []struct {
		name    string
		value   auth.Identity
		allowed bool
	}{
		{"super admin session", auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session"}, true},
		{"ordinary session", auth.Identity{UserID: "user", Role: users.RoleUser, AuthMethod: "session"}, false},
		{"admin API key", auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "apikey", APIKeyType: users.APIKeyTypeAdmin}, false},
		{"act as", auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session", ActAsUserID: "user"}, false},
		{"anonymous", auth.Identity{}, false},
	}

	for _, route := range routes {
		for _, identity := range identities {
			name := identity.name + " " + route.method + " " + route.path
			t.Run(name, func(t *testing.T) {
				server := &Server{}
				mux := http.NewServeMux()
				gate := func(next http.HandlerFunc) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						value, ok := auth.FromContext(r.Context())
						if !ok || !isRAGEvalAdminIdentity(value) {
							w.WriteHeader(http.StatusForbidden)
							return
						}
						next(w, r)
					}
				}
				server.registerParserEvaluationRoutes(mux, gate)
				request := httptest.NewRequest(route.method, route.path, nil)
				if identity.value.UserID != "" {
					request = request.WithContext(auth.WithIdentity(request.Context(), identity.value))
				}
				recorder := httptest.NewRecorder()
				mux.ServeHTTP(recorder, request)
				if identity.allowed && recorder.Code == http.StatusForbidden {
					t.Fatalf("super-admin session was rejected: %s", recorder.Body.String())
				}
				if !identity.allowed && recorder.Code != http.StatusForbidden {
					t.Fatalf("identity passed strict gate with status %d", recorder.Code)
				}
			})
		}
	}
}

func TestParserEvaluationAPIWorkflowAndArtifactBoundary(t *testing.T) {
	server, mux := newParserEvalAPITestServer(t)
	identity := auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session"}

	create := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs", strings.NewReader(`{"judgeModelBindingId":"vision-binding"}`), "create-key-01", "application/json")
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	runID := responseDataString(t, create.Body.Bytes(), "id")

	unknown := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs", strings.NewReader(`{"judgeModelBindingId":"vision-binding","unknown":true}`), "create-key-02", "application/json")
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("closed JSON status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	documentBytes := minimalParserEvalDOCX(t)
	uploadBody, contentType := parserEvalMultipart(t, "sample.docx", parseeval.MediaTypeDOCX, documentBytes)
	upload := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs/"+runID+"/documents", uploadBody, "upload-key-01", contentType)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", upload.Code, upload.Body.String())
	}
	documentID := responseDataString(t, upload.Body.Bytes(), "id")
	if strings.Contains(upload.Body.String(), "objectKey") || strings.Contains(upload.Body.String(), "sourceObjectKey") {
		t.Fatalf("upload response leaked an object key: %s", upload.Body.String())
	}

	artifactPath := "/api/admin/parser-evals/runs/" + runID + "/documents/" + documentID + "/artifacts/source"
	artifact := parserEvalAPIRequest(t, mux, identity, http.MethodGet, artifactPath, nil, "", "")
	if artifact.Code != http.StatusOK || !bytes.Equal(artifact.Body.Bytes(), documentBytes) {
		t.Fatalf("source artifact status=%d bytes=%d", artifact.Code, artifact.Body.Len())
	}
	if artifact.Header().Get("Content-Type") != parseeval.MediaTypeDOCX || artifact.Header().Get("X-Content-Type-Options") != "nosniff" || artifact.Header().Get("Cache-Control") != "no-store, private" {
		t.Fatalf("unsafe artifact headers: %+v", artifact.Header())
	}
	badArtifact := parserEvalAPIRequest(t, mux, identity, http.MethodGet, artifactPath+"?objectKey=secret", nil, "", "")
	if badArtifact.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary artifact query accepted: %d %s", badArtifact.Code, badArtifact.Body.String())
	}

	start := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs/"+runID+"/start", nil, "start-key-01", "")
	if start.Code != http.StatusOK || responseDataString(t, start.Body.Bytes(), "status") != store.ParserEvalRunQueued {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
	}
	startAgain := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs/"+runID+"/start", nil, "start-key-01", "")
	if startAgain.Code != http.StatusOK || responseDataString(t, startAgain.Body.Bytes(), "status") != store.ParserEvalRunQueued {
		t.Fatalf("idempotent start status=%d body=%s", startAgain.Code, startAgain.Body.String())
	}
	detail := parserEvalAPIRequest(t, mux, identity, http.MethodGet, "/api/admin/parser-evals/runs/"+runID, nil, "", "")
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), "objectKey") || strings.Contains(detail.Body.String(), "sourceObjectKey") {
		t.Fatalf("detail status=%d or leaked key: %s", detail.Code, detail.Body.String())
	}

	// A draft is directly deletable; an active run must first become terminal.
	draft := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs", strings.NewReader(`{"judgeModelBindingId":"vision-binding"}`), "create-key-03", "application/json")
	draftID := responseDataString(t, draft.Body.Bytes(), "id")
	deleted := parserEvalAPIRequest(t, mux, identity, http.MethodDelete, "/api/admin/parser-evals/runs/"+draftID, nil, "", "")
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete draft status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	activeDelete := parserEvalAPIRequest(t, mux, identity, http.MethodDelete, "/api/admin/parser-evals/runs/"+runID, nil, "", "")
	if activeDelete.Code != http.StatusConflict {
		t.Fatalf("active delete status=%d body=%s", activeDelete.Code, activeDelete.Body.String())
	}
	cancel := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs/"+runID+"/cancel", nil, "cancel-key-01", "")
	cancelAgain := parserEvalAPIRequest(t, mux, identity, http.MethodPost, "/api/admin/parser-evals/runs/"+runID+"/cancel", nil, "cancel-key-01", "")
	if cancel.Code != http.StatusOK || cancelAgain.Code != http.StatusOK {
		t.Fatalf("cancel idempotency status=%d/%d bodies=%s / %s", cancel.Code, cancelAgain.Code, cancel.Body.String(), cancelAgain.Body.String())
	}

	_ = server
}

func TestParserEvalPublicJSONRecursivelyRemovesObjectKeys(t *testing.T) {
	value := decodePublicParserEvalJSON(`{"artifact":{"objectKey":"secret","sha256":"ok"},"items":[{"objectKey":"nested"}]}`)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "objectKey") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "nested") {
		t.Fatalf("object key was not redacted: %s", encoded)
	}
}

func TestParserEvalArtifactRequestIsClosed(t *testing.T) {
	tests := []struct {
		path string
		ok   bool
	}{
		{"/runs/r/documents/d/artifacts/source", true},
		{"/runs/r/documents/d/artifacts/source?page=1", false},
		{"/runs/r/documents/d/artifacts/truth-page?page=1", true},
		{"/runs/r/documents/d/artifacts/truth-page?page=7", false},
		{"/runs/r/documents/d/artifacts/truth-page?page=1&page=2", false},
		{"/runs/r/documents/d/artifacts/markdown?engine=markitdown", true},
		{"/runs/r/documents/d/artifacts/markdown?engine=other", false},
		{"/runs/r/documents/d/artifacts/judge-raw?order=anydoc-a", true},
		{"/runs/r/documents/d/artifacts/judge-raw?order=anydoc-a&extra=x", false},
		{"/runs/r/documents/d/artifacts/other", false},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.SetPathValue("kind", strings.Split(strings.Split(test.path, "?")[0], "/")[6])
			_, err := parserEvalArtifactRequest(request, 6)
			if (err == nil) != test.ok {
				t.Fatalf("err=%v want ok=%v", err, test.ok)
			}
		})
	}
}

func newParserEvalAPITestServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "parser-eval-api.db")) + "?_pragma=busy_timeout(5000)"
	database, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultParserEvaluationCfg()
	cfg.Enabled = true
	objectStore := objects.NewLocalFS(t.TempDir())
	service, err := parseeval.NewService(database, objectStore, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := parseeval.NewCleanup(database, objectStore)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{parserEvalService: service, parserEvalCleanup: cleanup, parserEvalCaps: staticParserEvalCapabilities{value: testParserEvalCapabilities(cfg)}}
	mux := http.NewServeMux()
	gate := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			identity, ok := auth.FromContext(r.Context())
			if !ok || !isRAGEvalAdminIdentity(identity) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next(w, r)
		}
	}
	server.registerParserEvaluationRoutes(mux, gate)
	return server, mux
}

func testParserEvalCapabilities(cfg config.ParserEvaluationCfg) parseeval.Capabilities {
	health := parseeval.CachedDependencyHealth{Healthy: true}
	return parseeval.Capabilities{
		Enabled: true, Available: true, WorkerEnabled: true,
		SupportedFormats: []parseeval.Format{parseeval.FormatDOCX, parseeval.FormatPPTX, parseeval.FormatXLSX},
		MaxFiles:         cfg.MaxFiles, MaxFileBytes: cfg.MaxFileBytes, MaxBatchBytes: cfg.MaxBatchBytes,
		MaxPages: cfg.MaxPages, RenderDPI: cfg.RenderDPI, MarkdownJudgeChars: cfg.MarkdownJudgeChars,
		Renderer: parseeval.RendererCapability{Health: health, Descriptor: parseeval.RendererDescriptor{
			ProtocolVersion: "parser-eval-renderer/v1", ServiceVersion: "test", LibreOfficeVersion: "test", PyMuPDFVersion: "test",
		}},
		Parsers: []parseeval.ParserCapability{
			{Engine: parseeval.EngineMarkItDown, Health: health, Descriptor: parseeval.ParserDescriptor{Name: "markitdown", Version: "1", WrapperVersion: "1"}},
			{Engine: parseeval.EngineAnyDoc, Health: health, Descriptor: parseeval.ParserDescriptor{Name: "anydoc", Version: "1", WrapperVersion: "1"}},
		},
		JudgeModelBindings: []parseeval.JudgeBindingSnapshot{{
			ID: "vision-binding", Provider: "test", Model: "vision", Fingerprint: strings.Repeat("a", 64), ModelDisplayName: "Vision Judge",
		}},
	}
}

func parserEvalAPIRequest(t *testing.T, mux *http.ServeMux, identity auth.Identity, method, path string, body io.Reader, key, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, body)
	request = request.WithContext(auth.WithIdentity(request.Context(), identity))
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func parserEvalMultipart(t *testing.T, fileName, mediaType string, content []byte) (*bytes.Reader, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, fileName))
	header.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buffer.Bytes()), writer.FormDataContentType()
}

func minimalParserEvalDOCX(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`,
		"word/document.xml":   `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"/>`,
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func responseDataString(t *testing.T, body []byte, key string) string {
	t.Helper()
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	value, ok := response.Data[key].(string)
	if !ok || value == "" {
		t.Fatalf("response data %q is missing: %s", key, body)
	}
	return value
}
