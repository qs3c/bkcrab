package agent

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed all:bundled_skills
var bundledSkillsFS embed.FS

// bundledHashFile是记录捆绑树的每技能sidecar
// 最新安装的哈希。它的存在+与磁盘上的匹配
// 树是InstallBundledSkills决定用户是否自定义
// 技能（跳过） ，或者它仍然是已发布的副本（可以安全地覆盖
// 当较新的二进制文件传送更新的捆绑包时）。
const bundledHashFile = ".bundled-hash"

// BundledSkillNames返回嵌入在
// 二进制。暴露，以便启动/重新加载代码可以保护这些条目免受
// object-Store Hydrator的“prune local-only dirs”步骤（捆绑技能
// 不存储在对象存储中；它们始终在启动时重新生成
// 由InstallBundledSkills提供）。
func BundledSkillNames() []string {
	entries, err := fs.ReadDir(bundledSkillsFS, "bundled_skills")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// InstallBundledSkills将bundled_skills/下嵌入的所有技能同步到
// 托管技能目录。荣誉BKCLAW_HOME因此每个产品
// 每个实例都有自己的副本。
//
// 升级行为由每项技能的.bundled-hash sidecar控制：
// -缺少目标→安装新鲜，写入sidecar。
// - Sidecar存在+磁盘上的哈希匹配Sidecar + Sidecar匹配
// 新发货的捆绑包哈希→已经是最新的，无需操作。
// - Sidecar存在+磁盘上的哈希匹配Sidecar +捆绑哈希不同
// → 用户尚未触摸，请安全地用新捆绑包替换树。
// -存在Sidecar ，但磁盘上的哈希发散→用户自定义，跳过。
// -没有sidecar （旧版安装） +磁盘上的哈希发生匹配
// 当前捆绑包→静默地采用Sidecar ，以便将来的升级流程。
// -没有sidecar +磁盘上的哈希不匹配，→无法告诉用户已修改
// 从较旧的捆绑包，保守的跳过。
func InstallBundledSkills() {
	targetDir := managedSkillsDir()
	if targetDir == "" {
		return
	}
	installBundledSkillsTo(bundledSkillsFS, "bundled_skills", targetDir)
}

// installBundledSkillsTo是可测试的核心：从
// 任何根植于srcRoot的fs.FS进入磁盘上的targetDir。生产调用者
// 传递embed.FS;测试传递fstest.MapFS。
func installBundledSkillsTo(srcFS fs.FS, srcRoot, targetDir string) {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		slog.Warn("failed to create bundled skills target", "dir", targetDir, "error", err)
		return
	}

	entries, err := fs.ReadDir(srcFS, srcRoot)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		skillTarget := filepath.Join(targetDir, skillName)
		srcSkillRoot := path.Join(srcRoot, skillName)

		bundleHash, err := embedTreeHash(srcFS, srcSkillRoot)
		if err != nil {
			slog.Warn("failed to hash bundled skill", "name", skillName, "error", err)
			continue
		}

		decision, reason := decideBundledInstall(skillTarget, bundleHash)
		switch decision {
		case decisionFresh:
			if err := copyEmbedTree(srcFS, srcSkillRoot, skillTarget); err != nil {
				slog.Warn("failed to install bundled skill", "name", skillName, "error", err)
				continue
			}
			if err := writeBundledHash(skillTarget, bundleHash); err != nil {
				slog.Warn("failed to write bundled hash sidecar", "name", skillName, "error", err)
			}
			slog.Info("installed bundled skill", "name", skillName, "path", skillTarget)
		case decisionOverwrite:
			// 首先擦除，以便在新捆绑包中删除的文件不会停留。
			if err := os.RemoveAll(skillTarget); err != nil {
				slog.Warn("failed to clear stale bundled skill", "name", skillName, "error", err)
				continue
			}
			if err := copyEmbedTree(srcFS, srcSkillRoot, skillTarget); err != nil {
				slog.Warn("failed to upgrade bundled skill", "name", skillName, "error", err)
				continue
			}
			if err := writeBundledHash(skillTarget, bundleHash); err != nil {
				slog.Warn("failed to write bundled hash sidecar", "name", skillName, "error", err)
			}
			slog.Info("upgraded bundled skill", "name", skillName, "path", skillTarget)
		case decisionAdoptSidecar:
			if err := writeBundledHash(skillTarget, bundleHash); err != nil {
				slog.Warn("failed to write bundled hash sidecar", "name", skillName, "error", err)
				continue
			}
			slog.Info("adopted bundled skill sidecar", "name", skillName, "path", skillTarget)
		case decisionUpToDate:
			// 无所事事
		case decisionUserModified:
			slog.Debug("skipping bundled skill, user-modified", "name", skillName, "path", skillTarget, "reason", reason)
		}
	}
}

type installDecision int

const (
	decisionFresh installDecision = iota
	decisionUpToDate
	decisionOverwrite
	decisionAdoptSidecar
	decisionUserModified
)

// decideBundledInstall分类如何处理一个捆绑技能的
// 给定新计算的捆绑包哈希的目标目录。请参阅
// 用于策略基本原理的InstallBundledSkills。
func decideBundledInstall(targetDir, bundleHash string) (installDecision, string) {
	if _, err := os.Stat(filepath.Join(targetDir, "SKILL.md")); err != nil {
		return decisionFresh, ""
	}
	diskHash, diskErr := diskTreeHash(targetDir)
	if diskErr != nil {
		return decisionUserModified, fmt.Sprintf("disk hash failed: %v", diskErr)
	}
	sidecarHash, sidecarErr := readBundledHash(targetDir)
	if sidecarErr != nil {
		// 预sidecar安装。如果磁盘内容，则静默采用sidecar
		// 已经与当前捆绑包匹配（因此下一次升级流程）。否则
		// 要保守—我们无法区分用户修改和旧包。
		if diskHash == bundleHash {
			return decisionAdoptSidecar, ""
		}
		return decisionUserModified, "no sidecar; disk hash != bundle hash"
	}
	if diskHash != sidecarHash {
		return decisionUserModified, "disk hash != sidecar hash"
	}
	if sidecarHash == bundleHash {
		return decisionUpToDate, ""
	}
	return decisionOverwrite, ""
}

// embedTreeHash按词法顺序哈希根目录下的每个文件，混合
// 相对路径，因此重命名不能伪装成仅限内容的更改。
func embedTreeHash(srcFS fs.FS, root string) (string, error) {
	h := sha256.New()
	err := fs.WalkDir(srcFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := fs.ReadFile(srcFS, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\n%d\n", rel, len(data))
		h.Write(data)
		h.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// diskTreeHash镜像embedTreeHash ，但行走磁盘上的目录。
// 跳过点文件（ .bundled-hash sidecar、.DS_Store等） ，因此
// 磁盘哈希保持带有捆绑哈希的apples-to-apples。
func diskTreeHash(root string) (string, error) {
	h := sha256.New()
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// 跳过任何深度的点文件—捆绑技能不会隐藏
		// 文件，因此磁盘上以“.”开头的任何内容都是我们的
		// sidecar或操作系统噪音（ .DS_Store、编辑器交换文件等）。
		for _, seg := range strings.Split(rel, "/") {
			if strings.HasPrefix(seg, ".") {
				return nil
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\n%d\n", rel, len(data))
		h.Write(data)
		h.Write([]byte{'\n'})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readBundledHash(targetDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(targetDir, bundledHashFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func writeBundledHash(targetDir, hash string) error {
	return os.WriteFile(filepath.Join(targetDir, bundledHashFile), []byte(hash+"\n"), 0o644)
}

// managedSkillsDir是每个BkClaw实例的全局技能位置。
// 在internal/agent/skills.go中镜像bkclawManagedDir ，但保留为本地
// 因此，此文件的唯一依赖项是os/filepath。
func managedSkillsDir() string {
	if h := os.Getenv("BKCLAW_HOME"); h != "" {
		return filepath.Join(h, "skills")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bkclaw", "skills")
}

// copyEmbedTree在embed.FS中遍历src ，并将每个常规文件写入
// 到dst下的对应路径，创建中间目录。
func copyEmbedTree(src fs.FS, srcRoot, dst string) error {
	return fs.WalkDir(src, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
