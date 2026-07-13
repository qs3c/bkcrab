package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type learnerMigrationUsageStore interface {
	ListSkillUsage(ctx context.Context, agentID string) ([]store.SkillUsageRow, error)
}

var learnerMigrationLocks sync.Map

// migrateLegacyLearnerSkills moves only positively identified outputs from the
// pre-isolation layout (<agent>/skills) into <agent>/learner-skills. It is
// intentionally conservative: the ledger must say origin=learner, the stored
// content hash must match, and the old directory may contain only SKILL.md.
// Any ambiguity leaves the old asset untouched and visible as a manual skill.
//
// The old implementation never mirrored skill_manage writes into the ordinary
// object-store namespace, so this migration does not delete ambiguous
// <agent>/skills remote objects. It first syncs the new learner namespace and
// only then removes the verified local source. Re-running is safe.
func migrateLegacyLearnerSkills(ctx context.Context, usage learnerMigrationUsageStore, ws workspace.Store, agentID, agentDir string, target *skills.Manager) {
	if usage == nil || target == nil || agentID == "" || agentDir == "" {
		return
	}
	wantRoot := filepath.Clean(skills.LearnerSkillsDir(agentDir))
	if filepath.Clean(target.RootDir()) != wantRoot {
		slog.Warn("learner migration skipped: unexpected target root", "agent", agentID, "root", target.RootDir())
		return
	}

	abs, err := filepath.Abs(agentDir)
	if err != nil {
		abs = filepath.Clean(agentDir)
	}
	lockValue, _ := learnerMigrationLocks.LoadOrStore(abs, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	rows, err := usage.ListSkillUsage(ctx, agentID)
	if err != nil {
		slog.Warn("learner migration: list ledger failed", "agent", agentID, "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	// Hold the agent-wide lease for the whole migration batch and decide remote
	// authority exactly once under that lease. Syncing the first legacy skill
	// initializes the namespace marker; re-reading that marker per row would
	// incorrectly treat our own migration as a newer authoritative runtime and
	// delete every remaining legacy source without copying it.
	leaser, _ := usage.(skills.MutationLeaser)
	lease, err := skills.WaitForLearnerAgentLeaseGuard(ctx, leaser, agentID)
	if err != nil {
		slog.Warn("learner migration: agent mutation lease unavailable", "agent", agentID, "error", err)
		return
	}
	defer func() {
		if err := lease.Release(); err != nil {
			slog.Warn("learner migration: release agent mutation lease failed", "agent", agentID, "error", err)
		}
	}()
	ctx = lease.Context()

	remoteAuthoritative := false
	if ws != nil {
		initialized, err := skills.LearnerNamespaceInitialized(ctx, ws, agentID)
		if err != nil {
			// Failing closed is essential here: treating an unreadable namespace
			// as unused can resurrect an intentionally deleted learner skill.
			slog.Warn("learner migration: remote namespace state unavailable", "agent", agentID, "error", err)
			return
		}
		remoteAuthoritative = initialized
	}

	legacyRoot := filepath.Join(agentDir, "skills")
	legacy := skills.NewManager(legacyRoot, skills.DefaultManagerConfig())
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return
		}
		func(row store.SkillUsageRow) {
			if row.Origin != "learner" || row.Slug == "" || row.ContentHash == "" {
				return
			}
			unlock, err := skills.LockLearnerSkillOperation(target.RootDir(), row.Slug)
			if err != nil {
				slog.Warn("learner migration: local mutation lock unavailable", "agent", agentID, "skill", row.Slug, "error", err)
				return
			}
			defer unlock()

			if remoteAuthoritative {
				// The dedicated namespace has already been used by a newer runtime.
				// Its current contents (including an initialized empty set) win over
				// every pre-isolation Pod-local copy. Remove only a hash-verified
				// legacy learner; ambiguous/manual content remains untouched.
				if _, ok := verifiedLegacyLearnerSource(legacyRoot, row); !ok {
					return
				}
				if err := legacy.Delete(row.Slug); err != nil {
					slog.Warn("learner migration: stale source delete failed", "agent", agentID, "skill", row.Slug, "error", err)
					return
				}
				slog.Info("removed stale legacy learner after remote initialization", "agent", agentID, "skill", row.Slug)
				return
			}
			// Manager.Read performs canonical slug validation before any direct
			// filepath work below. If the old source is already gone, reconcile a
			// verified local learner copy upward (for local-only installations that
			// later gained object storage), then continue idempotently.
			if _, legacyExists := legacy.Read(row.Slug); !legacyExists {
				if existing, targetExists := target.Read(row.Slug); targetExists && store.HashSkillContent(existing) == row.ContentHash {
					if err := skills.SyncLearnerSkillContent(ctx, ws, agentID, row.Slug, existing); err != nil {
						slog.Warn("learner reconciliation: remote sync failed", "agent", agentID, "skill", row.Slug, "error", err)
					}
				}
				return
			}
			content, ok := verifiedLegacyLearnerSource(legacyRoot, row)
			if !ok {
				slog.Warn("learner migration: legacy source is ambiguous; keeping it in place", "agent", agentID, "skill", row.Slug)
				return
			}

			createdTarget := false
			if existing, exists := target.Read(row.Slug); exists {
				if store.HashSkillContent(existing) != row.ContentHash {
					slog.Warn("learner migration conflict: target differs; keeping legacy source", "agent", agentID, "skill", row.Slug)
					return
				}
			} else if err := target.Create(row.Slug, content); err != nil {
				slog.Warn("learner migration: create target failed", "agent", agentID, "skill", row.Slug, "error", err)
				return
			} else {
				createdTarget = true
			}

			if err := skills.SyncLearnerSkillContent(ctx, ws, agentID, row.Slug, content); err != nil {
				if createdTarget {
					_ = target.Delete(row.Slug)
					_ = skills.DeleteLearnerSkillUp(ctx, ws, agentID, row.Slug)
				}
				slog.Warn("learner migration: remote sync failed; keeping legacy source", "agent", agentID, "skill", row.Slug, "error", err)
				return
			}
			if err := legacy.Delete(row.Slug); err != nil {
				slog.Warn("learner migration: source delete failed; both copies retained", "agent", agentID, "skill", row.Slug, "error", err)
				return
			}
			slog.Info("migrated legacy learner skill", "agent", agentID, "skill", row.Slug)
		}(row)
	}
}

func verifiedLegacyLearnerSource(root string, row store.SkillUsageRow) (string, bool) {
	dir := filepath.Join(root, row.Slug)
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "SKILL.md" || entries[0].IsDir() {
		return "", false
	}
	if entries[0].Type()&os.ModeSymlink != 0 {
		return "", false
	}
	info, err := entries[0].Info()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", false
	}
	content := string(data)
	if store.HashSkillContent(content) != row.ContentHash {
		return "", false
	}
	return content, true
}
