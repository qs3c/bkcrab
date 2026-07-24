package document

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testSourceHash     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAssetHash      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testAttachmentHash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func validParsedInput() ParsedDocumentInput {
	bbox := NormalizedBBox{100, 200, 700, 800}
	return ParsedDocumentInput{
		SchemaVersion: ParsedDocumentSchemaVersion,
		Source: ParsedSource{
			DocID: "doc_1", FileName: "guide.pdf", Format: "pdf",
			ByteSize: 123, SHA256: testSourceHash,
		},
		Parser: ParserInfo{Name: "pdf-fake", Version: "v1"},
		Units: []MarkdownUnit{{
			ID: "unit_page_0001", Location: SourceLocation{Kind: LocationPage, Index: 1, Label: "第 1 页"},
			Markdown: "正文\n\n![架构图](rag-asset://occ_1)",
		}},
		Assets: []ExtractedAsset{{
			LocalID: "asset_1", ContentSHA256: testAssetHash, Kind: AssetKindImage,
			SourceKind: SourceKindEmbeddedOriginal, SourceMIME: "image/png",
			Width: 20, Height: 10, ByteSize: 4, BundleEntry: "assets/asset_1.png",
		}},
		Occurrences: []AssetOccurrence{{
			ID: "occ_1", AssetLocalID: "asset_1", UnitID: "unit_page_0001", Order: 1,
			Location: SourceLocation{Kind: LocationPage, Index: 1, Label: "第 1 页"},
			BBox:     &bbox, AltText: "架构图", Confidence: 1,
		}},
	}
}

func TestParsedDocumentCloseIsIdempotentAndTransientIsNotJSON(t *testing.T) {
	var cleanupCalls atomic.Int32
	wantErr := errors.New("cleanup failed")
	doc := NewParsedDocument(validParsedInput(), func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("data")), nil
	}, func() error {
		cleanupCalls.Add(1)
		return wantErr
	})

	if _, err := json.Marshal(doc); err == nil {
		t.Fatal("transient ParsedDocument must not be JSON encoded as an artifact")
	}
	if err := doc.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("first Close error = %v", err)
	}
	if err := doc.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("second Close error = %v", err)
	}
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("cleanup calls = %d, want 1", got)
	}

	noop := NewParsedDocument(ParsedDocumentInput{SchemaVersion: ParsedDocumentSchemaVersion}, nil, nil)
	if err := noop.Close(); err != nil {
		t.Fatalf("no-op Close: %v", err)
	}
}

func TestParsedDocumentValidateReferencesOrderingAndPaths(t *testing.T) {
	doc := NewParsedDocument(validParsedInput(), nil, nil)
	if err := doc.Validate(); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ParsedDocument)
	}{
		{"missing asset reference", func(d *ParsedDocument) { d.Occurrences[0].AssetLocalID = "missing" }},
		{"missing unit reference", func(d *ParsedDocument) { d.Occurrences[0].UnitID = "missing" }},
		{"zero based page", func(d *ParsedDocument) { d.Units[0].Location.Index = 0 }},
		{"location mismatch", func(d *ParsedDocument) { d.Occurrences[0].Location.Index = 2 }},
		{"path traversal", func(d *ParsedDocument) { d.Assets[0].BundleEntry = "../secret" }},
		{"absolute path", func(d *ParsedDocument) { d.Assets[0].BundleEntry = `C:\\temp\\asset.png` }},
		{"duplicate content asset", func(d *ParsedDocument) {
			dup := d.Assets[0]
			dup.LocalID = "asset_2"
			d.Assets = append(d.Assets, dup)
		}},
		{"duplicate bundle entry", func(d *ParsedDocument) {
			dup := d.Assets[0]
			dup.LocalID = "asset_2"
			dup.ContentSHA256 = strings.Repeat("c", 64)
			d.Assets = append(d.Assets, dup)
		}},
		{"unknown marker", func(d *ParsedDocument) { d.Units[0].Markdown += "\n![](rag-asset://unknown)" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := NewParsedDocument(validParsedInput(), nil, nil)
			tt.edit(candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}

	input := validParsedInput()
	input.Units = append(input.Units, MarkdownUnit{
		ID: "unit_page_0002", Location: SourceLocation{Kind: LocationPage, Index: 2, Label: "第 2 页"}, Markdown: "第二页",
	})
	ordered := NewParsedDocument(input, nil, nil)
	if err := ordered.Validate(); err != nil {
		t.Fatalf("ordered pages rejected: %v", err)
	}
	ordered.Units[0], ordered.Units[1] = ordered.Units[1], ordered.Units[0]
	if err := ordered.Validate(); err == nil {
		t.Fatal("out-of-order source units must be rejected")
	}

	codeInput := validParsedInput()
	codeInput.Units[0].Markdown += "\n\n```markdown\n![](rag-asset://not_an_occurrence)\n```"
	if err := NewParsedDocument(codeInput, nil, nil).Validate(); err != nil {
		t.Fatalf("internal-scheme-like text inside code must stay literal: %v", err)
	}
}

func TestAssetIDAndInternalMarkerTrustBoundary(t *testing.T) {
	id1, err := AssetID("doc_1", testAssetHash)
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := AssetID("doc_1", testAssetHash)
	idOther, _ := AssetID("doc_2", testAssetHash)
	if id1 != id2 || id1 == idOther || !strings.HasPrefix(id1, "ast_") || len(id1) != 36 {
		t.Fatalf("unexpected IDs: %q %q %q", id1, id2, idOther)
	}
	if _, err := AssetID("doc_1", strings.ToUpper(testAssetHash)); err == nil {
		t.Fatal("non-canonical hash must be rejected")
	}
	attachmentID, err := AttachmentID("doc_1", testAttachmentHash)
	if err != nil || !strings.HasPrefix(attachmentID, "att_") || len(attachmentID) != 36 {
		t.Fatalf("unexpected attachment ID %q: %v", attachmentID, err)
	}
	attachmentIDAgain, _ := AttachmentID("doc_1", testAttachmentHash)
	attachmentIDOther, _ := AttachmentID("doc_2", testAttachmentHash)
	if attachmentID != attachmentIDAgain || attachmentID == attachmentIDOther {
		t.Fatalf("attachment IDs are not stable/document-scoped: %q %q %q", attachmentID, attachmentIDAgain, attachmentIDOther)
	}

	url, err := InternalAssetURL("occ_1")
	if err != nil || url != "rag-asset://occ_1" {
		t.Fatalf("InternalAssetURL = %q, %v", url, err)
	}
	allowed := map[string]struct{}{"occ_1": {}}
	if got, ok := ParseInternalAssetURL(url, true, allowed); !ok || got != "occ_1" {
		t.Fatalf("trusted marker = %q, %v", got, ok)
	}
	if _, ok := ParseInternalAssetURL(url, false, allowed); ok {
		t.Fatal("user-authored internal asset marker must not be trusted")
	}
	if _, ok := ParseInternalAssetURL("rag-asset://other", true, allowed); ok {
		t.Fatal("marker outside the current occurrence map must be rejected")
	}
}

func TestParsedDocumentValidatesOccurrenceBoundVisioAttachment(t *testing.T) {
	input := validParsedInput()
	input.Attachments = []ExtractedAttachment{{
		LocalID: "attachment_1", ContentSHA256: testAttachmentHash,
		Kind: AttachmentKindVisioSource, FileName: "architecture.vsdx",
		MIMEType: MIMETypeVSDX, ByteSize: 128, BundleEntry: "attachments/attachment_1.vsdx",
	}}
	input.Occurrences[0].AttachmentLocalID = "attachment_1"
	if err := NewParsedDocument(input, nil, nil).Validate(); err != nil {
		t.Fatalf("valid attachment rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ParsedDocument)
	}{
		{"dangling occurrence attachment", func(d *ParsedDocument) {
			d.Occurrences[0].AttachmentLocalID = "attachment_missing"
		}},
		{"unreferenced attachment", func(d *ParsedDocument) {
			d.Occurrences[0].AttachmentLocalID = ""
		}},
		{"unsafe attachment file name", func(d *ParsedDocument) {
			d.Attachments[0].FileName = "../architecture.vsdx"
		}},
		{"wrong attachment MIME", func(d *ParsedDocument) {
			d.Attachments[0].MIMEType = "application/zip"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := NewParsedDocument(input, nil, nil)
			candidate.Attachments = append([]ExtractedAttachment(nil), input.Attachments...)
			candidate.Occurrences = append([]AssetOccurrence(nil), input.Occurrences...)
			test.edit(candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("Validate unexpectedly succeeded")
			}
		})
	}
}
