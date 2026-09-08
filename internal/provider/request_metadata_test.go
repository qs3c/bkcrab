package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestSessionScopeIsStableIsolatedAndOpaque(t *testing.T) {
	ctx := WithSession(context.Background(), "agent", "private-owner", "private-chat")
	id := SessionIDFromContext(ctx)
	if len(id) != len("bkcrab_")+64 || strings.Contains(id, "private") {
		t.Fatalf("session is not an opaque bounded ID: %q", id)
	}
	if SessionIDFromContext(WithSession(context.Background(), "agent", "private-owner", "private-chat")) != id {
		t.Fatal("same conversation changed its session ID")
	}
	for _, scope := range [][]string{
		{"agent", "other-owner", "private-chat"}, {"agent", "private-owner", "other-chat"},
		{"rag-eval", "private-owner", "private-chat"}, {"agent", "private-owner\x00private-chat"},
	} {
		if SessionIDFromContext(WithSession(ctx, scope...)) == id {
			t.Fatalf("different scope shared a session: %q", scope)
		}
	}
	child, cancel := context.WithCancel(EnsureSession(ctx))
	defer cancel()
	if SessionIDFromContext(child) != id {
		t.Fatal("nested operation lost conversation routing")
	}
	first := EnsureSession(context.Background())
	if SessionIDFromContext(first) == "" || SessionIDFromContext(EnsureSession(first)) != SessionIDFromContext(first) ||
		SessionIDFromContext(EnsureSession(context.Background())) == SessionIDFromContext(first) {
		t.Fatal("standalone operations must be stable within an operation and isolated across operations")
	}
}

func TestRequestMetadataIsOnlySentToOpenCode(t *testing.T) {
	ctx := WithSession(context.Background(), "test", "owner", "conversation")
	for _, host := range []string{"opencode.ai", "OPENCODE.AI:443", "api.openai.com", "opencode.ai.example.com", "example.com"} {
		t.Run(host, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer existing")
			ApplyRequestMetadata(req)
			got := req.Header.Get("x-opencode-session")
			if strings.EqualFold(req.URL.Hostname(), "opencode.ai") {
				if got != SessionIDFromContext(ctx) {
					t.Fatalf("session header=%q", got)
				}
			} else if got != "" {
				t.Fatal("OpenCode session leaked to another host")
			}
			if !strings.HasPrefix(req.UserAgent(), "BkCrab/") || req.Header.Get("Authorization") != "Bearer existing" {
				t.Fatal("missing client identity or changed authorization")
			}
		})
	}
}

type metadataRoundTripper func(*http.Request) (*http.Response, error)

func (fn metadataRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestOpenCodeMetadataAcrossChatAndStream(t *testing.T) {
	for _, api := range []string{"openai-chat", "anthropic-messages"} {
		t.Run(api, func(t *testing.T) {
			ctx := WithSession(context.Background(), "agent", "owner", "chat")
			calls := 0
			transport := metadataRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("x-opencode-session") != SessionIDFromContext(ctx) || !strings.HasPrefix(r.UserAgent(), "BkCrab/") {
					t.Errorf("missing metadata on %s", api)
				}
				body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
				if api == "anthropic-messages" {
					if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
						t.Error("Anthropic authentication changed")
					}
					body = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				} else if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("OpenAI authentication changed")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			p := NewProvider("test-key", "https://opencode.ai/zen/go/v1", api)
			switch client := p.(type) {
			case *OpenAIProvider:
				client.client.Transport = transport
			case *AnthropicProvider:
				client.client.Transport = transport
			}
			messages := []Message{{Role: "user", Content: "hi"}}
			answer, err := p.Chat(ctx, messages, nil, "test-model", 32, .1)
			if err != nil || answer.Content != "ok" {
				t.Fatalf("Chat: answer=%+v err=%v", answer, err)
			}
			stream, err := p.ChatStream(ctx, messages, nil, "test-model", 32, .1)
			if err != nil {
				t.Fatal(err)
			}
			for {
				if _, ok := stream.Next(); !ok {
					break
				}
			}
			if stream.Err() != nil || calls != 2 {
				t.Fatalf("stream: calls=%d err=%v", calls, stream.Err())
			}
		})
	}
}

func TestConcurrentRequestsDoNotShareMutableSession(t *testing.T) {
	p := NewOpenAI("key", "https://opencode.ai/zen/go/v1")
	var wg sync.WaitGroup
	for index := 0; index < 30; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx := WithSession(context.Background(), "owner", fmt.Sprint(index))
			for retry := 0; retry < 3; retry++ {
				req, err := p.buildRequest(ctx, nil, nil, "model", 16, 0, true)
				if err != nil || req.Header.Get("x-opencode-session") != SessionIDFromContext(ctx) {
					t.Errorf("session changed across concurrent requests: %v", err)
				}
			}
		}(index)
	}
	wg.Wait()
}
