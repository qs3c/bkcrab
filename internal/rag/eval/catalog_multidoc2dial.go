package eval

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const multiDoc2DialArchiveSHA256 = "f0c034c249663d7b3cb08b19cf2cc2c3d101372485be982621d4711931a1ce00"

type MultiDoc2DialAdapter struct{}

func (MultiDoc2DialAdapter) ID() string { return CatalogMultiDoc2Dial }

type multiDoc2DialSpan struct {
	ID   string `json:"id_sp"`
	Text string `json:"text_sp"`
}

type multiDoc2DialDocument struct {
	Title  string                       `json:"title"`
	DocID  string                       `json:"doc_id"`
	Domain string                       `json:"domain"`
	Text   string                       `json:"doc_text"`
	Spans  map[string]multiDoc2DialSpan `json:"spans"`
}

type multiDoc2DialReference struct {
	SpanID string `json:"id_sp"`
	Label  string `json:"label"`
	DocID  string `json:"doc_id"`
}

type multiDoc2DialTurn struct {
	DA         string                   `json:"da"`
	References []multiDoc2DialReference `json:"references"`
	Role       string                   `json:"role"`
	TurnID     int                      `json:"turn_id"`
	Utterance  string                   `json:"utterance"`
}

type multiDoc2DialDialogue struct {
	ID    string              `json:"dial_id"`
	Turns []multiDoc2DialTurn `json:"turns"`
}

func (MultiDoc2DialAdapter) Prepare(ctx context.Context, source CatalogSource, options CatalogImportOptions) (_ *PreparedCatalogDataset, retErr error) {
	if source == nil {
		return nil, errors.New("catalog source is required")
	}
	if err := options.ApplyDefaults(); err != nil {
		return nil, err
	}
	if options.CatalogID != CatalogMultiDoc2Dial || options.Track != DatasetTrackTextRAG {
		return nil, errors.New("MultiDoc2Dial adapter options are invalid")
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
	archivePath := prepared.root + string(os.PathSeparator) + "source.zip"
	if err = copyCatalogSourceToFile(ctx, source, "multidoc2dial.zip", archivePath, multiDoc2DialArchiveSHA256, 16<<20); err != nil {
		return nil, err
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open MultiDoc2Dial archive: %w", err)
	}
	defer archive.Close()
	var documentRoot struct {
		Documents map[string]map[string]multiDoc2DialDocument `json:"doc_data"`
	}
	if err = decodeZipJSON(archive.File, "multidoc2dial/multidoc2dial_doc.json", 64<<20, &documentRoot); err != nil {
		return nil, err
	}
	dialogueFile := map[string]string{
		"validation": "multidoc2dial/multidoc2dial_dial_validation.json",
		"train":      "multidoc2dial/multidoc2dial_dial_train.json",
		"test":       "multidoc2dial/multidoc2dial_dial_test.json",
	}[options.Split]
	var dialogueRoot struct {
		Dialogues map[string][]multiDoc2DialDialogue `json:"dial_data"`
	}
	if err = decodeZipJSON(archive.File, dialogueFile, 48<<20, &dialogueRoot); err != nil {
		return nil, err
	}
	documentsBySource := map[string]multiDoc2DialDocument{}
	documentIDs := map[string]string{}
	domains := make([]string, 0, len(documentRoot.Documents))
	for domain := range documentRoot.Documents {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	for _, domain := range domains {
		sourceIDs := make([]string, 0, len(documentRoot.Documents[domain]))
		for sourceID := range documentRoot.Documents[domain] {
			sourceIDs = append(sourceIDs, sourceID)
		}
		sort.Strings(sourceIDs)
		for _, sourceID := range sourceIDs {
			document := documentRoot.Documents[domain][sourceID]
			id := catalogDocumentID("mdd", sourceID)
			fileName := id + ".md"
			content := "# " + strings.TrimSpace(document.Title) + "\n\n" + strings.TrimSpace(document.Text) + "\n"
			if err = prepared.AddDocument(id, fileName, "text/markdown", strings.NewReader(content), map[string]any{
				"sourceDocId": sourceID, "domain": domain, "title": document.Title,
			}); err != nil {
				return nil, err
			}
			documentIDs[sourceID] = id
			documentsBySource[sourceID] = document
		}
	}
	cases := make([]Case, 0, 5_000)
	for _, domain := range domains {
		dialogues := dialogueRoot.Dialogues[domain]
		sort.Slice(dialogues, func(i, j int) bool { return dialogues[i].ID < dialogues[j].ID })
		for _, dialogue := range dialogues {
			userHistory := []string{}
			var current *multiDoc2DialTurn
			var history []string
			for turnIndex := range dialogue.Turns {
				turn := dialogue.Turns[turnIndex]
				switch strings.ToLower(strings.TrimSpace(turn.Role)) {
				case "user":
					turnCopy := turn
					current = &turnCopy
					history = append([]string(nil), userHistory...)
					userHistory = append(userHistory, strings.TrimSpace(turn.Utterance))
				case "agent":
					if current == nil || strings.TrimSpace(current.Utterance) == "" || strings.TrimSpace(turn.Utterance) == "" {
						continue
					}
					referenceDocumentIDs, referenceContexts := []string{}, []string{}
					sourceRefs := make([]map[string]string, 0, len(turn.References))
					seenDocs, seenContexts := map[string]struct{}{}, map[string]struct{}{}
					for _, reference := range turn.References {
						if id := documentIDs[reference.DocID]; id != "" {
							if _, duplicate := seenDocs[id]; !duplicate {
								seenDocs[id] = struct{}{}
								referenceDocumentIDs = append(referenceDocumentIDs, id)
							}
						}
						if document, ok := documentsBySource[reference.DocID]; ok {
							if span, exists := document.Spans[reference.SpanID]; exists {
								text := strings.TrimSpace(span.Text)
								if text != "" {
									if _, duplicate := seenContexts[text]; !duplicate {
										seenContexts[text] = struct{}{}
										referenceContexts = append(referenceContexts, text)
									}
								}
							}
						}
						sourceRefs = append(sourceRefs, map[string]string{"docId": reference.DocID, "spanId": reference.SpanID, "label": reference.Label})
					}
					sort.Strings(referenceDocumentIDs)
					caseID := "mdd_" + dialogue.ID + "_" + strconv.Itoa(turn.TurnID)
					cases = append(cases, Case{ID: caseID, UserInput: strings.TrimSpace(current.Utterance), Reference: strings.TrimSpace(turn.Utterance),
						ReferenceContexts: referenceContexts, ReferenceDocumentIDs: referenceDocumentIDs, History: append([]string(nil), history...),
						Tags: []string{domain, "multi_turn"}, Metadata: map[string]any{"domain": domain, "dialogueId": dialogue.ID,
							"userTurnId": current.TurnID, "agentTurnId": turn.TurnID, "dialogAct": turn.DA, "sourceReferences": sourceRefs}})
					current = nil
				}
			}
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

func copyCatalogSourceToFile(ctx context.Context, source CatalogSource, logicalPath, target, expectedSHA string, maxBytes int64) error {
	reader, err := source.Open(ctx, logicalPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(reader, maxBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(target)
		return errors.Join(copyErr, closeErr)
	}
	if written > maxBytes {
		_ = os.Remove(target)
		return errors.New("catalog source exceeds byte limit")
	}
	if expectedSHA != "" && hex.EncodeToString(hasher.Sum(nil)) != expectedSHA {
		_ = os.Remove(target)
		return errors.New("catalog source checksum mismatch")
	}
	return nil
}

func decodeZipJSON(files []*zip.File, name string, maxBytes int64, target any) error {
	for _, file := range files {
		if file.Name != name {
			continue
		}
		if int64(file.UncompressedSize64) > maxBytes {
			return fmt.Errorf("archive member %s exceeds byte limit", name)
		}
		reader, err := file.Open()
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(io.LimitReader(reader, maxBytes+1))
		err = decoder.Decode(target)
		closeErr := reader.Close()
		if err == nil {
			var trailing any
			if trailingErr := decoder.Decode(&trailing); !errors.Is(trailingErr, io.EOF) {
				err = errors.New("archive JSON contains trailing data")
			}
		}
		return errors.Join(err, closeErr)
	}
	return fmt.Errorf("archive member %s is missing", name)
}

func selectCatalogCases(namespace string, cases []Case, sampleSize int, seed int64) ([]Case, error) {
	byID := make(map[string]Case, len(cases))
	ids := make([]string, 0, len(cases))
	for _, item := range cases {
		if _, duplicate := byID[item.ID]; duplicate {
			return nil, fmt.Errorf("duplicate catalog case %q", item.ID)
		}
		byID[item.ID] = item
		ids = append(ids, item.ID)
	}
	selectedIDs, err := StableSampleIDs(namespace, ids, sampleSize, seed)
	if err != nil {
		return nil, err
	}
	out := make([]Case, len(selectedIDs))
	for index, id := range selectedIDs {
		out[index] = byID[id]
	}
	return out, nil
}

func markdownBytes(title, body string) *bytes.Reader {
	return bytes.NewReader([]byte("# " + strings.TrimSpace(title) + "\n\n" + strings.TrimSpace(body) + "\n"))
}
