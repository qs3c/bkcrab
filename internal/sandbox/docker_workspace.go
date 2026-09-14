package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/qs3c/bkcrab/internal/workspace"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// SyncWorkspace reads only one bounded buffer at a time. Root confines all file
// opens even when an agent concurrently replaces a directory with a symlink.
// Same-size modifications are detected by content, not by object length.
func (d *DockerExecutor) SyncWorkspace(ctx context.Context, ws workspace.Store, agent, project, session string) error {
	if project != "" {
		session = ""
	}
	if d.sb.workspace == "" {
		return nil
	}
	root, err := os.OpenRoot(d.sb.workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err = f.Stat()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		h := sha256.New()
		if _, err = io.Copy(h, contextReader{ctx, f}); err != nil {
			return err
		}
		digest := string(h.Sum(nil))
		// Cache successful contents only. Failed uploads remain retryable.
		if d.synced != nil && d.synced[p] == digest {
			return nil
		}
		old, err := ws.Get(ctx, agent, project, session, p)
		if err == nil {
			h.Reset()
			n, readErr := io.Copy(h, io.LimitReader(contextReader{ctx, old}, info.Size()+1))
			old.Close()
			if readErr != nil {
				return readErr
			}
			if n == info.Size() && string(h.Sum(nil)) == digest {
				d.remember(p, digest)
				return nil
			}
		} else if !errors.Is(err, workspace.ErrNotFound) {
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		// Rehash bytes actually uploaded; a concurrent background writer must
		// not make our cache describe bytes different from the stored object.
		h.Reset()
		if err = ws.Put(ctx, agent, project, session, p, io.TeeReader(contextReader{ctx, f}, h), info.Size(), ""); err != nil {
			return fmt.Errorf("sync %s: %w", p, err)
		}
		d.remember(p, string(h.Sum(nil)))
		return nil
	})
}
func (d *DockerExecutor) remember(p, digest string) {
	if d.synced == nil {
		d.synced = map[string]string{}
	}
	d.synced[p] = digest
}

// HydrateWorkspace never overwrites an existing local file: it may be newer
// than MinIO after a crash. Restore missing objects without buffering them.
func (d *DockerExecutor) HydrateWorkspace(ctx context.Context, ws workspace.Store, agent, project, session string) error {
	if project != "" {
		session = ""
	}
	root, err := os.OpenRoot(d.sb.workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	objects, err := ws.List(ctx, agent, project, session)
	if err != nil {
		return err
	}
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := obj.Path
		if !fs.ValidPath(p) || p == "." {
			return fmt.Errorf("invalid stored workspace path %q", p)
		}
		if _, err := root.Lstat(p); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		r, err := ws.Get(ctx, agent, project, session, p)
		if err != nil {
			return err
		}
		f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			r.Close()
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		_, copyErr := io.Copy(f, contextReader{ctx, r})
		r.Close()
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			root.Remove(p)
			return errors.Join(copyErr, closeErr)
		}
	}
	return nil
}
