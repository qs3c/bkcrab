// Package imagegen 包含内置的 image_gen 提供商。提供商接受提示词（+ 可选的 size/n），
// 并返回 LLM 可见的文本负载，其中嵌入内联 base64 图像数据或远程 URL——
// 根据上游提供的内容而定。聊天 UI 会内联渲染 markdown 图像标签。
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
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
// （例如 gpt-image-1 使用 response_format=b64_json）。
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

func canonicalSizes() []string {
	return []string{domain.SizeSquare, domain.SizeLandscape, domain.SizePortrait}
}

func validateAdapterRequest(provider string, request domain.GenerateRequest) error {
	if strings.TrimSpace(request.Prompt) == "" || request.Count < 1 || request.Count > 4 {
		return domain.NewProviderError(domain.ErrorInvalidRequest, provider, 0, errors.New("invalid prompt or count"))
	}
	return nil
}

func cfgModel(cfg domain.ResolvedProviderConfig, fallback string) string {
	if strings.TrimSpace(cfg.Model) != "" {
		return strings.TrimSpace(cfg.Model)
	}
	return fallback
}

func postJSON(ctx context.Context, endpoint string, body any, headers map[string]string) (*http.Response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return http.DefaultClient.Do(request)
}

func typedTransportError(provider string, err error) error {
	return domain.NewProviderError(domain.ErrorUpstreamTransient, provider, 0, err)
}

func classifyHTTPResponse(provider string, response *http.Response, successful ...int) error {
	for _, status := range successful {
		if response.StatusCode == status {
			return nil
		}
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	lower := strings.ToLower(string(body))
	kind := domain.ErrorInvalidRequest
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		kind = domain.ErrorAuthConfig
	case response.StatusCode == http.StatusTooManyRequests:
		kind = domain.ErrorRateLimited
	case response.StatusCode == http.StatusNotFound:
		kind = domain.ErrorModelUnavailable
	case response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500:
		kind = domain.ErrorUpstreamTransient
	case strings.Contains(lower, "safety") || strings.Contains(lower, "content_policy") || strings.Contains(lower, "content policy"):
		kind = domain.ErrorSafetyRejected
	}
	return domain.NewProviderError(kind, provider, response.StatusCode, errors.New("provider rejected request"))
}

func validSourceURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func imageMIME(data []byte) (string, bool) {
	mimeType := http.DetectContentType(data)
	return mimeType, strings.HasPrefix(mimeType, "image/")
}

func canonicalLegacySize(size string) string {
	if strings.TrimSpace(size) == "" {
		return domain.SizeSquare
	}
	return size
}

func legacyProviderResponse(prompt string, result domain.ProviderResult) toolproviders.Response {
	urls := make([]string, 0, len(result.Images))
	for _, image := range result.Images {
		if image.SourceURL != "" {
			urls = append(urls, image.SourceURL)
			continue
		}
		if len(image.Bytes) > 0 {
			mimeType := image.MIMEType
			if mimeType == "" {
				mimeType = "image/png"
			}
			urls = append(urls, "data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(image.Bytes))
		}
	}
	return toolproviders.Response{Text: renderURLs(prompt, urls), Raw: result}
}

func legacyProviderError(err error) error {
	switch domain.ProviderErrorKind(err) {
	case domain.ErrorRateLimited, domain.ErrorUpstreamTransient, domain.ErrorModelUnavailable,
		domain.ErrorEmptyResult, domain.ErrorIncompleteResult, domain.ErrorMalformedResult:
		return toolproviders.Retry(err)
	default:
		return err
	}
}
