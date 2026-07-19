package rag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

// Start launches the bounded indexing workers and requeues durable PENDING or
// RUNNING tasks left by an earlier process. Calling Start more than once is
// harmless.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		for i := 0; i < s.workerCount; i++ {
			go s.worker(ctx)
		}
		go s.recoverTasks(ctx)
	})
}

func (s *Service) recoverTasks(ctx context.Context) {
	tasks, err := s.st.ListRunnableRAGIndexTasks(ctx)
	if err != nil {
		slog.Error("rag: recover index tasks failed", "error", err)
		return
	}
	for _, task := range tasks {
		select {
		case s.tasks <- task.ID:
		case <-ctx.Done():
			return
		}
	}
}

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.tasks:
			s.runTask(ctx, id)
		}
	}
}

// UploadDocument validates and persists an original document, creates its
// durable indexing task, then schedules asynchronous processing.
func (s *Service) UploadDocument(ctx context.Context, ownerID, kbID, fileName string, r io.Reader, size int64) (*store.RAGDocumentRecord, error) {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
	}
	if kb.Status != "active" {
		return nil, errors.New("知识库正在删除中")
	}
	fileName = strings.TrimSpace(fileName)
	if !parse.SupportedExt(fileName) {
		return nil, fmt.Errorf("不支持的文件类型（支持 md/txt/pdf/docx）")
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
	if err := s.obj.Put(ctx, key, r, size, contentType); err != nil {
		return nil, fmt.Errorf("保存原件: %w", err)
	}
	fileType := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
	if fileType == "markdown" {
		fileType = "md"
	}
	doc := &store.RAGDocumentRecord{
		ID:         docID,
		KBID:       kbID,
		FileName:   filepath.Base(fileName),
		FileType:   fileType,
		FileSize:   size,
		ObjectKey:  key,
		Status:     "PENDING",
		Version:    1,
		UploadedAt: time.Now().UTC(),
	}
	taskID, err := s.st.CreateRAGDocumentWithIndexTask(ctx, doc, 3)
	if err != nil {
		_ = s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID))
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
	if kb.Status != "active" {
		return errors.New("知识库正在删除中")
	}
	doc, err := s.GetDocument(ctx, ownerID, kbID, docID)
	if err != nil {
		return err
	}
	doc.Version++
	doc.Status = "PENDING"
	doc.ErrorMsg = ""
	doc.IndexedAt = nil
	taskID, err := s.st.UpdateRAGDocumentWithIndexTask(ctx, doc, 3)
	if err != nil {
		return err
	}
	s.scheduleTask(taskID)
	return nil
}

func (s *Service) DeleteDocument(ctx context.Context, ownerID, kbID, docID string) error {
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
	if _, err := s.GetDocument(ctx, ownerID, kbID, docID); err != nil {
		return err
	}
	if err := s.vec.DeleteDoc(ctx, kbID, docID); err != nil {
		return fmt.Errorf("删除文档向量: %w", err)
	}
	if err := s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID)); err != nil {
		return fmt.Errorf("删除文档原件: %w", err)
	}
	return s.st.DeleteRAGDocument(ctx, docID)
}

func (s *Service) enqueue(ctx context.Context, docID string) error {
	taskID, err := s.st.CreateRAGIndexTask(ctx, docID, 3)
	if err != nil {
		return err
	}
	s.scheduleTask(taskID)
	return nil
}

func (s *Service) scheduleTask(taskID int64) {
	select {
	case s.tasks <- taskID:
	default:
		// The durable row remains PENDING and will be recovered after restart.
		slog.Warn("rag: index task queue full; task persisted for recovery", "task", taskID)
	}
}

func (s *Service) runTask(ctx context.Context, taskID int64) {
	task, err := s.st.GetRAGIndexTask(ctx, taskID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Error("rag: read index task failed", "task", taskID, "error", err)
		}
		return
	}
	if err := s.st.UpdateRAGIndexTask(ctx, taskID, "RUNNING", task.RetryCount, ""); err != nil {
		slog.Error("rag: persist RUNNING task state failed", "task", taskID, "error", err)
		return
	}
	attemptVersion := 0
	if doc, getErr := s.st.GetRAGDocument(ctx, task.DocID); getErr == nil {
		attemptVersion = doc.Version
	}
	err = s.doIndex(ctx, task.DocID, attemptVersion)
	if err == nil {
		if updateErr := s.st.UpdateRAGIndexTask(ctx, taskID, "DONE", task.RetryCount, ""); updateErr != nil && !errors.Is(updateErr, store.ErrNotFound) {
			slog.Error("rag: persist DONE task state failed", "task", taskID, "error", updateErr)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	slog.Error("rag: document indexing failed", "doc", task.DocID, "retry", task.RetryCount, "error", err)
	if task.RetryCount < task.MaxRetry {
		nextRetry := task.RetryCount + 1
		if updateErr := s.st.UpdateRAGIndexTask(ctx, taskID, "PENDING", nextRetry, err.Error()); updateErr != nil {
			if !errors.Is(updateErr, store.ErrNotFound) {
				slog.Error("rag: persist retry task state failed", "task", taskID, "error", updateErr)
			}
			return
		}
		go func(retry int) {
			delay := time.Duration(1<<(retry-1)) * time.Second
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
				select {
				case s.tasks <- taskID:
				case <-ctx.Done():
				}
			case <-ctx.Done():
			}
		}(nextRetry)
		return
	}

	if updateErr := s.st.UpdateRAGIndexTask(ctx, taskID, "FAILED", task.RetryCount, err.Error()); updateErr != nil {
		if !errors.Is(updateErr, store.ErrNotFound) {
			slog.Error("rag: persist FAILED task state failed", "task", taskID, "error", updateErr)
		}
		return
	}
	if doc, getErr := s.st.GetRAGDocument(ctx, task.DocID); getErr == nil && attemptVersion > 0 && doc.Version == attemptVersion {
		doc.Status = "FAILED"
		doc.ErrorMsg = err.Error()
		updated, updateErr := s.st.UpdateRAGDocumentIfVersion(ctx, doc, attemptVersion)
		if updateErr != nil {
			slog.Error("rag: persist FAILED document state failed", "doc", task.DocID, "error", updateErr)
		} else if !updated {
			slog.Info("rag: skipped stale FAILED document state", "doc", task.DocID, "version", attemptVersion)
		}
	}
}

// doIndex implements parse -> split -> embed -> upsert-new -> delete-old.
// Old searchable chunks remain intact until the complete new version has been
// embedded and written successfully.
func (s *Service) doIndex(ctx context.Context, docID string, expectedVersion int) error {
	initial, err := s.st.GetRAGDocument(ctx, docID)
	if err != nil {
		return err
	}
	kbLock := s.kbMutex(initial.KBID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(docID)
	docLock.Lock()
	defer docLock.Unlock()

	// Re-read after acquiring both locks: a delete may have completed between
	// the initial lookup and lock acquisition.
	doc, err := s.st.GetRAGDocument(ctx, docID)
	if err != nil {
		return err
	}
	if expectedVersion > 0 && doc.Version != expectedVersion {
		return nil
	}
	expectedVersion = doc.Version
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return err
	}
	if kb.Status != "active" {
		return nil
	}
	if err := s.vec.EnsureCollection(ctx, kb.ID, kb.EmbedDims); err != nil {
		return fmt.Errorf("准备向量 collection: %w", err)
	}
	doc.Status = "PROCESSING"
	doc.ErrorMsg = ""
	updated, err := s.st.UpdateRAGDocumentIfVersion(ctx, doc, expectedVersion)
	if err != nil {
		return err
	}
	if !updated {
		return nil
	}

	rc, err := s.obj.Get(ctx, doc.ObjectKey)
	if err != nil {
		return fmt.Errorf("读原件: %w", err)
	}
	parsed, parseErr := parse.Parse(rc, doc.FileName)
	closeErr := rc.Close()
	if parseErr != nil {
		return parseErr
	}
	if closeErr != nil {
		return fmt.Errorf("关闭原件: %w", closeErr)
	}

	cfg := split.Config{ChunkSize: kb.ChunkSize, ChunkOverlap: kb.ChunkOverlap}
	var chunks []split.Chunk
	switch parsed.Format {
	case "md", "docx":
		chunks = split.Markdown(parsed.Pages[0].Text, cfg)
	case "pdf":
		for _, page := range parsed.Pages {
			for _, chunk := range split.SlidingWindow(page.Text, cfg, "", page.Num) {
				chunk.Index = len(chunks)
				chunks = append(chunks, chunk)
			}
		}
	default:
		chunks = split.SlidingWindow(parsed.Pages[0].Text, cfg, "", 0)
	}
	if len(chunks) == 0 {
		return errors.New("分块结果为空")
	}

	texts := make([]string, len(chunks))
	totalTokens := 0
	for i, chunk := range chunks {
		texts[i] = chunk.SearchContent
		if texts[i] == "" {
			texts[i] = chunk.Content
		}
		totalTokens += chunk.Tokens
	}
	vectors, err := s.embedderForKB(ctx, kb).Embed(ctx, texts)
	if err != nil {
		return err
	}
	data := make([]vector.ChunkData, len(chunks))
	for i, chunk := range chunks {
		data[i] = vector.ChunkData{
			DocID:         doc.ID,
			Index:         chunk.Index,
			Content:       chunk.Content,
			SearchContent: texts[i],
			SectionTitle:  chunk.SectionTitle,
			PageNum:       chunk.PageNum,
			DocVersion:    doc.Version,
			Vector:        vectors[i],
		}
	}
	currentDoc, err := s.st.GetRAGDocument(ctx, doc.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if currentDoc.Version != expectedVersion {
		return nil
	}
	currentKB, err := s.st.GetRAGKB(ctx, kb.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if currentKB.Status != "active" {
		return nil
	}
	if err := s.vec.UpsertChunks(ctx, kb.ID, data); err != nil {
		return fmt.Errorf("写入向量库: %w", err)
	}
	if err := s.vec.DeleteOldVersions(ctx, kb.ID, doc.ID, doc.Version); err != nil {
		// The new-version upsert is idempotent, so retry the durable task until
		// stale chunks are actually removed instead of marking a mixed-version
		// index as complete.
		return fmt.Errorf("清理旧向量版本: %w", err)
	}

	doc.Status = "DONE"
	doc.ErrorMsg = ""
	doc.ChunkCount = len(chunks)
	doc.TokenCount = totalTokens
	now := time.Now().UTC()
	doc.IndexedAt = &now
	updated, err = s.st.UpdateRAGDocumentIfVersion(ctx, doc, expectedVersion)
	if err != nil {
		return err
	}
	if updated {
		return nil
	}

	// A delete or newer reindex won the race after the vector upsert. Never
	// roll its relational state back. If the row was deleted, remove the
	// just-written orphaned vectors; if it advanced, clean only older versions.
	currentDoc, err = s.st.GetRAGDocument(ctx, doc.ID)
	if errors.Is(err, store.ErrNotFound) {
		if cleanupErr := s.vec.DeleteDoc(ctx, kb.ID, doc.ID); cleanupErr != nil {
			return fmt.Errorf("清理已删除文档的孤儿向量: %w", cleanupErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if currentDoc.Version > expectedVersion {
		if cleanupErr := s.vec.DeleteOldVersions(ctx, kb.ID, doc.ID, currentDoc.Version); cleanupErr != nil {
			return fmt.Errorf("清理过期索引任务向量: %w", cleanupErr)
		}
	}
	return nil
}
