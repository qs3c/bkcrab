package sandbox

import (
	"context"
	"time"
)

// Executor 抽象了单个用户的沙箱化执行环境。
// 在云模式下，所有代理工具调用（exec、read_file、write_file、list_dir）
// 都通过此接口路由，以便每个用户获得隔离的文件系统和运行时。
// 实现可以是 Docker 容器、Firecracker 微 VM、E2B 托管沙箱或任何其他后端。
type Executor interface {
	// Exec 运行一个 shell 命令并返回合并的 stdout+stderr。
	Exec(ctx context.Context, command string, timeout time.Duration) (string, error)
	// ReadFile 从沙箱文件系统读取文件。
	ReadFile(ctx context.Context, path string) (string, error)
	// WriteFile 将内容写入文件（根据需要创建父目录）。
	WriteFile(ctx context.Context, path, content string) (string, error)
	// ListDir 列出目录并返回人类可读的列表。
	ListDir(ctx context.Context, path string) (string, error)
	// Backend 返回底层提供者的简短标识符
	//（"docker"、"e2b"、"boxlite"）。用于日志行，以便操作员可以
	// 一目了然地确认哪个提供者处理了给定的 exec。
	Backend() string
	// Close 销毁沙箱并释放资源。
	Close() error
}

// ExecutorPool 管理每个（代理、项目、会话）的沙箱生命周期。
// Get 在首次访问时延迟创建沙箱；Release 拆除它。
//
// 为什么是 agentID + projectID + sessionID：同一代理的并行会话
// 不能看到彼此的 /workspace 文件（冲突 + 串扰）——
// 每个会话获得自己的容器，具有会话作用域的绑定挂载。
// projectID 覆盖该隔离：同一项目中的每个聊天共享一个挂载在项目文件夹上的容器，
// 因此笔记/文件在项目的聊天之间持久存在。
// 池键是 (agentID, projectID, sessionID)；空 project + 空 session
// 是旧调用者使用的代理共享作用域。
type ExecutorPool interface {
	Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error)
	Release(agentID, projectID, sessionID string) error
	CloseAll()
	// Backend 返回底层提供者的简短标识符
	//（"docker"、"e2b"、"boxlite"）。镜像 Executor.Backend，
	// 以便持有池句柄的调用者不必延迟解析执行器来了解提供者名称。
	Backend() string
}

// WorkspaceSnapshotter 是 Executor 可以实现的可选能力，以支持驱逐时刷新。
// 返回从沙箱相对路径到 /workspace 下所有内容的文件内容映射。
//
// 实现预期是尽力而为的：文件内容应反映调用时沙箱看到的内容，
// 但不保证与活动 shell 的完全一致性（代理在刷新期间不应写入）。
// 大文件/二进制文件按原样返回。
//
// 不是基础 Executor 接口的一部分，因为并非每个后端都可以廉价地枚举其工作区
//（例如 E2B 需要额外的 API 调用）；调用者应进行类型断言并在缺失时优雅跳过。
type WorkspaceSnapshotter interface {
	SnapshotWorkspace(ctx context.Context) (map[string][]byte, error)
}

// RemoteWorkspace 标记其 /workspace 不与主机文件系统共享（无绑定挂载）的执行器。
// 实现者需要在每次成功执行后进行显式同步——否则技能在沙箱内写入的文件
//（例如 image-tool 的 /workspace/gen_xxx.webp）永远无法对主机的
// workspace.Store 可见，UI 呈现它们时会出问题。
// Docker 不实现此接口（其 /workspace 是绑定挂载，exec 返回时文件已在主机上）；
// E2B 实现它。
type RemoteWorkspace interface {
	IsRemoteWorkspace()
}

// PoolConfig 保存创建沙箱池的配置。
type PoolConfig struct {
	Backend   string // "docker"、"e2b"（未来）
	Image     string // 容器镜像（用于 docker 后端）
	Policy    *Policy
	// E2B 特定字段（未来）
	E2BTemplate string
	E2BAPIKey   string
}
