package setup

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag"
)

const ragEvalJudgeMaxRequestBytes = int64(2 << 20)

// RAGEvalJudgeResolver returns the current default model binding for the
// authenticated evaluation owner. The sidecar never receives provider
// credentials; only this main-process boundary resolves them.
type RAGEvalJudgeResolver func(context.Context, string) (rag.AnswerModel, string, error)

type ragEvalJudgeRequest struct {
	Model               string             `json:"model"`
	Messages            []provider.Message `json:"messages"`
	Tools               []provider.Tool    `json:"tools"`
	MaxTokens           int                `json:"max_tokens"`
	MaxCompletionTokens int                `json:"max_completion_tokens"`
	Temperature         *float64           `json:"temperature"`
	Stream              bool               `json:"stream"`
}

func (s *Server) handleRAGEvalJudgeProxy(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled || s.ragEvalJudgeResolver == nil {
		http.Error(w, "evaluation judge is unavailable", http.StatusServiceUnavailable)
		return
	}
	expected := strings.TrimSpace(s.ragCfg.Evaluation.Sidecar.APIKey)
	provided := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer ")
	if expected == "" || len(expected) != len(provided) || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ownerID := strings.TrimSpace(r.Header.Get("X-BkCrab-Eval-Owner"))
	if ownerID == "" || len(ownerID) > 120 {
		http.Error(w, "bounded evaluation owner is required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, ragEvalJudgeMaxRequestBytes)
	var request ragEvalJudgeRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid judge request", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid judge request", http.StatusBadRequest)
		return
	}
	if request.Stream || request.Model != s.ragCfg.Evaluation.Sidecar.LLMModel || len(request.Messages) == 0 {
		http.Error(w, "unsupported judge request", http.StatusBadRequest)
		return
	}
	model, resolvedModel, err := s.ragEvalJudgeResolver(r.Context(), ownerID)
	if err != nil {
		http.Error(w, "judge model resolution failed", http.StatusBadGateway)
		return
	}
	maxTokens := request.MaxCompletionTokens
	if maxTokens <= 0 {
		maxTokens = request.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	if maxTokens > 131_072 {
		http.Error(w, "judge max tokens exceeds limit", http.StatusBadRequest)
		return
	}
	temperature := 0.0
	if request.Temperature != nil {
		temperature = *request.Temperature
	}
	response, err := model.Chat(r.Context(), request.Messages, request.Tools, resolvedModel, maxTokens, temperature)
	if err != nil || response == nil {
		http.Error(w, "judge provider request failed", http.StatusBadGateway)
		return
	}
	now := time.Now().UTC()
	finishReason := "stop"
	message := map[string]any{"role": "assistant", "content": response.Content}
	if len(response.ToolCalls) > 0 {
		finishReason = "tool_calls"
		message["tool_calls"] = response.ToolCalls
	}
	payload := map[string]any{
		"id":      "chatcmpl_eval_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		"object":  "chat.completion",
		"created": now.Unix(),
		"model":   request.Model,
		"choices": []map[string]any{{
			"index": 0, "message": message, "finish_reason": finishReason,
		}},
		"usage": map[string]int{
			"prompt_tokens": response.Usage.InputTokens, "completion_tokens": response.Usage.OutputTokens,
			"total_tokens": response.Usage.InputTokens + response.Usage.OutputTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}
