package setup

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/qs3c/bkcrab/internal/jeveval"
)

func (s *Server) SetRAGEvaluationJev(service *jeveval.Service) { s.ragEvalJev = service }
func (s *Server) handleRAGEvalJev(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled || s.ragEvalJev == nil || s.ragEvalJudgeResolver == nil {
		http.Error(w, "Jev evaluator unavailable", 503)
		return
	}
	expected := strings.TrimSpace(s.ragCfg.Evaluation.Sidecar.APIKey)
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	owner := strings.TrimSpace(r.Header.Get("X-BkCrab-Eval-Owner"))
	if owner == "" || len(owner) > 120 {
		http.Error(w, "invalid owner", 400)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	var request jeveval.Request
	if err := d.Decode(&request); err != nil {
		http.Error(w, "invalid judge request", 400)
		return
	}
	var tail any
	if !errors.Is(d.Decode(&tail), io.EOF) {
		http.Error(w, "invalid judge request", 400)
		return
	}
	// Reuse the main process's active-super-admin owner check. No credentials
	// or arbitrary endpoint, model prompt, or provider key come from the sidecar.
	if _, _, err := s.ragEvalJudgeResolver(r.Context(), owner); err != nil {
		http.Error(w, "judge owner unavailable", 403)
		return
	}
	response := s.ragEvalJev.Evaluate(r.Context(), request)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
