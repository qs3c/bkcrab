package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// maxAttachmentBytes限制单个附件，无论它是否
// 作为数据URL （内存中的base64 ）或HTTPS URL （流式传输）到达
// fetch ）。25 MB匹配Anthropic的每个图像信封，并防止
// 来自下沉网关的病理数据URL。
const maxAttachmentBytes = 25 * 1024 * 1024

// maxAttachmentNameLen在清理后限制调用方提供的文件名。
// 96大致是文件名在终端启动之前的最长
// 包裹在聊天气泡中，并且完全没有任何路径长度限制。
const maxAttachmentNameLen = 96

// 附件是来电者希望具体化为/workspace的一个项目
// 对于当前回合。URL是必需的（数据URL或http （ s ） URL ）;名称
// 是可选的，如果给出，将被消毒并用作磁盘上的
// 文件名，因此LLM会看到可读的内容，如“quarterly.pdf”
// 而不是`image_3jk7l_0.pdf`。
//
// 同名语义：
// -在一个回合内：具有相同名称的第二个附件是
// 消歧义为“<stem>-<idx><ext>” （令牌拼接，如果这也
// 碰撞）。没有无声损失。
// -交叉路口：重新上传相同的姓名将覆盖之前的
// /workspace中的文件。这与“将相同名称拖到
// 文件夹“心理模型并避免无限制的`notes-1.md`，
// `notes-2.md`, …累积。需要保留旧版本的呼叫者
// 版本必须自行更改名称。
type Attachment struct {
	URL  string
	Name string
}

// WriteSessionAttachments将用户附加的字节具体化为
// 客服代表的会话工作区技能（图像工具、文件读取器等）
// 可以通过/workspace/联系他们<filename>。每个网址都是以下其中之一：
// - 数据 URL：“数据：image/png；base64、iVBORw...”
// - HTTPS 网址："https://example.com/report.pdf"
//
// 记录并跳过每项错误—单个不良网址不得
// 整个回合都沉没了。按输入顺序返回相对文件名，
// 省略任何失败的内容。
//
// 为什么有三种说法：
//
// -主机工作区目录：涵盖无沙箱案例（主机EXEC使用此
// dir作为cwd ）和docker case （ a.workspacePath绑定安装在
// /工作区）。
// - workspace.Store.Put ： E2B/MULTIPOD的耐用切换—
// LifecyclePool的创建时水合物将其复制到/workspace上
// 下一个沙盒启动。
// -沙盒executor.WriteFile ：涵盖E2B中间会话案例，其中
// 沙盒已经水化，不会从应用商店中重新提取。
//
// Docker不需要第三次写入（绑定挂载使主机写入显示
// ） ，但调用它是无害的。房东写的也是
// 对E2B无害（无人读取的网关本地字节）。
func (a *Agent) WriteSessionAttachments(ctx context.Context, sessionID, projectID string, atts []Attachment) []string {
	if len(atts) == 0 {
		return nil
	}
	var paths []string
	// 从毫秒时间戳派生的简短的base36-ish令牌。
	// 足够长的时间来避免在任何逼真的聊天节奏中发生冲突（
	// 人类必须在同一毫秒内上传两次） ；
	// 足够短，以至于生成的文件名— `image_3jk7l_0.jpg` —
	// 读取为普通用户附件，而不是系统生成的临时文件
	// 文件。早期的“in_1777972819289_0.jpg”形状制作模型
	// 到达read_file/`file`/`identify`以“验证”上传
	// 然后传递给技能。
	token := strconv.FormatInt(time.Now().UnixMilli(), 36)
	if len(token) > 5 {
		token = token[len(token)-5:]
	}
	// 跟踪此批次中分配的名称，以便两个附件
	// 相同的呼叫者提供的姓名不会相互重击。交叉转向
	// 碰撞被故意留下来覆盖—重新上传
	// `notes.md`应该替换，而不是永远累积`notes-1.md`。
	used := make(map[string]struct{}, len(atts))
	for i, att := range atts {
		data, ext, err := decodeAttachment(ctx, att.URL)
		if err != nil {
			slog.Warn("attachment decode failed", "agent", a.name, "session", sessionID, "index", i, "error", err)
			continue
		}
		name := buildAttachmentName(att.Name, token, i, ext, used)
		used[name] = struct{}{}

		// 1.主机工作区目录（通过绑定挂载覆盖无沙箱+ Docker ）
		if a.workspacePath != "" {
			full := filepath.Join(a.workspacePath, name)
			if mkErr := os.MkdirAll(a.workspacePath, 0o755); mkErr == nil {
				if wErr := os.WriteFile(full, data, 0o644); wErr != nil {
					slog.Warn("attachment host write failed", "agent", a.name, "session", sessionID, "path", full, "error", wErr)
				}
			}
		}

		// 2.耐用的商店（通过生成水合物覆盖E2B/多豆荚）
		if a.workspaceStore != nil {
			if pErr := a.workspaceStore.Put(ctx, a.agentID, projectID, sessionID, name, strings.NewReader(string(data)), int64(len(data)), contentTypeFromExt(ext)); pErr != nil {
				slog.Warn("attachment store put failed", "agent", a.name, "session", sessionID, "path", name, "error", pErr)
			}
		}

		// 3.实时沙箱（涵盖E2B中期）。尽力而为；缺失
		// pool/get失败仅仅意味着下一个exec将从
		// 通过Hydrate-on-Create储存。
		if a.sandboxPool != nil {
			if ex, gErr := a.sandboxPool.Get(ctx, a.name, projectID, sessionID); gErr == nil && ex != nil {
				if _, wErr := ex.WriteFile(ctx, "/workspace/"+name, string(data)); wErr != nil {
					slog.Warn("attachment sandbox write failed", "agent", a.name, "session", sessionID, "path", name, "error", wErr)
				}
			}
		}

		paths = append(paths, name)
	}
	return paths
}

// decodeAttachment将数据URL或HTTPS URL转换为原始字节加上
// 尽力文件扩展名（ “.png” ， “.jpg” ，... ）。未知/缺失
// MIME映射到“.bin”。
func decodeAttachment(ctx context.Context, u string) ([]byte, string, error) {
	if strings.HasPrefix(u, "data:") {
		return decodeDataURL(u)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, "", fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	httpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(httpCtx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxAttachmentBytes {
		return nil, "", fmt.Errorf("attachment exceeds %d bytes", maxAttachmentBytes)
	}
	ext := extFromMIME(resp.Header.Get("Content-Type"))
	if ext == "" {
		ext = filepath.Ext(parsed.Path) // 回退到URL扩展
	}
	if ext == "" {
		ext = ".bin"
	}
	return body, ext, nil
}

func decodeDataURL(u string) ([]byte, string, error) {
	comma := strings.IndexByte(u, ',')
	if comma < 0 {
		return nil, "", fmt.Errorf("data url missing comma")
	}
	header := u[5:comma] // strip "data:"
	payload := u[comma+1:]

	var mime string
	isB64 := false
	for _, part := range strings.Split(header, ";") {
		switch {
		case part == "base64":
			isB64 = true
		case part == "":
			// noop —没有MIME的前导“data:”是合法的
		case mime == "":
			mime = part
		}
	}
	var data []byte
	if isB64 {
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode: %w", err)
		}
		data = decoded
	} else {
		decoded, err := url.QueryUnescape(payload)
		if err != nil {
			return nil, "", fmt.Errorf("urlencoded decode: %w", err)
		}
		data = []byte(decoded)
	}
	if len(data) > maxAttachmentBytes {
		return nil, "", fmt.Errorf("attachment exceeds %d bytes", maxAttachmentBytes)
	}
	ext := extFromMIME(mime)
	if ext == "" {
		ext = ".bin"
	}
	return data, ext, nil
}

func extFromMIME(ct string) string {
	// 剥离参数：“image/png; charset = …”→“image/png”
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.TrimSpace(strings.ToLower(ct)) {
	// 圖片
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/heic":
		return ".heic"
	case "image/svg+xml":
		return ".svg"
	// 文档—着陆，因为即使对于模型来说，真正的扩展也很重要
	// 无法本机读取字节，因为LLM选择其
	// 基于扩展名的工具。`.bin`使其达到
	// file/identify; `.pdf`使其达到正确的阅读器。
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/markdown", "text/x-markdown":
		return ".md"
	case "text/csv":
		return ".csv"
	case "text/html":
		return ".html"
	case "application/json":
		return ".json"
	case "application/xml", "text/xml":
		return ".xml"
	case "application/zip":
		return ".zip"
	}
	return ""
}

// buildAttachmentName将调用方的可选Name转换为保险箱
// 磁盘上的文件名。如果名称为空（或消毒为空） ，则
// 回到历史“image_<token>_<i><ext>”形状，以便存在
// 调用者看不到任何行为变化。如果Name存在，我们会保留它
// (sanitized) ，在用户省略时追加MIME派生的EXT ，
// 并通过后缀“-<i>”消除批内重复项的歧义。
func buildAttachmentName(raw, token string, idx int, ext string, used map[string]struct{}) string {
	clean := sanitizeAttachmentName(raw)
	if clean == "" {
		return fmt.Sprintf("image_%s_%d%s", token, idx, ext)
	}
	if path.Ext(clean) == "" && ext != "" {
		clean += ext
	}
	if _, dup := used[clean]; !dup {
		return clean
	}
	stem := strings.TrimSuffix(clean, path.Ext(clean))
	tail := path.Ext(clean)
	// 第一个消歧义： “<stem>-<idx><ext>”。通常是独一无二的，但
	// 如果用户显式命名了较早的附件，则可能会发生冲突
	// `report-2.pdf`和后来具有相同名称的
	// 在idx = 2处着陆。
	candidate := fmt.Sprintf("%s-%d%s", stem, idx, tail)
	if _, dup := used[candidate]; !dup {
		return candidate
	}
	// 最终回退：每回合令牌中的拼接。令牌是唯一的
	// 每个WriteSessionAttachments调用，因此“<stem>-<token> -<idx><ext>”
	// 在批次内无碰撞。
	return fmt.Sprintf("%s-%s-%d%s", stem, token, idx, tail)
}

// sanitizeAttachmentName剥离路径分隔符、父目录令牌、
// 控制字符，以及来自调用者提供的文件名的前导点。
// 如果没有可用的内容，则返回“” ，以便调用者可以回退。
// 使用path.Base （而不是filepath.Base ） ，因此
// 浏览器在Linux网关上进行相同的处理。
func sanitizeAttachmentName(raw string) string {
	if raw == "" {
		return ""
	}
	// 将Windows分隔符规范化为/so路径。Base可靠地提取
	// 无论我们在电线的哪一侧运行，最后一个组件。
	raw = strings.ReplaceAll(raw, `\`, "/")
	raw = path.Base(raw)
	// `path.Base ("..") = = ".."` ；显式拒绝。
	if raw == "." || raw == ".." {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r < 0x20, r == 0x7f:
			// 控制char — drop
		case r == '/', r == '\\', r == ':', r == 0:
			// 路径分隔符/驱动器前缀/NUL — DROP
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.TrimLeft(out, ".") // 很少使用hidden-dotfile前缀
	if len(out) > maxAttachmentNameLen {
		// 从茎截断，以便我们保留扩展。字节-
		// 在UTF-8上切片将切断多字节符文（ CJK文件名
		// 是3字节/字符） ，并在磁盘上生成无效的UTF-8 ，因此返回
		// 关闭到字节预算或低于字节预算的最近的符文边界。
		ext := path.Ext(out)
		stem := strings.TrimSuffix(out, ext)
		keep := maxAttachmentNameLen - len(ext)
		if keep < 1 {
			keep = 1
		}
		if len(stem) > keep {
			for keep > 0 && !utf8.RuneStart(stem[keep]) {
				keep--
			}
			stem = stem[:keep]
		}
		out = stem + ext
	}
	return out
}

func contentTypeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".heic":
		return "image/heic"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".md":
		return "text/markdown"
	case ".csv":
		return "text/csv"
	case ".html":
		return "text/html"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".zip":
		return "application/zip"
	}
	return ""
}
