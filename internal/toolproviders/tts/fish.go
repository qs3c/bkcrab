package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Fish 向 https://api.fish.audio/v1/tts（即 fish.studio）发送请求。
// 认证方式为 `Authorization: Bearer <token>`。Voice（LLM 提供的 `voice` 参数）
// 映射到 `reference_id` 字段——Fish 将语音公开为克隆语音 ID，由管理员/用户从仪表盘中选择。
// 省略时，请求也省略该字段，Fish 使用内置语音。
type Fish struct{}

func (Fish) Category() string { return Category }
func (Fish) Name() string     { return "fish" }

func (f *Fish) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("fish: missing api key")
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"text":   a.Text,
		"format": "mp3",
	}
	if a.Voice != "" {
		body["reference_id"] = a.Voice
	}
	// Model（"fish/<model>" 中的后缀）选择合成后端（s1 / speech-1.5 等）。
	// 设置时，将其作为 `backend` 传递；默认为 "s1"——保持未设置状态，
	// 以便 Fish 选择自己的当前默认值。
	if req.Config.Model != "" {
		body["backend"] = req.Config.Model
	}
	buf, _ := json.Marshal(body)

	endpoint := "https://api.fish.audio/v1/tts"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("fish request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("fish", resp); err != nil {
		return toolproviders.Response{}, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("read fish audio: %w", err)
	}
	return writeAudio(data, "mp3")
}
