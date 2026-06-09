package webfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Jina calls r.jina.ai, which proxies an arbitrary URL and returns
// LLM-friendly markdown. The free tier works without a key but is rate
// limited; an API key (Bearer) raises the quota. We mark this provider as
// CredentialFree so admins can use it key-less from the dashboard, and
// pass the key through as a Bearer token when one is configured.
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

	// r.jina.ai expects the target URL appended verbatim (not query-
	// escaped) — query-escaping breaks their router and yields 4xx.
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
	// Jina already returns clean markdown — no HTML stripping needed,
	// just truncate to the caller's cap.
	return toolproviders.Response{Text: truncate(strings.TrimSpace(string(body)), a.MaxLen)}, nil
}
