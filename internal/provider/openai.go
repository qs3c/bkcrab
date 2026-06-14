package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// OpenAIProvider 为 OpenAI 兼容 API 实现 Provider 接口。
type OpenAIProvider struct {
	apiKey  string
	apiBase string
	client  *http.Client
}

// NewOpenAI 创建一个新的 OpenAI 兼容提供者。apiBase 按原样使用——
// 操作员配置的 URL 是唯一的事实来源。
// 当 apiBase 为空时，我们故意不默认使用 "https://api.openai.com/v1"：
// 那个静默默认值导致了当用户只配置了 deepseek 提供者但解析路径
// 选择了空配置时，"为什么会调用 OpenAI" 的谜团。
// 现在空的 apiBase 会导致调用显式失败，这正是我们想要的。
func NewOpenAI(apiKey, apiBase string) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey:  apiKey,
		apiBase: NormalizeAPIBase(apiBase, "openai-chat"),
		client:  newLLMHTTPClient(),
	}
}

// apiMessage 是发送给 OpenAI API 的消息的线路格式。
// 它对 Content 使用 json.RawMessage 以支持字符串和数组两种格式。
//
// ReasoningContent 是 DeepSeek 的思考模式字段。DeepSeek 要求
// 在后续轮次中回显此字段，否则会返回
// `invalid_request_error: The reasoning_content in the thinking mode
// must be passed back to the API.` 纯 OpenAI 忽略未知字段，
// 所以 omitempty 使非 DeepSeek 提供者不受影响。
type apiMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
}

type chatRequest struct {
	Model         string            `json:"model"`
	Messages      []json.RawMessage `json:"messages"`
	Tools         []Tool            `json:"tools,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Temperature   float64           `json:"temperature,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
}

// streamOptions.include_usage 告诉 OpenAI 兼容 API 在 [DONE] 之前
// 发出一个携带总令牌计数的最终数据块。没有这个标志，
// 流式路径完全不返回使用量，这会破坏每轮目标令牌核算
// 和管理员计量。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// sseUsage 镜像了在设置 stream_options.include_usage 时最终 SSE 数据块
// 返回的 `usage` 块。某些 OpenAI 兼容 API（例如 DeepSeek）通过
// prompt_tokens_details 暴露提示缓存命中/未命中令牌——
// 我们捕获它们，以便管理员计量在需要时可以将缓存读取
// 从 input_tokens 中分离出来。
type sseUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

// toAPIMessages 将提供者 Messages 转换为线路格式的 apiMessages，
// 处理多模态消息的 ContentParts。
//
// 防御性清理：OpenAI/DeepSeek 拒绝任何带有 `tool_calls` 的助手消息
// 没有紧随其后的回答每个 tool_call_id 的 `tool` 消息的请求。
// 脏会话可能使我们处于这种状态——例如，代理的工具循环检测器在
// loop.go:1218 追加了助手的 tool_calls，然后跳出循环而
// 未执行工具，留下孤儿。修复流式 RawAssistant 之前的旧会话
// 也可能在持久化的 RawAssistant 中携带孤立的 tool_calls。
// 这两种情况过去都表现为 `An assistant message with 'tool_calls'
// must be followed by tool messages`。
// 我们在构建线路格式时剥离有问题的 tool_calls（以及任何孤立的工具回复），
// 以便请求能够通过——会话保持其历史记录不变。
func toAPIMessages(msgs []Message) []json.RawMessage {
	orphanAssistant, orphanTool := findOrphanToolCalls(msgs)
	out := make([]json.RawMessage, 0, len(msgs))
	for i, m := range msgs {
		if orphanTool[i] {
			continue
		}

		// 对于带有缓存的原始 JSON 的助手消息，直接使用它
		// 以保证提示缓存命中（字节相同的前缀）——
		// 除非缓存的消息包含孤立的 tool_calls，
		// 在这种情况下重新构建时不包含它们。
		if m.Role == "assistant" && len(m.RawAssistant) > 0 && !orphanAssistant[i] {
			out = append(out, m.RawAssistant)
			continue
		}

		am := apiMessage{
			Role:       m.Role,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if !orphanAssistant[i] {
			am.ToolCalls = m.ToolCalls
		}
		if len(m.ContentParts) > 0 {
			am.Content, _ = json.Marshal(m.ContentParts)
		} else {
			am.Content, _ = json.Marshal(m.Content)
		}
		raw, _ := json.Marshal(am)
		out = append(out, raw)
	}
	return out
}

// findOrphanToolCalls 遍历 msgs 并标记那些声明的 tool_calls 未被
// 紧随其后的工具消息完全回答的助手消息，
// 以及在剥离后会悬空的工具消息。
// `m.ToolCalls` 和嵌入在 `m.RawAssistant` 中的 tool_calls
// 都会被考虑，因为旧会话只在原始 JSON 中存储它们。
func findOrphanToolCalls(msgs []Message) (orphanAssistant, orphanTool map[int]bool) {
	orphanAssistant = map[int]bool{}
	orphanTool = map[int]bool{}
	for i, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		want := assistantToolCallIDs(m)
		if len(want) == 0 {
			continue
		}
		// 从紧随其后的工具消息序列中收集 ID。
		got := map[string]bool{}
		j := i + 1
		for j < len(msgs) && msgs[j].Role == "tool" {
			if id := msgs[j].ToolCallID; id != "" {
				got[id] = true
			}
			j++
		}
		missing := false
		for _, id := range want {
			if !got[id] {
				missing = true
				break
			}
		}
		if !missing {
			continue
		}
		orphanAssistant[i] = true
		// 丢弃任何引用了这个助手（现已移除）的 tool_calls 的工具消息，
		// 以免 API 将它们作为悬空的工具回复拒绝。
		wantSet := map[string]bool{}
		for _, id := range want {
			wantSet[id] = true
		}
		for k := i + 1; k < j; k++ {
			if wantSet[msgs[k].ToolCallID] {
				orphanTool[k] = true
			}
		}
	}
	return
}

// assistantToolCallIDs 从存储的助手消息中提取 tool_call ID，
// 同时检查解析后的 ToolCalls 字段和嵌入在原始 JSON 中的
// tool_calls（旧会话流式传输消息时只在 RawAssistant 中携带 ID）。
func assistantToolCallIDs(m Message) []string {
	if len(m.ToolCalls) > 0 {
		ids := make([]string, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			ids = append(ids, tc.ID)
		}
		return ids
	}
	if len(m.RawAssistant) == 0 {
		return nil
	}
	var raw struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(m.RawAssistant, &raw); err != nil || len(raw.ToolCalls) == 0 {
		return nil
	}
	ids := make([]string, 0, len(raw.ToolCalls))
	for _, tc := range raw.ToolCalls {
		ids = append(ids, tc.ID)
	}
	return ids
}

// sseDelta 镜像 OpenAI 流式 delta 结构，包括工具调用索引。
type sseToolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

type sseDelta struct {
	Role             string             `json:"role,omitempty"`
	Content          string             `json:"content,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []sseToolCallDelta `json:"tool_calls,omitempty"`
}

type sseChoice struct {
	Delta        sseDelta `json:"delta"`
	FinishReason string   `json:"finish_reason"`
}

type sseResponse struct {
	Choices []sseChoice `json:"choices"`
	Usage   *sseUsage   `json:"usage,omitempty"` // 仅在 include_usage=true 时的最终数据块上出现
}

func (p *OpenAIProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64, stream bool) (*http.Request, error) {
	req := chatRequest{
		Model:       StripProviderPrefix(model),
		Messages:    toAPIMessages(messages),
		MaxTokens:   maxTokens,
		Temperature: temperature,
		Stream:      stream,
	}
	if stream {
		// include_usage 添加一个携带调用令牌计数的终端数据块。
		// 对于管理员计量和每轮流式调用的目标令牌预算核算都是必需的。
		// 不支持此标志的提供者会静默忽略它。
		req.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if len(tools) > 0 {
		req.Tools = tools
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.apiBase + "/chat/completions"
	slog.Info("openai request", "url", url, "model", req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	return httpReq, nil
}

func (p *OpenAIProvider) Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error) {
	httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	return p.parseSSE(resp.Body)
}

// ChatStream 返回一个 StreamReader，在数据块从 LLM 到达时产生它们。
func (p *OpenAIProvider) ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error) {
	httpReq, err := p.buildRequest(ctx, messages, tools, model, maxTokens, temperature, true)
	if err != nil {
		return nil, err
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamChunk, 64)
	reader := NewStreamReader(ch)

	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		toolCalls := make(map[int]*ToolCall)
		var contentBuilder, reasoningBuilder strings.Builder
		var usage Usage

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				// 发送包含累积的工具调用和完整 RawAssistant 的最终数据块。
				// DeepSeek 思考模式要求 reasoning_content 在下一轮中往返
				//（否则 API 会以 400 拒绝），因此我们在此处序列化
				// OpenAI 线路格式的助手消息，并让调用者原样持久化它。
				//
				// tool_calls 必须包含在内。streamChatToResponse
				// 对每个模型调用都使用 ChatStream（包括工具迭代），
				// 以便实时 Web UI 可以渲染文本增量——
				// 当模型发出 tool_calls 时，线路格式的 RawAssistant
				// 也必须携带它们，否则下一轮的 API 调用会发送
				// `assistant.RawAssistant`（无 tool_calls）后跟工具回复，
				// 而 OpenAI 会以 400 拒绝："Messages with role 'tool'
				// must be a response to a preceding message with 'tool_calls'"。
				var tcs []ToolCall
				for i := 0; i < len(toolCalls); i++ {
					if tc, ok := toolCalls[i]; ok {
						tcs = append(tcs, *tc)
					}
				}
				reasoning := reasoningBuilder.String()
				rawMsg := apiMessage{
					Role:             "assistant",
					ToolCalls:        tcs,
					ReasoningContent: reasoning,
				}
				rawMsg.Content, _ = json.Marshal(contentBuilder.String())
				raw, _ := json.Marshal(rawMsg)
				select {
				case ch <- StreamChunk{
					ToolCalls:    tcs,
					Done:         true,
					Thinking:     reasoning,
					Usage:        usage,
					RawAssistant: raw,
				}:
				case <-ctx.Done():
				}
				return
			}

			var chunk sseResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				slog.Warn("parse SSE chunk", "error", err, "data", data)
				continue
			}

			// 当 stream_options.include_usage=true 时，Usage 位于
			// 带有空 choices 的终端数据块上。捕获它以便 [DONE]
			// 数据块在 StreamChunk.Usage 上恰好发出一次。
			if chunk.Usage != nil {
				usage = openaiUsageToProvider(chunk.Usage)
			}

			if len(chunk.Choices) == 0 {
				continue
			}

			delta := chunk.Choices[0].Delta

			// 累积工具调用
			for _, tc := range delta.ToolCalls {
				existing, ok := toolCalls[tc.Index]
				if !ok {
					toolCalls[tc.Index] = &ToolCall{
						ID:   tc.ID,
						Type: tc.Type,
						Function: FunctionCall{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
				} else {
					if tc.ID != "" {
						existing.ID = tc.ID
					}
					if tc.Type != "" {
						existing.Type = tc.Type
					}
					if tc.Function.Name != "" {
						existing.Function.Name += tc.Function.Name
					}
					existing.Function.Arguments += tc.Function.Arguments
				}
			}

			if delta.ReasoningContent != "" {
				reasoningBuilder.WriteString(delta.ReasoningContent)
			}

			// 产生内容数据块
			if delta.Content != "" {
				contentBuilder.WriteString(delta.Content)
				select {
				case ch <- StreamChunk{Content: delta.Content}:
				case <-ctx.Done():
					return
				}
			}
		}

		if err := scanner.Err(); err != nil {
			reader.SetErr(fmt.Errorf("read stream: %w", err))
		}
	}()

	return reader, nil
}

func (p *OpenAIProvider) parseSSE(reader io.Reader) (*Response, error) {
	scanner := bufio.NewScanner(reader)
	// 增加缓冲区大小以处理大型 SSE 数据块
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var contentBuilder strings.Builder
	var reasoningBuilder strings.Builder
	toolCalls := make(map[int]*ToolCall)
	var usage Usage

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk sseResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			slog.Warn("parse SSE chunk", "error", err, "data", data)
			continue
		}

		// 当 stream_options.include_usage=true 时，流式使用量在
		// choices=[] 的终端数据块上到达。非流式数据块也携带它；
		// 两种路径都汇集于此。
		if chunk.Usage != nil {
			usage = openaiUsageToProvider(chunk.Usage)
		}

		if len(chunk.Choices) == 0 {
			continue
		}

		delta := chunk.Choices[0].Delta

		if delta.Content != "" {
			contentBuilder.WriteString(delta.Content)
		}
		if delta.ReasoningContent != "" {
			reasoningBuilder.WriteString(delta.ReasoningContent)
		}

		for _, tc := range delta.ToolCalls {
			existing, ok := toolCalls[tc.Index]
			if !ok {
				toolCalls[tc.Index] = &ToolCall{
					ID:   tc.ID,
					Type: tc.Type,
					Function: FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				}
			} else {
				if tc.ID != "" {
					existing.ID = tc.ID
				}
				if tc.Type != "" {
					existing.Type = tc.Type
				}
				if tc.Function.Name != "" {
					existing.Function.Name += tc.Function.Name
				}
				existing.Function.Arguments += tc.Function.Arguments
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	reasoning := reasoningBuilder.String()
	result := &Response{
		Content:  contentBuilder.String(),
		Thinking: reasoning,
		Usage:    usage,
	}
	for i := 0; i < len(toolCalls); i++ {
		if tc, ok := toolCalls[i]; ok {
			result.ToolCalls = append(result.ToolCalls, *tc)
		}
	}

	// 与 Anthropic 提供者相同的泄漏 XML 恢复——通过 api.deepseek.com
	//（openai-chat 兼容模式，而非 anthropic 路由）提供的 DeepSeek-flash
	// 将其工具调用作为 DSML 样式文本发出，而不是（或除了）结构化的
	// tool_calls 字段。无条件地从内容中剥离 XML；
	// 仅当结构化通道为空时才从其中合成工具调用。
	if cleaned, calls := extractLeakedToolCalls(result.Content); cleaned != result.Content {
		result.Content = cleaned
		if len(result.ToolCalls) == 0 && len(calls) > 0 {
			slog.Warn("recovered leaked tool-call XML from text content (openai-chat)",
				"count", len(calls))
			result.ToolCalls = calls
		} else if len(calls) > 0 {
			slog.Debug("stripped leaked tool-call XML echoing a structured tool_use (openai-chat)",
				"echo_count", len(calls), "structured_count", len(result.ToolCalls))
		}
	}

	// 捕获原始助手消息以进行缓存安全的重放。
	// 重建 API 期望返回的精确消息格式。
	// reasoning_content 必须为 DeepSeek 的思考模式往返
	//（否则下一轮会失败：`The reasoning_content in
	// the thinking mode must be passed back to the API.`）。
	rawMsg := apiMessage{
		Role:             "assistant",
		ToolCalls:        result.ToolCalls,
		ReasoningContent: reasoning,
	}
	rawMsg.Content, _ = json.Marshal(result.Content)
	result.RawAssistant, _ = json.Marshal(rawMsg)

	return result, nil
}

// openaiUsageToProvider 将 OpenAI 风格的使用量块折叠为
// 提供者中立的 Usage。缓存的提示令牌（如果报告了）作为
// CacheReadTokens 呈现，而 input_tokens 是*未缓存*的剩余部分，
// 因此 input+cache_read 仍然等于总提示大小。
func openaiUsageToProvider(u *sseUsage) Usage {
	out := Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		out.InputTokens -= u.PromptTokensDetails.CachedTokens
		if out.InputTokens < 0 {
			out.InputTokens = 0
		}
	}
	return out
}
