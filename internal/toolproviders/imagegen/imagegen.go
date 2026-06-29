// Package imagegen 包含内置的 image_gen 提供商。提供商接受提示词（+ 可选的 size/n），
// 并返回 LLM 可见的文本负载，其中嵌入内联 base64 图像数据或远程 URL——
// 根据上游提供的内容而定。聊天 UI 会内联渲染 markdown 图像标签。
package imagegen

import (
	"fmt"
	"strings"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Category 是这些提供商插入的工具类别。
const Category = "image_gen"

// RegisterAll 在 r 中注册所有内置的 image_gen 提供商。
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&OpenAI{})
	r.Register(&Fal{})
	r.Register(&Replicate{})
	r.Register(&None{})
}

type args struct {
	Prompt string
	Size   string
	N      int
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["prompt"].(string); ok {
		a.Prompt = strings.TrimSpace(s)
	}
	if a.Prompt == "" {
		return a, fmt.Errorf("prompt is required")
	}
	if s, ok := raw["size"].(string); ok {
		a.Size = s
	}
	switch v := raw["n"].(type) {
	case float64:
		a.N = int(v)
	case int:
		a.N = v
	}
	if a.N <= 0 {
		a.N = 1
	}
	if a.N > 4 {
		a.N = 4
	}
	return a, nil
}

// renderURLs 从图像 URL 列表构建 LLM 可见的响应。
// 每个 URL 作为 markdown 图像标签发出，以便聊天 UI 内联渲染，
// 而模型无需了解 markdown 细节。
func renderURLs(prompt string, urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Generated %d image(s) for: %s\n\n", len(urls), prompt)
	for i, u := range urls {
		fmt.Fprintf(&sb, "%d. ![image %d](%s)\n", i+1, i+1, u)
	}
	return sb.String()
}

// renderB64 内联输出 base64 图像。当提供商返回原始字节时使用
//（例如 gpt-image-1 使用 response_format=b64_json）。
func renderB64(prompt string, b64s []string) string {
	if len(b64s) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Generated %d image(s) for: %s\n\n", len(b64s), prompt)
	for i, b := range b64s {
		fmt.Fprintf(&sb, "%d. ![image %d](data:image/png;base64,%s)\n", i+1, i+1, b)
	}
	return sb.String()
}
