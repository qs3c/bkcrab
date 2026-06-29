package skills

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkcrab/internal/workspace"
)

// 技能包是需要存在于本地磁盘上的静态文件树，供 SkillsLoader 发现，
// 但在多 Pod 部署中磁盘是 Pod 本地的。为了确保跨副本的安装一致性，
// 我们将每个已安装的技能镜像到共享对象存储中，并在启动/重新加载时
// 将其回填到每个 Pod 的磁盘上。
//
// 工作空间存储桶下的键布局：
//
//	<owner>/skills/<skillName>/<relFile>
//
// Owner 是每个 Agent 技能的 Agent ID，或者是全局技能目录
// （`~/.bkcrab/skills/`）的 GlobalSkillOwner。
const (
	// GlobalSkillOwner 是用作对象存储中全局安装技能前缀的合成 "agent ID"。
	// 真实 Agent 不会与之冲突，因为 Agent 名称被验证为小写字母数字加连字符
	//（参见 setup/handlers_agents.go:agentNameRE），
	// 因此前导下划线使此命名空间保持独立。
	GlobalSkillOwner = "_global"

	// skillsKeyPrefix 将每个技能对象限定在 owner 下。每个
	// 调用者必须通过 buildKey 来保持一致。
	skillsKeyPrefix = "skills"

	// userSkillOwnerPrefix 是每个用户伪 owner 键的前导标记
	//（`_user_<uid>`）。与 GlobalSkillOwner 相同的前导下划线约定，
	// 因此它永远不会与真实 Agent ID 冲突。
	userSkillOwnerPrefix = "_user_"
)

// UserSkillOwner 返回聊天者每个用户技能的工作空间.Store 伪 owner 键。
// 空的 userID 返回 ""，以便调用者可以在传统/单用户安装上短路。
func UserSkillOwner(userID string) string {
	if userID == "" {
		return ""
	}
	return userSkillOwnerPrefix + userID
}

func buildKey(skillName, relPath string) string {
	// relPath 已经规范化（当我们通过此辅助函数时，filepath.Walk 通过 filepath.ToSlash 产生正斜杠等价物）。
	rel := strings.TrimPrefix(filepath.ToSlash(relPath), "/")
	return skillsKeyPrefix + "/" + skillName + "/" + rel
}

// SyncSkillUp 将 <rootDir>/<skillName>/ 下的每个文件上传到
// 对象存储中的 <owner>/skills/<skillName>/。符号链接被跟随
// （os.Lstat 过滤器排除它们以避免重复目标）。每次安装后调用是安全的；
// 现有键会被覆盖。
func SyncSkillUp(ctx context.Context, ws workspace.Store, owner, skillName, rootDir string) error {
	if ws == nil {
		return nil // 未配置对象存储 — 无需镜像
	}
	skillDir := filepath.Join(rootDir, skillName)
	info, err := os.Stat(skillDir)
	if err != nil {
		return fmt.Errorf("stat skill dir %s: %w", skillDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("skill path %s is not a directory", skillDir)
	}

	uploaded := 0
	walkErr := filepath.Walk(skillDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // 跳过符号链接
		}
		rel, relErr := filepath.Rel(skillDir, path)
		if relErr != nil {
			return relErr
		}
		f, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer f.Close()

		key := buildKey(skillName, rel)
		// 技能位于 Agent 共享作用域中（project 和 session 均为空），
		// 因此 Agent 的每次聊天都会看到相同的集合；每个作用域
		// 的子树保留给聊天产物。
		if putErr := ws.Put(ctx, owner, "", "", key, f, info.Size(), ""); putErr != nil {
			return fmt.Errorf("put %s: %w", key, putErr)
		}
		uploaded++
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	slog.Info("skill mirrored to object store",
		"owner", owner, "skill", skillName, "files", uploaded)
	return nil
}

// MirrorSkillsUp 上传 rootDir 下名称在对象存储中（owner 下）不存在的
// 任何本地技能子目录。与 HydrateSkillsDown 配对使用：
// hydrate-down 在 LoadSkills 入口处将远程→本地同步，
// mirror-up 捕获 Agent 刚才在本地写入的内容
// （通常是 `npx skills add -g -y` 到每个用户的绑定挂载中）并推送，
// 以便兄弟 Pod 在下一个水合周期中看到。
//
// 跳过标准是"远程不存在技能名称"，而不是逐文件比较，
// 因此已存在于远程的技能被视为权威远程（不重新上传，不覆盖）。
// 没有 SKILL.md 的半安装目录会被跳过，以避免在安装过程中上传部分状态。
func MirrorSkillsUp(ctx context.Context, ws workspace.Store, owner, rootDir string) error {
	if ws == nil || owner == "" {
		return nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return fmt.Errorf("list object store skills for %s: %w", owner, err)
	}
	prefix := skillsKeyPrefix + "/"
	remoteSkills := make(map[string]bool)
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Path, prefix)
		if slash := strings.IndexByte(rest, '/'); slash > 0 {
			remoteSkills[rest[:slash]] = true
		}
	}
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		// 缺少本地目录 == 无需镜像；不是错误。
		return nil
	}
	uploaded := 0
	for _, e := range entries {
		if !e.IsDir() || remoteSkills[e.Name()] {
			continue
		}
		// 仅上传看起来已完全安装的目录。没有此保护，
		// 并发的 npx 安装可能会推送一个未完全写入的目录树。
		if _, statErr := os.Stat(filepath.Join(rootDir, e.Name(), "SKILL.md")); statErr != nil {
			continue
		}
		if upErr := SyncSkillUp(ctx, ws, owner, e.Name(), rootDir); upErr != nil {
			slog.Warn("mirror skill up failed", "owner", owner, "skill", e.Name(), "error", upErr)
			continue
		}
		uploaded++
	}
	if uploaded > 0 {
		slog.Info("mirrored new local skills to object store", "owner", owner, "count", uploaded)
	}
	return nil
}

// DeleteSkillUp 删除 <owner>/skills/<skillName>/ 下的所有对象。
// 缺失的键会被容忍。
func DeleteSkillUp(ctx context.Context, ws workspace.Store, owner, skillName string) error {
	if ws == nil {
		return nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return fmt.Errorf("list skills for %s: %w", owner, err)
	}
	prefix := skillsKeyPrefix + "/" + skillName + "/"
	removed := 0
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		if err := ws.Delete(ctx, owner, "", "", o.Path); err != nil {
			if errors.Is(err, workspace.ErrNotFound) {
				continue
			}
			return fmt.Errorf("delete %s: %w", o.Path, err)
		}
		removed++
	}
	slog.Info("skill removed from object store",
		"owner", owner, "skill", skillName, "files", removed)
	return nil
}

// HydrateSkillsDown 将 <owner>/skills/ 下的每个技能对象镜像到
// <rootDir>/，以便 SkillsLoader（文件系统扫描器）看到与对象存储相同的技能集合。
//
// 双向协调：
//  1. 对于每个远程键，创建/覆盖本地文件（大小匹配时跳过 — 廉价的重入防护）。
//  2. 对于每个没有远程键的本地顶级技能目录，将其删除。
//     这就是将删除操作从 Pod A 传播到 Pod B 的方式。
//
// `keepLocal` 是一个允许列表，其中的技能文件夹名称无论远程状态如何
// 都不会被修剪。全局技能目录使用此列表来保护捆绑技能
// （从启动时的嵌入 FS 安装，从未上传到对象存储）。
// 对于每个 Agent 的目录，传递 nil。
//
// 幸存的技能内的文件级差异（远程从捆绑包中删除了一个文件）不会被协调；
// 技能在安装时被整体替换，因此实践中不应发生逐文件漂移。
func HydrateSkillsDown(ctx context.Context, ws workspace.Store, owner, rootDir string, keepLocal ...string) error {
	if ws == nil {
		return nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return fmt.Errorf("list object store skills for %s: %w", owner, err)
	}
	prefix := skillsKeyPrefix + "/"

	// 远程视图：存储中存在哪些技能名称目录。
	remoteSkills := make(map[string]bool)
	fetched := 0
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Path, prefix)
		slash := strings.IndexByte(rest, '/')
		if slash > 0 {
			remoteSkills[rest[:slash]] = true
		}

		target := filepath.Join(rootDir, filepath.FromSlash(rest))
		if existing, statErr := os.Stat(target); statErr == nil && !existing.IsDir() {
			// 相同大小 → 已水合。我们在每次安装时覆盖远程键，
			// 因此内容变化时大小也会变化；真正的校验和匹配需要额外的 HEAD/ETag，
			// 对于静态技能包来说很少值得这样做。
			if o.Size >= 0 && existing.Size() == o.Size {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
		}
		rc, err := ws.Get(ctx, owner, "", "", o.Path)
		if err != nil {
			if errors.Is(err, workspace.ErrNotFound) {
				continue
			}
			return fmt.Errorf("get %s: %w", o.Path, err)
		}
		f, err := os.Create(target)
		if err != nil {
			rc.Close()
			return fmt.Errorf("create %s: %w", target, err)
		}
		if _, err := io.Copy(f, rc); err != nil {
			f.Close()
			rc.Close()
			return fmt.Errorf("copy %s: %w", target, err)
		}
		f.Close()
		rc.Close()
		fetched++
	}

	// 协调删除：本地存在但远程列表中不存在的任何顶级技能目录
	// 已在另一个 Pod 上被删除 — 删除它，以便此 Pod 的 SkillsLoader
	// 停止返回过期条目。`keepLocal` 保护捆绑技能（嵌入在二进制文件中，
	// 从未镜像到 OSS）在首次返回空 OSS 列表时不被清除。
	//
	// 安全说明：当远程有零个技能对象时，该列表与"OSS 配置错误"或
	// "仅安装文件系统技能的全新安装"无法区分。在这种情况下进行修剪
	// 是破坏性的 — 它会删除操作员放入 BKCRAB_HOME/skills/ 中的
	// 每个本地技能（对于完全不使用 OSS 的产品 Agent）。
	// 除非远程权威地至少有一个技能，否则完全跳过修剪，
	// 这是唯一"远程缺失"具有含义的状态。
	keep := make(map[string]bool, len(keepLocal))
	for _, name := range keepLocal {
		keep[name] = true
	}
	removed := 0
	if entries, err := os.ReadDir(rootDir); err == nil && len(remoteSkills) > 0 {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if remoteSkills[e.Name()] || keep[e.Name()] {
				continue
			}
			if err := os.RemoveAll(filepath.Join(rootDir, e.Name())); err != nil {
				slog.Warn("failed to prune stale local skill",
					"owner", owner, "skill", e.Name(), "error", err)
				continue
			}
			removed++
		}
	}

	if fetched > 0 || removed > 0 {
		slog.Info("skills reconciled with object store",
			"owner", owner, "dir", rootDir, "fetched", fetched, "pruned", removed)
	}
	return nil
}

// ListRemoteSkillNames 返回对象存储中 <owner>/skills/ 下存在的
// 唯一技能文件夹名称。用于管理 UI 可以显示 Agent 拥有的所有技能，
// 即使此 Pod 尚未将其水合。
func ListRemoteSkillNames(ctx context.Context, ws workspace.Store, owner string) ([]string, error) {
	if ws == nil {
		return nil, nil
	}
	objs, err := ws.List(ctx, owner, "", "")
	if err != nil {
		return nil, err
	}
	prefix := skillsKeyPrefix + "/"
	seen := make(map[string]bool)
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(o.Path, prefix)
		slash := strings.IndexByte(rest, '/')
		if slash <= 0 {
			continue
		}
		seen[rest[:slash]] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out, nil
}
