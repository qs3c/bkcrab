package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

type OpenRAGBenchAdapter struct{}

func (OpenRAGBenchAdapter) ID() string { return CatalogOpenRAGBench }

type openRAGQuery struct {
	Query  string `json:"query"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

type openRAGQrel struct {
	DocID     string `json:"doc_id"`
	SectionID int    `json:"section_id"`
}

type openRAGSection struct {
	SectionID int               `json:"section_id"`
	Text      string            `json:"text"`
	Tables    map[string]string `json:"tables"`
	Images    map[string]string `json:"images"`
}

type openRAGCorpusDocument struct {
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Abstract string           `json:"abstract"`
	Sections []openRAGSection `json:"sections"`
}

func (OpenRAGBenchAdapter) Prepare(ctx context.Context, source CatalogSource, options CatalogImportOptions) (_ *PreparedCatalogDataset, retErr error) {
	if source == nil {
		return nil, errors.New("catalog source is required")
	}
	if err := options.ApplyDefaults(); err != nil {
		return nil, err
	}
	if options.CatalogID != CatalogOpenRAGBench {
		return nil, errors.New("Open RAGBench adapter options are invalid")
	}
	preset, _ := CatalogPresetByID(options.CatalogID)
	prepared, err := newPreparedCatalogDataset(preset.Name, options.Track, DatasetSource{
		CatalogID: preset.ID, URL: preset.SourceURL, Revision: preset.Revision, AdapterID: preset.ID,
		AdapterVersion: preset.AdapterVersion, Split: options.Split, SampleSize: options.SampleSize, Seed: options.Seed,
		EvidenceTypes: append([]string(nil), options.EvidenceTypes...), License: preset.License,
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			_ = prepared.Close()
		}
	}()
	queries := map[string]openRAGQuery{}
	answers := map[string]string{}
	qrels := map[string]openRAGQrel{}
	for logicalPath, target := range map[string]any{
		"pdf/arxiv/queries.json": &queries, "pdf/arxiv/answers.json": &answers, "pdf/arxiv/qrels.json": &qrels,
	} {
		if err = decodeCatalogJSON(ctx, source, logicalPath, 2<<20, target); err != nil {
			return nil, err
		}
	}
	evidence := make(map[string]struct{}, len(options.EvidenceTypes))
	for _, value := range options.EvidenceTypes {
		evidence[value] = struct{}{}
	}
	allCases := make([]Case, 0, len(queries))
	queryIDs := make([]string, 0, len(queries))
	for queryID := range queries {
		queryIDs = append(queryIDs, queryID)
	}
	sort.Strings(queryIDs)
	for _, queryID := range queryIDs {
		query := queries[queryID]
		qrel, hasQrel := qrels[queryID]
		answer, hasAnswer := answers[queryID]
		if _, enabled := evidence[query.Source]; !enabled || !hasQrel || !hasAnswer || strings.TrimSpace(query.Query) == "" || strings.TrimSpace(qrel.DocID) == "" {
			continue
		}
		documentID := openRAGDocumentID(qrel.DocID)
		allCases = append(allCases, Case{ID: "orb_" + queryID, UserInput: strings.TrimSpace(query.Query), Reference: strings.TrimSpace(answer),
			ReferenceDocumentIDs: []string{documentID}, Tags: uniqueSortedStrings([]string{query.Type, query.Source}), Metadata: map[string]any{
				"sourceQueryId": queryID, "sourceType": query.Type, "evidenceType": query.Source,
				"sourceDocId": qrel.DocID, "sourceSectionId": qrel.SectionID,
			}})
	}
	selected, err := selectCatalogCases(preset.ID+"@"+preset.Revision+"/"+options.Split+"/"+strings.Join(options.EvidenceTypes, ","), allCases, options.SampleSize, options.Seed)
	if err != nil {
		return nil, err
	}
	if options.Track == DatasetTrackPDFE2E {
		selected, err = prepareOpenRAGPDFTrack(ctx, source, prepared, selected, qrels, options)
	} else {
		err = prepareOpenRAGTextTrack(ctx, source, prepared, selected, qrels)
	}
	if err != nil {
		return nil, err
	}
	prepared.Dataset.Description = preset.Description
	prepared.Dataset.Cases = selected
	prepared.Dataset.Source.SampleSize = len(selected)
	return prepared, nil
}

func prepareOpenRAGTextTrack(ctx context.Context, source CatalogSource, prepared *PreparedCatalogDataset, cases []Case, qrels map[string]openRAGQrel) error {
	entries, err := source.List(ctx, "pdf/arxiv/corpus/")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	referenceSections := wantedOpenRAGSections(cases, qrels)
	documentSections := make(map[string]map[int]string, len(referenceSections))
	var sectionsMu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, entry := range entries {
		entry := entry
		if !strings.HasPrefix(entry.Path, "pdf/arxiv/corpus/") || path.Ext(entry.Path) != ".json" {
			continue
		}
		group.Go(func() error {
			var document openRAGCorpusDocument
			if err := decodeCatalogJSON(groupCtx, source, entry.Path, 16<<20, &document); err != nil {
				return err
			}
			if strings.TrimSpace(document.ID) == "" {
				document.ID = strings.TrimSuffix(path.Base(entry.Path), ".json")
			}
			content, sections := renderOpenRAGDocument(document)
			documentID := openRAGDocumentID(document.ID)
			if err := prepared.AddDocument(documentID, documentID+".md", "text/markdown", strings.NewReader(content), map[string]any{
				"sourceDocId": document.ID, "title": document.Title, "sourceFormat": "preprocessed-json",
			}); err != nil {
				return err
			}
			if len(referenceSections[document.ID]) > 0 {
				sectionsMu.Lock()
				documentSections[document.ID] = sections
				sectionsMu.Unlock()
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	prepared.SortDocuments()
	for documentID, wanted := range referenceSections {
		for sectionID := range wanted {
			wanted[sectionID] = strings.TrimSpace(documentSections[documentID][sectionID])
		}
	}
	applyOpenRAGReferenceContexts(cases, qrels, referenceSections)
	return nil
}

func prepareOpenRAGPDFTrack(ctx context.Context, source CatalogSource, prepared *PreparedCatalogDataset, cases []Case, qrels map[string]openRAGQrel, options CatalogImportOptions) ([]Case, error) {
	pdfURLs := map[string]string{}
	if err := decodeCatalogJSON(ctx, source, "pdf/arxiv/pdf_urls.json", 1<<20, &pdfURLs); err != nil {
		return nil, err
	}
	positiveDocs := uniqueCaseSourceDocs(cases)
	if len(positiveDocs) > options.CorpusLimit {
		selectedDocs, err := StableSampleIDs(CatalogOpenRAGBench+"/pdf-positive-docs", positiveDocs, options.CorpusLimit, options.Seed)
		if err != nil {
			return nil, err
		}
		allowed := make(map[string]struct{}, len(selectedDocs))
		for _, id := range selectedDocs {
			allowed[id] = struct{}{}
		}
		filtered := cases[:0]
		for _, item := range cases {
			if _, ok := allowed[caseSourceDocID(item)]; ok {
				filtered = append(filtered, item)
			}
		}
		cases = filtered
		positiveDocs = selectedDocs
	}
	documentIDs := append([]string(nil), positiveDocs...)
	if remaining := options.CorpusLimit - len(documentIDs); remaining > 0 {
		positive := make(map[string]struct{}, len(positiveDocs))
		for _, id := range positiveDocs {
			positive[id] = struct{}{}
		}
		negatives := make([]string, 0, len(pdfURLs))
		for id := range pdfURLs {
			if _, isPositive := positive[id]; !isPositive {
				negatives = append(negatives, id)
			}
		}
		if len(negatives) > 0 {
			selected, err := StableSampleIDs(CatalogOpenRAGBench+"/pdf-negative-docs", negatives, remaining, options.Seed)
			if err != nil {
				return nil, err
			}
			documentIDs = append(documentIDs, selected...)
		}
	}
	sort.Strings(documentIDs)
	pdfGroup, pdfCtx := errgroup.WithContext(ctx)
	pdfGroup.SetLimit(4)
	for _, sourceDocID := range documentIDs {
		sourceDocID := sourceDocID
		pdfGroup.Go(func() error {
			url := strings.TrimSpace(pdfURLs[sourceDocID])
			if url == "" {
				return fmt.Errorf("Open RAGBench PDF URL is missing for %s", sourceDocID)
			}
			reader, err := source.OpenExternal(pdfCtx, url)
			if err != nil {
				return err
			}
			documentID := openRAGDocumentID(sourceDocID)
			addErr := prepared.AddDocument(documentID, documentID+".pdf", "application/pdf", reader, map[string]any{
				"sourceDocId": sourceDocID, "sourceURL": url, "sourceFormat": "pdf",
			})
			closeErr := reader.Close()
			return errors.Join(addErr, closeErr)
		})
	}
	if err := pdfGroup.Wait(); err != nil {
		return nil, err
	}
	prepared.SortDocuments()
	referenceSections := wantedOpenRAGSections(cases, qrels)
	for sourceDocID, wanted := range referenceSections {
		var document openRAGCorpusDocument
		logicalPath := "pdf/arxiv/corpus/" + sourceDocID + ".json"
		if err := decodeCatalogJSON(ctx, source, logicalPath, 16<<20, &document); err != nil {
			return nil, err
		}
		_, sections := renderOpenRAGDocument(document)
		for sectionID := range wanted {
			wanted[sectionID] = strings.TrimSpace(sections[sectionID])
		}
	}
	applyOpenRAGReferenceContexts(cases, qrels, referenceSections)
	return cases, nil
}

func decodeCatalogJSON(ctx context.Context, source CatalogSource, logicalPath string, maxBytes int64, target any) error {
	reader, err := source.Open(ctx, logicalPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	decoder := json.NewDecoder(io.LimitReader(reader, maxBytes+1))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode catalog source %s: %w", logicalPath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("catalog source %s contains trailing JSON", logicalPath)
	}
	return nil
}

func renderOpenRAGDocument(document openRAGCorpusDocument) (string, map[int]string) {
	var output strings.Builder
	output.WriteString("# ")
	output.WriteString(strings.TrimSpace(document.Title))
	output.WriteString("\n")
	sections := make(map[int]string, len(document.Sections))
	for _, section := range document.Sections {
		text := strings.TrimSpace(section.Text)
		keys := make([]string, 0, len(section.Tables))
		for key := range section.Tables {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			table := strings.TrimSpace(section.Tables[key])
			marker := "![" + key + "](" + key + ")"
			if strings.Contains(text, marker) {
				text = strings.ReplaceAll(text, marker, table)
			} else if table != "" {
				text += "\n\n" + table
			}
		}
		sections[section.SectionID] = text
		if text != "" {
			output.WriteString("\n\n")
			output.WriteString(text)
		}
	}
	return strings.TrimSpace(output.String()) + "\n", sections
}

func wantedOpenRAGSections(cases []Case, qrels map[string]openRAGQrel) map[string]map[int]string {
	out := map[string]map[int]string{}
	for _, item := range cases {
		queryID, _ := item.Metadata["sourceQueryId"].(string)
		qrel, ok := qrels[queryID]
		if !ok {
			continue
		}
		if out[qrel.DocID] == nil {
			out[qrel.DocID] = map[int]string{}
		}
		out[qrel.DocID][qrel.SectionID] = ""
	}
	return out
}

func applyOpenRAGReferenceContexts(cases []Case, qrels map[string]openRAGQrel, sections map[string]map[int]string) {
	for index := range cases {
		queryID, _ := cases[index].Metadata["sourceQueryId"].(string)
		qrel, ok := qrels[queryID]
		if !ok {
			continue
		}
		if text := strings.TrimSpace(sections[qrel.DocID][qrel.SectionID]); text != "" {
			cases[index].ReferenceContexts = []string{text}
		}
	}
}

func openRAGDocumentID(sourceID string) string {
	sourceID = strings.TrimSpace(sourceID)
	if safeObjectID(sourceID) {
		return sourceID
	}
	return catalogDocumentID("orb", sourceID)
}

func uniqueCaseSourceDocs(cases []Case) []string {
	ids := make([]string, 0, len(cases))
	for _, item := range cases {
		if id := caseSourceDocID(item); id != "" {
			ids = append(ids, id)
		}
	}
	return uniqueSortedStrings(ids)
}

func caseSourceDocID(item Case) string {
	id, _ := item.Metadata["sourceDocId"].(string)
	return strings.TrimSpace(id)
}

func openRAGSectionKey(docID string, sectionID int) string {
	return docID + "#" + strconv.Itoa(sectionID)
}
