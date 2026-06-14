package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DockerExecutor 包装 DockerSandbox 以实现 Executor。
// 容器将用户的工作区挂载到 /workspace，所有工具调用作为 docker exec 命令转发。
type DockerExecutor struct {
	sb *DockerSandbox
}

// NewDockerExecutor 创建一个由 Docker 容器支持的沙箱 Executor。
// workspace 是要挂载的主机端目录（例如从 S3 同步的用户工作区，
// 或用于临时使用的 tmpdir）。
func NewDockerExecutor(image, workspace string, policy *Policy) (*DockerExecutor, error) {
	sb := NewDockerSandbox(image, workspace, policy)
	if err := sb.Create(); err != nil {
		return nil, fmt.Errorf("create docker sandbox: %w", err)
	}
	return &DockerExecutor{sb: sb}, nil
}

func (d *DockerExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	execCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// docker 是客户端/守护进程模式——exec.CommandContext 只 SIGKILL
	// 本地的 `docker exec` CLI，这只会分离附加的客户端；
	// 容器内的内部进程继续运行直到自然完成。
	// 为了使超时在容器*内部*实际生效，我们将用户命令包装在 `setsid` 中，
	// 使其成为新的会话领导者，将其 pid（== 其 pgid）存储在标记文件中，
	// 并在取消时运行一个*单独的* `docker exec`，
	// 通过 `kill -KILL -$pgid` 信号整个进程组。
	//
	// 这取代了取消时强制移除整个容器的旧行为，
	// 旧行为保留了工作区绑定挂载但杀死了所有兄弟守护进程——
	// 最痛苦的是 camoufox-cli 的无头 Firefox，在下次调用时
	// 通过代理冷启动大约需要 3 分钟。
	// 进程组范围的 kill 保留容器——以及 camoufox 守护进程
	//（Python 使用 start_new_session=True 生成它，
	// 因此它存在于自己的会话中）——保持活动。
	//
	// 命令通过环境变量传递，以避免与内部 `sh -c` 的引号冲突；
	// eval 重新解析它，使得管道/重定向/展开的行为与调用者直接运行
	// `sh -c "$command"` 相同。
	marker := randomExecMarker()
	pgidFile := "/tmp/fc-pgid-" + marker
	wrapped := fmt.Sprintf(
		`__FC_PGID=%s __FC_CMD=%s setsid -w sh -c 'echo $$ > "$__FC_PGID"; eval "$__FC_CMD"'`,
		shellQuote(pgidFile), shellQuote(command),
	)

	done := make(chan struct{})
	go func() {
		select {
		case <-execCtx.Done():
			// 尽力而为：启动一个单独的 docker exec 来杀死进程组。
			// 5 秒预算，以便卡住的 `docker exec` 不会阻塞调用者——
			// 如果无法到达守护进程，我们早就失败了。
			killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer killCancel()
			_, _ = d.sb.Exec(killCtx, fmt.Sprintf(
				`pgid=$(cat %s 2>/dev/null); [ -n "$pgid" ] && kill -KILL -"$pgid"; rm -f %s`,
				shellQuote(pgidFile), shellQuote(pgidFile)),
				"")
		case <-done:
		}
	}()
	defer close(done)

	out, err := d.sb.Exec(execCtx, wrapped, "/workspace")
	// 正常退出时，goroutine 从未触发——我们自己清理标记文件。
	// 超时情况下，goroutine 已经删除了它。
	if execCtx.Err() == nil {
		_, _ = d.sb.Exec(context.Background(),
			fmt.Sprintf("rm -f %s", shellQuote(pgidFile)), "")
	}
	return out, err
}

// randomExecMarker 返回 16 个十六进制字符——足以防止并行 exec
// 相互覆盖 /tmp 中的 pgid 文件。
func randomExecMarker() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf[:])
}

func (d *DockerExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	return d.sb.Exec(ctx, fmt.Sprintf("cat %s", shellQuote(path)), "/workspace")
}

func (d *DockerExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	// 通过 stdin 而不是 argv 传输内容。Heredoc-in-argv（先前的实现）
	// 将字节切片到 docker-exec 命令行中，当内容包含 NULL 字节时，
	// 立即失败并报错 "fork/exec: invalid argument"——
	// 每个 PNG、音频文件或其他二进制 blob 都会触发此问题，
	// 因为 execve 拒绝 argv 元素中的 NUL。stdin 完全绕过了 argv 的限制。
	cmd := fmt.Sprintf("mkdir -p \"$(dirname %s)\" && cat > %s",
		shellQuote(path), shellQuote(path))
	out, err := d.sb.ExecWithStdin(ctx, cmd, "/workspace", strings.NewReader(content))
	if err != nil {
		return out, err
	}
	return fmt.Sprintf("Written to %s", path), nil
}

func (d *DockerExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return d.sb.Exec(ctx, fmt.Sprintf("ls -la %s", shellQuote(path)), "/workspace")
}

func (d *DockerExecutor) Close() error {
	return d.sb.Close()
}

// SnapshotWorkspace 遍历主机端挂载的工作区目录（以绑定方式挂载到容器中的
// /workspace），并返回每个常规文件的字节，以其容器相对路径为键。
// 由 LifecyclePool 用于驱逐时刷新，以便代理通过 `exec`（而不是 write_file）
// 创建的文件仍然进入持久化存储。
//
// 直接遍历主机目录比通过 exec 执行 tar 更快更可靠：
// 挂载已经为我们提供了沙箱看到的相同字节的 POSIX 视图。
func (d *DockerExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	root := d.sb.workspace
	if root == "" {
		return nil, nil
	}
	out := make(map[string][]byte)
	err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// Backend 返回 "docker"——用于每执行日志行，以便操作员可以确认
// 哪个提供者处理了给定的工具调用。
func (d *DockerExecutor) Backend() string { return "docker" }

// 确保 DockerExecutor 满足可选的快照契约。此处的编译错误将标记任何意外的接口漂移。
var _ WorkspaceSnapshotter = (*DockerExecutor)(nil)

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// DockerExecutorPool 管理每个（代理、会话）的 DockerExecutor 实例。
type DockerExecutorPool struct {
	mu        sync.Mutex
	executors map[string]*DockerExecutor // key = poolKey(agentID, sessionID)
	image     string
	policy    *Policy
	// workspaceRoot 是 BKCLAW_HOME——每个会话获得一个私有挂载，
	// 根目录为 workspaceRoot/workspaces/<agentID>/sessions/<sessionID>/。
	workspaceRoot string
}

// poolKey 是执行器池使用的复合映射键。每个（项目，会话）对
// 都有自己的槽位——包括属于同一项目的聊天——因为并行运行的两个项目聊天
// 会共享 Python 内核/shell 状态并相互干扰。
// 项目挂载本身在文件系统级别共享，因此兄弟会话保持可见
//（参见 pool.Get 了解挂载逻辑）。
//
// 两者都为空时回退到代理共享的沙箱槽位，用于旧调用者（管理员 shell、fixtures）。
func poolKey(agentID, projectID, sessionID string) string {
	switch {
	case projectID != "" && sessionID != "":
		return agentID + ":p:" + projectID + ":s:" + sessionID
	case projectID != "":
		return agentID + ":p:" + projectID
	case sessionID != "":
		return agentID + ":s:" + sessionID
	default:
		return agentID
	}
}

// 池上的 Backend 镜像 DockerExecutor.Backend，以便 LifecyclePool
// 无需解析延迟执行器即可显示提供者身份。
func (p *DockerExecutorPool) Backend() string { return "docker" }

// NewDockerExecutorPool 创建一个 Docker 后端执行器池。
func NewDockerExecutorPool(image, workspaceRoot string, policy *Policy) *DockerExecutorPool {
	if image == "" {
		image = "thinkany/bkclaw-sandbox:latest"
	}
	return &DockerExecutorPool{
		executors:     make(map[string]*DockerExecutor),
		image:         image,
		policy:        policy,
		workspaceRoot: workspaceRoot,
	}
}

func (p *DockerExecutorPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.executors[key]; ok {
		return ex, nil
	}

	// 绑定挂载布局。项目聊天挂载项目根目录（因此兄弟会话显示在 /workspace 下）
	// 并将 cwd 设置到自己的子目录中，因此相对写入默认为聊天的文件，
	// 但读取/遍历看到整个项目。镜像 workspace.LocalFS：
	//
	//   pid="p", sid="s" → 挂载 projects/p/，工作目录 /workspace/s/
	//   pid=""，  sid="s" → 挂载 sessions/s/，工作目录 /workspace
	//   pid="p", sid=""  → 挂载 projects/p/，工作目录 /workspace
	//   都为空       → 挂载代理根目录，工作目录 /workspace
	//
	// 每个聊天每个容器——即使在同一个项目中——以便并发聊天不共享 shell 状态。
	// 共享的部分是文件系统挂载，而不是容器。
	workspace := filepath.Join(p.workspaceRoot, "workspaces", agentID)
	var workdir string
	switch {
	case projectID != "" && sessionID != "":
		workspace = filepath.Join(workspace, "projects", projectID)
		workdir = "/workspace/" + sessionID
		// 在磁盘上预先创建每个聊天的子目录，以便 docker 的 `-w` 落在
		// 现有路径上；Docker 会创建缺失的工作目录，但只能以 root 身份创建，
		// 导致代理以后无法写入。
		if err := os.MkdirAll(filepath.Join(workspace, sessionID), 0o755); err != nil {
			return nil, fmt.Errorf("create chat workspace subdir: %w", err)
		}
	case projectID != "":
		workspace = filepath.Join(workspace, "projects", projectID)
	case sessionID != "":
		workspace = filepath.Join(workspace, "sessions", sessionID)
	}
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, fmt.Errorf("create workspace dir %s: %w", workspace, err)
	}

	// 手动构建沙箱，以便我们可以在 Create() 烘焙 docker run 参数之前
	// 连接技能挂载。通过 NewDockerExecutor 构建会在尚未告知技能目录的
	// 沙箱上立即调用 Create。
	sb := NewDockerSandbox(p.image, workspace, p.policy)
	if workdir != "" {
		sb.SetWorkdir(workdir)
	}
	sb.SetSkillDirs(skillDirsForAgent(p.workspaceRoot, agentID))
	// 将聊天者的按用户技能主机目录绑定挂载到沙箱中 `npx skills add -g -y`
	// 写入的路径，因此代理在聊天中安装的任何技能都会落到主机磁盘上，
	// 并对下一次 LoadSkills 扫描可见。UserID 通过 ctx 流入
	//（由 HandleMessage/HandleMessageStream 设置）；空值跳过挂载，
	// 这对于非聊天调用者是正确的回退。
	if uid := UserIDFromContext(ctx); uid != "" {
		base := os.Getenv("BKCLAW_HOME")
		if base == "" {
			if h, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(h, ".bkclaw")
			}
		}
		if base != "" {
			sb.SetUserSkillsHostDir(filepath.Join(base, "users", uid, "skills"))
		}
	}
	if err := sb.Create(); err != nil {
		return nil, fmt.Errorf("create docker sandbox: %w", err)
	}
	ex := &DockerExecutor{sb: sb}
	p.executors[key] = ex
	return ex, nil
}

func (p *DockerExecutorPool) Release(agentID, projectID, sessionID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.executors[key]; ok {
		delete(p.executors, key)
		return ex.Close()
	}
	return nil
}

func (p *DockerExecutorPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, ex := range p.executors {
		ex.Close()
		delete(p.executors, k)
	}
}

// 确保接口被满足。
var (
	_ Executor     = (*DockerExecutor)(nil)
	_ ExecutorPool = (*DockerExecutorPool)(nil)
)
