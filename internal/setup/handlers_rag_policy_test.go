package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/store"
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

func TestRAGPolicySyncResponseMasksInternalFailureDetails(t *testing.T) {
	response := ragPolicySyncResponse(&store.RAGPolicySyncTaskRecord{ID: "sync-1", Status: store.RAGPolicySyncFailed, ErrorCode: "build_failed", ErrorMessage: "s3://secret-bucket/object endpoint=https://internal"})
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "secret-bucket") || strings.Contains(text, "internal") {
		t.Fatalf("internal failure detail leaked: %s", text)
	}
	if !strings.Contains(text, "旧索引仍正常") {
		t.Fatalf("safe failure reassurance missing: %s", text)
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

func TestRAGIngestionPolicyRouteRejectsArbitraryPolicyBody(t *testing.T) {
	server := NewServer(0)
	mux := http.NewServeMux()
	server.registerRAGPolicyRoutes(mux, func(next http.HandlerFunc) http.HandlerFunc { return next })
	request := httptest.NewRequest(http.MethodPost, "/api/admin/rag-policies/ingestion/promotions", strings.NewReader(`{"runId":"run","profileId":"profile","confirmationRunId":"confirmation","policy":{"apiKey":"secret"}}`))
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRAGKBPolicySyncRequiresExplicitConfirmation(t *testing.T) {
	server, resolver, _, regular, service := newRAGAPITestServer(t)
	kb, err := service.CreateKB(context.Background(), regular.ID, "sync-confirm", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	request := ragJSONRequest(t, resolver, http.MethodPost, "/api/rag/kbs/"+kb.ID+"/policy-syncs", regular.ID, `{"targetPolicyVersion":2,"confirm":false}`)
	recorder := callRAGHandler(t, server, server.handleStartRAGKBPolicySync, request, map[string]string{"id": kb.ID})
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "confirmation") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
