package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/qs3c/bkclaw/internal/privacy"
	"github.com/qs3c/bkclaw/internal/store"
)

type Target string

const (
	TargetUser   Target = "user"
	TargetMemory Target = "memory"
)

type Action string

const (
	ActionList    Action = "list"
	ActionAdd     Action = "add"
	ActionReplace Action = "replace"
	ActionRemove  Action = "remove"
)

type Config struct {
	Enabled         bool
	UserCharLimit   int
	MemoryCharLimit int
}

func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		UserCharLimit:   4000,
		MemoryCharLimit: 12000,
	}
}

type Operation struct {
	Action  Action `json:"action"`
	Content string `json:"content,omitempty"`
	OldText string `json:"old_text,omitempty"`
}

type Result struct {
	Success    bool     `json:"success"`
	Done       bool     `json:"done"`
	Target     Target   `json:"target"`
	EntryCount int      `json:"entry_count"`
	Usage      string   `json:"usage"`
	Message    string   `json:"message"`
	Entries    []string `json:"entries,omitempty"`
}

type Mutator func(current []byte, exists bool) (next []byte, delete bool, err error)

type Store interface {
	GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	MutateWorkspaceFile(ctx context.Context, agentID, userID, filename string, fn Mutator) ([]byte, error)
}

type Options struct {
	Store   Store
	Root    string
	AgentID string
	UserID  string
	Config  Config
}

type Manager struct {
	store   Store
	root    string
	agentID string
	userID  string
	config  Config
}

const entryDelimiter = "\n\n§\n\n"

var pathLocks sync.Map

func NewManager(opts Options) *Manager {
	return &Manager{
		store:   opts.Store,
		root:    opts.Root,
		agentID: opts.AgentID,
		userID:  opts.UserID,
		config:  normalizeConfig(opts.Config),
	}
}

func Filename(target Target) (string, error) {
	switch target {
	case TargetUser:
		return "USER.md", nil
	case TargetMemory:
		return "MEMORY.md", nil
	default:
		return "", fmt.Errorf("unknown memory target %q", target)
	}
}

func (m *Manager) List(ctx context.Context, target Target) Result {
	if !m.config.Enabled {
		return failureResult(target, nil, m.config, "managed memory is disabled")
	}
	if _, err := Filename(target); err != nil {
		return failureResult(target, nil, m.config, err.Error())
	}

	data, err := m.load(ctx, target)
	if err != nil {
		return failureResult(target, nil, m.config, err.Error())
	}
	entries, _ := parseEntries(target, data)
	return listResult(target, entries, m.config, "listed entries")
}

func (m *Manager) Apply(ctx context.Context, target Target, ops []Operation) Result {
	if !m.config.Enabled {
		return failureResult(target, nil, m.config, "managed memory is disabled")
	}
	filename, err := Filename(target)
	if err != nil {
		return failureResult(target, nil, m.config, err.Error())
	}
	if len(ops) == 0 {
		return failureResult(target, nil, m.config, "no memory operations requested")
	}
	if allListOperations(ops) {
		data, err := m.load(ctx, target)
		if err != nil {
			return failureResult(target, nil, m.config, err.Error())
		}
		entries, _ := parseEntries(target, data)
		return listResult(target, entries, m.config, "listed entries")
	}

	var opResult Result
	mutate := func(current []byte, exists bool) ([]byte, bool, error) {
		entries, _ := parseEntries(target, current)
		nextEntries, result := applyOperations(target, entries, ops, m.config)
		opResult = result
		if !result.Success {
			return current, false, operationFailure{result: result}
		}
		next := serialize(target, nextEntries)
		return next, false, nil
	}

	var data []byte
	if m.store != nil {
		data, err = m.store.MutateWorkspaceFile(ctx, m.agentID, m.userID, filename, mutate)
	} else {
		data, err = m.mutateFile(filename, mutate)
	}
	if err != nil {
		var opErr operationFailure
		if errors.As(err, &opErr) {
			return opErr.result
		}
		return failureResult(target, nil, m.config, err.Error())
	}
	if opResult.Done {
		return opResult
	}
	entries, _ := parseEntries(target, data)
	return successResult(target, entries, m.config, "applied operations")
}

func (m *Manager) Render(ctx context.Context, target Target) string {
	result := m.List(ctx, target)
	if !result.Success {
		return ""
	}
	return m.RenderEntries(target, result.Entries)
}

func (m *Manager) RenderEntries(target Target, entries []string) string {
	return strings.Join(safeEntriesForList(target, entries), "\n\n")
}

func parseEntries(target Target, data []byte) (entries []string, managed bool) {
	text := normalizeNewlines(string(data))
	text = strings.TrimPrefix(text, "\ufeff")
	if strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), marker(target)) {
		trimmed := strings.TrimLeft(text, " \t\r\n")
		rest := strings.TrimPrefix(trimmed, marker(target))
		rest = strings.TrimPrefix(rest, "\n")
		if strings.TrimSpace(rest) == "" {
			return nil, true
		}
		for _, part := range strings.Split(rest, entryDelimiter) {
			entry := strings.TrimSpace(part)
			if entry != "" {
				entries = append(entries, entry)
			}
		}
		return dedupeExactEntries(entries), true
	}
	if isComplexLegacyMarkdown(text) {
		if trimmed := strings.TrimSpace(text); trimmed != "" {
			return []string{trimmed}, false
		}
		return nil, false
	}

	var paragraph []string
	addEntry := func(entry string) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return
		}
		entries = append(entries, entry)
	}
	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		addEntry(strings.Join(paragraph, "\n"))
		paragraph = nil
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flushParagraph()
			continue
		}
		if heading, ok := parseHeading(trimmed); ok {
			flushParagraph()
			if !isAutoPersistedHeading(heading) {
				addEntry(heading)
			}
			continue
		}
		if bullet, ok := parseBullet(trimmed); ok {
			flushParagraph()
			addEntry(bullet)
			continue
		}
		paragraph = append(paragraph, trimmed)
	}
	flushParagraph()
	return dedupeExactEntries(entries), false
}

func serialize(target Target, entries []string) []byte {
	cleaned := make([]string, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			cleaned = append(cleaned, entry)
		}
	}
	body := strings.Join(cleaned, entryDelimiter)
	if body == "" {
		return []byte(marker(target) + "\n")
	}
	return []byte(marker(target) + "\n" + body)
}

func applyOperations(target Target, current []string, ops []Operation, cfg Config) ([]string, Result) {
	cfg = normalizeConfig(cfg)
	original := cloneEntries(current)
	if !cfg.Enabled {
		return original, failureResult(target, original, cfg, "managed memory is disabled")
	}
	if _, err := Filename(target); err != nil {
		return original, failureResult(target, original, cfg, err.Error())
	}
	if len(ops) == 0 {
		return original, failureResult(target, original, cfg, "no memory operations requested")
	}

	next := cloneEntries(current)
	for _, op := range ops {
		switch op.Action {
		case ActionList:
			continue
		case ActionAdd:
			content := strings.TrimSpace(op.Content)
			if content == "" {
				return original, failureResult(target, original, cfg, "add content is empty")
			}
			if containsManagedDelimiter(content) {
				return original, failureResult(target, original, cfg, "add content contains managed delimiter")
			}
			if msg := unsafeMessage(content); msg != "" {
				return original, failureResult(target, original, cfg, msg)
			}
			if containsExact(next, content) {
				continue
			}
			next = append(next, content)
		case ActionReplace:
			oldText := strings.TrimSpace(op.OldText)
			if oldText == "" {
				return original, failureResult(target, original, cfg, "replace old_text is required")
			}
			content := strings.TrimSpace(op.Content)
			if content == "" {
				return original, failureResult(target, original, cfg, "replace content is empty")
			}
			if containsManagedDelimiter(content) {
				return original, failureResult(target, original, cfg, "replace content contains managed delimiter")
			}
			if msg := unsafeMessage(content); msg != "" {
				return original, failureResult(target, original, cfg, msg)
			}
			matchIndex, count := uniqueSubstringMatch(next, oldText)
			if count == 0 {
				return original, failureResult(target, original, cfg, "old_text matches no entries")
			}
			if count > 1 {
				return original, failureResult(target, original, cfg, fmt.Sprintf("old_text matches %d entries", count))
			}
			next[matchIndex] = content
		case ActionRemove:
			oldText := strings.TrimSpace(op.OldText)
			if oldText == "" {
				return original, failureResult(target, original, cfg, "remove old_text is required")
			}
			matchIndex, count := uniqueSubstringMatch(next, oldText)
			if count == 0 {
				return original, failureResult(target, original, cfg, "old_text matches no entries")
			}
			if count > 1 {
				return original, failureResult(target, original, cfg, fmt.Sprintf("old_text matches %d entries", count))
			}
			next = append(next[:matchIndex], next[matchIndex+1:]...)
		default:
			return original, failureResult(target, original, cfg, fmt.Sprintf("unsupported memory action %q", op.Action))
		}
	}

	if limit := limitForTarget(target, cfg); limit > 0 {
		size := serializedRuneCount(target, next)
		if size > limit {
			return original, failureResult(target, original, cfg, fmt.Sprintf("managed memory over limit: %d/%d characters", size, limit))
		}
	}
	return next, successResult(target, next, cfg, "applied operations")
}

func (m *Manager) load(ctx context.Context, target Target) ([]byte, error) {
	filename, err := Filename(target)
	if err != nil {
		return nil, err
	}
	if m.store != nil {
		data, err := m.store.GetWorkspaceFileExact(ctx, m.agentID, m.userID, filename)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return data, err
	}
	data, err := os.ReadFile(m.path(filename))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func (m *Manager) mutateFile(filename string, fn Mutator) ([]byte, error) {
	path := m.path(filename)
	lock := mutexForPath(path)
	lock.Lock()
	defer lock.Unlock()

	current, err := os.ReadFile(path)
	exists := true
	if errors.Is(err, os.ErrNotExist) {
		current = nil
		exists = false
	} else if err != nil {
		return nil, err
	}

	next, deleteFile, err := fn(append([]byte(nil), current...), exists)
	if err != nil {
		return append([]byte(nil), current...), err
	}
	if deleteFile {
		if !exists {
			return nil, nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, nil
	}
	if bytes.Equal(current, next) {
		return append([]byte(nil), current...), nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(next); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, err
	}
	cleanup = false
	return append([]byte(nil), next...), nil
}

func (m *Manager) path(filename string) string {
	root := m.root
	if root == "" {
		root = "."
	}
	return filepath.Join(root, filename)
}

func marker(target Target) string {
	return fmt.Sprintf("<!-- bkclaw-memory:v1 target=%s -->", target)
}

func normalizeConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg == (Config{}) {
		return defaults
	}
	if cfg.UserCharLimit == 0 {
		cfg.UserCharLimit = defaults.UserCharLimit
	}
	if cfg.MemoryCharLimit == 0 {
		cfg.MemoryCharLimit = defaults.MemoryCharLimit
	}
	return cfg
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

func parseHeading(line string) (string, bool) {
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == len(line) {
		return "", false
	}
	text := strings.TrimSpace(line[i:])
	return text, text != ""
}

func parseBullet(line string) (string, bool) {
	for _, prefix := range []string{"- ", "* ", "+ ", "• "} {
		if strings.HasPrefix(line, prefix) {
			text := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			return text, text != ""
		}
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && line[i] == '.' && line[i+1] == ' ' {
		text := strings.TrimSpace(line[i+2:])
		return text, text != ""
	}
	return "", false
}

func isAutoPersistedHeading(heading string) bool {
	lower := strings.ToLower(heading)
	return strings.HasPrefix(lower, "auto-persisted") || strings.HasPrefix(lower, "auto-updated")
}

func isComplexLegacyMarkdown(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "```") || strings.Contains(line, "|") {
			return true
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return true
		}
	}
	return false
}

func dedupeExactEntries(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := map[string]bool{}
	deduped := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" || seen[entry] {
			continue
		}
		seen[entry] = true
		deduped = append(deduped, entry)
	}
	if len(deduped) == 0 {
		return nil
	}
	return deduped
}

func containsManagedDelimiter(content string) bool {
	return strings.Contains(normalizeNewlines(content), entryDelimiter)
}

func containsExact(entries []string, content string) bool {
	for _, entry := range entries {
		if entry == content {
			return true
		}
	}
	return false
}

func uniqueSubstringMatch(entries []string, oldText string) (int, int) {
	matchIndex := -1
	count := 0
	for i, entry := range entries {
		if strings.Contains(entry, oldText) {
			if count == 0 {
				matchIndex = i
			}
			count++
		}
	}
	return matchIndex, count
}

func allListOperations(ops []Operation) bool {
	for _, op := range ops {
		if op.Action != ActionList {
			return false
		}
	}
	return len(ops) > 0
}

func safeEntriesForList(target Target, entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	filename, err := Filename(target)
	if err != nil {
		filename = "memory file"
	}
	safe := make([]string, 0, len(entries))
	for _, entry := range entries {
		if threats := privacy.ScanMemoryStrict(entry); len(threats) > 0 {
			safe = append(safe, blockedEntryPlaceholder(filename, threats))
			continue
		}
		safe = append(safe, entry)
	}
	return safe
}

func blockedEntryPlaceholder(filename string, threats []privacy.Threat) string {
	return fmt.Sprintf(
		"[BLOCKED: %s entry contained threat pattern(s): %s. Use memory(remove) to delete the original.]",
		filename,
		strings.Join(threatTypes(threats), ", "),
	)
}

func unsafeMessage(content string) string {
	threats := privacy.ScanMemoryStrict(content)
	if len(threats) == 0 {
		return ""
	}
	return fmt.Sprintf("unsafe memory content rejected: threat pattern(s): %s", strings.Join(threatTypes(threats), ", "))
}

func threatTypes(threats []privacy.Threat) []string {
	seen := map[string]bool{}
	types := make([]string, 0, len(threats))
	for _, threat := range threats {
		typ := string(threat.Type)
		if typ == "" || seen[typ] {
			continue
		}
		seen[typ] = true
		types = append(types, typ)
	}
	if len(types) == 0 {
		return []string{"unknown"}
	}
	if len(types) > 1 {
		sort.Strings(types)
	}
	return types
}

func successResult(target Target, entries []string, cfg Config, message string) Result {
	return resultWithEntries(true, target, entries, cfg, message)
}

func failureResult(target Target, entries []string, cfg Config, message string) Result {
	return resultWithEntries(false, target, entries, cfg, message)
}

func listResult(target Target, entries []string, cfg Config, message string) Result {
	copied := cloneEntries(entries)
	return Result{
		Success:    true,
		Done:       true,
		Target:     target,
		EntryCount: len(copied),
		Usage:      usage(target, copied, cfg),
		Message:    message,
		Entries:    safeEntriesForList(target, copied),
	}
}

func resultWithEntries(success bool, target Target, entries []string, cfg Config, message string) Result {
	copied := cloneEntries(entries)
	return Result{
		Success:    success,
		Done:       true,
		Target:     target,
		EntryCount: len(copied),
		Usage:      usage(target, copied, cfg),
		Message:    message,
		Entries:    copied,
	}
}

func usage(target Target, entries []string, cfg Config) string {
	size := serializedRuneCount(target, entries)
	limit := limitForTarget(target, cfg)
	if limit <= 0 {
		return strconv.Itoa(size) + " characters"
	}
	return fmt.Sprintf("%d/%d characters", size, limit)
}

func serializedRuneCount(target Target, entries []string) int {
	return utf8.RuneCount(serialize(target, entries))
}

func limitForTarget(target Target, cfg Config) int {
	switch target {
	case TargetUser:
		return cfg.UserCharLimit
	case TargetMemory:
		return cfg.MemoryCharLimit
	default:
		return 0
	}
}

func cloneEntries(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	return append([]string(nil), entries...)
}

func mutexForPath(path string) *sync.Mutex {
	key, err := filepath.Abs(path)
	if err != nil {
		key = filepath.Clean(path)
	}
	lock, _ := pathLocks.LoadOrStore(key, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

type operationFailure struct {
	result Result
}

func (e operationFailure) Error() string {
	return e.result.Message
}
