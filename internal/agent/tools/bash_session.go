package tools

// 后台 shell 管理 — 克劳德代码风格。
//
// 一个工具调用（带有 run_in_background=true 的 exec ）会启动一个长-
// 运行命令并立即返回“bash_id”。代理
// 通过 `bash_output(bash_id)` 观察进度（返回新的 stdout/stderr
// 自上次调用以来）并以“kill_shell(bash_id)”终止。
//
// 范围（故意缩小）：
// - 仅限主机模式 os/exec；沙盒模式背景是 v2 的后续版本
// - 仅尾部观察；没有发送键/粘贴/交互控制
// （这些用例通过从常规 `exec` 调用的 tmux 进行路由）
// - 会话是代理私有的并且一直存在直到被杀死或Registry.Close

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// bufferCap 限制每个会话保留的输出。当超过时，
// 最旧的字节将被丢弃 FIFO。 4 MiB 可轻松容纳 30 分钟
// 开发服务器日志，同时将总内存限制为 4 MiB × 实时会话。
const bufferCap = 4 * 1024 * 1024

// outputBuffer 是一个具有硬上限的线程安全 FIFO 字节缓冲区。
// 它跟踪曾经写入的字节总数（“绝对偏移量”）
// 因此，读取“自上次检查以来”的调用者可以在截断中幸存下来：
// 较旧的字节被丢弃，现有的读取游标前进到
// 当前头和调用者得知一些输出丢失了。
type outputBuffer struct {
	mu       sync.Mutex
	data     []byte
	head     int // absolute offset of data[0]; equals total bytes dropped
	total    int // absolute offset just past data[end]; equals total bytes ever written
	maxBytes int
}

func newOutputBuffer(maxBytes int) *outputBuffer {
	return &outputBuffer{maxBytes: maxBytes}
}

// Write 将 p 追加到缓冲区，如果 cap 为，则删除最旧的字节
// 超过了。总是成功；返回 len(p), nil 以满足 io.Writer
// （因此它可以直接插入 exec.Cmd.Stdout / Stderr）。
func (b *outputBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	b.total += len(p)
	if len(b.data) > b.maxBytes {
		drop := len(b.data) - b.maxBytes
		b.data = b.data[drop:]
		b.head += drop
	}
	return len(p), nil
}

// readSince 返回从绝对偏移量 `since` 开始的内容。如果
// `since` 位于 head 下方（旧内容已被删除），返回
// 当前使用“dropped=true”保存的所有内容，以便调用者可以发出警告
// 模型。返回新的绝对偏移量供调用者记住。
func (b *outputBuffer) readSince(since int) (out []byte, dropped bool, newSince int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if since < b.head {
		dropped = true
		since = b.head
	}
	// 由于现在 ≥ b.head，因此通过构造开始 ≥ 0。唯一的
	// 剩余的超出范围的情况是“调用者的光标位于
	// 我们曾经生产过”（自 > b.total 起），这意味着有
	// 还没有什么新东西。
	start := since - b.head
	if start > len(b.data) {
		return nil, dropped, b.total
	}
	// 复制出来，以便调用者可以释放锁而不会出现别名。
	out = append([]byte(nil), b.data[start:]...)
	return out, dropped, b.total
}

// bashSession 是单个后台 shell 命令和状态
// 需要观察并终止它。
type bashSession struct {
	id        string
	command   string
	startedAt time.Time

	cmd    *exec.Cmd
	cancel context.CancelFunc
	out    *outputBuffer

	// readCursor 是已经返回到 bash_output 的绝对偏移量。
	// 由 readMu 保护（与 outputBuffer.mu 分开，因此编写者和
	// 读者不会争夺一把锁）。
	readMu     sync.Mutex
	readCursor int

	// 当 cmd.Wait 返回时，done 恰好翻转为 true 一次。退出代码是
	// 写在完成之前；读取 exitCode 是安全的，当且仅当 did 为 true
	// （通过atomic.Bool获取-释放）。
	done     atomic.Bool
	exitCode int
	exitErr  error
}

// status 报告会话的运行时状态。
type bashStatus int

const (
	statusRunning bashStatus = iota
	statusExited
)

// 快照返回会话最终状态的一致视图。
// 运行时可观察状态； exitCode 仅在以下情况下才有意义
// 状态 == 状态已退出。
func (s *bashSession) snapshot() (status bashStatus, exitCode int, exitErr error) {
	if !s.done.Load() {
		return statusRunning, 0, nil
	}
	return statusExited, s.exitCode, s.exitErr
}

// readNew 提取自上次 readNew 调用以来产生的所有输出
// 会议。 drop=true 表示缓冲区滚动超过读取游标
// 有些字节永久消失了。
func (s *bashSession) readNew() (out []byte, dropped bool) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	out, dropped, next := s.out.readSince(s.readCursor)
	s.readCursor = next
	return out, dropped
}

// Kill 通过与会话关联的取消函数发出 SIGKILL 信号
// 自己的背景。如果会话已经完成，则返回 nil。幂等。
func (s *bashSession) kill() error {
	if s.done.Load() {
		return nil
	}
	s.cancel()
	return nil
}

// shellManager 拥有注册表的每个后台会话。会议
// 比启动它们的请求上下文更长久——它们在被杀死时死亡，在
// 自然退出，或在Registry.Close 上退出。
type shellManager struct {
	mu      sync.Mutex
	shells  map[string]*bashSession
	counter int
	closed  bool // Close was called; Start refuses new sessions
}

func newShellManager() *shellManager {
	return &shellManager{shells: make(map[string]*bashSession)}
}

// Start 通过 `sh -c` 启动命令并返回其 bash_id。这
// 会话一直存在，直到终止或自然退出；调用者的 ctx 不
// 传播给孩子——ctx在回合结束时死亡并且需要
// 每个后台进程都有它。
func (m *shellManager) Start(command string, env []string) (*bashSession, error) {
	if command == "" {
		return nil, errors.New("command is required")
	}

	// 关闭后拒绝新会话。没有这个，一个开始
	// 关闭的比赛可能会在关闭后的地图上进行
	// 并永远泄漏进程。
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("shell manager closed")
	}
	m.mu.Unlock()

	// context.Background 这里是有意为之的——一个后台 shell
	// 必须比产生它的回合更长久。会话自己的取消
	// 是在自然退出之前终止它的唯一路径。
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	// 失败关闭：如果调用者没有传递显式环境，则构建一个
	// 擦掉了一个而不是让 Go 默认为裸 os.Environ()
	// 继承——这条路径就是守护进程秘密到达聊天回复的方式。
	if env != nil {
		cmd.Env = env
	} else {
		cmd.Env = buildSubprocessEnv(nil)
	}

	// 生成一个新的进程组，以便kill_shell到达每个
	// 后代（例如 `sh -c "npm run dev"` 分叉节点 — 没有组
	// 杀死，杀死 sh 使节点运行）。覆盖 CommandContext 的
	// 默认取消（只会直接杀死cmd.Process）发送
	// 对整个组发出 SIGKILL。
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd.Process.Pid)
	}

	out := newOutputBuffer(bufferCap)
	cmd.Stdout = out
	cmd.Stderr = out // outputBuffer's Write is mutex-protected so concurrent stdout+stderr is safe

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start background shell: %w", err)
	}

	// 重新检查是否在执行插入操作的同一个锁下关闭。这
	// 上面的启动前检查只是为了避免分叉 sh
	// 当我们知道我们要关闭时——比赛安全的判决就在这里。
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel() // group-kill the shell we just spawned
		return nil, errors.New("shell manager closed")
	}
	m.counter++
	id := fmt.Sprintf("bash_%d", m.counter)
	s := &bashSession{
		id:        id,
		command:   command,
		startedAt: time.Now(),
		cmd:       cmd,
		cancel:    cancel,
		out:       out,
	}
	m.shells[id] = s
	m.mu.Unlock()

	// Reaper：当进程退出时 cmd.Wait 返回，或者当 ctx 为
	// 取消（杀死）。捕获退出代码，然后翻转完成。
	go func() {
		err := cmd.Wait()
		s.exitErr = err
		var ee *exec.ExitError
		if err == nil {
			s.exitCode = 0
		} else if errors.As(err, &ee) {
			s.exitCode = ee.ExitCode()
		} else {
			// 因取消或管道/IO 错误而终止。使用 -1 表示信号
			// “异常终止”——代理可以消除歧义
			// 快照返回的 exit_err 字符串。
			s.exitCode = -1
		}
		s.done.Store(true)
		// 注意：我们故意不从地图中删除会话
		// 这里。 bash_output 在退出后仍然有用，因此代理可以
		// 获取最终输出和退出状态。注册表.关闭句柄
		// 清理，或者未来的 TTL 驱逐可以分层在上面。
	}()

	return s, nil
}

// Get 通过 bash_id 获取会话，如果未找到则返回 nil。
func (m *shellManager) Get(id string) *bashSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shells[id]
}

// 关闭会杀死每个实时会话，清除注册表，并拒绝任何
// 未来开始。幂等。从注册表调用。关闭代理
// 关闭，这样后台进程就不会比其所有者寿命更长。
func (m *shellManager) Close() {
	m.mu.Lock()
	shells := m.shells
	m.shells = make(map[string]*bashSession)
	m.closed = true
	m.mu.Unlock()
	for _, s := range shells {
		s.cancel()
	}
}

// list 返回所有当前会话的快照，按 id 排序。
// 目前仅用于测试；为未来的 list_shells 工具公开。
func (m *shellManager) list() []*bashSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*bashSession, 0, len(m.shells))
	for _, s := range m.shells {
		out = append(out, s)
	}
	return out
}
