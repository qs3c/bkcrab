package setup

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/config"
)

// PluginsHandler 负责插件管理与 hook 插件发现。
type PluginsHandler struct {
	cfg *configRepo
	mw  *Middleware
}

// NewPluginsHandler 构造 PluginsHandler。
func NewPluginsHandler(cfg *configRepo, mw *Middleware) *PluginsHandler {
	return &PluginsHandler{cfg: cfg, mw: mw}
}

// RegisterRoutes 注册插件管理路由（多数仅超级管理员）。
func (s *PluginsHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/api/plugins", wrap(s.mw.Admin(s.handleListPlugins)))
	r.PUT("/api/plugins/:id", wrap(s.mw.Admin(s.handleUpdatePlugin)))
	// hook 插件元数据：agent 拥有者（非仅管理员）需要查看可启用的插件
	r.GET("/api/plugins/hook", wrap(s.mw.Auth(s.handleListHookPlugins)))
}

// --- 插件 ---

func (s *PluginsHandler) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	cfg, _ := s.cfg.loadUserConfig(r)
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

		// 读取 plugin.json 获取元数据
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

// handleListHookPlugins 返回可发现的 hook 类型插件，用于 Context 页面上的每个 agent 插件开关。
// 只读，不对管理员门控（agent 拥有者需要查看可用插件来选择在其 agent 上启用哪些）—
// 它故意省略了管理 /api/plugins 端点暴露的每个插件的运行时状态（running/stopped）。
func (s *PluginsHandler) handleListHookPlugins(w http.ResponseWriter, r *http.Request) {
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
		// 过滤条件为 Type=="hook" 或 capabilities 包含 "hook"。
		// 旧插件仅使用 Type；新插件可能声明多个 capabilities
		//（例如既是工具又是 hook 的插件）。
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

func (s *PluginsHandler) handleUpdatePlugin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled *bool                  `json:"enabled,omitempty"`
		Config  map[string]interface{} `json:"config,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	cfg, err := s.cfg.loadUserConfig(r)
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

	if err := s.cfg.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
