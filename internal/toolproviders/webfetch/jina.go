package webfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Jina 调用 r.jina.ai，它代理任意 URL 并返回对 LLM 友好的 markdown。
// 免费层级无需密钥即可使用但有速率限制；API 密钥（Bearer）可提高配额。
// 我们将此提供商标记为 CredentialFree，以便管理员可以从仪表盘无密钥使用，
// 并在配置了密钥时将其作为 Bearer 令牌传递。
type Jina struct{}

func (Jina) Category() string     { return Category }
func (Jina) Name() string         { return "jina" }
func (Jina) CredentialFree() bool { return true }

const (
	jinaTimeout = 30 * time.Second
	jinaBase    = "https://r.jina.ai/"
)

func (j *Jina) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, jinaTimeout)
	defer cancel()

	// r.jina.ai 期望目标 URL 原样追加（不进行查询转义）——
	// 查询转义会破坏其路由器并导致 4xx 错误。
	target := jinaBase + strings.TrimSpace(a.URL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Accept", "text/plain")
	if k := strings.TrimSpace(req.Config.APIKey); k != "" {
		httpReq.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("jina request: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("jina HTTP %d", resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			return toolproviders.Response{}, toolproviders.Retry(err)
		default:
			return toolproviders.Response{}, err
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(a.MaxLen*3)))
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("jina read: %w", err))
	}
	// Jina 已返回干净的 markdown——无需剥离 HTML，只需截断到调用方的上限。
	return toolproviders.Response{Text: truncate(strings.TrimSpace(string(body)), a.MaxLen)}, nil
}
