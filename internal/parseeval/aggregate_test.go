package parseeval

import (
	"strings"
	"testing"
)

func pointer[T any](value T) *T { return &value }

func encodedParserResult(t *testing.T, status StepStatus, parseMS, endToEndMS *int64) string {
	t.Helper()
	result := ParserResult{Status: status, ParseDurationMS: parseMS, EndToEndDurationMS: endToEndMS}
	if status == StepSucceeded {
		result.Descriptor = ParserDescriptor{Name: "parser", Version: "1", WrapperVersion: "1"}
		result.Markdown = StoredArtifact{ObjectKey: "parser-eval/runs/run/documents/doc/outputs/result.md", SHA256: strings.Repeat("b", 64), MediaType: "text/markdown", ByteSize: 10}
		result.Stats = StructureStats{Version: "parser-structure-stats-v1", Characters: 10}
	} else {
		result.Error = ErrorDetail{Code: "parse_failed", Message: "parse failed"}
	}
	raw, err := EncodeBoundedJSON(result, MaxResultJSONBytes)
	if err != nil {
		t.Fatalf("encode parser result: %v", err)
	}
	return raw
}

func uniformQuality(value float64) QualityScore {
	return QualityScore{Completeness: value, Structure: value, Formatting: value, Cleanliness: value, Total: value}
}

func encodedJudgeResult(t *testing.T, status StepStatus, markitdown, anydoc float64, winner Winner, usage TokenUsage, cost *float64) string {
	t.Helper()
	result := JudgeResult{
		Status:           status,
		MarkItDownA:      JudgeSlot{Order: JudgeMarkItDownA, Status: StepFailed, Error: ErrorDetail{Code: "slot_failed", Message: "failed"}},
		AnyDocA:          JudgeSlot{Order: JudgeAnyDocA, Status: StepFailed, Error: ErrorDetail{Code: "slot_failed", Message: "failed"}},
		Usage:            usage,
		EstimatedCostUSD: cost,
	}
	if status == StepSucceeded {
		artifactA := StoredArtifact{ObjectKey: "parser-eval/runs/run/documents/doc/judge/markitdown-a.json", SHA256: strings.Repeat("c", 64), MediaType: "application/json", ByteSize: 10}
		artifactB := artifactA
		artifactB.ObjectKey = "parser-eval/runs/run/documents/doc/judge/anydoc-a.json"
		verdict := BlindVerdict{A: BlindScores{5, 5, 5, 5}, B: BlindScores{4, 4, 4, 4}, Winner: PositionWinnerA, Reason: "valid"}
		result.MarkItDownA = JudgeSlot{Order: JudgeMarkItDownA, Status: StepSucceeded, Verdict: verdict, Raw: artifactA}
		result.AnyDocA = JudgeSlot{Order: JudgeAnyDocA, Status: StepSucceeded, Verdict: verdict, Raw: artifactB}
		result.MarkItDownScore = pointer(uniformQuality(markitdown))
		result.AnyDocScore = pointer(uniformQuality(anydoc))
		result.Winner = pointer(winner)
	} else {
		result.Error = ErrorDetail{Code: "judge_incomplete", Message: "failed"}
	}
	raw, err := EncodeBoundedJSON(result, MaxResultJSONBytes)
	if err != nil {
		t.Fatalf("encode judge result: %v", err)
	}
	return raw
}

func TestDurationSummaryMedianAndNearestRankP95(t *testing.T) {
	for name, test := range map[string]struct {
		values []float64
		median float64
		p95    float64
	}{
		"even": {values: []float64{20, 10}, median: 15, p95: 20},
		"odd":  {values: []float64{100, 10, 20}, median: 20, p95: 100},
		"p95":  {values: []float64{20, 1, 12, 4, 18, 8, 15, 3, 17, 6, 11, 2, 14, 5, 19, 7, 13, 9, 16, 10}, median: 10.5, p95: 19},
	} {
		t.Run(name, func(t *testing.T) {
			got := summarizeDurations(test.values)
			if got.Samples != len(test.values) || got.Median == nil || *got.Median != test.median || got.P95 == nil || *got.P95 != test.p95 {
				t.Fatalf("summary = %+v", got)
			}
		})
	}
	if got := summarizeDurations(nil); got.Samples != 0 || got.Median != nil || got.P95 != nil {
		t.Fatalf("empty summary = %+v", got)
	}
}

func TestAggregateRecomputesMetricsWithoutFillingMissingValues(t *testing.T) {
	knownCost := 0.25
	documents := []DocumentView{
		{
			Format:               FormatDOCX,
			MarkItDownResultJSON: encodedParserResult(t, StepSucceeded, pointer(int64(10)), pointer(int64(100))),
			AnyDocResultJSON:     encodedParserResult(t, StepSucceeded, pointer(int64(40)), pointer(int64(140))),
			JudgeResultJSON:      encodedJudgeResult(t, StepSucceeded, 80, 60, WinnerMarkItDown, TokenUsage{InputTokens: 10, OutputTokens: 2}, &knownCost),
		},
		{
			Format:               FormatDOCX,
			MarkItDownResultJSON: encodedParserResult(t, StepSucceeded, pointer(int64(20)), pointer(int64(200))),
			AnyDocResultJSON:     encodedParserResult(t, StepFailed, nil, nil),
			JudgeResultJSON:      encodedJudgeResult(t, StepFailed, 0, 0, WinnerTie, TokenUsage{InputTokens: 3}, &knownCost),
		},
		{
			Format:               FormatPPTX,
			MarkItDownResultJSON: encodedParserResult(t, StepSucceeded, pointer(int64(30)), pointer(int64(300))),
			AnyDocResultJSON:     encodedParserResult(t, StepSucceeded, pointer(int64(50)), pointer(int64(150))),
			JudgeResultJSON:      encodedJudgeResult(t, StepSucceeded, 40, 80, WinnerAnyDoc, TokenUsage{InputTokens: 20, OutputTokens: 4}, nil),
		},
		{
			Format:               FormatPPTX,
			MarkItDownResultJSON: encodedParserResult(t, StepSucceeded, nil, pointer(int64(400))),
			AnyDocResultJSON:     encodedParserResult(t, StepSucceeded, pointer(int64(60)), nil),
			JudgeResultJSON:      encodedJudgeResult(t, StepSucceeded, 60, 60, WinnerTie, TokenUsage{InputTokens: 30, OutputTokens: 6}, &knownCost),
		},
		{
			Format:               FormatXLSX,
			MarkItDownResultJSON: encodedParserResult(t, StepFailed, nil, nil),
			AnyDocResultJSON:     encodedParserResult(t, StepSucceeded, pointer(int64(70)), pointer(int64(170))),
			JudgeResultJSON:      `{"status":"PENDING"}`,
		},
	}

	summary := Aggregate(documents)
	if summary.MarkItDown.Documents != 5 || summary.MarkItDown.Successes != 4 || summary.MarkItDown.SuccessRate != 0.8 {
		t.Fatalf("markitdown aggregate = %+v", summary.MarkItDown)
	}
	if summary.AnyDoc.Documents != 5 || summary.AnyDoc.Successes != 4 || summary.AnyDoc.SuccessRate != 0.8 {
		t.Fatalf("anydoc aggregate = %+v", summary.AnyDoc)
	}
	if summary.MarkItDown.Parse.Samples != 3 || *summary.MarkItDown.Parse.Median != 20 || summary.AnyDoc.Parse.Samples != 4 || *summary.AnyDoc.Parse.Median != 55 {
		t.Fatalf("duration samples: markitdown=%+v anydoc=%+v", summary.MarkItDown.Parse, summary.AnyDoc.Parse)
	}
	if summary.MarkItDown.Quality == nil || summary.MarkItDown.Quality.Total != 60 || summary.AnyDoc.Quality == nil || summary.AnyDoc.Quality.Total != 200.0/3 {
		t.Fatalf("global quality: markitdown=%+v anydoc=%+v", summary.MarkItDown.Quality, summary.AnyDoc.Quality)
	}
	if len(summary.Formats) != 3 || summary.Formats[0].Format != FormatDOCX || summary.Formats[1].Format != FormatPPTX || summary.Formats[2].Format != FormatXLSX {
		t.Fatalf("format order = %+v", summary.Formats)
	}
	if summary.Formats[0].Quality == nil || summary.Formats[0].Quality.MarkItDown.Total != 80 || summary.Formats[1].Quality == nil || summary.Formats[1].Quality.MarkItDown.Total != 50 || summary.Formats[2].Quality != nil {
		t.Fatalf("format quality = %+v", summary.Formats)
	}
	if summary.MacroQuality == nil || summary.MacroQuality.MarkItDown.Total != 65 || summary.MacroQuality.AnyDoc.Total != 65 {
		t.Fatalf("macro quality = %+v", summary.MacroQuality)
	}
	if summary.Wins != (WinCounts{MarkItDown: 1, AnyDoc: 1, Ties: 1, Failures: 2}) {
		t.Fatalf("wins = %+v", summary.Wins)
	}
	if summary.Usage.InputTokens != 63 || summary.Usage.OutputTokens != 12 {
		t.Fatalf("usage = %+v", summary.Usage)
	}
	if summary.EstimatedCostUSD != nil {
		t.Fatalf("unknown pricing must make total cost nil, got %v", *summary.EstimatedCostUSD)
	}
	if err := summary.Validate(); err != nil {
		t.Fatalf("validate summary: %v", err)
	}
}

func TestAggregateKnownCostsAndMalformedJSON(t *testing.T) {
	cost := 0.125
	summary := Aggregate([]DocumentView{
		{Format: FormatDOCX, MarkItDownResultJSON: `{`, AnyDocResultJSON: `{`, JudgeResultJSON: encodedJudgeResult(t, StepFailed, 0, 0, WinnerTie, TokenUsage{InputTokens: 2}, &cost)},
		{Format: FormatDOCX, MarkItDownResultJSON: `{"status":"PENDING"}`, AnyDocResultJSON: `{"status":"PENDING"}`, JudgeResultJSON: encodedJudgeResult(t, StepFailed, 0, 0, WinnerTie, TokenUsage{OutputTokens: 3}, &cost)},
	})
	if summary.MarkItDown.Successes != 0 || summary.MarkItDown.Parse.Median != nil || summary.Wins.Failures != 2 {
		t.Fatalf("malformed/pending handling = %+v", summary)
	}
	if summary.EstimatedCostUSD == nil || *summary.EstimatedCostUSD != 0.25 {
		t.Fatalf("cost = %v", summary.EstimatedCostUSD)
	}
}
