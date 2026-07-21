package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

type setupRAGUserCleanerFunc func(context.Context, string) error

func (fn setupRAGUserCleanerFunc) CleanupRAGUser(ctx context.Context, userID string) error {
	return fn(ctx, userID)
}

func newAdminRAGDeleteStore(t *testing.T) *store.DBStore {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "admin-rag-delete.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("migrate sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedAdminRAGDeleteUser(t *testing.T, st store.Store, userID, kbID string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateUser(ctx, &store.UserRecord{
		ID:           userID,
		Username:     userID,
		Email:        userID + "@example.test",
		PasswordHash: "test-only",
		Role:         users.RoleUser,
		Status:       users.StatusActive,
		AgentQuota:   -1,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.CreateRAGKB(ctx, &store.RAGKBRecord{
		ID:            kbID,
		UserID:        userID,
		Name:          "test kb",
		EmbedProvider: "system",
		EmbedModel:    "test-embedding",
		EmbedDims:     4,
		ChunkSize:     512,
		ChunkOverlap:  64,
		ParseMode:     store.RAGParseModeStandard,
		Status:        "ACTIVE",
	}); err != nil {
		t.Fatalf("create RAG KB: %v", err)
	}
}

func TestServerWiresRAGUserCleanerForEitherSetterOrder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ragFirst bool
	}{
		{name: "store then RAG service"},
		{name: "RAG service then store", ragFirst: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := newAdminRAGDeleteStore(t)
			userID := "u_order"
			kbID := "kb_order"
			seedAdminRAGDeleteUser(t, st, userID, kbID)

			vectors := vector.NewFake()
			if err := vectors.EnsureCollection(ctx, kbID, 4); err != nil {
				t.Fatalf("ensure fake vector collection: %v", err)
			}
			service := rag.New(rag.Deps{
				Store:   st,
				Vector:  vectors,
				Objects: objects.NewLocalFS(filepath.Join(t.TempDir(), "objects")),
			})
			server := NewServer(0)
			if tc.ragFirst {
				server.SetRAGService(service)
				server.SetStore(st)
			} else {
				server.SetStore(st)
				server.SetRAGService(service)
			}

			if server.ragUserCleaner == nil {
				t.Fatal("RAG user cleaner was not wired")
			}
			if err := server.accounts.Delete(ctx, userID); err != nil {
				t.Fatalf("delete RAG user: %v", err)
			}
			if _, err := st.GetUser(ctx, userID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("user should be hard-deleted after RAG cleanup, got %v", err)
			}
			if _, err := st.GetRAGKB(ctx, kbID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("RAG KB should be cleaned before user deletion, got %v", err)
			}
		})
	}
}

func TestHandleDeleteUserReportsMissingRAGCleanerAsUnavailable(t *testing.T) {
	ctx := context.Background()
	st := newAdminRAGDeleteStore(t)
	seedAdminRAGDeleteUser(t, st, "u_missing_cleaner", "kb_missing_cleaner")
	server := NewServer(0)
	server.SetStore(st)

	req := httptest.NewRequest(http.MethodDelete, "/api/users/u_missing_cleaner", nil)
	req.SetPathValue("id", "u_missing_cleaner")
	recorder := httptest.NewRecorder()
	server.handleDeleteUser(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	user, err := st.GetUser(ctx, "u_missing_cleaner")
	if err != nil {
		t.Fatalf("user should remain for retry: %v", err)
	}
	if user.Status != users.StatusDeleting {
		t.Fatalf("user status = %q, want %q", user.Status, users.StatusDeleting)
	}
}

func TestHandleDeleteUserDoesNotLeakRAGCleanupBackendDetails(t *testing.T) {
	ctx := context.Background()
	st := newAdminRAGDeleteStore(t)
	seedAdminRAGDeleteUser(t, st, "u_cleanup_failure", "kb_cleanup_failure")
	server := NewServer(0)
	server.SetStore(st)
	server.setRAGUserCleaner(setupRAGUserCleanerFunc(func(context.Context, string) error {
		return errors.New("object delete failed at rag/u_cleanup_failure/kb_cleanup_failure/ secret-endpoint")
	}))

	req := httptest.NewRequest(http.MethodDelete, "/api/users/u_cleanup_failure", nil)
	req.SetPathValue("id", "u_cleanup_failure")
	recorder := httptest.NewRecorder()
	server.handleDeleteUser(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "rag/u_cleanup_failure") || strings.Contains(recorder.Body.String(), "secret-endpoint") {
		t.Fatalf("response leaked backend cleanup detail: %s", recorder.Body.String())
	}
	user, err := st.GetUser(ctx, "u_cleanup_failure")
	if err != nil || user.Status != users.StatusDeleting {
		t.Fatalf("failed cleanup must preserve deleting user, user=%+v err=%v", user, err)
	}
}

func TestWriteRAGErrorReportsLifecycleCleanupAsRetryableWithoutBackendDetail(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRAGError(recorder, rag.ErrLifecycleCleanupPending)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// Production lifecycle errors expose the sentinel text only; the handler's
	// mapping must never require inspecting or reflecting an object key.
	if !strings.Contains(recorder.Body.String(), rag.ErrLifecycleCleanupPending.Error()) {
		t.Fatalf("response did not identify retryable cleanup: %s", recorder.Body.String())
	}
}
