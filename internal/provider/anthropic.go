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

// AnthropicProvider 为 Anthropic Messages API 实现 Provider 接口。
type AnthropicProvider struct {
	apiKey  string
	apiBase string
	client  *http.Client
}

// NewAnthropic 创建一个新的 Anthropic Messages API 提供者。
func NewAnthropic(apiKey, apiBase string) *AnthropicProvider {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = "https://api.anthropic.com"
	}
	return &AnthropicProvider{
		apiKey:  apiKey,
		apiBase: NormalizeAPIBase(apiBase, "anthropic-messages"),
		client:  newLLMHTTPClient(),
	}
}

// anthropicMessage 是 Anthropic Messages API 的线路格式。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	Messages  []anthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
}

// toAnthropicMessages 将提供者 Messages 转换为 Anthropic 线路格式。
// 提取系统消息并返回其余部分。
func toAnthropicMessages(msgs []Message) (string, []anthropicMessage) {
	var system string
	var out []anthropicMessage

	// Anthropic 拒绝任何 tool_result 没有出现在紧随其后的消息中的 tool_use。
	// 当循环检测/达到上限的合成在 tool_use 和 padOrphanToolResults
	// 填充之间注入了系统或助手消息时，代理循环可能产生这种形状。
	// 重用 openai 路径的扫描器来标记孤立的助手 tool_calls，
	// 然后扫描整个消息列表，查找我们刚刚决定丢弃的 tool_use
	// 对应的工具回复——openai 扫描器只检查紧接着的工具运行，
	// 这可能会漏掉在中间非工具消息之后出现的填充结果。
	// 会话历史保持不变；这仅影响线路构建。
	orphanAssistant, orphanTool := findOrphanToolCalls(msgs)
	orphanIDs := map[string]bool{}
	for i, m := range msgs {
		if orphanAssistant[i] {
			for _, id := range assistantToolCallIDs(m) {
				orphanIDs[id] = true
			}
		}
	}
	if len(orphanIDs) > 0 {
		for i, m := range msgs {
			if m.Role == "tool" && orphanIDs[m.ToolCallID] {
				orphanTool[i] = true
			}
		}
	}

	for i, m := range msgs {
		if m.Role == "system" {
			system = m.Content
			continue
		}
		if orphanTool[i] {
			continue
		}
		// 如果剥离孤立的 tool_calls 后，助手消息没有文本形式的内容
		//（没有 Content、没有 ContentParts、没有 Thinking），
		// 则完全丢弃它。Anthropic 拒绝无内容的消息，错误信息为
		// "expected a string or a list"，所以我们不能只发出一个空壳。
		//
		// RawAssistant 故意不在此守卫中：当它在孤立轮次中非空时，
		// 它恰好捕获了我们即将剥离的 tool_use 块（或来自先前提供者的
		// OpenAI 格式的 blob）——两者都不能重放为有效的 Anthropic 消息，
		// 因此仅仅为了"保留"RawAssistant 而保留消息会在下一个用户轮次
		// 产生 `content: null` 和 400 错误
		//（"messages.N.content: Input should be a valid array"）。
		// 真实的思考内容通过 Thinking / thinkingBlockFor 存活，
		// 它们确实可以往返。
		if orphanAssistant[i] && m.Content == "" && len(m.ContentParts) == 0 &&
			m.Thinking == "" {
			continue
		}

		am := anthropicMessage{Role: m.Role}

		// 工具结果变为 role "user"，带有 tool_result 内容块。
		// Anthropic 要求并行 tool_use 批次的每个 tool_result
		// 都在同一个用户消息中——即使连续发送不同的用户消息，
		// 也会被拒绝："tool_use ids ... without tool_result
		// blocks immediately after"。因此将连续的工具消息合并为
		// 一个包含各自 tool_result 块的单个用户消息。
		if m.Role == "tool" {
			block := map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			if n := len(out); n > 0 && out[n-1].Role == "user" {
				var existing []interface{}
				if err := json.Unmarshal(out[n-1].Content, &existing); err == nil && len(existing) > 0 {
					allToolResults := true
					for _, eb := range existing {
						mp, ok := eb.(map[string]interface{})
						if !ok || mp["type"] != "tool_result" {
							allToolResults = false
							break
						}
					}
					if allToolResults {
						existing = append(existing, block)
						out[n-1].Content, _ = json.Marshal(existing)
						continue
					}
				}
			}
			am.Role = "user"
			am.Content, _ = json.Marshal([]interface{}{block})
			out = append(out, am)
			continue
		}

		// 带有工具调用的助手消息。orphanAssistant 标记那些 tool_calls
		// 没有匹配的 tool_result 运行的消息——在这种情况下只发出文本，
		// 以免 API 因悬空的 tool_use 而拒绝请求。
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && !orphanAssistant[i] {
			var blocks []interface{}
			if tb := thinkingBlockFor(m); tb != nil {
				blocks = append(blocks, tb)
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
			// Anthropic 拒绝其输入不是 JSON 对象的 tool_use 块——
			// `Arguments` 可能以 ""（模型流式传输了带有空输入且从未触发
			// input_json_delta 事件的 tool_use）、`null` 或
			// 像字符串这样的裸值的形式出现。将所有情况强制转换为
			// 空对象，以便历史消息成功重放。真实输入保持不变地往返。
			input := parseToolInput(tc.Function.Arguments)
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": input,
				})
			}
			am.Content, _ = json.Marshal(blocks)
			out = append(out, am)
			continue
		}

		// 普通文本内容
		if len(m.ContentParts) > 0 {
			var blocks []interface{}
			if m.Role == "assistant" {
				if tb := thinkingBlockFor(m); tb != nil {
					blocks = append(blocks, tb)
				}
			}
			for _, part := range m.ContentParts {
				if part.Type == "text" {
					blocks = append(blocks, map[string]interface{}{
						"type": "text",
						"text": part.Text,
					})
				} else if part.Type == "image_url" && part.ImageURL != nil {
					blocks = append(blocks, map[string]interface{}{
						"type": "image",
						"source": map[string]string{
							"type": "url",
							"url":  part.ImageURL.URL,
						},
					})
				}
			}
			am.Content, _ = json.Marshal(blocks)
		} else if m.Role == "assistant" {
			var blocks []interface{}
			if tb := thinkingBlockFor(m); tb != nil {
				blocks = append(blocks, tb)
			}
			if m.Content != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": m.Content,
				})
			}
			if len(blocks) > 0 {
				am.Content, _ = json.Marshal(blocks)
			} else if m.Content != "" {
				am.Content, _ = json.Marshal(m.Content)
			}
		} else if m.Content != "" {
			am.Content, _ = json.Marshal(m.Content)
		} else {
			// 防御性：无内容的用户/系统消息将序列化为缺少 `content` 字段
			// 的线路对象，Anthropic 会以 "expected a string or a list" 拒绝。
			// 发送空字符串，以便即使上游调用者产生退化的消息
			//（例如在 ContentParts 持久化之前的旧会话加载）也能往返。
			am.Content, _ = json.Marshal("")
		}

		out = append(out, am)
	}

	return system, out
}

// parseToolInput 将存储的 tool_use Arguments 字符串解码为
// Anthropic 线路格式所需的 JSON 对象。任何不是对象的内容——
// 空字符串、null、JSON 数组、裸值——都会被强制转换为
// 空对象 {}，以便消息干净地重放。真实输入保持不变地往返。
//
// 为什么要强制转换：当 Anthropic 流式传输输入为 `{}` 的 tool_use 时，
// 它在 `content_block_start` 中发出占位符，但没有后续的
// `input_json_delta` 事件（没有要流式传输的内容），
// 因此我们的 argsJSON 累加器保持为 ""。
// 这会被持久化到 session_messages.tool_calls，
// 并在下一次轮次重放时被拒绝："tool_use.input: Input should be an object"。
// 在线路构建时强制转换成本低廉，
// 并且还能涵盖来自通过 Claude-compat 提供服务的非 Anthropic 提供者
// 的任何其他未来形状漂移。
func parseToolInput(raw string) interface{} {
	if raw == "" {
		return map[string]interface{}{}
	}
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return map[string]interface{}{}
	}
	if _, ok := v.(map[string]interface{}); !ok {
		return map[string]interface{}{}
	}
	return v
}

// thinkingBlockFor 返回助手消息先前思考的内容块映射，
// 如果没有要重放的内容则返回 nil。扩展思考模型
//（真正的 Anthropic、DeepSeek 的 /anthropic 兼容模式）
// 在丢弃先前的 `content[].thinking` 时会拒绝下一轮，
// 因此我们原样回显它。
func thinkingBlockFor(m Message) map[string]interface{} {
	if m.Role != "assistant" {
		return nil
	}
	// 优先使用我们在响应中捕获的原始思考块——为真正的 Anthropic
	// 保留签名。回退到我们在捕获签名之前写入的会话中存储在
	// Message.Thinking 上的纯文本。
	if len(m.RawAssistant) > 0 {
		var raw map[string]interface{}
		if err := json.Unmarshal(m.RawAssistant, &raw); err == nil {
			if t, _ := raw["type"].(string); t == "thinking" {
				return raw
			}
		}
	}
	if m.Thinking != "" {
		return map[string]interface{}{
			"type":     "thinking",
			"thinking": m.Thinking,
		}
	}
	return nil
}

func (p *AnthropicProvider) buildRequest(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64, stream bool) (*http.Request, error) {
	system, anthropicMsgs := toAnthropicMessages(messages)

	if maxTokens <= 0 {
		maxTokens = 4096
	}

	// Anthropic 在支持扩展思考的模型（Opus 4.x、Sonnet 4.5+、Haiku 4.5）
	// 上弃用了 `temperature`——发送它会返回硬 400 错误。
	// 较旧的模型接受它，但系统默认值 0.7 很少在每个代理上有意义地调整，
	// 因此我们在所有情况下都丢弃它，而不是按模型进行门控。
	_ = temperature
	req := anthropicRequest{
		Model:     StripProviderPrefix(model),
		Messages:  anthropicMsgs,
		System:    system,
		MaxTokens: maxTokens,
		Stream:    stream,
	}

	if len(tools) > 0 {
		for _, t := range tools {
			req.Tools = append(req.Tools, anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			})
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := p.apiBase + "/v1/messages"
	slog.Info("anthropic request", "url", url, "model", req.Model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	return httpReq, nil
}

// Anthropic SSE 事件类型
type anthropicSSEEvent struct {
	Type string `json:"type"`
}

type anthropicContentBlockStart struct {
	Type         string                     `json:"type"`
	Index        int                        `json:"index"`
	ContentBlock anthropicContentBlockEntry `json:"content_block"`
}

type anthropicContentBlockEntry struct {
	Type  string          `json:"type"` // "text" 或 "tool_use"
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Text  string          `json:"text,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicContentBlockDelta struct {
	Type  string                `json:"type"`
	Index int                   `json:"index"`
	Delta anthropicDeltaContent `json:"delta"`
}

type anthropicDeltaContent struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

// anthropicUsage 镜像 Anthropic Messages 返回的 `usage` 字段。
// message_start 携带输入令牌数（以及提示缓存细分）；
// message_delta 携带最终的 output_tokens。我们捕获两者并
// 在 Response / StreamChunk 的 provider.Usage 上暴露总数。
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type anthropicMessageStart struct {
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
}

type anthropicMessageDelta struct {
	Usage anthropicUsage `json:"usage"`
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error) {
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

func (p *AnthropicProvider) ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error) {
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

		type blockState struct {
			blockType string // "text" | "tool_use" | "thinking"
			id        string
			name      string
			argsJSON  strings.Builder
			thinking  strings.Builder
			signature string
		}
		blocks := make(map[int]*blockState)
		var usage Usage

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			data := strings.TrimPrefix(line, "data: ")

			var event anthropicSSEEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}

			switch event.Type {
			case "message_start":
				var ms anthropicMessageStart
				if json.Unmarshal([]byte(data), &ms) == nil {
					usage.InputTokens = ms.Message.Usage.InputTokens
					usage.CacheReadTokens = ms.Message.Usage.CacheReadInputTokens
					usage.CacheCreationTokens = ms.Message.Usage.CacheCreationInputTokens
				}
			case "message_delta":
				var md anthropicMessageDelta
				if json.Unmarshal([]byte(data), &md) == nil && md.Usage.OutputTokens > 0 {
					usage.OutputTokens = md.Usage.OutputTokens
				}
			case "content_block_start":
				var cbs anthropicContentBlockStart
				if json.Unmarshal([]byte(data), &cbs) == nil {
					blocks[cbs.Index] = &blockState{
						blockType: cbs.ContentBlock.Type,
						id:        cbs.ContentBlock.ID,
						name:      cbs.ContentBlock.Name,
					}
					if cbs.ContentBlock.Text != "" {
						select {
						case ch <- StreamChunk{Content: cbs.ContentBlock.Text}:
						case <-ctx.Done():
							return
						}
					}
				}

			case "content_block_delta":
				var cbd anthropicContentBlockDelta
				if json.Unmarshal([]byte(data), &cbd) == nil {
					switch cbd.Delta.Type {
					case "text_delta":
						if cbd.Delta.Text != "" {
							select {
							case ch <- StreamChunk{Content: cbd.Delta.Text}:
							case <-ctx.Done():
								return
							}
						}
					case "input_json_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.argsJSON.WriteString(cbd.Delta.PartialJSON)
						}
					case "thinking_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.thinking.WriteString(cbd.Delta.Thinking)
						}
					case "signature_delta":
						if bs, ok := blocks[cbd.Index]; ok {
							bs.signature = cbd.Delta.Signature
						}
					}
				}

			case "message_stop":
				var toolCalls []ToolCall
				var thinkingText, thinkingSig string
				for i := 0; i < len(blocks); i++ {
					bs, ok := blocks[i]
					if !ok {
						continue
					}
					switch bs.blockType {
					case "tool_use":
						toolCalls = append(toolCalls, ToolCall{
							ID:   bs.id,
							Type: "function",
							Function: FunctionCall{
								Name:      bs.name,
								Arguments: bs.argsJSON.String(),
							},
						})
					case "thinking":
						if t := bs.thinking.String(); t != "" {
							thinkingText = t
						}
						if bs.signature != "" {
							thinkingSig = bs.signature
						}
					}
				}
				select {
				case ch <- StreamChunk{
					ToolCalls:         toolCalls,
					Thinking:          thinkingText,
					ThinkingSignature: thinkingSig,
					Usage:             usage,
					Done:              true,
				}:
				case <-ctx.Done():
				}
				return
			}
		}

		if err := scanner.Err(); err != nil {
			reader.SetErr(fmt.Errorf("read stream: %w", err))
		}
	}()

	return reader, nil
}

func (p *AnthropicProvider) parseSSE(body io.Reader) (*Response, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var contentBuilder strings.Builder

	type blockState struct {
		blockType string
		id        string
		name      string
		argsJSON  strings.Builder
		thinking  strings.Builder
		signature string
	}
	blocks := make(map[int]*blockState)
	var usage Usage

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event anthropicSSEEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			slog.Warn("parse anthropic SSE event", "error", err, "data", data)
			continue
		}

		switch event.Type {
		case "message_start":
			var ms anthropicMessageStart
			if json.Unmarshal([]byte(data), &ms) == nil {
				usage.InputTokens = ms.Message.Usage.InputTokens
				usage.CacheReadTokens = ms.Message.Usage.CacheReadInputTokens
				usage.CacheCreationTokens = ms.Message.Usage.CacheCreationInputTokens
			}
		case "message_delta":
			var md anthropicMessageDelta
			if json.Unmarshal([]byte(data), &md) == nil && md.Usage.OutputTokens > 0 {
				usage.OutputTokens = md.Usage.OutputTokens
			}
		case "content_block_start":
			var cbs anthropicContentBlockStart
			if json.Unmarshal([]byte(data), &cbs) == nil {
				blocks[cbs.Index] = &blockState{
					blockType: cbs.ContentBlock.Type,
					id:        cbs.ContentBlock.ID,
					name:      cbs.ContentBlock.Name,
				}
				if cbs.ContentBlock.Text != "" {
					contentBuilder.WriteString(cbs.ContentBlock.Text)
				}
			}

		case "content_block_delta":
			var cbd anthropicContentBlockDelta
			if json.Unmarshal([]byte(data), &cbd) == nil {
				switch cbd.Delta.Type {
				case "text_delta":
					contentBuilder.WriteString(cbd.Delta.Text)
				case "input_json_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.argsJSON.WriteString(cbd.Delta.PartialJSON)
					}
				case "thinking_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.thinking.WriteString(cbd.Delta.Thinking)
					}
				case "signature_delta":
					if bs, ok := blocks[cbd.Index]; ok {
						bs.signature = cbd.Delta.Signature
					}
				}
			}

		case "message_stop":
			// 完成
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	result := &Response{
		Content: contentBuilder.String(),
		Usage:   usage,
	}
	var thinkingBuilder strings.Builder
	var thinkingSig string
	for i := 0; i < len(blocks); i++ {
		bs, ok := blocks[i]
		if !ok {
			continue
		}
		switch bs.blockType {
		case "tool_use":
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:   bs.id,
				Type: "function",
				Function: FunctionCall{
					Name:      bs.name,
					Arguments: bs.argsJSON.String(),
				},
			})
		case "thinking":
			if t := bs.thinking.String(); t != "" {
				thinkingBuilder.WriteString(t)
			}
			if bs.signature != "" {
				thinkingSig = bs.signature
			}
		}
	}
	if thinking := thinkingBuilder.String(); thinking != "" {
		result.Thinking = thinking
		// DeepSeek 的 Anthropic 兼容端点（以及真正的 Anthropic 扩展思考）
		// 要求思考块在下一轮中原样回显。将 {thinking, signature}
		// 打包到 RawAssistant 中，以便 toAnthropicMessages
		// 可以将其作为内容块重放。
		type thinkingBlock struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature,omitempty"`
		}
		if raw, err := json.Marshal(thinkingBlock{
			Type:      "thinking",
			Thinking:  thinking,
			Signature: thinkingSig,
		}); err == nil {
			result.RawAssistant = raw
		}
	}

	// 回退：某些通过 anthropic-compat 端点提供的非 Claude 模型
	//（例如 MiMo、DeepSeek-flash）在文本内容块中发出 Claude 风格的
	// 工具调用 XML。我们需要分别防范两种失败模式：
	//
	//   1. XML 替代了结构化的 tool_use → 我们必须从 XML 合成工具调用，
	//      否则代理循环只看到文本响应并将其视为最终答案。
	//
	//   2. XML 与结构化的 tool_use 并存（DeepSeek-flash 每次迭代都
	//      将其预期调用作为文本回显）→ 结构化调用已经驱动执行，
	//      但文本 DSML 作为 assistant.content 附带。这膨胀了每一轮的
	//      提示，更糟的是，在子代理达到上限的强制交付路径中，
	//      它会作为子代理的"最终答案"发送回父代理——
	//      在聊天 UI 中以原始 DSML 形式呈现。
	//
	// 因此无条件地从内容中剥离 XML；仅当没有要调度的结构化调用时
	// 才合成工具调用。
	if cleaned, calls := extractLeakedToolCalls(result.Content); cleaned != result.Content {
		result.Content = cleaned
		if len(result.ToolCalls) == 0 && len(calls) > 0 {
			slog.Warn("recovered leaked tool-call XML from text content",
				"count", len(calls))
			result.ToolCalls = calls
		} else if len(calls) > 0 {
			slog.Debug("stripped leaked tool-call XML echoing a structured tool_use",
				"echo_count", len(calls), "structured_count", len(result.ToolCalls))
		}
	}

	return result, nil
}
