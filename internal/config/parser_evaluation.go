package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	parserEvaluationMaxFiles      = 50
	parserEvaluationMaxTimeoutMS  = 24 * 60 * 60 * 1000
	parserEvaluationMaxFileBytes  = int64(512 << 20)
	parserEvaluationMaxBatchBytes = int64(10 << 30)
	parserEvaluationMaxRetention  = 3650
)

// ParserEvaluationCfg is deployment-scoped. It deliberately lives outside
// RAGCfg because parser evaluation is a separate admin product and must not be
// affected by user- or agent-scoped RAG settings.
type ParserEvaluationCfg struct {
	Enabled            bool   `json:"enabled,omitempty"`
	WorkerEnabled      bool   `json:"workerEnabled,omitempty"`
	RendererEndpoint   string `json:"rendererEndpoint,omitempty"`
	MarkItDownEndpoint string `json:"markitdownEndpoint,omitempty"`
	AnyDocEndpoint     string `json:"anydocEndpoint,omitempty"`
	RenderTimeoutMS    int    `json:"renderTimeoutMs,omitempty"`
	ParseTimeoutMS     int    `json:"parseTimeoutMs,omitempty"`
	JudgeTimeoutMS     int    `json:"judgeTimeoutMs,omitempty"`
	MaxFileBytes       int64  `json:"maxFileBytes,omitempty"`
	MaxFiles           int    `json:"maxFiles,omitempty"`
	MaxBatchBytes      int64  `json:"maxBatchBytes,omitempty"`
	RetentionDays      int    `json:"retentionDays,omitempty"`
	MaxPages           int    `json:"maxPages,omitempty"`
	RenderDPI          int    `json:"renderDPI,omitempty"`
	MarkdownJudgeChars int    `json:"markdownJudgeChars,omitempty"`

	workerEnabledSet bool
}

func DefaultParserEvaluationCfg() ParserEvaluationCfg {
	return ParserEvaluationCfg{
		WorkerEnabled:      true,
		RendererEndpoint:   "http://parser-eval-renderer:8080",
		MarkItDownEndpoint: "http://parser-eval-markitdown:8080",
		AnyDocEndpoint:     "http://parser-eval-anydoc:8080",
		RenderTimeoutMS:    600_000,
		ParseTimeoutMS:     600_000,
		JudgeTimeoutMS:     240_000,
		MaxFileBytes:       50 << 20,
		MaxFiles:           50,
		MaxBatchBytes:      500 << 20,
		RetentionDays:      90,
		MaxPages:           6,
		RenderDPI:          100,
		MarkdownJudgeChars: 40_000,
		workerEnabledSet:   true,
	}
}

func (c *ParserEvaluationCfg) ApplyDefaults() {
	if c == nil {
		return
	}
	defaults := DefaultParserEvaluationCfg()
	if !c.workerEnabledSet {
		c.WorkerEnabled = defaults.WorkerEnabled
	}
	if strings.TrimSpace(c.RendererEndpoint) == "" {
		c.RendererEndpoint = defaults.RendererEndpoint
	}
	if strings.TrimSpace(c.MarkItDownEndpoint) == "" {
		c.MarkItDownEndpoint = defaults.MarkItDownEndpoint
	}
	if strings.TrimSpace(c.AnyDocEndpoint) == "" {
		c.AnyDocEndpoint = defaults.AnyDocEndpoint
	}
	if c.RenderTimeoutMS <= 0 {
		c.RenderTimeoutMS = defaults.RenderTimeoutMS
	}
	if c.ParseTimeoutMS <= 0 {
		c.ParseTimeoutMS = defaults.ParseTimeoutMS
	}
	if c.JudgeTimeoutMS <= 0 {
		c.JudgeTimeoutMS = defaults.JudgeTimeoutMS
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = defaults.MaxFileBytes
	}
	if c.MaxFiles <= 0 {
		c.MaxFiles = defaults.MaxFiles
	}
	if c.MaxBatchBytes <= 0 {
		c.MaxBatchBytes = defaults.MaxBatchBytes
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = defaults.RetentionDays
	}
	if c.MaxPages <= 0 {
		c.MaxPages = defaults.MaxPages
	}
	if c.RenderDPI <= 0 {
		c.RenderDPI = defaults.RenderDPI
	}
	if c.MarkdownJudgeChars <= 0 {
		c.MarkdownJudgeChars = defaults.MarkdownJudgeChars
	}
}

func (c ParserEvaluationCfg) Validate() error {
	for name, endpoint := range map[string]string{
		"rendererEndpoint":   c.RendererEndpoint,
		"markitdownEndpoint": c.MarkItDownEndpoint,
		"anydocEndpoint":     c.AnyDocEndpoint,
	} {
		if err := validateParserEvaluationEndpoint(endpoint); err != nil {
			return fmt.Errorf("parserEvaluation.%s: %w", name, err)
		}
	}
	for name, value := range map[string]int{
		"renderTimeoutMs": c.RenderTimeoutMS,
		"parseTimeoutMs":  c.ParseTimeoutMS,
		"judgeTimeoutMs":  c.JudgeTimeoutMS,
	} {
		if value <= 0 || value > parserEvaluationMaxTimeoutMS {
			return fmt.Errorf("parserEvaluation.%s must be in [1,%d], got %d", name, parserEvaluationMaxTimeoutMS, value)
		}
	}
	if c.MaxFileBytes <= 0 || c.MaxFileBytes > parserEvaluationMaxFileBytes {
		return fmt.Errorf("parserEvaluation.maxFileBytes must be in [1,%d], got %d", parserEvaluationMaxFileBytes, c.MaxFileBytes)
	}
	if c.MaxFiles <= 0 || c.MaxFiles > parserEvaluationMaxFiles {
		return fmt.Errorf("parserEvaluation.maxFiles must be in [1,%d], got %d", parserEvaluationMaxFiles, c.MaxFiles)
	}
	if c.MaxBatchBytes < c.MaxFileBytes || c.MaxBatchBytes > parserEvaluationMaxBatchBytes {
		return fmt.Errorf("parserEvaluation.maxBatchBytes must be between maxFileBytes and %d, got %d", parserEvaluationMaxBatchBytes, c.MaxBatchBytes)
	}
	if c.RetentionDays <= 0 || c.RetentionDays > parserEvaluationMaxRetention {
		return fmt.Errorf("parserEvaluation.retentionDays must be in [1,%d], got %d", parserEvaluationMaxRetention, c.RetentionDays)
	}
	if c.MaxPages <= 0 || c.MaxPages > 100 {
		return fmt.Errorf("parserEvaluation.maxPages must be in [1,100], got %d", c.MaxPages)
	}
	if c.RenderDPI < 36 || c.RenderDPI > 300 {
		return fmt.Errorf("parserEvaluation.renderDPI must be in [36,300], got %d", c.RenderDPI)
	}
	if c.MarkdownJudgeChars <= 0 || c.MarkdownJudgeChars > 200_000 {
		return fmt.Errorf("parserEvaluation.markdownJudgeChars must be in [1,200000], got %d", c.MarkdownJudgeChars)
	}
	return nil
}

func validateParserEvaluationEndpoint(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("must be an absolute HTTP endpoint")
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an absolute credential-free HTTP endpoint without path, query, or fragment")
	}
	return nil
}
