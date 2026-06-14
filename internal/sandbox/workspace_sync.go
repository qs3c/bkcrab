package sandbox

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceStore 抽象了用户工作区文件的持久化存储。
// 实现可以是 S3、MinIO、本地文件系统或数据库 BLOB。
type WorkspaceStore interface {
	// List 返回用户工作区的所有文件路径。
	List(ctx context.Context, userID string) ([]string, error)
	// Get 从持久化存储中读取文件。
	Get(ctx context.Context, userID, path string) (io.ReadCloser, error)
	// Put 将文件写入持久化存储。
	Put(ctx context.Context, userID, path string, r io.Reader) error
	// Delete 从持久化存储中删除文件。
	Delete(ctx context.Context, userID, path string) error
}

// WorkspaceSync 处理从持久化存储中填充沙箱工作区以及将更改刷新回去。
type WorkspaceSync struct {
	store WorkspaceStore
}

// NewWorkspaceSync 创建一个同步管理器。
func NewWorkspaceSync(store WorkspaceStore) *WorkspaceSync {
	return &WorkspaceSync{store: store}
}

// Hydrate 将用户的所有工作区文件下载到本地目录。
// 在首次为用户创建沙箱时调用。
func (ws *WorkspaceSync) Hydrate(ctx context.Context, userID, localDir string) error {
	files, err := ws.store.List(ctx, userID)
	if err != nil {
		return fmt.Errorf("list workspace files: %w", err)
	}

	for _, path := range files {
		rc, err := ws.store.Get(ctx, userID, path)
		if err != nil {
			slog.Warn("workspace hydrate: skip file", "user", userID, "path", path, "error", err)
			continue
		}

		localPath := filepath.Join(localDir, path)
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			rc.Close()
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(localPath), err)
		}

		f, err := os.Create(localPath)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create %s: %w", localPath, err)
		}
		_, copyErr := io.Copy(f, rc)
		f.Close()
		rc.Close()
		if copyErr != nil {
			return fmt.Errorf("write %s: %w", localPath, copyErr)
		}
	}

	slog.Info("workspace hydrated", "user", userID, "files", len(files))
	return nil
}

// Flush 将本地目录中的所有文件上传到持久化存储。
// 在沙箱即将被销毁时调用。
func (ws *WorkspaceSync) Flush(ctx context.Context, userID, localDir string) error {
	var count int
	err := filepath.Walk(localDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		relPath, _ := filepath.Rel(localDir, path)
		relPath = filepath.ToSlash(relPath)

		// 跳过隐藏文件和已知的临时文件
		if strings.HasPrefix(filepath.Base(relPath), ".") {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		if err := ws.store.Put(ctx, userID, relPath, f); err != nil {
			slog.Warn("workspace flush: skip file", "user", userID, "path", relPath, "error", err)
			return nil // continue flushing other files
		}
		count++
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", localDir, err)
	}
	slog.Info("workspace flushed", "user", userID, "files", count)
	return nil
}

// SyncFile 将单个文件上传到持久化存储。在 write_file 工具执行后调用，
// 用于实时同步（可选——调用者可以通过 Flush 批量处理）。
func (ws *WorkspaceSync) SyncFile(ctx context.Context, userID, localDir, relPath string) error {
	fullPath := filepath.Join(localDir, relPath)
	f, err := os.Open(fullPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return ws.store.Put(ctx, userID, filepath.ToSlash(relPath), f)
}
