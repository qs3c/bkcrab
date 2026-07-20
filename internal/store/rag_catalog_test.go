package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRAGDocumentVersion(docID string, version int64) *RAGDocumentVersionRecord {
	return &RAGDocumentVersionRecord{
		DocID:                        docID,
		DocVersion:                   version,
		Status:                       RAGDocumentVersionPending,
		SourceSHA256:                 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ParseMode:                    RAGParseModeStandard,
		ChunkSize:                    512,
		ChunkOverlap:                 64,
		ParserVersion:                "parser-v1",
		SplitterVersion:              "splitter-v1",
		ParseFingerprint:             "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		IndexFingerprint:             "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		VisionModel:                  "",
		VisionProviderFingerprint:    "",
		VisionPromptVersion:          "",
		TextModel:                    "",
		TextProviderFingerprint:      "",
		EnrichmentPromptVersion:      "",
		EnrichmentEnabled:            false,
		MaxDocumentAIRequests:        300,
		MaxDocumentAITokens:          200000,
		MaxDocumentAICostMicroUSD:    1_000_000,
		EmbeddingProvider:            "system",
		EmbeddingModel:               "embed-v1",
		EmbeddingDimensions:          8,
		EmbeddingContractFingerprint: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}
}

func TestRAGDocumentVersionAndCatalogCRUD(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.CreateRAGKB(ctx, &RAGKBRecord{
		ID: "kb_catalog", UserID: "u_catalog", Name: "catalog",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 8,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard,
		Status: "active", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create KB: %v", err)
	}

	doc := &RAGDocumentRecord{
		ID: "doc_catalog", KBID: "kb_catalog", FileName: "catalog.md", FileType: "md",
		ObjectKey: "rag/u/kb/doc/catalog.md", Status: "PENDING", Version: 5,
		ActiveVersion: 0, IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: now,
	}
	version := testRAGDocumentVersion(doc.ID, doc.Version)
	// Callers cannot smuggle mutable result state into an immutable snapshot.
	version.Status = RAGDocumentVersionDone
	version.ParseArtifactKey = "caller/result.json"
	version.PageCount = 99
	version.AssetCount = 99
	version.Degraded = true
	version.WarningCount = 99
	mismatched := *version
	mismatched.DocVersion++
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, &mismatched, 3); !errors.Is(err, ErrRAGDocumentVersionMismatch) {
		t.Fatalf("mismatched document/version err=%v", err)
	}
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, 3); err != nil {
		t.Fatalf("atomic create document/version/task: %v", err)
	}
	gotDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || gotDoc.Version != 5 || gotDoc.ActiveVersion != 0 || gotDoc.IndexFormatVersion != 1 {
		t.Fatalf("document = %+v err=%v", gotDoc, err)
	}
	gotVersion, err := st.GetRAGDocumentVersion(ctx, doc.ID, 5)
	if err != nil || gotVersion.ParserVersion != "parser-v1" || gotVersion.EmbeddingDimensions != 8 ||
		gotVersion.Status != RAGDocumentVersionPending || gotVersion.ParseArtifactKey != "" ||
		gotVersion.PageCount != 0 || gotVersion.AssetCount != 0 || gotVersion.Degraded ||
		gotVersion.WarningCount != 0 {
		t.Fatalf("version = %+v err=%v", gotVersion, err)
	}

	// Result-state writes are fence-only in production. Seed a terminal catalog
	// fixture directly so this CRUD test cannot reintroduce an unfenced API.
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_document_versions SET
		status='DONE',parse_artifact_key='artifact/parsed.json',page_count=2,
		asset_count=1,degraded=TRUE,warning_count=2 WHERE doc_id=? AND doc_version=?`,
		doc.ID, int64(5)); err != nil {
		t.Fatalf("seed version result: %v", err)
	}
	gotVersion, err = st.GetRAGDocumentVersion(ctx, doc.ID, 5)
	if err != nil || gotVersion.Status != RAGDocumentVersionDone || gotVersion.PageCount != 2 ||
		gotVersion.ParseFingerprint != version.ParseFingerprint {
		t.Fatalf("updated version = %+v err=%v", gotVersion, err)
	}

	chunks := []RAGChunkRecord{
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: 5, ChunkIndex: 0, SectionTitle: "A", LocationJSON: `{"kind":"document"}`, RawContent: "raw zero", Enhancement: "", SearchContent: "A raw zero", TokenCount: 3, CreatedAt: now},
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: 5, ChunkIndex: 1, SectionTitle: "B", LocationJSON: `{"kind":"page","index":1}`, RawContent: "raw one", Enhancement: "summary", SearchContent: "B raw one summary", TokenCount: 5, CreatedAt: now},
	}
	if err := st.PutRAGChunks(ctx, chunks); err != nil {
		t.Fatalf("put chunks: %v", err)
	}
	refs := []RAGChunkRef{{DocID: doc.ID, DocVersion: 5, ChunkIndex: 1}, {DocID: doc.ID, DocVersion: 5, ChunkIndex: 0}}
	gotChunks, err := st.ListRAGChunksByRefs(ctx, refs)
	if err != nil || len(gotChunks) != 2 || gotChunks[0].ChunkIndex != 0 || gotChunks[1].ChunkIndex != 1 {
		t.Fatalf("chunks = %+v err=%v", gotChunks, err)
	}

	asset := &RAGAssetRecord{
		ID: "ast_catalog", DocID: doc.ID,
		ContentSHA256:      "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		SourceKind:         "embedded_original",
		SourceMIME:         "image/png",
		DisplayMIME:        "image/webp",
		SourceObjectKey:    "rag/u/kb/doc/assets/source.png",
		DisplayObjectKey:   "rag/u/kb/doc/assets/display.webp",
		ThumbnailObjectKey: "rag/u/kb/doc/assets/thumbnail.webp",
		DisplayStatus:      "ready",
		DisplaySHA256:      "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		ByteSize:           100,
		Width:              20,
		Height:             10,
		FirstSeenVersion:   5,
		LastSeenVersion:    5,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatalf("upsert asset: %v", err)
	}
	seenAgain := *asset
	seenAgain.FirstSeenVersion = 3
	seenAgain.LastSeenVersion = 8
	if err := st.UpsertRAGAsset(ctx, &seenAgain); err != nil {
		t.Fatalf("upsert seen asset: %v", err)
	}
	gotAsset, err := st.GetRAGAsset(ctx, asset.ID)
	if err != nil || gotAsset.FirstSeenVersion != 3 || gotAsset.LastSeenVersion != 8 {
		t.Fatalf("asset = %+v err=%v", gotAsset, err)
	}
	conflict := seenAgain
	conflict.SourceObjectKey = "mutated"
	if err := st.UpsertRAGAsset(ctx, &conflict); !errors.Is(err, ErrRAGAssetConflict) {
		t.Fatalf("immutable asset conflict err=%v", err)
	}

	mappings := []RAGChunkAssetRecord{{
		DocID: doc.ID, DocVersion: 5, ChunkIndex: 1, AssetID: asset.ID, Ordinal: 0,
		LocationJSON: `{"kind":"page","index":1}`, Caption: "diagram", OCRText: "hello",
	}}
	if err := st.PutRAGChunkAssets(ctx, mappings); err != nil {
		t.Fatalf("put chunk assets: %v", err)
	}
	gotMappings, err := st.ListRAGChunkAssetsByRefs(ctx, refs)
	if err != nil || len(gotMappings) != 1 || gotMappings[0].AssetID != asset.ID {
		t.Fatalf("chunk assets = %+v err=%v", gotMappings, err)
	}
	assets, err := st.ListRAGAssetsByIDs(ctx, []string{asset.ID})
	if err != nil || len(assets) != 1 || assets[0].DisplayStatus != "ready" {
		t.Fatalf("assets by ids = %+v err=%v", assets, err)
	}
	assets, err = st.ListRAGAssetsByChunkRefs(ctx, refs)
	if err != nil || len(assets) != 1 || assets[0].ID != asset.ID {
		t.Fatalf("assets by refs = %+v err=%v", assets, err)
	}

	if err := st.DeleteRAGChunkAssetsByDocumentVersion(ctx, doc.ID, 5); err != nil {
		t.Fatalf("delete mappings: %v", err)
	}
	if err := st.DeleteRAGChunksByDocumentVersion(ctx, doc.ID, 5); err != nil {
		t.Fatalf("delete chunks: %v", err)
	}
	gotChunks, err = st.ListRAGChunksByRefs(ctx, refs)
	if err != nil || len(gotChunks) != 0 {
		t.Fatalf("chunks survived exact-version delete: %+v err=%v", gotChunks, err)
	}
	if _, err := st.GetRAGAsset(ctx, asset.ID); err != nil {
		t.Fatalf("exact-version catalog delete removed content asset: %v", err)
	}
}

func TestRAGIndexGCAndDocumentAIBasicCRUD(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	gc := &RAGIndexGCTaskRecord{
		DocID: "doc_gc", RetiredVersion: 4, RetiredAt: now,
		NotBefore: now.Add(time.Minute), Status: "PENDING", CreatedAt: now,
	}
	id, err := st.CreateRAGIndexGCTask(ctx, gc)
	if err != nil || id == 0 {
		t.Fatalf("create GC task id=%d err=%v", id, err)
	}
	duplicateID, err := st.CreateRAGIndexGCTask(ctx, gc)
	if err != nil || duplicateID != id {
		t.Fatalf("idempotent GC task id=%d want=%d err=%v", duplicateID, id, err)
	}
	gotGC, err := st.GetRAGIndexGCTask(ctx, id)
	if err != nil || gotGC.RetiredVersion != 4 || gotGC.Status != "PENDING" {
		t.Fatalf("GC task = %+v err=%v", gotGC, err)
	}

	taskBudget := &RAGDocumentAITaskBudgetRecord{
		TaskID: 77, UserID: "u_budget", MaxRequests: 10, MaxTokens: 200,
		MaxCostMicroUSD: 3000, ChargedRequests: 1, ChargedTokens: 2,
		ChargedCostMicroUSD: 3, UpdatedAt: now,
	}
	if err := st.CreateRAGDocumentAITaskBudget(ctx, taskBudget); err != nil {
		t.Fatalf("create task budget: %v", err)
	}
	gotTaskBudget, err := st.GetRAGDocumentAITaskBudget(ctx, 77)
	if err != nil || gotTaskBudget.MaxTokens != 200 || gotTaskBudget.ChargedRequests != 1 {
		t.Fatalf("task budget = %+v err=%v", gotTaskBudget, err)
	}

	period := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	userBudget := &RAGDocumentAIUserBudgetRecord{UserID: "u_budget", PeriodStartUTC: period, UpdatedAt: now}
	if err := st.CreateRAGDocumentAIUserBudget(ctx, userBudget); err != nil {
		t.Fatalf("create user budget: %v", err)
	}
	if _, err := st.GetRAGDocumentAIUserBudget(ctx, "u_budget", period); err != nil {
		t.Fatalf("get user budget: %v", err)
	}

}
