// Package enrich adds optional, typed semantic summaries to table and code
// chunks. It never replaces source text and is deliberately isolated from
// answer-model and agent configuration.
package enrich

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/chunktext"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

const EnrichmentSchemaVersion = "text-enrichment-v1"

var (
	ErrInvalidResponse    = errors.New("enrich: invalid typed response")
	ErrRawContentTooLarge = errors.New("enrich: raw chunk cannot fit provider or storage limits")
)

// BlockKind is intentionally independent from the splitter's internal AST.
// Only table and code blocks cross the optional network boundary.
type BlockKind string

const (
	BlockTable BlockKind = "table"
	BlockCode  BlockKind = "code"
)

// CacheScope confines cached results to one document prefix. Empty or partial
// scopes disable caching instead of creating a cross-document cache.
type CacheScope struct {
	UserID string
	KBID   string
	DocID  string
}

func (s CacheScope) valid() bool {
	return strings.TrimSpace(s.UserID) != "" && strings.TrimSpace(s.KBID) != "" && strings.TrimSpace(s.DocID) != ""
}

// EnrichableBlock is the stable splitter-to-enricher contract. TokenBudget
// and ByteBudget are the actual remaining capacities after raw text, heading
// context, and the subordinate enhancement label have been accounted for.
type EnrichableBlock struct {
	Kind        BlockKind
	RawContent  string
	TokenBudget int
	ByteBudget  int
	Scope       CacheScope
}

func (b EnrichableBlock) validate() error {
	if b.Kind != BlockTable && b.Kind != BlockCode {
		return fmt.Errorf("enrich: unsupported block kind %q", b.Kind)
	}
	if strings.TrimSpace(b.RawContent) == "" || !utf8.ValidString(b.RawContent) || strings.ContainsRune(b.RawContent, 0) {
		return errors.New("enrich: raw block must be non-empty valid UTF-8")
	}
	if b.TokenBudget < 0 || b.ByteBudget < 0 {
		return errors.New("enrich: block budgets cannot be negative")
	}
	return nil
}

type ColumnMeaning struct {
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

type TableEnhancement struct {
	Topic       string          `json:"topic"`
	Columns     []ColumnMeaning `json:"columns"`
	KeyEntities []string        `json:"keyEntities"`
	Units       []string        `json:"units"`
	Ranges      []string        `json:"ranges"`
	Summary     string          `json:"summary"`
}

type CodeEnhancement struct {
	Language        string   `json:"language"`
	Responsibility  string   `json:"responsibility"`
	Inputs          []string `json:"inputs"`
	Outputs         []string `json:"outputs"`
	SideEffects     []string `json:"sideEffects"`
	Symbols         []string `json:"symbols"`
	ErrorConditions []string `json:"errorConditions"`
	Description     string   `json:"description"`
}

// Enhancement retains the typed result. Text is derived deterministically;
// callers never trust a second free-form envelope field from the provider.
type Enhancement struct {
	Kind  BlockKind         `json:"kind"`
	Table *TableEnhancement `json:"table,omitempty"`
	Code  *CodeEnhancement  `json:"code,omitempty"`

	// rendered is a per-chunk bounded view. It is intentionally not cached:
	// the same typed result may be reused under a different chunk budget.
	rendered    string
	renderedSet bool
}

func (e Enhancement) validate(limits SchemaLimits) error {
	limits = limits.normalized()
	switch e.Kind {
	case BlockTable:
		if e.Table == nil || e.Code != nil {
			return errors.New("table enhancement must contain only table data")
		}
		if err := validateTable(e.Table, limits); err != nil {
			return err
		}
	case BlockCode:
		if e.Code == nil || e.Table != nil {
			return errors.New("code enhancement must contain only code data")
		}
		if err := validateCode(e.Code, limits); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported enhancement kind %q", e.Kind)
	}
	if strings.TrimSpace(e.Text()) == "" {
		return errors.New("enhancement contains no useful text")
	}
	return nil
}

func validateTable(value *TableEnhancement, limits SchemaLimits) error {
	if value == nil || len(value.Columns) > limits.MaxItems || len(value.KeyEntities) > limits.MaxItems ||
		len(value.Units) > limits.MaxItems || len(value.Ranges) > limits.MaxItems {
		return errors.New("table enhancement exceeds schema item limits")
	}
	values := []string{value.Topic, value.Summary}
	for _, column := range value.Columns {
		values = append(values, column.Name, column.Meaning)
	}
	values = append(values, value.KeyEntities...)
	values = append(values, value.Units...)
	values = append(values, value.Ranges...)
	return validateFields(values, limits)
}

func validateCode(value *CodeEnhancement, limits SchemaLimits) error {
	if value == nil || len(value.Inputs) > limits.MaxItems || len(value.Outputs) > limits.MaxItems ||
		len(value.SideEffects) > limits.MaxItems || len(value.Symbols) > limits.MaxItems ||
		len(value.ErrorConditions) > limits.MaxItems {
		return errors.New("code enhancement exceeds schema item limits")
	}
	values := []string{value.Language, value.Responsibility, value.Description}
	values = append(values, value.Inputs...)
	values = append(values, value.Outputs...)
	values = append(values, value.SideEffects...)
	values = append(values, value.Symbols...)
	values = append(values, value.ErrorConditions...)
	return validateFields(values, limits)
}

func validateFields(values []string, limits SchemaLimits) error {
	total := 0
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > limits.MaxFieldBytes {
			return errors.New("enhancement field is invalid or too large")
		}
		total += len(value)
	}
	if total > limits.MaxTextBytes {
		return errors.New("enhancement text exceeds schema byte limit")
	}
	return nil
}

// Text renders typed fields into compact searchable prose.
func (e Enhancement) Text() string {
	if e.renderedSet {
		return e.rendered
	}
	var lines []string
	switch e.Kind {
	case BlockTable:
		if e.Table == nil {
			return ""
		}
		appendLine := func(label, value string) {
			if value = strings.TrimSpace(value); value != "" {
				lines = append(lines, label+"："+value)
			}
		}
		appendLine("表格主题", e.Table.Topic)
		if len(e.Table.Columns) > 0 {
			columns := make([]string, 0, len(e.Table.Columns))
			for _, column := range e.Table.Columns {
				name, meaning := strings.TrimSpace(column.Name), strings.TrimSpace(column.Meaning)
				if name != "" || meaning != "" {
					columns = append(columns, strings.Trim(name+"："+meaning, "："))
				}
			}
			appendLine("列含义", strings.Join(columns, "；"))
		}
		appendLine("关键实体", joinNonEmpty(e.Table.KeyEntities))
		appendLine("单位", joinNonEmpty(e.Table.Units))
		appendLine("范围", joinNonEmpty(e.Table.Ranges))
		appendLine("摘要", e.Table.Summary)
	case BlockCode:
		if e.Code == nil {
			return ""
		}
		appendLine := func(label, value string) {
			if value = strings.TrimSpace(value); value != "" {
				lines = append(lines, label+"："+value)
			}
		}
		appendLine("代码语言", e.Code.Language)
		appendLine("主要职责", e.Code.Responsibility)
		appendLine("输入", joinNonEmpty(e.Code.Inputs))
		appendLine("输出", joinNonEmpty(e.Code.Outputs))
		appendLine("副作用", joinNonEmpty(e.Code.SideEffects))
		appendLine("关键符号", joinNonEmpty(e.Code.Symbols))
		appendLine("错误条件", joinNonEmpty(e.Code.ErrorConditions))
		appendLine("检索描述", e.Code.Description)
	}
	return strings.Join(lines, "\n")
}

func boundedEnhancement(value Enhancement, block EnrichableBlock) Enhancement {
	value.rendered = limitText(value.Text(), block.TokenBudget, block.ByteBudget)
	value.renderedSet = true
	return value
}

func joinNonEmpty(values []string) string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return strings.Join(result, "；")
}

// Enricher is independent of splitter AST details. The exact same durable
// task-scoped budget pointer used by Vision and image transcription is passed
// to every initial and repair attempt.
type Enricher interface {
	Enrich(context.Context, EnrichableBlock, *vision.TaskDocumentAIBudget) (Enhancement, error)
}

type Warning struct {
	ChunkIndex int    `json:"chunkIndex"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

type ProcessConfig struct {
	SystemEnabled bool
	TextModel     string
	KBEnabled     bool
	MaxBlocks     int
	Finalize      FinalizeConfig
	Scope         CacheScope
}

type Processor struct {
	enricher Enricher
}

func NewProcessor(enricher Enricher) *Processor { return &Processor{enricher: enricher} }

// EnrichChunks applies all three opt-in/configuration gates before scheduling
// work. Parse mode is intentionally absent: standard and auto have identical
// opt-in requirements.
func (p *Processor) EnrichChunks(ctx context.Context, chunks []split.Chunk, cfg ProcessConfig, budget *vision.TaskDocumentAIBudget) ([]split.Chunk, []Warning) {
	result := append([]split.Chunk(nil), chunks...)
	if p == nil || p.enricher == nil || !cfg.SystemEnabled || strings.TrimSpace(cfg.TextModel) == "" || !cfg.KBEnabled {
		return result, nil
	}
	maxBlocks := cfg.MaxBlocks
	if maxBlocks <= 0 {
		maxBlocks = 200
	}

	type outcome struct {
		index       int
		enhancement string
		warning     *Warning
	}
	eligible := 0
	outcomes := make(chan outcome, len(result))
	for i := range result {
		kind, ok := stableKind(result[i].Kind)
		if !ok {
			continue
		}
		eligible++
		if eligible > maxBlocks {
			outcomes <- outcome{index: i, warning: &Warning{ChunkIndex: result[i].Index,
				Code: "enrichment_block_limit", Message: "table/code enrichment block limit reached; source text retained"}}
			continue
		}
		tokens, bytes, err := RemainingEnhancementBudget(ctx, result[i], cfg.Finalize)
		if err != nil {
			outcomes <- outcome{index: i, warning: &Warning{ChunkIndex: result[i].Index,
				Code: "enrichment_budget_error", Message: "could not calculate a safe enhancement budget; source text retained"}}
			continue
		}
		if tokens <= 0 || bytes <= 0 {
			outcomes <- outcome{index: i, warning: &Warning{ChunkIndex: result[i].Index,
				Code: "enrichment_no_capacity", Message: "chunk has no remaining enhancement capacity; source text retained"}}
			continue
		}
		block := EnrichableBlock{Kind: kind, RawContent: result[i].RawContent,
			TokenBudget: tokens, ByteBudget: bytes, Scope: cfg.Scope}
		go func(index int, input EnrichableBlock) {
			value, err := p.enricher.Enrich(ctx, input, budget)
			if err != nil {
				outcomes <- outcome{index: index, warning: &Warning{ChunkIndex: result[index].Index,
					Code: "enrichment_failed", Message: "table/code enrichment failed; source text retained"}}
				return
			}
			text := limitText(value.Text(), input.TokenBudget, input.ByteBudget)
			outcomes <- outcome{index: index, enhancement: text}
		}(i, block)
	}

	warnings := make([]Warning, 0)
	for range eligible {
		item := <-outcomes
		if item.warning != nil {
			warnings = append(warnings, *item.warning)
			continue
		}
		result[item.index].Enhancement = item.enhancement
	}
	sort.SliceStable(warnings, func(i, j int) bool {
		if warnings[i].ChunkIndex != warnings[j].ChunkIndex {
			return warnings[i].ChunkIndex < warnings[j].ChunkIndex
		}
		return warnings[i].Code < warnings[j].Code
	})
	return result, warnings
}

func stableKind(kind split.BlockKind) (BlockKind, bool) {
	switch kind {
	case split.BlockTable:
		return BlockTable, true
	case split.BlockCode:
		return BlockCode, true
	default:
		return "", false
	}
}

// Tokenizer is the optional provider tokenizer used for the second boundary
// check immediately before embedding.
type Tokenizer interface {
	CountTokens(context.Context, string) (int, error)
}

type TokenizerFunc func(context.Context, string) (int, error)

func (f TokenizerFunc) CountTokens(ctx context.Context, value string) (int, error) {
	return f(ctx, value)
}

type FinalizeConfig struct {
	ChunkSize             int
	MaxSearchContentBytes int
	CollectionMaxLength   int
	ProviderTokenizer     Tokenizer
}

func (c FinalizeConfig) normalized() (FinalizeConfig, error) {
	if c.ChunkSize <= 0 {
		c.ChunkSize = 512
	}
	if c.MaxSearchContentBytes <= 0 {
		c.MaxSearchContentBytes = 60 << 10
	}
	if c.CollectionMaxLength <= 0 {
		c.CollectionMaxLength = config.RAGMilvusContentMaxLength
	}
	if c.MaxSearchContentBytes > c.CollectionMaxLength {
		return FinalizeConfig{}, fmt.Errorf("maxSearchContentBytes %d exceeds collection maxLength %d", c.MaxSearchContentBytes, c.CollectionMaxLength)
	}
	return c, nil
}

// RemainingEnhancementBudget returns content-only capacity after accounting
// for the fixed subordinate label. Both estimator and provider tokenizer are
// applied; the stricter remaining token value wins.
func RemainingEnhancementBudget(ctx context.Context, chunk split.Chunk, cfg FinalizeConfig) (int, int, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return 0, 0, err
	}
	base := baseSearchContent(chunk)
	labeled := chunktext.AppendEnhancement(base, "x")
	byteLimit := min(cfg.MaxSearchContentBytes, cfg.CollectionMaxLength)
	remainingBytes := byteLimit - len([]byte(labeled)) + len("x")
	// Keep the one-character sentinel charged. Tokenizers are not additive at
	// token boundaries, so subtracting CountTokens("x") can overstate the
	// capacity available after the fixed enhancement label.
	remainingTokens := cfg.ChunkSize - split.EstimateTokens(labeled)
	if cfg.ProviderTokenizer != nil {
		baseTokens, err := cfg.ProviderTokenizer.CountTokens(ctx, labeled)
		if err != nil {
			return 0, 0, err
		}
		remainingTokens = min(remainingTokens, cfg.ChunkSize-baseTokens)
	}
	return max(0, remainingTokens), max(0, remainingBytes), nil
}

// FinalizeChunk derives SearchContent only after enrichment. It first shrinks
// or drops Enhancement; if immutable raw text itself violates a provider or
// storage boundary it asks the splitter for a deterministic structural
// re-split and never truncates raw source silently.
func FinalizeChunk(ctx context.Context, chunk split.Chunk, cfg FinalizeConfig) ([]split.Chunk, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	chunk.RawContent = strings.TrimSpace(chunk.RawContent)
	chunk.Content = chunk.RawContent
	base := baseSearchContent(chunk)
	chunk.SearchContent = base
	chunk.Enhancement = strings.TrimSpace(chunk.Enhancement)
	if ok, err := finalTextFits(ctx, base, cfg); err != nil {
		return nil, err
	} else if !ok {
		chunk.Enhancement = ""
		return resplitRawChunk(ctx, chunk, cfg)
	}

	if chunk.Enhancement != "" {
		tokens, bytes, err := RemainingEnhancementBudget(ctx, chunk, cfg)
		if err != nil {
			return nil, err
		}
		chunk.Enhancement = limitText(chunk.Enhancement, tokens, bytes)
		chunk.Enhancement, err = largestFittingEnhancement(ctx, base, chunk.Enhancement, cfg)
		if err != nil {
			return nil, err
		}
		if chunk.Enhancement != "" {
			chunk.SearchContent = chunktext.AppendEnhancement(base, chunk.Enhancement)
		}
	}
	chunk.Tokens = split.EstimateTokens(chunk.SearchContent)
	return []split.Chunk{chunk}, nil
}

func FinalizeChunks(ctx context.Context, chunks []split.Chunk, cfg FinalizeConfig) ([]split.Chunk, error) {
	result := make([]split.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		finalized, err := FinalizeChunk(ctx, chunk, cfg)
		if err != nil {
			return nil, err
		}
		result = append(result, finalized...)
	}
	for i := range result {
		result[i].Index = i
	}
	return result, nil
}

func baseSearchContent(chunk split.Chunk) string {
	raw := strings.TrimSpace(chunk.RawContent)
	search := strings.TrimSpace(chunk.SearchContent)
	if raw != "" && strings.HasSuffix(search, raw) {
		return strings.TrimSuffix(search, raw) + raw
	}
	return chunktext.Search(chunk.SectionTitle, raw)
}

func finalTextFits(ctx context.Context, value string, cfg FinalizeConfig) (bool, error) {
	if !utf8.ValidString(value) || len([]byte(value)) > cfg.MaxSearchContentBytes ||
		len([]byte(value)) > cfg.CollectionMaxLength || split.EstimateTokens(value) > cfg.ChunkSize {
		return false, nil
	}
	if cfg.ProviderTokenizer == nil {
		return true, nil
	}
	tokens, err := cfg.ProviderTokenizer.CountTokens(ctx, value)
	if err != nil {
		return false, err
	}
	return tokens <= cfg.ChunkSize, nil
}

func resplitRawChunk(ctx context.Context, chunk split.Chunk, cfg FinalizeConfig) ([]split.Chunk, error) {
	testTarget := func(target int) ([]split.Chunk, bool, error) {
		parts := split.ResplitChunk(chunk, split.Config{ChunkSize: target, ChunkOverlap: 0})
		if len(parts) == 0 {
			return nil, false, nil
		}
		for i := range parts {
			parts[i].Enhancement = ""
			parts[i].SearchContent = baseSearchContent(parts[i])
			parts[i].Tokens = split.EstimateTokens(parts[i].SearchContent)
			ok, err := finalTextFits(ctx, parts[i].SearchContent, cfg)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				return nil, false, nil
			}
		}
		return parts, true, nil
	}

	// A smaller structural target yields chunks no larger than a bigger target.
	// Binary search finds the largest fitting target in O(log ChunkSize)
	// splitter passes instead of reparsing once for every integer token size.
	low, high := 1, max(1, cfg.ChunkSize-1)
	var best []split.Chunk
	for low <= high {
		middle := low + (high-low)/2
		parts, fit, err := testTarget(middle)
		if err != nil {
			return nil, err
		}
		if fit {
			best = parts
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if len(best) > 0 {
		return best, nil
	}
	return nil, ErrRawContentTooLarge
}

func largestFittingEnhancement(
	ctx context.Context,
	base, enhancement string,
	cfg FinalizeConfig,
) (string, error) {
	enhancement = strings.TrimSpace(enhancement)
	if enhancement == "" {
		return "", nil
	}
	if ok, err := finalTextFits(ctx, chunktext.AppendEnhancement(base, enhancement), cfg); err != nil {
		return "", err
	} else if ok {
		return enhancement, nil
	}
	runes := []rune(enhancement)
	low, high, best := 1, len(runes)-1, 0
	for low <= high {
		middle := low + (high-low)/2
		candidate := strings.TrimSpace(string(runes[:middle]))
		ok, err := finalTextFits(ctx, chunktext.AppendEnhancement(base, candidate), cfg)
		if err != nil {
			return "", err
		}
		if ok {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if best == 0 {
		return "", nil
	}
	return strings.TrimSpace(string(runes[:best])), nil
}

func limitText(value string, tokenBudget, byteBudget int) string {
	value = strings.TrimSpace(value)
	if value == "" || tokenBudget <= 0 || byteBudget <= 0 {
		return ""
	}
	runes := []rune(value)
	low, high, best := 1, len(runes), 0
	for low <= high {
		middle := low + (high-low)/2
		candidate := string(runes[:middle])
		if split.EstimateTokens(candidate) <= tokenBudget && len([]byte(candidate)) <= byteBudget {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	if best == 0 {
		return ""
	}
	if best == len(runes) {
		return value
	}
	cut := best
	for i := best - 1; i >= 0; i-- {
		switch runes[i] {
		case '\n', '。', '！', '？', '；', '.', '!', '?', ';':
			cut = i + 1
			i = -1
		}
	}
	return strings.TrimSpace(string(runes[:cut]))
}
