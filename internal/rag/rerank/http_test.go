package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPClientReranksAndValidatesRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Fatalf("request path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		var request struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "qwen3-reranker" || request.Query != "安装" ||
			request.TopN != 2 || len(request.Documents) != 3 {
			t.Fatalf("request = %+v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.61},
				{"index": 2, "relevance_score": 0.92},
			},
		})
	}))
	t.Cleanup(server.Close)

	client, err := NewHTTP(server.URL+"/v1", "secret", "qwen3-reranker", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	results, err := client.Rerank(context.Background(), "安装", []string{"a", "b", "c"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0] != (Result{Index: 2, Score: 0.92}) ||
		results[1] != (Result{Index: 0, Score: 0.61}) {
		t.Fatalf("results = %+v", results)
	}
}

func TestHTTPClientRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name string
		body any
		want string
	}{
		{name: "empty", body: map[string]any{"results": []any{}}, want: "空结果"},
		{name: "missing fields", body: map[string]any{"results": []any{map[string]any{}}}, want: "缺少"},
		{name: "index", body: map[string]any{"results": []any{map[string]any{"index": 3, "relevance_score": 0.8}}}, want: "非法 index"},
		{name: "score", body: map[string]any{"results": []any{map[string]any{"index": 0, "relevance_score": 1.2}}}, want: "非法分数"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(tt.body)
			}))
			defer server.Close()
			client, err := NewHTTP(server.URL, "", "", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Rerank(context.Background(), "q", []string{"a"}, 1)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}
