package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/workspace"
)

// E2B API：https://e2b.dev/docs
// 沙箱创建：POST https://api.e2b.dev/sandboxes
// 命令执行：通过沙箱上的 envd 使用 Connect 协议

const e2bBaseURL = "https://api.e2b.dev"
const e2bEnvdPort = "49983"

// E2BExecutor 使用 E2B 托管沙箱实现 Executor。
type E2BExecutor struct {
	apiKey      string
	sandboxID   string
	accessToken string
	client      *http.Client
	template    string        // 为 recreate() 记住，以便新沙箱使用相同的模板
	timeout     time.Duration // 为 recreate() 记住
	// 填充源——由池在创建后设置，以便 recreate()
	// 无需回退到池即可重建 /skills + /workspace。
	// 工作区存储是可选的；技能目录可能为空。
	skillDirs []string
	workspace workspace.Store
	agentID   string
	projectID string
	sessionID string
}

func newE2BExecutor(ctx context.Context, apiKey, template string, timeout time.Duration) (*E2BExecutor, error) {
	if template == "" {
		template = "base"
	}
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	// 没有全局 Client.Timeout：它涵盖包括流式传输正文的整个往返，
	// 这会在 60 秒时静默截断长时间执行的调用（图像生成等），
	// 并使工具返回空输出且无错误。我们依赖调用站点的按请求
	// context.WithTimeout 代替——execOnce 从用户提供的工具超时派生 ctx，
	// 下面的 create-sandbox 使用显式的短 ctx。
	client := &http.Client{}

	// 字段名为 `templateID`（驼峰命名法）——由服务器的验证错误确认：
	// 当字段被重命名为蛇形命名法时，出现
	// `Error at "/templateID": property "templateID" is missing`。
	// 蛇形命名法形式出现在某些 SDK 源代码中，
	// 但生产 REST API 拒绝它。
	body, _ := json.Marshal(map[string]interface{}{
		"templateID": template,
		"timeout":    int(timeout.Seconds()),
	})
	// 将 create-sandbox 调用限制为 60 秒——调用本身通常在 1-2 秒内完成；
	// 如果挂起超过此时间，则是控制平面问题，我们宁愿显式超时，
	// 也不愿在未从 ctx 继承截止时间的请求上无限期等待。
	createCtx, cancelCreate := context.WithTimeout(ctx, 60*time.Second)
	defer cancelCreate()
	req, err := http.NewRequestWithContext(createCtx, "POST", e2bBaseURL+"/sandboxes", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("e2b create sandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("e2b create sandbox: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		SandboxID       string `json:"sandboxID"`
		EnvdAccessToken string `json:"envdAccessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("e2b parse response: %w", err)
	}

	slog.Info("e2b sandbox created", "sandboxID", result.SandboxID, "template", template)

	return &E2BExecutor{
		apiKey:      apiKey,
		sandboxID:   result.SandboxID,
		accessToken: result.EnvdAccessToken,
		client:      client,
		template:    template,
		timeout:     timeout,
	}, nil
}

func (e *E2BExecutor) envdURL() string {
	return fmt.Sprintf("https://%s-%s.e2b.app", e2bEnvdPort, e.sandboxID)
}

// recreate 销毁当前沙箱并创建一个新沙箱。执行器最初构建时使用的
// 相同模板/超时被重用——在此处硬编码"base"会在自定义模板沙箱
// 空闲超时后静默降级。完整的填充（技能 + 工作区）被重放，
// 以便 /skills/<name>/ 和 /workspace/ 在重新创建之间保持填充状态。
func (e *E2BExecutor) recreate(ctx context.Context) error {
	slog.Info("e2b sandbox expired, recreating", "oldSandboxID", e.sandboxID)
	newEx, err := newE2BExecutor(ctx, e.apiKey, e.template, e.timeout)
	if err != nil {
		return err
	}
	e.sandboxID = newEx.sandboxID
	e.accessToken = newEx.accessToken
	// Hydrate 是强制性的，原因与在 Get() 中相同——它是使 /workspace
	// 对执行用户可写的唯一步骤。如果我们只是在这里警告日志记录失败，
	// 重新创建的沙箱会静默损坏：代理调用成功，但每个 /workspace 写入
	// 都因 Permission denied 失败，字节被困在 /tmp 中，
	// 在下次驱逐时消失。
	if err := e.Hydrate(ctx); err != nil {
		return fmt.Errorf("hydrate after recreate (sandboxID=%s): %w", e.sandboxID, err)
	}
	if err := verifyWorkspaceWritable(ctx, e); err != nil {
		return fmt.Errorf("recreated sandbox unusable (sandboxID=%s): %w", e.sandboxID, err)
	}
	return nil
}

// SetHydrationSources 记录 Hydrate() 在下一次调用时应从中拉取的输入。
// 由池在沙箱创建后立即调用；执行器然后携带它们，
// 以便 recreate() 可以重放所有内容而无需询问池。
// 对于你没有的任何源传递 nil/空值。
func (e *E2BExecutor) SetHydrationSources(skillDirs []string, ws workspace.Store, agentID, projectID, sessionID string) {
	e.skillDirs = append(e.skillDirs[:0], skillDirs...)
	e.workspace = ws
	e.agentID = agentID
	e.projectID = projectID
	e.sessionID = sessionID
}

// Hydrate 使用代理工具期望在磁盘上找到的所有内容填充沙箱：
//   - /skills/<name>/...   来自每个配置的技能目录（按代理 + 全局，
//     先到先得优先级以匹配 docker 绑定挂载层）
//   - /workspace/...       来自代理的 workspace.Store（以便通过 write_file
//     在过去会话中写入的文件在沙箱重启后存活，与现有按文件
//     hydrateWorkspace 相同的契约）
//
// 实现：将所有内容打包成一个 tar.gz，通过 envd 的 /files 多部分端点上传
// （与 writeFileOnce 使用的路径相同——已知有效），
// 然后运行一个小型 `bash -c` 来解压+chown。
// 早期版本将 base64 编码的 tar 内联在 `bash -c` 参数中；
// 这对于微小包有效，但经验上一旦编码后的有效负载超过约 80KB 的 base64
// （8 个捆绑技能已经是 103KB 并触发了问题），Connect 协议响应就被截断。
// 截断表现为 envd 的 End-frame，其中 `exited=false` 且无退出状态——
// 旧代码将其视为成功，因为 exitCode 仍然是 0，
// 因此 Hydrate 看起来运行了，而 chown 步骤实际上在中途被截断。
// 修复方法将批量传输完全移出 exec 通道，
// 因此脚本保持小型且大小恒定，与包大小无关。
func (e *E2BExecutor) Hydrate(ctx context.Context) error {
	bundle := newTarBundle()

	skillCount := 0
	skillFileCount := 0
	seen := make(map[string]bool) // 每个技能：第一个目录优先
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
				slog.Warn("e2b hydrate: skill tar", "skill", name, "error", err)
				continue
			}
			skillCount++
			skillFileCount += n
		}
	}

	workspaceCount := 0
	if e.workspace != nil {
		// 对于项目聊天，填充整个项目（使用 session="" 的 List），
		// 以便聊天在 /workspace/<other-sid>/... 看到兄弟聊天的文件——
		// 与 docker 通过挂载 projects/<pid>/ 作为绑定根获得的可见性相同。
		// 松散的聊天保持限定于自己的会话子树。
		listProject := e.projectID
		listSession := e.sessionID
		if e.projectID != "" {
			listSession = ""
		}
		objs, err := e.workspace.List(ctx, e.agentID, listProject, listSession)
		if err != nil {
			slog.Warn("e2b hydrate: workspace list", "agent", e.agentID, "project", e.projectID, "session", e.sessionID, "error", err)
		} else {
			for _, obj := range objs {
				rc, err := e.workspace.Get(ctx, e.agentID, listProject, listSession, obj.Path)
				if err != nil {
					slog.Warn("e2b hydrate: workspace get", "path", obj.Path, "error", err)
					continue
				}
				data, rerr := io.ReadAll(rc)
				rc.Close()
				if rerr != nil {
					slog.Warn("e2b hydrate: workspace read", "path", obj.Path, "error", rerr)
					continue
				}
				rel := strings.TrimPrefix(obj.Path, "/")
				if err := bundle.addBytes("workspace/"+rel, data, 0o644, obj.ModTime); err != nil {
					slog.Warn("e2b hydrate: workspace tar", "path", obj.Path, "error", err)
					continue
				}
				workspaceCount++
			}
		}
	}

	if err := bundle.close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}

	// 为什么这里的每个字都很重要：
	// - 我们始终运行 mkdir+chown 步骤，即使没有文件要推送。
	//   /workspace 和 /skills 必须存在且对 `user` 可写——
	//   image-tool/write_file/任何写入那里的内容如果目录缺失
	//   都会因 ENOENT 或 EACCES 失败。这是一个空工作区的新会话的
	//   失败模式：没有文件→先前的代码提前返回→/workspace 从未创建→
	//   当 LLM 尝试以非 root 的 `user` 身份创建它时报 "mkdir: Permission denied"。
	// - `sudo`：E2B 的 "base" 模板以 `user` 身份运行，对 / 没有写权限。
	//   默认用户根据 e2b 发布的 Dockerfile 具有无密码 sudo；
	//   移除 sudo 的自定义模板需要保留它或预先创建 /skills + /workspace
	//   并 chown 给 user。
	// - 包（当有文件存在时）现在在此 exec 运行之前通过 /files 端点上传——
	//   参见上面的 uploadBytes 和 Hydrate 本身的大小相关注释。
	//   下面的脚本仅将 /tmp/fc-hydrate.tar.gz 作为路径引用，
	//   因此 bash -c 参数保持小型，无论包有多大。
	// - 解压后 chown：tar-as-root 以 root 所有者落地文件，
	//   因此在解压后重新 chown；代理后续写入以 user 身份运行。
	if bundle.fileCount > 0 {
		// 以 `user` 所有者身份上传到 /tmp；tar 仍在 sudo 下运行，
		// 因此它可以将 /skills/* 和 /workspace/* 放在文件系统根目录。
		if err := e.uploadBytes(ctx, "/tmp/fc-hydrate.tar.gz", bundle.gz.Bytes()); err != nil {
			return fmt.Errorf("hydrate upload tar: %w", err)
		}
	}
	cmdParts := []string{
		"set -e",
		"sudo mkdir -p /skills /workspace",
		"sudo chown user:user /skills /workspace",
	}
	if bundle.fileCount > 0 {
		cmdParts = append(cmdParts,
			"sudo tar -xzf /tmp/fc-hydrate.tar.gz -C /",
			"sudo chown -R user:user /skills /workspace",
			"rm -f /tmp/fc-hydrate.tar.gz",
		)
	}
	cmd := strings.Join(cmdParts, "; ")
	out, err := e.execOnce(ctx, cmd, 60*time.Second)
	if err != nil {
		slog.Warn("e2b hydrate extract failed", "sandboxID", e.sandboxID, "error", err, "out", out)
		return fmt.Errorf("hydrate sandbox dirs: %w (output: %s)", err, out)
	}
	slog.Info("e2b sandbox hydrated",
		"sandboxID", e.sandboxID,
		"skills", skillCount,
		"skillFiles", skillFileCount,
		"workspaceFiles", workspaceCount,
		"tarBytes", bundle.gz.Len())
	return nil
}

// tarBundle 是 archive/tar + gzip 的小型辅助封装，
// 这样 Hydrate 路径就不必重复 writer-close 的舞蹈。
// 包中的所有路径都是沙箱相对的（无前导斜杠）；调用者选择解压根目录。
type tarBundle struct {
	gz        bytes.Buffer
	gw        *gzip.Writer
	tw        *tar.Writer
	fileCount int
}

func newTarBundle() *tarBundle {
	b := &tarBundle{}
	b.gw = gzip.NewWriter(&b.gz)
	b.tw = tar.NewWriter(b.gw)
	return b
}

// addBytes 将内存中的文件添加到包中。tar -xz 从条目路径自动创建父目录，
// 因此我们不需要显式的目录条目——通过主机上的往返测试验证。
func (b *tarBundle) addBytes(name string, data []byte, mode int64, modTime time.Time) error {
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

// addLocalDir 遍历主机目录并将其下的每个常规文件添加到包中，
// 根目录为 sandboxPrefix。符号链接/套接字等被跳过——
// 技能包应该是普通文件。
func (b *tarBundle) addLocalDir(localRoot, sandboxPrefix string) (int, error) {
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
		name := sandboxPrefix + "/" + filepath.ToSlash(rel)
		if err := b.addBytes(name, data, int64(info.Mode().Perm()), info.ModTime()); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

func (b *tarBundle) close() error {
	if err := b.tw.Close(); err != nil {
		return err
	}
	return b.gw.Close()
}

// isSandboxGone 检查错误是否指示沙箱已被销毁。
func isSandboxGone(statusCode int) bool {
	return statusCode == 502 || statusCode == 404
}

// connectEnvelope 将 JSON 有效负载包装在 Connect 协议信封帧中。
// 格式：[1 字节标志][4 字节大端长度][有效负载]
func connectEnvelope(payload []byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = 0 // flags: no compression, not end of stream
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

// parseConnectStream 读取 Connect 协议流式响应。
// 每个帧：[1 字节标志][4 字节长度][有效负载]
func parseConnectStream(data []byte) []json.RawMessage {
	var messages []json.RawMessage
	for len(data) >= 5 {
		flags := data[0]
		length := binary.BigEndian.Uint32(data[1:5])
		data = data[5:]
		if uint32(len(data)) < length {
			break
		}
		payload := data[:length]
		data = data[length:]

		// flags & 0x02 = end_stream（尾部），跳过它
		if flags&0x02 != 0 {
			continue
		}
		messages = append(messages, json.RawMessage(payload))
	}
	return messages
}

func (e *E2BExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	// 匹配 Docker 的行为：默认 cwd 是 /workspace，而不是调用用户的 $HOME。
	// 没有这个，来自代理命令的相对路径写入
	//（例如 `camoufox-cli screenshot out.png`）会落在 /home/user/，
	// 永远无法到达主机可见的工作区。
	// Hydrate 在任何代理 Exec 运行之前创建 /workspace，
	// 而 recreate() 在沙箱替换后重新填充，因此 `cd` 保证在此处成功。
	wrapped := "cd /workspace && " + command
	result, err := e.execOnce(ctx, wrapped, timeout)
	if err != nil && strings.Contains(err.Error(), "HTTP 502") || strings.Contains(fmt.Sprint(err), "HTTP 404") {
		if rerr := e.recreate(ctx); rerr != nil {
			return "", fmt.Errorf("sandbox recreate failed: %w (original: %v)", rerr, err)
		}
		return e.execOnce(ctx, wrapped, timeout)
	}
	return result, err
}

func (e *E2BExecutor) execOnce(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"process": map[string]interface{}{
			"cmd":  "/bin/bash",
			"args": []string{"-c", command},
		},
	})

	enveloped := connectEnvelope(payload)

	// 按请求截止时间：timeout（用户提供的工具预算）加上 30 秒余量，
	// 以便服务器在我们放弃之前有时间刷新尾部帧。
	// 关键是因为底层 http.Client 现在没有全局超时——
	// 没有这个，如果 envd 在流式传输中死亡，请求将永远挂起。
	execCtx, cancelExec := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancelExec()

	reqURL := e.envdURL() + "/process.Process/Start"
	req, err := http.NewRequestWithContext(execCtx, "POST", reqURL, bytes.NewReader(enveloped))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Timeout-Ms", fmt.Sprintf("%d", int(timeout.Milliseconds())))
	if e.accessToken != "" {
		req.Header.Set("X-Access-Token", e.accessToken)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b exec: %w", err)
	}
	defer resp.Body.Close()

	// 不要丢弃 ReadAll 的错误——当连接在流式传输中途断开时
	//（例如我们的截止时间在进程完成流式输出之前到达），
	// 这是我们拥有的唯一表明我们获取的字节不完整的信号。
	// 以前我们静默保留部分正文，解析器看到零个完整帧，
	// 工具返回空的 stdout——这正是我们在长时间运行的 exec 调用中遇到的症状。
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return string(body), fmt.Errorf("e2b exec body read: %w (got %d bytes)", readErr, len(body))
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("e2b exec HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Parse Connect streaming response frames
	frames := parseConnectStream(body)

	var stdout, stderr strings.Builder
	exitCode := 0
	exited := false

	for _, frame := range frames {
		// E2B 响应格式：{"event":{"data":{"stdout":"base64..."}}}
		var msg struct {
			Event struct {
				Start *struct {
					Pid int `json:"pid"`
				} `json:"start,omitempty"`
				Data *struct {
					Stdout string `json:"stdout,omitempty"` // base64 encoded
					Stderr string `json:"stderr,omitempty"` // base64 encoded
				} `json:"data,omitempty"`
				End *struct {
					Exited bool   `json:"exited"`
					Status string `json:"status"` // "exit status 0"
				} `json:"end,omitempty"`
			} `json:"event"`
		}
		if json.Unmarshal(frame, &msg) != nil {
			continue
		}
		if msg.Event.Data != nil {
			if msg.Event.Data.Stdout != "" {
				if decoded, err := base64.StdEncoding.DecodeString(msg.Event.Data.Stdout); err == nil {
					stdout.Write(decoded)
				}
			}
			if msg.Event.Data.Stderr != "" {
				if decoded, err := base64.StdEncoding.DecodeString(msg.Event.Data.Stderr); err == nil {
					stderr.Write(decoded)
				}
			}
		}
		if msg.Event.End != nil {
			exited = msg.Event.End.Exited
			// 解析 "exit status N" 以获取退出码
			if strings.HasPrefix(msg.Event.End.Status, "exit status ") {
				fmt.Sscanf(msg.Event.End.Status, "exit status %d", &exitCode)
			}
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}
	output = strings.TrimSpace(output)

	slog.Info("e2b exec completed", "sandboxID", e.sandboxID, "exitCode", exitCode, "exited", exited, "outputLen", len(output), "frames", len(frames), "bodyBytes", len(body))

	// 拒绝没有传递正确"End/exited=true"尾部的流。
	// 为什么这很重要：当请求有效负载将 envd 推过某个内部缓冲区时
	//（经验表明单个 bash -c 参数中超过 ~80KB 的 base64 会触发它），
	// 响应返回带有帧但没有最终退出状态的。
	// exitCode 保持为零默认值，否则我们会返回 nil 错误——
	// 这正是 Hydrate 看起来成功而 chown 步骤实际上未运行的静默失败模式，
	// 然后 verifyWorkspaceWritable 报告 "/workspace probe: Permission denied"，
	// 无明显原因说明为什么 chown 没有生效。
	if !exited {
		if output == "" {
			output = "(no output — response stream truncated before exit-status trailer)"
		}
		return output, fmt.Errorf("e2b exec did not exit cleanly (frames=%d, bodyBytes=%d): %s", len(frames), len(body), output)
	}

	if exitCode != 0 {
		if output == "" {
			output = fmt.Sprintf("Process exited with code %d", exitCode)
		}
		return output, fmt.Errorf("exit code %d", exitCode)
	}
	return output, nil
}

func (e *E2BExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	result, err := e.readFileOnce(ctx, path)
	if err != nil && (strings.Contains(err.Error(), "HTTP 502") || strings.Contains(err.Error(), "HTTP 404")) {
		if rerr := e.recreate(ctx); rerr != nil {
			return "", rerr
		}
		return e.readFileOnce(ctx, path)
	}
	return result, err
}

func (e *E2BExecutor) readFileOnce(ctx context.Context, path string) (string, error) {
	reqURL := fmt.Sprintf("%s/files?path=%s&username=user", e.envdURL(), url.QueryEscape(path))
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return "", err
	}
	if e.accessToken != "" {
		req.Header.Set("X-Access-Token", e.accessToken)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("e2b read: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("e2b read HTTP %d: %s", resp.StatusCode, string(body))
	}
	return string(body), nil
}

func (e *E2BExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	result, err := e.writeFileOnce(ctx, path, content)
	if err != nil && (strings.Contains(err.Error(), "HTTP 502") || strings.Contains(err.Error(), "HTTP 404")) {
		if rerr := e.recreate(ctx); rerr != nil {
			return "", rerr
		}
		return e.writeFileOnce(ctx, path, content)
	}
	return result, err
}

func (e *E2BExecutor) writeFileOnce(ctx context.Context, filePath, content string) (string, error) {
	if err := e.uploadBytes(ctx, filePath, []byte(content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(content), filePath), nil
}

// uploadBytes 将 `data` POST 到 envd 的 /files 多部分端点，
// 路径为 `sandboxPath`，由 exec 运行的 `user` 账户拥有。
// 从 writeFileOnce 中提取出来，以便 Hydrate 可以通过相同的
// 大型有效负载安全路径发送其 tar 包，而不是将 base64  blob
// 内联在 `bash -c` 参数中（后者经验上在 ~80KB 处截断
// Connect 响应流并留下半填充的沙箱；参见 Hydrate 调用者上的注释）。
func (e *E2BExecutor) uploadBytes(ctx context.Context, sandboxPath string, data []byte) error {
	// E2B envd 的 POST /files 期望 multipart/form-data 带有 `file` 字段，
	// 而不是原始的 octet-stream 正文。早期的原始正文版本返回 200 OK，
	// 但静默丢弃了上传，导致沙箱内的文件不存在——
	// 在上传的技能在执行时因 "No such file or directory" 失败时被发现。
	reqURL := fmt.Sprintf("%s/files?path=%s&username=user",
		e.envdURL(), url.QueryEscape(sandboxPath))

	// envd：目标路径来自 `path` 查询参数；多部分的 `filename` 只是元数据，
	// 因此使用 basename 即可。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", path.Base(sandboxPath))
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if e.accessToken != "" {
		req.Header.Set("X-Access-Token", e.accessToken)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2b upload: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("e2b upload HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (e *E2BExecutor) ListDir(ctx context.Context, path string) (string, error) {
	// 使用 exec 来列出目录，因为 files API 没有列表端点
	return e.Exec(ctx, fmt.Sprintf("ls -la %s", path), 10*time.Second)
}

// IsRemoteWorkspace 将此执行器标记为云托管，
// 以便 LifecyclePool 在每次 exec 后运行 syncSnapshot，
// 而不是仅在空闲驱逐时。参见 sandbox.RemoteWorkspace。
func (e *E2BExecutor) IsRemoteWorkspace() {}

// SnapshotWorkspace 将 /workspace 打包为 tar 并通过 stdout 以 base64 形式
// 将字节发送回来。这是 Hydrate 的 tar+base64 推送的逆操作——
// 由 LifecyclePool 使用，在每次成功 exec 后将沙箱端文件刷新回持久化
// workspace.Store，以便技能在沙箱内写入的文件
// （image-tool 的 /workspace/gen_xxx.webp 等）最终可以从主机的 UI/
// 签名 URL 路径访问。
//
// 返回 /workspace 相对路径到内容的映射。当 /workspace 为空或不存在时静默跳过。
func (e *E2BExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	// `2>/dev/null` 吞掉当 /workspace 尚不存在时的
	// "tar: ./: directory not found" 干扰；我们仍然希望以空结果继续。
	// base64 -w0 保持输出在单行上，以便 envd 的帧解析器不会与
	// 空白折叠斗争。如果 /workspace 完全缺失，则回退到空 tar。
	cmd := "if [ -d /workspace ]; then " +
		"tar -czf - -C /workspace . 2>/dev/null | base64 -w0; " +
		"fi"
	out, err := e.execOnce(ctx, cmd, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace exec: %w (output: %s)", err, out)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	gz, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace decode: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("snapshot workspace gunzip: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	out2 := make(map[string][]byte)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out2, fmt.Errorf("snapshot workspace tar read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Tar 名称以 "./" 开头，因为我们从 /workspace 内部打包了 `.`；
		// 去除它，以便调用者看到 workspace.Store 使用的相同代理相对路径布局。
		name := strings.TrimPrefix(hdr.Name, "./")
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			continue
		}
		// 跳过 macOS 资源分支（`._foo`），以防有人曾对 BSD-tar 模板
		// 运行此操作——Linux/E2B 的 GNU tar 不会发出这些，
		// 但它们会污染存储。
		base := name
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if strings.HasPrefix(base, "._") {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return out2, fmt.Errorf("snapshot workspace read entry %s: %w", name, err)
		}
		out2[name] = data
	}
	return out2, nil
}

// verifyWorkspaceWritable 对 /workspace 执行一次性探测，
// 使用与代理 exec 运行相同的 `user` 账户。
// 它捕获 Hydrate 看似成功但底层 chown 实际上未生效的静默失败模式——
// 经验上观察到当 e2b 模板提供的 /workspace 由 root 拥有且 chown 步骤
// 被跳过或无 sudo 时发生。探测写入一个字节然后删除它；
// touch+rm 往返是代理在其第一次 /workspace 写入时使用的相同操作模式，
// 因此任何在那里失败的操作也会在此处失败。
func verifyWorkspaceWritable(ctx context.Context, ex *E2BExecutor) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	const probeCmd = `touch /workspace/.fc-health && rm -f /workspace/.fc-health && echo ok`
	out, err := ex.execOnce(probeCtx, probeCmd, 10*time.Second)
	if err != nil {
		return fmt.Errorf("/workspace probe: %w (out=%s)", err, strings.TrimSpace(out))
	}
	if !strings.Contains(out, "ok") {
		return fmt.Errorf("/workspace not writable as exec user (out=%s)", strings.TrimSpace(out))
	}
	return nil
}

func (e *E2BExecutor) Close() error {
	req, _ := http.NewRequest("DELETE",
		fmt.Sprintf("%s/sandboxes/%s", e2bBaseURL, e.sandboxID), nil)
	req.Header.Set("X-API-Key", e.apiKey)
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	slog.Info("e2b sandbox closed", "sandboxID", e.sandboxID)
	return nil
}

// Backend 返回 "e2b"——用于每执行日志行，以便操作员可以
// 一目了然地确认哪个提供者处理了给定的工具调用。
func (e *E2BExecutor) Backend() string { return "e2b" }

// 池上的 Backend 镜像 E2BExecutor.Backend，以便 LifecyclePool
// 无需解析延迟执行器即可显示提供者身份。
func (p *E2BExecutorPool) Backend() string { return "e2b" }

// E2BExecutorPool 管理每个用户的 E2B 沙箱。
type E2BExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*E2BExecutor
	apiKey    string
	template  string
	timeout   time.Duration
	home      string          // 用于解析每个代理技能目录的工作区根目录
	workspace workspace.Store // 可选——设置时，/workspace 与 /skills 一起被填充
}

// NewE2BExecutorPool——`home` 是 docker 后端用于 `-v` 挂载的 BKCRAB_HOME；
// 池使用它来解析要将哪些技能目录推送到每个新沙箱中。
func NewE2BExecutorPool(apiKey, template, home string, timeout time.Duration) *E2BExecutorPool {
	return &E2BExecutorPool{
		executors: make(map[string]*E2BExecutor),
		apiKey:    apiKey,
		template:  template,
		timeout:   timeout,
		home:      home,
	}
}

// SetWorkspace 插入 workspace.Store，其内容应镜像到每个新沙箱内的
// /workspace。可选——当为 nil 时，仅填充 /skills。
// 由网关在 LifecyclePool 自己的工作区连接后调用，
// 以便内部池和生命周期层看到相同的事实来源。
func (p *E2BExecutorPool) SetWorkspace(ws workspace.Store) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.workspace = ws
}

func (p *E2BExecutorPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.executors[key]; ok {
		return ex, nil
	}
	ex, err := newE2BExecutor(ctx, p.apiKey, p.template, p.timeout)
	if err != nil {
		return nil, err
	}
	ex.SetHydrationSources(skillDirsForAgent(p.home, agentID), p.workspace, agentID, projectID, sessionID)
	if err := ex.Hydrate(ctx); err != nil {
		// Hydrate 是将 /workspace 的拥有者更改为 exec 运行所基于的
		// 非 root `user` 账户的步骤；没有它，代理对 /workspace 的每次写入
		// 都会静默地获得 Permission denied——参见 e2b_probe_diag_test.go
		// 中的复现器。以前这里是 WARN；沙箱仍然会被缓存，
		// 每个后续轮次都会表现为"代理写了文件但它们消失了"。
		// 拆除它，以便调用者重试或显式失败。
		_ = ex.Close()
		return nil, fmt.Errorf("e2b hydrate: %w", err)
	}
	if err := verifyWorkspaceWritable(ctx, ex); err != nil {
		_ = ex.Close()
		return nil, fmt.Errorf("e2b sandbox unusable: %w", err)
	}
	warmupCamoufoxDaemon(ctx, ex)
	p.executors[key] = ex
	return ex, nil
}

// warmupCamoufoxDaemon 作为沙箱配置的一部分生成 camoufox-cli 后台守护进程，
// 以便代理的第一次 `camoufox-cli open` 调用附加到正在运行的守护进程，
// 而不是竞态生成一个。没有这个，CLI 的自动生成路径只等待 5 秒的守护进程
// 套接字——经验上在全新的 e2b 沙箱中太短（Python 启动 + Firefox 握手
// 通常需要更长时间），并且同一沙箱内的并发 exec 可能都尝试生成，
// 表现为用户可见的 "Daemon did not start within 5 seconds" 失败。
//
// 尽力而为：任何错误以 WARN 级别记录，沙箱创建仍然成功——
// 从不接触浏览器的代理不应因为 camoufox 无法启动而失败，
// 原始的（慢速）冷启动路径作为回退保持不变。
// 限制为 120 秒，以便卡住的 camoufox 安装不会无限期阻挡沙箱创建。
func warmupCamoufoxDaemon(ctx context.Context, ex *E2BExecutor) {
	warmCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// `open about:blank` 是启动守护进程的最廉价调用：
	// 无网络、无 GeoIP 查找、无真实页面加载。
	// Dockerfile 中的代理填充仍然适用——如果设置了 HTTPS_PROXY，
	// 浏览器以 --proxy 启动，匹配代理稍后的调用将看到的内容，
	// 因此预热的守护进程配置相同。
	out, err := ex.execOnce(warmCtx, "cd /workspace && camoufox-cli open about:blank", 120*time.Second)
	if err != nil {
		slog.Warn("e2b camoufox warmup failed (first browser call will pay cold-start)",
			"sandboxID", ex.sandboxID, "error", err, "out", strings.TrimSpace(out))
		return
	}
	slog.Info("e2b camoufox daemon warmed", "sandboxID", ex.sandboxID)
}

func (p *E2BExecutorPool) Release(agentID, projectID, sessionID string) error {
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

func (p *E2BExecutorPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ex := range p.executors {
		ex.Close()
	}
	p.executors = make(map[string]*E2BExecutor)
}

var (
	_ Executor             = (*E2BExecutor)(nil)
	_ ExecutorPool         = (*E2BExecutorPool)(nil)
	_ WorkspaceSnapshotter = (*E2BExecutor)(nil)
	_ RemoteWorkspace      = (*E2BExecutor)(nil)
)
