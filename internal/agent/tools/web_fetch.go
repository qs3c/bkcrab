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

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type webFetchArgs struct {
	URL    string `json:"url"`
	MaxLen int    `json:"max_length,omitempty"` // default 10000
}

const (
	defaultMaxLen  = 10000
	fetchTimeout   = 30 * time.Second
	fetchUserAgent = "BkCrab/1.0 (AI Agent Web Fetcher)"
)

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// safeFetchClient 是一个 http.Client，其拨号器拒绝私有，
// 环回、链路本地、多播和 CGNAT 地址 — SSRF
// web_fetch 的防御。在 DNS 完成之后，检查在 DIAL 时间运行
// 已解决，因此主机名指向 169.254.169.254（云
// 元数据）或 DNS 重新绑定技巧仍然会被阻止。我们拨打
// 直接解析IP而不是让net.Dial重新解析，
// 关闭我们的检查和实际连接之间的 TOCTOU。
var safeFetchClient = &http.Client{
	Timeout: fetchTimeout,
	Transport: &http.Transport{
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		IdleConnTimeout:       60 * time.Second,
	},
	// 限制重定向链，以便攻击者无法跟踪公共 URL
	// 一个内部的。每个重定向目标也会经过
	// safeDialContext，但有限的深度使请求保持有限。
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
	// 拨打我们已经验证过的第一个IP；将主机：端口传递回
	// net.Dialer 会进行第二次解析，攻击者控制
	// 权威 DNS 可以在我们的检查之间交换 169.254.169.254
	// 和表盘。
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
		// 100.64.0.0/10 — CGNAT，可以路由到某些提供商的内部基础设施
		if ip4[0] == 100 && ip4[1]&0xc0 == 0x40 {
			return true
		}
		// 169.254/16 由 IsLinkLocalUnicast 覆盖，但 AWS/GCP 元数据
		// 具体使用169.254.169.254；把它拼出来作为守卫
		// 如果未来的 Go 版本发布的话，读者和腰带和吊带者
		// 缩小 IsLinkLocalUnicast。
		if ip4[0] == 169 && ip4[1] == 254 {
			return true
		}
	}
	return false
}

func init() {
	// Register 将从 registerWebFetch 调用
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

// RegisterWebFetch 将 web_fetch 工具注册到内置的
// http.DefaultClient 后端。这是传统的零配置路径；来电者
// 想要提供者路由应该使用 RegisterWebFetchChain 代替并且
// 跳过这个。
//
// 该描述故意直白地描述了“在获取之前进行搜索”
// 规则和 URL 猜测失败模式。仅有的型号
// web_fetch 倾向于依赖训练内存 URL——这些 URL 是
// 经常陈旧/产生幻觉——并燃烧十几发404'ing。
// 在这里调用它，工具目录被序列化到
// 模型的提示，远比埋葬更有效
// SOUL.md 中的指导。
func RegisterWebFetch(r *Registry) {
	r.Register("web_fetch", webFetchDescription, webFetchSchema, webFetchToolWith(r))
}

// RegisterWebFetchChain 注册由 toolproviders.Chain 支持的 web_fetch。
// 当链为零或不可用时，注册回退到
// 传统的直接获取器，因此该工具保持可用 - 链优先，内置
// 倒退。工具名称+模式不变，因此模型的视角是
// 无论哪个后端为调用提供服务，都是相同的。
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
		// 在链路径上镜像直接获取器方案防护：
		// 上游提供商可能会也可能不会拒绝 file:// /
		// gopher:// / data:// 本身，我们不想依赖
		// 第三方执行我们的最低标准。同样检查
		// 非链分支在webFetchTool中运行。
		if err := assertHTTPScheme(args.URL); err != nil {
			return "", err
		}
		// 重复使用相同的每轮重复 URL 来保护直接
		// 使用 fetcher 使模型无法燃烧旋转的子弹
		// 猜测的 URL 全部为 404。无论哪个都适用
		// 提供商实际上正在支持这一呼吁。
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

// webFetchToolWith 将注册表绑定到工具闭包中，以便
// 实现时可以查阅转状态失败历史记录。包装
// 而不是让该工具进入每个代理的全局保留
// 当同一进程为多个代理服务时，注册表会被隔离。
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

	// 拒绝重试本轮已失败的 URL。这
	// 代理循环的循环检测器捕获“完全相同的调用 3 次”
	// 一行”，但不是更常见的模式：模型旋转
	// 通过 5 个猜测的 URL 全部都是 404，然后回到
	// 第一个猜测。每次尝试对于循环来说看起来“不同”
	// 检测器，但我们知道用户正在为往返付费
	// 前期会失败。
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

	// 限制读取正文以防止内存问题
	limitReader := io.LimitReader(resp.Body, int64(maxLen*3)) // read more than needed since HTML is verbose
	body, err := io.ReadAll(limitReader)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	// 去除 HTML 标签
	text := stripHTML(string(body))

	// 截断至最大长度
	if len(text) > maxLen {
		text = text[:maxLen] + "\n[...truncated]"
	}

	return text, nil
}

// assertHTTPScheme 预先拒绝非 http(s) URL。文件:// 会
// 让该工具读取主机文件系统； gopher:// / ftp:// / 数据://
// 打开我们从未打算支持的奇怪表面。两者都直接
// fetcher 和 toolproviders-chain fetcher 称此为门
// 无论哪个后端为调用提供服务，都是统一的。
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

// stripHTML 删除 HTML 标签并清理空白。
func stripHTML(html string) string {
	// 完全删除脚本和样式元素
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	// 删除 HTML 标签
	text := htmlTagRe.ReplaceAllString(html, " ")

	// 解码常见的 HTML 实体
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	// 折叠空白
	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")

	// 折叠多个换行符
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}
