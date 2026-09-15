package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qs3c/bkcrab/internal/observability"
)

func TestProviderOperationalMetrics(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, scenario := range []string{"stream", "buffered", "buffered-truncated", "rejected", "truncated", "canceled"} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				m := observability.New()
				observability.SetDefault(m)
				defer observability.SetDefault(nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if scenario == "rejected" {
						w.WriteHeader(429)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					if scenario == "canceled" {
						<-r.Context().Done()
						return
					}
					if protocol == "openai" {
						io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
						if scenario != "truncated" && scenario != "buffered-truncated" {
							io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
						}
					} else {
						io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hi\"}}\n\n")
						if scenario != "truncated" && scenario != "buffered-truncated" {
							io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
						}
					}
				}))
				defer srv.Close()
				var p Provider = NewOpenAI("test", srv.URL)
				if protocol == "anthropic" {
					p = NewAnthropic("test", srv.URL)
				}
				mode, outcome := "stream", "ok"
				if scenario == "buffered" || scenario == "buffered-truncated" {
					mode = "buffered"
					if scenario == "buffered-truncated" {
						outcome = "error"
					}
					if _, err := p.Chat(ctx, nil, nil, "private-model", 30, 0); err != nil {
						t.Fatal(err)
					}
				} else {
					r, err := p.ChatStream(ctx, nil, nil, "private-model", 30, 0)
					if scenario == "rejected" {
						if err == nil {
							t.Fatal("expected rejection")
						}
						outcome = "error"
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if scenario == "canceled" {
							cancel()
							outcome = "canceled"
						}
						for {
							if _, ok := r.Next(); !ok {
								break
							}
						}
						if scenario == "truncated" {
							outcome = "error"
						}
					}
				}
				if n := testutil.ToFloat64(m.Requests.WithLabelValues("llm", protocol, mode, outcome)); n != 1 {
					t.Fatalf("completion count = %v", n)
				}
				if n := testutil.ToFloat64(m.Inflight.WithLabelValues("llm", protocol, mode)); n != 0 {
					t.Fatalf("inflight leaked: %v", n)
				}
				if scenario == "stream" || scenario == "buffered" {
					if n := testutil.ToFloat64(m.Tokens.WithLabelValues(protocol, "input")); n != 7 {
						t.Fatalf("input tokens = %v", n)
					}
					if n := testutil.ToFloat64(m.Tokens.WithLabelValues(protocol, "output")); n != 3 {
						t.Fatalf("output tokens = %v", n)
					}
				}
			})
		}
	}
}
