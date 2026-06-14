package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// ElevenLabs 向 /v1/text-to-speech/{voice_id} 发送请求，
// API 密钥放在非标准的 `xi-api-key` 头中。
// Model（"elevenlabs/<model>" 中的后缀）默认为 "eleven_multilingual_v2"；
// Voice 默认为长期内置的 "Rachel" 语音。响应为原始 audio/mpeg。
type ElevenLabs struct{}

func (ElevenLabs) Category() string { return Category }
func (ElevenLabs) Name() string     { return "elevenlabs" }

// elevenLabsDefaultVoice 是调用方未传递 voice_id 时使用的默认值。
// "Rachel" 是 ElevenLabs 的标准示例语音，在所有账户层级上均可用。
const elevenLabsDefaultVoice = "21m00Tcm4TlvDq8ikWAM"

func (e *ElevenLabs) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("elevenlabs: missing api key")
	}
	model := req.Config.Model
	if model == "" {
		model = "eleven_multilingual_v2"
	}
	voice := a.Voice
	if voice == "" {
		voice = elevenLabsDefaultVoice
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"text":     a.Text,
		"model_id": model,
	}
	buf, _ := json.Marshal(body)
	url := "https://api.elevenlabs.io/v1/text-to-speech/" + voice + "?output_format=mp3_44100_128"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// ElevenLabs 使用自定义的认证头，而非 Bearer。
	httpReq.Header.Set("xi-api-key", req.Config.APIKey)
	httpReq.Header.Set("Accept", "audio/mpeg")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("elevenlabs request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("elevenlabs", resp); err != nil {
		return toolproviders.Response{}, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("read elevenlabs audio: %w", err)
	}
	return writeAudio(data, "mp3")
}
