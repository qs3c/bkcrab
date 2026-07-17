package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
)

func TestPlanQueryUsesQuestionHistoryAndSingleLLMCall(t *testing.T) {
	var calls atomic.Int32
	var gotSystem, gotUser string
	service := &Service{queryLLM: func(_ context.Context, userID, systemPrompt, userPrompt string) (string, error) {
		calls.Add(1)
		if userID != "u1" {
			t.Fatalf("planner user = %q, want u1", userID)
		}
		gotSystem, gotUser = systemPrompt, userPrompt
		return `{"rewritten_query":"Windows 系统如何安装 bkcrab？","hypothetical_document":"在 Windows 系统中安装 bkcrab 时，需要准备 Docker 环境。"}`, nil
	}}

	plan := service.planQuery(context.Background(), "u1", SearchContext{
		Query:   "那 Windows 呢？",
		History: []string{"如何安装 bkcrab？"},
	})
	if calls.Load() != 1 {
		t.Fatalf("planner calls = %d, want 1", calls.Load())
	}
	if plan.RewrittenQuery != "Windows 系统如何安装 bkcrab？" ||
		plan.HypotheticalDocument != "在 Windows 系统中安装 bkcrab 时，需要准备 Docker 环境。" {
		t.Fatalf("query plan = %+v", plan)
	}
	if !strings.Contains(gotSystem, "历史提问") || !strings.Contains(gotSystem, "口语化") {
		t.Fatalf("planner system prompt is missing rewrite requirements: %q", gotSystem)
	}

	const prefix = "请处理下面的 JSON 数据：\n"
	if !strings.HasPrefix(gotUser, prefix) {
		t.Fatalf("planner user prompt = %q", gotUser)
	}
	var payload struct {
		HistoryQuestions []string `json:"history_questions"`
		CurrentQuery     string   `json:"current_query"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(gotUser, prefix)), &payload); err != nil {
		t.Fatalf("decode planner input: %v", err)
	}
	if len(payload.HistoryQuestions) != 1 || payload.HistoryQuestions[0] != "如何安装 bkcrab？" {
		t.Fatalf("planner history = %#v", payload.HistoryQuestions)
	}
	if payload.CurrentQuery != "那 Windows 呢？" {
		t.Fatalf("planner current query = %q", payload.CurrentQuery)
	}
}

func TestPlanQueryFallsBackAndSupportsRewriteOnly(t *testing.T) {
	input := SearchContext{Query: "  原始问题  "}
	tests := []struct {
		name string
		llm  QueryLLMFn
		want QueryPlan
	}{
		{
			name: "planner unavailable",
			want: QueryPlan{RewrittenQuery: "原始问题", HypotheticalDocument: "原始问题"},
		},
		{
			name: "planner error",
			llm: func(context.Context, string, string, string) (string, error) {
				return "", errors.New("upstream unavailable")
			},
			want: QueryPlan{RewrittenQuery: "原始问题", HypotheticalDocument: "原始问题"},
		},
		{
			name: "invalid output",
			llm: func(context.Context, string, string, string) (string, error) {
				return "not-json", nil
			},
			want: QueryPlan{RewrittenQuery: "原始问题", HypotheticalDocument: "原始问题"},
		},
		{
			name: "rewrite only",
			llm: func(context.Context, string, string, string) (string, error) {
				return `{"rewritten_query":"规范问题","hypothetical_document":""}`, nil
			},
			want: QueryPlan{RewrittenQuery: "规范问题", HypotheticalDocument: "规范问题"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := &Service{queryLLM: tt.llm}
			got := service.planQuery(context.Background(), "u1", input)
			if got != tt.want {
				t.Fatalf("plan = %+v, want %+v", got, tt.want)
			}
		})
	}
}

type queryCaptureVector struct {
	*vector.Fake
	mu        sync.Mutex
	queryText []string
}

func (v *queryCaptureVector) HybridSearch(ctx context.Context, kbID string, queryVec []float32, queryText string, topK int) ([]vector.SearchHit, error) {
	v.mu.Lock()
	v.queryText = append(v.queryText, queryText)
	v.mu.Unlock()
	return v.Fake.HybridSearch(ctx, kbID, queryVec, queryText, topK)
}

func (v *queryCaptureVector) texts() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.queryText...)
}

func TestSearchRoutesRewriteToBM25AndHyDEToDense(t *testing.T) {
	var embedMu sync.Mutex
	var embedInputs [][]string
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		embedMu.Lock()
		embedInputs = append(embedInputs, append([]string(nil), request.Input...))
		embedMu.Unlock()
		data := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(embedServer.Close)

	var plannerCalls atomic.Int32
	vec := &queryCaptureVector{Fake: vector.NewFake()}
	service := New(Deps{
		Store:   newRAGTestStore(t),
		Vector:  vec,
		Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedServer.URL, Model: "test", Dims: 4},
		},
		QueryLLM: func(context.Context, string, string, string) (string, error) {
			plannerCalls.Add(1)
			return `{"rewritten_query":"Windows 安装 bkcrab","hypothetical_document":"Windows 部署 bkcrab 的安装说明"}`, nil
		},
	})
	ctx := context.Background()
	first, err := service.CreateKB(ctx, "u1", "手册一", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateKB(ctx, "u1", "手册二", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.SearchWithContext(ctx, "u1", []string{first.ID, second.ID}, SearchContext{
		Query:   "那 Windows 呢？",
		History: []string{"如何安装 bkcrab？"},
	}, 5); err != nil {
		t.Fatal(err)
	}
	if plannerCalls.Load() != 1 {
		t.Fatalf("planner calls = %d, want one call for all KBs", plannerCalls.Load())
	}
	embedMu.Lock()
	gotEmbedInputs := append([][]string(nil), embedInputs...)
	embedMu.Unlock()
	if len(gotEmbedInputs) != 1 || len(gotEmbedInputs[0]) != 1 || gotEmbedInputs[0][0] != "Windows 部署 bkcrab 的安装说明" {
		t.Fatalf("embedding inputs = %#v, want only HyDE", gotEmbedInputs)
	}
	texts := vec.texts()
	if len(texts) != 2 || texts[0] != "Windows 安装 bkcrab" || texts[1] != "Windows 安装 bkcrab" {
		t.Fatalf("BM25 query texts = %#v, want rewritten query for both KBs", texts)
	}
}
