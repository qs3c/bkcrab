package skills

import (
	"math"
	"sort"

	"github.com/qs3c/bkcrab/internal/store"
)

const (
	DefaultLifecycleActiveMax = 10
	DefaultLifecycleAssetMax  = 50
	// 引用 store 的单一真相源,与 DecayFactor 的默认保持一致、杜绝漂移。
	DefaultLifecycleHalfLifeLoads     = store.DefaultHalfLifeLoads
	DefaultLifecycleProtectLoads      = 20
	DefaultLifecycleEditProtectLoads  = 30
	DefaultLifecycleDeleteAfterLoads  = 200
	DefaultLifecycleExplicitGain      = 3
	DefaultLifecycleCleanupEveryTurns = 20
)

type LifecycleConfig struct {
	ActiveMax        int
	AssetMax         int
	HalfLifeLoads    int
	ProtectLoads     int
	EditProtectLoads int
	DeleteAfterLoads int
}

func normalizeLifecycleConfig(c LifecycleConfig) LifecycleConfig {
	if c.ActiveMax <= 0 {
		c.ActiveMax = DefaultLifecycleActiveMax
	}
	if c.AssetMax <= 0 {
		c.AssetMax = DefaultLifecycleAssetMax
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
// It is kept as the ledger-only compatibility entry point. Call RankCatalog when
// the caller has the complete learner catalog so skills without a ledger row also
// consume capacity.
func Rank(rows []store.SkillUsageRow, nowSeq int64, cfg LifecycleConfig) (map[string]bool, []string) {
	catalog := make([]string, 0, len(rows))
	for _, row := range rows {
		catalog = append(catalog, row.Slug)
	}
	return RankCatalog(rows, catalog, nowSeq, cfg)
}

type lifecycleRankTier uint8

const (
	rankEditedProtected lifecycleRankTier = iota
	rankCreatedProtected
	rankMature
	rankUntracked
)

type lifecycleRankCandidate struct {
	slug      string
	tier      lifecycleRankTier
	freshness int64
	score     float64
	lastLoad  int64
	created   int64
}

// RankCatalog ranks the complete learner catalog under one absolute ActiveMax
// limit. Every catalog entry consumes capacity, including edit-protected,
// creation-protected, and untracked entries. The deterministic priority is:
// edit protection, creation protection, mature activity, then no ledger row.
// AssetMax separately bounds persisted learner assets: capacity eviction only
// selects mature catalog entries that already have lifecycle ledger rows.
//
// Deletion candidates are always derived from all ledger rows, independent of
// catalogSlugs. This lets callers pass a filtered or temporarily stale catalog
// without suppressing lifecycle cleanup.
func RankCatalog(rows []store.SkillUsageRow, catalogSlugs []string, nowSeq int64, cfg LifecycleConfig) (map[string]bool, []string) {
	cfg = normalizeLifecycleConfig(cfg)

	bestBySlug := make(map[string]lifecycleRankCandidate, len(rows))
	hasLiveRow := make(map[string]bool, len(rows))
	hasDeletableRow := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.Slug == "" {
			continue
		}
		if lifecycleRowDeletable(row, nowSeq, cfg) {
			hasDeletableRow[row.Slug] = true
			continue
		}

		hasLiveRow[row.Slug] = true
		candidate := lifecycleCandidate(row, nowSeq, cfg)
		if current, ok := bestBySlug[row.Slug]; !ok || lifecycleCandidateLess(candidate, current) {
			bestBySlug[row.Slug] = candidate
		}
	}

	deletableSet := make(map[string]bool, len(hasDeletableRow))
	for slug := range hasDeletableRow {
		// The ledger has a uniqueness constraint, but treating a slug as live if
		// any duplicate row is live makes this helper deterministic and safe for
		// synthetic/imported input too.
		if !hasLiveRow[slug] {
			deletableSet[slug] = true
		}
	}

	seen := make(map[string]bool, len(catalogSlugs))
	candidates := make([]lifecycleRankCandidate, 0, len(catalogSlugs))
	trackedAssetCount := 0
	capacityCandidates := make([]lifecycleRankCandidate, 0, len(catalogSlugs))
	for _, slug := range catalogSlugs {
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		if hasDeletableRow[slug] && !hasLiveRow[slug] {
			continue
		}
		if candidate, ok := bestBySlug[slug]; ok {
			candidates = append(candidates, candidate)
			trackedAssetCount++
			if candidate.tier == rankMature {
				capacityCandidates = append(capacityCandidates, candidate)
			}
			continue
		}
		candidates = append(candidates, lifecycleRankCandidate{slug: slug, tier: rankUntracked})
	}

	// AssetMax bounds learner assets that are present in both the catalog and
	// lifecycle ledger. Untracked catalog entries are deliberately not deleted:
	// the caller must first reconcile them into the ledger. Protected entries may
	// temporarily keep the asset store above the limit; only mature entries are
	// eligible for capacity eviction.
	if overflow := trackedAssetCount - cfg.AssetMax; overflow > 0 {
		sort.Slice(capacityCandidates, func(i, j int) bool {
			return lifecycleEvictionLess(capacityCandidates[i], capacityCandidates[j])
		})
		if overflow > len(capacityCandidates) {
			overflow = len(capacityCandidates)
		}
		for _, candidate := range capacityCandidates[:overflow] {
			deletableSet[candidate.slug] = true
		}
	}

	if len(deletableSet) > 0 {
		kept := candidates[:0]
		for _, candidate := range candidates {
			if !deletableSet[candidate.slug] {
				kept = append(kept, candidate)
			}
		}
		candidates = kept
	}

	sort.Slice(candidates, func(i, j int) bool {
		return lifecycleCandidateLess(candidates[i], candidates[j])
	})
	if len(candidates) > cfg.ActiveMax {
		candidates = candidates[:cfg.ActiveMax]
	}
	active := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		active[candidate.slug] = true
	}
	deletable := make([]string, 0, len(deletableSet))
	for slug := range deletableSet {
		deletable = append(deletable, slug)
	}
	sort.Strings(deletable)
	return active, deletable
}

func lifecycleRowDeletable(row store.SkillUsageRow, nowSeq int64, cfg LifecycleConfig) bool {
	return row.Origin == "learner" &&
		row.EditedSeq == 0 &&
		row.TotalLoads == 0 &&
		loadAge(nowSeq, row.CreatedSeq) > int64(cfg.DeleteAfterLoads)
}

func lifecycleCandidate(row store.SkillUsageRow, nowSeq int64, cfg LifecycleConfig) lifecycleRankCandidate {
	candidate := lifecycleRankCandidate{
		slug:     row.Slug,
		score:    effectiveActivity(row, nowSeq, cfg.HalfLifeLoads),
		lastLoad: row.LastLoadSeq,
		created:  row.CreatedSeq,
	}
	switch {
	case row.EditedSeq > 0 && loadAge(nowSeq, row.EditedSeq) < int64(cfg.EditProtectLoads):
		candidate.tier = rankEditedProtected
		candidate.freshness = row.EditedSeq
	case loadAge(nowSeq, row.CreatedSeq) < int64(cfg.ProtectLoads):
		candidate.tier = rankCreatedProtected
		candidate.freshness = row.CreatedSeq
	default:
		candidate.tier = rankMature
	}
	return candidate
}

// lifecycleEvictionLess orders mature assets from the best eviction candidate
// to the worst: least effective activity, oldest load (or creation when never
// loaded), oldest creation, then slug for a deterministic final tie-break.
func lifecycleEvictionLess(left, right lifecycleRankCandidate) bool {
	if left.score != right.score {
		return left.score < right.score
	}
	leftRecency := left.lastLoad
	if leftRecency == 0 {
		leftRecency = left.created
	}
	rightRecency := right.lastLoad
	if rightRecency == 0 {
		rightRecency = right.created
	}
	if leftRecency != rightRecency {
		return leftRecency < rightRecency
	}
	if left.created != right.created {
		return left.created < right.created
	}
	return left.slug < right.slug
}

func lifecycleCandidateLess(left, right lifecycleRankCandidate) bool {
	if left.tier != right.tier {
		return left.tier < right.tier
	}
	if left.freshness != right.freshness {
		return left.freshness > right.freshness
	}
	if left.score != right.score {
		return left.score > right.score
	}
	if left.lastLoad != right.lastLoad {
		return left.lastLoad > right.lastLoad
	}
	return left.slug < right.slug
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
