package sandbox

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkclaw/internal/workspace"
)

// bytesReader 将字节切片包装为 io.Reader——内联辅助函数，
// 以便刷新代码不会因 bytes.NewReader 调用而变得混乱。
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// LifecyclePool 用两个对多租户云部署成本至关重要的控制旋钮包装任何 ExecutorPool：
//
//  1. 延迟创建——沙箱在第一个工具调用之前不会被启动。
//     仅聊天（没有 exec/read_file/write_file）的代理永远不会启动沙箱，
//     因此空闲用户无需为沙箱计算付费。
//  2. 空闲驱逐——后台清理器 Release() 在 IdleTTL 内未被使用的沙箱。
//     下一次调用重新创建它们；在此期间没有任何运行。
//
// 后端无关：适用于 DockerExecutorPool、E2B 或任何未来的实现。
// 内部池仍然处理实际的创建/销毁。
type LifecyclePool struct {
	inner   ExecutorPool
	idleTTL time.Duration
	sweep   time.Duration

	mu sync.Mutex
	// 两个映射都使用 poolKey(agentID, sessionID) 作为键，
	// 以便可以独立跟踪每个会话的沙箱。lastUsed 驱动空闲驱逐；
	// hydrated 跟踪我们是否已经将 workspace.Store 内容复制到此沙箱
	//（驱逐时变为 false，以便下一次延迟创建从持久化存储重新填充）。
	lastUsed map[string]time.Time
	hydrated map[string]bool
	// scopes 将相同的复合键映射回 (agentID, sessionID)，
	// 以便刷新+释放路径可以与正确的工作区作用域通信，而无需重新解析键。
	scopes map[string]sandboxScope

	// workspace 是可选的 blob 存储，在沙箱创建时引导 /workspace。
	// 当为 nil 时，沙箱从空开始，并依赖 write_file 工具调用
	//（已经通过 workspace.Store 写入）来生成代理稍后通过 read_file 读取的文件。
	workspace workspace.Store

	stopCh chan struct{}
	done   chan struct{}
}

// sandboxScope 是沙箱所属的 (agentID, projectID, sessionID) 元组。
// 与复合映射键一起存储，以便生命周期代码可以使用正确的作用域
// 回调 ExecutorPool.Get/Release，而无需重新解析。
type sandboxScope struct {
	agentID   string
	projectID string
	sessionID string
}

// NewLifecyclePool 使用空闲跟踪包装内部池。idleTTL=0 禁用驱逐
//（所有内容保持活动）；sweep=0 使用合理的默认值。
func NewLifecyclePool(inner ExecutorPool, idleTTL, sweep time.Duration) *LifecyclePool {
	if sweep <= 0 {
		sweep = 30 * time.Second
	}
	return &LifecyclePool{
		inner:    inner,
		idleTTL:  idleTTL,
		sweep:    sweep,
		lastUsed: make(map[string]time.Time),
		hydrated: make(map[string]bool),
		scopes:   make(map[string]sandboxScope),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetWorkspace 安装用于在第一个工具调用时引导每个沙箱的持久化 blob 存储。
// 传递 nil 以禁用填充（沙箱以空的 /workspace 启动）。
//
// 在创建时自行填充的池（E2B 使用一次 tar+exec 往返来处理技能和工作区；
// 参见 E2BExecutorPool.Get → Hydrate）通过内部池上的 SetWorkspace
// 接收相同的存储，以便它们有机会将其折叠到批量上传中——
// 这比我们为 docker 保留的按文件回退更快更可靠。
func (p *LifecyclePool) SetWorkspace(ws workspace.Store) {
	p.workspace = ws
	if sw, ok := p.inner.(workspaceAware); ok {
		sw.SetWorkspace(ws)
	}
}

// workspaceAware 由内部池实现，这些池将工作区填充折叠到
// 自己的创建时批量上传中（因此 LifecyclePool 不应通过按文件路径重复填充）。
type workspaceAware interface {
	SetWorkspace(ws workspace.Store)
}

// Start 启动空闲清理 goroutine。可以安全地多次调用；只有第一次启动
// 实际启动循环。
func (p *LifecyclePool) Start() {
	if p.idleTTL <= 0 {
		close(p.done) // nothing to do; keep Shutdown() cheap
		return
	}
	go p.loop()
}

func (p *LifecyclePool) loop() {
	defer close(p.done)
	t := time.NewTicker(p.sweep)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			p.evictIdle()
		}
	}
}

// evictIdle 扫描 lastUsed 并 Release() 任何早于 idleTTL 的内容。
// 持有每次迭代的锁；Release 可能很慢（销毁容器），
// 因此我们在实际拆除前释放映射锁，以避免阻塞其他代理上的新 Get()。
func (p *LifecyclePool) evictIdle() {
	cutoff := time.Now().Add(-p.idleTTL)
	p.mu.Lock()
	toEvict := make([]sandboxScope, 0)
	for k, t := range p.lastUsed {
		if t.Before(cutoff) {
			toEvict = append(toEvict, p.scopes[k])
		}
	}
	// 在锁下从映射中移除，以免竞态的 Get 将被驱逐的沙箱误认为活动的。
	// 同时清除 hydrated，以便下一次延迟创建从工作区存储重新同步。
	for _, sc := range toEvict {
		k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
		delete(p.lastUsed, k)
		delete(p.hydrated, k)
		delete(p.scopes, k)
	}
	p.mu.Unlock()

	for _, sc := range toEvict {
		// 尽力刷新：如果执行器实现了 WorkspaceSnapshotter
		// 并且我们有工作区存储，则在销毁前上传沙箱写入的任何内容
		//（尚未通过 write_file 写入的）。
		p.flushIfSupported(sc)

		if err := p.inner.Release(sc.agentID, sc.projectID, sc.sessionID); err != nil {
			slog.Warn("sandbox evict failed", "agent", sc.agentID, "session", sc.sessionID, "error", err)
			continue
		}
		slog.Info("sandbox evicted (idle)", "agent", sc.agentID, "session", sc.sessionID, "idleTTL", p.idleTTL)
	}
}

// flushIfSupported 快照沙箱工作区并上传任何尚未在持久化存储中的内容。
// 当后端没有实现 WorkspaceSnapshotter（除了 E2B 之外，
// docker 是当前唯一的实现者）或未配置 workspace.Store 时静默跳过。
func (p *LifecyclePool) flushIfSupported(sc sandboxScope) {
	if p.workspace == nil {
		return
	}
	ex, err := p.inner.Get(context.Background(), sc.agentID, sc.projectID, sc.sessionID)
	if err != nil {
		return
	}
	p.syncSnapshot(context.Background(), sc, ex, "evict")
}

// syncSnapshot 执行实际的快照+差异+Put 工作。
// 从 flushIfSupported 中提取出来，以便执行后同步（lazyExecutor.Exec）
// 可以重用它，而无需通过内部池重新获取执行器。
// `cause` 是一个日志标签，以便我们可以在 slog 中区分驱逐刷新和每次执行的同步。
func (p *LifecyclePool) syncSnapshot(ctx context.Context, sc sandboxScope, ex Executor, cause string) {
	if p.workspace == nil {
		return
	}
	snapper, ok := ex.(WorkspaceSnapshotter)
	if !ok {
		return
	}
	files, err := snapper.SnapshotWorkspace(ctx)
	if err != nil {
		slog.Warn("sandbox sync: snapshot failed", "agent", sc.agentID, "session", sc.sessionID, "cause", cause, "error", err)
		return
	}
	written := 0
	for path, data := range files {
		// 跳过存储中已存在且大小相同的文件——
		// 避免在没有更改时每次同步都重写每个文件。
		// 内容相等性会更严格，但需要对每个文件进行完整的往返；大小通常就足够了。
		if info, err := p.workspace.Stat(ctx, sc.agentID, sc.projectID, sc.sessionID, path); err == nil && info.Size == int64(len(data)) {
			continue
		}
		if err := p.workspace.Put(ctx, sc.agentID, sc.projectID, sc.sessionID, path, bytesReader(data), int64(len(data)), ""); err != nil {
			slog.Warn("sandbox sync: put failed", "agent", sc.agentID, "session", sc.sessionID, "cause", cause, "path", path, "error", err)
			continue
		}
		written++
	}
	if written > 0 {
		slog.Info("sandbox synced to workspace store", "agent", sc.agentID, "session", sc.sessionID, "cause", cause, "files", written)
	}
}

// Get 返回一个延迟代理：其上的工具调用将按需从内部池获取底层执行器
//（如果需要则创建新的沙箱）并更新最后使用的时间戳。
//
// 契约匹配 ExecutorPool.Get，因此 LifecyclePool 是一个即插即用的包装器。
func (p *LifecyclePool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	return &lazyExecutor{pool: p, scope: sandboxScope{agentID: agentID, projectID: projectID, sessionID: sessionID}}, nil
}

// Release 转发到内部池并删除 lastUsed 条目。对于显式拆除（代理删除）
// 很有用——正常流程依赖空闲驱逐。
func (p *LifecyclePool) Release(agentID, projectID, sessionID string) error {
	k := poolKey(agentID, projectID, sessionID)
	p.mu.Lock()
	delete(p.lastUsed, k)
	delete(p.hydrated, k)
	delete(p.scopes, k)
	p.mu.Unlock()
	return p.inner.Release(agentID, projectID, sessionID)
}

// CloseAll 停止清理器并拆除每个活动的沙箱。在网关关闭时调用；
// 跳过此步骤将导致 E2B 实例泄漏，直到其最大 TTL 过期前一直计费。
func (p *LifecyclePool) CloseAll() {
	select {
	case <-p.stopCh:
		// 已经停止
	default:
		close(p.stopCh)
	}
	<-p.done
	p.inner.CloseAll()
	p.mu.Lock()
	p.lastUsed = make(map[string]time.Time)
	p.hydrated = make(map[string]bool)
	p.scopes = make(map[string]sandboxScope)
	p.mu.Unlock()
}

// inner 获取底层 Executor，在第一次调用时创建。
// 与 Get() 分开，以便 lazyExecutor 可以每次更新 lastUsed。
// 首次创建（无论是全新还是驱逐后）时，它会从配置的 workspace.Store
// 填充 /workspace，以便执行的命令看到 write_file 在之前会话中产生的文件。
func (p *LifecyclePool) getInner(ctx context.Context, sc sandboxScope) (Executor, error) {
	k := poolKey(sc.agentID, sc.projectID, sc.sessionID)
	p.mu.Lock()
	needsHydrate := !p.hydrated[k]
	p.lastUsed[k] = time.Now()
	p.scopes[k] = sc
	if needsHydrate {
		p.hydrated[k] = true // 提前设置，以便并发的第二次调用不会重复填充
	}
	p.mu.Unlock()

	ex, err := p.inner.Get(ctx, sc.agentID, sc.projectID, sc.sessionID)
	if err != nil {
		// 回滚 hydrated 标志，以便重试时会再次尝试。
		p.mu.Lock()
		p.hydrated[k] = false
		p.mu.Unlock()
		return nil, err
	}
	// 当内部池已经将 /workspace 作为其自己的批量填充的一部分推送时
	//（E2B 这样做——一次 tar.gz 通过 exec 一次性涵盖 /skills 和 /workspace），
	// 跳过按文件回退。否则（docker），通过 ex.WriteFile 复制每个对象。
	if needsHydrate && p.workspace != nil {
		if _, selfHydrates := p.inner.(workspaceAware); !selfHydrates {
			hydrateWorkspace(ctx, p.workspace, ex, sc.agentID, sc.projectID, sc.sessionID, defaultSandboxRoot)
		}
	}
	return ex, nil
}

// lazyExecutor 是 Get() 返回的内容。每个工具调用通过 pool.getInner 路由，
// 它 (a) 刷新空闲计时器，并且 (b) 如果这是自上次驱逐以来的第一次调用，
// 则延迟创建真正的沙箱。
type lazyExecutor struct {
	pool  *LifecyclePool
	scope sandboxScope
}

func (l *lazyExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	out, execErr := ex.Exec(ctx, command, timeout)
	// 仅对云沙箱（RemoteWorkspace 标记）执行执行后同步。
	// Docker 的 /workspace 绑定挂载到主机，因此文件立即出现，无需同步；
	// 每次 exec 后重复快照+Put 循环只会搅动 workspace.Store
	//（当它是 S3 后端时特别昂贵）。
	// E2B 的 /workspace 位于云沙箱内部；没有这种拉取，
	// 技能写入的文件（image-tool 的 /workspace/gen_xxx.webp）
	// 永远无法到达主机，UI 将显示损坏的图像。
	// 尽力而为——从不覆盖 exec 结果。
	if _, remote := ex.(RemoteWorkspace); remote {
		l.pool.syncSnapshot(ctx, l.scope, ex, "post-exec")
	}
	return out, execErr
}

func (l *lazyExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	return ex.ReadFile(ctx, path)
}

func (l *lazyExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	out, writeErr := ex.WriteFile(ctx, path, content)
	// 在云沙箱上将写入镜像到持久化存储——与上述执行后同步相同的原因。
	// 没有这个，落到 ex.WriteFile 的 write_file（和 apply_patch）调用
	//（任何绝对 /workspace 路径——参见 file.go 的 isWorkspacePath，
	// 它拒绝绝对路径）只进入 E2B 沙箱，并在空闲驱逐时消失，
	// 永远无法到达 UI 和签名 URL 读取的主机 workspace.Store。
	// 针对性的单文件 Put 而不是 syncSnapshot：我们已经将字节保留在内存中，
	// 不需要每次写入都进行完整的 tar 往返。尽力而为——从不覆盖写入结果。
	if writeErr == nil {
		if _, remote := ex.(RemoteWorkspace); remote {
			l.pool.mirrorSandboxWrite(ctx, l.scope, path, content)
		}
	}
	return out, writeErr
}

// mirrorSandboxWrite 将单个沙箱端的写入复制到持久化 workspace.Store。
// 跳过 /workspace 之外的路径（例如 /tmp/、/home/user/），
// 因为这些路径没有存储映射。镜像 syncSnapshot 使用的 sessionID 作用域 Put，
// 因此 write_file 和 exec 生成的文件落在相同的存储位置。
func (p *LifecyclePool) mirrorSandboxWrite(ctx context.Context, sc sandboxScope, sandboxPath, content string) {
	if p.workspace == nil {
		return
	}
	const prefix = "/workspace/"
	if !strings.HasPrefix(sandboxPath, prefix) {
		return
	}
	key := strings.TrimPrefix(sandboxPath, prefix)
	if key == "" {
		return
	}
	if err := p.workspace.Put(ctx, sc.agentID, sc.projectID, sc.sessionID, key,
		bytesReader([]byte(content)), int64(len(content)), ""); err != nil {
		slog.Warn("sandbox sync: write_file mirror failed",
			"agent", sc.agentID, "project", sc.projectID, "session", sc.sessionID,
			"path", key, "error", err)
		return
	}
	slog.Debug("sandbox synced to workspace store", "agent", sc.agentID, "session", sc.sessionID, "cause", "post-write", "path", key)
}

func (l *lazyExecutor) ListDir(ctx context.Context, path string) (string, error) {
	ex, err := l.pool.getInner(ctx, l.scope)
	if err != nil {
		return "", err
	}
	return ex.ListDir(ctx, path)
}

// Close 在延迟代理上是空操作——底层执行器的生命周期由 LifecyclePool 拥有，
// 而不是任何持有句柄的单个调用者。真正的拆除通过 LifecyclePool.Release / CloseAll 发生。
func (l *lazyExecutor) Close() error { return nil }

// Backend 通过委托给内部池来报告提供者名称，内部池将其作为类型级常量。
// 无需实例化真正的执行器——答案在池的生命周期内是静态的。
func (l *lazyExecutor) Backend() string { return l.pool.inner.Backend() }

// LifecyclePool 上的 Backend 委托给包装的内部池。镜像每个执行器的 Backend，
// 以便调用者可以在包装器堆栈的任何层询问"这是哪个提供者？"。
func (p *LifecyclePool) Backend() string { return p.inner.Backend() }

// 确保接口被满足。
var (
	_ Executor     = (*lazyExecutor)(nil)
	_ ExecutorPool = (*LifecyclePool)(nil)
)
