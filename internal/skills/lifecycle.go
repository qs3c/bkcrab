package skills

import (
	"math"
	"sort"

	"github.com/qs3c/bkcrab/internal/store"
)

const (
	DefaultLifecycleActiveMax = 10
	// 引用 store 的单一真相源,与 DecayFactor 的默认保持一致、杜绝漂移。
	DefaultLifecycleHalfLifeLoads    = store.DefaultHalfLifeLoads
	DefaultLifecycleProtectLoads     = 20
	DefaultLifecycleEditProtectLoads = 30
	DefaultLifecycleDeleteAfterLoads = 200
	DefaultLifecycleExplicitGain     = 3
)

type LifecycleConfig struct {
	ActiveMax        int
	HalfLifeLoads    int
	ProtectLoads     int
	EditProtectLoads int
	DeleteAfterLoads int
}

func normalizeLifecycleConfig(c LifecycleConfig) LifecycleConfig {
	if c.ActiveMax <= 0 {
		c.ActiveMax = DefaultLifecycleActiveMax
	}
	if c.HalfLifeLoads <= 0 {
		c.HalfLifeLoads = DefaultLifecycleHalfLifeLoads
	}
	if c.ProtectLoads <= 0 {
		c.ProtectLoads = DefaultLifecycleProtectLoads
	}
	if c.EditProtectLoads <= 0 {
		c.EditProtectLoads = DefaultLifecycleEditProtectLoads
	}
	if c.DeleteAfterLoads <= 0 {
		c.DeleteAfterLoads = DefaultLifecycleDeleteAfterLoads
	}
	return c
}

// NowSeq returns the agent-local skill lifecycle clock.
func NowSeq(rows []store.SkillUsageRow) int64 {
	var max int64
	for _, r := range rows {
		if r.LastLoadSeq > max {
			max = r.LastLoadSeq
		}
	}
	return max
}

// Rank derives the active catalog set and deletion candidates from ledger rows.
func Rank(rows []store.SkillUsageRow, nowSeq int64, cfg LifecycleConfig) (map[string]bool, []string) {
	cfg = normalizeLifecycleConfig(cfg)
	active := make(map[string]bool)
	type candidate struct {
		slug  string
		score float64
	}
	candidates := make([]candidate, 0, len(rows))
	var deletable []string

	for _, r := range rows {
		if r.Slug == "" {
			continue
		}
		createdAge := loadAge(nowSeq, r.CreatedSeq)
		if r.Origin == "learner" && r.EditedSeq == 0 && r.TotalLoads == 0 && createdAge > int64(cfg.DeleteAfterLoads) {
			deletable = append(deletable, r.Slug)
			continue
		}
		if r.EditedSeq > 0 && loadAge(nowSeq, r.EditedSeq) < int64(cfg.EditProtectLoads) {
			active[r.Slug] = true
			continue
		}
		if createdAge < int64(cfg.ProtectLoads) {
			active[r.Slug] = true
			continue
		}
		candidates = append(candidates, candidate{slug: r.Slug, score: effectiveActivity(r, nowSeq, cfg.HalfLifeLoads)})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].slug < candidates[j].slug
		}
		return candidates[i].score > candidates[j].score
	})
	for i, c := range candidates {
		if i >= cfg.ActiveMax {
			break
		}
		active[c.slug] = true
	}
	sort.Strings(deletable)
	return active, deletable
}

func loadAge(nowSeq, seq int64) int64 {
	if nowSeq <= seq {
		return 0
	}
	return nowSeq - seq
}

func effectiveActivity(row store.SkillUsageRow, nowSeq int64, halfLifeLoads int) float64 {
	score := row.Activity * store.DecayFactor(loadAge(nowSeq, row.LastLoadSeq), halfLifeLoads)
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return score
}
