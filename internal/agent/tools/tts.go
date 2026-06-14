package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// RegisterTTSChain 针对提供商链注册 tts 工具。缺席的
// 凭据 ⇒ 该工具对代理根本不可见。
func RegisterTTSChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil {
		return
	}
	// “none”是一个哨兵，意味着管理员明确选择退出
	// bkclaw 的 tts。在链中的任何位置检测到 → 不注册
	// 完全使用该工具，以便模型回退到其自己的本机音频
	// 能力（或没有）。
	for _, ref := range chain.Order {
		name := ref
		if i := strings.IndexByte(ref, '/'); i >= 0 {
			name = ref[:i]
		}
		if name == "none" {
			return
		}
	}
	if !chain.Available() {
		return
	}
	r.Register("tts", "Convert text to speech. Uses a configurable provider chain (OpenAI tts-1, MiniMax speech-02, …) with automatic fallback. The audio file is attached to the chat message automatically.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"text": map[string]interface{}{
				"type":        "string",
				"description": "Text to synthesize",
			},
			"voice": map[string]interface{}{
				"type":        "string",
				"description": "Voice id (provider-specific; default picked automatically)",
			},
		},
		"required": []string{"text"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args map[string]any
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		resp, err := chain.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}
