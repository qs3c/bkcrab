package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// Boxlite REST 沙箱提供者——遵循 OpenAPI 规范
// https://github.com/boxlite-ai/boxlite/blob/main/openapi/rest-sandbox-open-api.yaml
//
// 认证是作为 `Authorization: Bearer <apikey>` 发送的静态 API 密钥。
// 早期版本对 /oauth/tokens 进行了 OAuth2 client_credentials 交换——
// 该端点已被上游移除；操作员粘贴的密钥现在直接是 bearer 令牌。
// BoxliteClientID 保留在配置结构上以实现向后兼容（现有管理行继续工作），
// 但它不再发送到任何地方。
//
// 生命周期：
//   POST   /{prefix}/boxes          创建 box（已配置）
//   POST   /{prefix}/boxes/{id}/start  启动 VM（我们急切地执行此操作，
//                                       以便 Hydrate 有运行目标）
//   PUT    /{prefix}/boxes/{id}/files?path=/  application/x-tar 批量上传
//   POST   /{prefix}/boxes/{id}/exec  → 返回 {execution_id}
//   GET    /{prefix}/boxes/{id}/executions/{exec_id}/attach
//          → WebSocket 双向，二进制帧为 [channel:u8][bytes]
//            (0x01 stdout, 0x02 stderr)，文本帧 {"type":"exit","exit_code":N}
//            完成后跟随正常关闭。
//   DELETE /{prefix}/boxes/{id}?force=true

const (
	// BoxLite Cloud 开发环境——唯一经过端到端验证的公共端点
	//（OAuth + createBox + attach）。OpenAPI servers 节点声明
	// `https://api.boxlite.ai/v1`，但该主机位于 Cloudflare Access 后面，
	// 并以 403 HTML 墙拒绝普通的 client_credentials。
	// 当 BoxLite 发布可公开访问的生产端点时，更新此默认值——
	// 在此之前，生产租户的操作员必须在管理 UI 中显式设置其 URL。
	defaultBoxliteURL      = "https://api.dev.boxlite.ai/api/v1"
	defaultBoxliteClientID = "default"
	defaultBoxlitePrefix   = "default"
	defaultBoxliteImage    = "thinkany/bkclaw-sandbox:latest"
)

// BoxliteExecutor 针对远程 Boxlite REST API 实现 Executor。
type BoxliteExecutor struct {
	baseURL string // 已去除尾部斜杠
	prefix  string
	// clientID 是旧的 OAuth2 client_id，保留在结构体上，
	// 以便设置它的旧配置行不会破坏构造函数。
	// 在 apikey-as-bearer 切换后未使用。
	clientID string
	apiKey   string
	image    string
	timeout  time.Duration

	client *http.Client

	mu    sync.Mutex
	boxID string

	// 填充源——与 E2BExecutor 使用的形状相同，以便 recreate()
	// 可以在 box 消失时从头重建 /skills + /workspace。
	skillDirs []string
	workspace workspace.Store
	agentID   string
	projectID string
	sessionID string
}

func newBoxliteExecutor(ctx context.Context, baseURL, prefix, clientID, apiKey, image string, timeout time.Duration) (*BoxliteExecutor, error) {
	if baseURL == "" {
		baseURL = defaultBoxliteURL
	}
	if prefix == "" {
		prefix = defaultBoxlitePrefix
	}
	if clientID == "" {
		clientID = defaultBoxliteClientID
	}
	if image == "" {
		image = defaultBoxliteImage
	}
	if apiKey == "" {
		return nil, fmt.Errorf("boxlite: apikey is required")
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	// 没有全局 http.Client.Timeout：exec 往返可能需要数分钟
	//（长时间运行的构建、图像生成等）。我们通过 context.WithTimeout
	// 在请求级别绑定——与 E2BExecutor 在 60 秒流截断错误后采用的模式相同。
	e := &BoxliteExecutor{
		baseURL:  strings.TrimRight(baseURL, "/"),
		prefix:   prefix,
		clientID: clientID,
		apiKey:   apiKey,
		image:    image,
		timeout:  timeout,
		client:   &http.Client{},
	}

	if err := e.createBox(ctx); err != nil {
		return nil, fmt.Errorf("boxlite create box: %w", err)
	}
	if err := e.startBox(ctx); err != nil {
		// 如果启动失败，尽力清理，以免在服务器上泄漏已配置但从未启动的 box。
		_ = e.Close()
		return nil, fmt.Errorf("boxlite start box: %w", err)
	}
	return e, nil
}

// authHeader 返回 "Bearer <apikey>"。在新认证方案中，apikey 就是 bearer 令牌——
// 无需交换、无需缓存、无需过期处理。
// 返回 (string, error) 形状，以便调用者（以前需要处理令牌刷新失败）
// 无需重写；错误结果今天始终为 nil。
func (e *BoxliteExecutor) authHeader(_ context.Context) (string, error) {
	return "Bearer " + e.apiKey, nil
}

func (e *BoxliteExecutor) prefixPath(suffix string) string {
	return fmt.Sprintf("%s/%s%s", e.baseURL, e.prefix, suffix)
}

func (e *BoxliteExecutor) createBox(ctx context.Context) error {
	body, _ := json.Marshal(map[string]interface{}{
		"image": e.image,
		// auto_remove 防止服务器累积已停止的 box，
		// 当我们的 Close() 因网络争用而丢弃显式 DELETE 时——
		// 它们在停止时会自动收集。
		"auto_remove": true,
	})
	createCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(createCtx, "POST", e.prefixPath("/boxes"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var box struct {
		BoxID string `json:"box_id"`
	}
	if err := json.Unmarshal(respBody, &box); err != nil {
		return fmt.Errorf("decode box: %w", err)
	}
	if box.BoxID == "" {
		return fmt.Errorf("empty box_id in response: %s", string(respBody))
	}
	e.mu.Lock()
	e.boxID = box.BoxID
	e.mu.Unlock()
	slog.Info("boxlite box created", "boxID", box.BoxID, "image", e.image)
	return nil
}

func (e *BoxliteExecutor) startBox(ctx context.Context) error {
	startCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(startCtx, "POST", e.prefixPath("/boxes/"+e.boxID+"/start"), nil)
	if err != nil {
		return err
	}
	auth, err := e.authHeader(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// SetHydrationSources 记录 Hydrate() 应从中拉取的输入。
// 与 E2BExecutor 形状相同——在构造后设置，以便 recreate() 可以重放。
func (e *BoxliteExecutor) SetHydrationSources(skillDirs []string, ws workspace.Store, agentID, projectID, sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.skillDirs = append(e.skillDirs[:0], skillDirs...)
	e.workspace = ws
	e.agentID = agentID
	e.projectID = projectID
	e.sessionID = sessionID
}

// Hydrate 将 /skills 和 /workspace 打包成一个 tar 并通过
// `PUT /files?path=/` 推送。与 E2B 路径不同，
// 我们不需要 base64 + exec 的把戏——Boxlite 的 Files API
// 接受原始 tar 字节并直接在目标位置解压。
func (e *BoxliteExecutor) Hydrate(ctx context.Context) error {
	bundle := newPlainTarBundle()

	skillCount := 0
	skillFileCount := 0
	seen := make(map[string]bool)
	for _, dir := range e.skillDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			if seen[name] {
				continue
			}
			seen[name] = true
			n, err := bundle.addLocalDir(filepath.Join(dir, name), "skills/"+name)
			if err != nil {
				slog.Warn("boxlite hydrate: skill tar", "skill", name, "error", err)
				continue
			}
			skillCount++
			skillFileCount += n
		}
	}

	workspaceCount := 0
	if e.workspace != nil {
		listProject := e.projectID
		listSession := e.sessionID
		if e.projectID != "" {
			listSession = ""
		}
		objs, err := e.workspace.List(ctx, e.agentID, listProject, listSession)
		if err != nil {
			slog.Warn("boxlite hydrate: workspace list", "agent", e.agentID, "error", err)
		} else {
			for _, obj := range objs {
				rc, err := e.workspace.Get(ctx, e.agentID, listProject, listSession, obj.Path)
				if err != nil {
					slog.Warn("boxlite hydrate: workspace get", "path", obj.Path, "error", err)
					continue
				}
				data, rerr := io.ReadAll(rc)
				rc.Close()
				if rerr != nil {
					slog.Warn("boxlite hydrate: workspace read", "path", obj.Path, "error", rerr)
					continue
				}
				rel := strings.TrimPrefix(obj.Path, "/")
				if err := bundle.addBytes("workspace/"+rel, data, 0o644, obj.ModTime); err != nil {
					slog.Warn("boxlite hydrate: workspace tar", "path", obj.Path, "error", err)
					continue
				}
				workspaceCount++
			}
		}
	}

	// 始终确保远程端存在父目录，即使 tar 是空的——
	// 否则代理的第一次 write_file 会因 ENOENT 失败。
	if err := bundle.ensureDir("skills/"); err != nil {
		return fmt.Errorf("tar skills dir: %w", err)
	}
	if err := bundle.ensureDir("workspace/"); err != nil {
		return fmt.Errorf("tar workspace dir: %w", err)
	}
	if err := bundle.close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}

	// BoxLite Files API 说明：尽管 OpenAPI 规范声称
	// "Uploads a tar archive and extracts it at the specified path"，
	// 但开发云的 PUT /files 并不解压。经验结果：
	//   - path = 文件路径 → 将请求正文原样写入该文件（父目录自动创建）。204。
	//   - path = 现有目录 + Content-Type x-tar →
	//     将原始 tar 存储为 `boxlite-upload-<rand>.tar` 在其中。
	//     仍然是 204——对我们的填充目的来说静默错误。
	// 我们通过以下方式解决：
	//   1. 将 tar PUT 到确定性文件路径 `/tmp/hydrate.tar`
	//   2. exec `tar -xf /tmp/hydrate.tar -C /` 实际解压
	//   3. 删除临时文件以保持 /tmp 干净
	// 当一个技能包有数十个文件时，一次上传 + 一次 exec
	// 仍然比逐个文件的 PUT 更便宜。
	const stagingPath = "/tmp/fc-hydrate.tar"
	uploadCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	uploadURL := e.prefixPath("/boxes/"+e.boxID+"/files") +
		"?path=" + url.QueryEscape(stagingPath) + "&overwrite=true"
	req, err := http.NewRequestWithContext(uploadCtx, "PUT", uploadURL, bytes.NewReader(bundle.buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("upload tar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload tar HTTP %d: %s", resp.StatusCode, string(body))
	}

	// 通过 exec 解压。tar -C / + 包路径如 "skills/..." 将内容
	// 放置到 /skills/...——匹配代理工具期望的内容
	//（python /skills/<name>/main.py）以及 Docker 后端通过绑定挂载
	// 提供的内容。之后 rm 保持 /tmp 在 recreate() 周期中整洁。
	extractCmd := fmt.Sprintf("tar -xf %s -C / && rm -f %s", stagingPath, stagingPath)
	if _, err := e.execOnce(ctx, extractCmd, 60*time.Second); err != nil {
		return fmt.Errorf("extract hydrate tar: %w", err)
	}

	slog.Info("boxlite sandbox hydrated",
		"boxID", e.boxID,
		"skills", skillCount,
		"skillFiles", skillFileCount,
		"workspaceFiles", workspaceCount,
		"tarBytes", bundle.buf.Len())
	return nil
}

// plainTarBundle 是 e2b 的 tarBundle 的 boxlite 变体：无 gzip，
// 因为 Files API 期望 application/x-tar。addLocalDir/addBytes/ensureDir
// 镜像 e2b 辅助函数。
type plainTarBundle struct {
	buf       bytes.Buffer
	tw        *tar.Writer
	fileCount int
}

func newPlainTarBundle() *plainTarBundle {
	b := &plainTarBundle{}
	b.tw = tar.NewWriter(&b.buf)
	return b
}

func (b *plainTarBundle) addBytes(name string, data []byte, mode int64, modTime time.Time) error {
	if modTime.IsZero() {
		modTime = time.Now()
	}
	if err := b.tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(name, "/"),
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
		ModTime:  modTime,
	}); err != nil {
		return err
	}
	if _, err := b.tw.Write(data); err != nil {
		return err
	}
	b.fileCount++
	return nil
}

func (b *plainTarBundle) ensureDir(name string) error {
	return b.tw.WriteHeader(&tar.Header{
		Name:     strings.TrimPrefix(name, "/"),
		Mode:     0o755,
		Typeflag: tar.TypeDir,
		ModTime:  time.Now(),
	})
}

func (b *plainTarBundle) addLocalDir(localRoot, prefix string) (int, error) {
	count := 0
	err := filepath.Walk(localRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(localRoot, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		name := prefix + "/" + filepath.ToSlash(rel)
		if err := b.addBytes(name, data, int64(info.Mode().Perm()), info.ModTime()); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func (b *plainTarBundle) close() error { return b.tw.Close() }

// Exec 通过异步 exec + WebSocket attach 对运行 shell 命令。
// 在 404（box 消失）时，我们重新创建并重试一次——匹配 E2B 的
// 过期重新创建模式。
func (e *BoxliteExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	wrapped := "cd /workspace && " + command
	out, err := e.execOnce(ctx, wrapped, timeout)
	if err != nil && isBoxliteGone(err) {
		if rerr := e.recreate(ctx); rerr != nil {
			return "", fmt.Errorf("recreate after stale box: %w (original: %v)", rerr, err)
		}
		return e.execOnce(ctx, wrapped, timeout)
	}
	return out, err
}

func isBoxliteGone(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "HTTP 404") || strings.Contains(s, "HTTP 502") || strings.Contains(s, "HTTP 410")
}

func (e *BoxliteExecutor) recreate(ctx context.Context) error {
	slog.Info("boxlite box gone, recreating", "oldBoxID", e.boxID)
	// 静态 apikey——无需令牌刷新；只需重建 box 状态。
	if err := e.createBox(ctx); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	if err := e.startBox(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := e.Hydrate(ctx); err != nil {
		return fmt.Errorf("hydrate: %w", err)
	}
	return nil
}

// execOnce 启动一个执行并通过附加的 WebSocket 流式传输其输出，
// 直到服务器发出 {"type":"exit",...} 并关闭。
func (e *BoxliteExecutor) execOnce(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	body, _ := json.Marshal(map[string]interface{}{
		"command":         "/bin/sh",
		"args":            []string{"-c", command},
		"timeout_seconds": timeout.Seconds(),
	})
	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	req, err := http.NewRequestWithContext(startCtx, "POST", e.prefixPath("/boxes/"+e.boxID+"/exec"), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("boxlite exec start: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("boxlite exec start HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var ex struct {
		ExecutionID string `json:"execution_id"`
	}
	if err := json.Unmarshal(respBody, &ex); err != nil {
		return "", fmt.Errorf("decode exec start: %w (%s)", err, string(respBody))
	}
	if ex.ExecutionID == "" {
		return "", fmt.Errorf("empty execution_id: %s", string(respBody))
	}

	return e.attachAndDrain(ctx, ex.ExecutionID, timeout)
}

// attachAndDrain 打开附加的 WebSocket 并读取帧，直到退出消息或
// 父上下文截止时间。二进制帧是带有通道标记的 stdout/stderr 有效负载；
// 文本帧是控制 JSON。
func (e *BoxliteExecutor) attachAndDrain(ctx context.Context, execID string, timeout time.Duration) (string, error) {
	wsURL, err := e.attachWebsocketURL(execID)
	if err != nil {
		return "", err
	}
	auth, err := e.authHeader(ctx)
	if err != nil {
		return "", err
	}
	hdr := http.Header{}
	hdr.Set("Authorization", auth)

	// 给 WS 拨号一个短限制——如果升级将失败（令牌错误、exec_id 错误、
	// box 未运行），它将快速失败。
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDial()
	dialer := websocket.DefaultDialer
	conn, dialResp, err := dialer.DialContext(dialCtx, wsURL, hdr)
	if err != nil {
		status := 0
		body := ""
		if dialResp != nil {
			status = dialResp.StatusCode
			b, _ := io.ReadAll(dialResp.Body)
			body = string(b)
			dialResp.Body.Close()
		}
		return "", fmt.Errorf("boxlite attach: %w (HTTP %d: %s)", err, status, body)
	}
	defer conn.Close()

	// 流截止时间：用户提供的工具超时 + 30 秒余量，
	// 以便服务器在我们放弃之前有时间刷新尾部帧。
	streamCtx, cancelStream := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancelStream()
	conn.SetReadDeadline(time.Now().Add(timeout + 30*time.Second))

	// 在每个 Pong 上重置读取截止时间，以便 keepalive（服务器根据规范每 15 秒 ping）
	// 在长时间运行的 exec 中保持连接活跃。
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(timeout + 30*time.Second))
		return nil
	})

	// 异步取消：如果 streamCtx 超时或父 ctx 被取消，
	// 推动连接关闭，以便阻塞的 ReadMessage 返回。
	doneReading := make(chan struct{})
	go func() {
		select {
		case <-streamCtx.Done():
			_ = conn.Close()
		case <-doneReading:
		}
	}()
	defer close(doneReading)

	var stdout, stderr bytes.Buffer
	exitCode := -1
	exited := false

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if exited {
				break
			}
			// 服务器在退出文本帧之后发送的正常关闭
			// 作为 websocket.CloseError(1000) 到达。
			// 如果我们已经看到上面的退出帧，则视为成功；否则显示错误。
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				break
			}
			return combineOutput(&stdout, &stderr), fmt.Errorf("attach read: %w", err)
		}
		switch msgType {
		case websocket.BinaryMessage:
			if len(data) == 0 {
				continue
			}
			channel := data[0]
			payload := data[1:]
			switch channel {
			case 0x01:
				stdout.Write(payload)
			case 0x02:
				stderr.Write(payload)
			default:
				// 未知通道；规范目前只定义了 1 和 2，
				// 但未来的服务器可能会添加更多。静默丢弃，
				// 而不是破坏合并后的输出。
			}
		case websocket.TextMessage:
			var ctrl struct {
				Type     string `json:"type"`
				ExitCode int    `json:"exit_code"`
				Message  string `json:"message"`
			}
			if json.Unmarshal(data, &ctrl) != nil {
				continue
			}
			switch ctrl.Type {
			case "exit":
				exitCode = ctrl.ExitCode
				exited = true
				// 服务器即将关闭。停止读取；下一个 ReadMessage 将看到关闭帧。
			case "error":
				// 根据规范非致命——连接保持打开。记录日志并继续读取，
				// 以便我们仍然捕获退出帧。
				slog.Warn("boxlite attach error frame", "boxID", e.boxID, "exec", execID, "message", ctrl.Message)
			}
		}
		if exited {
			// 再尝试读取一帧以等待关闭到来，
			// 然后在上面的读取错误时退出。带有短截止时间的 ReadMessage
			// 保持此循环有界。
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		}
	}

	output := combineOutput(&stdout, &stderr)
	slog.Info("boxlite exec completed", "boxID", e.boxID, "exec", execID, "exitCode", exitCode, "exited", exited, "outputLen", len(output))
	if !exited {
		return output, fmt.Errorf("exec did not emit exit frame before deadline")
	}
	if exitCode != 0 {
		if output == "" {
			output = fmt.Sprintf("Process exited with code %d", exitCode)
		}
		return output, fmt.Errorf("exit code %d", exitCode)
	}
	return output, nil
}

func combineOutput(stdout, stderr *bytes.Buffer) string {
	out := stdout.String()
	if stderr.Len() > 0 {
		if out != "" {
			out += "\n"
		}
		out += stderr.String()
	}
	return strings.TrimSpace(out)
}

// attachWebsocketURL 通过将 http(s) 基础 URL 镜像到 ws(s) 方案上，
// 构建附加端点的 wss:// URL。
func (e *BoxliteExecutor) attachWebsocketURL(execID string) (string, error) {
	u, err := url.Parse(e.baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported scheme %q in boxlite URL", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + e.prefix + "/boxes/" + e.boxID + "/executions/" + execID + "/attach"
	return u.String(), nil
}

func (e *BoxliteExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	return e.Exec(ctx, fmt.Sprintf("cat %s", shellQuote(path)), 30*time.Second)
}

func (e *BoxliteExecutor) WriteFile(ctx context.Context, filePath, content string) (string, error) {
	// BoxLite Files API 特性：当路径指向具体文件路径时，
	// 请求正文按原样写入，父目录自动创建（经验验证——参见 Hydrate 的说明）。
	// 我们以前使用 heredoc-over-exec 的方式，在内容包含随机标记
	// 和二进制内容时会失败；PUT 是二进制安全的，完全跳过 shell。
	uploadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	uploadURL := e.prefixPath("/boxes/"+e.boxID+"/files") +
		"?path=" + url.QueryEscape(filePath) + "&overwrite=true"
	req, err := http.NewRequestWithContext(uploadCtx, "PUT", uploadURL, strings.NewReader(content))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	auth, err := e.authHeader(ctx)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("boxlite write: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("boxlite write HTTP %d: %s", resp.StatusCode, string(body))
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), filePath), nil
}

func (e *BoxliteExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return e.Exec(ctx, fmt.Sprintf("ls -la %s", shellQuote(path)), 10*time.Second)
}

// shellQuote 用单引号引用路径，以便安全地包含在 shell 命令中。
// shellQuote 在 docker_executor.go 中声明并在包范围内共享；
// boxlite 重用相同的辅助函数而不是重新声明。

// Backend 返回 "boxlite"——用于每执行日志行，以便操作员可以
// 一目了然地确认哪个提供者处理了给定的工具调用。
func (e *BoxliteExecutor) Backend() string { return "boxlite" }

// IsRemoteWorkspace 将此执行器标记为云托管，
// 以便 LifecyclePool 在每次 exec 后运行 SnapshotWorkspace。
// 与 E2BExecutor 遵循的相同契约。
func (e *BoxliteExecutor) IsRemoteWorkspace() {}

// SnapshotWorkspace 通过 Files API 将 /workspace 作为 tar 下载，
// 并返回 LifecyclePool 需要将沙箱端写入镜像回持久化 workspace.Store 的
//（路径 → 字节）映射。
func (e *BoxliteExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	u := e.prefixPath("/boxes/"+e.boxID+"/files") + "?path=/workspace"
	req, err := http.NewRequestWithContext(dlCtx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	auth, err := e.authHeader(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("snapshot HTTP %d: %s", resp.StatusCode, string(body))
	}
	out := make(map[string][]byte)
	tr := tar.NewReader(resp.Body)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("snapshot tar read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// 服务器将请求的目录内容打包成 tar；在表现良好的实现中，
		// 条目名称已经是相对于 /workspace 的，但要防御性地剥离两种形式。
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")
		name = strings.TrimPrefix(name, "workspace/")
		if name == "" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return out, fmt.Errorf("snapshot read %s: %w", name, err)
		}
		out[name] = data
	}
	return out, nil
}

func (e *BoxliteExecutor) Close() error {
	e.mu.Lock()
	boxID := e.boxID
	e.boxID = ""
	e.mu.Unlock()
	if boxID == "" {
		return nil
	}
	delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(delCtx, "DELETE", e.prefixPath("/boxes/"+boxID)+"?force=true", nil)
	if err != nil {
		return err
	}
	auth, err := e.authHeader(delCtx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	slog.Info("boxlite box closed", "boxID", boxID)
	return nil
}

// BoxliteExecutorPool 管理每个（代理、项目、会话）的 Boxlite box。
type BoxliteExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*BoxliteExecutor
	baseURL   string
	prefix    string
	clientID  string
	apiKey    string
	image     string
	home      string
	timeout   time.Duration
	workspace workspace.Store
}

// 池上的 Backend 镜像 BoxliteExecutor.Backend，以便 LifecyclePool
// 无需解析延迟执行器即可显示提供者身份。
func (p *BoxliteExecutorPool) Backend() string { return "boxlite" }

// NewBoxliteExecutorPool 构造一个 Boxlite 后端池。
// 默认值与公共 Boxlite Cloud 匹配——操作员可以为自行托管的运行器
// 或暂存环境覆盖 URL/prefix/clientID。
func NewBoxliteExecutorPool(baseURL, prefix, clientID, apiKey, image, home string, timeout time.Duration) *BoxliteExecutorPool {
	if baseURL == "" {
		baseURL = defaultBoxliteURL
	}
	if prefix == "" {
		prefix = defaultBoxlitePrefix
	}
	if clientID == "" {
		clientID = defaultBoxliteClientID
	}
	if image == "" {
		image = defaultBoxliteImage
	}
	return &BoxliteExecutorPool{
		executors: make(map[string]*BoxliteExecutor),
		baseURL:   baseURL,
		prefix:    prefix,
		clientID:  clientID,
		apiKey:    apiKey,
		image:     image,
		home:      home,
		timeout:   timeout,
	}
}

// SetWorkspace 插入 workspace.Store，其内容应镜像到每个新 box 上的
// /workspace。镜像 E2BExecutorPool。
func (p *BoxliteExecutorPool) SetWorkspace(ws workspace.Store) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workspace = ws
}

func (p *BoxliteExecutorPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.executors[key]; ok {
		return ex, nil
	}
	ex, err := newBoxliteExecutor(ctx, p.baseURL, p.prefix, p.clientID, p.apiKey, p.image, p.timeout)
	if err != nil {
		return nil, err
	}
	ex.SetHydrationSources(skillDirsForAgent(p.home, agentID), p.workspace, agentID, projectID, sessionID)
	if err := ex.Hydrate(ctx); err != nil {
		_ = ex.Close()
		return nil, fmt.Errorf("boxlite hydrate: %w", err)
	}
	p.executors[key] = ex
	return ex, nil
}

func (p *BoxliteExecutorPool) Release(agentID, projectID, sessionID string) error {
	p.mu.Lock()
	key := poolKey(agentID, projectID, sessionID)
	ex, ok := p.executors[key]
	delete(p.executors, key)
	p.mu.Unlock()
	if ok {
		return ex.Close()
	}
	return nil
}

func (p *BoxliteExecutorPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ex := range p.executors {
		_ = ex.Close()
	}
	p.executors = make(map[string]*BoxliteExecutor)
}

var (
	_ Executor             = (*BoxliteExecutor)(nil)
	_ ExecutorPool         = (*BoxliteExecutorPool)(nil)
	_ WorkspaceSnapshotter = (*BoxliteExecutor)(nil)
	_ RemoteWorkspace      = (*BoxliteExecutor)(nil)
)
