package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
)

func TestUpdatePluginEnablesPluginFramework(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, _ := newAuthTestServer(t, ctx)
	t.Setenv("BKCRAB_HOME", t.TempDir())

	body := strings.NewReader(`{"enabled":true,"config":{"url":"http://mem0:8100","topK":3}}`)
	req := authTestRequestWithBody(t, ctx, resolver, http.MethodPut, "/api/plugins/mem0", adminUser.ID, body)
	req.SetPathValue("id", "mem0")
	rr := httptest.NewRecorder()

	s.requireSuperAdmin(s.handleUpdatePlugin)(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var plugins config.PluginsCfg
	if err := scope.SettingInto(ctx, s.dataStore, "plugins", "", "", &plugins); err != nil {
		t.Fatalf("load plugins setting: %v", err)
	}
	if !plugins.Enabled {
		t.Fatal("plugins.enabled = false, want true after enabling a plugin")
	}
	entry, ok := plugins.Entries["mem0"]
	if !ok {
		t.Fatal("plugins.entries.mem0 missing")
	}
	if !entry.Enabled {
		t.Fatal("plugins.entries.mem0.enabled = false, want true")
	}
	if entry.Config["url"] != "http://mem0:8100" {
		t.Fatalf("mem0 config url = %#v", entry.Config["url"])
	}
	if entry.Config["topK"] != float64(3) {
		t.Fatalf("mem0 config topK = %#v", entry.Config["topK"])
	}
}

func TestListPluginsIncludesSavedConfig(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, _ := newAuthTestServer(t, ctx)
	home := t.TempDir()
	t.Setenv("BKCRAB_HOME", home)
	writeTestPluginManifest(t, home, "mem0")

	updateBody := strings.NewReader(`{"enabled":true,"config":{"url":"http://mem0:8100","apiKey":"secret","topK":4}}`)
	updateReq := authTestRequestWithBody(t, ctx, resolver, http.MethodPut, "/api/plugins/mem0", adminUser.ID, updateBody)
	updateReq.SetPathValue("id", "mem0")
	updateRR := httptest.NewRecorder()
	s.requireSuperAdmin(s.handleUpdatePlugin)(updateRR, updateReq)
	if updateRR.Code != http.StatusOK {
		t.Fatalf("update status = %d, body = %s", updateRR.Code, updateRR.Body.String())
	}

	listReq := authTestRequest(t, ctx, resolver, http.MethodGet, "/api/plugins", adminUser.ID)
	listRR := httptest.NewRecorder()
	s.requireSuperAdmin(s.handleListPlugins)(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", listRR.Code, listRR.Body.String())
	}

	var plugins []struct {
		ID      string         `json:"id"`
		Enabled bool           `json:"enabled"`
		Config  map[string]any `json:"config"`
	}
	decodeJSONResponse(t, listRR, &plugins)

	var mem0 *struct {
		ID      string         `json:"id"`
		Enabled bool           `json:"enabled"`
		Config  map[string]any `json:"config"`
	}
	for i := range plugins {
		if plugins[i].ID == "mem0" {
			mem0 = &plugins[i]
			break
		}
	}
	if mem0 == nil {
		t.Fatalf("mem0 plugin missing from list: %#v", plugins)
	}
	if !mem0.Enabled {
		t.Fatal("mem0 enabled = false, want true")
	}
	if mem0.Config["url"] != "http://mem0:8100" {
		t.Fatalf("mem0 config url = %#v", mem0.Config["url"])
	}
	if mem0.Config["apiKey"] != "secret" {
		t.Fatalf("mem0 config apiKey = %#v", mem0.Config["apiKey"])
	}
	if mem0.Config["topK"] != float64(4) {
		t.Fatalf("mem0 config topK = %#v", mem0.Config["topK"])
	}
}

func writeTestPluginManifest(t *testing.T, home, id string) {
	t.Helper()
	dir := filepath.Join(home, "plugins", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}
	manifest := `{"id":"` + id + `","name":"` + id + `","version":"1.0.0","type":"hook","command":"python3 plugin.py","capabilities":["hook"]}`
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write plugin manifest: %v", err)
	}
}

func authTestRequestWithBody(t *testing.T, ctx context.Context, resolver interface {
	IssueSession(context.Context, string) (*http.Cookie, error)
}, method, path, userID string, body *strings.Reader) *http.Request {
	t.Helper()

	cookie, err := resolver.IssueSession(ctx, userID)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	req := httptest.NewRequest(method, path, body)
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func decodeJSONResponse(t *testing.T, rr *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), out); err != nil {
		t.Fatalf("decode response %q: %v", rr.Body.String(), err)
	}
}
