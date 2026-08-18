package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type TATQAAdapter struct{}

func (TATQAAdapter) ID() string { return CatalogTATQA }

type tatQATable struct {
	UID  string     `json:"uid"`
	Rows [][]string `json:"table"`
}

type tatQAParagraph struct {
	UID   string `json:"uid"`
	Order int    `json:"order"`
	Text  string `json:"text"`
}

type tatQAQuestion struct {
	UID           string          `json:"uid"`
	Order         int             `json:"order"`
	Question      string          `json:"question"`
	Answer        json.RawMessage `json:"answer"`
	Derivation    string          `json:"derivation"`
	AnswerType    string          `json:"answer_type"`
	AnswerFrom    string          `json:"answer_from"`
	RelParagraphs []string        `json:"rel_paragraphs"`
	Comparison    bool            `json:"req_comparison"`
	Scale         string          `json:"scale"`
}

type tatQAContext struct {
	Table      tatQATable       `json:"table"`
	Paragraphs []tatQAParagraph `json:"paragraphs"`
	Questions  []tatQAQuestion  `json:"questions"`
}

func (TATQAAdapter) Prepare(ctx context.Context, source CatalogSource, options CatalogImportOptions) (_ *PreparedCatalogDataset, retErr error) {
	if source == nil {
		return nil, errors.New("catalog source is required")
	}
	if err := options.ApplyDefaults(); err != nil {
		return nil, err
	}
	if options.CatalogID != CatalogTATQA || options.Track != DatasetTrackTextRAG {
		return nil, errors.New("TAT-QA adapter options are invalid")
	}
	preset, _ := CatalogPresetByID(options.CatalogID)
	prepared, err := newPreparedCatalogDataset(preset.Name, options.Track, DatasetSource{
		CatalogID: preset.ID, URL: preset.SourceURL, Revision: preset.Revision, AdapterID: preset.ID,
		AdapterVersion: preset.AdapterVersion, Split: options.Split, SampleSize: options.SampleSize, Seed: options.Seed, License: preset.License,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = prepared.Close()
		}
	}()
	logicalPath := map[string]string{
		"dev":       "tatqa_dataset_dev.json",
		"train":     "tatqa_dataset_train.json",
		"test_gold": "tatqa_dataset_test_gold.json",
	}[options.Split]
	reader, err := source.Open(ctx, logicalPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var contexts []tatQAContext
	decoder := json.NewDecoder(io.LimitReader(reader, 32<<20))
	if err = decoder.Decode(&contexts); err != nil {
		return nil, fmt.Errorf("decode TAT-QA %s: %w", options.Split, err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("TAT-QA source contains trailing JSON")
	}
	cases := make([]Case, 0, 20_000)
	for contextIndex, item := range contexts {
		sort.SliceStable(item.Paragraphs, func(i, j int) bool { return item.Paragraphs[i].Order < item.Paragraphs[j].Order })
		tableMarkdown := renderMarkdownTable(item.Table.Rows)
		var body strings.Builder
		body.WriteString("## Table\n\n")
		body.WriteString(tableMarkdown)
		body.WriteString("\n\n## Paragraphs\n")
		paragraphByOrder := make(map[string]string, len(item.Paragraphs))
		for _, paragraph := range item.Paragraphs {
			text := strings.TrimSpace(paragraph.Text)
			paragraphByOrder[strconv.Itoa(paragraph.Order)] = text
			body.WriteString("\n### Paragraph ")
			body.WriteString(strconv.Itoa(paragraph.Order))
			body.WriteString("\n\n")
			body.WriteString(text)
			body.WriteByte('\n')
		}
		sourceID := strings.TrimSpace(item.Table.UID)
		if sourceID == "" {
			sourceID = fmt.Sprintf("%s-context-%d", options.Split, contextIndex)
		}
		documentID := catalogDocumentID("tat", sourceID)
		if err = prepared.AddDocument(documentID, documentID+".md", "text/markdown", markdownBytes("TAT-QA financial context", body.String()), map[string]any{
			"sourceContextId": sourceID, "split": options.Split, "tableUid": item.Table.UID,
		}); err != nil {
			return nil, err
		}
		for _, question := range item.Questions {
			if strings.TrimSpace(question.UID) == "" || strings.TrimSpace(question.Question) == "" {
				continue
			}
			referenceContexts := []string{}
			seen := map[string]struct{}{}
			for _, order := range question.RelParagraphs {
				if text := paragraphByOrder[strings.TrimSpace(order)]; text != "" {
					if _, duplicate := seen[text]; !duplicate {
						seen[text] = struct{}{}
						referenceContexts = append(referenceContexts, text)
					}
				}
			}
			if strings.Contains(strings.ToLower(question.AnswerFrom), "table") && tableMarkdown != "" {
				referenceContexts = append(referenceContexts, tableMarkdown)
			}
			answer, answerErr := renderTATQAAnswer(question.Answer)
			if answerErr != nil {
				return nil, fmt.Errorf("decode TAT-QA answer %s: %w", question.UID, answerErr)
			}
			cases = append(cases, Case{ID: "tat_" + question.UID, UserInput: strings.TrimSpace(question.Question), Reference: answer,
				ReferenceContexts: referenceContexts, ReferenceDocumentIDs: []string{documentID},
				Tags: uniqueSortedStrings([]string{question.AnswerType, question.AnswerFrom}), Metadata: map[string]any{
					"sourceQuestionId": question.UID, "sourceContextId": sourceID, "answerType": question.AnswerType,
					"answerFrom": question.AnswerFrom, "scale": question.Scale, "derivation": question.Derivation,
					"requiresComparison": question.Comparison, "questionOrder": question.Order,
				}})
		}
	}
	selected, err := selectCatalogCases(preset.ID+"@"+preset.Revision+"/"+options.Split, cases, options.SampleSize, options.Seed)
	if err != nil {
		return nil, err
	}
	prepared.Dataset.Description = preset.Description
	prepared.Dataset.Cases = selected
	prepared.Dataset.Source.SampleSize = len(selected)
	return prepared, nil
}

func renderTATQAAnswer(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed), nil
	case json.Number:
		return typed.String(), nil
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			switch child := item.(type) {
			case string:
				parts = append(parts, child)
			case json.Number:
				parts = append(parts, child.String())
			default:
				return "", errors.New("answer array contains an unsupported value")
			}
		}
		return strings.Join(uniqueStringsPreserveOrder(parts), " ; "), nil
	default:
		return "", errors.New("answer has an unsupported JSON type")
	}
}

func renderMarkdownTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	columns := 0
	for _, row := range rows {
		if len(row) > columns {
			columns = len(row)
		}
	}
	if columns == 0 {
		return ""
	}
	var output strings.Builder
	writeRow := func(row []string) {
		output.WriteString("|")
		for column := 0; column < columns; column++ {
			value := ""
			if column < len(row) {
				value = strings.TrimSpace(row[column])
			}
			value = strings.NewReplacer("|", "\\|", "\r", " ", "\n", "<br>").Replace(value)
			output.WriteString(" ")
			output.WriteString(value)
			output.WriteString(" |")
		}
		output.WriteByte('\n')
	}
	writeRow(rows[0])
	delimiter := make([]string, columns)
	for index := range delimiter {
		delimiter[index] = "---"
	}
	writeRow(delimiter)
	for _, row := range rows[1:] {
		writeRow(row)
	}
	return strings.TrimSpace(output.String())
}

func uniqueStringsPreserveOrder(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
