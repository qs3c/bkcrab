package eval

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/qs3c/bkcrab/internal/store"
)

type ExportStore interface {
	GetRAGEvalRun(context.Context, string) (*store.RAGEvalRunRecord, error)
	ListRAGEvalCaseResults(context.Context, string, string, int) ([]store.RAGEvalCaseResultRecord, error)
	ListRAGEvalMetricResults(context.Context, string, string, int) ([]store.RAGEvalMetricResultRecord, error)
}

type ExportAuthorizer interface {
	AuthorizeRAGEvalExport(context.Context, string, *store.RAGEvalRunRecord) error
}

type TraceObjectSink interface {
	PutRAGEvalTrace(context.Context, string, string, string, json.RawMessage) (string, error)
}

type ExportFormat string

const (
	ExportJSON ExportFormat = "json"
	ExportCSV  ExportFormat = "csv"
)

type ExportService struct {
	Store            ExportStore
	Authorizer       ExportAuthorizer
	TraceSink        TraceObjectSink
	InlineTraceBytes int
}

type ExportRecord struct {
	RunID          string          `json:"runId"`
	CaseID         string          `json:"caseId"`
	CaseStatus     string          `json:"caseStatus"`
	Response       string          `json:"response,omitempty"`
	Contexts       json.RawMessage `json:"contexts"`
	MetricName     string          `json:"metricName,omitempty"`
	MetricVersion  string          `json:"metricVersion,omitempty"`
	MetricStatus   string          `json:"metricStatus,omitempty"`
	MetricValue    *float64        `json:"metricValue,omitempty"`
	MetricReason   string          `json:"metricReason,omitempty"`
	SearchTrace    json.RawMessage `json:"searchTrace,omitempty"`
	SearchTraceRef string          `json:"searchTraceRef,omitempty"`
	AnswerTrace    json.RawMessage `json:"answerTrace,omitempty"`
	AnswerTraceRef string          `json:"answerTraceRef,omitempty"`
}

// Export reauthorizes every call and writes incrementally to the destination.
// Large traces must cross a controlled object sink and are represented only by
// opaque references in the export stream.
func (s ExportService) Export(ctx context.Context, actorID, runID string, format ExportFormat, includeTraces bool, dst io.Writer) error {
	if s.Store == nil || s.Authorizer == nil || dst == nil {
		return errors.New("export dependencies are incomplete")
	}
	run, err := s.Store.GetRAGEvalRun(ctx, runID)
	if err != nil {
		return err
	}
	if err = s.Authorizer.AuthorizeRAGEvalExport(ctx, actorID, run); err != nil {
		return err
	}
	if format != ExportJSON && format != ExportCSV {
		return errors.New("unsupported export format")
	}
	results, err := s.loadCaseResults(ctx, runID)
	if err != nil {
		return err
	}
	metrics, err := s.loadMetricResults(ctx, runID)
	if err != nil {
		return err
	}
	byCase := make(map[string][]store.RAGEvalMetricResultRecord, len(results))
	for _, metric := range metrics {
		byCase[metric.CaseID] = append(byCase[metric.CaseID], metric)
	}
	write := func(record ExportRecord) error { return nil }
	var csvWriter *csv.Writer
	firstJSON := true
	if format == ExportJSON {
		if _, err = io.WriteString(dst, "[\n"); err != nil {
			return err
		}
		write = func(record ExportRecord) error {
			if !firstJSON {
				if _, err := io.WriteString(dst, ",\n"); err != nil {
					return err
				}
			}
			firstJSON = false
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			_, err = dst.Write(encoded)
			return err
		}
	} else {
		csvWriter = csv.NewWriter(dst)
		if err = csvWriter.Write([]string{"run_id", "case_id", "case_status", "response", "contexts_json", "metric_name", "metric_version", "metric_status", "metric_value", "metric_reason", "search_trace", "search_trace_ref", "answer_trace", "answer_trace_ref"}); err != nil {
			return err
		}
		write = func(record ExportRecord) error {
			value := ""
			if record.MetricValue != nil {
				value = strconv.FormatFloat(*record.MetricValue, 'g', -1, 64)
			}
			cells := []string{record.RunID, record.CaseID, record.CaseStatus, record.Response, string(record.Contexts), record.MetricName, record.MetricVersion, record.MetricStatus, value, record.MetricReason, string(record.SearchTrace), record.SearchTraceRef, string(record.AnswerTrace), record.AnswerTraceRef}
			for index := range cells {
				cells[index] = safeCSVCell(cells[index])
			}
			return csvWriter.Write(cells)
		}
	}
	for _, result := range results {
		base := ExportRecord{RunID: runID, CaseID: result.CaseID, CaseStatus: result.Status, Response: result.Response, Contexts: validRaw(result.ContextsJSON)}
		if includeTraces {
			if base.SearchTrace, base.SearchTraceRef, err = s.trace(ctx, runID, result.CaseID, "search", result.SearchTraceJSON); err != nil {
				return err
			}
			if base.AnswerTrace, base.AnswerTraceRef, err = s.trace(ctx, runID, result.CaseID, "answer", result.AnswerTraceJSON); err != nil {
				return err
			}
		}
		caseMetrics := byCase[result.CaseID]
		if len(caseMetrics) == 0 {
			if err = write(base); err != nil {
				return err
			}
			continue
		}
		for _, metric := range caseMetrics {
			record := base
			record.MetricName = metric.MetricName
			record.MetricVersion = metric.MetricVersion
			record.MetricStatus = metric.Status
			record.MetricReason = metric.Reason
			if metric.Value.Valid {
				value := metric.Value.Float64
				record.MetricValue = &value
			}
			if err = write(record); err != nil {
				return err
			}
		}
	}
	if format == ExportJSON {
		_, err = io.WriteString(dst, "\n]\n")
		return err
	}
	csvWriter.Flush()
	return csvWriter.Error()
}

func (s ExportService) loadCaseResults(ctx context.Context, runID string) ([]store.RAGEvalCaseResultRecord, error) {
	out := []store.RAGEvalCaseResultRecord{}
	cursor := ""
	for {
		items, err := s.Store.ListRAGEvalCaseResults(ctx, runID, cursor, 200)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		out = append(out, items...)
		cursor = items[len(items)-1].CaseID
	}
	return out, nil
}
func (s ExportService) loadMetricResults(ctx context.Context, runID string) ([]store.RAGEvalMetricResultRecord, error) {
	out := []store.RAGEvalMetricResultRecord{}
	cursor := ""
	for {
		items, err := s.Store.ListRAGEvalMetricResults(ctx, runID, cursor, 200)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		out = append(out, items...)
		last := items[len(items)-1]
		cursor = last.CaseID + ":" + last.MetricName
	}
	return out, nil
}

func (s ExportService) trace(ctx context.Context, runID, caseID, kind, raw string) (json.RawMessage, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", nil
	}
	if !json.Valid([]byte(raw)) {
		return nil, "", fmt.Errorf("invalid %s trace", kind)
	}
	limit := s.InlineTraceBytes
	if limit <= 0 {
		limit = 4 << 10
	}
	if len(raw) <= limit {
		return json.RawMessage(raw), "", nil
	}
	if s.TraceSink == nil {
		return nil, "", errors.New("large trace requires controlled object sink")
	}
	ref, err := s.TraceSink.PutRAGEvalTrace(ctx, runID, caseID, kind, json.RawMessage(raw))
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(ref) == "" {
		return nil, "", errors.New("trace sink returned empty reference")
	}
	return nil, ref, nil
}
func validRaw(raw string) json.RawMessage {
	if json.Valid([]byte(raw)) {
		return json.RawMessage(raw)
	}
	return json.RawMessage("[]")
}

func safeCSVCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}
