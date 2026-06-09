package provider

import (
	"strings"
	"testing"
)

// TestOpenAIParseSSEUsage feeds a canned SSE stream through the OpenAI
// parser and checks Usage was extracted from the terminal include_usage
// chunk. This is the path goal-budget accounting + admin metering rely
// on for every web-chat streaming turn.
func TestOpenAIParseSSEUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":15,"prompt_tokens_details":{"cached_tokens":80}}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	p := &OpenAIProvider{}
	resp, err := p.parseSSE(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if resp.Content != "hello" {
		t.Errorf("content = %q, want %q", resp.Content, "hello")
	}
	// openaiUsageToProvider subtracts cached_tokens from prompt_tokens
	// to expose the uncached billable portion as InputTokens. So with
	// prompt_tokens=120 + cached=80 we expect InputTokens=40 and
	// CacheReadTokens=80.
	if resp.Usage.InputTokens != 40 {
		t.Errorf("InputTokens = %d, want 40 (120 prompt − 80 cached)", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 15 {
		t.Errorf("OutputTokens = %d, want 15", resp.Usage.OutputTokens)
	}
	if resp.Usage.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens = %d, want 80", resp.Usage.CacheReadTokens)
	}
}

// TestOpenAIParseSSENoUsage exercises the providers-don't-report-usage
// path (legacy endpoints, Ollama, etc.). Usage should land as the
// zero-value struct so goal-budget code can detect "can't measure"
// via an explicit zero check.
func TestOpenAIParseSSENoUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	p := &OpenAIProvider{}
	resp, err := p.parseSSE(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 ||
		resp.Usage.CacheReadTokens != 0 || resp.Usage.CacheCreationTokens != 0 {
		t.Errorf("Usage = %+v, want zero-value for streams that omit usage", resp.Usage)
	}
}

// TestAnthropicParseSSEUsage exercises the Anthropic SSE shape:
// usage rides on message_start (prompt + cache fields) and the final
// output_tokens count lands on message_delta.
func TestAnthropicParseSSEUsage(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","usage":{"input_tokens":50,"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	p := &AnthropicProvider{}
	resp, err := p.parseSSE(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("content = %q, want %q", resp.Content, "hi")
	}
	if resp.Usage.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, want 42 (from message_delta)", resp.Usage.OutputTokens)
	}
	if resp.Usage.CacheReadTokens != 30 {
		t.Errorf("CacheReadTokens = %d, want 30 (from message_start)", resp.Usage.CacheReadTokens)
	}
	if resp.Usage.CacheCreationTokens != 10 {
		t.Errorf("CacheCreationTokens = %d, want 10 (from message_start)", resp.Usage.CacheCreationTokens)
	}
}
