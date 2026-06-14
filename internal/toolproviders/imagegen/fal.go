package imagegen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// Fal 向 https://fal.run/<model-path> 发送请求。
// 引用中的 "<model>" 部分成为 URL 的尾部（例如 "fal/flux-dev" →
// https://fal.run/fal-ai/flux/dev）。认证方式为 "Key <token>"。
type Fal struct{}

func (Fal) Category() string { return Category }
func (Fal) Name() string     { return "fal" }

var falModelRoutes = map[string]string{
	"flux-dev":     "fal-ai/flux/dev",
	"flux-schnell": "fal-ai/flux/schnell",
	"flux-pro":     "fal-ai/flux-pro",
}

func (f *Fal) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("fal: missing api key")
	}
	modelKey := req.Config.Model
	if modelKey == "" {
		modelKey = "flux-dev"
	}
	path, ok := falModelRoutes[modelKey]
	if !ok {
		// 允许调用方也传递原始模型路径（例如 "fal/fal-ai/flux/dev"）。
		path = modelKey
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"prompt":     a.Prompt,
		"num_images": a.N,
	}
	if a.Size != "" {
		body["image_size"] = a.Size
	}
	buf, _ := json.Marshal(body)
	url := "https://fal.run/" + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Key "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("fal request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("fal", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("fal decode: %w", err)
	}
	urls := make([]string, 0, len(out.Images))
	for _, img := range out.Images {
		if img.URL != "" {
			urls = append(urls, img.URL)
		}
	}
	if len(urls) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: renderURLs(a.Prompt, urls)}, nil
}
