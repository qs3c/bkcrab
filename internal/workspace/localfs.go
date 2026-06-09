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

// LocalFS stores objects under a root directory, one subtree per agent. This
// is the default backend for single-host deployments — same on-disk layout
// the agent tools already use, so existing agents upgrade in place.
type LocalFS struct {
	// root is usually ~/.bkclaw/workspaces. Objects for agent foo go to
	// <root>/foo/<path>.
	root string
}

// NewLocalFS returns a LocalFS rooted at the given directory. The directory
// is created on first Put; callers don't need to pre-create it.
func NewLocalFS(root string) *LocalFS {
	return &LocalFS{root: root}
}

// Root returns the on-disk root the LocalFS was constructed with
// (typically ~/.bkclaw/workspaces). Exposed so callers that need
// to compute a host path for an external tool — e.g. "open in
// Finder" / shelling out — can join from the same anchor LocalFS
// uses internally without re-deriving it from BKCLAW_HOME.
func (f *LocalFS) Root() string {
	return f.root
}

// LocalScopeDir implements the LocalScoper marker. Always returns
// (path, true) for LocalFS — every (agent, project, session) tuple
// has a real host directory we can reveal in Finder. S3 / R2
// implementations of Store don't implement LocalScoper, so handlers
// that probe via type-assertion get ok=false and 503 the request.
func (f *LocalFS) LocalScopeDir(agentID, projectID, sessionID string) (string, bool) {
	return f.scopeDir(agentID, projectID, sessionID), true
}

// scopeDir returns the on-disk directory for a (agent, project, session)
// scope:
//
//	pid="", sid=""   →  <root>/<agent>/                          (agent-shared)
//	pid="", sid="x"  →  <root>/<agent>/sessions/x/               (loose chat)
//	pid="p", sid=""  →  <root>/<agent>/projects/p/               (project root)
//	pid="p", sid="x" →  <root>/<agent>/projects/p/x/             (project chat)
//
// Project chats keep their own subdir inside the project so two
// concurrent chats can't collide on `notes.md`, and "move chat into
// /out of project" is a single directory rename. Sandbox containers
// for project chats mount the project root (so siblings are visible
// at `/workspace/<other-sid>/...`) but cwd into the chat's subdir
// so relative writes default to the chat's own files — see
// docker_executor.go's pool.Get.
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

// resolvePath joins scopeDir with path and rejects attempts to escape via
// "..". Any symbolic link inside the scope dir is left alone — escape via
// symlinks is a filesystem-level trust boundary users control.
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

// List walks files under the scope dir. With projectID and sessionID
// both empty we walk the agent root recursively — session and project
// subtrees show up with prefixes like "sessions/<id>/file.png" or
// "projects/<id>/notes.md", which is what the admin file browser wants.
// With either set we walk only that subtree.
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

// Move renames the source scope dir to the destination scope dir.
// LocalFS gets it for free as a single os.Rename — both source and
// destination live under the same agent root, so the kernel handles
// it atomically (within one filesystem). Refuses to clobber a
// non-empty destination so a buggy caller can't silently merge two
// chats' files; returns ErrMoveDestinationExists in that case.
//
// No-op when the source dir doesn't exist (the chat never wrote any
// workspace files yet — common for brand-new sessions). Empty
// destination dirs are removed first so MkdirAll-style placeholders
// from earlier code paths don't trip the conflict check.
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

// SignedURL is not supported for local files — there's nothing to sign. Call
// sites that need to hand a URL to a browser should fall through to the
// gateway's existing /api/agents/{id}/files/{path} endpoint, which streams
// the file over the authenticated channel.
func (f *LocalFS) SignedURL(ctx context.Context, agentID, projectID, sessionID, path string, ttl time.Duration) (string, error) {
	return "", ErrSignedURLUnsupported
}
