package setup

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/skills"
)

// --- 技能 ---

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	skillsDir := filepath.Join(homeDir, "skills")
	// 首先从对象存储中补充技能，以便未处理原始安装的 pod 仍然能看到技能包。
	// 将捆绑技能名称作为保留本地列表传入，这样空的对象存储响应不会导致我们修剪内置技能。
	// 当未配置对象存储（本地模式）或尚未镜像任何内容时，此操作为空操作。
	if s.workspaceStore != nil {
		if err := skills.HydrateSkillsDown(
			r.Context(), s.workspaceStore, skills.GlobalSkillOwner, skillsDir,
			agent.BundledSkillNames()...,
		); err != nil {
			slog.Warn("failed to hydrate global skills from object store", "error", err)
		}
	}
	out := scanSkillsDir(skillsDir)
	if out == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s *Server) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	homeDir, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	skillPath := filepath.Join(homeDir, "skills", name)
	if err := os.RemoveAll(skillPath); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 同时从对象存储中删除，以便其他 pod 在下次补充时丢弃它。
	// 此处失败不应导致删除失败 — 本地副本已经消失，过期的远程副本只会在下次重载时重新出现（烦人但不危险）。
	if s.workspaceStore != nil {
		if derr := skills.DeleteSkillUp(r.Context(), s.workspaceStore, skills.GlobalSkillOwner, name); derr != nil {
			slog.Warn("failed to remove global skill from object store", "skill", name, "error", derr)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleListAgentSkills 列出安装到 agent 自身主目录（~/.bkclaw/agents/<id>/skills/）中的技能。
// 加载器"Layer 1"以最高优先级获取这些技能 — 它们专属于该 agent。
func (s *Server) handleListAgentSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 技能列表会暴露每个技能的 env spec（拥有者设置了哪些环境变量键）。
	// 仅限拥有者 — Identity.CanAccessAgent 对会话调用者延迟返回 true，
	// 会让任何已登录用户枚举任何 agent 的技能。
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	homePath, err := config.AgentHomeDir(id)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	skillsDir := filepath.Join(homePath, "skills")
	// 按需从对象存储中补充此 agent 的技能，以便尚未缓存该包的副本 pod 在 UI 中仍然能列出它。
	if s.workspaceStore != nil {
		if err := skills.HydrateSkillsDown(r.Context(), s.workspaceStore, id, skillsDir); err != nil {
			slog.Warn("failed to hydrate agent skills from object store",
				"agent", id, "error", err)
		}
	}
	out := scanSkillsDir(skillsDir)
	if out == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, out)
}

// scanSkillsDir 读取 dir 下的每个 SKILL.md，返回管理 UI 渲染的
// {name, description, location, type, envSpec?} 条目列表。
// 在全局 /api/skills 和按 agent 的 /api/agents/{id}/skills 路径之间共享，
// 以保持 frontmatter 解析（description, envSpec）的一致性 — 之前两个处理程序出现偏差，
// agent 作用域的那个回退到"第一个非 # 行"，然后导致字面 `---` frontmatter 分隔符被当作描述。
func scanSkillsDir(dir string) []map[string]any {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		skillPath := filepath.Join(dir, name, "SKILL.md")
		desc := ""
		var envSpec []agent.SkillEnvSpec
		if data, readErr := os.ReadFile(skillPath); readErr == nil {
			fm, body := agent.SplitSkillFrontmatter(data)
			if fm != nil {
				if fm.Description != "" {
					desc = fm.Description
				}
				// 顶级 `env:` 快捷方式优先；回退到带命名空间的 metadata.bkclaw|openclaw.env 形式。
				if len(fm.Env) > 0 {
					envSpec = fm.Env
				} else if meta := agent.ParseSkillMetadata(&fm.Metadata); meta != nil && meta.Meta() != nil {
					envSpec = meta.Meta().Env
				}
			}
			if desc == "" {
				for _, line := range strings.SplitN(body, "\n", 5) {
					line = strings.TrimSpace(line)
					if line != "" && !strings.HasPrefix(line, "#") {
						desc = line
						break
					}
				}
			}
		}
		entryOut := map[string]any{
			"name":        name,
			"description": desc,
			"location":    filepath.Join(dir, name),
			"type":        "skill",
		}
		if len(envSpec) > 0 {
			entryOut["envSpec"] = envSpec
		}
		out = append(out, entryOut)
	}
	return out
}

// handleDeleteAgentSkill 仅从 agent 自身的主目录中删除技能。全局/共享技能不受影响。
func (s *Server) handleDeleteAgentSkill(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	// 变更操作 — 仅限拥有者。Identity.CanAccessAgent 对会话调用者延迟返回 true，
	// 会让任何人删除任何 agent 的技能。
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	homePath, err := config.AgentHomeDir(id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	skillPath := filepath.Join(homePath, "skills", name)
	if err := os.RemoveAll(skillPath); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 从对象存储中删除，以便其他 pod 在下次补充时丢弃它。
	if s.workspaceStore != nil {
		if derr := skills.DeleteSkillUp(r.Context(), s.workspaceStore, id, name); derr != nil {
			slog.Warn("failed to remove agent skill from object store",
				"agent", id, "skill", name, "error", derr)
		}
	}
	// 热重载 agent，使已删除的技能从其上下文中移除。
	if ag := s.resolveAgent(r, id); ag != nil {
		ag.ReloadWorkspaceFiles()
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
