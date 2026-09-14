package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qs3c/bkcrab/internal/workspace"
)

type skillDirsKey struct{}

// WithSkillDirs pins the skill view selected for one turn. An explicitly empty
// view is distinct from a legacy caller that has no override.
func WithSkillDirs(ctx context.Context, dirs []string) context.Context {
	return context.WithValue(ctx, skillDirsKey{}, slices.Clone(dirs))
}
func requestedSkillDirs(ctx context.Context, fallback []string) []string {
	if dirs, ok := ctx.Value(skillDirsKey{}).([]string); ok {
		return slices.Clone(dirs)
	}
	return fallback
}
func skillViewChanged(ctx context.Context, current []string) bool {
	dirs, ok := ctx.Value(skillDirsKey{}).([]string)
	return ok && !slices.Equal(dirs, current)
}

// A published skill is a symlink to an immutable version. Internal package
// symlinks remain unsupported; only the top-level skill root is resolved.
func skillDirectory(root, name string) (string, bool) {
	if strings.HasPrefix(name, ".") {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolved)
	return resolved, err == nil && info.IsDir()
}

// Switching a cloud sandbox must preserve its workspace first. Unlike the
// best-effort idle flush, any error here prevents destruction of the old box.
func preserveSkillSwitchWorkspace(ctx context.Context, ex Executor, ws workspace.Store, agentID, projectID, sessionID string) error {
	if _, remote := ex.(RemoteWorkspace); !remote {
		return nil
	}
	snapper, ok := ex.(WorkspaceSnapshotter)
	if !ok || ws == nil {
		return fmt.Errorf("cannot switch skill version without a durable workspace")
	}
	files, err := snapper.SnapshotWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("snapshot before skill switch: %w", err)
	}
	for path, data := range files {
		if err := ws.Put(ctx, agentID, projectID, sessionID, path, bytes.NewReader(data), int64(len(data)), ""); err != nil {
			return fmt.Errorf("preserve workspace before skill switch: %w", err)
		}
	}
	return nil
}
