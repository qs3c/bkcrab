package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
)

// ImportPreparedCatalog publishes the adapter output through the same
// immutable validation and object-staging path used by canonical uploads.
// Callers retain ownership of prepared and must Close it after this returns.
func (s *DatasetService) ImportPreparedCatalog(ctx context.Context, datasetID string, version int64, createdBy string, options CatalogImportOptions, prepared *PreparedCatalogDataset) (DatasetImportResult, error) {
	if prepared == nil {
		return DatasetImportResult{}, errors.New("prepared catalog dataset is required")
	}
	if err := options.ApplyDefaults(); err != nil {
		return DatasetImportResult{}, err
	}
	manifest, err := json.Marshal(prepared.Dataset)
	if err != nil {
		return DatasetImportResult{}, err
	}
	selectorFingerprint, err := Fingerprint(options)
	if err != nil {
		return DatasetImportResult{}, err
	}
	uploads := make([]DatasetDocumentUpload, 0, len(prepared.Documents))
	readers := make([]io.ReadCloser, 0, len(prepared.Documents))
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()
	for _, document := range prepared.Documents {
		reader, openErr := document.Open()
		if openErr != nil {
			return DatasetImportResult{}, openErr
		}
		readers = append(readers, reader)
		uploads = append(uploads, DatasetDocumentUpload{ExternalID: document.ID, FileName: document.FileName,
			MediaType: document.MediaType, SizeBytes: document.SizeBytes, Reader: reader})
	}
	return s.ImportCanonical(ctx, DatasetImportRequest{
		DatasetID: datasetID, Version: version, CreatedBy: createdBy,
		SourceType: "builtin-catalog", Track: prepared.Dataset.Track, Source: prepared.Dataset.Source,
		SelectorFingerprint: selectorFingerprint, Manifest: bytes.NewReader(manifest), Documents: uploads,
	})
}
