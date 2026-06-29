package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LocalFS 将对象存储在根目录下，每个代理一个子树。
// 这是单主机部署的默认后端——与代理工具已使用的磁盘布局相同，
// 因此现有代理可以原地升级。
type LocalFS struct {
	// root 通常是 ~/.bkcrab/workspaces。代理 foo 的对象位于
	// <root>/foo/<path>。
	root string
}

// NewLocalFS 返回以给定目录为根的 LocalFS。该目录在首次 Put 时创建；
// 调用者无需预先创建。
func NewLocalFS(root string) *LocalFS {
	return &LocalFS{root: root}
}

// Root 返回构造 LocalFS 时使用的磁盘根目录（通常为
// ~/.bkcrab/workspaces）。公开以便需要为外部工具计算主机路径的
// 调用者——例如"在 Finder 中打开"/外部调用——可以从 LocalFS 内部
// 使用的同一锚点拼接路径，而无需从 BKCRAB_HOME 重新推导。
func (f *LocalFS) Root() string {
	return f.root
}

// LocalScopeDir 实现 LocalScoper 标记。对 LocalFS 始终返回
// (path, true)——每个 (agent, project, session) 三元组都有我们
// 可以在 Finder 中展示的真实主机目录。Store 的 S3/R2 实现不实现
// LocalScoper，因此通过类型断言探测的处理程序得到 ok=false
// 并返回 503。
func (f *LocalFS) LocalScopeDir(agentID, projectID, sessionID string) (string, bool) {
	return f.scopeDir(agentID, projectID, sessionID), true
}

// scopeDir 返回 (agent, project, session) 作用域的磁盘目录：
//
//	pid="", sid=""   →  <root>/<agent>/                          （代理共享）
//	pid="", sid="x"  →  <root>/<agent>/sessions/x/               （松散聊天）
//	pid="p", sid=""  →  <root>/<agent>/projects/p/               （项目根目录）
//	pid="p", sid="x" →  <root>/<agent>/projects/p/x/             （项目聊天）
//
// 项目聊天在项目内保留自己的子目录，以便两个并发聊天不会在
// `notes.md` 上冲突，且"将聊天移入/移出项目"是单个目录重命名。
// 项目聊天的沙箱容器挂载项目根目录（以便同级在 `/workspace/<other-sid>/...`
// 可见），但 cwd 进入聊天的子目录，以便相对写入默认写入聊天自己的文件
// ——参见 docker_executor.go 的 pool.Get。
func (f *LocalFS) scopeDir(agentID, projectID, sessionID string) string {
	switch {
	case projectID != "" && sessionID != "":
		return filepath.Join(f.root, agentID, "projects", projectID, sessionID)
	case projectID != "":
		return filepath.Join(f.root, agentID, "projects", projectID)
	case sessionID != "":
		return filepath.Join(f.root, agentID, "sessions", sessionID)
	default:
		return filepath.Join(f.root, agentID)
	}
}

// resolvePath 将 scopeDir 与路径连接，并拒绝通过 ".." 逃逸的尝试。
// 作用域目录内的任何符号链接都保持不变——通过符号链接逃逸是用户控制的
// 文件系统级信任边界。
func (f *LocalFS) resolvePath(agentID, projectID, sessionID, path string) (string, error) {
	dir := f.scopeDir(agentID, projectID, sessionID)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absDir, filepath.Clean("/"+path)) // strip leading ../
	if full != absDir && !strings.HasPrefix(full, absDir+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace: path %q escapes scope root", path)
	}
	return full, nil
}

func (f *LocalFS) Put(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, _ int64, _ string) error {
	full, err := f.resolvePath(agentID, projectID, sessionID, path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (f *LocalFS) Get(ctx context.Context, agentID, projectID, sessionID, path string) (io.ReadCloser, error) {
	full, err := f.resolvePath(agentID, projectID, sessionID, path)
	if err != nil {
		return nil, err
	}
	rc, err := os.Open(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return rc, err
}

func (f *LocalFS) Stat(ctx context.Context, agentID, projectID, sessionID, path string) (*ObjectInfo, error) {
	full, err := f.resolvePath(agentID, projectID, sessionID, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ObjectInfo{
		Path:        path,
		Size:        info.Size(),
		ContentType: mime.TypeByExtension(filepath.Ext(path)),
		ModTime:     info.ModTime().UTC(),
	}, nil
}

// List 遍历作用域目录下的文件。当 projectID 和 sessionID 都为空时，
// 我们递归遍历代理根目录——会话和项目子树以前缀形式出现，如
// "sessions/<id>/file.png" 或 "projects/<id>/notes.md"，这正是管理员
// 文件浏览器想要的。设置任一参数时，我们仅遍历该子树。
func (f *LocalFS) List(ctx context.Context, agentID, projectID, sessionID string) ([]ObjectInfo, error) {
	dir := f.scopeDir(agentID, projectID, sessionID)
	var out []ObjectInfo
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, ObjectInfo{
			Path:        filepath.ToSlash(rel),
			Size:        info.Size(),
			ContentType: mime.TypeByExtension(filepath.Ext(p)),
			ModTime:     info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// Move 将源作用域目录重命名为目标作用域目录。
// LocalFS 通过一个 os.Rename 即可免费实现——源和目标都在同一个
// 代理根目录下，因此内核原子地处理它（在同一个文件系统内）。
// 拒绝覆盖非空目标，以免有 bug 的调用者静默合并两个聊天的文件；
// 此时返回 ErrMoveDestinationExists。
//
// 当源目录不存在时无操作（聊天尚未写入任何工作区文件——
// 这对于全新会话很常见）。空的目标目录首先被删除，以便早期
// 代码路径中的 MkdirAll 风格占位符不会触发冲突检查。
func (f *LocalFS) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	src := f.scopeDir(agentID, fromProjectID, fromSessionID)
	dst := f.scopeDir(agentID, toProjectID, toSessionID)
	if src == dst {
		return nil
	}
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if info, err := os.Stat(dst); err == nil {
		if info.IsDir() {
			entries, rderr := os.ReadDir(dst)
			if rderr != nil {
				return rderr
			}
			if len(entries) == 0 {
				if rmErr := os.Remove(dst); rmErr != nil {
					return rmErr
				}
			} else {
				return ErrMoveDestinationExists
			}
		} else {
			return ErrMoveDestinationExists
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (f *LocalFS) Delete(ctx context.Context, agentID, projectID, sessionID, path string) error {
	full, err := f.resolvePath(agentID, projectID, sessionID, path)
	if err != nil {
		return err
	}
	err = os.Remove(full)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SignedURL 本地文件不支持——没有可签名的内容。
// 需要将 URL 提供给浏览器的调用点应回退到网关现有的
// /api/agents/{id}/files/{path} 端点，该端点通过已认证的通道传输文件。
func (f *LocalFS) SignedURL(ctx context.Context, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error) {
	return "", ErrSignedURLUnsupported
}
