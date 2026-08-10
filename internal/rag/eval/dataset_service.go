package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	CanonicalJSONSourceType = "canonical-json"
	defaultManifestBytes    = int64(16 << 20)
	maxManifestBytes        = int64(64 << 20)
	defaultDocumentBytes    = int64(50 << 20)
	maxDocumentBytes        = int64(512 << 20)
)

var ErrDatasetValidation = errors.New("evaluation dataset validation failed")

type DatasetStore interface {
	CreateRAGEvalDatasetVersion(context.Context, *store.RAGEvalDatasetVersionRecord) error
	TransitionRAGEvalDatasetVersion(context.Context, string, string, string, string) (bool, error)
	PutRAGEvalCorpusDocument(context.Context, *store.RAGEvalCorpusDocumentRecord) error
	PutRAGEvalCase(context.Context, *store.RAGEvalCaseRecord) error
	ListRAGEvalCorpusDocuments(context.Context, string, string, int) ([]store.RAGEvalCorpusDocumentRecord, error)
	ListRAGEvalCases(context.Context, string, string, int) ([]store.RAGEvalCaseRecord, error)
}

type DatasetLifecycleStore interface {
	DatasetStore
	TombstoneRAGEvalDataset(context.Context, string) (bool, error)
	ListRAGEvalDatasetStagingCandidates(context.Context, time.Time, int) ([]store.RAGEvalDatasetVersionRecord, error)
	ListRAGEvalDatasetGCCandidates(context.Context, time.Time, int) ([]string, error)
	PurgeRAGEvalDataset(context.Context, string) (bool, error)
}

type DatasetDocumentUpload struct {
	ExternalID string
	FileName   string
	MediaType  string
	SizeBytes  int64
	Reader     io.Reader
}

type DatasetImportRequest struct {
	DatasetID   string
	Version     int64
	CreatedBy   string
	Manifest    io.Reader
	Documents   []DatasetDocumentUpload
	MaxManifest int64
	MaxDocument int64
}

type DatasetImportResult struct {
	Version *store.RAGEvalDatasetVersionRecord
	Report  ValidationReport
}

type DatasetPreviewPage struct {
	Documents          []store.RAGEvalCorpusDocumentRecord `json:"documents"`
	Cases              []store.RAGEvalCaseRecord           `json:"cases"`
	NextDocumentCursor string                              `json:"nextDocumentCursor,omitempty"`
	NextCaseCursor     string                              `json:"nextCaseCursor,omitempty"`
}

type DatasetService struct {
	store   DatasetStore
	objects objects.Store
}

func NewDatasetService(datasetStore DatasetStore, objectStore objects.Store) (*DatasetService, error) {
	if datasetStore == nil || objectStore == nil {
		return nil, errors.New("dataset store and object store are required")
	}
	return &DatasetService{store: datasetStore, objects: objectStore}, nil
}

func decodeCanonicalManifest(reader io.Reader, limit int64) (CanonicalDataset, []byte, error) {
	if reader == nil {
		return CanonicalDataset{}, nil, errors.New("canonical manifest is required")
	}
	if limit <= 0 {
		limit = defaultManifestBytes
	}
	if limit > maxManifestBytes {
		return CanonicalDataset{}, nil, errors.New("canonical manifest limit exceeds service maximum")
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return CanonicalDataset{}, nil, err
	}
	if int64(len(raw)) > limit {
		return CanonicalDataset{}, nil, errors.New("canonical manifest exceeds byte limit")
	}
	var dataset CanonicalDataset
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&dataset); err != nil {
		return CanonicalDataset{}, nil, fmt.Errorf("decode canonical manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return CanonicalDataset{}, nil, errors.New("canonical manifest contains trailing JSON")
	}
	return dataset, raw, nil
}

func safeDatasetFileName(name string) bool {
	name = strings.TrimSpace(name)
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && path.Base(name) == name
}

var objectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

func safeObjectID(value string) bool {
	return objectIDPattern.MatchString(value) && value != "." && value != ".."
}

func marshalStringArray(values []string) string {
	if values == nil {
		values = []string{}
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func marshalMetadata(value map[string]any) string {
	if value == nil {
		return "{}"
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

type hashingExactReader struct {
	reader    io.Reader
	remaining int64
	hash      io.Writer
	written   int64
}

func (r *hashingExactReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	n, err := r.reader.Read(buffer)
	if n > 0 {
		_, _ = r.hash.Write(buffer[:n])
		r.remaining -= int64(n)
		r.written += int64(n)
	}
	return n, err
}

func streamDocument(ctx context.Context, objectStore objects.Store, key string, upload DatasetDocumentUpload, maxBytes int64) (string, error) {
	if upload.Reader == nil || upload.SizeBytes < 0 || upload.SizeBytes > maxBytes {
		return "", errors.New("document upload size is invalid")
	}
	hasher := sha256.New()
	stream := &hashingExactReader{reader: upload.Reader, remaining: upload.SizeBytes, hash: hasher}
	if err := objectStore.Put(ctx, key, stream, upload.SizeBytes, upload.MediaType); err != nil {
		return "", err
	}
	if stream.written != upload.SizeBytes {
		return "", errors.New("document upload ended before declared size")
	}
	var extra [1]byte
	if n, err := upload.Reader.Read(extra[:]); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "", errors.New("document upload exceeds declared size")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (s *DatasetService) ImportCanonical(ctx context.Context, request DatasetImportRequest) (result DatasetImportResult, err error) {
	if !safeObjectID(strings.TrimSpace(request.DatasetID)) || request.Version <= 0 || strings.TrimSpace(request.CreatedBy) == "" {
		return result, errors.New("dataset import identity is required")
	}
	dataset, rawManifest, err := decodeCanonicalManifest(request.Manifest, request.MaxManifest)
	if err != nil {
		return result, err
	}
	if request.MaxDocument <= 0 {
		request.MaxDocument = defaultDocumentBytes
	}
	if request.MaxDocument > maxDocumentBytes {
		return result, errors.New("document limit exceeds service maximum")
	}
	uploads := make(map[string]DatasetDocumentUpload, len(request.Documents))
	fileNames := make(map[string]struct{}, len(request.Documents))
	for _, upload := range request.Documents {
		upload.ExternalID = strings.TrimSpace(upload.ExternalID)
		if !safeObjectID(upload.ExternalID) || !safeDatasetFileName(upload.FileName) {
			return result, errors.New("document id and flat file name are required")
		}
		if _, duplicate := uploads[upload.ExternalID]; duplicate {
			return result, fmt.Errorf("duplicate document id %q", upload.ExternalID)
		}
		folded := strings.ToLower(upload.FileName)
		if _, duplicate := fileNames[folded]; duplicate {
			return result, fmt.Errorf("duplicate file name %q", upload.FileName)
		}
		uploads[upload.ExternalID], fileNames[folded] = upload, struct{}{}
	}
	versionID := "rdv_" + uuid.NewString()
	manifestKey := path.Join("rag-eval", "datasets", request.DatasetID, "versions", fmt.Sprint(request.Version), "manifest.json")
	rawDigest := sha256.Sum256(rawManifest)
	fingerprint := hex.EncodeToString(rawDigest[:])
	totalBytes := int64(0)
	for _, document := range dataset.Corpus {
		if document.SizeBytes > 0 && totalBytes > (1<<62)-document.SizeBytes {
			return result, errors.New("dataset total bytes overflow")
		}
		totalBytes += document.SizeBytes
	}
	report := ValidateDataset(dataset, DefaultValidationLimits())
	if report.Valid {
		fingerprint, err = DatasetFingerprint(dataset)
		if err != nil {
			return result, err
		}
	}
	reportJSON, _ := json.Marshal(report)
	version := &store.RAGEvalDatasetVersionRecord{
		ID: versionID, DatasetID: request.DatasetID, Version: request.Version,
		Status: store.RAGEvalDatasetDraft, SourceType: CanonicalJSONSourceType,
		ManifestObjectKey: manifestKey, CorpusSHA256: fingerprint,
		CaseCount: int64(len(dataset.Cases)), DocumentCount: int64(len(dataset.Corpus)), TotalBytes: totalBytes,
		ValidationReportJSON: string(reportJSON), CreatedBy: request.CreatedBy,
	}
	if err = s.store.CreateRAGEvalDatasetVersion(ctx, version); err != nil {
		return result, err
	}
	result = DatasetImportResult{Version: version, Report: report}
	markFailed := func(from string) {
		changed, _ := s.store.TransitionRAGEvalDatasetVersion(context.Background(), versionID, from, store.RAGEvalDatasetFailed, string(reportJSON))
		if changed {
			version.Status = store.RAGEvalDatasetFailed
		}
	}
	failVersion := func(cause error) (DatasetImportResult, error) {
		markFailed(store.RAGEvalDatasetDraft)
		return result, cause
	}
	if !report.Valid {
		return failVersion(ErrDatasetValidation)
	}
	if len(uploads) != len(dataset.Corpus) {
		return failVersion(errors.New("uploaded documents do not match manifest corpus"))
	}
	stagedKeys, finalKeys := []string{}, []string{}
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		for _, key := range append(stagedKeys, finalKeys...) {
			_ = s.objects.Delete(context.Background(), key)
		}
	}()
	for index := range dataset.Corpus {
		document := &dataset.Corpus[index]
		upload, ok := uploads[document.ID]
		if !ok || upload.FileName != document.FileName || upload.SizeBytes != document.SizeBytes {
			return failVersion(fmt.Errorf("upload for document %q does not match manifest", document.ID))
		}
		stagingKey := path.Join("rag-eval", "staging", versionID, document.ID)
		stagedKeys = append(stagedKeys, stagingKey)
		digest, uploadErr := streamDocument(ctx, s.objects, stagingKey, upload, request.MaxDocument)
		if uploadErr != nil {
			return failVersion(uploadErr)
		}
		if digest != document.SHA256 {
			return failVersion(fmt.Errorf("sha256 mismatch for document %q", document.ID))
		}
		finalKey := path.Join("rag-eval", "datasets", request.DatasetID, "versions", fmt.Sprint(request.Version), "corpus", document.ID, document.FileName)
		finalKeys = append(finalKeys, finalKey)
		document.ObjectKey = finalKey
		if err = s.store.PutRAGEvalCorpusDocument(ctx, &store.RAGEvalCorpusDocumentRecord{
			DatasetVersionID: versionID, ExternalID: document.ID, FileName: document.FileName,
			MediaType: document.MediaType, SizeBytes: document.SizeBytes, SHA256: document.SHA256,
			ObjectKey: finalKey, MetadataJSON: marshalMetadata(document.Metadata),
		}); err != nil {
			return failVersion(err)
		}
	}
	for _, item := range dataset.Cases {
		if err = s.store.PutRAGEvalCase(ctx, &store.RAGEvalCaseRecord{
			DatasetVersionID: versionID, ExternalID: item.ID, UserInput: item.UserInput,
			ReferenceAnswer: item.Reference, ReferenceContextsJSON: marshalStringArray(item.ReferenceContexts),
			ReferenceContextIDsJSON: marshalStringArray(item.ReferenceContextIDs), HistoryJSON: marshalStringArray(item.History),
			ExpectedAbstention: item.ExpectedAbstention, TagsJSON: marshalStringArray(item.Tags), MetadataJSON: marshalMetadata(item.Metadata),
		}); err != nil {
			return failVersion(err)
		}
	}
	changed, err := s.store.TransitionRAGEvalDatasetVersion(ctx, versionID, store.RAGEvalDatasetDraft, store.RAGEvalDatasetValidating, string(reportJSON))
	if err != nil || !changed {
		return result, fmt.Errorf("claim dataset validation: changed=%v: %w", changed, err)
	}
	for index, stagingKey := range stagedKeys {
		reader, getErr := s.objects.Get(ctx, stagingKey)
		if getErr != nil {
			markFailed(store.RAGEvalDatasetValidating)
			return result, getErr
		}
		putErr := s.objects.Put(ctx, finalKeys[index], reader, dataset.Corpus[index].SizeBytes, dataset.Corpus[index].MediaType)
		closeErr := reader.Close()
		if putErr != nil || closeErr != nil {
			markFailed(store.RAGEvalDatasetValidating)
			return result, errors.Join(putErr, closeErr)
		}
	}
	canonicalManifest, err := json.Marshal(dataset)
	if err != nil {
		return result, err
	}
	finalKeys = append(finalKeys, manifestKey)
	if err = s.objects.Put(ctx, manifestKey, bytes.NewReader(canonicalManifest), int64(len(canonicalManifest)), "application/json"); err != nil {
		markFailed(store.RAGEvalDatasetValidating)
		return result, err
	}
	changed, err = s.store.TransitionRAGEvalDatasetVersion(ctx, versionID, store.RAGEvalDatasetValidating, store.RAGEvalDatasetReady, string(reportJSON))
	if err != nil || !changed {
		return result, fmt.Errorf("publish dataset version: changed=%v: %w", changed, err)
	}
	for _, key := range stagedKeys {
		_ = s.objects.Delete(context.Background(), key)
	}
	cleanup = false
	version.Status = store.RAGEvalDatasetReady
	return result, nil
}

func (s *DatasetService) Preview(ctx context.Context, versionID, documentCursor, caseCursor string, limit int) (DatasetPreviewPage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	documents, err := s.store.ListRAGEvalCorpusDocuments(ctx, versionID, documentCursor, limit)
	if err != nil {
		return DatasetPreviewPage{}, err
	}
	cases, err := s.store.ListRAGEvalCases(ctx, versionID, caseCursor, limit)
	if err != nil {
		return DatasetPreviewPage{}, err
	}
	page := DatasetPreviewPage{Documents: documents, Cases: cases}
	if len(documents) == limit {
		page.NextDocumentCursor = documents[len(documents)-1].ID
	}
	if len(cases) == limit {
		page.NextCaseCursor = cases[len(cases)-1].ID
	}
	return page, nil
}

func (s *DatasetService) Tombstone(ctx context.Context, datasetID string) (bool, error) {
	lifecycle, ok := s.store.(DatasetLifecycleStore)
	if !ok {
		return false, errors.New("dataset lifecycle store is unavailable")
	}
	return lifecycle.TombstoneRAGEvalDataset(ctx, datasetID)
}

// CleanupStaging removes only staging prefixes for DRAFT/FAILED versions and
// any leftover staging from READY versions older than the caller's TTL cutoff.
func (s *DatasetService) CleanupStaging(ctx context.Context, before time.Time, limit int) (int, error) {
	lifecycle, ok := s.store.(DatasetLifecycleStore)
	if !ok {
		return 0, errors.New("dataset lifecycle store is unavailable")
	}
	candidates, err := lifecycle.ListRAGEvalDatasetStagingCandidates(ctx, before, limit)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, candidate := range candidates {
		if err := s.objects.DeletePrefix(ctx, path.Join("rag-eval", "staging", candidate.ID)+"/"); err != nil {
			return cleaned, err
		}
		cleaned++
	}
	return cleaned, nil
}

// GarbageCollect tombstoned datasets only after the store proves no run
// references any version. SQL is purged first so an object-store failure can
// leak bytes but can never destroy data still referenced by a run.
func (s *DatasetService) GarbageCollect(ctx context.Context, before time.Time, limit int) (int, error) {
	lifecycle, ok := s.store.(DatasetLifecycleStore)
	if !ok {
		return 0, errors.New("dataset lifecycle store is unavailable")
	}
	candidates, err := lifecycle.ListRAGEvalDatasetGCCandidates(ctx, before, limit)
	if err != nil {
		return 0, err
	}
	cleaned := 0
	for _, datasetID := range candidates {
		purged, err := lifecycle.PurgeRAGEvalDataset(ctx, datasetID)
		if err != nil {
			return cleaned, err
		}
		if !purged {
			continue
		}
		if err := s.objects.DeletePrefix(ctx, path.Join("rag-eval", "datasets", datasetID)+"/"); err != nil {
			return cleaned, fmt.Errorf("dataset %s SQL purged; object prefix cleanup required: %w", datasetID, err)
		}
		cleaned++
	}
	return cleaned, nil
}
