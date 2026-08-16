package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag"
)

type staticEvalJudge struct {
	tools []provider.Tool
}

func (s *staticEvalJudge) Chat(_ context.Context, _ []provider.Message, tools []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	s.tools = tools
	return &provider.Response{ToolCalls: []provider.ToolCall{{ID: "call_score", Type: "function", Function: provider.FunctionCall{Name: "score", Arguments: `{"value":1}`}}}, Usage: provider.Usage{InputTokens: 7, OutputTokens: 2}}, nil
}

func TestRAGEvalJudgeProxyRequiresInternalTokenAndForwardsOwner(t *testing.T) {
	server := NewServer(0)
	server.SetRAGConfig(config.RAGCfg{Evaluation: config.RAGEvaluationCfg{Enabled: true, Sidecar: config.RAGEvaluatorCfg{APIKey: "internal-secret", LLMModel: "bkcrab-default"}}})
	resolvedOwner := ""
	judge := &staticEvalJudge{}
	server.SetRAGEvaluationJudgeResolver(func(_ context.Context, owner string) (rag.AnswerModel, string, error) {
		resolvedOwner = owner
		return judge, "resolved-model", nil
	})
	body := `{"model":"bkcrab-default","messages":[{"role":"user","content":"judge this"}],"tools":[{"type":"function","function":{"name":"score","parameters":{"type":"object"}}}],"max_tokens":32}`

	unauthorized := httptest.NewRecorder()
	server.handleRAGEvalJudgeProxy(unauthorized, httptest.NewRequest(http.MethodPost, "/internal/rag-eval/judge/v1/chat/completions", strings.NewReader(body)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/internal/rag-eval/judge/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer internal-secret")
	request.Header.Set("X-BkCrab-Eval-Owner", "admin")
	response := httptest.NewRecorder()
	server.handleRAGEvalJudgeProxy(response, request)
	if response.Code != http.StatusOK || resolvedOwner != "admin" || len(judge.tools) != 1 ||
		!strings.Contains(response.Body.String(), `"prompt_tokens":7`) || !strings.Contains(response.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("status=%d owner=%q body=%s", response.Code, resolvedOwner, response.Body.String())
	}
}
