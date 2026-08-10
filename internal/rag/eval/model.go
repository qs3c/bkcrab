// Package eval contains the model-agnostic RAG evaluation contracts.
package eval

import (
	"encoding/json"

	"github.com/qs3c/bkcrab/internal/config"
)

const (
	MetricBundleV1                   = "rag-core-v1"
	ExpectedEvaluatorProtocolVersion = "rag-evaluator-v1"
	ExpectedRagasVersion             = "0.3.9"
)

type CorpusDocument struct {
	ID        string         `json:"id"`
	FileName  string         `json:"fileName"`
	MediaType string         `json:"mediaType"`
	ObjectKey string         `json:"objectKey,omitempty"`
	SHA256    string         `json:"sha256"`
	SizeBytes int64          `json:"sizeBytes"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type Case struct {
	ID                  string         `json:"id"`
	UserInput           string         `json:"user_input"`
	Reference           string         `json:"reference,omitempty"`
	ReferenceContexts   []string       `json:"reference_contexts,omitempty"`
	ReferenceContextIDs []string       `json:"reference_context_ids,omitempty"`
	History             []string       `json:"history,omitempty"`
	ExpectedAbstention  bool           `json:"expected_abstention"`
	Tags                []string       `json:"tags,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

type CanonicalDataset struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Corpus      []CorpusDocument `json:"corpus"`
	Cases       []Case           `json:"cases"`
}

type Profile struct {
	Name string                    `json:"name"`
	Data config.RAGEvalProfileData `json:"data"`
}

type RunMode string

const (
	RunModeFull       RunMode = "FULL_PIPELINE"
	RunModeOnlineOnly RunMode = "ONLINE_ONLY"
)

func (m RunMode) Valid() bool { return m == RunModeFull || m == RunModeOnlineOnly }

type MetricStatus string

const (
	MetricOK                  MetricStatus = "ok"
	MetricSkippedMissingInput MetricStatus = "skipped_missing_input"
	MetricError               MetricStatus = "error"
)

type MetricResult struct {
	Status  MetricStatus   `json:"status"`
	Value   *float64       `json:"value,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

func score(value float64) *float64 { return &value }

func CloneJSON(value any) (json.RawMessage, error) { return json.Marshal(value) }
