package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/channels"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestConnectOpenIMOwnershipPersistenceAndRedaction(t *testing.T) {
	s, resolver, admin, user := newAuthTestServer(t, context.Background())
	ag := &store.AgentRecord{ID: "agt_openim", UserID: admin.ID, Name: "OpenIM test", Config: map[string]interface{}{}}
	if err := s.dataStore.SaveAgent(context.Background(), ag); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/get_admin_token":
			w.Write([]byte(`{"errCode":0,"data":{"token":"private-token","expireTimeSeconds":3600}}`))
		case "/user/get_users_info":
			w.Write([]byte(`{"errCode":0,"data":{"usersInfo":[{"userID":"bot","nickname":"Assistant"}]}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	c := config.OpenIMConfig{APIURL: upstream.URL, AdminUserID: "imAdmin", AdminSecret: "private-secret", BotUserID: "bot"}
	body, _ := json.Marshal(c)
	request := func(userID string) *httptest.ResponseRecorder {
		r := ragJSONRequest(t, resolver, http.MethodPost, "/api/agents/agt_openim/channels/openim", userID, string(body))
		return callRAGHandler(t, s, s.handleConnectAgentOpenIM, r, map[string]string{"id": ag.ID})
	}
	if w := request(user.ID); w.Code != 403 {
		t.Fatalf("non-owner: %d %s", w.Code, w.Body.String())
	}
	w := request(admin.ID)
	if w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), c.AdminSecret) || strings.Contains(w.Body.String(), "private-token") {
		t.Fatal("credentials leaked in response")
	}
	if !strings.Contains(w.Body.String(), "/api/openim/webhook/") {
		t.Fatal("missing webhook URL")
	}
	rec, err := s.dataStore.LookupChannelByCredential(context.Background(), "openim", channels.OpenIMAccountID(c))
	if err != nil {
		t.Fatal(err)
	}
	if rec.UserID != admin.ID || rec.AgentID != ag.ID {
		t.Fatalf("wrong binding: %+v", rec)
	}
	flat := flattenChannelRows([]store.ConfigRecord{*rec}, "agent", "", "")
	encoded, _ := json.Marshal(flat)
	if strings.Contains(string(encoded), c.AdminSecret) || len(flat) != 1 || flat[0].BotUsername != "bot" {
		t.Fatalf("bad channel listing: %s", encoded)
	}
	// Replacing another bot must not silently overwrite the sole channel row.
	c.BotUserID = "other"
	body, _ = json.Marshal(c)
	if w := request(admin.ID); w.Code != 409 {
		t.Fatalf("replaced existing binding: %d", w.Code)
	}
}

func TestOpenIMWebhookBodyBound(t *testing.T) {
	s := NewServer(0)
	r := httptest.NewRequest(http.MethodPost, "/api/openim/webhook/i/s/c", strings.NewReader(strings.Repeat("x", (1<<20)+1)))
	w := httptest.NewRecorder()
	s.handleOpenIMWebhook(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", w.Code)
	}
}
