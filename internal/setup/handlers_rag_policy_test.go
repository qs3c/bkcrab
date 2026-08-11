package setup

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestRAGPolicyPromotionUsesStrictSessionIdentityGate(t *testing.T) {
	tests := []struct {
		name     string
		identity auth.Identity
		allowed  bool
	}{{"super admin session", auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session"}, true}, {"ordinary user", auth.Identity{UserID: "user", Role: users.RoleUser, AuthMethod: "session"}, false}, {"admin key", auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "apikey", APIKeyType: users.APIKeyTypeAdmin}, false}, {"actAs", auth.Identity{UserID: "admin", Role: users.RoleSuperAdmin, AuthMethod: "session", ActAsUserID: "user"}, false}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isRAGEvalAdminIdentity(test.identity); got != test.allowed {
				t.Fatalf("allowed=%v want %v", got, test.allowed)
			}
		})
	}
}

func TestRAGPolicyRoutesRemainUnavailableWithoutPromotionService(t *testing.T) {
	server := NewServer(0)
	mux := http.NewServeMux()
	server.registerRAGPolicyRoutes(mux, func(next http.HandlerFunc) http.HandlerFunc { return next })
	request := httptest.NewRequest(http.MethodPost, "/api/admin/rag-policies/runtime/promotions", strings.NewReader(`{"runId":"run","profileId":"profile","confirmationRunId":"confirmation","fields":["minScore"],"policy":{"minScore":0}}`))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
