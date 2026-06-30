// Package modelspec provides an embedded slim model specification catalog.
//
// Runtime lookups are offline only. Refreshing the snapshot is handled by the
// generator under ./gen during development.
package modelspec

//go:generate go run ./gen -out catalog.json

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed catalog.json
var embedded []byte

// Spec is the compact runtime view of a model's limits.
type Spec struct {
	ContextWindow   int
	MaxOutputTokens int
}

// Entry is one catalog row. The same model id may appear for multiple hosts.
type Entry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Output  int    `json:"output"`
	APIHost string `json:"apiHost"`
}

type catalogFile struct {
	Entries []Entry `json:"entries"`
}

// Catalog indexes catalog entries by normalized model id and display name.
type Catalog struct {
	byID   map[string][]Entry
	byName map[string][]Entry
}

// Load parses catalog JSON and builds lookup indexes.
func Load(data []byte) (*Catalog, error) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}

	c := &Catalog{
		byID:   map[string][]Entry{},
		byName: map[string][]Entry{},
	}
	for _, e := range f.Entries {
		e.APIHost = normalize(e.APIHost)
		if id := normalize(e.ID); id != "" {
			c.byID[id] = append(c.byID[id], e)
		}
		if name := normalize(e.Name); name != "" {
			c.byName[name] = append(c.byName[name], e)
		}
	}
	return c, nil
}

// Lookup resolves modelID against the catalog. apiHost prefers a matching
// provider host; otherwise the most conservative non-zero limits are returned.
func (c *Catalog) Lookup(modelID, apiHost string) (Spec, bool) {
	if c == nil {
		return Spec{}, false
	}
	key := normalize(modelID)
	if key == "" {
		return Spec{}, false
	}

	cands := c.byID[key]
	if len(cands) == 0 {
		cands = c.byName[key]
	}
	if len(cands) == 0 {
		return Spec{}, false
	}

	if host := normalize(apiHost); host != "" {
		matched := make([]Entry, 0, len(cands))
		for _, e := range cands {
			if e.APIHost == host {
				matched = append(matched, e)
			}
		}
		if len(matched) > 0 {
			cands = matched
		}
	}

	var spec Spec
	for _, e := range cands {
		if e.Context > 0 && (spec.ContextWindow == 0 || e.Context < spec.ContextWindow) {
			spec.ContextWindow = e.Context
		}
		if e.Output > 0 && (spec.MaxOutputTokens == 0 || e.Output < spec.MaxOutputTokens) {
			spec.MaxOutputTokens = e.Output
		}
	}
	if spec.ContextWindow == 0 && spec.MaxOutputTokens == 0 {
		return Spec{}, false
	}
	return spec, true
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

var defaultCatalog = sync.OnceValue(func() *Catalog {
	c, err := Load(embedded)
	if err != nil {
		return nil
	}
	return c
})

// Lookup resolves modelID against the embedded catalog.
func Lookup(modelID, apiHost string) (Spec, bool) {
	return defaultCatalog().Lookup(modelID, apiHost)
}
