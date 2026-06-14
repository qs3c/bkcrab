package setup

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/skills"
)

// handleInstallSkill 从 skills.sh、clawhub.ai 或指定的 GitHub 仓库安装技能。请求体：
//
//	{
//	  "source": "skillssh" | "clawhub" | "github" | "" (自动),
//	  "name":   "<技能标识/文件夹名称>",
//	  "repo":   "owner/repo"  (仅限 github),
//	  "agent":  "<agent-id>"  (可选；若设置，则安装到该 agent 自己的技能目录并热重载；
//	                           否则全局安装 — 仅限管理员)
//	}
//
// 当 source 为空时的优先级：skills.sh → clawhub。
// 全局安装（无 `agent` 参数）需要本地/管理员用户 — 云端用户
// 不能通过此端点修改共享技能。
func (s *SkillsHandler) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"`
		Name   string `json:"name"`
		Skill  string `json:"skill"` // legacy alias for "name"
		Repo   string `json:"repo"`
		Agent  string `json:"agent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.Name == "" {
		req.Name = req.Skill
	}
	if req.Name == "" && req.Repo == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name or repo required"})
		return
	}

	if !s.authorizeSkillInstallTarget(w, r, req.Agent) {
		return
	}
	targetDir, err := resolveInstallTarget(r, req.Agent)
	if err != nil {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	result, err := runInstall(req.Source, req.Name, req.Repo, targetDir)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	// 将已安装的技能包镜像到共享对象存储中，以便其他 pod 在下次重载时加载。
	// 没有这一步，技能只存在于当前 pod 的 emptyDir 中，而路由到其他 pod 的聊天请求将无法看到它。
	if s.workspaceStore != nil && result != nil && result.Name != "" {
		owner := req.Agent
		if owner == "" {
			owner = skills.GlobalSkillOwner
		}
		if uerr := skills.SyncSkillUp(r.Context(), s.workspaceStore, owner, result.Name, targetDir); uerr != nil {
			slog.Warn("failed to mirror skill to object store",
				"owner", owner, "skill", result.Name, "error", uerr)
		}
	}

	if req.Agent != "" {
		if ag := s.guard.resolveAgent(r, req.Agent); ag != nil {
			ag.ReloadWorkspaceFiles()
		}
	}

	slog.Info("skill installed",
		"source", result.Source, "name", result.Name,
		"version", result.Version, "path", result.InstalledAt, "agent", req.Agent)
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"source":      result.Source,
		"name":        result.Name,
		"version":     result.Version,
		"installedAt": result.InstalledAt,
		"files":       result.FilesWritten,
	})
}

// authorizeSkillInstallTarget 强制执行注册表安装和 zip 上传共用的变更及目标范围规则。
func (s *SkillsHandler) authorizeSkillInstallTarget(w http.ResponseWriter, r *http.Request, agentID string) bool {
	if !requireWritable(w, r) {
		return false
	}
	if agentID != "" {
		// 仅限拥有者 — Identity.CanAccessAgent 对会话调用者延迟返回 true，
		// 因此如果没有显式的拥有者检查，任何人都可以将技能推送到其他人的 agent 主目录中。
		return s.guard.requireAgentOwner(w, r, agentID) != nil
	}
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return false
	}
	if !ident.CanAdminPlatform() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "platform admin required"})
		return false
	}
	return true
}

// resolveInstallTarget 选择安装的目标目录。授权在此辅助函数之前完成：
// agent 安装仅限拥有者；全局安装仅限平台管理员。
func resolveInstallTarget(r *http.Request, agentID string) (string, error) {
	if agentID != "" {
		// agents.id 全局唯一，因此主目录不需要用户命名空间 — 拥有者检查在此调用之前已完成。
		homePath, err := config.AgentHomeDir(agentID)
		if err != nil {
			return "", fmt.Errorf("resolve agent home: %w", err)
		}
		dir := filepath.Join(homePath, "skills")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create agent skills dir: %w", err)
		}
		return dir, nil
	}
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// runInstall 分发到正确的技能后端。当 source 为空时，先尝试 skills.sh 再尝试 clawhub
// （skill-creator 是聊天级别的回退方式，不是注册表 — 当两个源都未命中时，agent 工具会提供它）。
func runInstall(source, name, repo, targetDir string) (*skills.Result, error) {
	switch source {
	case "github":
		if repo == "" {
			return nil, fmt.Errorf("source=github requires 'repo'")
		}
		return skills.InstallFromGitHubRepo(repo, name, targetDir)
	case "clawhub":
		return skills.InstallFromClawHub(name, targetDir)
	case "skillssh", "skills.sh":
		results, err := skills.SearchSkillsSh(name)
		if err != nil {
			return nil, err
		}
		pick := skills.PickSkillsShExact(results, name)
		if pick == nil || pick.SkillID != name {
			return nil, fmt.Errorf("skill %q not found on skills.sh", name)
		}
		return skills.InstallFromSkillsSh(*pick, targetDir)
	case "", "auto":
		if repo != "" {
			return skills.InstallFromGitHubRepo(repo, name, targetDir)
		}
		return skills.InstallAuto(name, targetDir)
	default:
		return nil, fmt.Errorf("unknown source %q", source)
	}
}

// handleUploadSkill 从用户提供的 .zip 文件安装技能。
// Multipart POST：字段 `file` 为 zip 文件；可选的 `name` 表单字段
// 覆盖推断出的技能文件夹名称。可选的 `?agent=<id>` 查询参数
// 将安装范围限定到某个 agent 的主目录（与 handleInstallSkill 相同的认证+目标规则）。
//
// 布局假设：
//   - 带有单个公共顶级目录的 zip（例如 `my-skill/...`）：
//     该目录成为技能文件夹名称，其内容直接解压到 <target>/my-skill/ 下。
//   - 没有公共顶级目录的 zip（文件在根目录）：我们将它们包裹在
//     <target>/<name>/ 文件夹中，其中 <name> 默认为上传文件名去除扩展名，
//     并可通过 `name` 表单字段覆盖。
//
// Zip-slip 保护：每个解压的文件路径都经过验证，确保其保持在
// 所选技能目录下。符号链接被跳过 — Go 的 archive/zip
// 不会自动跟随它们，我们也拒绝在磁盘上重新创建它们。
func (s *SkillsHandler) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 64 << 20 // 64 MiB
	agentID := r.URL.Query().Get("agent")
	if !s.authorizeSkillInstallTarget(w, r, agentID) {
		return
	}

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "file field required"})
		return
	}
	defer file.Close()

	if hdr.Size > maxUploadSize {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "zip too large"})
		return
	}

	targetDir, err := resolveInstallTarget(r, agentID)
	if err != nil {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUploadSize+1))
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if int64(len(data)) > maxUploadSize {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "zip too large"})
		return
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "not a valid zip: " + err.Error()})
		return
	}

	commonTop := detectCommonTopDir(zr.File)
	skillName := strings.TrimSpace(r.FormValue("name"))
	if skillName == "" {
		skillName = commonTop
	}
	if skillName == "" {
		base := filepath.Base(hdr.Filename)
		skillName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	skillName = sanitizeSkillName(skillName)
	if skillName == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "could not determine skill name"})
		return
	}

	stripPrefix := ""
	if commonTop != "" {
		// Zip 已包含外层目录 — 在解压时剥离它，这样就不会出现 skill/skill/SKILL.md
		//（无论外层目录名是否匹配 skillName，或用户通过表单字段重命名，此逻辑均有效）。
		stripPrefix = commonTop + "/"
	}

	// 验证：一个有效的技能必须在其根目录下有 SKILL.md 文件。我们在创建任何目录之前进行检查，
	// 这样错误的上传（例如会话日志的 zip 包、随机文件夹）就不会污染 agent 的技能目录。
	// 精确匹配 SKILL.md（区分大小写 — agent/skills.go 中的运行时读取 "SKILL.md"）。
	hasSkillMD := false
	for _, entry := range zr.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		name := entry.Name
		if stripPrefix != "" && strings.HasPrefix(name, stripPrefix) {
			name = strings.TrimPrefix(name, stripPrefix)
		}
		if name == "SKILL.md" {
			hasSkillMD = true
			break
		}
	}
	if !hasSkillMD {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "zip is not a valid skill: SKILL.md not found at the skill root",
		})
		return
	}

	skillDir := filepath.Join(targetDir, skillName)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	skillDirAbs, err := filepath.Abs(filepath.Clean(skillDir))
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	files := make([]string, 0, len(zr.File))

	for _, entry := range zr.File {
		name := entry.Name
		if stripPrefix != "" {
			if name == strings.TrimSuffix(stripPrefix, "/") || name == stripPrefix {
				continue
			}
			if !strings.HasPrefix(name, stripPrefix) {
				continue
			}
			name = strings.TrimPrefix(name, stripPrefix)
		}
		if name == "" {
			continue
		}
		// 拒绝任何清理后名称逃逸出技能目录的条目。
		clean := filepath.Clean(name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			slog.Warn("skipping unsafe zip entry", "name", entry.Name)
			continue
		}
		dest := filepath.Join(skillDirAbs, clean)
		destAbs, err := filepath.Abs(filepath.Clean(dest))
		if err != nil || (destAbs != skillDirAbs && !strings.HasPrefix(destAbs, skillDirAbs+string(os.PathSeparator))) {
			slog.Warn("skipping zip-slip entry", "name", entry.Name, "dest", destAbs)
			continue
		}
		// 拒绝符号链接 — Go 的 archive/zip 通过 mode 位暴露它们。
		if entry.Mode()&os.ModeSymlink != 0 {
			slog.Warn("skipping symlink in zip", "name", entry.Name)
			continue
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(destAbs, 0o755); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		rc, err := entry.Open()
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		out, err := os.OpenFile(destAbs, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// 也对每个文件复制设置上限 — 在外部 64MiB 上限之后的纵深防御，防止 zip 炸弹。
		if _, err := io.Copy(out, io.LimitReader(rc, maxUploadSize)); err != nil {
			rc.Close()
			out.Close()
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		rc.Close()
		out.Close()
		files = append(files, clean)
	}

	if s.workspaceStore != nil {
		owner := agentID
		if owner == "" {
			owner = skills.GlobalSkillOwner
		}
		if uerr := skills.SyncSkillUp(r.Context(), s.workspaceStore, owner, skillName, targetDir); uerr != nil {
			slog.Warn("failed to mirror uploaded skill to object store",
				"owner", owner, "skill", skillName, "error", uerr)
		}
	}
	if agentID != "" {
		if ag := s.guard.resolveAgent(r, agentID); ag != nil {
			ag.ReloadWorkspaceFiles()
		}
	}

	slog.Info("skill uploaded",
		"name", skillName, "agent", agentID, "files", len(files), "path", skillDir)
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"source":      "upload",
		"name":        skillName,
		"installedAt": skillDir,
		"files":       files,
	})
}

// detectCommonTopDir 返回所有 zip 条目共享的第一个路径段，
// 如果条目不一致或任何条目位于 zip 根目录，则返回 ""。
// 用于判断用户的上传是否已将技能包裹在一个应剥离的文件夹中（并将其复用为技能名称）。
func detectCommonTopDir(files []*zip.File) string {
	var top string
	for _, f := range files {
		n := f.Name
		if n == "" {
			continue
		}
		// macOS Finder 打包的 zip 包含 `__MACOSX/` 元数据 — 忽略它，这样它的存在不会破坏公共顶级目录检测。
		if strings.HasPrefix(n, "__MACOSX/") {
			continue
		}
		idx := strings.Index(n, "/")
		if idx <= 0 {
			// 文件位于 zip 根目录 → 没有公共顶级目录
			return ""
		}
		seg := n[:idx]
		if top == "" {
			top = seg
		} else if top != seg {
			return ""
		}
	}
	return top
}

// sanitizeSkillName 去除用户提供的技能名称中的路径分隔符和其他意外字符，
// 留下一个安全的单层目录组件。
func sanitizeSkillName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.TrimSuffix(name, "/")
	name = strings.TrimSuffix(name, "\\")
	// 取最后一个路径段，以防用户输入了路径。
	name = filepath.Base(name)
	if name == "." || name == ".." || name == "/" || name == "\\" {
		return ""
	}
	// 过滤不安全的字符。
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '\x00':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// handleSearchSkills 返回搜索结果。source=skillssh（默认）访问
// https://skills.sh；source=clawhub 代理 clawhub.ai 的搜索端点。
// GET /api/skills/search?q=xxx&source=skillssh|clawhub
func (s *SkillsHandler) handleSearchSkills(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "skillssh"
	}

	switch source {
	case "skillssh", "skills.sh":
		results, err := skills.SearchSkillsSh(query)
		if err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"source": "skills.sh", "results": results})
	case "clawhub":
		u := fmt.Sprintf("https://clawhub.ai/api/v1/search?q=%s&limit=20", url.QueryEscape(query))
		resp, err := http.Get(u)
		if err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	default:
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unsupported source"})
	}
}
