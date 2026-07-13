package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/qs3c/bkcrab/internal/privacy"
)

type ManagerConfig struct {
	MaxContentChars     int
	MaxDescriptionChars int
	MaxSlugChars        int
}

func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		MaxContentChars:     100_000,
		MaxDescriptionChars: 1024,
		MaxSlugChars:        64,
	}
}

func normalizeManagerConfig(cfg ManagerConfig) ManagerConfig {
	def := DefaultManagerConfig()
	if cfg.MaxContentChars == 0 {
		cfg.MaxContentChars = def.MaxContentChars
	}
	if cfg.MaxDescriptionChars == 0 {
		cfg.MaxDescriptionChars = def.MaxDescriptionChars
	}
	if cfg.MaxSlugChars == 0 {
		cfg.MaxSlugChars = def.MaxSlugChars
	}
	return cfg
}

type Manager struct {
	root   string
	config ManagerConfig
}

func NewManager(root string, cfg ManagerConfig) *Manager {
	return &Manager{root: root, config: normalizeManagerConfig(cfg)}
}

// RootDir returns the directory this manager owns. It is exposed only inside
// the repository so the learner tool can mirror the exact managed tree to the
// dedicated object-store namespace without reconstructing paths independently.
func (m *Manager) RootDir() string {
	if m == nil {
		return ""
	}
	return m.root
}

// IsLearnerSkillsRoot reports whether root is the dedicated learner-managed
// layer. Tool wiring uses this as a fail-closed guard against accidentally
// attaching skill_manage to an installed/manual skills manager.
func IsLearnerSkillsRoot(root string) bool {
	return root != "" && filepath.Base(filepath.Clean(root)) == LearnerSkillsDirName
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var skillPathLocks sync.Map

var learnerPrivateInstancePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:/Users|/home)/[A-Za-z0-9._-]+(?:/|\b)`),
	regexp.MustCompile(`(?i)[A-Z]:\\Users\\[A-Za-z0-9._-]+(?:\\|\b)`),
	regexp.MustCompile(`(?i)https?://[^/\s]+\.(?:internal|local|corp)(?::\d+)?(?:/|\b)`),
}

func lockForPath(path string) *sync.Mutex {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	lock, _ := skillPathLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

// LockLearnerSkillOperation serializes the full local learner asset operation
// (hydrate or local->remote mutation), not merely one atomic file rename.
// The returned unlock function must always be called.
func LockLearnerSkillOperation(root, slug string) (func(), error) {
	if !IsLearnerSkillsRoot(root) {
		return nil, fmt.Errorf("skill manager root %q is not the learner-skills layer", root)
	}
	if err := ValidateSlug(slug); err != nil {
		return nil, err
	}
	lock := lockForPath(filepath.Join(root, slug, ".learner-operation"))
	lock.Lock()
	return lock.Unlock, nil
}

func (m *Manager) skillPath(slug string) string {
	return filepath.Join(m.root, slug, "SKILL.md")
}

func (m *Manager) validateSlug(slug string) error {
	return validateSlug(slug, m.config.MaxSlugChars)
}

// ValidateSlug applies the canonical skill directory-name policy used by the
// manager and object-store layer. Keeping one validator prevents a valid
// manager operation from being mirrored under a different/escaping key.
func ValidateSlug(slug string) error {
	return validateSlug(slug, DefaultManagerConfig().MaxSlugChars)
}

func validateSlug(slug string, maxChars int) error {
	if slug == "" {
		return errors.New("skill slug is required")
	}
	if utf8.RuneCountInString(slug) > maxChars {
		return fmt.Errorf("skill slug exceeds %d chars", maxChars)
	}
	if !slugRe.MatchString(slug) {
		return fmt.Errorf("invalid skill slug %q: use lowercase letters, digits, dots, hyphens, underscores; must start with a letter or digit", slug)
	}
	return nil
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parseFrontmatter 解析 SKILL.md 的 YAML frontmatter,返回 frontmatter 与正文。
// 与 validateContent 共用,保证 List 与写入校验对"合法 frontmatter"判定一致。
func parseFrontmatter(content string) (skillFrontmatter, string, error) {
	var fm skillFrontmatter
	if !strings.HasPrefix(content, "---\n") {
		return fm, "", errors.New("SKILL.md must start with YAML frontmatter (---)")
	}
	rest := strings.TrimPrefix(content, "---\n")
	const frontmatterEnd = "\n---\n"
	end := strings.Index(rest, frontmatterEnd)
	if end < 0 {
		return fm, "", errors.New("SKILL.md frontmatter is not closed with ---")
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fm, "", fmt.Errorf("frontmatter parse error: %w", err)
	}
	return fm, rest[end+len(frontmatterEnd):], nil
}

func (m *Manager) validateContent(content string) error {
	if utf8.RuneCountInString(content) > m.config.MaxContentChars {
		return fmt.Errorf("skill content exceeds %d chars", m.config.MaxContentChars)
	}
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return err
	}
	if strings.TrimSpace(fm.Name) == "" {
		return errors.New("frontmatter must include non-empty 'name'")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return errors.New("frontmatter must include non-empty 'description'")
	}
	if utf8.RuneCountInString(fm.Description) > m.config.MaxDescriptionChars {
		return fmt.Errorf("description exceeds %d chars", m.config.MaxDescriptionChars)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("SKILL.md must have content after the frontmatter")
	}
	return nil
}

func (m *Manager) scanContent(content string) error {
	threats := privacy.ScanSkillStrict(content)
	if len(threats) > 0 {
		seen := map[string]bool{}
		var types []string
		for _, th := range threats {
			typ := string(th.Type)
			if seen[typ] {
				continue
			}
			seen[typ] = true
			types = append(types, typ)
		}
		return fmt.Errorf("unsafe skill content rejected: threat pattern(s): %s", strings.Join(types, ", "))
	}
	if IsLearnerSkillsRoot(m.root) {
		if privacy.ContainsSensitiveInstanceData(content) {
			return errors.New("private instance data rejected from shared learner skill: replace PII, credentials, IP addresses, and account-specific values with descriptive placeholders or configuration variables")
		}
		for _, pattern := range learnerPrivateInstancePatterns {
			if pattern.MatchString(content) {
				return errors.New("private instance data rejected from shared learner skill: replace owner-specific home paths and internal hostnames with descriptive placeholders or configuration variables")
			}
		}
	}
	return nil
}

func (m *Manager) Create(slug, content string) error {
	return m.write(slug, content, false)
}

func (m *Manager) Update(slug, content string) error {
	return m.write(slug, content, true)
}

// ValidateWrite performs every deterministic validation used by Create and
// Update without touching the filesystem. Cadence mutation receipts call this
// before crossing their durable prepared boundary so recovery is never left
// with an intent that cannot pass format or security validation. The returned
// content is the canonical byte representation Manager.write will persist.
func (m *Manager) ValidateWrite(slug, content string) (string, error) {
	if m == nil {
		return "", errors.New("skill manager is not configured")
	}
	if err := m.validateSlug(slug); err != nil {
		return "", err
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if err := m.validateContent(content); err != nil {
		return "", err
	}
	if err := m.scanContent(content); err != nil {
		return "", err
	}
	return content, nil
}

func (m *Manager) write(slug, content string, mustExist bool) error {
	var err error
	content, err = m.ValidateWrite(slug, content)
	if err != nil {
		return err
	}

	path := m.skillPath(slug)
	lock := lockForPath(path)
	lock.Lock()
	defer lock.Unlock()

	_, statErr := os.Stat(path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if mustExist && !exists {
		return fmt.Errorf("skill %q does not exist", slug)
	}
	if !mustExist && exists {
		return fmt.Errorf("skill %q already exists", slug)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".SKILL.md.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (m *Manager) Read(slug string) (string, bool) {
	if err := m.validateSlug(slug); err != nil {
		return "", false
	}
	data, err := os.ReadFile(m.skillPath(slug))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func (m *Manager) Delete(slug string) error {
	if err := m.validateSlug(slug); err != nil {
		return err
	}
	lock := lockForPath(m.skillPath(slug))
	lock.Lock()
	defer lock.Unlock()
	dir := filepath.Join(m.root, slug)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return fmt.Errorf("skill %q does not exist", slug)
	}
	return os.RemoveAll(dir)
}

// SkillListItem 是 List 的一项:目录 slug 加 SKILL.md frontmatter 元数据。
type SkillListItem struct {
	Slug        string
	Name        string
	Description string
}

// List 枚举根目录下所有带合法 SKILL.md 的技能,按 slug 升序。单个技能的
// frontmatter 损坏只跳过该项,不让整个列表失败;根目录不存在返回 nil。
func (m *Manager) List() []SkillListItem {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil
	}
	var out []SkillListItem
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		content, ok := m.Read(slug)
		if !ok {
			continue
		}
		fm, _, err := parseFrontmatter(content)
		if err != nil {
			continue
		}
		out = append(out, SkillListItem{Slug: slug, Name: fm.Name, Description: fm.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}
