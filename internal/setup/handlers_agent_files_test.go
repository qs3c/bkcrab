package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestAgentFileUploadScopesNewSessionBeforeDatabaseRowExists(t *testing.T) {
	s, resolver, owner, ws := newAgentFileUploadTestServer(t)

	rr := uploadAgentFileForTest(t, s, resolver, owner.ID,
		"/api/agents/agt_upload/files?sessionId=s-new-chat", "meeting-recording.wav", []byte("audio"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	assertWorkspaceFileForTest(t, ws, "", "s-new-chat", "meeting-recording.wav", "audio")
	if rc, err := ws.Get(context.Background(), "agt_upload", "", "", "meeting-recording.wav"); err == nil {
		rc.Close()
		t.Fatal("attachment was also written to the agent root")
	} else if !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("check agent root: %v", err)
	}
}

func TestAgentFileUploadScopesNewProjectSessionFromHint(t *testing.T) {
	s, resolver, owner, ws := newAgentFileUploadTestServer(t)
	ctx := context.Background()
	if err := s.dataStore.SaveProject(ctx, &store.ProjectRecord{
		UserID: owner.ID, AgentID: "agt_upload", ID: "proj_upload", Name: "Upload project",
	}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	rr := uploadAgentFileForTest(t, s, resolver, owner.ID,
		"/api/agents/agt_upload/files?sessionId=s-new-project-chat&projectId=proj_upload",
		"brief.pdf", []byte("pdf"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	assertWorkspaceFileForTest(t, ws, "proj_upload", "s-new-project-chat", "brief.pdf", "pdf")
}

func TestAgentFileUploadUsesCanonicalScopeForExistingProjectSession(t *testing.T) {
	s, resolver, owner, ws := newAgentFileUploadTestServer(t)
	ctx := context.Background()
	if err := s.dataStore.SaveProject(ctx, &store.ProjectRecord{
		UserID: owner.ID, AgentID: "agt_upload", ID: "proj_existing", Name: "Existing project",
	}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	if err := s.dataStore.SaveSession(ctx, owner.ID, "agt_upload", "s-existing-key", &store.SessionRecord{
		Channel: "web", ChatID: "chat-canonical", ProjectID: "proj_existing",
	}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	rr := uploadAgentFileForTest(t, s, resolver, owner.ID,
		"/api/agents/agt_upload/files?sessionId=s-existing-key&projectId=untrusted-hint",
		"notes.txt", []byte("notes"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	assertWorkspaceFileForTest(t, ws, "proj_existing", "chat-canonical", "notes.txt", "notes")
}

func TestAgentFileUploadRejectsUnsafeUnresolvedSessionID(t *testing.T) {
	s, resolver, owner, _ := newAgentFileUploadTestServer(t)

	rr := uploadAgentFileForTest(t, s, resolver, owner.ID,
		"/api/agents/agt_upload/files?sessionId=..%2Fescape", "notes.txt", []byte("notes"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func newAgentFileUploadTestServer(t *testing.T) (*Server, *auth.Resolver, *users.Account, *workspace.LocalFS) {
	t.Helper()
	ctx := context.Background()
	s, resolver, _, owner := newAuthTestServer(t, ctx)
	if err := s.dataStore.SaveAgent(ctx, &store.AgentRecord{
		ID: "agt_upload", UserID: owner.ID, Name: "Upload Agent",
	}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}
	ws := workspace.NewLocalFS(t.TempDir())
	s.SetWorkspaceStore(ws)
	return s, resolver, owner, ws
}

func uploadAgentFileForTest(
	t *testing.T,
	s *Server,
	resolver *auth.Resolver,
	userID, target, fileName string,
	content []byte,
) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write multipart: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}

	req := authTestRequest(t, context.Background(), resolver, http.MethodPost, target, userID)
	req.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
	req.ContentLength = int64(body.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.SetPathValue("id", "agt_upload")
	rr := httptest.NewRecorder()
	s.authMiddleware(s.handleAgentFileUpload)(rr, req)
	return rr
}

func assertWorkspaceFileForTest(
	t *testing.T,
	ws workspace.Store,
	projectID, sessionID, fileName, want string,
) {
	t.Helper()
	rc, err := ws.Get(context.Background(), "agt_upload", projectID, sessionID, fileName)
	if err != nil {
		t.Fatalf("workspace Get(project=%q session=%q): %v", projectID, sessionID, err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read workspace file: %v", err)
	}
	if string(got) != want {
		t.Fatalf("workspace file=%q want=%q", got, want)
	}
}
