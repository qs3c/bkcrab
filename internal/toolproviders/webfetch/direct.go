package webfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Direct 是无密钥的内置获取器：net/http GET，剥离 HTML，截断。
// 它选择加入 CredentialFree，以便链在没有 API 密钥时也将其视为可用——
// 管理员从仪表盘中选择它的方式与选择其他提供商相同。
type Direct struct{}

func (Direct) Category() string     { return Category }
func (Direct) Name() string         { return "direct" }
func (Direct) CredentialFree() bool { return true }

const (
	directTimeout   = 30 * time.Second
	directUserAgent = "BkCrab/1.0 (AI Agent Web Fetcher)"
)

func (d *Direct) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, directTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("User-Agent", directUserAgent)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("direct fetch: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 将 429/5xx 提升为可重试，以便链回退到下一个提供商；
		// 4xx 配置类错误原样暴露。
		err := fmt.Errorf("direct HTTP %d", resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			return toolproviders.Response{}, toolproviders.Retry(err)
		default:
			return toolproviders.Response{}, err
		}
	}

	// 读取上限的 3 倍，因为 HTML 很冗长，且剥离标签后会大幅缩小——
	// 与旧版 direct 获取器使用的启发式算法相同。
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(a.MaxLen*3)))
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("direct read: %w", err))
	}
	text := truncate(stripHTML(string(body)), a.MaxLen)
	return toolproviders.Response{Text: text}, nil
}
