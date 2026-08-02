package setup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/fairqueue"
)

type staticFairQueueHealthProvider struct {
	snapshot fairqueue.HealthSnapshot
}

func (p staticFairQueueHealthProvider) FairQueueHealthSnapshot() fairqueue.HealthSnapshot {
	return p.snapshot
}

func readyFairQueueHealthSnapshot() fairqueue.HealthSnapshot {
	return fairqueue.HealthSnapshot{FairQueue: fairqueue.FairQueueHealthSnapshot{
		Enabled: true,
		Status:  fairqueue.HealthStatusHealthy,
		Mode:    "fair",
		MySQL: fairqueue.MySQLHealthSnapshot{
			Status:          fairqueue.MySQLStatusOK,
			SchemaReady:     true,
			SessionAffinity: fairqueue.SessionAffinityVerified,
		},
		Rabbit: fairqueue.RabbitHealthSnapshot{Status: "unavailable"},
		Redis:  fairqueue.RedisHealthSnapshot{Status: "unavailable"},
	}}
}

func TestFairQueueReadinessUsesOnlyAPIAndMySQLSafety(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*fairqueue.HealthSnapshot)
		provider bool
		want     int
	}{
		{name: "provider not initialized", want: http.StatusServiceUnavailable},
		{name: "rabbit and redis unavailable do not withdraw the API", provider: true, want: http.StatusOK},
		{name: "overall dependency degradation does not withdraw the API", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.Status = fairqueue.HealthStatusDegraded
		}, want: http.StatusOK},
		{name: "enabled legacy dependency degradation does not withdraw the API", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.Status = fairqueue.HealthStatusDegraded
			snapshot.FairQueue.Mode = "legacy"
			snapshot.FairQueue.MySQL.SessionAffinity = fairqueue.SessionAffinityUnknown
		}, want: http.StatusOK},
		{name: "mysql unavailable", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.MySQL.Status = fairqueue.MySQLStatusUnavailable
		}, want: http.StatusServiceUnavailable},
		{name: "mysql writer mismatch", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.MySQL.Status = fairqueue.MySQLStatusMismatch
		}, want: http.StatusServiceUnavailable},
		{name: "schema invalid", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.MySQL.SchemaReady = false
		}, want: http.StatusServiceUnavailable},
		{name: "session mismatch", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.MySQL.SessionAffinity = fairqueue.SessionAffinityMismatch
		}, want: http.StatusServiceUnavailable},
		{name: "fair runtime failure does not withdraw a mysql-ready API", provider: true, mutate: func(snapshot *fairqueue.HealthSnapshot) {
			snapshot.FairQueue.Status = fairqueue.HealthStatusFailed
		}, want: http.StatusOK},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := NewServer(0)
			if test.provider {
				snapshot := readyFairQueueHealthSnapshot()
				if test.mutate != nil {
					test.mutate(&snapshot)
				}
				s.SetFairQueueHealthProvider(staticFairQueueHealthProvider{snapshot: snapshot})
			}
			recorder := newHealthRecorder(t, s.handleReady)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func TestCompatibilityHealthAndLivenessNeverExposeDependencyState(t *testing.T) {
	s := NewServer(0)
	failed := readyFairQueueHealthSnapshot()
	failed.FairQueue.Status = fairqueue.HealthStatusFailed
	failed.FairQueue.MySQL.Status = fairqueue.MySQLStatusMismatch
	s.SetFairQueueHealthProvider(staticFairQueueHealthProvider{snapshot: failed})

	for name, handler := range map[string]http.HandlerFunc{
		"healthz": s.handleHealth,
		"livez":   s.handleLive,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := newHealthRecorder(t, handler)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if body := recorder.Body.String(); body != "ok" {
				t.Fatalf("body = %q, want compatibility body %q", body, "ok")
			}
		})
	}
}

func TestFairQueueAdminHealthRouteRequiresPlatformAdmin(t *testing.T) {
	ctx := context.Background()
	s, resolver, adminUser, regularUser := newAuthTestServer(t, ctx)
	s.SetFairQueueHealthProvider(staticFairQueueHealthProvider{snapshot: readyFairQueueHealthSnapshot()})
	s.port = freeTCPPort(t)

	runCtx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(runCtx) }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not stop after context cancellation")
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		resp := fairQueueHealthHTTPResponse(t, baseURL, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
		}
	})

	t.Run("regular user", func(t *testing.T) {
		cookie, err := resolver.IssueSession(ctx, regularUser.ID)
		if err != nil {
			t.Fatalf("IssueSession: %v", err)
		}
		resp := fairQueueHealthHTTPResponse(t, baseURL, cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusForbidden)
		}
	})

	t.Run("super admin receives only the cached snapshot", func(t *testing.T) {
		cookie, err := resolver.IssueSession(ctx, adminUser.ID)
		if err != nil {
			t.Fatalf("IssueSession: %v", err)
		}
		resp := fairQueueHealthHTTPResponse(t, baseURL, cookie)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(body) != 1 || body["fairQueue"] == nil {
			t.Fatalf("top-level response = %v, want only fairQueue", body)
		}
		var detail fairqueue.FairQueueHealthSnapshot
		if err := json.Unmarshal(body["fairQueue"], &detail); err != nil {
			t.Fatalf("Unmarshal fairQueue: %v", err)
		}
		if detail.Status != fairqueue.HealthStatusHealthy || detail.Mode != "fair" {
			t.Fatalf("detail = %#v", detail)
		}
	})
}

func TestReadyRouteReturnsServiceUnavailableBeforeHealthInitialization(t *testing.T) {
	s := NewServer(0)
	s.port = freeTCPPort(t)
	runCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(runCtx) }()
	baseURL := "http://127.0.0.1:" + strconv.Itoa(s.port)
	waitForSetupServer(t, baseURL, errCh)
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop after context cancellation")
		}
	})

	resp, err := setupRouteTestHTTPClient.Get(baseURL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, http.StatusServiceUnavailable, body)
	}
}

func newHealthRecorder(t *testing.T, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, mustHealthRequest(t))
	return recorder
}

func mustHealthRequest(t *testing.T) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "/health", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return request
}

func fairQueueHealthHTTPResponse(t *testing.T, baseURL string, cookie *http.Cookie) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/admin/health/fairqueue", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := setupRouteTestHTTPClient.Do(request)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return response
}
