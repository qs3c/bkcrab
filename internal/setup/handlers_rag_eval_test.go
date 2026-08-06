package setup

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestRAGEvalAdminIdentityMatrix(t *testing.T) {
	tests := []struct {
		name     string
		identity auth.Identity
		want     bool
	}{
		{name: "super admin session", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session"}, want: true},
		{name: "regular user session", identity: auth.Identity{UserID: "user", Role: users.RoleUser, AuthMethod: "session"}},
		{name: "admin API key", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "apikey", APIKeyType: users.APIKeyTypeAdmin}},
		{name: "act-as session", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session", ActAsUserID: "user"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRAGEvalAdminIdentity(tt.identity); got != tt.want {
				t.Fatalf("isRAGEvalAdminIdentity() = %v, want %v", got, tt.want)
			}
		})
	}
}
