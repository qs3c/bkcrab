package setup

import (
	"encoding/json"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestRAGEvalAdminIdentityMatrix(t *testing.T) {
	tests := []struct {
		name     string
		identity auth.Identity
		want     bool
	}{
		{name: "super admin session", identity: auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session"}, want: true},
		{name: "anonymous", identity: auth.Identity{}},
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

func TestMaskedDatasetVersionOmitsObjectKeyAndValidationReport(t *testing.T) {
	encoded, err := json.Marshal(maskEvalDatasetVersions([]store.RAGEvalDatasetVersionRecord{{ID: "v1", ManifestObjectKey: "secret/object/key", ValidationReportJSON: `{"private":true}`}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || json.Valid(encoded) == false {
		t.Fatal("masked DTO must be valid JSON")
	}
	var decoded []map[string]any
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded[0]["ManifestObjectKey"]; ok {
		t.Fatal("object key leaked")
	}
	if _, ok := decoded[0]["ValidationReportJSON"]; ok {
		t.Fatal("validation report leaked")
	}
}
