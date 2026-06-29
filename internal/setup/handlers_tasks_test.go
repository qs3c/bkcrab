package setup

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/users"
)

var setupRouteTestHTTPClient = &http.Client{Timeout: 2 * time.Second}

func TestListTasksRouteRequiresPlatformAdmin(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, regularUser := newAuthTestServer(t, ctx)
	s.port = freeTCPPort(t)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Run(runCtx)
	}()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		resp := tasksListHTTPResponse(t, baseURL, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("regular user is rejected", func(t *testing.T) {
		cookie, err := resolver.IssueSession(ctx, regularUser.ID)
		if err != nil {
			t.Fatalf("IssueSession: %v", err)
		}
		resp := tasksListHTTPResponse(t, baseURL, cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
		}
	})

	t.Run("super admin is allowed", func(t *testing.T) {
		cookie, err := resolver.IssueSession(ctx, adminUser.ID)
		if err != nil {
			t.Fatalf("IssueSession: %v", err)
		}
		resp := tasksListHTTPResponse(t, baseURL, cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if got := strings.TrimSpace(string(body)); got != "[]" {
			t.Fatalf("body = %q, want []", got)
		}
	})

	t.Run("admin api key is allowed", func(t *testing.T) {
		if s.apikeys == nil {
			t.Fatal("apikeys not configured")
		}
		_, token, err := s.apikeys.Create(ctx, adminUser.ID, "tasks-admin", users.APIKeyTypeAdmin, nil)
		if err != nil {
			t.Fatalf("Create admin apikey: %v", err)
		}
		resp := tasksListBearerHTTPResponse(t, baseURL, token)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
	})

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after context cancellation")
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitForSetupServer(t *testing.T, baseURL string, errCh <-chan error) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			t.Fatalf("Run exited before server was ready: %v", err)
		default:
		}
		resp, err := setupRouteTestHTTPClient.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not become ready at %s", baseURL)
}

func tasksListHTTPResponse(t *testing.T, baseURL string, cookie *http.Cookie) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/tasks", baseURL), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := setupRouteTestHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

func tasksListBearerHTTPResponse(t *testing.T, baseURL, token string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/tasks", baseURL), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := setupRouteTestHTTPClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}
