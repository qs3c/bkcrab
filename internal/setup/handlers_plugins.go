package setup

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bkclaw-ai/bkclaw/internal/config"
)

// --- Plugins ---

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	cfg, _ := s.loadUserConfig(r)
	pluginsDir := filepath.Join(homeDir, "plugins")
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	var plugins []map[string]any
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()

		// Read plugin.json for metadata
		pluginType := "unknown"
		version := ""
		manifestPath := filepath.Join(pluginsDir, id, "plugin.json")
		if data, readErr := os.ReadFile(manifestPath); readErr == nil {
			var manifest map[string]any
			if json.Unmarshal(data, &manifest) == nil {
				if t, ok := manifest["type"].(string); ok {
					pluginType = t
				}
				if v, ok := manifest["version"].(string); ok {
					version = v
				}
			}
		}

		enabled := false
		if cfg != nil && cfg.Plugins.Entries != nil {
			if pe, ok := cfg.Plugins.Entries[id]; ok {
				enabled = pe.Enabled
			}
		}

		status := "stopped"
		if enabled {
			status = "running"
		}

		plugins = append(plugins, map[string]any{
			"id":      id,
			"type":    pluginType,
			"version": version,
			"status":  status,
			"enabled": enabled,
		})
	}
	if plugins == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, plugins)
}

// handleListHookPlugins returns the discoverable hook-type plugins
// for use in per-agent plugin toggles on the Context page. Read-only,
// not admin-gated (agent owners need to see the available plugins to
// pick which to enable on their agents) — it deliberately leaves out
// the per-plugin runtime state (running/stopped) the admin /api/plugins
// endpoint exposes.
func (s *Server) handleListHookPlugins(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	pluginsDir := filepath.Join(homeDir, "plugins")
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	var out []map[string]any
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		manifestPath := filepath.Join(pluginsDir, id, "plugin.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest map[string]any
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		// Filter on either Type=="hook" OR capabilities containing "hook".
		// Older plugins use Type alone; newer ones may declare multiple
		// capabilities (e.g. a plugin that's both a tool AND a hook).
		isHook := false
		if t, ok := manifest["type"].(string); ok && t == "hook" {
			isHook = true
		}
		if caps, ok := manifest["capabilities"].([]any); ok && !isHook {
			for _, c := range caps {
				if s, ok := c.(string); ok && s == "hook" {
					isHook = true
					break
				}
			}
		}
		if !isHook {
			continue
		}
		out = append(out, map[string]any{
			"id":          id,
			"name":        manifest["name"],
			"description": manifest["description"],
			"version":     manifest["version"],
		})
	}
	if out == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s *Server) handleUpdatePlugin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled *bool                  `json:"enabled,omitempty"`
		Config  map[string]interface{} `json:"config,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	if cfg.Plugins.Entries == nil {
		cfg.Plugins.Entries = make(map[string]config.PluginEntryCfg)
	}
	entry := cfg.Plugins.Entries[id]
	if req.Enabled != nil {
		entry.Enabled = *req.Enabled
	}
	if req.Config != nil {
		entry.Config = req.Config
	}
	cfg.Plugins.Entries[id] = entry

	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
