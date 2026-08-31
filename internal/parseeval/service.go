package parseeval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	MediaTypeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MediaTypePPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MediaTypeXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

var (
	ErrInvalidUpload          = errors.New("parser evaluation: invalid upload")
	ErrFileTooLarge           = errors.New("parser evaluation: file exceeds size limit")
	ErrDeclaredSizeMismatch   = errors.New("parser evaluation: declared size mismatch")
	ErrIdempotencyConflict    = errors.New("parser evaluation: idempotency conflict")
	ErrUploadInProgress       = errors.New("parser evaluation: upload is in progress")
	ErrForbidden              = errors.New("parser evaluation: forbidden")
	ErrImmutable              = errors.New("parser evaluation: run is immutable")
	ErrInvalidArtifactRequest = errors.New("parser evaluation: invalid artifact request")
	ErrArtifactUnavailable    = errors.New("parser evaluation: artifact unavailable")
)

type parserEvalStore interface {
	CreateParserEvalRun(context.Context, *store.ParserEvalRunRecord) error
	GetParserEvalRun(context.Context, string) (*store.ParserEvalRunRecord, error)
	ReserveParserEvalDocument(context.Context, *store.ParserEvalDocumentRecord, int, int64) error
	CompleteParserEvalDocumentUpload(context.Context, string, string, string) (bool, error)
	AbortParserEvalDocumentUpload(context.Context, string, string) (bool, error)
	DeleteParserEvalDraftDocument(context.Context, string, string) (bool, error)
	GetParserEvalDocument(context.Context, string, string) (*store.ParserEvalDocumentRecord, error)
}

type Service struct {
	store   parserEvalStore
	objects objects.Store
	config  config.ParserEvaluationCfg
	now     func() time.Time
}

func NewService(database parserEvalStore, objectStore objects.Store, cfg config.ParserEvaluationCfg) (*Service, error) {
	if database == nil || objectStore == nil {
		return nil, errors.New("parser evaluation store and object store are required")
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{store: database, objects: objectStore, config: cfg, now: func() time.Time { return time.Now().UTC() }}, nil
}

type DraftSelection struct {
	JudgeModelBindingID string `json:"judgeModelBindingId"`
}

func (s DraftSelection) Validate() error {
	if strings.TrimSpace(s.JudgeModelBindingID) == "" || !utf8.ValidString(s.JudgeModelBindingID) ||
		utf8.RuneCountInString(s.JudgeModelBindingID) > 384 || strings.ContainsRune(s.JudgeModelBindingID, '\x00') {
		return errors.New("judge model binding is required")
	}
	return nil
}

type CreateDraftRequest struct {
	CreatedBy           string
	JudgeModelBindingID string
	IdempotencyKey      string
}

func (s *Service) CreateDraft(ctx context.Context, request CreateDraftRequest) (*store.ParserEvalRunRecord, error) {
	if err := validateActor(request.CreatedBy); err != nil {
		return nil, err
	}
	selection := DraftSelection{JudgeModelBindingID: request.JudgeModelBindingID}
	selectionJSON, err := EncodeBoundedJSON(selection, MaxSnapshotJSONBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidUpload, err)
	}
	id, err := newOperationID("per_", request.CreatedBy, "create-draft", "", request.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if request.IdempotencyKey != "" {
		if existing, getErr := s.store.GetParserEvalRun(ctx, id); getErr == nil {
			return matchDraft(existing, request.CreatedBy, selection)
		} else if !errors.Is(getErr, store.ErrNotFound) {
			return nil, getErr
		}
	}
	now := s.now().UTC()
	record := &store.ParserEvalRunRecord{
		ID:                    id,
		Status:                store.ParserEvalRunDraft,
		Stage:                 store.ParserEvalRunStageUploading,
		ProgressJSON:          `{"documentsTotal":0,"documentsCompleted":0}`,
		ExecutionSnapshotJSON: selectionJSON,
		SummaryJSON:           `{}`,
		CreatedBy:             request.CreatedBy,
		CreatedAt:             now,
		UpdatedAt:             now,
		ExpiresAt:             now.Add(time.Duration(s.config.RetentionDays) * 24 * time.Hour),
	}
	if err := s.store.CreateParserEvalRun(ctx, record); err != nil {
		if request.IdempotencyKey != "" {
			if existing, getErr := s.store.GetParserEvalRun(ctx, id); getErr == nil {
				return matchDraft(existing, request.CreatedBy, selection)
			}
		}
		return nil, err
	}
	return record, nil
}

func matchDraft(record *store.ParserEvalRunRecord, actor string, selection DraftSelection) (*store.ParserEvalRunRecord, error) {
	if record == nil || record.CreatedBy != actor {
		return nil, ErrIdempotencyConflict
	}
	existing, err := DecodeClosedJSON[DraftSelection]([]byte(record.ExecutionSnapshotJSON), MaxSnapshotJSONBytes, func(value *DraftSelection) error {
		return value.Validate()
	})
	if err != nil || existing != selection {
		return nil, ErrIdempotencyConflict
	}
	return record, nil
}

type UploadDocumentRequest struct {
	RunID             string
	CreatedBy         string
	FileName          string
	MediaType         string
	DeclaredSizeBytes int64
	Reader            io.Reader
	IdempotencyKey    string
}

func (s *Service) UploadDocument(ctx context.Context, request UploadDocumentRequest) (*store.ParserEvalDocumentRecord, error) {
	if err := validateActor(request.CreatedBy); err != nil {
		return nil, err
	}
	if err := validateObjectID(request.RunID); err != nil {
		return nil, fmt.Errorf("%w: invalid run ID", ErrInvalidUpload)
	}
	run, err := s.store.GetParserEvalRun(ctx, request.RunID)
	if err != nil {
		return nil, err
	}
	if run.CreatedBy != request.CreatedBy {
		return nil, ErrForbidden
	}
	if run.Status != store.ParserEvalRunDraft {
		return nil, ErrImmutable
	}
	format, err := validateUploadIdentity(request.FileName, request.MediaType)
	if err != nil {
		return nil, err
	}
	spooled, err := s.spoolAndValidate(ctx, request.Reader, request.DeclaredSizeBytes, format)
	if err != nil {
		return nil, err
	}
	defer spooled.Close()

	documentID, err := newOperationID("ped_", request.CreatedBy, "upload-document", request.RunID, request.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	record := &store.ParserEvalDocumentRecord{
		ID:        documentID,
		RunID:     request.RunID,
		FileName:  request.FileName,
		Format:    string(format),
		MediaType: request.MediaType,
		SizeBytes: spooled.size,
		SHA256:    spooled.sha256,
		Status:    store.ParserEvalDocumentUploaded,
		Stage:     store.ParserEvalDocumentStageValidating,
	}
	if request.IdempotencyKey != "" {
		if existing, getErr := s.store.GetParserEvalDocument(ctx, request.RunID, documentID); getErr == nil {
			return matchUploadedDocument(existing, record)
		} else if !errors.Is(getErr, store.ErrNotFound) {
			return nil, getErr
		}
	}
	if err := s.store.ReserveParserEvalDocument(ctx, record, s.config.MaxFiles, s.config.MaxBatchBytes); err != nil {
		if request.IdempotencyKey != "" {
			if existing, getErr := s.store.GetParserEvalDocument(ctx, request.RunID, documentID); getErr == nil {
				return matchUploadedDocument(existing, record)
			}
		}
		return nil, err
	}
	sourceKey, err := SourceObjectKey(request.RunID, documentID)
	if err != nil {
		return nil, s.compensateUpload(ctx, request.RunID, documentID, err)
	}
	if _, err := spooled.file.Seek(0, io.SeekStart); err != nil {
		return nil, s.compensateUpload(ctx, request.RunID, documentID, err)
	}
	if err := s.objects.Put(ctx, sourceKey, io.LimitReader(spooled.file, spooled.size), spooled.size, request.MediaType); err != nil {
		return nil, s.compensateUpload(ctx, request.RunID, documentID, err)
	}
	completed, err := s.store.CompleteParserEvalDocumentUpload(ctx, request.RunID, documentID, sourceKey)
	if err != nil || !completed {
		if err == nil {
			err = ErrImmutable
		}
		return nil, s.compensateUpload(ctx, request.RunID, documentID, err)
	}
	record.SourceObjectKey = sourceKey
	return record, nil
}

func matchUploadedDocument(existing, expected *store.ParserEvalDocumentRecord) (*store.ParserEvalDocumentRecord, error) {
	if existing == nil || expected == nil || existing.RunID != expected.RunID || existing.FileName != expected.FileName ||
		existing.Format != expected.Format || existing.MediaType != expected.MediaType || existing.SizeBytes != expected.SizeBytes ||
		existing.SHA256 != expected.SHA256 {
		return nil, ErrIdempotencyConflict
	}
	if existing.SourceObjectKey == "" {
		return nil, ErrUploadInProgress
	}
	return existing, nil
}

func (s *Service) compensateUpload(ctx context.Context, runID, documentID string, primary error) error {
	prefix, prefixErr := DocumentObjectPrefix(runID, documentID)
	var cleanupErrors []error
	if prefixErr != nil {
		cleanupErrors = append(cleanupErrors, prefixErr)
	} else if err := s.objects.DeletePrefix(ctx, prefix); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("delete upload objects: %w", err))
	}
	if _, err := s.store.AbortParserEvalDocumentUpload(ctx, runID, documentID); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("abort upload reservation: %w", err))
	}
	return errors.Join(append([]error{primary}, cleanupErrors...)...)
}

type spooledUpload struct {
	directory string
	file      *os.File
	size      int64
	sha256    string
}

func (u *spooledUpload) Close() {
	if u == nil {
		return
	}
	if u.file != nil {
		_ = u.file.Close()
	}
	if u.directory != "" {
		_ = os.RemoveAll(u.directory)
	}
}

func (s *Service) spoolAndValidate(ctx context.Context, reader io.Reader, declaredSize int64, format Format) (*spooledUpload, error) {
	if reader == nil || declaredSize <= 0 {
		return nil, fmt.Errorf("%w: file is empty", ErrInvalidUpload)
	}
	if declaredSize > s.config.MaxFileBytes {
		return nil, ErrFileTooLarge
	}
	directory, err := os.MkdirTemp("", "bkcrab-parser-eval-*")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "source.bin"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	spooled := &spooledUpload{directory: directory, file: file}
	keep := false
	defer func() {
		if !keep {
			spooled.Close()
		}
	}()
	hasher := sha256.New()
	limited := &io.LimitedReader{R: &uploadContextReader{ctx: ctx, reader: reader}, N: s.config.MaxFileBytes + 1}
	written, err := io.Copy(io.MultiWriter(file, hasher), limited)
	if err != nil {
		return nil, err
	}
	if written > s.config.MaxFileBytes {
		return nil, ErrFileTooLarge
	}
	if written != declaredSize {
		return nil, ErrDeclaredSizeMismatch
	}
	if written == 0 {
		return nil, fmt.Errorf("%w: file is empty", ErrInvalidUpload)
	}
	if err := validateOOXML(file, written, format); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidUpload, err)
	}
	spooled.size = written
	spooled.sha256 = hex.EncodeToString(hasher.Sum(nil))
	keep = true
	return spooled, nil
}

type uploadContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *uploadContextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func validateUploadIdentity(fileName, mediaType string) (Format, error) {
	if !utf8.ValidString(fileName) || strings.TrimSpace(fileName) == "" || len(fileName) > 512 ||
		utf8.RuneCountInString(fileName) > 255 || strings.ContainsAny(fileName, "/\\\x00") || fileName == "." || fileName == ".." {
		return "", fmt.Errorf("%w: invalid file name", ErrInvalidUpload)
	}
	for _, character := range fileName {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: invalid file name", ErrInvalidUpload)
		}
	}
	extension := strings.ToLower(filepath.Ext(fileName))
	formats := map[string]struct {
		format    Format
		mediaType string
	}{
		".docx": {FormatDOCX, MediaTypeDOCX},
		".pptx": {FormatPPTX, MediaTypePPTX},
		".xlsx": {FormatXLSX, MediaTypeXLSX},
	}
	wanted, exists := formats[extension]
	if !exists || mediaType != wanted.mediaType {
		return "", fmt.Errorf("%w: extension and MIME type must identify docx, pptx, or xlsx", ErrInvalidUpload)
	}
	return wanted.format, nil
}

func validateActor(actor string) error {
	if strings.TrimSpace(actor) == "" || !utf8.ValidString(actor) || utf8.RuneCountInString(actor) > 128 || strings.ContainsRune(actor, '\x00') {
		return ErrForbidden
	}
	return nil
}

func newOperationID(prefix, actor, operation, parentID, idempotencyKey string) (string, error) {
	if idempotencyKey == "" {
		return prefix + uuid.NewString(), nil
	}
	return deterministicID(prefix, actor, operation, parentID, idempotencyKey)
}

func deterministicID(prefix, actor, operation, parentID, idempotencyKey string) (string, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 || !utf8.ValidString(idempotencyKey) || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return "", fmt.Errorf("%w: idempotency key must contain 8 to 128 non-space bytes", ErrInvalidUpload)
	}
	for _, character := range idempotencyKey {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: invalid idempotency key", ErrInvalidUpload)
		}
	}
	digest := sha256.Sum256([]byte(actor + "\x00" + operation + "\x00" + parentID + "\x00" + idempotencyKey))
	return prefix + hex.EncodeToString(digest[:16]), nil
}

func (s *Service) RemoveDraftDocument(ctx context.Context, runID, documentID, actor string) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	run, err := s.store.GetParserEvalRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.CreatedBy != actor {
		return ErrForbidden
	}
	if run.Status != store.ParserEvalRunDraft {
		return ErrImmutable
	}
	if _, err := s.store.GetParserEvalDocument(ctx, runID, documentID); err != nil {
		return err
	}
	prefix, err := DocumentObjectPrefix(runID, documentID)
	if err != nil {
		return err
	}
	if err := s.objects.DeletePrefix(ctx, prefix); err != nil {
		return err
	}
	deleted, err := s.store.DeleteParserEvalDraftDocument(ctx, runID, documentID)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrImmutable
	}
	return nil
}

type ArtifactRequest struct {
	Kind   ArtifactKind
	Page   int
	Engine Engine
	Order  JudgeOrder
}

type Artifact struct {
	Reader      io.ReadCloser
	SizeBytes   int64
	ContentType string
	FileName    string
}

func (s *Service) OpenArtifact(ctx context.Context, runID, documentID string, request ArtifactRequest) (Artifact, error) {
	if err := validateArtifactRequest(request); err != nil {
		return Artifact{}, err
	}
	document, err := s.store.GetParserEvalDocument(ctx, runID, documentID)
	if err != nil {
		return Artifact{}, err
	}
	var key, contentType, fileName string
	var size int64
	switch request.Kind {
	case ArtifactSource:
		key, err = SourceObjectKey(runID, documentID)
		if err == nil && document.SourceObjectKey != key {
			err = ErrArtifactUnavailable
		}
		size, contentType, fileName = document.SizeBytes, document.MediaType, document.FileName
	case ArtifactTruthPage:
		var result TruthResult
		result, err = DecodeClosedJSON[TruthResult]([]byte(document.TruthJSON), MaxResultJSONBytes, func(value *TruthResult) error { return value.Validate() })
		if err == nil && !result.Successful() {
			err = ErrArtifactUnavailable
		}
		if err == nil && request.Page > len(result.Pages) {
			err = ErrArtifactUnavailable
		}
		if err == nil {
			stored := result.Pages[request.Page-1].Artifact
			key, err = TruthPageObjectKey(runID, documentID, request.Page)
			if err == nil && stored.ObjectKey != key {
				err = ErrArtifactUnavailable
			}
			size, contentType, fileName = stored.ByteSize, "image/png", fmt.Sprintf("page-%04d.png", request.Page)
			if stored.MediaType != contentType {
				err = ErrArtifactUnavailable
			}
		}
	case ArtifactMarkdown:
		raw := document.MarkItDownResultJSON
		if request.Engine == EngineAnyDoc {
			raw = document.AnyDocResultJSON
		}
		var result ParserResult
		result, err = DecodeClosedJSON[ParserResult]([]byte(raw), MaxResultJSONBytes, func(value *ParserResult) error { return value.Validate() })
		if err == nil && !result.Successful() {
			err = ErrArtifactUnavailable
		}
		if err == nil {
			key, err = MarkdownObjectKey(runID, documentID, request.Engine)
			if err == nil && result.Markdown.ObjectKey != key {
				err = ErrArtifactUnavailable
			}
			size, contentType, fileName = result.Markdown.ByteSize, "text/markdown; charset=utf-8", string(request.Engine)+".md"
			if result.Markdown.MediaType != contentType {
				err = ErrArtifactUnavailable
			}
		}
	case ArtifactJudgeRaw:
		var result JudgeResult
		result, err = DecodeClosedJSON[JudgeResult]([]byte(document.JudgeResultJSON), MaxResultJSONBytes, func(value *JudgeResult) error { return value.Validate() })
		var slot JudgeSlot
		if request.Order == JudgeMarkItDownA {
			slot = result.MarkItDownA
		} else {
			slot = result.AnyDocA
		}
		if err == nil && !slot.Successful() {
			err = ErrArtifactUnavailable
		}
		if err == nil {
			key, err = JudgeRawObjectKey(runID, documentID, request.Order)
			if err == nil && slot.Raw.ObjectKey != key {
				err = ErrArtifactUnavailable
			}
			size, contentType, fileName = slot.Raw.ByteSize, "application/json", string(request.Order)+".json"
			if slot.Raw.MediaType != contentType {
				err = ErrArtifactUnavailable
			}
		}
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrArtifactUnavailable, err)
	}
	reader, err := s.objects.Get(ctx, key)
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrArtifactUnavailable, err)
	}
	return Artifact{Reader: reader, SizeBytes: size, ContentType: contentType, FileName: fileName}, nil
}

func validateArtifactRequest(request ArtifactRequest) error {
	if !request.Kind.Valid() {
		return ErrInvalidArtifactRequest
	}
	switch request.Kind {
	case ArtifactSource:
		if request.Page != 0 || request.Engine != "" || request.Order != "" {
			return ErrInvalidArtifactRequest
		}
	case ArtifactTruthPage:
		if request.Page <= 0 || request.Page > 100 || request.Engine != "" || request.Order != "" {
			return ErrInvalidArtifactRequest
		}
	case ArtifactMarkdown:
		if request.Page != 0 || !request.Engine.Valid() || request.Order != "" {
			return ErrInvalidArtifactRequest
		}
	case ArtifactJudgeRaw:
		if request.Page != 0 || request.Engine != "" || !request.Order.Valid() {
			return ErrInvalidArtifactRequest
		}
	}
	return nil
}
