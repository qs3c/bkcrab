package tools

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkcrab/internal/buildinfo"
)

// RouteTarget 标识哪个后端应处理文件/执行调用。
// 集中于此，因此每个工具都通过相同的决策进行调度
// 每个处理程序开放编码其自己的 if-else 梯子。
type RouteTarget int

const (
	// RouteSandbox 通过注册的 sandbox.Executor 进行调度
	// （例如ReadFile / ex.WriteFile / ex.Exec）。云中和中的默认值
	// local-with-sandbox 模式 — 沙箱是代理所在的位置
	// “当前工作环境”生活，因此写道可以到达
	// 由同一会话中的后续执行人员执行。
	RouteSandbox RouteTarget = iota

	// RouteWorkspaceStore 调度到持久工作区。存储在
	// （代理 ID、项目 ID、会话 ID）。用于相对路径，例如
	// `report.md` 预计聊天内容将比沙箱更长久
	// 容器并在 UI 的文件浏览器中可见。
	RouteWorkspaceStore

	// RouteSystemStore 分派到身份文件存储
	// （SOUL.md / IDENTITY.md / MEMORY.md / …）。云模式下的数据库支持
	// 因此管理 UI 会在 pod 之间看到相同的内容。
	RouteSystemStore

	// RouteSkillStore 将文件写入每用户技能存储桶下
	// (`skills/<name>/...`) 位于主机磁盘上，以便 SkillsLoader 拾取它。
	// 仅与技能创建者脚手架相关。
	RouteSkillStore

	// RouteHostFS 分派到运营商主机上的原始 os.*。仅有的
	// 在本地模式下可达并且当路径是显式主机时
	// 参考 (~/Documents/, /Users/<u>/projects/, …) — 绝不用于
	// 模糊的绝对路径，例如 /tmp/foo，否则会泄漏
	// 沙箱内部划伤到操作员的机器上。
	RouteHostFS

	// RouteRefuseSuggestSandbox 是策略：操作需要沙箱才能运行
	// 安全但未配置沙箱。来电者显示一条消息
	// 告诉用户启用设置→运行时→沙箱。
	RouteRefuseSuggestSandbox
)

// 操作标记正在路由的工作类型，以便应用策略
// 每个操作的处理方式不同（例如，即使在主机上 exec 也是危险的）
// 从同一路径读取就可以了）。
type Operation int

const (
	OpRead Operation = iota
	OpWrite
	OpList
	OpExec
)

// RouteFor 决定当前下哪个后端处理(path, op)
// 部署模式+沙箱可用性。三个高级规则
// （云→沙箱；本地+沙箱→沙箱优先；本地无沙箱→
// host-with-care）住在这里，所以每个需要调度的工具
// 类似路径的 arg 应该调用它而不是发明自己的
// 分类。
func (r *Registry) routeFor(path string, op Operation) RouteTarget {
	// 存储的工件模式始终路由到其专用存储
	// 无论部署模式如何——这些都是持久性问题，
	// 不是“这个命令在哪里运行”问题。
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		return RouteWorkspaceStore
	}
	if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(path) {
		return RouteSystemStore
	}
	if r.isSkillPath(path) && r.skillRoot() != "" {
		return RouteSkillStore
	}

	sandboxOK := r.executor != nil

	// 规则 1：云/托管部署 → 沙箱对于一切都是强制性的
	// 这不属于上面的存储工件路线。如果
	// 操作员忘记配置，拒绝而不是默默
	// 在 pod 的主机文件系统上运行。
	if buildinfo.IsHostedDeploy() {
		if sandboxOK {
			return RouteSandbox
		}
		return RouteRefuseSuggestSandbox
	}

	// 规则 2：已配置沙箱的本地 → 沙箱优先。主机磁盘是
	// 仅可通过显式主机范围路径（操作员的
	// 文档，绝对的 /Users/<u>/... 显然不是
	// 用于升级操作的沙箱内部、bkcrab-内部子树）。
	if sandboxOK {
		if isBkCrabInternalPath(path) {
			return RouteHostFS
		}
		if isExplicitHostScope(path) {
			return RouteHostFS
		}
		return RouteSandbox
	}

	// 规则三：本地无沙箱 → 主机盘是唯一选择，但是
	// 标记危险操作，以便调用者可以建议启用沙箱。
	if isDangerousOnHost(op, path) {
		return RouteRefuseSuggestSandbox
	}
	return RouteHostFS
}

// isExplicitHostScope 清楚地报告路径是否是一个喋喋不休的路径
// 意味着对操作员实际主机文件系统的引用（而不是
// 沙盒容器的同一字符串的视图）。用于本地+沙箱
// 模式允许读/写操作员的真实文件，同时保持
// 路由到沙箱的模糊绝对路径（例如 /tmp/scratch.js）。
//
// 今天的启发式是基于路径前缀的：
// - ~/文档、~/下载、~/桌面、~/项目、~/代码、~/工作
// - /Users/<u>/... 和 /home/<u>/... 不是 bkcrab-internal
// 并且不只是沙箱
//
// 裸 ~/ （没有可识别的用户内容子目录）不是主机范围：
// “~/foo” 比操作员的更可能是沙箱相关的划痕
// 主目录。如果操作员想要在他们的实际家下铺设一条路径，他们
// 可以拼写出来（〜/ Documents / foo，/ Users / mike / code / foo）。
func isExplicitHostScope(path string) bool {
	if isBkCrabInternalPath(path) || isSandboxOnlyPath(path) {
		return false
	}
	if strings.HasPrefix(path, "~/") {
		rest := path[2:]
		for _, prefix := range hostHomeContentDirs {
			if rest == prefix || strings.HasPrefix(rest, prefix+"/") {
				return true
			}
		}
		return false
	}
	if !filepath.IsAbs(path) {
		return false
	}
	if strings.HasPrefix(path, "/Users/") || strings.HasPrefix(path, "/home/") {
		return true
	}
	return false
}

// hostHomeContentDirs 是“这显然
// 表示聊天者的实际主机文件”~下的子目录。保守
// 因为不在此列表中的任何内容都默认为 P2 模式下的沙箱，
// 当有疑问时，这是正确的选择——沙箱是可恢复的，主机
// 磁盘写入则不然。
var hostHomeContentDirs = []string{
	"Documents", "Downloads", "Desktop",
	"projects", "code", "work", "src",
}

// isBkCrabInternalPath 报告路径是否属于 BkCrab 的范围
// 运行时管理的目录（~/.bkcrab/...）。这些有专用路由
// （workspaceStore、身份存储等）和工具不得写入它们
// 通过面向聊天的主机路径，否则它们会破坏内部状态。
func isBkCrabInternalPath(path string) bool {
	if strings.HasPrefix(path, "~/.bkcrab") {
		return true
	}
	if filepath.IsAbs(path) {
		if home, err := os.UserHomeDir(); err == nil {
			bkcrabDir := filepath.Join(home, ".bkcrab")
			if path == bkcrabDir || strings.HasPrefix(path, bkcrabDir+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// isSandboxOnlyPath 报告路径是否仅存在于沙箱内
// 容器——通常是绑定安装目标。主机的绑定源位于
// 不同的位置，因此简单的主机扩展将始终为 404。
//
// - ~/.agents/... ：npx 技能的安装目录（从绑定安装
// 〜/.bkcrab/users/<uid>/skills/)
// - /root/.agents/.. ：相同，通过沙箱解析的绝对路径
func isSandboxOnlyPath(path string) bool {
	if strings.HasPrefix(path, "~/.agents") || strings.HasPrefix(path, "/root/.agents") {
		return true
	}
	return false
}

// isDangerousOnHost 对本地无沙箱的操作进行分类
// 分支机构应该拒绝而不是默默地对运营商做
// 真机。保守：真正的意思是“要求用户打开
// 在执行此操作之前先进行沙箱操作。”返回 false 并不意味着该操作是
// 安全，只是我们愿意委托给主机文件系统
// 现有的路径遏制守卫。
func isDangerousOnHost(op Operation, path string) bool {
	if op != OpExec && op != OpWrite {
		return false
	}
	// 写入系统位置绝对是沙箱领域。
	if filepath.IsAbs(path) {
		for _, p := range []string{"/etc", "/usr", "/var", "/bin", "/sbin", "/opt", "/root"} {
			if path == p || strings.HasPrefix(path, p+"/") {
				return true
			}
		}
	}
	return false
}

// errSandboxRequiredMessage 是发出的面向用户的字符串
// RouteFor 返回 RouteRefuseSuggestSandbox。措辞如此喋喋不休
// 知道该操作是出于安全考虑而被拒绝，而不是因为能力而被拒绝。
const errSandboxRequiredMessage = "This operation needs a sandbox to run safely, but none is configured. Ask the operator to enable Settings → Runtime → Sandbox, then retry."
