package tools

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkclaw/internal/buildinfo"
)

// RouteTarget identifies which backend should handle a file/exec call.
// Centralised here so every tool dispatches via the same decision instead
// of each handler open-coding its own if-else ladder.
type RouteTarget int

const (
	// RouteSandbox dispatches via the registered sandbox.Executor
	// (ex.ReadFile / ex.WriteFile / ex.Exec). Default in cloud and in
	// local-with-sandbox modes — the sandbox is where the agent's
	// "current working environment" lives, so writes there are reachable
	// by subsequent execs in the same session.
	RouteSandbox RouteTarget = iota

	// RouteWorkspaceStore dispatches to the durable workspace.Store under
	// (agentID, projectID, sessionID). Used for relative paths like
	// `report.md` that the chatter expects to outlive the sandbox
	// container and be visible in the UI's file browser.
	RouteWorkspaceStore

	// RouteSystemStore dispatches to the identity-files store
	// (SOUL.md / IDENTITY.md / MEMORY.md / …). DB-backed in cloud mode
	// so the admin UI sees the same content across pods.
	RouteSystemStore

	// RouteSkillStore writes the file under the per-user skills bucket
	// (`skills/<name>/...`) on host disk so SkillsLoader picks it up.
	// Only relevant for skill-creator scaffolding.
	RouteSkillStore

	// RouteHostFS dispatches to raw os.* on the operator's host. Only
	// reachable in local mode AND when the path is an explicit host
	// reference (~/Documents/, /Users/<u>/projects/, …) — never for
	// vague absolute paths like /tmp/foo, which would otherwise leak
	// sandbox-internal scratch onto the operator's machine.
	RouteHostFS

	// RouteRefuseSuggestSandbox is policy: the op needs sandbox to run
	// safely but no sandbox is configured. Caller surfaces a message
	// telling the user to enable Settings → Runtime → Sandbox.
	RouteRefuseSuggestSandbox
)

// Operation tags the kind of work being routed so policy can apply
// different treatment per op (e.g. exec is dangerous on host even when
// reads from the same path are fine).
type Operation int

const (
	OpRead Operation = iota
	OpWrite
	OpList
	OpExec
)

// routeFor decides which backend handles (path, op) under the current
// deployment mode + sandbox availability. The three high-level rules
// (cloud → sandbox; local+sandbox → sandbox-first; local-no-sandbox →
// host-with-care) live here, so every tool that needs to dispatch a
// path-like arg should call this rather than inventing its own
// classification.
func (r *Registry) routeFor(path string, op Operation) RouteTarget {
	// Stored-artifact patterns always route to their dedicated store
	// regardless of deployment mode — these are persistence concerns,
	// not "where does this command run" concerns.
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		return RouteWorkspaceStore
	}
	if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(path) {
		return RouteSystemStore
	}
	if r.isSkillPath(path) && r.skillRoot() != "" {
		return RouteSkillStore
	}

	sandboxOK := r.executor != nil

	// Rule 1: cloud / hosted deploy → sandbox is mandatory for everything
	// that doesn't fall under the stored-artifact routes above. If the
	// operator forgot to configure one, refuse rather than silently
	// running on the pod's host filesystem.
	if buildinfo.IsHostedDeploy() {
		if sandboxOK {
			return RouteSandbox
		}
		return RouteRefuseSuggestSandbox
	}

	// Rule 2: local with sandbox configured → sandbox-first. Host disk is
	// reachable only via explicit host-scope paths (the operator's
	// Documents, an absolute /Users/<u>/... that's clearly NOT
	// sandbox-internal, bkclaw-internal subtrees for upgrade ops).
	if sandboxOK {
		if isBkClawInternalPath(path) {
			return RouteHostFS
		}
		if isExplicitHostScope(path) {
			return RouteHostFS
		}
		return RouteSandbox
	}

	// Rule 3: local without sandbox → host disk is the only choice, but
	// flag dangerous ops so the caller can suggest enabling sandbox.
	if isDangerousOnHost(op, path) {
		return RouteRefuseSuggestSandbox
	}
	return RouteHostFS
}

// isExplicitHostScope reports whether path is one the chatter clearly
// meant as a reference to the operator's actual host filesystem (not the
// sandbox container's view of the same string). Used in local+sandbox
// mode to allow read/write of the operator's real files while keeping
// vague absolute paths (e.g. /tmp/scratch.js) routed into the sandbox.
//
// Today the heuristic is path-prefix based:
//   - ~/Documents, ~/Downloads, ~/Desktop, ~/projects, ~/code, ~/work
//   - /Users/<u>/... and /home/<u>/... that aren't bkclaw-internal
//     and aren't sandbox-only
//
// Bare ~/ (without a recognised user-content subdir) is NOT host-scope:
// "~/foo" is more likely sandbox-relative scratch than the operator's
// home dir. If the operator wants a path under their actual home, they
// can spell it out (~/Documents/foo, /Users/mike/code/foo).
func isExplicitHostScope(path string) bool {
	if isBkClawInternalPath(path) || isSandboxOnlyPath(path) {
		return false
	}
	if strings.HasPrefix(path, "~/") {
		rest := path[2:]
		for _, prefix := range hostHomeContentDirs {
			if rest == prefix || strings.HasPrefix(rest, prefix+"/") {
				return true
			}
		}
		return false
	}
	if !filepath.IsAbs(path) {
		return false
	}
	if strings.HasPrefix(path, "/Users/") || strings.HasPrefix(path, "/home/") {
		return true
	}
	return false
}

// hostHomeContentDirs is the conservative whitelist of "this clearly
// means the chatter's actual host file" subdirs under ~. Conservative
// because anything not on this list defaults to sandbox in P2 mode,
// which is the right call when in doubt — sandbox is recoverable, host
// disk writes aren't.
var hostHomeContentDirs = []string{
	"Documents", "Downloads", "Desktop",
	"projects", "code", "work", "src",
}

// isBkClawInternalPath reports whether path falls under BkClaw's
// runtime-managed dirs (~/.bkclaw/...). These have dedicated routing
// (workspaceStore, identity store, …) and tools must not write to them
// through the chat-facing host path or they'd corrupt internal state.
func isBkClawInternalPath(path string) bool {
	if strings.HasPrefix(path, "~/.bkclaw") {
		return true
	}
	if filepath.IsAbs(path) {
		if home, err := os.UserHomeDir(); err == nil {
			bkclawDir := filepath.Join(home, ".bkclaw")
			if path == bkclawDir || strings.HasPrefix(path, bkclawDir+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// isSandboxOnlyPath reports whether path only exists inside the sandbox
// container — typically a bind-mount target. Host has the bind-source at
// a different location, so naive host expansion would always 404.
//
//   - ~/.agents/...    : npx skills' install dir (bind-mounted from
//     ~/.bkclaw/users/<uid>/skills/)
//   - /root/.agents/.. : same, via the sandbox-resolved absolute path
func isSandboxOnlyPath(path string) bool {
	if strings.HasPrefix(path, "~/.agents") || strings.HasPrefix(path, "/root/.agents") {
		return true
	}
	return false
}

// isDangerousOnHost classifies operations that the local-no-sandbox
// branch should refuse rather than silently doing on the operator's
// real machine. Conservative: a true means "ask the user to turn on
// sandbox before doing this." Returning false doesn't mean the op is
// safe, just that we're willing to delegate to host filesystem with
// the existing path-containment guards.
func isDangerousOnHost(op Operation, path string) bool {
	if op != OpExec && op != OpWrite {
		return false
	}
	// Writes to system locations are definitely sandbox territory.
	if filepath.IsAbs(path) {
		for _, p := range []string{"/etc", "/usr", "/var", "/bin", "/sbin", "/opt", "/root"} {
			if path == p || strings.HasPrefix(path, p+"/") {
				return true
			}
		}
	}
	return false
}

// errSandboxRequiredMessage is the user-facing string emitted when
// routeFor returns RouteRefuseSuggestSandbox. Phrased so the chatter
// knows the op was withheld for safety, not denied for capability.
const errSandboxRequiredMessage = "This operation needs a sandbox to run safely, but none is configured. Ask the operator to enable Settings → Runtime → Sandbox, then retry."
