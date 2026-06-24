package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
)

// ctxAwareExtractProvider 在 ctx 已取消时返回错误(模拟真实 LLM HTTP 客户端会因
// ctx 取消而立刻失败),否则回一段有效的技能提取 JSON。
type ctxAwareExtractProvider struct {
	resp string
}

func (p *ctxAwareExtractProvider) Chat(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &provider.Response{Content: p.resp}, nil
}

func (p *ctxAwareExtractProvider) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	return nil, nil
}

// TestSkillExtractionSurvivesCanceledRequestCtx 锁定:技能提取在流式回合的后台
// goroutine 里运行,此时 HTTP 请求 ctx 已取消。它要发一次完整的 LLM 调用,挂在已
// 取消的 ctx 上必然失败,等于流式路径上完全提炼不到任何技能。提取必须脱离请求取消。
func TestSkillExtractionSurvivesCanceledRequestCtx(t *testing.T) {
	workspace := t.TempDir()
	prov := &ctxAwareExtractProvider{
		resp: `{"extract":true,"skill":{"name":"Demo","slug":"demo","description":"d","content":"# Demo\n"}}`,
	}
	sl := NewSkillsLearner(workspace, prov, "test-model")

	// 模拟流式回合收尾:HTTP 请求 ctx 已取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sl.MaybeExtract(ctx, []provider.Message{{Role: "user", Content: "hi"}}, sl.minToolCalls); err != nil {
		t.Fatalf("MaybeExtract: %v", err)
	}

	skillPath := filepath.Join(workspace, "skills", "demo", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("expected skill written at %s, got: %v (技能提取必须脱离请求 ctx 的取消)", skillPath, err)
	}
}
