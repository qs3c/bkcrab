package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestJevCandidateMappingConcurrencyAndUsage(t *testing.T) {
	var active, peak atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if v <= p || peak.CompareAndSwap(p, v) {
				break
			}
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		var req struct {
			State struct {
				Passages map[string]string `json:"passages"`
			} `json:"state"`
			Questions map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		a := map[string]any{}
		for id, doc := range req.State.Passages {
			score := 0.1
			if strings.Contains(doc, "退款") {
				score = 0.9
			}
			a[id] = map[string]any{"type": "noul", "noul": score}
			if req.Questions[id] == nil {
				t.Error("missing matching question")
			}
		}
		time.Sleep(20 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-fixed", "answers": a, "usage": map[string]any{"input_tokens": 20, "cost": 0.00001}})
	}))
	defer s.Close()
	c, err := NewJevHTTP(s.URL, "secret", "model", time.Second, 2, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	r, calls, err := c.RankDetailed(context.Background(), "查询退款条件", []string{"营业时间", "退款需七日内", "交通地址"}, 3)
	if err != nil || len(r) != 3 || r[0].Index != 1 || r[0].Score != 0.9 || r[1].Index != 0 || len(calls) != 3 || calls[0].Model != "jev-fixed" || peak.Load() != 2 {
		t.Fatalf("results=%+v calls=%+v peak=%d err=%v", r, calls, peak.Load(), err)
	}
}

func TestJevRejectsInvalidAnswersAndDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing", `{"model":"j","answers":{}}`, 200},
		{"out_of_range", `{"model":"j","answers":{"d0":{"type":"noul","noul":1.1}}}`, 200},
		{"wrong_type", `{"model":"j","answers":{"d0":{"type":"choice","noul":0.8}}}`, 200},
		{"rate_limit", "secret", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c, _ := NewJevHTTP(s.URL, "secret", "m", time.Second, 1, 1, 1)
			_, err := c.Rerank(context.Background(), "q", []string{"d"}, 1)
			if err == nil || strings.Contains(err.Error(), "secret") || n.Load() != 1 {
				t.Fatalf("err=%v calls=%d", err, n.Load())
			}
		})
	}
}

func TestJevTop20SingleRequestAndSizeGuard(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			State struct {
				Passages map[string]string `json:"passages"`
			} `json:"state"`
			Questions map[string]struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.State.Passages) != 20 || len(req.Questions) != 20 {
			t.Errorf("passages=%d questions=%d", len(req.State.Passages), len(req.Questions))
		}
		answers := map[string]any{}
		for i := 0; i < 20; i++ {
			id := fmt.Sprintf("d%d", i)
			q := req.Questions[id]
			if q.Type != "noul" || !strings.Contains(q.Instructions, "`passages."+id+"`") || req.State.Passages[id] != strings.Repeat("x", 1500) {
				t.Errorf("incorrect mapping or truncated passage: %s", id)
			}
			answers[id] = map[string]any{"type": "noul", "noul": float64(i) / 20}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": "jev-test", "answers": answers})
	}))
	defer s.Close()
	c, err := NewJevHTTP(s.URL, "secret", "m", time.Second, 2, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	docs := make([]string, 20)
	for i := range docs {
		docs[i] = strings.Repeat("x", 1500)
	}
	r, calls, err := c.RankDetailed(context.Background(), "query", docs, 5)
	if err != nil || len(r) != 5 || len(calls) != 1 || requests.Load() != 1 {
		t.Fatalf("results=%v calls=%d requests=%d err=%v", r, len(calls), requests.Load(), err)
	}
	for i, result := range r {
		if result.Index != 19-i {
			t.Errorf("rank %d: got index %d", i, result.Index)
		}
	}
	docs[0] = strings.Repeat("x", JevMaxRequestBytes)
	if _, err := c.Rerank(context.Background(), "query", docs, 5); err == nil || !strings.Contains(err.Error(), "64KB") || requests.Load() != 1 {
		t.Fatalf("oversized request: err=%v requests=%d", err, requests.Load())
	}
}

func TestJevBudgetAndCancellation(t *testing.T) {
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
	}))
	defer s.Close()
	c, _ := NewJevHTTP(s.URL, "secret", "m", 30*time.Millisecond, 1, 1, 0.0000001)
	if _, err := c.Rerank(context.Background(), "q", []string{"d"}, 1); err == nil || n.Load() != 0 {
		t.Fatal("budget did not stop request")
	}
	c, _ = NewJevHTTP(s.URL, "secret", "m", 30*time.Millisecond, 1, 1, 1)
	if _, err := c.Rerank(context.Background(), "q", []string{"d"}, 1); err == nil {
		t.Fatal("deadline not enforced")
	}
}
