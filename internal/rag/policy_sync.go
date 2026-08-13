package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

var ErrPolicySyncActive = errors.New("知识库正在执行整库策略同步")

const policySyncRollbackWindow = 24 * time.Hour

func (s *Service) ensureNoPolicySync(ctx context.Context, kbID string) error {
	active, err := s.st.IsRAGKBPolicySyncActive(ctx, kbID)
	if err != nil {
		return err
	}
	if active {
		return ErrPolicySyncActive
	}
	return nil
}

func (s *Service) StartKBPolicySync(ctx context.Context, ownerID, kbID string, targetPolicyVersion int64) (*store.RAGPolicySyncTaskRecord, error) {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
	}
	if err = s.ensureNoPolicySync(ctx, kbID); err != nil {
		return nil, err
	}
	if targetPolicyVersion <= 0 {
		active, activeErr := s.st.ActiveRAGPolicy(ctx, store.RAGPolicyIngestion)
		if activeErr != nil {
			return nil, activeErr
		}
		targetPolicyVersion = active.Version
	}
	if kb.PinnedPolicyVersion.Valid && kb.PinnedPolicyVersion.Int64 == targetPolicyVersion {
		return nil, errors.New("knowledge base already uses target ingestion policy")
	}
	policyRecord, err := s.st.GetRAGPolicy(ctx, store.RAGPolicyIngestion, targetPolicyVersion)
	if err != nil {
		return nil, err
	}
	policy, err := config.DecodeRAGIngestionPolicy([]byte(policyRecord.PolicyJSON))
	if err != nil {
		return nil, err
	}
	requestedBy := ownerID
	if requestedBy == "" {
		requestedBy = kb.UserID
	}
	documents, err := s.st.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		return nil, err
	}
	generationID := "rkg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	collectionKey, err := vector.GenerationCollectionKey(kbID, generationID)
	if err != nil {
		return nil, err
	}
	mapped := make([]store.RAGGenerationDocumentRecord, 0, len(documents))
	var sourceBytes int64
	for _, doc := range documents {
		if doc.ActiveVersion <= 0 || strings.EqualFold(doc.Status, store.RAGDocumentStatusDeleting) {
			continue
		}
		versions, listErr := s.st.ListRAGDocumentVersions(ctx, doc.ID)
		if listErr != nil {
			return nil, listErr
		}
		next := doc.Version + 1
		for _, version := range versions {
			if version.DocVersion >= next {
				next = version.DocVersion + 1
			}
		}
		mapped = append(mapped, store.RAGGenerationDocumentRecord{DocID: doc.ID, DocVersion: next})
		sourceBytes += doc.FileSize
	}
	generation := &store.RAGKBGenerationRecord{ID: generationID, KBID: kbID, PolicyVersion: targetPolicyVersion, CollectionKey: string(collectionKey), EmbeddingModel: policy.Embedding.Model, EmbeddingDims: policy.Embedding.Dims, CreatedBy: requestedBy}
	if err = s.st.CreateRAGKBGeneration(ctx, generation, mapped); err != nil {
		return nil, err
	}
	estimate, _ := json.Marshal(IngestionPolicyEstimate{DocumentCount: len(mapped), SourceBytes: sourceBytes, TemporaryBytesMax: sourceBytes * 2})
	task := &store.RAGPolicySyncTaskRecord{KBID: kbID, SourceGenerationID: kb.ActiveGenerationID.String, TargetGenerationID: generationID, TargetPolicyVersion: targetPolicyVersion, EstimateJSON: string(estimate), RequestedBy: requestedBy}
	if err = s.st.CreateRAGPolicySyncTask(ctx, task); err != nil {
		_ = s.vec.DropCollection(context.WithoutCancel(ctx), collectionKey)
		_, _ = s.st.AbandonUnreferencedRAGKBGeneration(context.WithoutCancel(ctx), generationID)
		return nil, err
	}
	return task, nil
}

func (s *Service) GetKBPolicySyncTask(ctx context.Context, ownerID, kbID, taskID string) (*store.RAGPolicySyncTaskRecord, error) {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return nil, err
	}
	task, err := s.st.GetRAGPolicySyncTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if task.KBID != kbID {
		return nil, ErrNotFound
	}
	return task, nil
}

func (s *Service) CancelKBPolicySync(ctx context.Context, ownerID, kbID, taskID string) error {
	task, err := s.GetKBPolicySyncTask(ctx, ownerID, kbID, taskID)
	if err != nil {
		return err
	}
	ok, err := s.st.RequestCancelRAGPolicySyncTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("policy sync is already terminal")
	}
	if current, getErr := s.st.GetRAGPolicySyncTask(ctx, taskID); getErr == nil && current.Status == store.RAGPolicySyncCancelled {
		generation, generationErr := s.st.GetRAGKBGeneration(ctx, task.TargetGenerationID)
		if generationErr == nil {
			if key, keyErr := vector.CollectionKeyFromPersistence(generation.CollectionKey); keyErr == nil {
				_ = s.vec.DropCollection(context.WithoutCancel(ctx), key)
			}
			_, _ = s.st.MarkRAGKBGenerationFailed(context.WithoutCancel(ctx), store.RAGPolicySyncFence{TaskID: task.ID, KBID: task.KBID, TargetGenerationID: task.TargetGenerationID, TargetPolicyVersion: task.TargetPolicyVersion}, "cancelled", "cancelled before execution")
		}
	}
	return nil
}

func (s *Service) RollbackKBPolicy(ctx context.Context, ownerID, kbID, targetGenerationID, expectedGenerationID, note string) error {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	if err := s.ensureNoPolicySync(ctx, kbID); err != nil {
		return err
	}
	actor := ownerID
	if actor == "" {
		actor = kb.UserID
	}
	ok, err := s.st.RollbackRAGKBGeneration(ctx, kbID, targetGenerationID, expectedGenerationID, actor, note, policySyncRollbackWindow)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("generation rollback is unavailable or expired")
	}
	return nil
}

func (s *Service) policySyncLoop(ctx context.Context) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		if fence, claimed, err := s.st.ClaimNextRAGPolicySyncTask(ctx, s.workerID+"-policy", s.leaseDuration); err == nil && claimed {
			s.runPolicySync(ctx, *fence)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) runPolicySync(ctx context.Context, fence store.RAGPolicySyncFence) {
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := s.leaseDuration / 3
		if interval <= 0 {
			interval = 10 * time.Second
		}
		if interval > 2*time.Second {
			interval = 2 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				ok, err := s.st.HeartbeatRAGPolicySyncTask(context.WithoutCancel(workCtx), fence, s.leaseDuration)
				if err != nil || !ok {
					cancelWork()
					return
				}
			}
		}
	}()
	defer func() {
		cancelWork()
		<-heartbeatDone
	}()
	ctx = workCtx
	task, err := s.st.GetRAGPolicySyncTask(ctx, fence.TaskID)
	if err != nil {
		return
	}
	generation, err := s.st.GetRAGKBGeneration(ctx, fence.TargetGenerationID)
	if err != nil {
		s.failPolicySync(ctx, fence, nil, err)
		return
	}
	collectionKey, err := vector.CollectionKeyFromPersistence(generation.CollectionKey)
	if err != nil {
		s.failPolicySync(ctx, fence, generation, err)
		return
	}
	policyRecord, err := s.st.GetRAGPolicy(ctx, store.RAGPolicyIngestion, fence.TargetPolicyVersion)
	if err != nil {
		s.failPolicySync(ctx, fence, generation, err)
		return
	}
	policy, err := config.DecodeRAGIngestionPolicy([]byte(policyRecord.PolicyJSON))
	if err != nil {
		s.failPolicySync(ctx, fence, generation, err)
		return
	}
	mapped, err := s.st.ListRAGKBGenerationDocuments(ctx, generation.ID)
	if err == nil {
		err = s.vec.EnsureCollection(ctx, collectionKey, policy.Embedding.Dims)
	}
	var chunkCount int64
	for index, mapping := range mapped {
		if err != nil {
			break
		}
		if mapping.Status == store.RAGGenerationDocumentReady {
			var existing []store.RAGChunkRecord
			existing, err = s.st.ListRAGChunksByDocumentVersion(ctx, mapping.DocID, mapping.DocVersion)
			chunkCount += int64(len(existing))
			progress, _ := json.Marshal(map[string]any{"stage": "building", "completedDocuments": index + 1, "totalDocuments": len(mapped), "chunkCount": chunkCount})
			if ok, progressErr := s.st.UpdateRAGPolicySyncProgress(ctx, fence, string(progress)); progressErr != nil || !ok {
				err = errors.Join(progressErr, errors.New("policy sync progress fence failed"))
			}
			continue
		}
		count, buildErr := s.buildPolicySyncDocument(ctx, fence, generation, policy, mapping)
		if buildErr != nil {
			err = buildErr
			break
		}
		chunkCount += int64(count)
		progress, _ := json.Marshal(map[string]any{"stage": "building", "completedDocuments": index + 1, "totalDocuments": len(mapped), "chunkCount": chunkCount})
		if ok, progressErr := s.st.UpdateRAGPolicySyncProgress(ctx, fence, string(progress)); progressErr != nil || !ok {
			err = errors.Join(progressErr, errors.New("policy sync progress fence failed"))
		}
	}
	if err == nil {
		if ok, readyErr := s.st.MarkRAGKBGenerationReady(ctx, fence, int64(len(mapped)), chunkCount); readyErr != nil || !ok {
			err = errors.Join(readyErr, errors.New("generation validation fence failed"))
		}
	}
	if err == nil {
		if ok, activateErr := s.st.ActivateRAGKBGeneration(ctx, fence, task.SourceGenerationID, task.RequestedBy, "confirmed policy sync", policySyncRollbackWindow); activateErr != nil || !ok {
			err = errors.Join(activateErr, errors.New("generation activation conflict"))
		}
	}
	if err != nil {
		s.failPolicySync(ctx, fence, generation, err)
	}
}

func (s *Service) buildPolicySyncDocument(ctx context.Context, fence store.RAGPolicySyncFence, generation *store.RAGKBGenerationRecord, policy config.RAGIngestionPolicyData, mapping store.RAGGenerationDocumentRecord) (int, error) {
	doc, err := s.st.GetRAGDocument(ctx, mapping.DocID)
	if err != nil || doc.KBID != fence.KBID {
		return 0, errors.Join(err, errors.New("generation document membership changed"))
	}
	kb, err := s.st.GetRAGKB(ctx, fence.KBID)
	if err != nil {
		return 0, err
	}
	target := *doc
	target.Version = mapping.DocVersion
	snapshot, binding, err := s.buildVersionSnapshotAndBindingForPolicy(ctx, &target, &policy)
	if err != nil {
		return 0, err
	}
	ok, err := s.st.CreateRAGGenerationDocumentVersion(ctx, fence, snapshot)
	if err != nil || !ok {
		return 0, errors.Join(err, errors.New("create generation document version fence failed"))
	}
	request := EvaluationPipelineRequest{Target: PipelineTarget{OwnerID: kb.UserID, GenerationID: generation.ID}, Ingestion: policy, Contract: DefaultEvaluationGenerationContract(), Embedding: binding}
	artifact, err := s.loadOrBuildEvaluationArtifact(ctx, request, EvaluationPipelineDocument{ID: doc.ID, FileName: doc.FileName, MediaType: doc.FileType, ObjectKey: doc.ObjectKey, SHA256: snapshot.SourceSHA256, SizeBytes: doc.FileSize})
	if err != nil {
		return 0, err
	}
	chunks, _, err := s.buildSearchChunks(ctx, artifact, searchChunkBuildOptions{ChunkSize: policy.ChunkSize, ChunkOverlap: policy.ChunkOverlap, EnrichmentEnabled: policy.EnrichmentEnabled, TextModel: policy.DocumentAI.TextModel, Scope: enrich.CacheScope{UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, IndexFingerprint: snapshot.IndexFingerprint}})
	if err != nil {
		return 0, err
	}
	texts := make([]string, len(chunks))
	for index := range chunks {
		texts[index] = chunks[index].SearchContent
	}
	vectors, err := embed.New(binding.Endpoint, binding.APIKey, policy.Embedding.Model, policy.Embedding.Dims).Embed(ctx, texts)
	if err != nil || len(vectors) != len(chunks) {
		return 0, errors.Join(err, errors.New("generation embedding count validation failed"))
	}
	key, _ := vector.CollectionKeyFromPersistence(generation.CollectionKey)
	_ = s.st.DeleteRAGChunksByDocumentVersion(ctx, doc.ID, mapping.DocVersion)
	sqlChunks := make([]store.RAGChunkRecord, len(chunks))
	vectorChunks := make([]vector.ChunkData, len(chunks))
	refs := make([]vector.ChunkRef, len(chunks))
	for index, chunk := range chunks {
		location := chunk.Location
		if location.Kind == "" {
			location = document.SourceLocation{Kind: document.LocationDocument}
		}
		locationJSON, _ := json.Marshal(location)
		page := 0
		if location.Kind == document.LocationPage {
			page = location.Index
		}
		sqlChunks[index] = store.RAGChunkRecord{KBID: fence.KBID, DocID: doc.ID, DocVersion: mapping.DocVersion, ChunkIndex: chunk.Index, SectionTitle: chunk.SectionTitle, LocationJSON: string(locationJSON), RawContent: chunk.RawContent, Enhancement: chunk.Enhancement, SearchContent: chunk.SearchContent, TokenCount: chunk.Tokens}
		vectorChunks[index] = vector.ChunkData{DocID: doc.ID, Index: chunk.Index, Content: chunk.RawContent, SearchContent: chunk.SearchContent, SectionTitle: chunk.SectionTitle, PageNum: page, DocVersion: mapping.DocVersion, Vector: vectors[index]}
		refs[index] = vector.ChunkRef{DocID: doc.ID, Index: chunk.Index, DocVersion: mapping.DocVersion}
	}
	if err = s.st.PutRAGChunks(ctx, sqlChunks); err == nil {
		err = s.vec.UpsertChunks(ctx, key, vectorChunks)
	}
	if err == nil {
		var found []vector.ChunkData
		found, err = s.vec.GetChunks(ctx, key, refs)
		if err == nil && len(found) != len(refs) {
			err = errors.New("generation vector sample validation failed")
		}
		if err == nil && len(found) > 0 {
			var hits []vector.SearchHit
			hits, err = s.vec.HybridSearch(ctx, key, vector.SearchQuery{Dense: [][]float32{found[0].Vector}, Text: found[0].SearchContent, ActiveVersions: map[string]int64{doc.ID: mapping.DocVersion}}, 1)
			if err == nil && (len(hits) == 0 || hits[0].DocID != doc.ID || hits[0].DocVersion != mapping.DocVersion) {
				err = errors.New("generation retrieval smoke validation failed")
			}
		}
	}
	if err != nil {
		return 0, err
	}
	if ok, completeErr := s.st.CompleteRAGGenerationDocument(ctx, fence, doc.ID, len(chunks)); completeErr != nil || !ok {
		return 0, errors.Join(completeErr, errors.New("generation document completion failed"))
	}
	return len(chunks), nil
}

func (s *Service) failPolicySync(ctx context.Context, fence store.RAGPolicySyncFence, generation *store.RAGKBGenerationRecord, cause error) {
	status := store.RAGPolicySyncFailed
	code := "build_failed"
	if task, err := s.st.GetRAGPolicySyncTask(context.WithoutCancel(ctx), fence.TaskID); err == nil && task.CancelRequestedAt.Valid {
		status, code = store.RAGPolicySyncCancelled, "cancelled"
	}
	marked, _ := s.st.MarkRAGKBGenerationFailed(context.WithoutCancel(ctx), fence, code, fmt.Sprint(cause))
	if !marked {
		return
	}
	if generation != nil {
		if key, err := vector.CollectionKeyFromPersistence(generation.CollectionKey); err == nil {
			_ = s.vec.DropCollection(context.WithoutCancel(ctx), key)
		}
	}
	_, _ = s.st.FinishRAGPolicySyncTask(context.WithoutCancel(ctx), fence, status, code, fmt.Sprint(cause))
}
