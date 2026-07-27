package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbedBatchesAndDims(t *testing.T) {
	t.Parallel()
	var batches [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Model != "m1" {
			t.Errorf("model = %q", request.Model)
		}
		if len(request.Input) > batchSize {
			t.Errorf("batch size = %d", len(request.Input))
		}
		batches = append(batches, append([]string(nil), request.Input...))

		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		response := struct {
			Data []item `json:"data"`
		}{}
		// Return items in reverse order to verify index-based ordering.
		for i := len(request.Input) - 1; i >= 0; i-- {
			response.Data = append(response.Data, item{
				Embedding: []float32{float32(i), 2, 3}, Index: i,
			})
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	client := New(server.URL+"/", "test-key", "m1", 3)
	if client.Model() != "m1" || client.Dims() != 3 {
		t.Fatalf("client metadata = %q/%d", client.Model(), client.Dims())
	}
	texts := make([]string, 20)
	for i := range texts {
		texts[i] = "t"
	}
	vectors, err := client.Embed(context.Background(), texts)
	if err != nil || len(vectors) != len(texts) {
		t.Fatalf("Embed returned %d vectors: %v", len(vectors), err)
	}
	if len(batches) != 3 || len(batches[0]) != 8 || len(batches[1]) != 8 || len(batches[2]) != 4 {
		t.Fatalf("unexpected batches: %v", batches)
	}
	if vectors[0][0] != 0 || vectors[7][0] != 7 || vectors[8][0] != 0 || vectors[16][0] != 0 {
		t.Fatalf("vectors are not in input order: first values %v/%v/%v/%v",
			vectors[0][0], vectors[7][0], vectors[8][0], vectors[16][0])
	}
}

func TestEmbedEmptyInputDoesNotCallEndpoint(t *testing.T) {
	t.Parallel()
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	defer server.Close()
	vectors, err := New(server.URL, "", "m", 3).Embed(context.Background(), nil)
	if err != nil || len(vectors) != 0 || called {
		t.Fatalf("vectors=%v err=%v called=%v", vectors, err, called)
	}
}

func TestEmbedBatchesByAggregateInputBytes(t *testing.T) {
	t.Parallel()
	var batchSizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		batchSizes = append(batchSizes, len(request.Input))
		type item struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}
		response := struct {
			Data []item `json:"data"`
		}{}
		for i := range request.Input {
			response.Data = append(response.Data, item{Embedding: []float32{1}, Index: i})
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	texts := make([]string, 5)
	for i := range texts {
		texts[i] = strings.Repeat("x", maxEmbeddingBatchInputBytes/2)
	}
	vectors, err := New(server.URL, "", "m", 1).Embed(context.Background(), texts)
	if err != nil || len(vectors) != len(texts) {
		t.Fatalf("Embed returned %d vectors: %v", len(vectors), err)
	}
	if got := fmt.Sprint(batchSizes); got != "[2 2 1]" {
		t.Fatalf("byte-bounded batch sizes = %s, want [2 2 1]", got)
	}
}

func TestEmbedDimsMismatch(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2],"index":0}]}`))
	}))
	defer server.Close()
	_, err := New(server.URL, "k", "m", 3).Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "维度不符") {
		t.Fatalf("dimension error = %v", err)
	}
}

func TestEmbedRejectsDuplicateIndex(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
{"embedding":[1],"index":0},{"embedding":[2],"index":0}]}`))
	}))
	defer server.Close()
	_, err := New(server.URL, "", "m", 1).Embed(
		context.Background(), []string{"x", "y"},
	)
	if err == nil || !strings.Contains(err.Error(), "重复 index") {
		t.Fatalf("duplicate index error = %v", err)
	}
}

func TestEmbedReportsEndpointError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	_, err := New(server.URL, "", "m", 1).Embed(context.Background(), []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "503: provider unavailable") {
		t.Fatalf("endpoint error = %v", err)
	}
}
