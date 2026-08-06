package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
)

func decodeClosedJSON(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values are not allowed")
	}
	return nil
}

func DecodeRAGRuntimePolicy(data []byte) (RAGRuntimePolicyData, error) {
	var value RAGRuntimePolicyData
	if err := decodeClosedJSON(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

func DecodeRAGIngestionPolicy(data []byte) (RAGIngestionPolicyData, error) {
	var value RAGIngestionPolicyData
	if err := decodeClosedJSON(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

func DecodeRAGEvalProfile(data []byte) (RAGEvalProfileData, error) {
	var value RAGEvalProfileData
	if err := decodeClosedJSON(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

// RAGEvaluationCfg controls the optional, super-admin-only evaluation plane.
// It is disabled by default and is deliberately separate from production RAG.
type RAGEvaluationCfg struct {
	Enabled                 bool            `json:"enabled,omitempty"`
	Sidecar                 RAGEvaluatorCfg `json:"sidecar,omitempty"`
	WorkerConcurrency       int             `json:"workerConcurrency,omitempty"`
	MaxBatchSize            int             `json:"maxBatchSize,omitempty"`
	MaxContextsPerSample    int             `json:"maxContextsPerSample,omitempty"`
	MaxContextBytes         int             `json:"maxContextBytes,omitempty"`
	MaxRequestBytes         int64           `json:"maxRequestBytes,omitempty"`
	MaxRunCases             int             `json:"maxRunCases,omitempty"`
	MaxRunTokens            int64           `json:"maxRunTokens,omitempty"`
	MaxRunCostUSD           float64         `json:"maxRunCostUSD,omitempty"`
	MaxRunDurationSec       int             `json:"maxRunDurationSec,omitempty"`
	RunRetentionDays        int             `json:"runRetentionDays,omitempty"`
	DatasetRetentionDays    int             `json:"datasetRetentionDays,omitempty"`
	GenerationRetentionDays int             `json:"generationRetentionDays,omitempty"`
}

type RAGEvaluatorCfg struct {
	Endpoint            string `json:"endpoint,omitempty"`
	APIKey              string `json:"apiKey,omitempty"`
	TimeoutMS           int    `json:"timeoutMs,omitempty"`
	MetricBundleVersion string `json:"metricBundleVersion,omitempty"`
	LLMProvider         string `json:"llmProvider,omitempty"`
	LLMModel            string `json:"llmModel,omitempty"`
	EmbeddingProvider   string `json:"embeddingProvider,omitempty"`
	EmbeddingModel      string `json:"embeddingModel,omitempty"`
}

func (c RAGEvaluatorCfg) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("endpoint", c.Endpoint),
		slog.Int("timeoutMs", c.TimeoutMS),
		slog.String("metricBundleVersion", c.MetricBundleVersion),
		slog.String("llmProvider", c.LLMProvider),
		slog.String("llmModel", c.LLMModel),
		slog.String("embeddingProvider", c.EmbeddingProvider),
		slog.String("embeddingModel", c.EmbeddingModel),
	)
}

func (c *RAGEvaluationCfg) ApplyDefaults() {
	if c.Sidecar.TimeoutMS <= 0 {
		c.Sidecar.TimeoutMS = 120_000
	}
	if c.Sidecar.MetricBundleVersion == "" {
		c.Sidecar.MetricBundleVersion = "rag-core-v1"
	}
	if c.WorkerConcurrency <= 0 {
		c.WorkerConcurrency = 1
	}
	if c.MaxBatchSize <= 0 {
		c.MaxBatchSize = 16
	}
	if c.MaxContextsPerSample <= 0 {
		c.MaxContextsPerSample = 20
	}
	if c.MaxContextBytes <= 0 {
		c.MaxContextBytes = 64 * 1024
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = 4 * 1024 * 1024
	}
	if c.MaxRunCases <= 0 {
		c.MaxRunCases = 1_000
	}
	if c.MaxRunTokens <= 0 {
		c.MaxRunTokens = 2_000_000
	}
	if c.MaxRunCostUSD <= 0 {
		c.MaxRunCostUSD = 25
	}
	if c.MaxRunDurationSec <= 0 {
		c.MaxRunDurationSec = 6 * 60 * 60
	}
	if c.RunRetentionDays <= 0 {
		c.RunRetentionDays = 90
	}
	if c.DatasetRetentionDays <= 0 {
		c.DatasetRetentionDays = 365
	}
	if c.GenerationRetentionDays <= 0 {
		c.GenerationRetentionDays = 30
	}
}

func (c RAGEvaluationCfg) Validate() error {
	if c.Enabled && strings.TrimSpace(c.Sidecar.Endpoint) == "" {
		return errors.New("rag.evaluation.sidecar.endpoint is required when evaluation is enabled")
	}
	if c.Sidecar.TimeoutMS < 0 || c.WorkerConcurrency < 0 || c.WorkerConcurrency > 32 ||
		c.MaxBatchSize < 0 || c.MaxBatchSize > 256 || c.MaxContextsPerSample < 0 ||
		c.MaxContextsPerSample > 100 || c.MaxContextBytes < 0 || c.MaxRequestBytes < 0 ||
		c.MaxRunCases < 0 || c.MaxRunCases > 100_000 || c.MaxRunTokens < 0 ||
		c.MaxRunDurationSec < 0 || c.RunRetentionDays < 0 || c.DatasetRetentionDays < 0 ||
		c.GenerationRetentionDays < 0 {
		return errors.New("rag.evaluation contains an invalid limit")
	}
	if math.IsNaN(c.MaxRunCostUSD) || math.IsInf(c.MaxRunCostUSD, 0) || c.MaxRunCostUSD < 0 {
		return errors.New("rag.evaluation.maxRunCostUSD must be finite and non-negative")
	}
	if v := strings.TrimSpace(c.Sidecar.MetricBundleVersion); v != "" && v != "rag-core-v1" {
		return fmt.Errorf("rag.evaluation.sidecar.metricBundleVersion %q is unsupported", v)
	}
	return nil
}

type RAGRerankerFailurePolicy string

const (
	RAGRerankerFallbackRRF RAGRerankerFailurePolicy = "fallback_rrf"
	RAGRerankerFailClosed  RAGRerankerFailurePolicy = "fail_closed"
)

func (p RAGRerankerFailurePolicy) Valid() bool {
	return p == "" || p == RAGRerankerFallbackRRF || p == RAGRerankerFailClosed
}

// RAGRuntimePolicyData is a closed, immutable online-policy payload.
type RAGRuntimePolicyData struct {
	Version                int64   `json:"version"`
	TopN                   int     `json:"topN"`
	CandidateTopK          int     `json:"candidateTopK"`
	MinScore               float64 `json:"minScore"`
	Temperature            float64 `json:"temperature"`
	MaxTokens              int     `json:"maxTokens"`
	RAGPromptBundleVersion string  `json:"ragPromptBundleVersion"`
}

func (p RAGRuntimePolicyData) Validate() error {
	if p.Version < 0 || p.TopN < 1 || p.TopN > 100 || p.CandidateTopK < p.TopN || p.CandidateTopK > 500 {
		return errors.New("invalid runtime policy topN/candidateTopK/version")
	}
	if math.IsNaN(p.MinScore) || math.IsInf(p.MinScore, 0) || p.MinScore < 0 || p.MinScore > 1 {
		return errors.New("runtime policy minScore must be between 0 and 1")
	}
	if math.IsNaN(p.Temperature) || math.IsInf(p.Temperature, 0) || p.Temperature < 0 || p.Temperature > 2 {
		return errors.New("runtime policy temperature must be between 0 and 2")
	}
	if p.MaxTokens < 1 || p.MaxTokens > 131_072 || strings.TrimSpace(p.RAGPromptBundleVersion) == "" {
		return errors.New("invalid runtime policy answer settings")
	}
	return nil
}

type RAGPolicyEmbeddingData struct {
	ContractFingerprint string `json:"contractFingerprint"`
	Model               string `json:"model"`
	Dims                int    `json:"dims"`
}

type RAGPolicyDocumentAIData struct {
	VisionModel             string `json:"visionModel,omitempty"`
	TextModel               string `json:"textModel,omitempty"`
	VisionPromptVersion     string `json:"visionPromptVersion,omitempty"`
	EnrichmentPromptVersion string `json:"enrichmentPromptVersion,omitempty"`
}

// RAGIngestionPolicyData intentionally contains references, never credentials.
type RAGIngestionPolicyData struct {
	Version           int64                   `json:"version"`
	ChunkSize         int                     `json:"chunkSize"`
	ChunkOverlap      int                     `json:"chunkOverlap"`
	ParseMode         ParseMode               `json:"parseMode"`
	EnrichmentEnabled bool                    `json:"enrichmentEnabled"`
	DocumentAI        RAGPolicyDocumentAIData `json:"documentAI"`
	Embedding         RAGPolicyEmbeddingData  `json:"embedding"`
}

func (p RAGIngestionPolicyData) Validate() error {
	if p.Version < 0 || p.ChunkSize < 128 || p.ChunkSize > 8192 || p.ChunkOverlap < 0 || p.ChunkOverlap >= p.ChunkSize {
		return errors.New("invalid ingestion policy chunk settings")
	}
	if !p.ParseMode.Valid() {
		return fmt.Errorf("invalid ingestion parseMode %q", p.ParseMode)
	}
	if strings.TrimSpace(p.Embedding.ContractFingerprint) == "" || strings.TrimSpace(p.Embedding.Model) == "" || p.Embedding.Dims < 1 || p.Embedding.Dims > 65_536 {
		return errors.New("invalid ingestion policy embedding contract")
	}
	return nil
}

type RAGEvalProfileData struct {
	Ingestion             RAGIngestionPolicyData   `json:"ingestion"`
	Runtime               RAGRuntimePolicyData     `json:"runtime"`
	RewriteEnabled        bool                     `json:"rewriteEnabled"`
	HyDEEnabled           bool                     `json:"hydeEnabled"`
	RerankerEnabled       bool                     `json:"rerankerEnabled"`
	RerankerModel         string                   `json:"rerankerModel,omitempty"`
	RerankerTimeoutMS     int                      `json:"rerankerTimeoutMs"`
	RerankerFailurePolicy RAGRerankerFailurePolicy `json:"rerankerFailurePolicy"`
	AnswerModel           string                   `json:"answerModel"`
}

func (p RAGEvalProfileData) Validate() error {
	if err := p.Ingestion.Validate(); err != nil {
		return err
	}
	if err := p.Runtime.Validate(); err != nil {
		return err
	}
	if !p.RerankerFailurePolicy.Valid() {
		return fmt.Errorf("invalid reranker failure policy %q", p.RerankerFailurePolicy)
	}
	if p.RerankerTimeoutMS < 0 || p.RerankerTimeoutMS > 300_000 {
		return errors.New("invalid reranker timeout")
	}
	if strings.TrimSpace(p.AnswerModel) == "" {
		return errors.New("answer model is required")
	}
	return nil
}

type RAGEvaluationCapabilities struct {
	Enabled             bool     `json:"enabled"`
	SidecarConfigured   bool     `json:"sidecarConfigured"`
	SidecarHealthy      bool     `json:"sidecarHealthy"`
	Reason              string   `json:"reason,omitempty"`
	MetricBundleVersion string   `json:"metricBundleVersion"`
	Metrics             []string `json:"metrics"`
	Importers           []string `json:"importers"`
	MaxBatchSize        int      `json:"maxBatchSize"`
	MaxRunCases         int      `json:"maxRunCases"`
}

func (c RAGEvaluationCfg) Capabilities(healthy bool, reason string) RAGEvaluationCapabilities {
	return RAGEvaluationCapabilities{
		Enabled: c.Enabled, SidecarConfigured: strings.TrimSpace(c.Sidecar.Endpoint) != "",
		SidecarHealthy: healthy, Reason: reason, MetricBundleVersion: c.Sidecar.MetricBundleVersion,
		Metrics:   []string{"context_precision", "context_recall", "faithfulness", "response_relevancy", "factual_correctness", "hit_at_k", "recall_at_k", "mrr", "ndcg", "citation_precision", "citation_coverage", "abstention_accuracy"},
		Importers: []string{"canonical-json"}, MaxBatchSize: c.MaxBatchSize, MaxRunCases: c.MaxRunCases,
	}
}
