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
	legacyRoot := filepath.Join(agentDir, "skills")
	legacy := skills.NewManager(legacyRoot, skills.DefaultManagerConfig())
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return
		}
		if row.Origin != "learner" || row.Slug == "" || row.ContentHash == "" {
			continue
		}
		// Manager.Read performs canonical slug validation before any direct
		// filepath work below. If the old source is already gone, reconcile a
		// verified local learner copy upward (for local-only installations that
		// later gained object storage), then continue idempotently.
		if _, legacyExists := legacy.Read(row.Slug); !legacyExists {
			if existing, targetExists := target.Read(row.Slug); targetExists && store.HashSkillContent(existing) == row.ContentHash {
				if err := skills.SyncLearnerSkillUp(ctx, ws, agentID, row.Slug, target.RootDir()); err != nil {
					slog.Warn("learner reconciliation: remote sync failed", "agent", agentID, "skill", row.Slug, "error", err)
				}
			}
			continue
		}
		content, ok := verifiedLegacyLearnerSource(legacyRoot, row)
		if !ok {
			slog.Warn("learner migration: legacy source is ambiguous; keeping it in place", "agent", agentID, "skill", row.Slug)
			continue
		}

		createdTarget := false
		if existing, exists := target.Read(row.Slug); exists {
			if store.HashSkillContent(existing) != row.ContentHash {
				slog.Warn("learner migration conflict: target differs; keeping legacy source", "agent", agentID, "skill", row.Slug)
				continue
			}
		} else if err := target.Create(row.Slug, content); err != nil {
			slog.Warn("learner migration: create target failed", "agent", agentID, "skill", row.Slug, "error", err)
			continue
		} else {
			createdTarget = true
		}

		if err := skills.SyncLearnerSkillUp(ctx, ws, agentID, row.Slug, target.RootDir()); err != nil {
			// If this invocation created the target, keeping it would split local
			// and remote authority. Remove it and leave the verified source intact.
			if createdTarget {
				_ = target.Delete(row.Slug)
				_ = skills.DeleteLearnerSkillUp(ctx, ws, agentID, row.Slug)
			}
			slog.Warn("learner migration: remote sync failed; keeping legacy source", "agent", agentID, "skill", row.Slug, "error", err)
			continue
		}
		if err := legacy.Delete(row.Slug); err != nil {
			slog.Warn("learner migration: source delete failed; both copies retained", "agent", agentID, "skill", row.Slug, "error", err)
			continue
		}
		slog.Info("migrated legacy learner skill", "agent", agentID, "skill", row.Slug)
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
