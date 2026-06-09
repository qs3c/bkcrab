package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bkclaw-ai/bkclaw/internal/workspace"
)

// Skill bundles are static file trees that need to live on the local disk
// for SkillsLoader to discover them, but in multi-pod deployments the
// disk is pod-local. To make installs consistent across replicas we mirror
// every installed skill into the shared object store and hydrate it back
// onto each pod's disk on startup / reload.
//
// Key layout under the workspace bucket:
//
//	<owner>/skills/<skillName>/<relFile>
//
// Owner is the agent ID for per-agent skills, or GlobalSkillOwner for the
// platform-wide directory (`~/.bkclaw/skills/`).
const (
	// GlobalSkillOwner is the synthetic "agent ID" used as the prefix for
	// globally-installed skills in the object store. A real agent can
	// never collide with it because agent names are validated to be lower
	// alphanumeric + hyphens (see setup/handlers_agents.go:agentNameRE),
	// so the leading underscore keeps this namespace separate.
	GlobalSkillOwner = "_global"

	// skillsKeyPrefix scopes every skill object under the owner. Every
	// caller must go through buildKey so this stays consistent.
	skillsKeyPrefix = "skills"

	// userSkillOwnerPrefix is the leading marker for per-user pseudo
	// owner keys (`_user_<uid>`). Same leading-underscore convention as
	// GlobalSkillOwner so it can never collide with a real agent ID.
	userSkillOwnerPrefix = "_user_"
)

// UserSkillOwner returns the workspace.Store pseudo-owner key for a
// chatter's per-user skills. Empty userID returns "" so callers can
// short-circuit on legacy / single-user installs.
func UserSkillOwner(userID string) string {
	if userID == "" {
		return ""
	}
	return userSkillOwnerPrefix + userID
}

func buildKey(skillName, relPath string) string {
	// relPath is already normalised (filepath.Walk yields forward-slash-
	// equivalents via filepath.ToSlash when we pass through this helper).
	rel := strings.TrimPrefix(filepath.ToSlash(relPath), "/")
	return skillsKeyPrefix + "/" + skillName + "/" + rel
}

// SyncSkillUp uploads every file under <rootDir>/<skillName>/ to the
// object store under <owner>/skills/<skillName>/. Symlinks are followed
// (os.Lstat filter excludes them to avoid duplicating targets). Safe to
// call after each install; existing keys are overwritten.
func SyncSkillUp(ctx context.Context, ws workspace.Store, owner, skillName, rootDir string) error {
	if ws == nil {
		return nil // no object store configured — nothing to mirror
	}
	skillDir := filepath.Join(rootDir, skillName)
	info, err := os.Stat(skillDir)
	if err != nil {
		return fmt.Errorf("stat skill dir %s: %w", skillDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skill path %s is not a directory", skillDir)
	}

	uploaded := 0
	walkErr := filepath.Walk(skillDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // skip symlinks
		}
		rel, relErr := filepath.Rel(skillDir, path)
		if relErr != nil {
			return relErr
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer f.Close()

		key := buildKey(skillName, rel)
		// Skills live in the agent-shared scope (project + session both
		// empty) so every chat of an agent sees the same set; per-scope
		// subtrees are reserved for chat artifacts.
		if putErr := ws.Put(ctx, owner, "", "", key, f, info.Size(), ""); putErr != nil {
			return fmt.Errorf("put %s: %w", key, putErr)
		}
		uploaded++
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	slog.Info("skill mirrored to object store",
		"owner", owner, "skill", skillName, "files", uploaded)
	return nil
}

// MirrorSkillsUp uploads any local skill subdir under rootDir whose name
// is absent from the object store under owner. Pairs with
// HydrateSkillsDown: hydrate-down brings remote→local at LoadSkills
// entry, mirror-up catches anything the agent just wrote locally
// (typically `npx skills add -g -y` into the per-user bind-mount) and
// pushes it so sibling pods see it on their next hydrate cycle.
//
// Skip-criterion is "skill name absent from remote", not per-file diff,
// so a skill that already exists on remote is treated as authoritative-
// remote (no re-upload, no overwrite). Half-installed dirs without
// SKILL.md are skipped to avoid uploading partial state mid-install.
func MirrorSkillsUp(ctx context.Context, ws workspace.Store, owner, rootDir string) error {
	if ws == nil || owner == "" {
		return nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return fmt.Errorf("list object store skills for %s: %w", owner, err)
	}
	prefix := skillsKeyPrefix + "/"
	remoteSkills := make(map[string]bool)
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Path, prefix)
		if slash := strings.IndexByte(rest, '/'); slash > 0 {
			remoteSkills[rest[:slash]] = true
		}
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		// Missing local dir == nothing to mirror; not an error.
		return nil
	}
	uploaded := 0
	for _, e := range entries {
		if !e.IsDir() || remoteSkills[e.Name()] {
			continue
		}
		// Only upload dirs that look fully installed. Without this guard
		// a concurrent npx install could push a half-written tree.
		if _, statErr := os.Stat(filepath.Join(rootDir, e.Name(), "SKILL.md")); statErr != nil {
			continue
		}
		if upErr := SyncSkillUp(ctx, ws, owner, e.Name(), rootDir); upErr != nil {
			slog.Warn("mirror skill up failed", "owner", owner, "skill", e.Name(), "error", upErr)
			continue
		}
		uploaded++
	}
	if uploaded > 0 {
		slog.Info("mirrored new local skills to object store", "owner", owner, "count", uploaded)
	}
	return nil
}

// DeleteSkillUp removes every object under <owner>/skills/<skillName>/.
// Missing keys are tolerated.
func DeleteSkillUp(ctx context.Context, ws workspace.Store, owner, skillName string) error {
	if ws == nil {
		return nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return fmt.Errorf("list skills for %s: %w", owner, err)
	}
	prefix := skillsKeyPrefix + "/" + skillName + "/"
	removed := 0
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		if err := ws.Delete(ctx, owner, "", "", o.Path); err != nil {
			if errors.Is(err, workspace.ErrNotFound) {
				continue
			}
			return fmt.Errorf("delete %s: %w", o.Path, err)
		}
		removed++
	}
	slog.Info("skill removed from object store",
		"owner", owner, "skill", skillName, "files", removed)
	return nil
}

// HydrateSkillsDown mirrors every skill object under <owner>/skills/
// into <rootDir>/ so SkillsLoader (filesystem scanner) sees the same set
// of skills the object store has.
//
// Two-way reconciliation:
//  1. For every remote key, create/overwrite the local file (skipped when
//     size matches — cheap re-entry guard).
//  2. For every local top-level skill directory that has no remote keys,
//     remove it. This is what propagates a delete from pod A to pod B.
//
// `keepLocal` is an allow-list of skill folder names that must never be
// pruned regardless of the remote state. The global-skills dir uses this
// to protect bundled skills (installed from the embedded FS at startup,
// never uploaded to the object store). Pass nil for per-agent dirs.
//
// File-level divergence inside a surviving skill (remote dropped one file
// from the bundle) is not reconciled; skills are replaced wholesale at
// install time, so per-file drift shouldn't happen in practice.
func HydrateSkillsDown(ctx context.Context, ws workspace.Store, owner, rootDir string, keepLocal ...string) error {
	if ws == nil {
		return nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return fmt.Errorf("list object store skills for %s: %w", owner, err)
	}
	prefix := skillsKeyPrefix + "/"

	// Remote view: which skill-name directories exist in the store.
	remoteSkills := make(map[string]bool)
	fetched := 0
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Path, prefix)
		slash := strings.IndexByte(rest, '/')
		if slash > 0 {
			remoteSkills[rest[:slash]] = true
		}

		target := filepath.Join(rootDir, filepath.FromSlash(rest))
		if existing, statErr := os.Stat(target); statErr == nil && !existing.IsDir() {
			// Same size → already hydrated. We overwrite remote keys on
			// every install so size changes when content does; true
			// checksum match would need an extra HEAD/ETag and is rarely
			// worth it for static skill bundles.
			if o.Size >= 0 && existing.Size() == o.Size {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		rc, err := ws.Get(ctx, owner, "", "", o.Path)
		if err != nil {
			if errors.Is(err, workspace.ErrNotFound) {
				continue
			}
			return fmt.Errorf("get %s: %w", o.Path, err)
		}
		f, err := os.Create(target)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create %s: %w", target, err)
		}
		if _, err := io.Copy(f, rc); err != nil {
			f.Close()
			rc.Close()
			return fmt.Errorf("copy %s: %w", target, err)
		}
		f.Close()
		rc.Close()
		fetched++
	}

	// Reconcile deletions: any top-level skill dir present locally but not
	// in the remote listing was deleted on another pod — drop it so this
	// pod's SkillsLoader stops returning a stale entry. `keepLocal` shields
	// bundled skills (embedded in the binary, never mirrored to OSS) from
	// getting nuked the first time an empty OSS listing comes back.
	//
	// SAFETY: when the remote has zero skill objects, the listing is
	// indistinguishable from "OSS misconfigured" or "fresh install with
	// only filesystem-installed skills". Pruning in that case is
	// destructive — it deletes every local skill the operator dropped
	// into BKCLAW_HOME/skills/ for product agents that don't use OSS
	// at all. Skip pruning entirely unless the remote authoritatively
	// has at least one skill, which is the only state where "missing
	// from remote" carries meaning.
	keep := make(map[string]bool, len(keepLocal))
	for _, name := range keepLocal {
		keep[name] = true
	}
	removed := 0
	if entries, err := os.ReadDir(rootDir); err == nil && len(remoteSkills) > 0 {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if remoteSkills[e.Name()] || keep[e.Name()] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(rootDir, e.Name())); err != nil {
				slog.Warn("failed to prune stale local skill",
					"owner", owner, "skill", e.Name(), "error", err)
				continue
			}
			removed++
		}
	}

	if fetched > 0 || removed > 0 {
		slog.Info("skills reconciled with object store",
			"owner", owner, "dir", rootDir, "fetched", fetched, "pruned", removed)
	}
	return nil
}

// ListRemoteSkillNames returns the unique skill folder names present in
// the object store under <owner>/skills/. Used so the admin UI can show
// all skills the agent owns even if this pod hasn't hydrated them yet.
func ListRemoteSkillNames(ctx context.Context, ws workspace.Store, owner string) ([]string, error) {
	if ws == nil {
		return nil, nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return nil, err
	}
	prefix := skillsKeyPrefix + "/"
	seen := make(map[string]bool)
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Path, prefix)
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			continue
		}
		seen[rest[:slash]] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out, nil
}
