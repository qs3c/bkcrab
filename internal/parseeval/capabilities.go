package parseeval

import "time"

type CachedDependencyHealth struct {
	Healthy   bool      `json:"healthy"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

type RendererCapability struct {
	Health     CachedDependencyHealth `json:"health"`
	Descriptor RendererDescriptor     `json:"descriptor"`
}

type ParserCapability struct {
	Engine     Engine                 `json:"engine"`
	Health     CachedDependencyHealth `json:"health"`
	Descriptor ParserDescriptor       `json:"descriptor"`
}

type Capabilities struct {
	Enabled            bool                   `json:"enabled"`
	Available          bool                   `json:"available"`
	Reason             string                 `json:"reason,omitempty"`
	WorkerEnabled      bool                   `json:"workerEnabled"`
	SupportedFormats   []Format               `json:"supportedFormats"`
	MaxFiles           int                    `json:"maxFiles"`
	MaxFileBytes       int64                  `json:"maxFileBytes"`
	MaxBatchBytes      int64                  `json:"maxBatchBytes"`
	MaxPages           int                    `json:"maxPages"`
	RenderDPI          int                    `json:"renderDPI"`
	MarkdownJudgeChars int                    `json:"markdownJudgeChars"`
	ScoringDimensions  []string               `json:"scoringDimensions"`
	Renderer           RendererCapability     `json:"renderer"`
	Parsers            []ParserCapability     `json:"parsers"`
	JudgeModelBindings []JudgeBindingSnapshot `json:"judgeModelBindings"`
}
