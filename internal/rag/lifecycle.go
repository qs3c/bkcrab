package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/store"
)

const lifecycleBatchSize = 100

var ErrLifecycleCleanupPending = errors.New("RAG cleanup incomplete; retry deletion")

var errRAGWorkerQuiescing = errors.New("RAG worker lease is still quiescing")

var errRAGKBProvisioningQuiescing = errors.New("RAG knowledge-base provisioning lease is still quiescing")

var errRAGMaintenanceFenceLost = errors.New("RAG document maintenance fence lost")

type lifecycleCleanupError struct{ cause error }

func (e *lifecycleCleanupError) Error() string { return ErrLifecycleCleanupPending.Error() }
func (e *lifecycleCleanupError) Unwrap() error { return e.cause }
func (e *lifecycleCleanupError) Is(target error) bool {
	return target == ErrLifecycleCleanupPending || errors.Is(e.cause, target)
}

func cleanupPending(err error) error {
	if err == nil {
		return nil
	}
	return &lifecycleCleanupError{cause: err}
}

// CleanupRAGUser is the narrow boundary injected into internal/users. The
// account service owns the user tombstone and final SQL deletion; RAG owns
// every KB, vector collection, object prefix, and catalog below that user.
func (s *Service) CleanupRAGUser(ctx context.Context, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("RAG user cleanup requires user id")
	}
	kbs, err := s.st.ListRAGKBsByUser(ctx, userID)
	if err != nil {
		return err
	}
	for i := range kbs {
		if err := s.deleteKBRecord(ctx, &kbs[i]); err != nil {
			return fmt.Errorf("cleanup RAG knowledge base %s: %w", kbs[i].ID, err)
		}
	}
	return nil
}

// deleteKBRecord writes the durable tombstone before waiting for local work.
// MarkRAGKBDeleting atomically revokes every child document/task as well, so a
// slow vector/object delete never leaves searchable or downloadable data.
func (s *Service) deleteKBRecord(ctx context.Context, kb *store.RAGKBRecord) error {
	if kb == nil || strings.TrimSpace(kb.ID) == "" {
		return ErrNotFound
	}
	marked, err := s.st.MarkRAGKBDeleting(ctx, kb.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}

	kbLock := s.kbMutex(marked.ID)
	kbLock.Lock()
	defer kbLock.Unlock()
	return s.cleanupDeletingKB(ctx, marked)
}

func (s *Service) cleanupDeletingKB(ctx context.Context, kb *store.RAGKBRecord) error {
	if kb == nil {
		return nil
	}
	ready, err := s.st.IsRAGKBCleanupReady(ctx, kb.ID)
	if err != nil {
		return cleanupPending(err)
	}
	if !ready {
		return cleanupPending(errRAGKBProvisioningQuiescing)
	}
	docs, err := s.st.ListRAGDocumentsByKB(ctx, kb.ID)
	if err != nil {
		return cleanupPending(err)
	}
	for i := range docs {
		ready, err := s.st.IsRAGDocumentCleanupReady(ctx, docs[i].ID)
		if err != nil {
			return cleanupPending(err)
		}
		if !ready {
			return cleanupPending(errRAGWorkerQuiescing)
		}
	}
	if err := s.vec.DropCollection(ctx, kb.ID); err != nil {
		return cleanupPending(err)
	}
	if err := s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/", kb.UserID, kb.ID)); err != nil {
		return cleanupPending(err)
	}
	if err := s.st.DeleteRAGKB(ctx, kb.ID); errors.Is(err, store.ErrNotFound) {
		return nil
	} else {
		return cleanupPending(err)
	}
}

func (s *Service) cleanupDeletingDocument(ctx context.Context, doc *store.RAGDocumentRecord) error {
	if doc == nil {
		return nil
	}
	ready, err := s.st.IsRAGDocumentCleanupReady(ctx, doc.ID)
	if err != nil {
		return cleanupPending(err)
	}
	if !ready {
		return cleanupPending(errRAGWorkerQuiescing)
	}
	if err := s.vec.DeleteDoc(ctx, doc.KBID, doc.ID); err != nil {
		return cleanupPending(err)
	}
	documentPrefix := path.Dir(strings.ReplaceAll(doc.ObjectKey, "\\", "/")) + "/"
	if err := s.obj.DeletePrefix(ctx, documentPrefix); err != nil {
		return cleanupPending(err)
	}
	if err := s.st.DeleteRAGDocument(ctx, doc.ID); errors.Is(err, store.ErrNotFound) {
		return nil
	} else {
		return cleanupPending(err)
	}
}

func (s *Service) lifecycleLoop(ctx context.Context) {
	interval := s.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	s.runLifecyclePass(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runLifecyclePass(ctx)
		}
	}
}

func (s *Service) runLifecyclePass(ctx context.Context) {
	if ctx.Err() != nil || s.st == nil || s.vec == nil || s.obj == nil {
		return
	}
	s.runAvailableGCTasks(ctx)
	s.expireStaleKBProvisions(ctx)
	s.retryDeletingResources(ctx)
	s.sweepObjectWriteStaging(ctx)
	s.sweepOrphanVersions(ctx)
	s.sweepStagingAssets(ctx)
	s.sweepStagingAttachments(ctx)
	s.sweepCacheObjects(ctx)
}

func (s *Service) sweepObjectWriteStaging(ctx context.Context) {
	ttl := s.stagingArtifactTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	candidates, err := s.st.ListRAGObjectWriteCleanupCandidates(ctx, ttl, lifecycleBatchSize)
	if err != nil {
		logLifecycleFailure("list_object_write_staging", "", err)
		return
	}
	for i := range candidates {
		if ctx.Err() != nil {
			return
		}
		candidate := candidates[i]
		fence, claimed, err := s.st.ClaimRAGObjectWriteCleanup(ctx, candidate)
		if err != nil {
			logLifecycleFailure("claim_object_write_cleanup", candidate.HandleID, err)
			continue
		}
		if !claimed || fence == nil {
			continue
		}
		if err := s.obj.Delete(ctx, fence.ObjectKey); err != nil {
			logLifecycleFailure("delete_staged_object", fence.HandleID, err)
			continue
		}
		finished, err := s.st.FinishRAGObjectWriteCleanup(ctx, *fence)
		if err != nil {
			logLifecycleFailure("finish_object_write_cleanup", fence.HandleID, err)
			continue
		}
		if !finished {
			logLifecycleFailure("finish_object_write_cleanup", fence.HandleID,
				errors.New("stale object-write cleanup fence"))
		}
	}
}

func (s *Service) expireStaleKBProvisions(ctx context.Context) {
	candidates, err := s.st.ListExpiredRAGKBProvisions(ctx, lifecycleBatchSize)
	if err != nil {
		logLifecycleFailure("list_expired_kb_provisions", "", err)
		return
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		kbLock := s.kbMutex(candidate.KBID)
		kbLock.Lock()
		_, _, err := s.st.ExpireRAGKBProvisioning(ctx, candidate)
		kbLock.Unlock()
		if err != nil {
			logLifecycleFailure("expire_kb_provision", candidate.KBID, err)
		}
	}
}

func (s *Service) runAvailableGCTasks(ctx context.Context) {
	for ctx.Err() == nil {
		claim, err := s.st.ClaimRAGIndexGCTask(ctx, s.workerID+"-gc", s.leaseDuration)
		if err != nil {
			telemetry.Emit(ctx, s.telemetry, telemetry.EventLifecycleGC, telemetry.Fields{
				Transition: "claim", Outcome: "error", ErrorCode: "store_error",
			})
			logLifecycleFailure("claim_index_gc", "", err)
			return
		}
		if claim == nil {
			return
		}
		s.runGCClaim(ctx, claim)
	}
}

func (s *Service) runGCClaim(parent context.Context, claim *store.RAGIndexGCClaim) {
	if claim == nil {
		return
	}
	maintenance, err := s.claimDocumentMaintenance(parent, claim.Fence.DocID, "gc-maint")
	if err != nil {
		logLifecycleFailure("claim_gc_document_maintenance", claim.Fence.DocID, err)
		s.retryGCClaim(parent, claim, "maintenance_error")
		return
	}
	if maintenance == nil {
		s.retryGCClaim(parent, claim, "maintenance_busy")
		return
	}
	defer s.releaseDocumentMaintenance(parent, *maintenance)
	started := time.Now()
	telemetry.Emit(parent, s.telemetry, telemetry.EventLifecycleGC, gcTelemetryFields(
		claim, "claim", "ok", "",
	))
	workCtx, cancelWork := context.WithCancel(parent)
	defer cancelWork()
	heartbeatCtx, stopHeartbeat := context.WithCancel(parent)
	var leaseLost atomic.Bool
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
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
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				ok, err := s.st.HeartbeatRAGIndexGCTask(heartbeatCtx, claim.Fence, s.leaseDuration)
				if err == nil && ok {
					ok, err = s.st.HeartbeatRAGDocumentMaintenance(
						heartbeatCtx, *maintenance, s.leaseDuration,
					)
				}
				if err != nil || !ok {
					errorCode := "fence_lost"
					if err != nil {
						errorCode = "store_error"
					}
					telemetry.Emit(heartbeatCtx, s.telemetry, telemetry.EventLifecycleGC, gcTelemetryFields(
						claim, "heartbeat", "error", errorCode,
					))
					leaseLost.Store(true)
					cancelWork()
					if err != nil && heartbeatCtx.Err() == nil {
						logLifecycleFailure("heartbeat_index_gc", claim.Fence.DocID, err)
					}
					return
				}
				telemetry.Emit(heartbeatCtx, s.telemetry, telemetry.EventLifecycleGC, gcTelemetryFields(
					claim, "heartbeat", "ok", "",
				))
			}
		}
	}()
	stopAndWait := func() {
		stopHeartbeat()
		<-heartbeatDone
	}

	valid, err := s.st.CheckRAGIndexGCFence(workCtx, claim.Fence)
	if err == nil && valid {
		valid, err = s.st.CheckRAGDocumentMaintenance(workCtx, *maintenance)
	}
	if err == nil && valid {
		err = s.vec.DeleteDocVersion(workCtx, claim.KBID, claim.Fence.DocID, claim.Fence.RetiredVersion)
	}
	stopAndWait()
	if leaseLost.Load() || parent.Err() != nil {
		return
	}
	if err != nil {
		logLifecycleFailure("delete_index_gc_vector", claim.Fence.DocID, err)
		s.retryGCClaim(parent, claim, "vector_error")
		return
	}
	if !valid {
		telemetry.Emit(parent, s.telemetry, telemetry.EventLifecycleGC, gcTelemetryFields(
			claim, "finish", "rejected", "fence_lost",
		))
		return
	}
	if err := s.checkDocumentMaintenance(parent, *maintenance); err != nil {
		telemetry.Emit(parent, s.telemetry, telemetry.EventLifecycleGC, gcTelemetryFields(
			claim, "finish", "rejected", "maintenance_fence_lost",
		))
		return
	}
	ok, err := s.st.FinishRAGIndexGCTask(parent, claim.Fence)
	if err != nil {
		logLifecycleFailure("finish_index_gc", claim.Fence.DocID, err)
		s.retryGCClaim(parent, claim, "store_error")
		return
	}
	if !ok {
		telemetry.Emit(parent, s.telemetry, telemetry.EventLifecycleGC, gcTelemetryFields(
			claim, "finish", "rejected", "fence_lost",
		))
		slog.Info("rag: index GC fence was superseded",
			"doc_id", claim.Fence.DocID, "doc_version", claim.Fence.RetiredVersion)
		return
	}
	fields := gcTelemetryFields(claim, "finish", "ok", "")
	fields.Duration = time.Since(started)
	telemetry.Emit(parent, s.telemetry, telemetry.EventLifecycleGC, fields)
}

func (s *Service) retryGCClaim(parent context.Context, claim *store.RAGIndexGCClaim, reason string) {
	ok, err := s.st.RetryRAGIndexGCTask(
		parent, claim.Fence, lifecycleRetryDelay(claim.Task.AttemptCount+1),
	)
	fields := gcTelemetryFields(claim, "retry", "scheduled", reason)
	fields.RetryCount = claim.Task.AttemptCount + 1
	if err != nil {
		fields.Outcome = "error"
		fields.ErrorCode = "store_error"
	} else if !ok {
		fields.Outcome = "rejected"
		fields.ErrorCode = "fence_lost"
	}
	telemetry.Emit(parent, s.telemetry, telemetry.EventLifecycleGC, fields)
}

func gcTelemetryFields(
	claim *store.RAGIndexGCClaim,
	transition, outcome, errorCode string,
) telemetry.Fields {
	if claim == nil {
		return telemetry.Fields{Transition: transition, Outcome: outcome, ErrorCode: errorCode}
	}
	return telemetry.Fields{
		DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID,
		DocVersion: claim.Fence.RetiredVersion, RetiredVersion: claim.Fence.RetiredVersion,
		ClaimGeneration: claim.Fence.ClaimGeneration, RetryCount: claim.Task.AttemptCount,
		Transition: transition, Outcome: outcome, ErrorCode: errorCode,
	}
}

func lifecycleRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}

func (s *Service) claimDocumentMaintenance(
	ctx context.Context,
	docID, operation string,
) (*store.RAGDocumentMaintenanceFence, error) {
	duration := s.leaseDuration
	if duration <= 0 {
		duration = time.Minute
	}
	return s.st.ClaimRAGDocumentMaintenance(ctx, docID, s.workerID+"-"+operation, duration)
}

func (s *Service) checkDocumentMaintenance(
	ctx context.Context,
	fence store.RAGDocumentMaintenanceFence,
) error {
	valid, err := s.st.CheckRAGDocumentMaintenance(ctx, fence)
	if err != nil {
		return err
	}
	if !valid {
		return errRAGMaintenanceFenceLost
	}
	return nil
}

func (s *Service) releaseDocumentMaintenance(
	ctx context.Context,
	fence store.RAGDocumentMaintenanceFence,
) {
	if _, err := s.st.ReleaseRAGDocumentMaintenance(ctx, fence); err != nil && ctx.Err() == nil {
		logLifecycleFailure("release_document_maintenance", fence.DocID, err)
	}
}

func (s *Service) startDocumentMaintenanceHeartbeat(
	parent context.Context,
	fence store.RAGDocumentMaintenanceFence,
) (context.Context, func()) {
	workCtx, cancelWork := context.WithCancel(parent)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		duration := s.leaseDuration
		if duration <= 0 {
			duration = time.Minute
		}
		interval := s.heartbeatInterval
		if interval <= 0 || interval >= duration {
			interval = duration / 3
		}
		if interval <= 0 {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				ok, err := s.st.HeartbeatRAGDocumentMaintenance(
					heartbeatCtx, fence, duration,
				)
				if err != nil || !ok {
					cancelWork()
					if err != nil && heartbeatCtx.Err() == nil {
						logLifecycleFailure("heartbeat_document_maintenance", fence.DocID, err)
					}
					return
				}
			}
		}
	}()
	return workCtx, func() {
		cancelHeartbeat()
		<-done
		cancelWork()
	}
}

func (s *Service) sweepOrphanVersions(ctx context.Context) {
	ttl := s.stagingArtifactTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	candidates, err := s.st.ListRAGVersionCleanupCandidates(ctx, ttl, lifecycleBatchSize)
	if err != nil {
		logLifecycleFailure("list_orphan_versions", "", err)
		return
	}
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return
		}
		docLock := s.docMutex(candidate.DocID)
		docLock.Lock()
		failureOperation, err := func() (string, error) {
			maintenance, err := s.claimDocumentMaintenance(ctx, candidate.DocID, "orphan")
			if err != nil {
				return "claim_document_maintenance", err
			}
			if maintenance == nil {
				return "", nil
			}
			defer s.releaseDocumentMaintenance(ctx, *maintenance)
			workCtx, stopHeartbeat := s.startDocumentMaintenanceHeartbeat(ctx, *maintenance)
			defer stopHeartbeat()
			if err := s.checkDocumentMaintenance(workCtx, *maintenance); err != nil {
				return "check_document_maintenance", err
			}
			if err := s.vec.DeleteDocVersion(workCtx, candidate.KBID, candidate.DocID, candidate.DocVersion); err != nil {
				return "sweep_orphan_version", err
			}
			if err := s.checkDocumentMaintenance(workCtx, *maintenance); err != nil {
				return "recheck_orphan_vector_maintenance", err
			}
			if err := s.st.DeleteRAGChunkAssetsByDocumentVersion(workCtx, candidate.DocID, candidate.DocVersion); err != nil {
				return "delete_orphan_chunk_assets", err
			}
			if err := s.st.DeleteRAGChunksByDocumentVersion(workCtx, candidate.DocID, candidate.DocVersion); err != nil {
				return "delete_orphan_chunks", err
			}
			if err := s.checkDocumentMaintenance(workCtx, *maintenance); err != nil {
				return "recheck_document_maintenance", err
			}
			if _, err := s.st.MarkRAGDocumentVersionGCED(workCtx, candidate.DocID, candidate.DocVersion); err != nil {
				return "mark_orphan_version_gced", err
			}
			if candidate.ParseArtifactKey != "" {
				referenced, err := s.st.IsRAGParseArtifactReferenced(
					workCtx, candidate.DocID, candidate.ParseArtifactKey,
				)
				if err != nil {
					return "check_parse_artifact_reference", err
				}
				if !referenced {
					if err := s.checkDocumentMaintenance(workCtx, *maintenance); err != nil {
						return "check_parse_artifact_maintenance", err
					}
					if err := s.obj.DeletePrefix(workCtx, path.Dir(candidate.ParseArtifactKey)+"/"); err != nil {
						return "delete_orphan_parse_artifact", err
					}
				}
			}
			return "", nil
		}()
		docLock.Unlock()
		if err != nil {
			logLifecycleFailure(failureOperation, candidate.DocID, err)
		}
	}
}

func (s *Service) sweepStagingAssets(ctx context.Context) {
	ttl := s.stagingArtifactTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	assets, err := s.st.ListRAGStagingAssetCleanupCandidates(ctx, ttl, lifecycleBatchSize)
	if err != nil {
		logLifecycleFailure("list_staging_assets", "", err)
		return
	}
	for i := range assets {
		asset := &assets[i]
		operation, err := func() (string, error) {
			docLock := s.docMutex(asset.DocID)
			docLock.Lock()
			defer docLock.Unlock()
			maintenance, err := s.claimDocumentMaintenance(ctx, asset.DocID, "asset")
			if err != nil {
				return "claim_staging_asset_maintenance", err
			}
			if maintenance == nil {
				return "", nil
			}
			defer s.releaseDocumentMaintenance(ctx, *maintenance)
			workCtx, stopHeartbeat := s.startDocumentMaintenanceHeartbeat(ctx, *maintenance)
			defer stopHeartbeat()

			claim, claimed, err := s.st.ClaimRAGStagingAssetCleanup(workCtx, *maintenance, asset.ID)
			if err != nil {
				return "claim_staging_asset_cleanup", err
			}
			if !claimed || claim == nil {
				return "", nil
			}
			for _, objectWrite := range claim.ObjectWrites {
				if err := s.obj.Delete(workCtx, objectWrite.ObjectKey); err != nil {
					return "delete_staging_asset_object", err
				}
				if _, err := s.st.FinishRAGObjectWriteCleanup(workCtx, objectWrite); err != nil {
					return "finish_staging_asset_object", err
				}
			}
			return "", nil
		}()
		if err != nil {
			logLifecycleFailure(operation, asset.ID, err)
		}
	}
}

func (s *Service) sweepStagingAttachments(ctx context.Context) {
	ttl := s.stagingArtifactTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	attachments, err := s.st.ListRAGStagingAttachmentCleanupCandidates(
		ctx, ttl, lifecycleBatchSize)
	if err != nil {
		logLifecycleFailure("list_staging_attachments", "", err)
		return
	}
	for i := range attachments {
		attachment := &attachments[i]
		operation, err := func() (string, error) {
			docLock := s.docMutex(attachment.DocID)
			docLock.Lock()
			defer docLock.Unlock()
			maintenance, err := s.claimDocumentMaintenance(
				ctx, attachment.DocID, "attachment")
			if err != nil {
				return "claim_staging_attachment_maintenance", err
			}
			if maintenance == nil {
				return "", nil
			}
			defer s.releaseDocumentMaintenance(ctx, *maintenance)
			workCtx, stopHeartbeat := s.startDocumentMaintenanceHeartbeat(ctx, *maintenance)
			defer stopHeartbeat()
			claim, claimed, err := s.st.ClaimRAGStagingAttachmentCleanup(
				workCtx, *maintenance, attachment.ID)
			if err != nil {
				return "claim_staging_attachment_cleanup", err
			}
			if !claimed || claim == nil {
				return "", nil
			}
			for _, objectWrite := range claim.ObjectWrites {
				if err := s.obj.Delete(workCtx, objectWrite.ObjectKey); err != nil {
					return "delete_staging_attachment_object", err
				}
				if _, err := s.st.FinishRAGObjectWriteCleanup(
					workCtx, objectWrite); err != nil {
					return "finish_staging_attachment_object", err
				}
			}
			return "", nil
		}()
		if err != nil {
			logLifecycleFailure(operation, attachment.ID, err)
		}
	}
}

func (s *Service) sweepCacheObjects(ctx context.Context) {
	ttl := s.stagingArtifactTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	s.cacheSweepCursorMu.Lock()
	docIDs, err := s.st.ListRAGCacheCatalogDocuments(
		ctx, s.cacheSweepCursor, lifecycleBatchSize,
	)
	if err == nil && len(docIDs) == 0 && s.cacheSweepCursor != "" {
		s.cacheSweepCursor = ""
		docIDs, err = s.st.ListRAGCacheCatalogDocuments(ctx, "", lifecycleBatchSize)
	}
	if err == nil && len(docIDs) > 0 {
		s.cacheSweepCursor = docIDs[len(docIDs)-1]
	}
	s.cacheSweepCursorMu.Unlock()
	if err != nil {
		logLifecycleFailure("list_cache_catalog_documents", "", err)
		return
	}
	for _, docID := range docIDs {
		if ctx.Err() != nil {
			return
		}
		docLock := s.docMutex(docID)
		docLock.Lock()
		operation, sweepErr := func() (string, error) {
			maintenance, err := s.claimDocumentMaintenance(ctx, docID, "cache")
			if err != nil {
				return "claim_cache_maintenance", err
			}
			if maintenance == nil {
				return "", nil
			}
			defer s.releaseDocumentMaintenance(ctx, *maintenance)
			workCtx, stopHeartbeat := s.startDocumentMaintenanceHeartbeat(ctx, *maintenance)
			defer stopHeartbeat()
			candidates, err := s.st.PruneRAGCacheCatalogAndListCleanupCandidates(
				workCtx, *maintenance, ttl,
				s.maxCacheFingerprintsPerDocument, lifecycleBatchSize,
			)
			if err != nil {
				return "list_cache_cleanup_candidates", err
			}
			for _, candidate := range candidates {
				if err := s.checkDocumentMaintenance(workCtx, *maintenance); err != nil {
					return "check_cache_maintenance", err
				}
				if err := s.obj.Delete(workCtx, candidate.ObjectKey); err != nil {
					return "delete_cache_object", err
				}
				if err := s.checkDocumentMaintenance(workCtx, *maintenance); err != nil {
					return "recheck_cache_maintenance", err
				}
				if _, err := s.st.DeleteRAGCacheObjectWithMaintenance(
					workCtx, *maintenance, candidate,
				); err != nil {
					return "delete_cache_catalog", err
				}
			}
			return "", nil
		}()
		docLock.Unlock()
		if sweepErr != nil {
			logLifecycleFailure(operation, docID, sweepErr)
		}
	}
}

func (s *Service) retryDeletingResources(ctx context.Context) {
	s.cacheSweepCursorMu.Lock()
	docs, err := s.st.ListDeletingRAGDocuments(
		ctx, s.deletingDocumentSweepCursor, lifecycleBatchSize,
	)
	if err == nil && len(docs) == 0 && s.deletingDocumentSweepCursor != "" {
		s.deletingDocumentSweepCursor = ""
		docs, err = s.st.ListDeletingRAGDocuments(ctx, "", lifecycleBatchSize)
	}
	if err == nil && len(docs) > 0 {
		s.deletingDocumentSweepCursor = docs[len(docs)-1].ID
	}
	s.cacheSweepCursorMu.Unlock()
	if err != nil {
		logLifecycleFailure("list_deleting_documents", "", err)
		return
	}
	for i := range docs {
		doc := &docs[i]
		kbLock := s.kbMutex(doc.KBID)
		kbLock.RLock()
		docLock := s.docMutex(doc.ID)
		docLock.Lock()
		err := s.cleanupDeletingDocument(ctx, doc)
		docLock.Unlock()
		kbLock.RUnlock()
		if err != nil {
			logLifecycleFailure("retry_document_delete", doc.ID, err)
		}
		if ctx.Err() != nil {
			return
		}
	}

	s.cacheSweepCursorMu.Lock()
	kbs, err := s.st.ListDeletingRAGKBs(ctx, s.deletingKBSweepCursor, lifecycleBatchSize)
	if err == nil && len(kbs) == 0 && s.deletingKBSweepCursor != "" {
		s.deletingKBSweepCursor = ""
		kbs, err = s.st.ListDeletingRAGKBs(ctx, "", lifecycleBatchSize)
	}
	if err == nil && len(kbs) > 0 {
		s.deletingKBSweepCursor = kbs[len(kbs)-1].ID
	}
	s.cacheSweepCursorMu.Unlock()
	if err != nil {
		logLifecycleFailure("list_deleting_kbs", "", err)
		return
	}
	for i := range kbs {
		kb := &kbs[i]
		kbLock := s.kbMutex(kb.ID)
		kbLock.Lock()
		err := s.cleanupDeletingKB(ctx, kb)
		kbLock.Unlock()
		if err != nil {
			logLifecycleFailure("retry_kb_delete", kb.ID, err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// Avoid rendering storage/provider errors in lifecycle logs: those errors may
// contain object keys or endpoint credentials. The operation and stable SQL ID
// are sufficient to correlate with provider-side telemetry.
func logLifecycleFailure(operation, id string, err error) {
	if err == nil {
		return
	}
	slog.Warn("rag: lifecycle operation will retry",
		"operation", operation, "resource_id", id, "error_type", fmt.Sprintf("%T", err))
}
