package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/users"
)

func TestListSkillsRequiresAuth(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, regularUser := newAuthTestServer(t, ctx)
	t.Setenv("BKCLAW_HOME", t.TempDir())

	skills := &SkillsHandler{
		workspaceStore: s.workspaceStore,
		guard:          &agentGuard{dataStore: s.dataStore, userResolver: s.userResolver},
	}
	handler := s.authMiddleware(skills.handleListSkills)

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, "/api/skills", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
		}
	})

	t.Run("regular user is allowed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, authTestRequest(t, ctx, resolver, http.MethodGet, "/api/skills", regularUser.ID))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
			t.Fatalf("body = %q, want []", got)
		}
	})

	t.Run("super admin is allowed", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler(rr, authTestRequest(t, ctx, resolver, http.MethodGet, "/api/skills", adminUser.ID))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
		}
		if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
			t.Fatalf("body = %q, want []", got)
		}
	})
}

func newAuthTestServer(t *testing.T, ctx context.Context) (*Server, *auth.Resolver, *users.Account, *users.Account) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "bkclaw.db")
	st, err := store.NewDBStore("sqlite", "file:"+dbPath+"?cache=shared")
	if err != nil {
		t.Fatalf("NewDBStore: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	accts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatalf("NewAccounts: %v", err)
	}
	adminUser := createAuthTestUser(t, ctx, accts, "admin", users.RoleSuperAdmin)
	regularUser := createAuthTestUser(t, ctx, accts, "user", users.RoleUser)
	resolver, err := auth.NewResolver(st)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	s := NewServer(0)
	s.SetStore(st)
	s.SetAuth(resolver)
	return s, resolver, adminUser, regularUser
}

func createAuthTestUser(t *testing.T, ctx context.Context, accts *users.Accounts, username, role string) *users.Account {
	t.Helper()

	acct, err := accts.Create(ctx, users.CreateInput{
		Username: username,
		Email:    username + "@example.test",
		Password: "password",
		Role:     role,
	})
	if err != nil {
		t.Fatalf("Create(%s): %v", username, err)
	}
	return acct
}

func authTestRequest(t *testing.T, ctx context.Context, resolver *auth.Resolver, method, path, userID string) *http.Request {
	t.Helper()

	cookie, err := resolver.IssueSession(ctx, userID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(cookie)
	return req
}
