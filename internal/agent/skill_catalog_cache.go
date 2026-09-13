package agent

import (
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Only publication-managed directories are cached. Their visible entries change
// by atomic rename, so directory mtime invalidates parsed metadata immediately.
// Arbitrary unmanaged filesystem skills retain their original discovery path.
type parsedSkillDirectory struct {
	modified time.Time
	checked  time.Time
	skills   map[string]Skill
}

var parsedSkillDirectories = struct {
	sync.Mutex
	entries map[string]parsedSkillDirectory
}{entries: make(map[string]parsedSkillDirectory)}

func discoverSkillsEnhanced(dir, layer string) map[string]Skill {
	if _, err := os.Stat(filepath.Join(dir, ".versions")); err != nil && filepath.Base(filepath.Dir(dir)) != ".views" {
		return discoverSkillsUncached(dir, layer)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return map[string]Skill{}
	}
	key := dir + "\x00" + layer
	parsedSkillDirectories.Lock()
	defer parsedSkillDirectories.Unlock()
	cached, ok := parsedSkillDirectories.entries[key]
	if !ok || !cached.modified.Equal(info.ModTime()) || time.Since(cached.checked) > 30*time.Second {
		cached = parsedSkillDirectory{info.ModTime(), time.Now(), discoverSkillsUncached(dir, layer)}
		if len(parsedSkillDirectories.entries) >= 256 {
			for k := range parsedSkillDirectories.entries {
				delete(parsedSkillDirectories.entries, k)
				break
			}
		}
		parsedSkillDirectories.entries[key] = cached
	}
	out := make(map[string]Skill, len(cached.skills))
	for name, s := range cached.skills {
		s.Gated, s.GateReason = checkGating(s.Metadata)
		out[name] = s
	}
	return out
}
