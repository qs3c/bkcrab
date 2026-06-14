package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// RegisterImageGenChain 针对提供者注册 image_gen 工具
// 链。仅当链中至少有一个提供商已注册时才注册
// 配置了凭据 - 因此没有 image-gen 密钥的代理看不到
// 它无法使用的工具。
func RegisterImageGenChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil {
		return
	}
	// “none”是一个哨兵，意味着管理员明确选择退出
	// bkclaw 的 image_gen。在链条中的任何位置检测到 → 不检测
	// 完全注册该工具，以便模型恢复到它自己的状态
	// 本机图像生成功能（或没有）。
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
	r.Register("image_gen", "Generate images from a text prompt. Uses a configurable provider chain (OpenAI gpt-image-1, fal flux, …) with automatic fallback. Returns markdown image tags that render inline in chat.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"prompt": map[string]interface{}{
				"type":        "string",
				"description": "Description of the image to generate",
			},
			"size": map[string]interface{}{
				"type":        "string",
				"description": "Image size (e.g. 1024x1024). Provider-specific.",
			},
			"n": map[string]interface{}{
				"type":        "integer",
				"description": "How many variations (default 1, max 4)",
			},
		},
		"required": []string{"prompt"},
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
