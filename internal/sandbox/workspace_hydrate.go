package sandbox

import (
	"context"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// hydrateWorkspace copies every object from the workspace Store (S3 / local
// FS / whatever) into the sandbox's /workspace directory. Called once per
// sandbox creation so that when an agent's first `exec` runs, the sandbox
// already contains the files written via `write_file` in past sessions.
//
// Implementation is single-threaded and best-effort — any per-file error is
// logged and skipped rather than failing the whole hydrate. The typical
// workspace is a handful of files (MB-range PDFs, audio clips); for larger
// setups consider paginated / parallel copy, or E2B's snapshot API.
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
		// Executor.WriteFile accepts the full sandbox path; all current
		// implementations (docker/e2b) handle mkdir implicitly.
		if _, wErr := ex.WriteFile(ctx, target, string(content)); wErr != nil {
			slog.Warn("workspace hydrate: sandbox write failed", "agent", agentID, "project", projectID, "session", sessionID, "path", target, "error", wErr)
			continue
		}
		copied++
	}
	slog.Info("workspace hydrated into sandbox", "agent", agentID, "project", projectID, "session", sessionID, "files", copied, "root", sandboxRoot)
}

// defaultSandboxRoot is where hydrated files land inside the sandbox. Kept
// as a constant so we don't have to thread config through two packages just
// for a single path. E2B and our Docker sandboxes both mount their working
// dir at /workspace by convention.
const defaultSandboxRoot = "/workspace"

// sanitizeSandboxPath strips leading slashes / `..` segments so hydrated
// keys can't escape /workspace even if the store somehow holds a malicious
// path. Mirror of internal/workspace.LocalFS.resolvePath's logic.
func sanitizeSandboxPath(p string) string {
	clean := path.Clean("/" + p)
	return strings.TrimPrefix(clean, "/")
}
