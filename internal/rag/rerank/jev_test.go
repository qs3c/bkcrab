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
