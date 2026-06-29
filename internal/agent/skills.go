package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/workspace"
	"gopkg.in/yaml.v3"
)

// Skill 表示一个已发现的技能。
type Skill struct {
	Name        string         // 目录名称
	Layer       string         // "agent", "user", "managed", "bundled", "extra"
	Content     string         // 可选的内联 SKILL.md 内容，用于始终加载的技能
	BaseDir     string         // 技能目录的绝对路径
	Description string         // 来自 frontmatter
	Metadata    *SkillMetadata // 解析后的 OpenClaw 元数据
	Gated       bool           // 如果门控要求未满足则为 true
	GateReason  string         // 门控失败的原因
}

// SkillFrontmatter 表示 SKILL.md 文件的 YAML frontmatter。
//
// Env 是声明可配置环境变量的便捷快捷方式——等同于将它们写在
// metadata.bkcrab.env 下，但免去了技能作者在不需要将技能发布到
// 非 bkcrab 运行时时的命名空间嵌套。HTTP 层合并两个来源，
// 冲突时顶级 Env 优先。
type SkillFrontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Homepage    string         `yaml:"homepage"`
	Env         []SkillEnvSpec `yaml:"env"`
	Metadata    yaml.Node      `yaml:"metadata"`
}

// SkillMetadata 表示技能元数据块。
// 支持 "bkcrab" 和 "openclaw" 两种键以实现向后兼容。
type SkillMetadata struct {
	BkCrab   *OpenClawMeta `json:"bkcrab"`
	OpenClaw *OpenClawMeta `json:"openclaw"`
}

// Meta 返回有效的元数据，bkcrab 优先于 openclaw。
func (m *SkillMetadata) Meta() *OpenClawMeta {
	if m.BkCrab != nil {
		return m.BkCrab
	}
	return m.OpenClaw
}

// OpenClawMeta 持有 OpenClaw 特定的元数据。
type OpenClawMeta struct {
	Emoji      string         `json:"emoji"`
	Homepage   string         `json:"homepage"`
	Always     bool           `json:"always"`
	OS         []string       `json:"os"`
	Requires   *SkillRequires `json:"requires"`
	PrimaryEnv string         `json:"primaryEnv"`
	// Env 声明此技能读取的可配置环境变量。
	// 暴露给管理 UI，使运维人员获得带标签的输入（带有帮助文本和密钥掩码），
	// 而不必 grep main.py 中的 os.environ.get() 调用。PrimaryEnv 保留
	// 作为旧版"单个 API 密钥"便捷路径；像 image-tool 这样的多提供商技能
	// 在此列出所有内容。
	Env     []SkillEnvSpec  `json:"env,omitempty"`
	Install json.RawMessage `json:"install"`
}

// SkillEnvSpec 描述一个可配置的环境变量。除 Name 外的所有字段都是
// 可选的。当名称匹配 /KEY|TOKEN|SECRET|PASSWORD/i 时，Secret 在 UI
// 层默认为 true，因此作者通常不必设置它。
//
// 同时携带 json 和 yaml 标签，使其可以通过 metadata.bkcrab.env 路径
// （yaml→generic→json→struct，json 标签）以及通过新的顶级
// frontmatter.Env 快捷方式（yaml→struct 直接，yaml 标签）往返。
type SkillEnvSpec struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty" yaml:"secret,omitempty"`
}

// SkillRequires 持有门控要求。
type SkillRequires struct {
	Bins    []string `json:"bins"`
	AnyBins []string `json:"anyBins"`
	Env     []string `json:"env"`
	Config  []string `json:"config"`
}

// SkillsLoader 从多层发现并合并技能，具有 OpenClaw 兼容性。
type SkillsLoader struct {
	homeDir   string
	agentDir  string
	teamDir   string
	skillsCfg config.SkillsConfig
	globalCfg config.SkillsCfg
	// workspaceStore 是可选的：设置后，LoadSkills 在扫描文件系统之前
	// 会从对象存储中水合全局和代理技能目录。没有这个，一个技能在 Pod 的
	// UserSpace 被缓存后上传到存储中，对该 Pod 是不可见的，直到重启
	// ——并且对未处理上传的副本完全不可见。
	workspaceStore workspace.Store
	agentID        string
	// userID 是聊天者。设置后，LoadSkills 还会扫描每个用户的技能目录
	// （~/.bkcrab/users/<uid>/skills/），使用户在与任何代理聊天时创建的
	// 技能可以在他们与之聊天的每个其他代理上重复使用。空值禁用此层
	// （早于每个用户技能的旧版/单用户安装）。
	userID string
}

// NewSkillsLoader 创建一个新的技能加载器。
func NewSkillsLoader(homeDir, agentDir, teamDir string, skillsCfg config.SkillsConfig) *SkillsLoader {
	return &SkillsLoader{
		homeDir:   homeDir,
		agentDir:  agentDir,
		teamDir:   teamDir,
		skillsCfg: skillsCfg,
	}
}

// NewSkillsLoaderWithGlobal 创建一个带有全局 SkillsCfg 的技能加载器，用于环境变量注入和条目。
func NewSkillsLoaderWithGlobal(homeDir, agentDir, teamDir string, skillsCfg config.SkillsConfig, globalCfg config.SkillsCfg) *SkillsLoader {
	sl := NewSkillsLoader(homeDir, agentDir, teamDir, skillsCfg)
	sl.globalCfg = globalCfg
	return sl
}

// WithObjectStore 连接 workspace.Store + agentID，使 LoadSkills 在扫描
// 文件系统之前从对象存储中水合技能。返回加载器以支持链式调用。
func (sl *SkillsLoader) WithObjectStore(ws workspace.Store, agentID string) *SkillsLoader {
	sl.workspaceStore = ws
	sl.agentID = agentID
	return sl
}

// WithUserID 启用每个用户的技能层（~/.bkcrab/users/<uid>/skills）。
// 与 WithObjectStore 一起设置时，水合还会拉取用户的伪所有者命名空间，
// 使在另一个 Pod 上创建的技能镜像到此 Pod 的磁盘。空 userID 禁用此层。
func (sl *SkillsLoader) WithUserID(userID string) *SkillsLoader {
	sl.userID = userID
	return sl
}

// LoadSkills 从所有层发现技能并返回合并后的结果。
// 优先级：代理工作空间 > 用户安装 > 托管 > 额外目录。
func (sl *SkillsLoader) LoadSkills() []Skill {
	// 将对象存储中的技能镜像到本地文件系统，使上传到 OSS（或在另一个
	// 副本上安装）的技能在此轮次可见——而不是在下次 Pod 重启后。廉价的
	// 幂等水合；存储按对象执行"大小匹配则跳过"。
	if sl.workspaceStore != nil {
		ctx := context.Background()
		managedDir := bkcrabManagedDir()
		if managedDir != "" {
			keep := BundledSkillNames()
			if err := skills.HydrateSkillsDown(ctx, sl.workspaceStore, skills.GlobalSkillOwner, managedDir, keep...); err != nil {
				slog.Warn("global skill hydrate failed", "error", err)
			}
		}
		if sl.agentID != "" && sl.agentDir != "" {
			agentSkills := filepath.Join(sl.agentDir, "skills")
			if err := skills.HydrateSkillsDown(ctx, sl.workspaceStore, sl.agentID, agentSkills); err != nil {
				slog.Warn("agent skill hydrate failed", "error", err)
			}
		}
		// 每个用户的技能桶：在聊天者使用的每个代理之间共享，与其他聊天者隔离。
		// 使用户在代理 A 上沉淀的实用技能可在代理 B 上使用，而不会污染代理
		// 所有者的官方技能集。
		if userDir := sl.userSkillsDir(); userDir != "" {
			owner := skills.UserSkillOwner(sl.userID)
			if err := skills.HydrateSkillsDown(ctx, sl.workspaceStore, owner, userDir); err != nil {
				slog.Warn("user skill hydrate failed", "user", sl.userID, "error", err)
			}
		// 代理在聊天过程中安装到绑定挂载的每个用户目录中的任何内容
		// （例如通过 `npx skills add -g -y`）此时仅为本地。将其推送到上层，
		// 使兄弟 Pod 在下一个水合周期中获取它。没有新内容时成本很低。
			if err := skills.MirrorSkillsUp(ctx, sl.workspaceStore, owner, userDir); err != nil {
				slog.Warn("user skill mirror-up failed", "user", sl.userID, "error", err)
			}
		}
	}

	skillsMap := make(map[string]Skill)

	disabled := make(map[string]bool, len(sl.skillsCfg.Disabled))
	for _, name := range sl.skillsCfg.Disabled {
		disabled[name] = true
	}
	// 也检查全局条目中 enabled: false 的项
	for name, entry := range sl.globalCfg.Entries {
		if !entry.Enabled {
			disabled[name] = true
		}
	}

	// 第 4 层（最低）：来自配置的额外目录
	for _, dir := range sl.globalCfg.Load.ExtraDirs {
		dir = expandPath(dir)
		for name, skill := range discoverSkillsEnhanced(dir, "extra") {
			if !disabled[name] {
				skillsMap[name] = skill
			}
		}
	}

	// 第 3 层：托管技能（~/.bkcrab/skills/）
	managedDir := bkcrabManagedDir()
	for name, skill := range discoverSkillsEnhanced(managedDir, "managed") {
		if !disabled[name] {
			skillsMap[name] = skill
		}
	}

	// 第 2 层：用户安装（~/.bkcrab/skills/）
	userDir := filepath.Join(sl.homeDir, "skills")
	for name, skill := range discoverSkillsEnhanced(userDir, "user") {
		if !disabled[name] {
			skillsMap[name] = skill
		}
	}

	// 第 1.5 层：团队技能
	if sl.teamDir != "" {
		teamSkillsDir := filepath.Join(sl.teamDir, "skills")
		for name, skill := range discoverSkillsEnhanced(teamSkillsDir, "team") {
			if !disabled[name] {
				skillsMap[name] = skill
			}
		}
	}

	// 第 1.3 层：每个用户的技能（此聊天者的个人桶）。
	// 位于代理（所有者策划的）之下，使代理的官方技能可以覆盖用户的
	// 同名工具，但位于团队/主机之上，使用户自己的技能始终优先于通用安装。
	if userDir := sl.userSkillsDir(); userDir != "" {
		for name, skill := range discoverSkillsEnhanced(userDir, "personal") {
			if !disabled[name] {
				skillsMap[name] = skill
			}
		}
	}

	// 第 1 层（最高）：代理工作空间技能
	agentSkillsDir := filepath.Join(sl.agentDir, "skills")
	for name, skill := range discoverSkillsEnhanced(agentSkillsDir, "agent") {
		if !disabled[name] {
			skillsMap[name] = skill
		}
	}

	// 应用门控和环境变量注入
	result := make([]Skill, 0, len(skillsMap))
	for _, s := range skillsMap {
		if s.Gated {
			slog.Debug("skill gated", "name", s.Name, "reason", s.GateReason)
			continue
		}
		result = append(result, s)
	}
	// 按名称排序，使系统提示中的技能顺序在轮次之间保持稳定。
	// Go map 迭代是随机的，所以没有这个，一个 122KB 的摘要会在每次刷新时
	// 以不同的位置排列技能——模型对块的位置很敏感（后面的块与更多前面的
	// 上下文竞争注意力），这在长尾群聊中产生了间歇性的"模型看不到技能 X"
	// 症状。字母顺序是最便宜的稳定顺序，也使日志差异比较变得简单。
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result
}

// BuildSkillsSummary 返回系统提示中的技能部分。
// 技能默认使用渐进式披露：将提示的常开上下文保持在较小的名称+描述目录，
// 让模型在需要完整 SKILL.md 指令时调用 load_skill。显式的始终加载技能
// 保持内联，以便与必须在第一次工具调用之前存在的技能兼容。
func (sl *SkillsLoader) BuildSkillsSummary(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(skillsDirective)
	alwaysLoad := sl.alwaysLoadSet()
	inline := make([]Skill, 0)

	sb.WriteString("\n\n<skill_catalog>\nPre-installed skills available to this agent. Treat any user mention of one of these names as a request to use that skill. Call `load_skill` with the skill name before following its detailed instructions:\n")
	for _, skill := range skills {
		desc := firstSentence(skill.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&sb, "- %s — %s\n", skill.Name, desc)
		if alwaysLoad[skill.Name] || skillAlwaysLoads(skill) {
			inline = append(inline, skill)
		}
	}
	sb.WriteString("</skill_catalog>")

	if len(inline) > 0 {
		sb.WriteString("\n\n<always_loaded_skills>\n")
		for _, skill := range inline {
			content := skill.Content
			if content == "" {
				content = loadSkillContent(skill.BaseDir)
			}
			fmt.Fprintf(&sb, "<skill name=%q layer=%q>\n%s\n</skill>\n", skill.Name, skill.Layer, content)
		}
		sb.WriteString("</always_loaded_skills>")
	}
	return sb.String()
}

func (sl *SkillsLoader) alwaysLoadSet() map[string]bool {
	out := make(map[string]bool, len(sl.skillsCfg.AlwaysLoad)+len(sl.globalCfg.AlwaysLoad))
	for _, name := range sl.skillsCfg.AlwaysLoad {
		if name != "" {
			out[name] = true
		}
	}
	for _, name := range sl.globalCfg.AlwaysLoad {
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func skillAlwaysLoads(skill Skill) bool {
	return skill.Metadata != nil && skill.Metadata.Meta() != nil && skill.Metadata.Meta().Always
}

// firstSentence 返回 s 的第一个句子片段——用于技能目录的一行摘要。
// 我们对输出进行限制以保持目录的可扫描性，即使技能的 Description 是一个
// 段落：在第一个 ". " / "。" / 换行处截断，然后硬限制在 140 个字符，
// 使单个长句不会淹没索引。修剪空白。
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, sep := range []string{"\n", ". ", "。"} {
		if i := strings.Index(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.TrimSpace(s)
	const cap = 140
	if r := []rune(s); len(r) > cap {
		s = string(r[:cap]) + "…"
	}
	return s
}

// skillsDirective 告诉 LLM 如何调用预安装的技能，以及在内联集合不覆盖
// 请求时在回退到临时代码之前尝试什么。触发条件是具体的——"在通过 exec
// 安装任何包之前"——而不是抽象的（"当没有技能覆盖它时"），因为抽象的
// 框架给了模型一个简单的合理化解释（"这是一次性的，跳过梯子"），导致
// 对已发布技能能处理的任务产生反射性的 `pip install` 调用。
const skillsDirective = `<skill_usage_rules>
The skills listed below are pre-installed for this agent. Only the catalog is always in context. Before using a skill, call the "load_skill" tool with its name to load the full SKILL.md instructions, then follow those instructions exactly. If an always-loaded skill is included inline below, you may use those inline instructions directly.

The sandbox image already has: python3 + pip, uv + uvx, node + npm + npx, the camoufox-cli anti-detect browser (run as ` + "`camoufox-cli open <url>`" + ` then ` + "`camoufox-cli snapshot -i`" + ` for refs; Camoufox/Firefox is pre-downloaded), git, curl, requests / pillow / beautifulsoup4 / lxml. DO NOT reinstall any of these — wasted tool calls and timeouts. If you see "command not found", check the spelling before reaching for npm/pip.

HTML preview: when the user asks to see / preview a web artifact, write the final HTML into the workspace and tell them the filename — the chat UI auto-renders .html files in a sandboxed iframe (CSS, JS, images, fonts work; cross-origin fetch from null origin does not). For source projects with a package.json (React, Vue, Vite, Next, …), run the project's build first (` + "`pnpm i && pnpm build`" + ` or the documented command) and point at the resulting ` + "`dist/index.html`" + ` (or equivalent). Live dev servers (` + "`vite dev`" + `, ` + "`next dev`" + `, ` + "`npm run dev`" + `) are NOT reachable from the browser — do not start them; they will hang and waste turns.

When the listed skills don't cover what the user asked for, follow this order BEFORE running any package install (pip / npm / apt / brew / cargo / gem / go install / …) via exec:

1. If a "find-skills" skill is listed above, run it FIRST to search the open skill ecosystem. If a credible match exists, surface it and offer to install it instead of installing the package yourself.
2. If no published skill fits, use "skill-creator" (if listed) to scaffold a new skill under skills/<name>/, then invoke it. Prefer this over inline scripts whenever the user might ask the same kind of thing again.
3. Only if find-skills found nothing AND skill-creator isn't appropriate (e.g. truly one-time throwaway like printing the date), fall through to the direct package install.

Skipping step 1 to "save time" is not allowed — it costs one tool call and prevents reinventing wheels the community has already published.
</skill_usage_rules>`

// SkillEnvVars 从全局配置返回特定技能的环境变量。
func (sl *SkillsLoader) SkillEnvVars(skillName string) map[string]string {
	// 每个代理的覆盖优先。仅当代理没有自己的条目或有条目但为空时
	// 才回退到全局条目（这样运维人员不必为了保持相同的默认值而将
	// 全局配置复制到每个代理）。
	var entry config.SkillEntryCfg
	var found bool
	if sl.agentID != "" {
		if agentMap, ok := sl.globalCfg.AgentEntries[sl.agentID]; ok {
			if e, ok := agentMap[skillName]; ok && (e.APIKey != "" || len(e.Env) > 0) {
				entry = e
				found = true
			}
		}
	}
	if !found {
		entry, found = sl.globalCfg.Entries[skillName]
	}
	slog.Info("SkillEnvVars lookup",
		"skillName", skillName,
		"loaderAgentID", sl.agentID,
		"agentEntriesKeys", mapKeys(sl.globalCfg.AgentEntries),
		"entriesKeys", entryKeys(sl.globalCfg.Entries),
		"found", found,
		"entryEnvCount", len(entry.Env))
	if !found {
		return nil
	}
	env := make(map[string]string, len(entry.Env)+1)
	for k, v := range entry.Env {
		env[k] = v
	}
	// 如果设置了 apiKey 且技能有 primaryEnv，则注入它
	if entry.APIKey != "" {
		// 找到技能以获取 primaryEnv
		// 这是一个便利功能——技能的 primaryEnv 告诉我们 apiKey 映射到哪个环境变量
		for _, dir := range sl.allSkillDirs() {
			skillDir := filepath.Join(dir, skillName)
			fm := parseFrontmatter(filepath.Join(skillDir, "SKILL.md"))
			if fm != nil && fm.Metadata.Kind == yaml.MappingNode {
				meta := parseMetadata(&fm.Metadata)
				if meta != nil && meta.Meta() != nil && meta.Meta().PrimaryEnv != "" {
					env[meta.Meta().PrimaryEnv] = entry.APIKey
					break
				}
			}
		}
	}
	return env
}

// AllSkillDirs 按优先级顺序返回所有技能目录。
func (sl *SkillsLoader) AllSkillDirs() []string {
	return sl.allSkillDirs()
}

func (sl *SkillsLoader) allSkillDirs() []string {
	var dirs []string
	dirs = append(dirs, filepath.Join(sl.agentDir, "skills"))
	if sl.teamDir != "" {
		dirs = append(dirs, filepath.Join(sl.teamDir, "skills"))
	}
	if userDir := sl.userSkillsDir(); userDir != "" {
		dirs = append(dirs, userDir)
	}
	dirs = append(dirs, filepath.Join(sl.homeDir, "skills"))
	dirs = append(dirs, bkcrabManagedDir())
	dirs = append(dirs, sl.globalCfg.Load.ExtraDirs...)
	return dirs
}

// userSkillsDir 返回 ~/.bkcrab/users/<uid>/skills（支持 BKCRAB_HOME）。
// 未设置 userID 时返回空，使加载器在单用户安装/旧版路径上完全跳过此层。
func (sl *SkillsLoader) userSkillsDir() string {
	if sl.userID == "" {
		return ""
	}
	base := bkcrabBaseDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "users", sl.userID, "skills")
}

// userSkillsRootDir 是每个用户技能子树的主机父目录（~/.bkcrab/users/<uid>/）。
// 返回的形式末尾不带 "skills/"，以便 file.go 的路径解析器可以像处理代理
// 主目录一样将相对的 "skills/foo/SKILL.md" 与其拼接；SkillsLoader 层通过
// userSkillsDir（附加了 "skills/"）到达实际的子目录。
func userSkillsRootDir(userID string) string {
	if userID == "" {
		return ""
	}
	base := bkcrabBaseDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "users", userID)
}

// discoverSkillsEnhanced 扫描目录中带有 SKILL.md 的技能子目录，
// 解析 frontmatter 并应用门控。它故意不为默认技能在内存中保留完整的
// SKILL.md 内容；模型通过 load_skill 按需加载该内容。
func discoverSkillsEnhanced(dir string, layer string) map[string]Skill {
	result := make(map[string]Skill)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return result
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, entry.Name())
		skillFile := filepath.Join(skillDir, "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue
		}

		absDir, _ := filepath.Abs(skillDir)

		// 解析 frontmatter
		fm := parseFrontmatterFromBytes(data)
		var meta *SkillMetadata
		var desc string
		if fm != nil {
			desc = fm.Description
			if fm.Metadata.Kind == yaml.MappingNode {
				meta = parseMetadata(&fm.Metadata)
			}
		}

		// 应用门控
		gated, gateReason := checkGating(meta)

		name := entry.Name()
		if fm != nil && fm.Name != "" {
			// 使用目录名称作为键，但存储 frontmatter 名称
			_ = fm.Name
		}

		result[name] = Skill{
			Name:        name,
			Layer:       layer,
			BaseDir:     absDir,
			Description: desc,
			Metadata:    meta,
			Gated:       gated,
			GateReason:  gateReason,
		}
	}

	return result
}

func loadSkillContent(skillDir string) string {
	data, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(data))
	return strings.ReplaceAll(content, "{baseDir}", skillDir)
}

func mapKeys(m map[string]map[string]config.SkillEntryCfg) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func entryKeys(m map[string]config.SkillEntryCfg) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// SplitSkillFrontmatter 是 HTTP 使用的导出入口点
// 当需要解析的 frontmatter 和原始正文时
// （例如，退回到第一条正文线以进行描述
// 无前置问题的技能）。当没有时返回 nil + 原始输入
// `---` 需要解析的 frontmatter。
func SplitSkillFrontmatter(data []byte) (*SkillFrontmatter, string) {
	fm := parseFrontmatterFromBytes(data)
	body := string(data)
	if fm == nil {
		return nil, body
	}
	// 从正文中剥离 frontmatter 块，以便调用者看不到
	// 扫描第一个散文行时的 YAML 行。
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "---") {
		rest := trimmed[3:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			after := rest[end+len("\n---"):]
			body = strings.TrimLeft(after, "\n")
		}
	}
	return fm, body
}

// ParseSkillMetadata 是 (yaml.Node →
// SkillMetadata）解码路径。 HTTP 技能列表处理程序使用它来
// 将 envSpec 表面到管理 UI。
func ParseSkillMetadata(node *yaml.Node) *SkillMetadata {
	return parseMetadata(node)
}

// parseFrontmatter 从 SKILL.md 文件路径读取并解析 YAML frontmatter。
func parseFrontmatter(path string) *SkillFrontmatter {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseFrontmatterFromBytes(data)
}

// parseFrontmatterFromBytes 从原始字节解析 YAML frontmatter。
func parseFrontmatterFromBytes(data []byte) *SkillFrontmatter {
	content := string(data)

	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return nil
	}

	// 寻找开局和闭局---
	trimmed := strings.TrimSpace(content)
	rest := trimmed[3:] // skip first ---
	endIdx := strings.Index(rest, "\n---")
	if endIdx < 0 {
		return nil
	}

	fmStr := rest[:endIdx]

	var fm SkillFrontmatter
	if err := yaml.Unmarshal([]byte(fmStr), &fm); err != nil {
		return nil
	}
	return &fm
}

// parseMetadata 将 yaml.Node 元数据转换为我们的 SkillMetadata 结构。
func parseMetadata(node *yaml.Node) *SkillMetadata {
	if node == nil || node.Kind == 0 {
		return nil
	}
	// 编组回 YAML，然后解码为类似 JSON 的结构
	yamlBytes, err := yaml.Marshal(node)
	if err != nil {
		return nil
	}

	// 将 YAML 解组为通用映射，然后编组为 JSON，然后解组为结构
	var raw interface{}
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		return nil
	}

	jsonBytes, err := json.Marshal(convertYAMLToJSON(raw))
	if err != nil {
		return nil
	}

	var meta SkillMetadata
	if err := json.Unmarshal(jsonBytes, &meta); err != nil {
		return nil
	}
	return &meta
}

// ConvertYAMLToJSON 转换 YAML map[string]interface{}（使用 map[interface{}]interface{}）
// 到兼容 JSON 的映射[字符串]接口{}。
func convertYAMLToJSON(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[k] = convertYAMLToJSON(v)
		}
		return result
	case map[interface{}]interface{}:
		result := make(map[string]interface{}, len(val))
		for k, v := range val {
			result[fmt.Sprint(k)] = convertYAMLToJSON(v)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, v := range val {
			result[i] = convertYAMLToJSON(v)
		}
		return result
	default:
		return v
	}
}

// checkGating 验证是否满足技能要求。
// 返回（门控，原因）。 ated=true 表示应该跳过该技能。
func checkGating(meta *SkillMetadata) (bool, string) {
	if meta == nil || meta.Meta() == nil {
		return false, ""
	}
	oc := meta.Meta()

	if oc.Always {
		return false, ""
	}

	// 检查操作系统要求
	if len(oc.OS) > 0 {
		currentOS := runtime.GOOS
		found := false
		for _, os := range oc.OS {
			if os == currentOS {
				found = true
				break
			}
		}
		if !found {
			return true, fmt.Sprintf("requires OS %v, current is %s", oc.OS, currentOS)
		}
	}

	if oc.Requires == nil {
		return false, ""
	}

	// 检查所需的二进制文件
	for _, bin := range oc.Requires.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			return true, fmt.Sprintf("required binary %q not found on PATH", bin)
		}
	}

	// 检查anyBins（至少一个必须存在）
	if len(oc.Requires.AnyBins) > 0 {
		found := false
		for _, bin := range oc.Requires.AnyBins {
			if _, err := exec.LookPath(bin); err == nil {
				found = true
				break
			}
		}
		if !found {
			return true, fmt.Sprintf("none of required binaries %v found on PATH", oc.Requires.AnyBins)
		}
	}

	// 检查所需的环境变量
	for _, envVar := range oc.Requires.Env {
		if os.Getenv(envVar) == "" {
			return true, fmt.Sprintf("required env var %q not set", envVar)
		}
	}

	return false, ""
}

// bkcrabBaseDir 返回 $BKCRAB_HOME 或 $HOME/.bkcrab。用作
// Skills/、users/<uid>/skills/ 等的父级。 荣誉 BKCRAB_HOME
// 因此多实例开发（每个产品一个堆栈）保持隔离。
func bkcrabBaseDir() string {
	if h := os.Getenv("BKCRAB_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".bkcrab")
}

// bkcrabManagedDir 返回 BkCrab 托管技能目录
// （~/.bkcrab/skills/，主机共享）。
func bkcrabManagedDir() string {
	base := bkcrabBaseDir()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "skills")
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// 如果给定路径位于技能目录内，FindSkillForPath 将返回技能名称。
func FindSkillForPath(path string, skillDirs []string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	for _, dir := range skillDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if strings.HasPrefix(absPath, absDir+string(filepath.Separator)) {
			// 提取技能名称（技能目录后的第一个组成部分）
			rel, err := filepath.Rel(absDir, absPath)
			if err != nil {
				continue
			}
			parts := strings.SplitN(rel, string(filepath.Separator), 2)
			if len(parts) > 0 {
				return parts[0]
			}
		}
	}
	return ""
}
