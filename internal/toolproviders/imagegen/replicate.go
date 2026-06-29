package imagegen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Replicate 向 https://api.replicate.com/v1/models/<owner>/<name>/predictions
// 发送请求，使用 `Prefer: wait` 获取同步响应（最长 60 秒），而不是轮询预测 URL。
// 认证方式为 `Authorization: Bearer <token>`
//（Replicate 也接受旧的 `Token <token>` 格式；我们使用 Bearer 以与目录其余部分保持一致）。
type Replicate struct{}

func (Replicate) Category() string { return Category }
func (Replicate) Name() string     { return "replicate" }

// replicateModelRoutes 将短模型键（"replicate/" 后的所有内容）
// 映射到 Replicate 上的 "<owner>/<name>" 路径。调用方也可以传递原始的
// owner/name 对（例如 "replicate/black-forest-labs/flux-schnell"），
// 我们会原样路由。
var replicateModelRoutes = map[string]string{
	"flux-schnell": "black-forest-labs/flux-schnell",
	"flux-dev":     "black-forest-labs/flux-dev",
	"flux-pro":     "black-forest-labs/flux-1.1-pro",
	"sdxl":         "stability-ai/sdxl",
	"ideogram":     "ideogram-ai/ideogram-v2",
}

func (r *Replicate) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("replicate: missing api key")
	}
	modelKey := req.Config.Model
	if modelKey == "" {
		modelKey = "flux-schnell"
	}
	path, ok := replicateModelRoutes[modelKey]
	if !ok {
		path = modelKey
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	input := map[string]any{
		"prompt":      a.Prompt,
		"num_outputs": a.N,
	}
	if a.Size != "" {
		// Replicate flux 模型使用 aspect_ratio（如 "1:1"）；width/height
		// 已从架构中移除。接受两者并透传——调用方的工具描述会告诉 LLM 发送什么。
		input["aspect_ratio"] = a.Size
	}
	buf, _ := json.Marshal(map[string]any{"input": input})

	url := "https://api.replicate.com/v1/models/" + path + "/predictions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)
	// Prefer: wait 使 Replicate 保持连接打开，直到预测完成（或 60 秒超时），
	// 因此我们不需要在此实现轮询循环。超过 60 秒后，响应返回 status="processing"，
	// 我们将其作为 ErrNoResults 暴露，以便链可以回退。
	httpReq.Header.Set("Prefer", "wait")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("replicate request: %w", err))
	}
	defer resp.Body.Close()
	// 即使 Prefer:wait 等待完成，Replicate 对已接受的预测仍返回 201 Created，
	// 因此将 200 和 201 都视为成功。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return toolproviders.Response{}, retriableHTTP("replicate", resp)
	}
	var out struct {
		Status string          `json:"status"` // "succeeded" / "failed" / "processing" / ...
		Error  string          `json:"error,omitempty"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("replicate decode: %w", err)
	}
	if out.Status == "failed" {
		return toolproviders.Response{}, fmt.Errorf("replicate failed: %s", out.Error)
	}
	if out.Status != "succeeded" {
		// "starting" / "processing" / "canceled" —— 视为可重试，以便链中的下一个提供商有机会。
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("replicate status %q", out.Status))
	}
	urls := decodeReplicateOutput(out.Output)
	if len(urls) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: renderURLs(a.Prompt, urls)}, nil
}

// decodeReplicateOutput 接受数组形式（flux: ["url1", "url2"]）
// 和单字符串形式（一些较旧的模型返回一个 URL），并返回扁平切片。
func decodeReplicateOutput(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil && single != "" {
		return []string{single}
	}
	return nil
}
