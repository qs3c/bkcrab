package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	ragassets "github.com/qs3c/bkcrab/internal/rag/assets"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

var errIndexFenceLost = errors.New("RAG index fence lost")

const (
	indexTaskPendingLimitCode       = "pending_limit"
	indexTaskReindexRateLimitCode   = "reindex_rate_limit"
	indexTaskRejectedTelemetryState = "rejected"
)

// Start launches bounded durable workers. The in-process channel only reduces
// latency; every wake makes a SQL claim, and the periodic pump makes a dropped
// wake (including a full channel) recover without a process restart.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		for i := 0; i < s.workerCount; i++ {
			go s.worker(ctx)
		}
		go s.taskPump(ctx)
		go s.documentAIReconcileLoop(ctx)
		go s.lifecycleLoop(ctx)
		s.wakeWorkers()
	})
}

func (s *Service) taskPump(ctx context.Context) {
	interval := s.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.wakeWorkers()
		}
	}
}

func (s *Service) wakeWorkers() {
	for i := 0; i < s.workerCount; i++ {
		s.scheduleTask(0)
	}
}

// recoverTasks is retained as a compatibility shim for callers/tests from the
// pre-lease worker. Recovery is now a durable SQL claim, not a one-time list.
func (s *Service) recoverTasks(context.Context) { s.wakeWorkers() }

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.tasks:
			s.claimAvailable(ctx)
		}
	}
}

func (s *Service) claimAvailable(ctx context.Context) {
	for ctx.Err() == nil {
		claim, err := s.st.ClaimRAGIndexTask(ctx, s.workerID, s.leaseDuration)
		if err != nil {
			telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
				Transition: "claim", Outcome: "error", ErrorCode: "store_error",
			})
			if ctx.Err() == nil {
				slog.Error("rag: durable index claim failed", "worker", s.workerID, "error", err)
			}
			return
		}
		if claim == nil {
			return
		}
		s.runClaim(ctx, claim)
	}
}

// UploadDocument validates and persists an original document, its immutable
// version snapshot, and its durable task in one relational transaction.
func (s *Service) UploadDocument(ctx context.Context, ownerID, kbID, fileName string, r io.Reader, size int64) (*store.RAGDocumentRecord, error) {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
	}
	if err := s.requireActiveUser(ctx, kb.UserID); err != nil {
		return nil, err
	}
	if kb.Status != "active" {
		return nil, errors.New("知识库正在删除中")
	}
	fileName = strings.TrimSpace(fileName)
	if !parse.SupportedExt(fileName) {
		return nil, fmt.Errorf("不支持的文件类型（支持 md/markdown/txt/pdf；Office 需能力可用）")
	}
	fileType := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
	if fileType == "markdown" {
		fileType = "md"
	}
	if fileType == "docx" || fileType == "pptx" || fileType == "xlsx" {
		if s.officeAvailable == nil || !s.officeAvailable() {
			return nil, errors.New("Office 文档解析能力当前不可用")
		}
	}
	if size < 0 {
		return nil, errors.New("文件大小不能为负数")
	}
	maxBytes := int64(s.cfg.Limits.MaxFileMB) * 1024 * 1024
	if size > maxBytes {
		return nil, fmt.Errorf("%w: 单文件上限 %dMB", ErrQuota, s.cfg.Limits.MaxFileMB)
	}
	docs, err := s.st.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		return nil, err
	}
	if len(docs) >= s.cfg.Limits.MaxDocsPerKB {
		return nil, fmt.Errorf("%w: 每知识库最多 %d 篇文档", ErrQuota, s.cfg.Limits.MaxDocsPerKB)
	}

	docID := "doc_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	key := objects.Key(kb.UserID, kbID, docID, fileName)
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	objectFence, err := s.st.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kbID, DocID: docID,
		ObjectKind: store.RAGObjectKindOriginal, ObjectKey: key, ReferenceKey: docID,
	})
	if err != nil {
		return nil, fmt.Errorf("register original object write: %w", err)
	}
	hasher := sha256.New()
	if err := s.obj.Put(ctx, key, io.TeeReader(r, hasher), size, contentType); err != nil {
		return nil, fmt.Errorf("保存原件: %w", err)
	}
	if ready, err := s.st.MarkRAGObjectWriteReady(ctx, *objectFence); err != nil {
		return nil, fmt.Errorf("mark original object ready: %w", err)
	} else if !ready {
		return nil, fmt.Errorf("mark original object ready: %w", store.ErrRAGLifecycleInactive)
	}
	doc := &store.RAGDocumentRecord{
		ID:                 docID,
		KBID:               kbID,
		FileName:           filepath.Base(fileName),
		FileType:           fileType,
		FileSize:           size,
		ObjectKey:          key,
		Status:             "PENDING",
		Version:            1,
		SourceSHA256:       hex.EncodeToString(hasher.Sum(nil)),
		ActiveVersion:      0,
		IndexFormatVersion: 1,
		ProcessingStage:    "queued",
		UploadedAt:         time.Now().UTC(),
	}
	snapshot, err := s.BuildVersionSnapshot(ctx, doc)
	if err != nil {
		_ = s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID))
		return nil, err
	}
	snapshot.DocVersion = doc.Version
	taskID, err := s.st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, snapshot, 3, store.RAGAdvancedEnqueuePolicy{
		UserID:             kb.UserID,
		MaxPendingTasks:    s.cfg.Limits.MaxPendingAdvancedTasksPerUser,
		MinReindexInterval: time.Duration(s.cfg.Limits.MinAdvancedReindexInterval) * time.Second,
	})
	if err != nil {
		_ = s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID))
		s.emitIndexTaskPolicyRejection(ctx, doc.ID, doc.Version, "enqueue", err)
		return nil, err
	}
	s.scheduleTask(taskID)
	return doc, nil
}

func (s *Service) ReindexDocument(ctx context.Context, ownerID, kbID, docID string) error {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(docID)
	docLock.Lock()
	defer docLock.Unlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	if err := s.requireActiveUser(ctx, kb.UserID); err != nil {
		return err
	}
	if kb.Status != "active" {
		return errors.New("知识库正在删除中")
	}
	doc, err := s.GetDocument(ctx, ownerID, kbID, docID)
	if err != nil {
		return err
	}
	snapshot, err := s.BuildVersionSnapshot(ctx, doc)
	if err != nil {
		return err
	}
	snapshot.DocVersion = 0 // assigned atomically by the store
	task, err := s.st.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, doc.Version, snapshot, store.RAGAdvancedEnqueuePolicy{
		UserID:             kb.UserID,
		MaxPendingTasks:    s.cfg.Limits.MaxPendingAdvancedTasksPerUser,
		MinReindexInterval: time.Duration(s.cfg.Limits.MinAdvancedReindexInterval) * time.Second,
	})
	if err != nil {
		s.emitIndexTaskPolicyRejection(ctx, doc.ID, doc.Version, "reindex", err)
		return err
	}
	s.scheduleTask(task.ID)
	return nil
}

func (s *Service) emitIndexTaskPolicyRejection(
	ctx context.Context,
	docID string,
	docVersion int64,
	transition string,
	err error,
) {
	errorCode := ""
	switch {
	case errors.Is(err, store.ErrRAGAdvancedPendingLimit):
		errorCode = indexTaskPendingLimitCode
	case errors.Is(err, store.ErrRAGAdvancedReindexRateLimit):
		errorCode = indexTaskReindexRateLimitCode
	default:
		return
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
		DocID: docID, DocVersion: docVersion, Transition: transition,
		Outcome: indexTaskRejectedTelemetryState, ErrorCode: errorCode,
	})
}

func (s *Service) DeleteDocument(ctx context.Context, ownerID, kbID, docID string) error {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()

	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return err
	}
	if _, err := s.GetDocument(ctx, ownerID, kbID, docID); err != nil {
		return err
	}
	// Persist the tombstone before waiting for an in-process index worker. SQL
	// search/claim snapshots and both asset authorization paths therefore fail
	// closed immediately, while cleanup remains safely retryable.
	doc, err := s.st.MarkRAGDocumentDeleting(ctx, docID)
	if err != nil {
		return err
	}
	docLock := s.docMutex(docID)
	docLock.Lock()
	defer docLock.Unlock()
	return s.cleanupDeletingDocument(ctx, doc)
}

func (s *Service) scheduleTask(taskID int64) {
	select {
	case s.tasks <- taskID:
	default:
		// The durable row is authoritative. A later poll will create another
		// wake after capacity becomes available.
		if taskID != 0 {
			slog.Warn("rag: index wake queue full; durable poller will recover", "task", taskID)
		}
	}
}

// runTask remains for package-level compatibility tests. The id is only a
// hint: correctness requires claiming the next due row from SQL.
func (s *Service) runTask(ctx context.Context, _ int64) {
	claim, err := s.st.ClaimRAGIndexTask(ctx, s.workerID, s.leaseDuration)
	if err != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
			Transition: "claim", Outcome: "error", ErrorCode: "store_error",
		})
		slog.Error("rag: durable index claim failed", "worker", s.workerID, "error", err)
		return
	}
	if claim != nil {
		s.runClaim(ctx, claim)
	}
}

func (s *Service) runClaim(parent context.Context, claim *store.RAGIndexClaim) {
	if claim == nil {
		return
	}
	defer func() {
		if _, err := s.st.AcknowledgeRAGIndexTaskQuiesced(parent, claim.Fence); err != nil && parent.Err() == nil {
			slog.Warn("rag: failed to acknowledge superseded worker quiescence",
				"task", claim.Fence.TaskID, "error", err)
		}
	}()
	claimStarted := time.Now()
	telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
		claim, "claim", "ok", "",
	))
	workCtx, cancelWork := context.WithCancel(parent)
	defer cancelWork()
	heartbeatCtx, stopHeartbeat := context.WithCancel(parent)
	var leaseLost atomic.Bool
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		s.heartbeatLoop(heartbeatCtx, claim.Fence, &leaseLost, cancelWork)
	}()
	stopAndWaitHeartbeat := func() {
		stopHeartbeat()
		<-heartbeatDone
	}

	doc, err := s.st.GetRAGDocument(workCtx, claim.Fence.DocID)
	previousActive := int64(0)
	if err == nil {
		previousActive = doc.ActiveVersion
	}
	var embeddingBinding config.RAGEmbeddingCfg
	if err == nil {
		var current *store.RAGDocumentVersionRecord
		current, embeddingBinding, err = s.buildVersionSnapshotAndBinding(workCtx, doc)
		if err == nil && !sameRuntimeProviderContracts(&claim.Version, current) {
			current.DocVersion = 0
			created, ok, supersedeErr := s.st.SupersedeRAGIndexTaskAndCreateVersion(workCtx, claim.Fence, current)
			stopAndWaitHeartbeat()
			if supersedeErr != nil {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "error", "store_error",
				))
				slog.Error("rag: supersede provider-mismatched index task", "task", claim.Fence.TaskID, "error", supersedeErr)
				return
			}
			if ok && created != nil {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "ok", "",
				))
				s.scheduleTask(created.ID)
			} else {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "rejected", "fence_lost",
				))
			}
			return
		}
	}
	if err != nil {
		stopAndWaitHeartbeat()
		s.finishClaimFailure(parent, claim, err, leaseLost.Load())
		return
	}

	activation, err := s.indexClaim(workCtx, claim, embeddingBinding)
	if errors.Is(err, errIndexFenceLost) {
		cancelWork()
	}
	stopAndWaitHeartbeat()
	if err != nil {
		s.finishClaimFailure(parent, claim, err, leaseLost.Load())
		return
	}
	if leaseLost.Load() || parent.Err() != nil {
		return
	}
	ok, err := s.st.ActivateAndFinishRAGIndexTask(parent, claim.Fence, activation, s.gcGracePeriod)
	if err != nil {
		telemetry.Emit(parent, s.telemetry, telemetry.EventActiveVersionSwitch, telemetry.Fields{
			DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID, DocVersion: claim.Fence.DocVersion,
			PreviousVersion: previousActive, ClaimGeneration: claim.Fence.ClaimGeneration,
			Transition: "activate", Outcome: "error", ErrorCode: "store_error",
			Duration: time.Since(claimStarted),
		})
		slog.Error("rag: atomic index activation failed", "task", claim.Fence.TaskID, "error", err)
		return
	}
	if !ok {
		telemetry.Emit(parent, s.telemetry, telemetry.EventActiveVersionSwitch, telemetry.Fields{
			DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID, DocVersion: claim.Fence.DocVersion,
			PreviousVersion: previousActive, ClaimGeneration: claim.Fence.ClaimGeneration,
			Transition: "activate", Outcome: "rejected", ErrorCode: "fence_lost",
			Duration: time.Since(claimStarted),
		})
		slog.Info("rag: skipped activation after index fence was lost", "task", claim.Fence.TaskID,
			"doc_version", claim.Fence.DocVersion, "generation", claim.Fence.ClaimGeneration)
		return
	}
	retiredVersion := int64(0)
	if previousActive > 0 && previousActive != claim.Fence.DocVersion {
		retiredVersion = previousActive
	}
	telemetry.Emit(parent, s.telemetry, telemetry.EventActiveVersionSwitch, telemetry.Fields{
		DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID, DocVersion: claim.Fence.DocVersion,
		PreviousVersion: previousActive, RetiredVersion: retiredVersion,
		ClaimGeneration: claim.Fence.ClaimGeneration, Transition: "activate", Outcome: "ok",
		Duration: time.Since(claimStarted),
	})
}

func (s *Service) heartbeatLoop(
	ctx context.Context,
	fence store.IndexFence,
	leaseLost *atomic.Bool,
	cancelWork context.CancelFunc,
) {
	interval := s.heartbeatInterval
	if interval <= 0 || interval >= s.leaseDuration {
		interval = s.leaseDuration / 3
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := s.st.HeartbeatRAGIndexTask(ctx, fence, s.leaseDuration)
			if ctx.Err() != nil {
				return
			}
			if err != nil || !ok {
				errorCode := "fence_lost"
				if err != nil {
					errorCode = "store_error"
				}
				telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
					DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
					ClaimGeneration: fence.ClaimGeneration, Transition: "heartbeat",
					Outcome: "error", ErrorCode: errorCode,
				})
				leaseLost.Store(true)
				cancelWork()
				if err != nil && ctx.Err() == nil {
					slog.Error("rag: index heartbeat failed; canceling work", "task", fence.TaskID, "error", err)
				}
				return
			}
			telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
				DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
				ClaimGeneration: fence.ClaimGeneration, Transition: "heartbeat", Outcome: "ok",
			})
		}
	}
}

func (s *Service) finishClaimFailure(parent context.Context, claim *store.RAGIndexClaim, err error, leaseLost bool) {
	if err == nil || claim == nil || leaseLost || parent.Err() != nil || errors.Is(err, errIndexFenceLost) {
		return
	}
	transient := isTransientIndexError(err)
	message := safeIndexErrorMessage(err, transient)
	if transient && claim.Task.RetryCount < claim.Task.MaxRetry {
		delay := indexRetryDelay(claim.Task.RetryCount + 1)
		ok, retryErr := s.st.RetryRAGIndexTask(parent, claim.Fence, message, delay)
		if retryErr != nil {
			telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
				claim, "retry", "error", "store_error",
			))
			slog.Error("rag: persist transient index retry", "task", claim.Fence.TaskID, "error", retryErr)
		} else if ok {
			fields := indexTaskTelemetryFields(claim, "retry", "scheduled", "")
			fields.RetryCount = claim.Task.RetryCount + 1
			telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, fields)
			slog.Warn("rag: transient index failure scheduled for retry", "task", claim.Fence.TaskID,
				"retry", claim.Task.RetryCount+1, "delay", delay, "error", message)
		} else {
			telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
				claim, "retry", "rejected", "fence_lost",
			))
		}
		return
	}
	ok, failErr := s.st.FailRAGIndexTask(parent, claim.Fence, message)
	if failErr != nil {
		telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
			claim, "finish", "error", "store_error",
		))
		slog.Error("rag: persist permanent index failure", "task", claim.Fence.TaskID, "error", failErr)
	} else if ok {
		telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
			claim, "finish", "error", "permanent_failure",
		))
		slog.Error("rag: document indexing failed permanently", "task", claim.Fence.TaskID, "error", message)
	} else {
		telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
			claim, "finish", "rejected", "fence_lost",
		))
	}
}

func indexTaskTelemetryFields(
	claim *store.RAGIndexClaim,
	transition, outcome, errorCode string,
) telemetry.Fields {
	if claim == nil {
		return telemetry.Fields{Transition: transition, Outcome: outcome, ErrorCode: errorCode}
	}
	return telemetry.Fields{
		DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID,
		DocVersion: claim.Fence.DocVersion, ClaimGeneration: claim.Fence.ClaimGeneration,
		RetryCount: claim.Task.RetryCount, Transition: transition, Outcome: outcome,
		ErrorCode: errorCode,
	}
}

func safeIndexErrorMessage(err error, transient bool) string {
	switch {
	case errors.Is(err, parse.ErrEmptyContent):
		return parse.ErrEmptyContent.Error()
	case errors.Is(err, parse.ErrDocumentLimitExceeded), errors.Is(err, sidecar.ErrBundleLimitExceeded),
		errors.Is(err, sidecar.ErrSourceLimitExceeded):
		return "文档超过解析硬限制"
	case errors.Is(err, parse.ErrSourceIntegrity), errors.Is(err, sidecar.ErrSourceIntegrity):
		return "文档原件与不可变快照不一致"
	case errors.Is(err, parse.ErrInvalidDocument):
		return "文档格式或内容无效"
	case errors.Is(err, sidecar.ErrInvalidBundle):
		return "文档解析服务返回不兼容结果"
	case errors.Is(err, sidecar.ErrCapabilityUnavailable):
		return "所需文档解析能力当前不可用"
	}
	var statusErr interface{ HTTPStatus() int }
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("文档索引依赖返回 HTTP %d", statusErr.HTTPStatus())
	}
	if transient {
		return "文档索引暂时失败，稍后重试"
	}
	return "文档索引失败"
}

func indexRetryDelay(retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	if retry > 8 {
		retry = 8
	}
	return time.Duration(1<<(retry-1)) * time.Second
}

func isTransientIndexError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, parse.ErrEmptyContent) || errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, parse.ErrInvalidDocument) || errors.Is(err, parse.ErrDocumentLimitExceeded) ||
		errors.Is(err, parse.ErrSourceIntegrity) || errors.Is(err, sidecar.ErrCapabilityUnavailable) ||
		errors.Is(err, sidecar.ErrInvalidBundle) || errors.Is(err, sidecar.ErrBundleLimitExceeded) ||
		errors.Is(err, sidecar.ErrSourceLimitExceeded) || errors.Is(err, sidecar.ErrSourceIntegrity) ||
		errors.Is(err, store.ErrRAGDocumentAIBudgetExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var statusErr interface{ HTTPStatus() int }
	if errors.As(err, &statusErr) {
		status := statusErr.HTTPStatus()
		switch {
		case status == http.StatusRequestTimeout,
			status == http.StatusTooEarly,
			status == http.StatusTooManyRequests,
			status >= 500 && status <= 599:
			return true
		case status >= 400 && status <= 499:
			return false
		}
	}
	message := strings.ToLower(err.Error())
	for _, permanent := range []string{
		"不支持的文件类型", "分块结果为空", "维度不符", "非法 index", "重复 index",
		"schema", "validation", "unsupported", "exceeds", "上限", "不能为空",
		"配置不可用", "endpoint 不可用", "knowledge base is not active",
	} {
		if strings.Contains(message, permanent) {
			return false
		}
	}
	if strings.Contains(message, "返回 429") {
		return true
	}
	for status := 400; status <= 499; status++ {
		if status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests {
			continue
		}
		if strings.Contains(message, fmt.Sprintf("返回 %d", status)) {
			return false
		}
	}
	for status := 500; status <= 599; status++ {
		if strings.Contains(message, fmt.Sprintf("返回 %d", status)) {
			return true
		}
	}
	// SQL, object-store and vector-store failures are retryable unless they
	// matched a deterministic validation/corruption condition above.
	return true
}

func (s *Service) indexClaim(
	ctx context.Context,
	claim *store.RAGIndexClaim,
	embeddingBinding config.RAGEmbeddingCfg,
) (activation store.RAGIndexActivation, resultErr error) {
	fence := claim.Fence
	version := &claim.Version

	initial, err := s.st.GetRAGDocument(ctx, fence.DocID)
	if err != nil {
		return activation, err
	}
	kbLock := s.kbMutex(initial.KBID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(fence.DocID)
	docLock.Lock()
	defer docLock.Unlock()

	doc, err := s.st.GetRAGDocument(ctx, fence.DocID)
	if err != nil {
		return activation, err
	}
	if strings.EqualFold(doc.Status, "deleting") ||
		doc.Version != fence.DocVersion || version.DocVersion != fence.DocVersion {
		return activation, errIndexFenceLost
	}
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return activation, err
	}
	if kb.Status != "active" {
		return activation, errors.New("knowledge base is not active")
	}
	parseMode := config.ParseMode(version.ParseMode)
	if !parseMode.Valid() {
		return activation, fmt.Errorf("unsupported parse mode %q", version.ParseMode)
	}
	if version.ChunkSize <= 0 || version.ChunkOverlap < 0 || version.ChunkOverlap >= version.ChunkSize {
		return activation, errors.New("invalid immutable chunk contract")
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "loading"}); err != nil {
		return activation, err
	}
	budget, err := s.newTaskDocumentAIBudget(claim, kb.UserID)
	if err != nil {
		return activation, err
	}
	artifact, _, artifactKey, err := s.loadOrParseArtifact(ctx, claim, kb, doc, parseMode, budget)
	if err != nil {
		return activation, err
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "chunking"}); err != nil {
		return activation, err
	}
	chunks := split.SplitArtifact(artifact, split.Config{
		ChunkSize: version.ChunkSize, ChunkOverlap: version.ChunkOverlap,
		EnhancementReserveTokens: func() int {
			if version.EnrichmentEnabled {
				return version.ChunkSize / 5
			}
			return 0
		}(),
	})
	if len(chunks) == 0 {
		return activation, errors.New("分块结果为空")
	}

	finalizeConfig := enrich.FinalizeConfig{
		ChunkSize: version.ChunkSize, MaxSearchContentBytes: s.cfg.Limits.MaxSearchContentBytes,
		CollectionMaxLength: config.RAGMilvusContentMaxLength, ProviderTokenizer: s.tokenizer,
	}
	chunks, enrichmentWarnings, err := s.splitAndEnrich(
		ctx, fence, version, kb, doc, chunks, finalizeConfig, budget,
	)
	if err != nil {
		return activation, err
	}
	chunks, err = enrich.FinalizeChunks(ctx, chunks, finalizeConfig)
	if err != nil {
		return activation, fmt.Errorf("finalize searchable chunks: %w", err)
	}
	if len(chunks) == 0 {
		return activation, errors.New("分块结果为空")
	}

	warningCount := len(artifact.Warnings) + len(enrichmentWarnings)
	degraded := len(enrichmentWarnings) > 0
	for _, warning := range artifact.Warnings {
		degraded = degraded || warning.Degraded
	}
	if err := s.fencedWarnings(ctx, fence, degraded, warningCount); err != nil {
		return activation, err
	}

	vectors, totalTokens, err := s.embedChunks(ctx, fence, kb.ID, version, embeddingBinding, chunks)
	if err != nil {
		return activation, err
	}
	vectorChunks, err := s.stageIndexVersion(ctx, fence, kb.ID, doc.ID, chunks, vectors)
	if err != nil {
		return activation, err
	}
	if err := s.upsertIndexVersion(ctx, fence, kb.ID, vectorChunks); err != nil {
		return activation, err
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "finalizing", Current: len(chunks), Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return activation, err
	}
	activation = store.RAGIndexActivation{
		VersionResult: store.RAGDocumentVersionResult{
			Status: store.RAGDocumentVersionDone, ParseArtifactKey: artifactKey,
			PageCount: parsedArtifactPageCount(artifact), AssetCount: len(artifact.Assets),
			Degraded: degraded, WarningCount: warningCount,
		},
		ChunkCount: len(chunks), TokenCount: totalTokens,
	}
	return activation, nil
}

const pipelineStageBatchSize = 200

func (s *Service) parsedArtifactPersister() *ragassets.Persister {
	maxArtifactBytes := s.cfg.Limits.MaxExtractedBytes
	if maxArtifactBytes <= 0 {
		maxArtifactBytes = 200 << 20
	}
	return &ragassets.Persister{
		Objects: s.obj, Catalog: s.st,
		Limits: ragassets.Limits{
			MaxAssets: s.cfg.Limits.MaxAssetsPerDocument, MaxAssetBytes: s.cfg.Limits.MaxAssetBytes,
			MaxExtractedBytes: s.cfg.Limits.MaxExtractedBytes, MaxImagePixels: s.cfg.Limits.MaxImagePixels,
			MaxArtifactBytes: maxArtifactBytes, DisplayMaxEdge: s.cfg.Limits.DisplayMaxEdge,
			ThumbnailMaxEdge: s.cfg.Limits.ThumbnailMaxEdge,
		},
	}
}

func (s *Service) loadOrParseArtifact(
	ctx context.Context,
	claim *store.RAGIndexClaim,
	kb *store.RAGKBRecord,
	doc *store.RAGDocumentRecord,
	parseMode config.ParseMode,
	budget *vision.TaskDocumentAIBudget,
) (*document.ParsedArtifact, bool, string, error) {
	version, fence := &claim.Version, claim.Fence
	persister := s.parsedArtifactPersister()
	logicalArtifactKey, err := document.ArtifactJSONKey(
		kb.UserID, kb.ID, doc.ID, version.ParseFingerprint,
	)
	if err != nil {
		return nil, false, "", err
	}
	artifactKey := strings.TrimSpace(version.ParseArtifactKey)
	if artifactKey == "" {
		artifactKey, err = document.VersionedObjectKey(logicalArtifactKey, fence.DocVersion)
		if err != nil {
			return nil, false, "", err
		}
		// A chunk-only reindex reuses the active generation's immutable parse
		// artifact. Validate it before pinning the new version so a missing or
		// corrupt old object can fall back to this version's own physical key.
		if doc.ActiveVersion > 0 && doc.ActiveVersion != fence.DocVersion {
			active, activeErr := s.st.GetRAGDocumentVersion(ctx, doc.ID, doc.ActiveVersion)
			if activeErr != nil && !errors.Is(activeErr, store.ErrNotFound) {
				return nil, false, "", activeErr
			}
			if activeErr == nil && active.ParseFingerprint == version.ParseFingerprint &&
				strings.TrimSpace(active.ParseArtifactKey) != "" {
				reuseKey := strings.TrimSpace(active.ParseArtifactKey)
				reuseRequest := ragassets.CacheRequest{
					UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion,
					ParseFingerprint: version.ParseFingerprint, ArtifactObjectKey: reuseKey,
					IndexFence: &fence,
					ExpectedSource: &document.ParsedSource{
						DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType,
						ByteSize: doc.FileSize, SHA256: version.SourceSHA256,
					},
				}
				if reused, hit, loadErr := persister.LoadParsedArtifact(ctx, reuseRequest); loadErr != nil {
					return nil, false, "", fmt.Errorf("load active parsed artifact cache: %w", loadErr)
				} else if hit && parsedArtifactMatchesSource(reused, doc, version) {
					recorded, recordErr := s.st.RecordRAGDocumentParseArtifact(ctx, fence, reuseKey)
					if recordErr != nil {
						return nil, false, "", fmt.Errorf("record reused parsed artifact: %w", recordErr)
					}
					if !recorded {
						return nil, false, "", errIndexFenceLost
					}
					telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
						DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
						ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
						CacheStatus: "hit", Outcome: "ok",
					})
					return reused, true, reuseKey, nil
				}
			}
		}
	}
	recorded, err := s.st.RecordRAGDocumentParseArtifact(ctx, fence, artifactKey)
	if err != nil {
		return nil, false, "", fmt.Errorf("record parsed artifact cleanup handle: %w", err)
	}
	if !recorded {
		return nil, false, "", errIndexFenceLost
	}
	expectedSource := document.ParsedSource{
		DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType,
		ByteSize: doc.FileSize, SHA256: version.SourceSHA256,
	}
	cacheRequest := ragassets.CacheRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion,
		ParseFingerprint: version.ParseFingerprint, ExpectedSource: &expectedSource,
		ArtifactObjectKey: artifactKey, IndexFence: &fence,
	}
	artifact, hit, err := persister.LoadParsedArtifact(ctx, cacheRequest)
	if err != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
			DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
			ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
			CacheStatus: "error", Outcome: "error", ErrorCode: "store_error",
		})
		return nil, false, "", fmt.Errorf("load parsed artifact cache: %w", err)
	}
	if hit && parsedArtifactMatchesSource(artifact, doc, version) {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
			DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
			ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
			CacheStatus: "hit", Outcome: "ok",
		})
		return artifact, true, artifactKey, nil
	}
	cacheStatus := "miss"
	if hit {
		cacheStatus = "stale"
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
		DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
		ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
		CacheStatus: cacheStatus, Outcome: "ok",
	})
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "parsing"}); err != nil {
		return nil, false, "", err
	}
	source := document.Source{
		DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType,
		Size: doc.FileSize, SHA256: version.SourceSHA256,
		Open: func(openCtx context.Context) (io.ReadCloser, error) {
			reader, openErr := s.obj.Get(openCtx, doc.ObjectKey)
			if openErr != nil {
				return nil, fmt.Errorf("读原件: %w", openErr)
			}
			return reader, nil
		},
	}
	parsed, parseErr := s.parser.Parse(ctx, source, parse.ParseOptions{
		Mode: parseMode, ParserVersion: version.ParserVersion,
		PageTranscriber: s.pageVision, ImageTranscriber: s.imageVision,
		DocumentAIBudget: budget,
		VisionScope: vision.CacheScope{
			UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID,
			ParseFingerprint: version.ParseFingerprint,
		},
		Progress: func(progressCtx context.Context, progress parse.ParseProgress) error {
			stage := strings.TrimSpace(progress.Stage)
			if stage == "" {
				stage = "parsing"
			}
			return s.fencedProgress(progressCtx, fence, store.RAGIndexProgress{
				Stage: stage, Current: progress.Current, Total: progress.Total, Unit: progress.Unit,
			})
		},
	})
	if parseErr != nil {
		if parsed != nil {
			parseErr = errors.Join(parseErr, parsed.Close())
		}
		return nil, false, "", parseErr
	}
	if parsed == nil {
		return nil, false, "", errors.New("parser returned a nil document")
	}
	if err := normalizeParsedDocument(parsed); err != nil {
		return nil, false, "", errors.Join(err, parsed.Close())
	}
	valid, err := s.st.CheckRAGIndexFence(ctx, fence)
	if err != nil {
		return nil, false, "", errors.Join(err, parsed.Close())
	}
	if !valid {
		return nil, false, "", errors.Join(errIndexFenceLost, parsed.Close())
	}
	artifact, err = persister.PersistParsedDocument(ctx, ragassets.PersistRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion,
		ParseFingerprint: version.ParseFingerprint, NeutralCaption: "图片（未进行视觉识别）",
		ArtifactObjectKey:   artifactKey,
		NormalizedObjectKey: path.Join(path.Dir(artifactKey), "normalized.md"),
		IndexFence:          &fence,
		Document:            parsed,
	})
	if err != nil {
		return nil, false, "", fmt.Errorf("persist parsed assets and artifact: %w", err)
	}
	return artifact, false, artifactKey, nil
}

func parsedArtifactMatchesSource(
	artifact *document.ParsedArtifact,
	doc *store.RAGDocumentRecord,
	version *store.RAGDocumentVersionRecord,
) bool {
	if artifact == nil || doc == nil || version == nil {
		return false
	}
	return artifact.Source.DocID == doc.ID && artifact.Source.FileName == doc.FileName &&
		strings.EqualFold(strings.TrimPrefix(artifact.Source.Format, "."), strings.TrimPrefix(doc.FileType, ".")) &&
		artifact.Source.ByteSize == doc.FileSize && artifact.Source.SHA256 == version.SourceSHA256
}

func normalizeParsedDocument(parsed *document.ParsedDocument) error {
	if parsed == nil {
		return errors.New("parsed document is nil")
	}
	occurrences := make(map[string]document.AssetOccurrence, len(parsed.Occurrences))
	for _, occurrence := range parsed.Occurrences {
		if _, duplicate := occurrences[occurrence.ID]; duplicate {
			return fmt.Errorf("duplicate parser occurrence %q", occurrence.ID)
		}
		occurrences[occurrence.ID] = occurrence
	}
	units, warnings, err := parse.NormalizeMarkdown(parsed.Units, occurrences, true)
	if err != nil {
		return fmt.Errorf("normalize parsed Markdown: %w", err)
	}
	parsed.Units = units
	parsed.Warnings = append(parsed.Warnings, warnings...)
	if err := parsed.Validate(); err != nil {
		return fmt.Errorf("validate normalized parsed document: %w", err)
	}
	return nil
}

func (s *Service) splitAndEnrich(
	ctx context.Context,
	fence store.IndexFence,
	version *store.RAGDocumentVersionRecord,
	kb *store.RAGKBRecord,
	doc *store.RAGDocumentRecord,
	chunks []split.Chunk,
	finalizeConfig enrich.FinalizeConfig,
	budget *vision.TaskDocumentAIBudget,
) ([]split.Chunk, []enrich.Warning, error) {
	if !version.EnrichmentEnabled {
		return chunks, nil, nil
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "enriching", Current: 0, Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return nil, nil, err
	}
	if s.enricher == nil || !s.cfg.Features.TextEnrichmentEnabled || strings.TrimSpace(version.TextModel) == "" {
		return chunks, []enrich.Warning{{ChunkIndex: -1, Code: "enrichment_unavailable",
			Message: "text enrichment was unavailable; source text retained"}}, nil
	}
	processor := enrich.NewProcessor(s.enricher)
	processor.SetRecorder(s.telemetry)
	enriched, warnings := processor.EnrichChunks(ctx, chunks, enrich.ProcessConfig{
		SystemEnabled: true, TextModel: version.TextModel, KBEnabled: true,
		MaxBlocks: s.cfg.Limits.MaxEnrichmentBlocksPerDocument, Finalize: finalizeConfig,
		Scope: enrich.CacheScope{
			UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID,
			IndexFingerprint: version.IndexFingerprint,
		},
	}, budget)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return enriched, warnings, nil
}

func (s *Service) embedChunks(
	ctx context.Context,
	fence store.IndexFence,
	kbID string,
	version *store.RAGDocumentVersionRecord,
	binding config.RAGEmbeddingCfg,
	chunks []split.Chunk,
) ([][]float32, int, error) {
	texts := make([]string, len(chunks))
	totalTokens := 0
	for i := range chunks {
		texts[i] = chunks[i].SearchContent
		if strings.TrimSpace(texts[i]) == "" {
			return nil, 0, fmt.Errorf("empty SearchContent at chunk %d", chunks[i].Index)
		}
		if !s.cfg.Limits.SearchContentWithinLimit(texts[i]) ||
			split.EstimateTokens(texts[i]) > version.ChunkSize {
			return nil, 0, fmt.Errorf("SearchContent exceeds final boundary at chunk %d", chunks[i].Index)
		}
		totalTokens += chunks[i].Tokens
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "embedding", Current: 0, Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return nil, 0, err
	}
	if err := s.vec.EnsureCollection(ctx, kbID, version.EmbeddingDimensions); err != nil {
		return nil, 0, fmt.Errorf("准备向量 collection: %w", err)
	}
	embedder := embed.New(binding.Endpoint, binding.APIKey,
		version.EmbeddingModel, version.EmbeddingDimensions)
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return nil, 0, err
	}
	if len(vectors) != len(chunks) {
		return nil, 0, fmt.Errorf("embedding vector count %d does not match chunk count %d", len(vectors), len(chunks))
	}
	return vectors, totalTokens, nil
}

func (s *Service) stageIndexVersion(
	ctx context.Context,
	fence store.IndexFence,
	kbID, docID string,
	chunks []split.Chunk,
	vectors [][]float32,
) ([]vector.ChunkData, error) {
	if len(chunks) != len(vectors) {
		return nil, errors.New("cannot stage mismatched chunks and vectors")
	}
	now := time.Now().UTC()
	vectorChunks := make([]vector.ChunkData, len(chunks))
	sqlChunks := make([]store.RAGChunkRecord, len(chunks))
	mappings := make([]store.RAGChunkAssetRecord, 0)
	for i, chunk := range chunks {
		location := chunk.Location
		if location.Kind == "" {
			location = document.SourceLocation{Kind: document.LocationDocument}
		}
		locationJSON, err := json.Marshal(location)
		if err != nil {
			return nil, fmt.Errorf("encode chunk %d location: %w", chunk.Index, err)
		}
		pageNum := 0
		if location.Kind == document.LocationPage {
			pageNum = location.Index
		}
		vectorChunks[i] = vector.ChunkData{
			DocID: docID, Index: chunk.Index, Content: chunk.RawContent,
			SearchContent: chunk.SearchContent, SectionTitle: chunk.SectionTitle,
			PageNum: pageNum, DocVersion: fence.DocVersion, Vector: vectors[i],
		}
		sqlChunks[i] = store.RAGChunkRecord{
			KBID: kbID, DocID: docID, DocVersion: fence.DocVersion, ChunkIndex: chunk.Index,
			SectionTitle: chunk.SectionTitle, LocationJSON: string(locationJSON), RawContent: chunk.RawContent,
			Enhancement: chunk.Enhancement, SearchContent: chunk.SearchContent,
			TokenCount: chunk.Tokens, CreatedAt: now,
		}
		for ordinal, binding := range chunk.AssetBindings {
			assetLocationJSON, err := json.Marshal(binding.Asset.Location)
			if err != nil {
				return nil, fmt.Errorf("encode chunk %d asset location: %w", chunk.Index, err)
			}
			mappings = append(mappings, store.RAGChunkAssetRecord{
				DocID: docID, DocVersion: fence.DocVersion, ChunkIndex: chunk.Index,
				AssetID: binding.Asset.ID, Ordinal: ordinal, LocationJSON: string(assetLocationJSON),
				Caption: binding.Asset.Caption, OCRText: binding.OCRText,
			})
		}
	}
	for start := 0; start < len(sqlChunks); start += pipelineStageBatchSize {
		valid, err := s.st.CheckRAGIndexFence(ctx, fence)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, errIndexFenceLost
		}
		end := min(start+pipelineStageBatchSize, len(sqlChunks))
		if err := s.st.PutRAGChunks(ctx, sqlChunks[start:end]); err != nil {
			return nil, fmt.Errorf("stage chunk catalog: %w", err)
		}
	}
	for start := 0; start < len(mappings); start += pipelineStageBatchSize {
		valid, err := s.st.CheckRAGIndexFence(ctx, fence)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, errIndexFenceLost
		}
		end := min(start+pipelineStageBatchSize, len(mappings))
		if err := s.st.PutRAGChunkAssets(ctx, mappings[start:end]); err != nil {
			return nil, fmt.Errorf("stage chunk assets: %w", err)
		}
	}
	return vectorChunks, nil
}

func (s *Service) upsertIndexVersion(
	ctx context.Context,
	fence store.IndexFence,
	kbID string,
	chunks []vector.ChunkData,
) error {
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "indexing", Current: 0, Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return err
	}
	for start := 0; start < len(chunks); start += pipelineStageBatchSize {
		valid, err := s.st.CheckRAGIndexFence(ctx, fence)
		if err != nil {
			return err
		}
		if !valid {
			return errIndexFenceLost
		}
		end := min(start+pipelineStageBatchSize, len(chunks))
		if err := s.vec.UpsertChunks(ctx, kbID, chunks[start:end]); err != nil {
			return fmt.Errorf("写入向量库: %w", err)
		}
	}
	return nil
}

func parsedArtifactPageCount(artifact *document.ParsedArtifact) int {
	if artifact == nil {
		return 0
	}
	pageCount := 0
	for _, unit := range artifact.Units {
		if unit.Location.Kind == document.LocationDocument && pageCount == 0 {
			pageCount = 1
		}
		if unit.Location.Kind != document.LocationDocument && unit.Location.Index > pageCount {
			pageCount = unit.Location.Index
		}
	}
	for _, warning := range artifact.Warnings {
		if warning.Location != nil && warning.Location.Kind != document.LocationDocument && warning.Location.Index > pageCount {
			pageCount = warning.Location.Index
		}
	}
	return pageCount
}

func (s *Service) fencedWarnings(ctx context.Context, fence store.IndexFence, degraded bool, warningCount int) error {
	ok, err := s.st.UpdateWarningRAGIndexTask(ctx, fence, degraded, warningCount)
	if err != nil {
		return err
	}
	if !ok {
		return errIndexFenceLost
	}
	return nil
}

func (s *Service) fencedProgress(ctx context.Context, fence store.IndexFence, progress store.RAGIndexProgress) error {
	ok, err := s.st.UpdateProgressRAGIndexTask(ctx, fence, progress)
	if err != nil {
		return err
	}
	if !ok {
		return errIndexFenceLost
	}
	return nil
}
