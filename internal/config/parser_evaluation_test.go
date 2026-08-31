package config

import (
	"strings"
	"testing"
)

func TestParserEvaluationDefaults(t *testing.T) {
	cfg := DefaultParserEvaluationCfg()
	if cfg.Enabled {
		t.Fatal("parser evaluation must default to disabled")
	}
	if !cfg.WorkerEnabled || cfg.RendererEndpoint != "http://parser-eval-renderer:8080" ||
		cfg.MarkItDownEndpoint != "http://parser-eval-markitdown:8080" ||
		cfg.AnyDocEndpoint != "http://parser-eval-anydoc:8080" {
		t.Fatalf("unexpected parser evaluation endpoints/defaults: %+v", cfg)
	}
	if cfg.RenderTimeoutMS != 600_000 || cfg.ParseTimeoutMS != 600_000 || cfg.JudgeTimeoutMS != 240_000 ||
		cfg.MaxFileBytes != 50<<20 || cfg.MaxFiles != 50 || cfg.MaxBatchBytes != 500<<20 ||
		cfg.RetentionDays != 90 || cfg.MaxPages != 6 || cfg.RenderDPI != 100 || cfg.MarkdownJudgeChars != 40_000 {
		t.Fatalf("unexpected parser evaluation limits: %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParserEvaluationValidation(t *testing.T) {
	valid := DefaultParserEvaluationCfg()
	valid.Enabled = true
	tests := []struct {
		name   string
		mutate func(*ParserEvaluationCfg)
		want   string
	}{
		{name: "renderer endpoint", mutate: func(c *ParserEvaluationCfg) { c.RendererEndpoint = "renderer:8080" }, want: "rendererEndpoint"},
		{name: "endpoint credentials", mutate: func(c *ParserEvaluationCfg) { c.AnyDocEndpoint = "http://user:pass@anydoc:8080" }, want: "anydocEndpoint"},
		{name: "parse timeout", mutate: func(c *ParserEvaluationCfg) { c.ParseTimeoutMS = 0 }, want: "parseTimeoutMs"},
		{name: "files", mutate: func(c *ParserEvaluationCfg) { c.MaxFiles = 51 }, want: "maxFiles"},
		{name: "batch bytes", mutate: func(c *ParserEvaluationCfg) { c.MaxBatchBytes = c.MaxFileBytes - 1 }, want: "maxBatchBytes"},
		{name: "retention", mutate: func(c *ParserEvaluationCfg) { c.RetentionDays = 0 }, want: "retentionDays"},
		{name: "pages", mutate: func(c *ParserEvaluationCfg) { c.MaxPages = 0 }, want: "maxPages"},
		{name: "dpi", mutate: func(c *ParserEvaluationCfg) { c.RenderDPI = 301 }, want: "renderDPI"},
		{name: "judge chars", mutate: func(c *ParserEvaluationCfg) { c.MarkdownJudgeChars = 0 }, want: "markdownJudgeChars"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error=%v, want field %q", err, tt.want)
			}
		})
	}
}

func TestLoadEnvParserEvaluation(t *testing.T) {
	t.Setenv("BKCRAB_PARSER_EVAL_ENABLED", "true")
	t.Setenv("BKCRAB_PARSER_EVAL_WORKER_ENABLED", "false")
	t.Setenv("BKCRAB_PARSER_EVAL_RENDERER_ENDPOINT", "http://renderer.internal:8181")
	t.Setenv("BKCRAB_PARSER_EVAL_MARKITDOWN_ENDPOINT", "http://mark.internal:8182")
	t.Setenv("BKCRAB_PARSER_EVAL_ANYDOC_ENDPOINT", "http://any.internal:8183")
	t.Setenv("BKCRAB_PARSER_EVAL_RENDER_TIMEOUT_MS", "1111")
	t.Setenv("BKCRAB_PARSER_EVAL_PARSE_TIMEOUT_MS", "2222")
	t.Setenv("BKCRAB_PARSER_EVAL_JUDGE_TIMEOUT_MS", "3333")
	t.Setenv("BKCRAB_PARSER_EVAL_MAX_FILE_BYTES", "4444")
	t.Setenv("BKCRAB_PARSER_EVAL_MAX_FILES", "12")
	t.Setenv("BKCRAB_PARSER_EVAL_MAX_BATCH_BYTES", "55555")
	t.Setenv("BKCRAB_PARSER_EVAL_RETENTION_DAYS", "45")
	t.Setenv("BKCRAB_PARSER_EVAL_MAX_PAGES", "5")
	t.Setenv("BKCRAB_PARSER_EVAL_RENDER_DPI", "120")
	t.Setenv("BKCRAB_PARSER_EVAL_MARKDOWN_JUDGE_CHARS", "12345")

	env := LoadEnv()
	got := env.ParserEvaluation
	if !got.Enabled || got.WorkerEnabled || got.RendererEndpoint != "http://renderer.internal:8181" ||
		got.MarkItDownEndpoint != "http://mark.internal:8182" || got.AnyDocEndpoint != "http://any.internal:8183" ||
		got.RenderTimeoutMS != 1111 || got.ParseTimeoutMS != 2222 || got.JudgeTimeoutMS != 3333 ||
		got.MaxFileBytes != 4444 || got.MaxFiles != 12 || got.MaxBatchBytes != 55555 ||
		got.RetentionDays != 45 || got.MaxPages != 5 || got.RenderDPI != 120 || got.MarkdownJudgeChars != 12345 {
		t.Fatalf("environment overlay mismatch: %+v", got)
	}

	var runtime Config
	env.ApplyToConfig(&runtime)
	if runtime.ParserEvaluation != got {
		t.Fatalf("runtime parser evaluation=%+v, want %+v", runtime.ParserEvaluation, got)
	}
}

func TestApplyDefaultsIncludesParserEvaluation(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.ParserEvaluation.MaxFiles != 50 || !cfg.ParserEvaluation.WorkerEnabled {
		t.Fatalf("parser evaluation defaults not applied: %+v", cfg.ParserEvaluation)
	}
}
