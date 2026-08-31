package parseeval

import (
	"math"
	"sort"
)

// DocumentView is the complete durable result state needed to recompute a run
// summary. Aggregate intentionally derives all metrics from these JSON slots so
// no third aggregate table or incrementally maintained counters are required.
type DocumentView struct {
	Format               Format
	MarkItDownResultJSON string
	AnyDocResultJSON     string
	JudgeResultJSON      string
}

type decodedDocumentView struct {
	format          Format
	markitdown      ParserResult
	markitdownValid bool
	anydoc          ParserResult
	anydocValid     bool
	judge           JudgeResult
	judgeValid      bool
}

func Aggregate(documents []DocumentView) RunSummary {
	decoded := make([]decodedDocumentView, 0, len(documents))
	for _, document := range documents {
		view := decodedDocumentView{format: document.Format}
		view.markitdown, view.markitdownValid = decodeParserResult(document.MarkItDownResultJSON)
		view.anydoc, view.anydocValid = decodeParserResult(document.AnyDocResultJSON)
		view.judge, view.judgeValid = decodeJudgeResult(document.JudgeResultJSON)
		decoded = append(decoded, view)
	}

	summary := RunSummary{
		MarkItDown: aggregateParser(decoded, EngineMarkItDown),
		AnyDoc:     aggregateParser(decoded, EngineAnyDoc),
	}
	formatQualities := make([]QualityComparison, 0, 3)
	for _, format := range []Format{FormatDOCX, FormatPPTX, FormatXLSX} {
		formatDocuments := filterFormat(decoded, format)
		if len(formatDocuments) == 0 {
			continue
		}
		entry := FormatAggregate{
			Format:     format,
			Documents:  len(formatDocuments),
			MarkItDown: aggregateParser(formatDocuments, EngineMarkItDown),
			AnyDoc:     aggregateParser(formatDocuments, EngineAnyDoc),
		}
		markitdownQuality, anydocQuality := collectQuality(formatDocuments)
		if len(markitdownQuality) > 0 && len(anydocQuality) > 0 {
			entry.Quality = &QualityComparison{MarkItDown: meanQuality(markitdownQuality), AnyDoc: meanQuality(anydocQuality)}
			formatQualities = append(formatQualities, *entry.Quality)
		}
		summary.Formats = append(summary.Formats, entry)
	}
	if len(formatQualities) > 0 {
		markitdown := make([]QualityScore, 0, len(formatQualities))
		anydoc := make([]QualityScore, 0, len(formatQualities))
		for _, quality := range formatQualities {
			markitdown = append(markitdown, quality.MarkItDown)
			anydoc = append(anydoc, quality.AnyDoc)
		}
		summary.MacroQuality = &QualityComparison{MarkItDown: meanQuality(markitdown), AnyDoc: meanQuality(anydoc)}
	}

	costKnown := true
	sawPricedJudge := false
	var totalCost float64
	for _, document := range decoded {
		if !document.judgeValid {
			summary.Wins.Failures++
			continue
		}
		summary.Usage = addTokenUsage(summary.Usage, document.judge.Usage)
		sawPricedJudge = true
		if document.judge.EstimatedCostUSD == nil {
			costKnown = false
		} else {
			totalCost += *document.judge.EstimatedCostUSD
		}
		if document.judge.Status != StepSucceeded || document.judge.Winner == nil {
			summary.Wins.Failures++
			continue
		}
		switch *document.judge.Winner {
		case WinnerMarkItDown:
			summary.Wins.MarkItDown++
		case WinnerAnyDoc:
			summary.Wins.AnyDoc++
		case WinnerTie:
			summary.Wins.Ties++
		default:
			summary.Wins.Failures++
		}
	}
	if sawPricedJudge && costKnown {
		summary.EstimatedCostUSD = &totalCost
	}
	return summary
}

func decodeParserResult(raw string) (ParserResult, bool) {
	result, err := DecodeClosedJSON[ParserResult]([]byte(raw), MaxResultJSONBytes, func(value *ParserResult) error { return value.Validate() })
	return result, err == nil
}

func decodeJudgeResult(raw string) (JudgeResult, bool) {
	result, err := DecodeClosedJSON[JudgeResult]([]byte(raw), MaxResultJSONBytes, func(value *JudgeResult) error { return value.Validate() })
	return result, err == nil
}

func filterFormat(documents []decodedDocumentView, format Format) []decodedDocumentView {
	filtered := make([]decodedDocumentView, 0, len(documents))
	for _, document := range documents {
		if document.format == format {
			filtered = append(filtered, document)
		}
	}
	return filtered
}

func aggregateParser(documents []decodedDocumentView, engine Engine) ParserAggregate {
	aggregate := ParserAggregate{Documents: len(documents)}
	parseDurations := make([]float64, 0, len(documents))
	endToEndDurations := make([]float64, 0, len(documents))
	for _, document := range documents {
		result, valid := document.markitdown, document.markitdownValid
		if engine == EngineAnyDoc {
			result, valid = document.anydoc, document.anydocValid
		}
		if !valid || !result.Successful() {
			continue
		}
		aggregate.Successes++
		if result.ParseDurationMS != nil {
			parseDurations = append(parseDurations, float64(*result.ParseDurationMS))
		}
		if result.EndToEndDurationMS != nil {
			endToEndDurations = append(endToEndDurations, float64(*result.EndToEndDurationMS))
		}
	}
	if aggregate.Documents > 0 {
		aggregate.SuccessRate = float64(aggregate.Successes) / float64(aggregate.Documents)
	}
	aggregate.Parse = summarizeDurations(parseDurations)
	aggregate.EndToEnd = summarizeDurations(endToEndDurations)
	markitdownQuality, anydocQuality := collectQuality(documents)
	quality := markitdownQuality
	if engine == EngineAnyDoc {
		quality = anydocQuality
	}
	if len(quality) > 0 {
		mean := meanQuality(quality)
		aggregate.Quality = &mean
	}
	return aggregate
}

func summarizeDurations(values []float64) DurationAggregate {
	result := DurationAggregate{Samples: len(values)}
	if len(values) == 0 {
		return result
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	median := ordered[middle]
	if len(ordered)%2 == 0 {
		median = (ordered[middle-1] + ordered[middle]) / 2
	}
	percentileIndex := int(math.Ceil(0.95*float64(len(ordered)))) - 1
	p95 := ordered[percentileIndex]
	result.Median = &median
	result.P95 = &p95
	return result
}

func collectQuality(documents []decodedDocumentView) ([]QualityScore, []QualityScore) {
	markitdown := make([]QualityScore, 0, len(documents))
	anydoc := make([]QualityScore, 0, len(documents))
	for _, document := range documents {
		if !document.judgeValid || document.judge.Status != StepSucceeded || document.judge.MarkItDownScore == nil || document.judge.AnyDocScore == nil {
			continue
		}
		markitdown = append(markitdown, *document.judge.MarkItDownScore)
		anydoc = append(anydoc, *document.judge.AnyDocScore)
	}
	return markitdown, anydoc
}

func meanQuality(values []QualityScore) QualityScore {
	var result QualityScore
	for _, value := range values {
		result.Completeness += value.Completeness
		result.Structure += value.Structure
		result.Formatting += value.Formatting
		result.Cleanliness += value.Cleanliness
		result.Total += value.Total
	}
	divisor := float64(len(values))
	if divisor > 0 {
		result.Completeness /= divisor
		result.Structure /= divisor
		result.Formatting /= divisor
		result.Cleanliness /= divisor
		result.Total /= divisor
	}
	return result
}
