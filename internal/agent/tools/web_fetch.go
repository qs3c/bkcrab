package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bkclaw-ai/bkclaw/internal/toolproviders"
)

type webFetchArgs struct {
	URL    string `json:"url"`
	MaxLen int    `json:"max_length,omitempty"` // default 10000
}

const (
	defaultMaxLen  = 10000
	fetchTimeout   = 30 * time.Second
	fetchUserAgent = "BkClaw/1.0 (AI Agent Web Fetcher)"
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// safeFetchClient is an http.Client whose dialer rejects private,
// loopback, link-local, multicast, and CGNAT addresses — the SSRF
// defense for web_fetch. The check runs at DIAL time, after DNS has
// resolved, so a hostname that points at 169.254.169.254 (cloud
// metadata) or a DNS-rebinding trick still gets stopped. We dial the
// resolved IP directly instead of letting net.Dial re-resolve, to
// close the TOCTOU between our check and the actual connection.
var safeFetchClient = &http.Client{
	Timeout: fetchTimeout,
	Transport: &http.Transport{
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	},
	// Cap redirect chains so an attacker can't follow a public URL into
	// an internal one. Each redirect target also goes through
	// safeDialContext, but bounded depth keeps the request finite.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses for %s", host)
	}
	for _, ip := range ips {
		if isBlockedAddr(ip.IP) {
			return nil, fmt.Errorf("blocked address %s for host %s", ip.IP, host)
		}
	}
	// Dial the first IP we already validated; passing host:port back to
	// net.Dialer would do a second resolution and an attacker controlling
	// authoritative DNS could swap in 169.254.169.254 between our check
	// and the dial.
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

func isBlockedAddr(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.IsPrivate() { // 10/8, 172.16/12, 192.168/16, fc00::/7
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 100.64.0.0/10 — CGNAT, can route to internal infra at some providers
		if ip4[0] == 100 && ip4[1]&0xc0 == 0x40 {
			return true
		}
		// 169.254/16 covered by IsLinkLocalUnicast, but AWS/GCP metadata
		// uses 169.254.169.254 specifically; spell it out as a guard for
		// readers and as belt-and-suspenders if a future Go release ever
		// narrows IsLinkLocalUnicast.
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

func init() {
	// Register will be called from registerWebFetch
}

const webFetchDescription = "Fetch a single known URL and return its plain text. " +
	"If the user's message itself contains a URL or bare domain " +
	"(e.g. 'idoubi.ai', 'https://example.com/cv'), fetch THAT URL " +
	"directly — prepend https:// for bare domains — instead of " +
	"running web_search first. DO NOT guess URLs from memory: " +
	"your training data has stale paths and you will burn rounds " +
	"on 404s. When the user described a page in natural language " +
	"with no URL, run web_search first to discover the URL, then " +
	"web_fetch that exact URL. If web_search isn't available, " +
	"prefer well-known stable hosts (en.wikipedia.org, github.com), " +
	"not date-stamped article URLs. A URL that returned 4xx/5xx " +
	"earlier in this turn will be refused if you retry it."

var webFetchSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"url": map[string]interface{}{
			"type":        "string",
			"description": "The exact URL to fetch (full https://… form). Don't paste search-result snippets or guessed paths.",
		},
		"max_length": map[string]interface{}{
			"type":        "integer",
			"description": "Maximum characters to return (default 10000)",
		},
	},
	"required": []string{"url"},
}

// RegisterWebFetch registers the web_fetch tool with the built-in
// http.DefaultClient backend. This is the legacy zero-config path; callers
// that want provider routing should use RegisterWebFetchChain instead and
// skip this.
//
// The description is deliberately blunt about the "search-before-fetch"
// rule and the URL-guessing failure mode. Models that have only
// web_fetch tend to fall back on training-memory URLs — which are
// often stale / hallucinated — and burn a dozen rounds 404'ing.
// Calling that out here, where the tool catalog gets serialized into
// the model's prompt, is far more effective than burying the
// guidance in SOUL.md.
func RegisterWebFetch(r *Registry) {
	r.Register("web_fetch", webFetchDescription, webFetchSchema, webFetchToolWith(r))
}

// RegisterWebFetchChain registers web_fetch backed by a toolproviders.Chain.
// When the chain is nil or unavailable, the registration falls back to the
// legacy direct fetcher so the tool stays available — chain-first, built-in
// fallback. Tool name + schema are unchanged so the model's perspective is
// identical regardless of which backend services the call.
func RegisterWebFetchChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil || !chain.Available() {
		RegisterWebFetch(r)
		return
	}
	r.Register("web_fetch", webFetchDescription, webFetchSchema, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args webFetchArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.URL == "" {
			return "", fmt.Errorf("url is required")
		}
		// Mirror the direct-fetcher scheme guard on the chain path:
		// the upstream provider may or may not reject file:// /
		// gopher:// / data:// itself, and we'd rather not depend on
		// a third-party to enforce our minimum bar. Same check the
		// non-chain branch runs in webFetchTool.
		if err := assertHTTPScheme(args.URL); err != nil {
			return "", err
		}
		// Re-use the same per-turn duplicate-URL guard the direct
		// fetcher uses so the model can't burn rounds rotating through
		// guessed URLs that all 404. Applies regardless of which
		// provider is actually backing the call.
		if r != nil {
			if prev := r.PriorFailure("web_fetch", string(rawArgs)); prev != "" {
				return "", fmt.Errorf(
					"already tried %s earlier in this turn (%s). DO NOT retry the same URL — pick a different source, or use web_search to find a verified URL",
					args.URL, prev,
				)
			}
		}
		raw := map[string]any{"url": args.URL}
		if args.MaxLen > 0 {
			raw["max_length"] = args.MaxLen
		}
		resp, err := chain.Execute(ctx, raw)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}

// webFetchToolWith binds the registry into the tool closure so the
// implementation can consult turn-state failure history. Wrapping
// instead of having the tool reach into a global keeps per-agent
// registries isolated when the same process serves multiple agents.
func webFetchToolWith(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		return webFetchTool(ctx, r, rawArgs)
	}
}

func webFetchTool(ctx context.Context, r *Registry, rawArgs json.RawMessage) (string, error) {
	var args webFetchArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}

	if args.URL == "" {
		return "", fmt.Errorf("url is required")
	}

	if err := assertHTTPScheme(args.URL); err != nil {
		return "", err
	}

	// Refuse a retry of a URL that already failed in this turn. The
	// agent loop's loop-detector catches "exact same call 3 times in
	// a row" but not the more common pattern: the model rotates
	// through five guessed URLs that all 404, then comes back to the
	// first guess. Each attempt looks "different" to the loop
	// detector but the user is paying for round-trips that we know
	// up-front will fail.
	if r != nil {
		if prev := r.PriorFailure("web_fetch", string(rawArgs)); prev != "" {
			return "", fmt.Errorf(
				"already tried %s earlier in this turn (%s). DO NOT retry the same URL — pick a different source, or use web_search to find a verified URL",
				args.URL, prev,
			)
		}
	}

	maxLen := args.MaxLen
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, args.URL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", fetchUserAgent)

	resp, err := safeFetchClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch url: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Read body with a limit to prevent memory issues
	limitReader := io.LimitReader(resp.Body, int64(maxLen*3)) // read more than needed since HTML is verbose
	body, err := io.ReadAll(limitReader)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	// Strip HTML tags
	text := stripHTML(string(body))

	// Truncate to max length
	if len(text) > maxLen {
		text = text[:maxLen] + "\n[...truncated]"
	}

	return text, nil
}

// assertHTTPScheme rejects non-http(s) URLs up front. file:// would
// let the tool read the host filesystem; gopher:// / ftp:// / data://
// open weird surfaces we never intended to support. Both the direct
// fetcher and the toolproviders-chain fetcher call this so the gate
// is uniform regardless of which backend services the call.
func assertHTTPScheme(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if scheme := strings.ToLower(u.Scheme); scheme != "http" && scheme != "https" {
		return fmt.Errorf("scheme %q not allowed; use http or https", u.Scheme)
	}
	return nil
}

// stripHTML removes HTML tags and cleans up whitespace.
func stripHTML(html string) string {
	// Remove script and style elements entirely
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	// Remove HTML tags
	text := htmlTagRe.ReplaceAllString(html, " ")

	// Decode common HTML entities
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	// Collapse whitespace
	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")

	// Collapse multiple newlines
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}
