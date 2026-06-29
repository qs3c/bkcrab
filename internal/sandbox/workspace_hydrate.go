package sandbox

import (
	"context"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/qs3c/bkcrab/internal/workspace"
)

// hydrateWorkspace 将工作区存储（S3/本地文件系统/其他）中的每个对象
// 复制到沙箱的 /workspace 目录。每次创建沙箱时调用一次，
// 以便当代理的第一个 `exec` 运行时，沙箱已经包含过去会话中
// 通过 `write_file` 写入的文件。
//
// 实现是单线程且尽力而为的——任何每个文件的错误都会被记录并跳过，
// 而不是使整个填充失败。典型的工作区是少量文件（MB 级的 PDF、音频剪辑）；
// 对于更大的设置，考虑分页/并行复制，或 E2B 的快照 API。
func hydrateWorkspace(ctx context.Context, ws workspace.Store, ex Executor, agentID, projectID, sessionID, sandboxRoot string) {
	if ws == nil || ex == nil {
		return
	}
	objs, err := ws.List(ctx, agentID, projectID, sessionID)
	if err != nil {
		slog.Warn("workspace hydrate: list failed", "agent", agentID, "project", projectID, "session", sessionID, "error", err)
		return
	}
	if len(objs) == 0 {
		return
	}
	copied := 0
	for _, obj := range objs {
		target := path.Join(sandboxRoot, obj.Path)
		rc, getErr := ws.Get(ctx, agentID, projectID, sessionID, obj.Path)
		if getErr != nil {
			slog.Warn("workspace hydrate: get failed", "agent", agentID, "project", projectID, "session", sessionID, "path", obj.Path, "error", getErr)
			continue
		}
		content, readErr := io.ReadAll(rc)
		rc.Close()
		if readErr != nil {
			slog.Warn("workspace hydrate: read failed", "agent", agentID, "project", projectID, "session", sessionID, "path", obj.Path, "error", readErr)
			continue
		}
		// Executor.WriteFile 接受完整的沙箱路径；
		// 所有当前实现（docker/e2b）都隐式处理 mkdir。
		if _, wErr := ex.WriteFile(ctx, target, string(content)); wErr != nil {
			slog.Warn("workspace hydrate: sandbox write failed", "agent", agentID, "project", projectID, "session", sessionID, "path", target, "error", wErr)
			continue
		}
		copied++
	}
	slog.Info("workspace hydrated into sandbox", "agent", agentID, "project", projectID, "session", sessionID, "files", copied, "root", sandboxRoot)
}

// defaultSandboxRoot 是填充后的文件在沙箱内的存放位置。
// 作为常量保持，这样我们就不必仅仅为了一个路径在两个包之间传递配置。
// E2B 和我们的 Docker 沙箱都按约定将其工作目录挂载在 /workspace。
const defaultSandboxRoot = "/workspace"

// sanitizeSandboxPath 去除前导斜杠和 `..` 段，
// 以便即使存储中持有恶意路径，填充的键也无法逃逸 /workspace。
// 镜像 internal/workspace.LocalFS.resolvePath 的逻辑。
func sanitizeSandboxPath(p string) string {
	clean := path.Clean("/" + p)
	return strings.TrimPrefix(clean, "/")
}
