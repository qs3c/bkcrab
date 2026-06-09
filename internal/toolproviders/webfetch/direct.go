package webfetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Direct is the no-key built-in fetcher: net/http GET, strip HTML, truncate.
// It opts into CredentialFree so the chain treats it as available even
// without an API key — admins pick it from the dashboard the same way they
// pick any other provider.
type Direct struct{}

func (Direct) Category() string       { return Category }
func (Direct) Name() string           { return "direct" }
func (Direct) CredentialFree() bool   { return true }

const (
	directTimeout   = 30 * time.Second
	directUserAgent = "FastClaw/1.0 (AI Agent Web Fetcher)"
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
		// Promote 429/5xx to retriable so the chain falls through to the
		// next provider; 4xx config-style errors are surfaced as-is.
		err := fmt.Errorf("direct HTTP %d", resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			return toolproviders.Response{}, toolproviders.Retry(err)
		default:
			return toolproviders.Response{}, err
		}
	}

	// Read 3× the cap because HTML is verbose and stripping tags shrinks
	// it substantially — same heuristic the legacy direct fetcher used.
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(a.MaxLen*3)))
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("direct read: %w", err))
	}
	text := truncate(stripHTML(string(body)), a.MaxLen)
	return toolproviders.Response{Text: text}, nil
}
