package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestAuthorizeSkillInstallTargetRequiresAdminForGlobalInstalls(t *testing.T) {
	s := NewServer(0)

	tests := []struct {
		name       string
		ident      auth.Identity
		wantOK     bool
		wantStatus int
	}{
		{
			name: "regular user session rejected",
			ident: auth.Identity{
				UserID:     "u_user",
				Role:       users.RoleUser,
				AuthMethod: "session",
			},
			wantOK:     false,
			wantStatus: http.StatusForbidden,
		},
		{
			name: "super admin session allowed",
			ident: auth.Identity{
				UserID:     "u_admin",
				Role:       users.RoleSuperAdmin,
				AuthMethod: "session",
			},
			wantOK: true,
		},
		{
			name: "admin api key allowed",
			ident: auth.Identity{
				UserID:     "u_user",
				Role:       users.RoleUser,
				AuthMethod: "apikey",
				APIKeyType: users.APIKeyTypeAdmin,
			},
			wantOK: true,
		},
		{
			name: "actAs super admin rejected as read only",
			ident: auth.Identity{
				UserID:      "u_admin",
				Role:        users.RoleSuperAdmin,
				AuthMethod:  "session",
				ActAsUserID: "u_other",
			},
			wantOK:     false,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			ok := s.authorizeSkillInstallTarget(rr, skillInstallRequest(tt.ident), "")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK && rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
		})
	}
}

func TestAuthorizeSkillInstallTargetKeepsAgentInstallsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	s, st, accts := newSkillInstallAuthServer(t, ctx)
	owner := createSkillInstallTestUser(t, ctx, accts, "owner", users.RoleUser)
	other := createSkillInstallTestUser(t, ctx, accts, "other", users.RoleUser)
	if err := st.SaveAgent(ctx, &store.AgentRecord{
		ID:     "agt_owner",
		UserID: owner.ID,
		Name:   "Owner Agent",
	}); err != nil {
		t.Fatalf("SaveAgent: %v", err)
	}

	tests := []struct {
		name       string
		ident      auth.Identity
		wantOK     bool
		wantStatus int
	}{
		{
			name: "owner allowed",
			ident: auth.Identity{
				UserID:     owner.ID,
				Role:       users.RoleUser,
				AuthMethod: "session",
			},
			wantOK: true,
		},
		{
			name: "non owner rejected",
			ident: auth.Identity{
				UserID:     other.ID,
				Role:       users.RoleUser,
				AuthMethod: "session",
			},
			wantOK:     false,
			wantStatus: http.StatusForbidden,
		},
		{
			name: "read only owner rejected",
			ident: auth.Identity{
				UserID:      "u_admin",
				Role:        users.RoleSuperAdmin,
				AuthMethod:  "session",
				ActAsUserID: owner.ID,
			},
			wantOK:     false,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			ok := s.authorizeSkillInstallTarget(rr, skillInstallRequest(tt.ident), "agt_owner")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK && rr.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
		})
	}
}

func skillInstallRequest(ident auth.Identity) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/skills/install", nil)
	return req.WithContext(auth.WithIdentity(req.Context(), ident))
}

func newSkillInstallAuthServer(t *testing.T, ctx context.Context) (*Server, store.Store, *users.Accounts) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "bkcrab.db")
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
	s := NewServer(0)
	s.SetStore(st)
	return s, st, accts
}

func createSkillInstallTestUser(t *testing.T, ctx context.Context, accts *users.Accounts, username, role string) *users.Account {
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
