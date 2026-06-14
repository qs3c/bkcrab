// Package tts 包含文本转语音提供商。该类别将生成的音频作为每临时文件的 MEDIA: 行返回，
// 以便聊天管道自动将音频片段附加到助手的出站消息中。
package tts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// Category 是这些提供商插入的工具类别。
const Category = "tts"

// RegisterAll 在 r 中注册内置的 TTS 提供商。
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&OpenAI{})
	r.Register(&MiniMax{})
	r.Register(&ElevenLabs{})
	r.Register(&Fish{})
	r.Register(&None{})
}

type args struct {
	Text  string
	Voice string
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["text"].(string); ok {
		a.Text = strings.TrimSpace(s)
	}
	if a.Text == "" {
		return a, fmt.Errorf("text is required")
	}
	if s, ok := raw["voice"].(string); ok {
		a.Voice = s
	}
	return a, nil
}

// writeAudio 将音频字节写入临时文件并返回 LLM 可见的响应。
// MEDIA: 标记就是 loop.go 中 extractMediaPaths 拾取文件并附加到出站聊天消息的方式。
func writeAudio(data []byte, ext string) (toolproviders.Response, error) {
	if len(data) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	if ext == "" {
		ext = "mp3"
	}
	f, err := os.CreateTemp("", "bkclaw-tts-*."+ext)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("create tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return toolproviders.Response{}, fmt.Errorf("write tmp: %w", err)
	}
	f.Close()
	path, _ := filepath.Abs(f.Name())
	// 第一行是 LLM 可见的状态；MEDIA: 行在文本到达模型之前被循环消费。
	text := fmt.Sprintf("Generated audio: %s\nMEDIA:%s", filepath.Base(path), path)
	return toolproviders.Response{Text: text}, nil
}
