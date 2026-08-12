package setup

import (
	"net/http"

	"github.com/qs3c/bkcrab/internal/fairqueue"
)

func writeLightweightHealth(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// handleHealth preserves the historical lightweight compatibility response.
// It intentionally does not become a dependency-health endpoint.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeLightweightHealth(w, http.StatusOK, "ok")
}

// handleLive reports only that the HTTP process/event loop is alive. Rabbit,
// Redis and MySQL outages must not cause a liveness-restart loop.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeLightweightHealth(w, http.StatusOK, "ok")
}

// handleReady keeps the durable MySQL-backed API available through Rabbit or
// Redis degradation. It withdraws readiness only while the cached gateway
// snapshot cannot prove API/MySQL/schema safety.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	snapshot, initialized := s.fairQueueHealthSnapshot()
	if !initialized || !fairQueueAPIReady(snapshot) {
		writeLightweightHealth(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeLightweightHealth(w, http.StatusOK, "ok")
}

func fairQueueAPIReady(snapshot fairqueue.HealthSnapshot) bool {
	return fairQueueResourceAPIReady(snapshot.FairQueue)
}

func fairQueueResourceAPIReady(health fairqueue.FairQueueHealthSnapshot) bool {
	if health.Status == "" {
		return false
	}
	if health.MySQL.Status != fairqueue.MySQLStatusOK || !health.MySQL.SchemaReady {
		return false
	}
	if health.MySQL.SessionAffinity == fairqueue.SessionAffinityMismatch {
		return false
	}
	// Fair mode cannot safely claim or mutate authoritative state before the
	// cached writer/session check has succeeded. Legacy and paused modes do not
	// require that fair-only identity fence.
	if (health.Mode == "fair" || health.Mode == "drain") && health.MySQL.SessionAffinity != fairqueue.SessionAffinityVerified {
		return false
	}
	for _, resource := range health.Resources {
		if !fairQueueResourceAPIReady(resource) {
			return false
		}
	}
	return true
}

// handleFairQueueHealth returns exactly the cached, serialization-safe
// snapshot. Authentication is applied by route registration; this handler
// performs no network or database work and adds no extra diagnostic fields.
func (s *Server) handleFairQueueHealth(w http.ResponseWriter, _ *http.Request) {
	snapshot, _ := s.fairQueueHealthSnapshot()
	jsonResponse(w, http.StatusOK, snapshot)
}
