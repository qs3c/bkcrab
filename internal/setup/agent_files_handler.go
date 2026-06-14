package setup

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/buildinfo"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// AgentFilesHandler 负责 per-agent 的工作区文件与系统文件（SOUL/IDENTITY/MEMORY 等）。
type AgentFilesHandler struct {
	dataStore      store.Store
	workspaceStore workspace.Store
	guard          *agentGuard
	ws             *workspaceRepo
	mw             *Middleware
}

// NewAgentFilesHandler 构造 AgentFilesHandler。
func NewAgentFilesHandler(dataStore store.Store, workspaceStore workspace.Store, guard *agentGuard, ws *workspaceRepo, mw *Middleware) *AgentFilesHandler {
	return &AgentFilesHandler{dataStore: dataStore, workspaceStore: workspaceStore, guard: guard, ws: ws, mw: mw}
}

// RegisterRoutes 注册 agent 工作区文件与系统文件路由。
func (s *AgentFilesHandler) RegisterRoutes(r *gin.Engine) {
	// 工作区文件
	r.GET("/api/agents/:id/files", wrap(s.mw.Auth(s.handleAgentFileList)))
	r.GET("/api/agents/:id/files.zip", wrap(s.mw.Auth(s.handleAgentFilesZip)))
	r.GET("/api/agents/:id/files/*path", wrap(s.mw.Auth(s.handleAgentFile)))
	r.POST("/api/agents/:id/files", wrap(s.mw.Auth(s.handleAgentFileUpload)))
	// 仅自托管：在本机文件浏览器中打开工作区目录；托管部署在 handler 内返回 403
	r.POST("/api/agents/:id/workspace/reveal", wrap(s.mw.Auth(s.handleAgentWorkspaceReveal)))

	// 系统文件（SOUL/IDENTITY/MEMORY/AGENTS 等）
	r.GET("/api/agents/:id/system-files/:name", wrap(s.mw.Auth(s.handleGetAgentSystemFile)))
	r.PUT("/api/agents/:id/system-files/:name", wrap(s.mw.Auth(s.handlePutAgentSystemFile)))
	r.DELETE("/api/agents/:id/system-files/:name", wrap(s.mw.Auth(s.handleDeleteAgentSystemFile)))
}

// Agent identity / memory files 文件 — 都存在于 agent_files 中，按 agent 作用域划分。
// 两个类别：
//
//   - identity 文件（下面的 agentIdentityFiles）是 agent 的规范"共享模板"。
//     它们存在于由 agent 拥有者的 user_id 键控的单行中 — 因此管理员配置、拥有者的编辑、
//     以及 agent 自己的 BOOTSTRAP 流程中的 write_file 调用都汇聚到同一行。
//     镜像 handlers_admin.forkAgentFiles 和 internal/agent/tools.identityFiles；
//     保持这三个列表同步。
//
//   - per-user 文件（USER.md, MEMORY.md）是真正因每个聊天者而不同的状态。
//     它们由调用者的有效 user_id 键控；非拥有者调用者可以编写自己的覆盖，
//     读取路径在不存在时回退到拥有者的行。
//
// 文件名允许列表门控此端点可以触及的文件；agent 运行时工具调用通过 workspace store 进行。
var agentSystemFileAllowlist = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "MEMORY.md": true,
	"HEARTBEAT.md": true, "USER.md": true, "agent.json": true,
}

var agentIdentityFiles = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "HEARTBEAT.md": true,
	"agent.json": true,
}

func (s *AgentFilesHandler) handleGetAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	caller := effectiveUserID(r)

	// Identity 文件：直接读取拥有者的行 — 这是唯一的事实来源，无论谁在询问。
	if agentIdentityFiles[name] {
		data, err := s.dataStore.GetAgentFileExact(r.Context(), id, rec.UserID, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				jsonResponse(w, http.StatusOK, map[string]any{"content": "", "source": "default"})
				return
			}
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"content": string(data), "source": "owner"})
		return
	}

	// Per-user 文件：优先使用调用者自己的行，回退到拥有者的行。
	// `source: "db"` 表示调用者已编写了覆盖；"owner" 表示我们通过回退显示
	// agent 拥有者的行。前端用此决定是否显示"已编辑"徽章并启用还原操作。
	if data, err := s.dataStore.GetAgentFileExact(r.Context(), id, caller, name); err == nil {
		baseContent := ""
		if rec.UserID != caller {
			if base, err2 := s.dataStore.GetAgentFileExact(r.Context(), id, rec.UserID, name); err2 == nil {
				baseContent = string(base)
			}
		}
		resp := map[string]any{"content": string(data), "source": "db"}
		if baseContent != "" {
			resp["baseContent"] = baseContent
		}
		jsonResponse(w, http.StatusOK, resp)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if rec.UserID != caller {
		if data, err := s.dataStore.GetAgentFileExact(r.Context(), id, rec.UserID, name); err == nil {
			jsonResponse(w, http.StatusOK, map[string]any{"content": string(data), "source": "owner"})
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"content": "", "source": "default"})
}

func (s *AgentFilesHandler) handlePutAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	target, ok := s.ws.resolveSystemFileTarget(w, r, id, name)
	if !ok {
		return
	}
	if err := s.dataStore.SaveAgentFile(r.Context(), id, target, name, []byte(body.Content)); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.guard.invalidateUser(target)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *AgentFilesHandler) handleDeleteAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	target, ok := s.ws.resolveSystemFileTarget(w, r, id, name)
	if !ok {
		return
	}
	if err := s.dataStore.DeleteAgentFile(r.Context(), id, target, name); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.guard.invalidateUser(target)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// resolveSystemFileTarget 确定对 (agentID, filename) 的写入/删除应影响哪个 user_id 行，并门控访问：
//
//   - Identity 文件（SOUL/IDENTITY/AGENTS/BOOTSTRAP/TOOLS/HEARTBEAT/agent.json）
//     始终以 agent 拥有者的行为目标 — 这是规范的"共享模板"。调用者必须是拥有者
//     或持有平台管理员权限（super_admin 会话或 type=admin apikey）。
//   - Per-user 文件（USER.md, MEMORY.md）以调用者自己的行为目标，
//     因此每个聊天者都有独立的覆盖。调用者只需要对 agent 的读取权限。
//
// 写入 4xx 并在权限/查找失败时返回 ok=false。
func (s *workspaceRepo) resolveSystemFileTarget(w http.ResponseWriter, r *http.Request, agentID, name string) (string, bool) {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return "", false
	}
	caller := effectiveUserID(r)
	ident, _ := auth.FromContext(r.Context())
	if agentIdentityFiles[name] {
		if rec.UserID != caller && !ident.CanAdminPlatform() {
			jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
			return "", false
		}
		return rec.UserID, true
	}
	if !s.guard.requireAgentReadable(w, r, agentID) {
		return "", false
	}
	return caller, true
}

// Workspace 文件 — 列出/获取/上传 agent 产生的工件。
// 由 workspace.Store blob 后端支持，其布局为
//
//   workspaces/<agent_id>/<session_id>/<path>
//
// 下面的 HTTP 文件端点操作在 agent 根级别（sessionID=""）—
// 那就是上传的目标位置，ListByAgent 返回该 agent 每个会话的对象。
// agent 运行时对于聊天中的工具调用传递自己的 sessionID；它们自动落在会话子前缀下。

// workspaceSessionScope 将 URL 中的 `?sessionId=` token 转换为
// workspaces/<agent>/sessions/ 下使用的目录名。URL token 是 session_key
// （因此仪表板可以统一地寻址任何会话），但工作区工件按 chat_id 命名空间 —
// 那是 agent 运行时在写入时传递的。
//
// 当 session_key 在调用者的 (user_id, agent_id) 下解析时返回 chat_id。
// 当查找失败时返回 "" — 包括会话属于不同用户的情况 —
// 因此调用者不会意外地扩大范围进入另一个用户的文件。
// 修复前的行为是回退到原始 URL token；在公开 agent 上，这允许非拥有者调用者
// 传递已知的拥有者 chat_id 并读取其文件，因为结果范围是 sessions/<他们的 chat>/。
func (s *workspaceRepo) workspaceSessionScope(ctx context.Context, agentID, urlToken string) string {
	tok := strings.TrimSpace(urlToken)
	if tok == "" || s.dataStore == nil {
		return ""
	}
	uid := config.UserIDFromContext(ctx)
	if uid == "" {
		return ""
	}
	_, _, chatID, err := s.dataStore.LookupSessionTriple(ctx, uid, agentID, tok)
	if err != nil || chatID == "" {
		return ""
	}
	return chatID
}

func (s *AgentFilesHandler) handleAgentFileList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"files": []any{}})
		return
	}
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	// 始终以 project 和 session 都为空的方式 List，以便返回的路径保持 agent 相对
	//（例如 "sessions/<sid>/foo.png" 或 "projects/<pid>/notes.md"）—
	// 下载端点期望该形状，在此处过滤比两个发散的代码路径更便宜。
	objects, err := s.workspaceStore.List(r.Context(), id, "", "")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	scope := s.ws.fileScopeForRequest(r, id)
	files := make([]map[string]any, 0, len(objects))
	for _, o := range objects {
		if !scope.acceptPath(o.Path) {
			continue
		}
		files = append(files, map[string]any{
			"path":    o.Path,
			"size":    o.Size,
			"modTime": o.ModTime.Unix(),
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"files": files})
}

// fileScope 描述哪些 agent 相对路径对文件浏览器/zip 过滤器可见。
// acceptPath 对作用域认为范围内的路径返回 true：
//
//	普通聊天：sessions/<chat_id>/ 下的路径
//	项目聊天：projects/<pid>/<chat_id>/ 下的路径（聊天自己的文件），
//	          加上直接位于 projects/<pid>/ 的文件（项目根目录的"共享/遗留"文件 —
//	          预子目录布局仍在那里，操作员可能有意将共享文件放在根目录）。
//	          其他聊天的子目录（projects/<pid>/<other-sid>/...）被排除 —
//	          它们属于那个聊天的面板。
//	无会话：所有内容（管理员浏览器）。
//
// archiveSuffix 返回 zip 文件名中使用的人类可读的作用域 id —
// 普通聊天为 chat_id，项目聊天为 "<pid>-<chat_id>"，
// 以便下载名称为 "agent-pid-sid.zip" 而不是仅靠 chat_id 消除歧义。
type fileScope struct {
	acceptPath    func(string) bool
	archiveSuffix string
}

// stripScopePrefix 从 agent 相对路径中删除最深的已知作用域前缀，
// 以便 zip 条目读作纯文件名。顺序很重要：项目聊天在会话聊天之前尝试，
// 这样 `projects/<pid>/<sid>/foo.md` 折叠为 `foo.md` 而不是 `<pid>/<sid>/foo.md`。
// 顶级项目文件也保留前导 `projects/<pid>/` 的剥离，以便它们也读作裸文件名。
func stripScopePrefix(p string) string {
	for _, top := range []string{"projects/", "sessions/"} {
		if !strings.HasPrefix(p, top) {
			continue
		}
		rest := p[len(top):]
		// 在作用域 id 后切割（一个路径段）。
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[i+1:]
			// 项目路径可以有第二个 id 段用于每个聊天的子目录；存在时也折叠它。
			if top == "projects/" {
				if j := strings.IndexByte(rest, '/'); j >= 0 {
					// 仅当第一个段看起来像聊天 id（s-... 前缀）时才将其视为聊天 id。
					// 否则保持 rest 不变，以便遗留的"子目录/file.md"结构不会被过度剥离。
					if first := rest[:j]; strings.HasPrefix(first, "s-") {
						rest = rest[j+1:]
					}
				}
			}
			return rest
		}
		return ""
	}
	return p
}

// rejectAllScope 返回一个不让任何内容通过的 fileScope。当调用者请求了 sessionId
// 但我们无法为他们解析时使用，这样非拥有者无法仅通过猜测/泄露 chat_id
// 在公开 agent 上扩大进入另一个用户的文件。
func rejectAllScope() fileScope {
	return fileScope{acceptPath: func(string) bool { return false }}
}

func (s *workspaceRepo) fileScopeForRequest(r *http.Request, agentID string) fileScope {
	rawSession := r.URL.Query().Get("sessionId")
	rawProject := r.URL.Query().Get("projectId")
	// 项目登录页面：没有打开特定的聊天，因此面板显示 projects/<pid>/ 下的所有内容 —
	// 每个聊天的子树加上根级别的共享文件。下面的 sessionId 分支是按聊天的视图；
	// 当 URL 是 /agents/<aid>/project/<pid> 且未选择聊天时使用此分支。
	if rawSession == "" && rawProject != "" {
		prefix := "projects/" + rawProject + "/"
		return fileScope{
			acceptPath:    func(p string) bool { return strings.HasPrefix(p, prefix) },
			archiveSuffix: rawProject,
		}
	}
	if rawSession == "" {
		// Agent 范围视图（完全没有范围参数）。拥有者 / super_admin
		// 可以合法地浏览每个文件；非拥有者（公开 agent 查看者、外部 apikey 调用者）
		// 必须指定一个他们拥有的会话，否则我们会给他们其他用户的文件。
		if s.guard.callerOwnsAgent(r, agentID) {
			return fileScope{acceptPath: func(string) bool { return true }}
		}
		return rejectAllScope()
	}
	chatID := s.workspaceSessionScope(r.Context(), agentID, rawSession)
	if chatID == "" {
		// sessionId 未解析到此调用者拥有的聊天 — 要么不存在，要么属于另一个用户。
		// 无论哪种方式，都不返回任何内容。修复前的行为是回退到"接受所有"，
		// 这在公开 agent 上意味着非拥有者可以通过传递垃圾 sessionId 列出每个聊天的文件。
		return rejectAllScope()
	}
	if pid := s.resolveSessionProject(r.Context(), r, agentID, rawSession); pid != "" {
		ownPrefix := "projects/" + pid + "/" + chatID + "/"
		rootPrefix := "projects/" + pid + "/"
		return fileScope{
			acceptPath: func(p string) bool {
				if strings.HasPrefix(p, ownPrefix) {
					return true
				}
				// 位于 projects/<pid>/<file> 的顶级文件（没有进一步的 "/" — 即不在任何 sid 子目录中）。
				if strings.HasPrefix(p, rootPrefix) {
					rest := p[len(rootPrefix):]
					return rest != "" && !strings.Contains(rest, "/")
				}
				return false
			},
			archiveSuffix: pid + "-" + chatID,
		}
	}
	prefix := "sessions/" + chatID + "/"
	return fileScope{
		acceptPath:    func(p string) bool { return strings.HasPrefix(p, prefix) },
		archiveSuffix: chatID,
	}
}

// handleAgentFilesZip 流式传输 agent 所有工作区文件的 zip（或仅当设置了 ?sessionId= 时的一个会话）。
// 文件以其会话相对路径添加，以便存档布局与用户在聊天面板中看到的匹配 — 没有外层包装目录。
func (s *AgentFilesHandler) handleAgentFilesZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		http.Error(w, "no workspace store", http.StatusServiceUnavailable)
		return
	}
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	objects, err := s.workspaceStore.List(r.Context(), id, "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	scope := s.ws.fileScopeForRequest(r, id)
	archiveName := fmt.Sprintf("%s.zip", id)
	if scope.archiveSuffix != "" {
		archiveName = fmt.Sprintf("%s-%s.zip", id, scope.archiveSuffix)
	}
	// 将条目包装在以存档命名的文件夹中，以便解压器（macOS Archive Utility、Windows Explorer、7zip…）
	// 将所有文件放在一个目录内，而不是松散地解压到 zip 旁边。
	// 没有这个，"解压了 5 个文件"看起来像"文件丢失了"，因为它们散布到 Downloads/ 中。
	wrapper := strings.TrimSuffix(archiveName, ".zip") + "/"

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	written, skipped, failed := 0, 0, 0
	for _, o := range objects {
		if !scope.acceptPath(o.Path) {
			skipped++
			continue
		}
		// 从存档条目名称中剥离最深的作用域前缀，这样用户在 zip 中看到干净的
		// 文件名，而不是嵌套的 `projects/<pid>/<sid>/foo.md` 路径。
		entryName := stripScopePrefix(o.Path)
		if entryName == "" {
			skipped++
			continue
		}
		hdr := &zip.FileHeader{
			Name:     wrapper + entryName,
			Method:   zip.Deflate,
			Modified: o.ModTime,
		}
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			// 继续而非返回 — 用其余条目完成存档比中途退出给用户留下单个文件更有用。
			// 修复前的行为：途中任何瞬时故障都会截断 zip 为已写入的内容，
			// 在生产中表现为"只有一张图片出来了"。
			slog.Warn("zip: create entry failed", "agent", id, "path", o.Path, "err", err)
			failed++
			continue
		}
		rc, err := s.workspaceStore.Get(r.Context(), id, "", "", o.Path)
		if err != nil {
			slog.Warn("zip: open object failed", "agent", id, "path", o.Path, "err", err)
			failed++
			continue
		}
		_, copyErr := io.Copy(entry, rc)
		rc.Close()
		if copyErr != nil {
			slog.Warn("zip: copy failed", "agent", id, "path", o.Path, "err", copyErr)
			failed++
			continue
		}
		written++
	}
	if err := zw.Close(); err != nil {
		slog.Warn("zip: writer close failed", "agent", id, "err", err)
	}
	slog.Info("zip: archive sent", "agent", id, "archive", archiveName,
		"objects", len(objects), "written", written, "skipped", skipped, "failed", failed)
}

// handleAgentWorkspaceReveal 在操作系统的原生文件浏览器（Finder/Explorer/xdg-open）中打开聊天者的工作区文件夹。
// 仅限自托管 — 托管部署没有"操作员的本地文件系统"的有意义概念，聊天者也不拥有守护进程，
// 因此暴露此功能将是权限泄露。从查询字符串读取 sessionId / projectId，
// 镜像 fileScopeForRequest 的解析（session_key → chat_id, project 查找），
// 以便打开的目录匹配聊天侧 Workspace 面板显示的内容。
//
// 尽力而为：成功时返回 200 及解析的路径，作用域错误时返回 4xx，
// 配置的工作区存储不暴露主机路径时返回 503（S3 / R2 部署），
// OS 打开命令失败时返回 500。非阻塞 — 我们不等待 Finder 实际显示窗口。
func (s *AgentFilesHandler) handleAgentWorkspaceReveal(w http.ResponseWriter, r *http.Request) {
	if buildinfo.IsHostedDeploy() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "workspace reveal is disabled on hosted deployments"})
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store configured"})
		return
	}
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}

	scoper, ok := s.workspaceStore.(workspace.LocalScoper)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store has no local path (e.g. S3-backed) — open in Finder is unavailable"})
		return
	}

	rawSession := r.URL.Query().Get("sessionId")
	rawProject := r.URL.Query().Get("projectId")

	// 解析到聊天侧面板作用域的相同 (project, chatID)。
	// 空的 rawSession + 非空 projectId 表示项目登录页面 — 打开项目根目录。
	// 两者都空表示 agent 根目录（管理员浏览器）；我们仍然允许，因为 requireAgentReadable 已经门控了访问。
	chatID := ""
	projectID := rawProject
	if rawSession != "" {
		chatID = s.ws.workspaceSessionScope(r.Context(), id, rawSession)
		if pid := s.ws.resolveSessionProject(r.Context(), r, id, rawSession); pid != "" {
			projectID = pid
		}
	}

	dir, ok := scoper.LocalScopeDir(id, projectID, chatID)
	if !ok || dir == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store did not return a host path"})
		return
	}

	// 预创建目录，这样 `open <不存在的路径>` 在一个尚未写入任何文件的全新聊天上不会出错 —
	// 空的文件夹仍然给用户一种进展的感觉。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if err := openInFileBrowser(dir); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

// openInFileBrowser 调用平台适当的"打开"命令。macOS 和 Linux 行为一致（在默认文件管理器中打开目录）；
// Windows 使用 explorer.exe。我们故意不等待子进程 — Finder 特别是立即返回，
// 而且无论哪种方式都没有有用的退出代码可显示。
func openInFileBrowser(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		// `explorer` 即使成功也返回退出码 1，因此我们不检查 err。
		// 唯一的真正失败模式是"二进制不在 PATH 上"，Start() 会报告。
		cmd = exec.Command("explorer", path)
		return cmd.Start()
	default:
		// Linux / *BSD — xdg-open 是 freedesktop 标准。
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	// 分离：我们不关心文件管理器的生命周期。
	go func() { _ = cmd.Wait() }()
	return nil
}

func (s *AgentFilesHandler) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")
	if rel == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	if s.workspaceStore != nil {
		s.ws.serveFileFromWorkspaceStore(w, r, id, rel)
		return
	}
	// Workspace store 未配置 — 回退到直接 FS 读取。
	// 本地 FS 布局镜像 workspace store：
	// ~/.bkclaw/workspaces/<agent_id>/<path>。
	home, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	root := filepath.Join(home, "workspaces", id)
	abs := filepath.Join(root, filepath.Clean("/"+rel))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "path escape"})
		return
	}
	// ServeFile 从 mime 数据库自身设置 Content-Type；我们只是在此基础上为 HTML 添加 CSP sandbox —
	// 与上面 setFileResponseHeaders 中相同的理由。
	if ext := strings.ToLower(filepath.Ext(rel)); ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, abs)
}

func (s *workspaceRepo) serveFileFromWorkspaceStore(w http.ResponseWriter, r *http.Request, agentID, path string) {
	rc, err := s.workspaceStore.Get(r.Context(), agentID, "", "", path)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	defer rc.Close()
	setFileResponseHeaders(w, path)
	io.Copy(w, rc)
}

// setFileResponseHeaders 为用户产生的工作区文件选择正确的 Content-Type，
// 并锁定 agent 生成的 HTML，使其即使在用户直接在标签页中打开 URL 时也无法访问
// 应用的 cookie/存储。从扩展名派生的 Content-Type 允许 iframe 渲染文件
// （octet-stream → about:blank，因为 iframe 不嗅探）。CSP `sandbox` 头部
// 与聊天预览通过 iframe `sandbox` 属性获得的保护相同，但在 HTTP 层应用，
// 因此无论文件如何加载都能生效。
func setFileResponseHeaders(w http.ResponseWriter, path string) {
	ext := strings.ToLower(filepath.Ext(path))
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
}

func (s *AgentFilesHandler) handleAgentFileUpload(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store"})
		return
	}
	if rec := s.guard.requireAgentOwner(w, r, id); rec == nil {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 聊天客户端对每个附件发送一个表单字段 "file"，因此 multipart 负载通常在同一个键下携带多个条目。
	// r.FormFile 只返回第一个 — 遍历 MultipartForm.File 以便多附件上传提交所有文件，而不仅仅是一个。
	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	// sessionId 将上传限定到 agent 实际看到的沙箱挂载点。
	// 我们解析会话以找到其 project_id，以便在项目聊天中的上传落在 projects/<pid>/ 旁边
	// 与 agent 自己的写入一起；普通聊天保留旧的 sessions/<chat>/ 子目录。
	sessionKey := r.URL.Query().Get("sessionId")
	sessionID := s.ws.workspaceSessionScope(r.Context(), id, sessionKey)
	projectID := s.ws.resolveSessionProject(r.Context(), r, id, sessionKey)
	if projectID != "" {
		// 项目会话不使用每个聊天的子目录 — 清除它，以便 workspace store 路由到 projects/<pid>/。
		sessionID = ""
	}
	saved := make([]map[string]any, 0, len(headers))
	for _, h := range headers {
		fh, err := h.Open()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		data, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.workspaceStore.Put(r.Context(), id, projectID, sessionID, h.Filename, strings.NewReader(string(data)), int64(len(data)), ""); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		saved = append(saved, map[string]any{"name": h.Filename, "size": len(data)})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "files": saved})
}
