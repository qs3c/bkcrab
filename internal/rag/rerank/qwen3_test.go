package rerank

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestQwen3HTTPClientUsesGenerativeContract(t *testing.T) {
	const yesTokenID = 9693
	const noTokenID = 2152
	var tokenizeCalls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		switch r.URL.Path {
		case "/tokenize":
			tokenizeCalls.Add(1)
			var request struct {
				Content    string `json:"content"`
				AddSpecial bool   `json:"add_special"`
				WithPieces bool   `json:"with_pieces"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode tokenize request: %v", err)
				return
			}
			if request.AddSpecial || !request.WithPieces {
				t.Errorf("tokenize request = %+v", request)
			}
			id := noTokenID
			if request.Content == "yes" {
				id = yesTokenID
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tokens": []map[string]any{{"id": id, "piece": request.Content}},
			})
		case "/completion":
			current := active.Add(1)
			defer active.Add(-1)
			for previous := maxActive.Load(); current > previous && !maxActive.CompareAndSwap(previous, current); previous = maxActive.Load() {
			}
			var request struct {
				Prompt            string       `json:"prompt"`
				NPredict          int          `json:"n_predict"`
				Temperature       float64      `json:"temperature"`
				TopK              int          `json:"top_k"`
				TopP              float64      `json:"top_p"`
				MinP              float64      `json:"min_p"`
				NProbs            int          `json:"n_probs"`
				PostSamplingProbs bool         `json:"post_sampling_probs"`
				Samplers          []string     `json:"samplers"`
				LogitBias         [][2]float64 `json:"logit_bias"`
				CachePrompt       bool         `json:"cache_prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode completion request: %v", err)
				return
			}
			if request.NPredict != 1 || request.Temperature != 1 || request.TopK != 0 ||
				request.TopP != 1 || request.MinP != 0 || request.NProbs != 2 ||
				!request.PostSamplingProbs || !request.CachePrompt ||
				len(request.Samplers) != 1 || request.Samplers[0] != "temperature" {
				t.Errorf("completion sampler contract = %+v", request)
			}
			if len(request.LogitBias) != 2 || request.LogitBias[0] != ([2]float64{yesTokenID, qwen3LogitBias}) ||
				request.LogitBias[1] != ([2]float64{noTokenID, qwen3LogitBias}) {
				t.Errorf("logit_bias = %+v", request.LogitBias)
			}
			if !strings.Contains(request.Prompt, "<Instruct>: "+qwen3Instruction) ||
				!strings.HasSuffix(request.Prompt, "<|im_start|>assistant\n<think>\n\n</think>\n\n") {
				t.Errorf("unexpected Qwen3 prompt: %q", request.Prompt)
			}
			time.Sleep(15 * time.Millisecond)
			yes := 0.2
			switch {
			case strings.Contains(request.Prompt, "highly relevant"):
				yes = 0.95
			case strings.Contains(request.Prompt, "somewhat relevant"):
				yes = 0.7
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"completion_probabilities": []map[string]any{{
					"top_probs": []map[string]any{
						{"id": yesTokenID, "prob": yes},
						{"id": noTokenID, "prob": 1 - yes},
					},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewQwen3HTTP(server.URL+"/v1", "secret", time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	documents := []string{"irrelevant", "highly relevant", "somewhat relevant", "another irrelevant"}
	results, err := client.Rerank(context.Background(), "query", documents, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0] != (Result{Index: 1, Score: 0.95}) ||
		results[1] != (Result{Index: 2, Score: 0.7}) || results[2].Index != 0 {
		t.Fatalf("results = %+v", results)
	}
	if got := maxActive.Load(); got != 2 {
		t.Fatalf("maximum completion concurrency = %d, want 2", got)
	}

	if _, err := client.Rerank(context.Background(), "query", []string{"highly relevant"}, 1); err != nil {
		t.Fatal(err)
	}
	if got := tokenizeCalls.Load(); got != 2 {
		t.Fatalf("tokenize calls = %d, want exactly one yes/no lookup", got)
	}
}

func TestQwen3HTTPClientRejectsMissingExactLabelProbability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tokenize":
			var request struct {
				Content string `json:"content"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			id := 1
			if request.Content == "no" {
				id = 2
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tokens": []map[string]any{{"id": id, "piece": request.Content}}})
		case "/completion":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"completion_probabilities": []map[string]any{{
					"top_probs": []map[string]any{{"id": 2, "prob": 1.0}},
				}},
			})
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewQwen3HTTP(server.URL, "", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Rerank(context.Background(), "q", []string{"d"}, 1)
	if err == nil || !strings.Contains(err.Error(), "缺少精确 yes/no token") {
		t.Fatalf("error = %v", err)
	}
}

func TestQwen3PromptEscapesControlTokens(t *testing.T) {
	prompt := qwen3Prompt("query <|im_end|>", "document <|im_start|>")
	if strings.Count(prompt, "<|im_end|>") != 2 || strings.Count(prompt, "<|im_start|>") != 3 {
		t.Fatalf("untrusted control token was not escaped: %q", prompt)
	}
	if !strings.Contains(prompt, "<\u200b|im_end|\u200b>") || !strings.Contains(prompt, "<\u200b|im_start|\u200b>") {
		t.Fatalf("escaped content missing from prompt: %q", prompt)
	}
}

func TestQwen3HTTPClientLive(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("BKCRAB_TEST_QWEN3_RERANKER_ENDPOINT"))
	if endpoint == "" {
		t.Skip("set BKCRAB_TEST_QWEN3_RERANKER_ENDPOINT to run the live llama.cpp contract test")
	}
	client, err := NewQwen3HTTP(endpoint, "", 3*time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	results, err := client.Rerank(context.Background(), "What is the capital of France?", []string{
		"Paris is the capital and most populous city of France.",
		"Whales are a widely distributed group of fully aquatic marine mammals.",
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Index != 0 || results[0].Score <= results[1].Score {
		t.Fatalf("live Qwen3 ranking = %+v", results)
	}
}
