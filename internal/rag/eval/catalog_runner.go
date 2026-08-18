package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

var ErrCatalogImportFenceLost = errors.New("evaluation catalog import lease lost")

type CatalogImportStore interface {
	CreateRAGEvalCatalogImport(context.Context, *store.RAGEvalCatalogImportRecord) error
	GetRAGEvalCatalogImport(context.Context, string) (*store.RAGEvalCatalogImportRecord, error)
	ListRAGEvalCatalogImports(context.Context, string, int) ([]store.RAGEvalCatalogImportRecord, error)
	ClaimRAGEvalCatalogImport(context.Context, string, string, time.Duration) (*store.RAGEvalCatalogImportFence, bool, error)
	ClaimNextRAGEvalCatalogImport(context.Context, string, time.Duration) (*store.RAGEvalCatalogImportFence, bool, error)
	HeartbeatRAGEvalCatalogImport(context.Context, store.RAGEvalCatalogImportFence, time.Duration) (bool, error)
	UpdateRAGEvalCatalogImport(context.Context, store.RAGEvalCatalogImportFence, string, string) (bool, error)
	RequestCancelRAGEvalCatalogImport(context.Context, string) (bool, error)
	FinishRAGEvalCatalogImport(context.Context, store.RAGEvalCatalogImportFence, string, string, string, string) (bool, error)
	GetRAGEvalDatasetVersionByNumber(context.Context, string, int64) (*store.RAGEvalDatasetVersionRecord, error)
}

type CatalogImportProgress struct {
	Documents int `json:"documents,omitempty"`
	Cases     int `json:"cases,omitempty"`
}

// CatalogImportRunner owns durable, restart-safe preparation jobs. The worker
// downloads only built-in pinned sources and publishes through DatasetService.
type CatalogImportRunner struct {
	store       CatalogImportStore
	datasets    *DatasetService
	objects     objects.Store
	workerID    string
	workers     int
	lease, poll time.Duration
	startOnce   sync.Once
}

func NewCatalogImportRunner(st CatalogImportStore, datasets *DatasetService, objectStore objects.Store, workerID string, workers int) (*CatalogImportRunner, error) {
	if st == nil || datasets == nil || objectStore == nil || strings.TrimSpace(workerID) == "" || workers < 1 || workers > 8 {
		return nil, errors.New("catalog import runner dependencies are incomplete")
	}
	return &CatalogImportRunner{store: st, datasets: datasets, objects: objectStore, workerID: strings.TrimSpace(workerID),
		workers: workers, lease: 45 * time.Second, poll: time.Second}, nil
}

func (r *CatalogImportRunner) Create(ctx context.Context, datasetID, createdBy string, options CatalogImportOptions) (*store.RAGEvalCatalogImportRecord, error) {
	if strings.TrimSpace(datasetID) == "" || strings.TrimSpace(createdBy) == "" {
		return nil, errors.New("dataset and creator are required")
	}
	if err := options.ApplyDefaults(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	record := &store.RAGEvalCatalogImportRecord{ID: "rei_" + uuid.NewString(), DatasetID: datasetID,
		CatalogID: options.CatalogID, RequestJSON: string(raw), ProgressJSON: "{}", CreatedBy: createdBy}
	if err := r.store.CreateRAGEvalCatalogImport(ctx, record); err != nil {
		return nil, err
	}
	return record, nil
}

func (r *CatalogImportRunner) Get(ctx context.Context, id string) (*store.RAGEvalCatalogImportRecord, error) {
	return r.store.GetRAGEvalCatalogImport(ctx, id)
}

func (r *CatalogImportRunner) List(ctx context.Context, cursor string, limit int) ([]store.RAGEvalCatalogImportRecord, error) {
	return r.store.ListRAGEvalCatalogImports(ctx, cursor, limit)
}

func (r *CatalogImportRunner) Cancel(ctx context.Context, id string) (bool, error) {
	return r.store.RequestCancelRAGEvalCatalogImport(ctx, id)
}

func (r *CatalogImportRunner) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		for index := 0; index < r.workers; index++ {
			go r.worker(ctx, fmt.Sprintf("%s-%d", r.workerID, index))
		}
	})
}

func (r *CatalogImportRunner) worker(ctx context.Context, worker string) {
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	for {
		if err := r.RunNext(ctx, worker); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("rag eval catalog importer", "worker", worker, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *CatalogImportRunner) RunNext(ctx context.Context, worker string) error {
	fence, ok, err := r.store.ClaimNextRAGEvalCatalogImport(ctx, worker, r.lease)
	if err != nil || !ok {
		return err
	}
	return r.runClaimed(ctx, *fence)
}

func (r *CatalogImportRunner) Run(ctx context.Context, id string) error {
	fence, ok, err := r.store.ClaimRAGEvalCatalogImport(ctx, id, r.workerID, r.lease)
	if err != nil || !ok {
		if err == nil {
			err = errors.New("catalog import is not claimable")
		}
		return err
	}
	return r.runClaimed(ctx, *fence)
}

func (r *CatalogImportRunner) runClaimed(parent context.Context, fence store.RAGEvalCatalogImportFence) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var userCancelled atomic.Bool
	heartbeatDone := make(chan struct{})
	leaseError := make(chan error, 1)
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(r.lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				record, err := r.store.GetRAGEvalCatalogImport(ctx, fence.ImportID)
				if err != nil {
					leaseError <- err
					cancel()
					return
				}
				if record.CancelRequestedAt.Valid {
					userCancelled.Store(true)
					cancel()
					return
				}
				ok, err := r.store.HeartbeatRAGEvalCatalogImport(ctx, fence, r.lease)
				if err != nil || !ok {
					if err == nil {
						err = ErrCatalogImportFenceLost
					}
					leaseError <- err
					cancel()
					return
				}
			}
		}
	}()

	record, err := r.store.GetRAGEvalCatalogImport(ctx, fence.ImportID)
	versionID := ""
	if err == nil {
		versionID, err = r.prepareAndImport(ctx, fence, record)
	}
	cancel()
	<-heartbeatDone
	select {
	case leaseErr := <-leaseError:
		if leaseErr != nil {
			return leaseErr
		}
	default:
	}
	if userCancelled.Load() {
		return r.finish(fence, store.RAGEvalRunCancelled, "", "cancelled", "catalog import cancelled")
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && parent.Err() != nil {
			return err // shutdown: let the lease expire so another worker resumes.
		}
		finishErr := r.finish(fence, store.RAGEvalRunFailed, "", "catalog_import_failed", err.Error())
		if finishErr != nil {
			return finishErr
		}
		return err
	}
	latest, getErr := r.store.GetRAGEvalCatalogImport(context.Background(), fence.ImportID)
	if getErr != nil {
		return getErr
	}
	if latest.CancelRequestedAt.Valid {
		return r.finish(fence, store.RAGEvalRunCancelled, versionID, "cancelled", "catalog import cancelled")
	}
	return r.finish(fence, store.RAGEvalRunSucceeded, versionID, "", "")
}

func (r *CatalogImportRunner) prepareAndImport(ctx context.Context, fence store.RAGEvalCatalogImportFence, record *store.RAGEvalCatalogImportRecord) (string, error) {
	var options CatalogImportOptions
	if err := json.Unmarshal([]byte(record.RequestJSON), &options); err != nil {
		return "", errors.New("stored catalog import request is invalid")
	}
	if err := options.ApplyDefaults(); err != nil {
		return "", err
	}
	selectorFingerprint, err := Fingerprint(options)
	if err != nil {
		return "", err
	}
	if existing, lookupErr := r.store.GetRAGEvalDatasetVersionByNumber(ctx, record.DatasetID, record.TargetVersion); lookupErr == nil {
		if existing.Status == store.RAGEvalDatasetReady && existing.SourceType == "builtin-catalog" && existing.SelectorFingerprint == selectorFingerprint {
			return existing.ID, nil
		}
		return "", errors.New("catalog import target version already exists with a different or failed contract")
	} else if !errors.Is(lookupErr, store.ErrNotFound) {
		return "", lookupErr
	}
	preset, ok := CatalogPresetByID(options.CatalogID)
	if !ok || preset.ID != record.CatalogID {
		return "", errors.New("stored catalog preset is invalid")
	}
	adapter, err := CatalogAdapterFor(options.CatalogID)
	if err != nil {
		return "", err
	}
	source, err := NewCatalogHTTPSource(preset, r.objects, nil)
	if err != nil {
		return "", err
	}
	if err := r.progress(ctx, fence, "downloading", CatalogImportProgress{}); err != nil {
		return "", err
	}
	prepared, err := adapter.Prepare(ctx, source, options)
	if err != nil {
		return "", err
	}
	defer prepared.Close()
	progress := CatalogImportProgress{Documents: len(prepared.Documents), Cases: len(prepared.Dataset.Cases)}
	if err := r.progress(ctx, fence, "validating_and_publishing", progress); err != nil {
		return "", err
	}
	result, err := r.datasets.ImportPreparedCatalog(ctx, record.DatasetID, record.TargetVersion, record.CreatedBy, options, prepared)
	if err != nil {
		return "", err
	}
	// Store the produced version before the terminal transition so the success
	// response remains restart-inspectable. The stage update is fenced; its
	// progress payload carries the immutable result ID as well.
	if err := r.progress(ctx, fence, "publishing", struct {
		CatalogImportProgress
		DatasetVersionID string `json:"datasetVersionId"`
	}{progress, result.Version.ID}); err != nil {
		return "", err
	}
	return result.Version.ID, nil
}

func (r *CatalogImportRunner) progress(ctx context.Context, fence store.RAGEvalCatalogImportFence, stage string, progress any) error {
	raw, _ := json.Marshal(progress)
	ok, err := r.store.UpdateRAGEvalCatalogImport(ctx, fence, stage, string(raw))
	if err != nil {
		return err
	}
	if !ok {
		return ErrCatalogImportFenceLost
	}
	return nil
}

func (r *CatalogImportRunner) finish(fence store.RAGEvalCatalogImportFence, status, versionID, code, message string) error {
	ok, err := r.store.FinishRAGEvalCatalogImport(context.Background(), fence, status, versionID, code, message)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCatalogImportFenceLost
	}
	return nil
}
