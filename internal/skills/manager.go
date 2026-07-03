package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

var skillPathLocks sync.Map

func lockForPath(path string) *sync.Mutex {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	lock, _ := skillPathLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (m *Manager) skillPath(slug string) string {
	return filepath.Join(m.root, slug, "SKILL.md")
}

func (m *Manager) validateSlug(slug string) error {
	if slug == "" {
		return errors.New("skill slug is required")
	}
	if utf8.RuneCountInString(slug) > m.config.MaxSlugChars {
		return fmt.Errorf("skill slug exceeds %d chars", m.config.MaxSlugChars)
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

func (m *Manager) validateContent(content string) error {
	if utf8.RuneCountInString(content) > m.config.MaxContentChars {
		return fmt.Errorf("skill content exceeds %d chars", m.config.MaxContentChars)
	}
	if !strings.HasPrefix(content, "---\n") {
		return errors.New("SKILL.md must start with YAML frontmatter (---)")
	}

	rest := strings.TrimPrefix(content, "---\n")
	const frontmatterEnd = "\n---\n"
	end := strings.Index(rest, frontmatterEnd)
	if end < 0 {
		return errors.New("SKILL.md frontmatter is not closed with ---")
	}

	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fmt.Errorf("frontmatter parse error: %w", err)
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
	body := rest[end+len(frontmatterEnd):]
	if strings.TrimSpace(body) == "" {
		return errors.New("SKILL.md must have content after the frontmatter")
	}
	return nil
}

func scanContent(content string) error {
	threats := privacy.ScanSkillStrict(content)
	if len(threats) == 0 {
		return nil
	}
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

func (m *Manager) Create(slug, content string) error {
	return m.write(slug, content, false)
}

func (m *Manager) Update(slug, content string) error {
	return m.write(slug, content, true)
}

func (m *Manager) write(slug, content string, mustExist bool) error {
	if err := m.validateSlug(slug); err != nil {
		return err
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if err := m.validateContent(content); err != nil {
		return err
	}
	if err := scanContent(content); err != nil {
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
	dir := filepath.Join(m.root, slug)
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		return fmt.Errorf("skill %q does not exist", slug)
	}
	return os.RemoveAll(dir)
}
