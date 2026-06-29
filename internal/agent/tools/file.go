package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkcrab/internal/sandbox"
	"github.com/qs3c/bkcrab/internal/skills"
)

type readFileArgs struct {
	Path string `json:"path"`
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type listDirArgs struct {
	Path string `json:"path"`
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// editSchema 是为 edit_file 公布的 JSON 模式。定义一次并且
// 由 registerFile / registerSandboxedFile 重用，因此两个注册
// 路径不能随参数形状而漂移。
var editSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"path": map[string]interface{}{
			"type":        "string",
			"description": "File path (relative to your working directory or absolute)",
		},
		"old_string": map[string]interface{}{
			"type":        "string",
			"description": "Exact text to replace. Must match a unique substring in the file unless replace_all is true.",
		},
		"new_string": map[string]interface{}{
			"type":        "string",
			"description": "Replacement text. Must differ from old_string.",
		},
		"replace_all": map[string]interface{}{
			"type":        "boolean",
			"description": "Replace every occurrence of old_string instead of requiring uniqueness. Defaults to false.",
		},
	},
	"required": []string{"path", "old_string", "new_string"},
}

const editDescription = "Edit a non-memory file by replacing an exact substring. Prefer this over write_file when changing only part of an ordinary file: it's cheaper, can't drop unrelated content, and validates the replacement was applied. USER.md and MEMORY.md are managed memory resources; use the memory tool for them. old_string must match a unique substring unless replace_all is true; new_string must differ from old_string. Read the file first if you're unsure of the exact text."

// validateFileTargetPath 拒绝类似写入操作的路径参数
// 无法引用单个文件。空字符串、目录后缀路径
// （“foo/”）和特殊目录别名（“.”、“..”、“/”）全部消失
// 通过下游路由（isWorkspacePath 将“”视为工作空间-
// 范围是因为 filepath.Clean("") == ".") 并最终在 os.OpenFile 上
// 会话目录，出现一个神秘的“是一个目录”错误。
// 尽早拒绝为模型提供了可操作的工具形信息
// 反而。
func validateFileTargetPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("path is required and must include a filename")
	}
	if strings.HasSuffix(path, "/") || strings.HasSuffix(path, string(filepath.Separator)) {
		return fmt.Errorf("path %q ends in a separator; include a filename at the end", path)
	}
	switch filepath.Clean(path) {
	case ".", "..", "/":
		return fmt.Errorf("path %q is a directory, not a file; include a filename", path)
	}
	return nil
}

// asIsDirToolError 检测到“是目录”故障模式（在以下情况下引发）
// 写入/编辑解析为现有目录而不是文件）和
// 将其提升为模型可以从中恢复的工具级消息。瀑布
// 否则返回调用者的包装错误。
func asIsDirToolError(opName, path string, err error) error {
	if err != nil && strings.Contains(err.Error(), "is a directory") {
		return fmt.Errorf("%s: %q resolves to a directory; include a filename in the path", opName, path)
	}
	return nil
}

// applyEdit 执行支持 edit_file 的内存中字符串替换。
// 集中化，因此每个后端（文件系统、workspaceStore、systemFileStore、
// 沙箱执行器）具有相同的唯一性/未找到/无操作规则。
// 返回新内容和替换内容的计数；如果编辑则出错
// 无法按要求申请。
func applyEdit(path, content, oldStr, newStr string, replaceAll bool) (string, int, error) {
	if oldStr == "" {
		return "", 0, fmt.Errorf("edit_file: old_string is empty (use write_file to create a file)")
	}
	if oldStr == newStr {
		return "", 0, fmt.Errorf("edit_file: new_string must differ from old_string")
	}
	count := strings.Count(content, oldStr)
	if count == 0 {
		return "", 0, fmt.Errorf("edit_file: old_string not found in %s — re-read the file and copy the exact text (whitespace/indentation matters)", path)
	}
	if count > 1 && !replaceAll {
		return "", 0, fmt.Errorf("edit_file: old_string matches %d locations in %s — provide more surrounding context to make it unique, or set replace_all=true", count, path)
	}
	if replaceAll {
		return strings.ReplaceAll(content, oldStr, newStr), count, nil
	}
	return strings.Replace(content, oldStr, newStr, 1), 1, nil
}

var errOutsideSandbox = fmt.Errorf("access denied: path is outside the allowed sandbox directory")

// globalSkillsDirSuffix 用于检测写入的尝试
// 管理员管理的全局技能目录（~/.bkcrab/skills/）。读取的内容是
// 很好——技能层已经暴露了这个内容——但是从
// 聊天可以让特工默默地安装/覆盖其他人的技能
// 主机上的代理。
const globalSkillsDirSuffix = "/.bkcrab/skills"

// 当 write_file 目标时返回 errGlobalSkillsDirWrite
// ~/.bkcrab/skills/ 来自代理聊天。该消息告诉模型
// 具体如何恢复。
var errGlobalSkillsDirWrite = fmt.Errorf("access denied: ~/.bkcrab/skills/ is the admin-managed global skills directory. To create a new skill, load the \"skill-creator\" skill and follow its workflow (it scaffolds into this agent's private skills dir). To install an existing one, use the install_skill tool")

const ManagedMemoryFileRefusal = `[refused: USER.md and MEMORY.md are managed memory resources. Use the memory tool with target="user" or target="memory" to list, add, replace, remove, or batch-edit entries.]`

// systemFiles 是代理元数据/身份文件。当相对路径
// 通过基本名称引用其中之一，文件工具根据
// 系统 root 而不是用户 root。
var systemFiles = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"BOOTSTRAP.md": true,
	"MEMORY.md":    true,
	"HEARTBEAT.md": true,
	"AGENTS.md":    true,
	"TOOLS.md":     true,
	"agent.json":   true,
}

func isManagedMemoryFilePath(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	slashClean := strings.ReplaceAll(clean, `\`, "/")
	base := pathpkg.Base(slashClean)
	if !strings.EqualFold(base, "USER.md") && !strings.EqualFold(base, "MEMORY.md") {
		return false
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\\`) || isWindowsAbsolutePath(path) {
		return true
	}
	return !strings.Contains(slashClean, "/")
}

func isWindowsAbsolutePath(path string) bool {
	if len(path) < 3 {
		return false
	}
	drive := path[0]
	return ((drive >= 'A' && drive <= 'Z') || (drive >= 'a' && drive <= 'z')) &&
		path[1] == ':' &&
		(path[2] == '\\' || path[2] == '/')
}

func (r *Registry) managedMemoryFileBlocked(path string) bool {
	return isManagedMemoryFilePath(path)
}

// isWorkspacePath 决定 write/read/list_dir 路径是否属于
// 工作区存储（相对于磁盘上代理的 home / systemRoot）。用途相同
// 规则为 rootForPath：身份文件名、`skills/` 子树，以及
// 绝对路径保留在磁盘上；其他一切都在工作空间范围内。
func (r *Registry) isWorkspacePath(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	if clean == "skills" || strings.HasPrefix(clean, "skills"+string(filepath.Separator)) {
		return false
	}
	if !strings.ContainsRune(clean, filepath.Separator) && systemFiles[clean] {
		return false
	}
	return true
}

// hostHomePath 返回已解析的绝对文件系统路径
// arg 看起来像chatter想要读/写的操作员主机路径，
// 否则为假。认可三种形式：
//
// ~ → 操作者的主目录
// ~/<rel> → 加入到操作者的主目录下
// /Users/<u>/... → macOS 风格的绝对根目录
// /home/<u>/... → Linux 风格的绝对主根
//
// 由自托管安装上的沙盒文件工具用来路由
// 诸如“read ~/Downloads/foo.csv”之类的请求到实际主机磁盘
// 而不是沙盒 FS 内的 404'ing。托管（多租户）
// 部署故意不这么称呼——聊天不拥有
// 守护进程的文件系统，因此暴露它会导致权限泄漏。
//
// 当路径不是主机主目录引用时返回 ("", false)，或者
// 当它落入 BkCrab 管理的根源之一时
// (~/.bkcrab/...) — 这些是运行时内部结构，应该保留
// 流经他们现有的路由（workspaceStore、身份
// 存储等），因此聊天写入不能破坏代理的数据库文件。
func hostHomePath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		// 仅沙箱/BkCrab 内部子树：跳过主机扩展
		// 因此读/写会落入沙箱执行器
		// 在路径不存在的主机磁盘上尝试（并失败）
		// 存在。与下面的绝对路径防护对称。
		// ~/.bkcrab/... — 运行时内部（数据库，工作区，...）
		// ~/.agents/... — npx 技能的沙箱绑定挂载目标。
		// 主机在 <BKCRAB_HOME>/users/<uid>/skills/ 中有这些，
		// 不下～。 `ls ~/.agents/skills/<x>/` 运行后
		// 在沙箱中模型自然调用
		// read_file 具有相同的路径；仅那条路
		// 在容器内解析。
		if strings.HasPrefix(path, "~/.bkcrab") || strings.HasPrefix(path, "~/.agents") {
			return "", false
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		if path == "~" {
			return home, true
		}
		return filepath.Join(home, path[2:]), true
	}
	if !filepath.IsAbs(path) {
		return "", false
	}
	if strings.HasPrefix(path, "/Users/") || strings.HasPrefix(path, "/home/") {
		// 即使在喋喋不休时也拒绝 BkCrab-内部子路径
		// 通过主机家庭频道联系他们。与同一个后卫
		// errGlobalSkillsDirWrite，范围更广。
		if home, err := os.UserHomeDir(); err == nil {
			bkcrabDir := filepath.Join(home, ".bkcrab")
			if path == bkcrabDir || strings.HasPrefix(path, bkcrabDir+string(filepath.Separator)) {
				return "", false
			}
		}
		return path, true
	}
	return "", false
}

// isSkillPath 报告路径是否是聊天时 `skills/<name>/...`
// write——技能创造者公约。绝对路径和裸路径
// `skills` 段不符合条件（后者是一个目录，而不是一个
// 文件写入）。清理路径，以便 `skills/./foo/SKILL.md` 匹配。
func (r *Registry) isSkillPath(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "skills" && strings.HasPrefix(clean, "skills"+string(filepath.Separator))
}

// SkillRoot 返回“skills/”子目录的主机父目录
// 聊天时技能写入应该落地。配置后按用户
// （喋喋不休的个人桶），否则代理回家。
func (r *Registry) skillRoot() string {
	if r.userSkillsRoot != "" {
		return r.userSkillsRoot
	}
	return r.systemRoot
}

// SkillStoreOwner 返回workspace.Store 伪所有者密钥
// 聊天创建的技能应该镜像到。 userSkillsRoot 时按用户
// 已设置（因此技能遵循代理之间的聊天）；代理 ID
// 否则（传统/单用户模式）。
func (r *Registry) skillStoreOwner() string {
	if r.userSkillsRoot != "" && r.userID != "" {
		return skills.UserSkillOwner(r.userID)
	}
	return r.agentID
}

// writeSkillToHost 将聊天创建的 `skills/<name>/<rel>` 文件放置在
// 主机磁盘并将其镜像到工作区存储，以便 SkillsLoader 的
// 本地扫描和任何同级 pod 的水合物都可以看到它。使用者
// 沙箱模式 write_file 路径（否则会捕获文件
// 在临时沙箱 FS 内）并通过主机模式 write_file 作为
// 写入后存储同步挂钩。
//
// 路径 arg 必须已通过 isSkillPath。返回绝对值
// 写入主机路径，以便调用者可以将其回显给模型。
func (r *Registry) writeSkillToHost(ctx context.Context, path, content string) (string, error) {
	root := r.skillRoot()
	if root == "" {
		return "", fmt.Errorf("write_file: no skills root configured for path %q", path)
	}
	full := filepath.Join(root, filepath.Clean(path))
	if isGlobalSkillsPath(full) {
		return "", errGlobalSkillsDirWrite
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	// 镜像到工作区存储，以便成为同级 Pod（云部署）
	// 在下一回合中吸收新技能，而不是等待
	// 吊舱重新启动。尽力而为；这里的失败不会取消文件的写入。
	if r.workspaceStore != nil {
		if owner := r.skillStoreOwner(); owner != "" {
			rel := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "skills/")
			parts := strings.SplitN(rel, "/", 2)
			if len(parts) >= 1 && parts[0] != "" {
				skillName := parts[0]
				skillsDir := filepath.Join(root, "skills")
				if err := skills.SyncSkillUp(ctx, r.workspaceStore, owner, skillName, skillsDir); err != nil {
					slog.Warn("skill mirror to store failed",
						"owner", owner, "skill", skillName, "error", err)
				}
			}
		}
	}
	return full, nil
}

// rootForPath 返回相对路径应解析的根：
// - 身份文件的 systemRoot（代理主目录）（SOUL.md、IDENTITY.md，...）；
// - userSkillsRoot (~/.bkcrab/users/<uid>/skills/) 用于“技能/...”
// 当聊天者的用户技能目录连接时写入（默认为
// 多用户安装）。在这里进行路由，以便积累聊天创建的技能
// 在聊天者的个人存储桶中 - 与他们的每个代理共享
// 聊天，与代理所有者的官方技能和隔离
// 同一共享代理上的其他用户。回退到 systemRoot 时
// userSkillsRoot 为空（旧版/单用户安装）；
// - userRoot（代理工作区）用于其他所有内容，面向用户
// 神器领地。
//
// 绝对路径按原样返回。
func (r *Registry) rootForPath(path string) string {
	if filepath.IsAbs(path) {
		return ""
	}
	clean := filepath.Clean(path)
	if clean == "skills" || strings.HasPrefix(clean, "skills"+string(filepath.Separator)) {
		// 配置时为每用户存储桶，否则为代理主页
		// （遗留行为）。保留前导的“skills/”前缀
		// 无论哪种情况，SkillsLoader 的扫描都会拾取它。
		if r.userSkillsRoot != "" {
			return r.userSkillsRoot
		}
		return r.systemRoot
	}
	// 单段系统文件（SOUL.md，IDENTITY.md，...）也路由
	// 家;像“notes/SOUL.md”这样的嵌套路径保留在用户内容中。
	if !strings.ContainsRune(clean, filepath.Separator) && systemFiles[clean] {
		return r.systemRoot
	}
	return r.userRoot
}

func registerFile(r *Registry) {
	r.Register("read_file", "Read the contents of a non-memory file. USER.md and MEMORY.md are managed memory resources; use the memory tool to inspect them.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (relative to your working directory or absolute)",
			},
		},
		"required": []string{"path"},
	}, makeReadFile(r))

	r.Register("write_file", "Write content to a non-memory file (creates directories as needed). USER.md and MEMORY.md are managed memory resources; use the memory tool to update them.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (relative to your working directory or absolute)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
		},
		"required": []string{"path", "content"},
	}, makeWriteFile(r))

	r.Register("list_dir", "List files and directories in a path", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path (relative to your working directory or absolute)",
			},
		},
		"required": []string{"path"},
	}, makeListDir(r))

	r.Register("edit_file", editDescription, editSchema, makeEditFile(r))
}

func resolvePath(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

// isGlobalSkillsPath 报告absPath 是否指向或低于
// 管理员管理的 ~/.bkcrab/skills/ 目录。跨用户主页工作
// 通过匹配 stable 后缀来定位位置。
func isGlobalSkillsPath(absPath string) bool {
	clean := filepath.Clean(absPath)
	return strings.HasSuffix(clean, globalSkillsDirSuffix) || strings.Contains(clean, globalSkillsDirSuffix+string(filepath.Separator))
}

// resolvePathSandboxed 解析路径并验证它是否位于其中
// 沙箱根。当解析的路径转义时返回错误。
func resolvePathSandboxed(root, sandboxRoot, path string) (string, error) {
	full := resolvePath(root, path)
	if sandboxRoot == "" {
		return full, nil
	}
	absRoot, err := filepath.Abs(sandboxRoot)
	if err != nil {
		return "", fmt.Errorf("invalid sandbox root: %w", err)
	}
	absFull, err := filepath.Abs(full)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}
	if !strings.HasPrefix(absFull, absRoot+string(filepath.Separator)) && absFull != absRoot {
		return "", errOutsideSandbox
	}
	return absFull, nil
}

// effectiveSandboxRoot 选择文件操作应强制执行的界限
// 针对“root”解析的路径。身份文件（SOUL.md / IDENTITY.md /
// ...) 住在 r.systemRoot — 代理主页，工作区沙箱之外
// mount - 因此工作区沙箱绑定总是会拒绝它们。
// 将系统文件操作限制在 systemRoot 本身，这
// 在不破坏合法性的情况下保持拉链式逃逸被阻止
// systemFileStore 查找时的“代理读取自己的 IDENTITY.md”流程
// 未命中（新鲜试剂、尚未水化的存储、未配置存储
// 全部）。
func (r *Registry) effectiveSandboxRoot(root string) string {
	if root == r.systemRoot && r.systemRoot != "" {
		return r.systemRoot
	}
	return r.sandboxRoot
}

// 当有效负载中包含 NUL 字节时，looksBinary 返回 true
// 第一个 8KB — JPEG/PNG/PDF/zip/wasm/等的近乎完美信号。我们
// 拒绝通过 read_file 读取二进制文件，因为字节被强制
// 转换为 Go 字符串，然后作为 tool_result 文本发送到 LLM：5MB JPG
// 变成约 150 万个乱码 UTF-8 标记，超出了每个模型的上下文
// 限制并将下一个推论变成一个多分钟的“思考......”
// 停顿（或彻底的 API 错误）。二进制文件的正确路径是
// 将路径直接提供给处理该格式的任何技能
// （图像工具的“输入”等）——永远不要内联字节。
func looksBinary(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

func binaryRefusal(path string, size int) string {
	// 设计上与技能无关：read_file 是一个系统工具，但它
	// 技能是二进制路径的正确消费者取决于什么
	// Host Agent 实际上已安装（图像编辑、OCR、
	// 存档摘录，...）。在这里命名特定技能会产生误导
	// 没有它的代理商。按技能指导属于
	// 技能的 SKILL.md / 代理的 SOUL.md，不在系统工具中
	// 错误路径。
	//
	// 该消息必须执行的操作：停止模型的“让我探测
	// 文件优先”反射（在 5MB 上文件/识别/内联 python
	// JPEG 会反复刻录并且永远不会产生有用的结果）。这
	// “勿探”线是承重部分。
	return fmt.Sprintf("[read_file refused: %q is a binary file (%d bytes). Binary bytes don't decode as text — loading them would blow past the context window. Don't probe with `file`, `identify`, `ls`, `python`, or any inline script — pass the path directly to whichever skill in your toolset handles this format (e.g. an image-editing skill for images). If your toolset doesn't have a skill for this format, tell the user instead of trying to inline-process the bytes.]", path, size)
}

func makeReadFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		if r.managedMemoryFileBlocked(args.Path) {
			return ManagedMemoryFileRefusal, nil
		}
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}

		// 镜像 makeWriteFile 的路由：userRoot 指定的路径转到
		// 配置工作区存储时。
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.sessionID, args.Path)
			if err != nil {
				return "", fmt.Errorf("workspace get: %w", err)
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return "", fmt.Errorf("workspace read: %w", err)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			return string(data), nil
		}

		// 身份文件读取始终首先通过持久存储
		// （db 是事实来源；磁盘上是后备）。使用宽容
		// 基本名称匹配，因此 LLM 将“IDENTITY.md”扩展为
		// 它在提示的“工作目录”中看到的完整主机路径
		// 商店里仍然排队——早些时候我们需要一个裸露的
		// 文件名和绝对路径完全绕过存储，
		// 从身份文件不存在的工作空间目录中读取。
		if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(args.Path) {
			name := filepath.Base(filepath.Clean(args.Path))
			if data, err := r.readSystemFileForUser(ctx, r.systemFileUserID(name), name); err == nil {
				return string(data), nil
			}
			// Store miss：直接尝试磁盘上代理的systemRoot，
			// 绕过resolvePathSandboxed。 systemRoot是代理
			// 元数据目录（例如 ~/.bkcrab/agents/<id>/agent）
			// 在 K8s 部署中，位于 sandboxRoot 之外，因此
			// 沙箱绑定总是会拒绝身份文件，即使
			// 尽管文件名是固定的白名单，无法转义
			// 表面。 “未找到”是合法的（新代理可能
			// 还没有 IDENTITY.md 行） — 返回空，因此代理
			// 将字段视为未设置，匹配方式
			// ContextBuilder.loadFile 加载身份文件
			// 系统提示。
			if r.systemRoot != "" {
				if data, err := os.ReadFile(filepath.Join(r.systemRoot, name)); err == nil {
					return string(data), nil
				}
			}
			return "", nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			// 身份文件（SOUL/IDENTITY/BOOTSTRAP/...）通常会被取消设置
			// 在新安装的 sqlite 上 - 商店只有向导的内容
			// 写入（通常只是 SOUL.md）和代理的主机主目录
			// 甚至没有被创建。表面“”而不是未找到的错误，所以
			// 代理将文件视为空白并继续，匹配方式
			// ContextBuilder.loadFile 为系统提示符加载它们。
			if os.IsNotExist(err) && isSingleSegmentSystemFile(args.Path) {
				return "", nil
			}
			return "", fmt.Errorf("read file: %w", err)
		}

		if looksBinary(data) {
			return binaryRefusal(args.Path, len(data)), nil
		}
		return string(data), nil
	}
}

func makeWriteFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args writeFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("write_file: %w", err)
		}

		if r.managedMemoryFileBlocked(args.Path) {
			return ManagedMemoryFileRefusal, nil
		}

		// 身份文件保密门——也可以阻止闲聊
		// 通过即时注入重写代理的角色。
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}

		// 配置工作区存储后，路由 userRoot-destined
		// 通过它来写。身份文件（systemRoot）仍然命中
		// 文件系统，因为内存存储已经覆盖了它们
		// 通过单独的路径实现持久性。
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			if err := r.workspaceStore.Put(ctx, r.agentID, r.projectID, r.sessionID, args.Path,
				strings.NewReader(args.Content), int64(len(args.Content)), ""); err != nil {
				if friendly := asIsDirToolError("write_file", args.Path, err); friendly != nil {
					return "", friendly
				}
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
		}

		// 身份文件（SOUL.md / IDENTITY.md / ...）需要登陆
		// 管理 UI 从中读取的同一个持久存储 — 否则
		// 代理的 BOOTSTRAP 流将写入 pod 本地磁盘，并且
		// 自定义页面将显示空白。路线经过
		// systemFileStore（如果可用）。
		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.systemFileUserID(name), name, []byte(args.Content)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			// 保留文件系统镜像，以便代理运行时（上下文
			// 构建器、技能加载器等）仍然从磁盘读取
			// 在此 Pod 上看到相同的内容。其他 pod 会选择
			// 通过他们自己的存储读取来进行下一个调用。
			if r.systemRoot != "" {
				disk := filepath.Join(r.systemRoot, name)
				_ = os.MkdirAll(filepath.Dir(disk), 0o755)
				_ = os.WriteFile(disk, []byte(args.Content), 0o644)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), name), nil
		}

		// 技能脚手架采用专用路径，因此相同的 writeSkillToHost
		// 处理沙盒模式的助手也会将文件+镜像着陆到
		// 工作区存储在这里，而不是复制 SyncSkillUp
		// 钩在两个地方。
		if r.isSkillPath(args.Path) && r.skillRoot() != "" {
			full, err := r.writeSkillToHost(ctx, args.Path, args.Content)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), full), nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		if isGlobalSkillsPath(fullPath) {
			return "", errGlobalSkillsDirWrite
		}
		dir := filepath.Dir(fullPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("create directory: %w", err)
		}

		if err := os.WriteFile(fullPath, []byte(args.Content), 0o644); err != nil {
			if friendly := asIsDirToolError("write_file", args.Path, err); friendly != nil {
				return "", friendly
			}
			return "", fmt.Errorf("write file: %w", err)
		}

		return fmt.Sprintf("Written %d bytes to %s", len(args.Content), fullPath), nil
	}
}

func makeEditFile(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args editFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("edit_file: %w", err)
		}

		if r.managedMemoryFileBlocked(args.Path) {
			return ManagedMemoryFileRefusal, nil
		}

		// 身份文件保密门。
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}

		// 镜像 makeWriteFile 的路由优先级：工作区存储优先
		// （用户工件），然后身份文件存储（SOUL.md / IDENTITY.md /
		// MEMORY.md ...），然后是文件系统。读和写必须命中
		// 相同的后端或编辑可能会悄悄地落在不同的后端
		// 存储比代理稍后读取的存储。
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.sessionID, args.Path)
			if err != nil {
				return "", fmt.Errorf("workspace get: %w", err)
			}
			data, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil {
				return "", fmt.Errorf("workspace read: %w", readErr)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.workspaceStore.Put(ctx, r.agentID, r.projectID, r.sessionID, args.Path,
				strings.NewReader(updated), int64(len(updated)), ""); err != nil {
				if friendly := asIsDirToolError("edit_file", args.Path, err); friendly != nil {
					return "", friendly
				}
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
		}

		if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(args.Path) {
			name := filepath.Clean(args.Path)
			uid := r.systemFileUserID(name)
			data, err := r.readSystemFileForUser(ctx, uid, name)
			if err != nil {
				return "", fmt.Errorf("system file get: %w", err)
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, uid, name, []byte(updated)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			// 与 makeWriteFile 相同的磁盘镜像不变式，所以这个 pod 的
			// 进程中的读者（上下文构建器、技能加载器）会看到
			// 立即有新内容。
			if r.systemRoot != "" {
				disk := filepath.Join(r.systemRoot, name)
				_ = os.MkdirAll(filepath.Dir(disk), 0o755)
				_ = os.WriteFile(disk, []byte(updated), 0o644)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", name, count), nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		if isGlobalSkillsPath(fullPath) {
			return "", errGlobalSkillsDirWrite
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("read file: %w", err)
		}
		if looksBinary(data) {
			return binaryRefusal(args.Path, len(data)), nil
		}
		updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(fullPath, []byte(updated), 0o644); err != nil {
			return "", fmt.Errorf("write file: %w", err)
		}
		return fmt.Sprintf("Edited %s (%d replacement(s))", fullPath, count), nil
	}
}

// isSingleSegmentSystemFile 匹配“SOUL.md”、“IDENTITY.md”等 —
// 允许列出的身份文件名，并且仅当写入目标时
// 顶级目录（无斜杠）。嵌套路径如
// “notes/SOUL.md”故意不符合条件。由 WRITE 路径使用
// 过度广泛的匹配会让用户劫持身份行
// 将任意文件放在 /any/path/IDENTITY.md 中。
func isSingleSegmentSystemFile(path string) bool {
	if filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	if strings.ContainsRune(clean, filepath.Separator) {
		return false
	}
	return systemFiles[clean]
}

// basenameIsSystemFile 是宽松的 READ 端变体：它接受
// 绝对路径和嵌套路径，只要 *basename* 是其中之一
// 身份文件名。身份文件是事实的来源
// 系统文件存储（db）；磁盘视图只是一个后备方案。法学硕士
// 经常将裸露的“IDENTITY.md”扩展为完整的主机路径
// 在系统提示符的“工作目录”行中看到 - 没有这个
// 宽松的匹配，这些读取完全绕过存储并错过
// 真实的内容。只读，因此上面的写路径攻击面
// 保持不变。
func basenameIsSystemFile(path string) bool {
	return systemFiles[filepath.Base(filepath.Clean(path))]
}

func makeListDir(r *Registry) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// 工作区存储有一个平键命名空间；我们合成一个“dir
		// 列表”通过过滤列表输出到其代理相关的条目
		// path 位于 args.Path 的前缀下。
		if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(args.Path) {
			objs, err := r.workspaceStore.List(ctx, r.agentID, r.projectID, r.sessionID)
			if err != nil {
				return "", fmt.Errorf("workspace list: %w", err)
			}
			prefix := strings.Trim(filepath.ToSlash(filepath.Clean(args.Path)), "/")
			if prefix == "." {
				prefix = ""
			}
			var sb strings.Builder
			seenDirs := map[string]bool{}
			for _, o := range objs {
				p := filepath.ToSlash(o.Path)
				if prefix != "" && !strings.HasPrefix(p, prefix+"/") && p != prefix {
					continue
				}
				rel := strings.TrimPrefix(p, prefix)
				rel = strings.TrimPrefix(rel, "/")
				if rel == "" {
					continue
				}
				if i := strings.IndexByte(rel, '/'); i >= 0 {
					dirName := rel[:i]
					if !seenDirs[dirName] {
						seenDirs[dirName] = true
						fmt.Fprintf(&sb, "d %s/\n", dirName)
					}
					continue
				}
				fmt.Fprintf(&sb, "f %s (%d bytes)\n", rel, o.Size)
			}
			return sb.String(), nil
		}

		root := r.rootForPath(args.Path)
		fullPath, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), args.Path)
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(fullPath)
		if err != nil {
			return "", fmt.Errorf("read dir: %w", err)
		}

		var sb strings.Builder
		for _, entry := range entries {
			info, _ := entry.Info()
			if entry.IsDir() {
				fmt.Fprintf(&sb, "d %s/\n", entry.Name())
			} else if info != nil {
				fmt.Fprintf(&sb, "f %s (%d bytes)\n", entry.Name(), info.Size())
			} else {
				fmt.Fprintf(&sb, "f %s\n", entry.Name())
			}
		}

		return sb.String(), nil
	}
}

// registerSandboxedFile 重新注册文件工具，以便它们委托给
// sandbox.Executor 用于不属于商店的路径。
//
// 重要提示：身份文件（SOUL.md、USER.md、MEMORY.md...）位于
// “systemFileStore”（云模式下的 Postgres）和工作区工件已上线
// 在“workspaceStore”中。如果我们将每条路径都直接路由到沙箱
// 执行者，代理会对其自己的身份文件执行 404 — 他们只是
// 沙箱 fs 中不存在。镜像商店路由
// 非沙盒路径；仅当没有存储句柄时才调用沙箱执行器
// 路径（绝对路径、“技能/...”、临时脚本等）。这
// 沙箱徽章仅针对执行程序回退路径发出 - store
// 点击故意不标记，因为它们没有在沙箱中运行。
func registerSandboxedFile(r *Registry, ex sandbox.Executor) {
	r.Register("read_file", "Read the contents of a non-memory file. USER.md and MEMORY.md are managed memory resources; use the memory tool to inspect them.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (identity file, workspace-relative, or absolute inside the sandbox)",
			},
		},
		"required": []string{"path"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args readFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		// 身份文件机密性门 - 与主机路径相同。
		// 在系统文件存储查找之前运行，因此永远不会出现喋喋不休的情况
		// 完全到达数据库行。
		if r.managedMemoryFileBlocked(args.Path) {
			return ManagedMemoryFileRefusal, nil
		}
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		// 身份文件（SOUL.md，IDENTITY.md，...）按基本名称路由
		// （宽松）而不是严格的 isSingleSegmentSystemFile
		// 路线供使用。需要单独检查读取情况，以便
		// LLM 发出的绝对路径，如
		// /data/.bkcrab/workspaces/<id>/IDENTITY.md 仍然会访问数据库。
		if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(args.Path) {
			name := filepath.Base(filepath.Clean(args.Path))
			if data, err := r.readSystemFileForUser(ctx, r.systemFileUserID(name), name); err == nil {
				return string(data), nil
			}
			return "", nil // miss → treat as unset (fresh agent)
		}
		switch r.routeFor(args.Path, OpRead) {
		case RouteWorkspaceStore:
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.sessionID, args.Path)
			if err == nil {
				defer rc.Close()
				data, readErr := io.ReadAll(rc)
				if readErr == nil {
					if looksBinary(data) {
						return binaryRefusal(args.Path, len(data)), nil
					}
					return string(data), nil
				}
			}
			// 掉进商店里的沙箱错过了所以一个新写的
			// 将代理放入沙箱中归档（中途，尚未
			// 镜像存储）仍然可读。
			out, err := ex.ReadFile(ctx, args.Path)
			if err == nil && looksBinary([]byte(out)) {
				return binaryRefusal(args.Path, len(out)), nil
			}
			return MetaSandboxPrefix + out, err
		case RouteSkillStore:
			full := filepath.Join(r.skillRoot(), filepath.Clean(args.Path))
			if data, err := os.ReadFile(full); err == nil {
				if looksBinary(data) {
					return binaryRefusal(args.Path, len(data)), nil
				}
				return string(data), nil
			}
			// 掉入沙箱所以预先安装了技能
			// 容器内的 /skills/<name>/ 仍然可以访问。
			out, err := ex.ReadFile(ctx, args.Path)
			if err == nil && looksBinary([]byte(out)) {
				return binaryRefusal(args.Path, len(out)), nil
			}
			return MetaSandboxPrefix + out, err
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path // explicit absolute non-home path
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("host read %s: %w", full, err)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			return string(data), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox (and RouteSystemStore handled above out-of-band)
			out, err := ex.ReadFile(ctx, args.Path)
			if err == nil && looksBinary([]byte(out)) {
				return binaryRefusal(args.Path, len(out)), nil
			}
			return MetaSandboxPrefix + out, err
		}
	})

	r.Register("write_file", "Write content to a non-memory file (creates directories as needed). USER.md and MEMORY.md are managed memory resources; use the memory tool to update them.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "File path (identity file, workspace-relative, or absolute inside the sandbox)",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Content to write",
			},
		},
		"required": []string{"path", "content"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args writeFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("write_file: %w", err)
		}
		if r.managedMemoryFileBlocked(args.Path) {
			return ManagedMemoryFileRefusal, nil
		}
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}
		switch r.routeFor(args.Path, OpWrite) {
		case RouteSystemStore:
			name := filepath.Clean(args.Path)
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.systemFileUserID(name), name, []byte(args.Content)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), name), nil
		case RouteWorkspaceStore:
			if err := r.workspaceStore.Put(ctx, r.agentID, r.projectID, r.sessionID, args.Path,
				strings.NewReader(args.Content), int64(len(args.Content)), ""); err != nil {
				if friendly := asIsDirToolError("write_file", args.Path, err); friendly != nil {
					return "", friendly
				}
				return "", fmt.Errorf("workspace put: %w", err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), args.Path), nil
		case RouteSkillStore:
			// 技能脚手架（技能创建者的`skills/<name>/...`）落地
			// 在 SkillsLoader 扫描 + 镜像到 OSS 的主机磁盘上，而不是
			// 在短暂的沙箱 FS 中。 writeSkillToHost 处理两者。
			full, err := r.writeSkillToHost(ctx, args.Path, args.Content)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), full), nil
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return "", fmt.Errorf("create directory: %w", err)
			}
			if err := os.WriteFile(full, []byte(args.Content), 0o644); err != nil {
				return "", fmt.Errorf("host write %s: %w", full, err)
			}
			return fmt.Sprintf("Written %d bytes to %s", len(args.Content), full), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox
			out, err := ex.WriteFile(ctx, args.Path, args.Content)
			return MetaSandboxPrefix + out, err
		}
	})

	r.Register("list_dir", "List files and directories in a path", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory path (workspace-relative or absolute inside the sandbox)",
			},
		},
		"required": []string{"path"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args listDirArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		switch r.routeFor(args.Path, OpList) {
		case RouteWorkspaceStore:
			objs, err := r.workspaceStore.List(ctx, r.agentID, r.projectID, r.sessionID)
			if err == nil {
				prefix := strings.Trim(filepath.ToSlash(filepath.Clean(args.Path)), "/")
				if prefix == "." {
					prefix = ""
				}
				var sb strings.Builder
				seenDirs := map[string]bool{}
				for _, o := range objs {
					p := filepath.ToSlash(o.Path)
					if prefix != "" && !strings.HasPrefix(p, prefix+"/") && p != prefix {
						continue
					}
					rel := strings.TrimPrefix(p, prefix)
					rel = strings.TrimPrefix(rel, "/")
					if rel == "" {
						continue
					}
					if i := strings.IndexByte(rel, '/'); i >= 0 {
						dirName := rel[:i]
						if !seenDirs[dirName] {
							seenDirs[dirName] = true
							fmt.Fprintf(&sb, "d %s/\n", dirName)
						}
						continue
					}
					fmt.Fprintf(&sb, "f %s (%d bytes)\n", rel, o.Size)
				}
				return sb.String(), nil
			}
			// 存储错误 → 落入沙箱，因此新编写的
			// 仅沙箱目录仍然可列出。
			out, err := ex.ListDir(ctx, args.Path)
			return MetaSandboxPrefix + out, err
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path
			}
			entries, err := os.ReadDir(full)
			if err != nil {
				return "", fmt.Errorf("host list %s: %w", full, err)
			}
			var sb strings.Builder
			for _, e := range entries {
				if e.IsDir() {
					fmt.Fprintf(&sb, "d %s/\n", e.Name())
					continue
				}
				info, ierr := e.Info()
				if ierr != nil {
					fmt.Fprintf(&sb, "f %s\n", e.Name())
					continue
				}
				fmt.Fprintf(&sb, "f %s (%d bytes)\n", e.Name(), info.Size())
			}
			return sb.String(), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox / RouteSystemStore (no list semantics) / RouteSkillStore
			out, err := ex.ListDir(ctx, args.Path)
			return MetaSandboxPrefix + out, err
		}
	})

	r.Register("edit_file", editDescription, editSchema, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args editFileArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if err := validateFileTargetPath(args.Path); err != nil {
			return "", fmt.Errorf("edit_file: %w", err)
		}
		if r.managedMemoryFileBlocked(args.Path) {
			return ManagedMemoryFileRefusal, nil
		}
		if r.identityFileBlocked(args.Path) {
			return IdentityFileRefusal, nil
		}

		// editSandboxRMW 是通过以下方式进行的读取-修改-写入回退
		// 沙箱执行器。当商店路线丢失或任何其他情况时使用
		// 发送到沙箱的路径routeFor。
		editSandboxRMW := func() (string, error) {
			content, err := ex.ReadFile(ctx, args.Path)
			if err != nil {
				return "", err
			}
			if looksBinary([]byte(content)) {
				return binaryRefusal(args.Path, len(content)), nil
			}
			updated, count, err := applyEdit(args.Path, content, args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if _, err := ex.WriteFile(ctx, args.Path, updated); err != nil {
				return "", err
			}
			return MetaSandboxPrefix + fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
		}

		switch r.routeFor(args.Path, OpWrite) {
		case RouteSystemStore:
			name := filepath.Clean(args.Path)
			uid := r.systemFileUserID(name)
			data, err := r.readSystemFileForUser(ctx, uid, name)
			if err != nil {
				return "", fmt.Errorf("system file get: %w", err)
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, uid, name, []byte(updated)); err != nil {
				return "", fmt.Errorf("system file save: %w", err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", name, count), nil
		case RouteWorkspaceStore:
			rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.sessionID, args.Path)
			if err == nil {
				data, readErr := io.ReadAll(rc)
				rc.Close()
				if readErr == nil {
					if looksBinary(data) {
						return binaryRefusal(args.Path, len(data)), nil
					}
					updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
					if err != nil {
						return "", err
					}
					if err := r.workspaceStore.Put(ctx, r.agentID, r.projectID, r.sessionID, args.Path,
						strings.NewReader(updated), int64(len(updated)), ""); err != nil {
						if friendly := asIsDirToolError("edit_file", args.Path, err); friendly != nil {
							return "", friendly
						}
						return "", fmt.Errorf("workspace put: %w", err)
					}
					return fmt.Sprintf("Edited %s (%d replacement(s))", args.Path, count), nil
				}
			}
			// 存储未命中 → 沙箱 RMW，因此是一个新创建的文件（尚未
			// 镜像）仍然是可编辑的。
			return editSandboxRMW()
		case RouteSkillStore:
			// 今天，技能文件没有就地编辑语义；落下
			// 到沙箱 RMW，它可以读取 /skills/<name>/...
			// 从只读挂载并写入（写入将失败
			// 如果挂载是 RO，则在 FS 层；这是正确的错误
			// 到模型表面）。
			return editSandboxRMW()
		case RouteHostFS:
			full, ok := hostHomePath(args.Path)
			if !ok {
				full = args.Path
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return "", fmt.Errorf("host read %s: %w", full, err)
			}
			if looksBinary(data) {
				return binaryRefusal(args.Path, len(data)), nil
			}
			updated, count, err := applyEdit(args.Path, string(data), args.OldString, args.NewString, args.ReplaceAll)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
				return "", fmt.Errorf("host write %s: %w", full, err)
			}
			return fmt.Sprintf("Edited %s (%d replacement(s))", full, count), nil
		case RouteRefuseSuggestSandbox:
			return "", fmt.Errorf("%s", errSandboxRequiredMessage)
		default: // RouteSandbox
			return editSandboxRMW()
		}
	})
}
