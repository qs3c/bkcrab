// Package workspace 是代理生成的工件（生成的 PDF/图片/音频、
// 下载的文件、中间数据等）的持久化 blob 存储。
//
// 目前提供的两种实现：
//   - LocalFS：写入 ~/.bkcrab/workspaces/<agent>/ 下。默认值；用于
//     单主机部署，保持现有文件系统语义不变。
//   - S3：写入任何兼容 S3 的存储桶（AWS S3, MinIO, R2, B2, ...）。
//     无状态多 Pod 部署必需——即使文件系统是 Pod 本地的，
//     任何 Pod 都可以读写同一对象。
//
// 保持此包不依赖 tools/handlers 的运行时依赖，以便沙箱代码和
// 代理代码可以共享同一个 Store，而不会相互引入依赖。
package workspace

import (
	"context"
	"errors"
	"io"
	"time"
)

// Store 是持久化工件存储。实现必须保证并发安全——
// 两个 Pod 可能同时访问同一个键。
//
// 路径是代理相对的（例如 "report.pdf", "images/cover.png"）。
// 绝不传入绝对路径。实现可以在代理作用域下自由添加自己的
// 命名空间（存储桶前缀、目录树等）。
//
// projectID 和 sessionID 共同命名一次聊天的 workspace 文件夹。
// 两者都设置时 projectID 优先：项目内的每个会话共享同一个文件夹，
// 这正是项目的全部价值（笔记/文件在项目的聊天之间持久化）。
// 磁盘上：
//
//	projectID="", sessionID=""   → <root>/<agentID>/<path>
//	projectID="", sessionID="x"  → <root>/<agentID>/sessions/x/<path>
//	projectID="p", *             → <root>/<agentID>/projects/p/<path>
//
// 两者都为空时 List 返回代理下的所有对象，无论项目/会话——
// 由管理员文件浏览器使用。指定作用域时 List 仅返回该子树。
type Store interface {
	Put(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string) error

	Get(ctx context.Context, agentID, projectID, sessionID, path string) (io.ReadCloser, error)

	Stat(ctx context.Context, agentID, projectID, sessionID, path string) (*ObjectInfo, error)

	List(ctx context.Context, agentID, projectID, sessionID string) ([]ObjectInfo, error)

	Delete(ctx context.Context, agentID, projectID, sessionID, path string) error

	// Move 将会话的整个工作区从一个 (project, session) 作用域
	// 重新定位到另一个。当聊天被拖入或拖出项目时使用：
	// session_id 保持不变，只有 projectID 改变（松散聊天时
	// fromProjectID / toProjectID 之一为 ""）。实现必须拒绝覆盖
	// 非空目标——返回 ErrMoveDestinationExists——以防失误合并
	// 两个聊天的文件。fromSessionID 和 toSessionID 通常相等，
	// 但作为单独参数保留，以防未来调用者想同时重命名会话。
	// 如果源作用域没有对象，则为无操作。
	Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error

	SignedURL(ctx context.Context, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error)
}

// ObjectInfo 描述一个已存储的对象。特定后端未知的字段为零值。
type ObjectInfo struct {
	Path        string    // agent-relative
	Size        int64     // bytes, -1 when unknown
	ContentType string    // e.g. "image/png"
	ModTime     time.Time // UTC
}

// 常见错误。实现在添加上下文时应使用 fmt.Errorf("%w: ...") 包装这些错误，
// 以便调用者仍能通过 errors.Is() 匹配。
var (
	ErrNotFound                = errors.New("workspace: object not found")
	ErrSignedURLUnsupported    = errors.New("workspace: signed URLs not supported by this backend")
	ErrMoveDestinationExists   = errors.New("workspace: move destination already exists")
)
