package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/document"
)

type phaseERAGChatStep struct {
	response provider.Response
	err      error
}

type phaseERAGStreamStep struct {
	chunks []provider.StreamChunk
	err    error
	before func()
}

type phaseERAGProvider struct {
	mu sync.Mutex

	chatSteps   []phaseERAGChatStep
	streamSteps []phaseERAGStreamStep
	chatCalls   int
	streamCalls int

	chatRequests   [][]provider.Message
	streamRequests [][]provider.Message
}

func clonePhaseEProviderMessages(messages []provider.Message) []provider.Message {
	out := append([]provider.Message(nil), messages...)
	for index := range out {
		out[index].ContentParts = append([]provider.ContentPart(nil), messages[index].ContentParts...)
		out[index].ToolCalls = append([]provider.ToolCall(nil), messages[index].ToolCalls...)
		if len(messages[index].Metadata) > 0 {
			out[index].Metadata = make(map[string]any, len(messages[index].Metadata))
			for key, value := range messages[index].Metadata {
				out[index].Metadata[key] = value
			}
		}
	}
	return out
}

func (p *phaseERAGProvider) Chat(_ context.Context, messages []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatRequests = append(p.chatRequests, clonePhaseEProviderMessages(messages))
	if p.chatCalls >= len(p.chatSteps) {
		return nil, fmt.Errorf("unexpected Chat call %d", p.chatCalls+1)
	}
	step := p.chatSteps[p.chatCalls]
	p.chatCalls++
	response := step.response
	return &response, step.err
}

func (p *phaseERAGProvider) ChatStream(_ context.Context, messages []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	p.mu.Lock()
	p.streamRequests = append(p.streamRequests, clonePhaseEProviderMessages(messages))
	if p.streamCalls >= len(p.streamSteps) {
		call := p.streamCalls + 1
		p.mu.Unlock()
		return nil, fmt.Errorf("unexpected ChatStream call %d", call)
	}
	step := p.streamSteps[p.streamCalls]
	p.streamCalls++
	p.mu.Unlock()
	if step.before != nil {
		step.before()
	}
	if step.err != nil {
		return nil, step.err
	}
	chunks := make(chan provider.StreamChunk, len(step.chunks))
	for _, chunk := range step.chunks {
		chunks <- chunk
	}
	close(chunks)
	return provider.NewStreamReader(chunks), nil
}

func phaseERAGToolCall(id, query string) provider.ToolCall {
	return provider.ToolCall{
		ID: id, Type: "function",
		Function: provider.FunctionCall{Name: "rag_search", Arguments: fmt.Sprintf(`{"query":%q}`, query)},
	}
}

func phaseERAGToolResult(t *testing.T, ids ...string) tools.ToolResult {
	t.Helper()
	location := document.SourceLocation{Kind: document.LocationDocument}
	refs := make([]rag.RAGResourceRef, 0, len(ids))
	for index, id := range ids {
		refs = append(refs, rag.RAGResourceRef{
			Asset: document.AssetRef{
				ID: id, Kind: document.AssetKindImage, Caption: "figure " + id,
				Location: location, MIMEType: "image/png",
			},
			KBID: "kb_1", KBName: "Manual", DocID: "doc_1", DocName: "manual.pdf",
			ChunkIndex: index, SourceLocation: location,
		})
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	return tools.ToolResult{
		Text:     "retrieved untrusted passages",
		Metadata: tools.ResultMetadata{tools.RAGResourcesMetadataKey: raw},
	}
}

func newPhaseERAGAgent(t *testing.T, scripted *phaseERAGProvider, maxIterations int, results map[string]tools.ToolResult, resultErrors map[string]error) *Agent {
	t.Helper()
	root := t.TempDir()
	ag := NewAgent(config.ResolvedAgent{
		ID: "phase-e-rag-agent", UserID: "owner", Model: "fake/model",
		Home: filepath.Join(root, "home"), Workspace: filepath.Join(root, "workspace"),
		PromptMode: config.PromptModeAgent, MaxTokens: 256, ContextWindow: 100000,
		MaxToolIterations: maxIterations,
	}, scripted, nil, root)
	ag.ToolRegistry().RegisterResult("rag_search", "read-only RAG", map[string]any{
		"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		"required": []string{"query"},
	}, func(_ context.Context, raw json.RawMessage) (tools.ToolResult, error) {
		var args struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return tools.ToolResult{}, err
		}
		result := results[args.Query]
		result.Metadata = result.Metadata.Clone()
		return result, resultErrors[args.Query]
	})
	return ag
}

func phaseEStoredMessages(ag *Agent, channel, chatID string) []provider.Message {
	return ag.sessions.Get(channel, "", chatID, "").GetMessages()
}

func phaseEOnlyPersistedSessionMessages(t *testing.T, ag *Agent) []provider.Message {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(ag.homePath, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("persisted session files = %v, want exactly one", paths)
	}
	key := strings.TrimSuffix(filepath.Base(paths[0]), ".jsonl")
	return ag.sessions.GetByKey(key).GetMessages()
}

func phaseEFinalAssistant(messages []provider.Message, content string) *provider.Message {
	for index := range messages {
		if messages[index].Role == "assistant" && len(messages[index].ToolCalls) == 0 && messages[index].Content == content {
			return &messages[index]
		}
	}
	return nil
}

func phaseEResourceIDs(t *testing.T, metadata map[string]any) []string {
	t.Helper()
	value, ok := metadata[tools.RAGResourcesMetadataKey]
	if !ok {
		return nil
	}
	blob, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var refs []rag.RAGResourceRef
	if err := json.Unmarshal(blob, &refs); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.Asset.ID)
	}
	return ids
}

func drainPhaseEStream(reader *provider.StreamReader) string {
	var reply strings.Builder
	for {
		chunk, ok := reader.Next()
		if !ok {
			return reply.String()
		}
		reply.WriteString(chunk.Content)
	}
}

func drainPhaseEEvents(channel chan ChatEvent) []ChatEvent {
	close(channel)
	var events []ChatEvent
	for event := range channel {
		events = append(events, event)
	}
	return events
}

func assertPhaseEMetadataEvent(t *testing.T, events []ChatEvent, content string, want map[string]any) {
	t.Helper()
	for _, event := range events {
		if event.Type != "content" || event.Data["content"] != content {
			continue
		}
		got, _ := event.Data["metadata"].(map[string]any)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("content event metadata = %s, want %s", gotJSON, wantJSON)
		}
		return
	}
	t.Fatalf("missing content event %q with assistant metadata", content)
}

func TestHandleMessageAggregatesRAGResourcesWithoutToolLeakage(t *testing.T) {
	ids := []string{
		"ast_00000000000000000000000000000001", "ast_00000000000000000000000000000002",
		"ast_00000000000000000000000000000003", "ast_00000000000000000000000000000004",
		"ast_00000000000000000000000000000005", "ast_00000000000000000000000000000006",
		"ast_00000000000000000000000000000007", "ast_00000000000000000000000000000008",
		"ast_00000000000000000000000000000009",
	}
	providerScript := &phaseERAGProvider{streamSteps: []phaseERAGStreamStep{
		{chunks: []provider.StreamChunk{{ToolCalls: []provider.ToolCall{
			phaseERAGToolCall("call_1", "first"), phaseERAGToolCall("call_2", "second"), phaseERAGToolCall("call_error", "failed"),
		}, Done: true}}},
		{chunks: []provider.StreamChunk{{Content: "first answer", Done: true}}},
		{chunks: []provider.StreamChunk{{Content: "second answer", Done: true}}},
	}}
	ag := newPhaseERAGAgent(t, providerScript, 6, map[string]tools.ToolResult{
		"first":  phaseERAGToolResult(t, ids[0], ids[1], ids[2], ids[3]),
		"second": phaseERAGToolResult(t, ids[2], ids[4], ids[5], ids[6], ids[7]),
		"failed": phaseERAGToolResult(t, ids[8]),
	}, map[string]error{"failed": errors.New("retrieval failed")})

	eventChannel := make(chan ChatEvent, 128)
	ctx := ContextWithChatEvents(context.Background(), eventChannel)
	reply := ag.HandleMessage(ctx, bus.InboundMessage{
		Channel: "web", ChatID: "rag-regular", UserID: "visitor", Text: "show the diagrams",
		PhotoURLs: []string{"data:image/png;base64,USER_UPLOAD"},
	})
	if reply != "first answer" {
		t.Fatalf("reply = %q", reply)
	}
	// A second turn proves persisted assistant metadata is omitted from the next
	// provider request while the original stored message remains unchanged.
	if got := ag.HandleMessage(ctx, bus.InboundMessage{Channel: "web", ChatID: "rag-regular", UserID: "visitor", Text: "continue"}); got != "second answer" {
		t.Fatalf("second reply = %q", got)
	}
	events := drainPhaseEEvents(eventChannel)

	messages := phaseEStoredMessages(ag, "web", "rag-regular")
	final := phaseEFinalAssistant(messages, "first answer")
	if final == nil {
		t.Fatal("stored final assistant message not found")
	}
	wantIDs := ids[:maxAssistantRAGResources]
	if got := phaseEResourceIDs(t, final.Metadata); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("aggregated resources = %v, want %v", got, wantIDs)
	}
	assertPhaseEMetadataEvent(t, events, "first answer", final.Metadata)
	historyMatched := false
	for _, entry := range ag.WebChatHistory("rag-regular") {
		if entry["role"] != "assistant" || entry["content"] != "first answer" {
			continue
		}
		historyMetadata, _ := entry["metadata"].(map[string]any)
		gotJSON, _ := json.Marshal(historyMetadata)
		wantJSON, _ := json.Marshal(final.Metadata)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("history metadata = %s, SSE/stored metadata = %s", gotJSON, wantJSON)
		}
		historyMatched = true
		break
	}
	if !historyMatched {
		t.Fatal("history reload did not surface the RAG assistant metadata")
	}

	toolMessageCount := 0
	for _, message := range messages {
		if message.Role != "tool" {
			continue
		}
		toolMessageCount++
		if _, leaked := message.Metadata[tools.RAGResourcesMetadataKey]; leaked {
			t.Fatalf("typed RAG metadata leaked into stored tool message: %#v", message.Metadata)
		}
		if len(message.ContentParts) != 0 {
			t.Fatalf("RAG tool result created content parts: %#v", message.ContentParts)
		}
	}
	if toolMessageCount != 3 {
		t.Fatalf("stored tool message count = %d, want 3", toolMessageCount)
	}
	for _, event := range events {
		if event.Type != "tool_result" {
			continue
		}
		metadata, _ := event.Data["metadata"].(map[string]any)
		if _, leaked := metadata[tools.RAGResourcesMetadataKey]; leaked {
			t.Fatalf("typed RAG metadata leaked into tool_result SSE: %#v", event.Data)
		}
	}

	var firstUser *provider.Message
	for index := range messages {
		if messages[index].Role == "user" && messages[index].TextContent() == "show the diagrams" {
			firstUser = &messages[index]
			break
		}
	}
	if firstUser == nil || len(firstUser.ContentParts) != 2 || firstUser.ContentParts[1].ImageURL == nil ||
		firstUser.ContentParts[1].ImageURL.URL != "data:image/png;base64,USER_UPLOAD" {
		t.Fatalf("user upload content parts changed: %#v", firstUser)
	}
	if len(final.ContentParts) != 0 {
		t.Fatalf("RAG references were injected as assistant content parts: %#v", final.ContentParts)
	}

	providerScript.mu.Lock()
	requests := append([][]provider.Message(nil), providerScript.streamRequests...)
	providerScript.mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("provider stream request count = %d, want 3", len(requests))
	}
	sawPriorFinal := false
	for _, message := range requests[2] {
		if len(message.Metadata) != 0 {
			t.Fatalf("application metadata reached next-turn provider history: %#v", message.Metadata)
		}
		if message.Role == "assistant" && message.Content == "first answer" {
			sawPriorFinal = true
		}
		if message.Role == "user" && message.TextContent() == "show the diagrams" {
			if len(message.ContentParts) != 2 || message.ContentParts[1].ImageURL == nil || message.ContentParts[1].ImageURL.URL != "data:image/png;base64,USER_UPLOAD" {
				t.Fatalf("provider history lost user content parts: %#v", message.ContentParts)
			}
		}
	}
	if !sawPriorFinal {
		t.Fatal("next provider request did not contain the prior assistant answer")
	}
	if got := phaseEResourceIDs(t, final.Metadata); !reflect.DeepEqual(got, wantIDs) {
		t.Fatalf("provider boundary mutated stored assistant metadata: %v", got)
	}
}

func TestExtractToolMetaTrustsOnlySandboxBuiltins(t *testing.T) {
	prefixed := tools.MetaSandboxPrefix + "output"
	for _, name := range []string{"exec", "read_file", "write_file", "list_dir", "edit_file", "apply_patch"} {
		t.Run("trusted_"+name, func(t *testing.T) {
			reg := tools.NewRegistry("", "")
			reg.Register(name, "", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) { return "", nil })
			content, metadata := extractToolMeta(reg, name, prefixed)
			if content != "output" || metadata["sandbox"] != true {
				t.Fatalf("trusted builtin result = %q %#v", content, metadata)
			}
		})
	}

	cases := []struct {
		name   string
		source tools.ToolSource
	}{
		{name: "rag_search", source: tools.SourceBuiltin},
		{name: "web_fetch", source: tools.SourceBuiltin},
		{name: "exec", source: tools.SourceMCP},
		{name: "write_file", source: tools.SourcePlugin},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("untrusted_%s_%d", testCase.name, testCase.source), func(t *testing.T) {
			reg := tools.NewRegistry("", "")
			reg.RegisterFrom(testCase.name, "", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) { return "", nil }, testCase.source)
			content, metadata := extractToolMeta(reg, testCase.name, prefixed)
			if content != prefixed || metadata != nil {
				t.Fatalf("untrusted result was decoded: %q %#v", content, metadata)
			}
		})
	}
}

func TestProviderMetadataBoundaryPreservesOriginalContentParts(t *testing.T) {
	original := []provider.Message{{
		Role: "user", Metadata: map[string]any{"ragResources": []string{"private"}},
		ContentParts: []provider.ContentPart{{Type: "text", Text: "hello"}, {Type: "image_url", ImageURL: &provider.ImageURL{URL: "data:image/png;base64,USER"}}},
	}}
	got := providerRequestMessages(original)
	if len(got[0].Metadata) != 0 {
		t.Fatalf("provider request metadata = %#v, want nil", got[0].Metadata)
	}
	if !reflect.DeepEqual(got[0].ContentParts, original[0].ContentParts) {
		t.Fatalf("content parts changed: got %#v want %#v", got[0].ContentParts, original[0].ContentParts)
	}
	if len(original[0].Metadata) == 0 {
		t.Fatal("provider boundary mutated the stored/original message")
	}
	capture := &phaseERAGProvider{chatSteps: []phaseERAGChatStep{{response: provider.Response{Content: "summary"}}}}
	wrapped := providerWithoutMessageMetadata(capture)
	if _, err := wrapped.Chat(context.Background(), original, nil, "fake/model", 1, 0); err != nil {
		t.Fatal(err)
	}
	if len(capture.chatRequests) != 1 || len(capture.chatRequests[0][0].Metadata) != 0 {
		t.Fatalf("compaction/provider wrapper leaked metadata: %#v", capture.chatRequests)
	}
	if len(original[0].Metadata) == 0 {
		t.Fatal("compaction/provider wrapper removed metadata from the retained original tail")
	}
}

func TestHandleMessageStreamAttachesRAGMetadataOnFinalAndFallback(t *testing.T) {
	assetID := "ast_10000000000000000000000000000001"
	for _, testCase := range []struct {
		name        string
		streamStep  phaseERAGStreamStep
		probeText   string
		wantContent string
	}{
		{
			name: "stream final", probeText: "probe",
			streamStep:  phaseERAGStreamStep{chunks: []provider.StreamChunk{{Content: "streamed answer", Done: true}}},
			wantContent: "streamed answer",
		},
		{
			name: "stream startup fallback", probeText: "fallback answer",
			streamStep:  phaseERAGStreamStep{err: errors.New("stream unavailable")},
			wantContent: "fallback answer",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			providerScript := &phaseERAGProvider{
				chatSteps: []phaseERAGChatStep{
					{response: provider.Response{ToolCalls: []provider.ToolCall{phaseERAGToolCall("call_stream", "one")}}},
					{response: provider.Response{Content: testCase.probeText}},
				},
				streamSteps: []phaseERAGStreamStep{testCase.streamStep},
			}
			ag := newPhaseERAGAgent(t, providerScript, 4, map[string]tools.ToolResult{
				"one": phaseERAGToolResult(t, assetID),
			}, nil)
			eventChannel := make(chan ChatEvent, 64)
			reader := ag.HandleMessageStream(ContextWithChatEvents(context.Background(), eventChannel), bus.InboundMessage{
				Channel: "web", ChatID: "rag-stream", UserID: "visitor", Text: "answer",
			})
			if got := drainPhaseEStream(reader); got != testCase.wantContent {
				t.Fatalf("stream reply = %q, want %q", got, testCase.wantContent)
			}
			events := drainPhaseEEvents(eventChannel)
			final := phaseEFinalAssistant(phaseEStoredMessages(ag, "web", "rag-stream"), testCase.wantContent)
			if final == nil {
				t.Fatal("stored stream final message not found")
			}
			if got := phaseEResourceIDs(t, final.Metadata); !reflect.DeepEqual(got, []string{assetID}) {
				t.Fatalf("stream final resources = %v", got)
			}
			assertPhaseEMetadataEvent(t, events, "", final.Metadata)
			for _, request := range append(providerScript.chatRequests, providerScript.streamRequests...) {
				for _, message := range request {
					if len(message.Metadata) != 0 {
						t.Fatalf("metadata reached streaming provider request: %#v", message.Metadata)
					}
				}
			}
		})
	}
}

func TestHandleMessageAttachesRAGMetadataAtIterationCap(t *testing.T) {
	assetID := "ast_20000000000000000000000000000001"
	for _, testCase := range []struct {
		name        string
		finalStep   phaseERAGStreamStep
		wantContent string
	}{
		{
			name:        "forced delivery succeeds",
			finalStep:   phaseERAGStreamStep{chunks: []provider.StreamChunk{{Content: "synthesized at cap", Done: true}}},
			wantContent: "synthesized at cap",
		},
		{
			name:        "forced delivery falls back",
			finalStep:   phaseERAGStreamStep{err: errors.New("final failed")},
			wantContent: "I've reached the maximum number of tool iterations (1) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			providerScript := &phaseERAGProvider{streamSteps: []phaseERAGStreamStep{
				{chunks: []provider.StreamChunk{{ToolCalls: []provider.ToolCall{phaseERAGToolCall("call_cap", "one")}, Done: true}}},
				testCase.finalStep,
			}}
			ag := newPhaseERAGAgent(t, providerScript, 1, map[string]tools.ToolResult{"one": phaseERAGToolResult(t, assetID)}, nil)
			eventChannel := make(chan ChatEvent, 64)
			got := ag.HandleMessage(ContextWithChatEvents(context.Background(), eventChannel), bus.InboundMessage{
				Channel: "web", ChatID: "rag-regular-cap", UserID: "visitor", Text: "answer",
			})
			if got != testCase.wantContent {
				t.Fatalf("cap reply = %q, want %q", got, testCase.wantContent)
			}
			events := drainPhaseEEvents(eventChannel)
			final := phaseEFinalAssistant(phaseEStoredMessages(ag, "web", "rag-regular-cap"), testCase.wantContent)
			if final == nil {
				t.Fatal("stored cap final message not found")
			}
			if final.Metadata["iterationCapReached"] != true || !reflect.DeepEqual(phaseEResourceIDs(t, final.Metadata), []string{assetID}) {
				t.Fatalf("cap metadata = %#v", final.Metadata)
			}
			assertPhaseEMetadataEvent(t, events, testCase.wantContent, final.Metadata)
		})
	}
}

func TestHandleMessageStreamAttachesRAGMetadataAtIterationCap(t *testing.T) {
	assetID := "ast_30000000000000000000000000000001"
	for _, testCase := range []struct {
		name          string
		finalStep     phaseERAGStreamStep
		wantContent   string
		metadataEvent string
	}{
		{
			name:        "stream cap delivery succeeds",
			finalStep:   phaseERAGStreamStep{chunks: []provider.StreamChunk{{Content: "stream cap answer", Done: true}}},
			wantContent: "stream cap answer", metadataEvent: "",
		},
		{
			name:          "stream cap delivery falls back",
			finalStep:     phaseERAGStreamStep{err: errors.New("stream cap failed")},
			wantContent:   "I've reached the maximum number of tool iterations (1) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.",
			metadataEvent: "I've reached the maximum number of tool iterations (1) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			providerScript := &phaseERAGProvider{
				chatSteps:   []phaseERAGChatStep{{response: provider.Response{ToolCalls: []provider.ToolCall{phaseERAGToolCall("call_stream_cap", "one")}}}},
				streamSteps: []phaseERAGStreamStep{testCase.finalStep},
			}
			ag := newPhaseERAGAgent(t, providerScript, 1, map[string]tools.ToolResult{"one": phaseERAGToolResult(t, assetID)}, nil)
			eventChannel := make(chan ChatEvent, 64)
			reader := ag.HandleMessageStream(ContextWithChatEvents(context.Background(), eventChannel), bus.InboundMessage{
				Channel: "web", ChatID: "rag-stream-cap", UserID: "visitor", Text: "answer",
			})
			if got := drainPhaseEStream(reader); got != testCase.wantContent {
				t.Fatalf("stream cap reply = %q, want %q", got, testCase.wantContent)
			}
			events := drainPhaseEEvents(eventChannel)
			final := phaseEFinalAssistant(phaseEStoredMessages(ag, "web", "rag-stream-cap"), testCase.wantContent)
			if final == nil {
				t.Fatal("stored stream cap final message not found")
			}
			if final.Metadata["iterationCapReached"] != true || !reflect.DeepEqual(phaseEResourceIDs(t, final.Metadata), []string{assetID}) {
				t.Fatalf("stream cap metadata = %#v", final.Metadata)
			}
			assertPhaseEMetadataEvent(t, events, testCase.metadataEvent, final.Metadata)
		})
	}
}

func TestHandleMessageSteerCarriesForwardRAGResources(t *testing.T) {
	firstID := "ast_40000000000000000000000000000001"
	secondID := "ast_40000000000000000000000000000002"
	providerScript := &phaseERAGProvider{}
	var ag *Agent
	steered := false
	providerScript.streamSteps = []phaseERAGStreamStep{
		{chunks: []provider.StreamChunk{{ToolCalls: []provider.ToolCall{phaseERAGToolCall("call_before_steer", "first")}, Done: true}}},
		{before: func() {
			steered = ag.SteerInbound(bus.InboundMessage{Channel: "web", ChatID: "rag-steer", UserID: "visitor"}, "also inspect the second diagram")
		}, chunks: []provider.StreamChunk{{Content: "answer before steer", Done: true}}},
		{chunks: []provider.StreamChunk{{ToolCalls: []provider.ToolCall{phaseERAGToolCall("call_after_steer", "second")}, Done: true}}},
		{chunks: []provider.StreamChunk{{Content: "answer after steer", Done: true}}},
	}
	ag = newPhaseERAGAgent(t, providerScript, 6, map[string]tools.ToolResult{
		"first": phaseERAGToolResult(t, firstID), "second": phaseERAGToolResult(t, secondID),
	}, nil)
	_ = ag.HandleMessage(context.Background(), bus.InboundMessage{Channel: "web", ChatID: "rag-steer", UserID: "visitor", Text: "inspect"})
	if !steered {
		t.Fatal("steer was not accepted during active turn")
	}
	messages := phaseEStoredMessages(ag, "web", "rag-steer")
	before := phaseEFinalAssistant(messages, "answer before steer")
	after := phaseEFinalAssistant(messages, "answer after steer")
	if before == nil || after == nil {
		t.Fatalf("steer final messages missing: before=%#v after=%#v", before, after)
	}
	if got := phaseEResourceIDs(t, before.Metadata); !reflect.DeepEqual(got, []string{firstID}) {
		t.Fatalf("pre-steer resources = %v", got)
	}
	if got := phaseEResourceIDs(t, after.Metadata); !reflect.DeepEqual(got, []string{firstID, secondID}) {
		t.Fatalf("post-steer cumulative resources = %v", got)
	}
	providerScript.mu.Lock()
	requests := append([][]provider.Message(nil), providerScript.streamRequests...)
	providerScript.mu.Unlock()
	if len(requests) < 3 {
		t.Fatalf("provider requests = %d, want at least 3", len(requests))
	}
	for _, message := range requests[2] {
		if len(message.Metadata) != 0 {
			t.Fatalf("steer continuation provider saw assistant metadata: %#v", message.Metadata)
		}
	}
}

func TestHandleMessageIgnoresRAGMetadataOutsideWebAndWithoutResources(t *testing.T) {
	assetID := "ast_50000000000000000000000000000001"
	for _, testCase := range []struct {
		name    string
		channel string
		result  tools.ToolResult
	}{
		{name: "non-web channel", channel: "telegram", result: phaseERAGToolResult(t, assetID)},
		{name: "web result without resources", channel: "web", result: tools.ToolResult{Text: "no matching image"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			providerScript := &phaseERAGProvider{streamSteps: []phaseERAGStreamStep{
				{chunks: []provider.StreamChunk{{ToolCalls: []provider.ToolCall{phaseERAGToolCall("call_no_gallery", "one")}, Done: true}}},
				{chunks: []provider.StreamChunk{{Content: "plain answer", Done: true}}},
			}}
			ag := newPhaseERAGAgent(t, providerScript, 3, map[string]tools.ToolResult{"one": testCase.result}, nil)
			eventChannel := make(chan ChatEvent, 32)
			got := ag.HandleMessage(ContextWithChatEvents(context.Background(), eventChannel), bus.InboundMessage{
				Channel: testCase.channel, ChatID: "rag-no-gallery", UserID: "visitor", Text: "answer",
			})
			if got != "plain answer" {
				t.Fatalf("reply = %q", got)
			}
			events := drainPhaseEEvents(eventChannel)
			var messages []provider.Message
			if testCase.channel == "web" {
				messages = phaseEStoredMessages(ag, testCase.channel, "rag-no-gallery")
			} else {
				messages = phaseEOnlyPersistedSessionMessages(t, ag)
			}
			final := phaseEFinalAssistant(messages, "plain answer")
			if final == nil || len(final.Metadata) != 0 {
				t.Fatalf("final metadata = %#v, want unchanged empty behavior", final)
			}
			for _, event := range events {
				if event.Type == "content" && event.Data["content"] == "plain answer" {
					if _, exists := event.Data["metadata"]; exists {
						t.Fatalf("plain final SSE unexpectedly has metadata: %#v", event.Data)
					}
				}
			}
		})
	}
}
