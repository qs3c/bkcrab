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
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

var errIndexFenceLost = errors.New("RAG index fence lost")

// Start launches bounded durable workers. The in-process channel only reduces
// latency; every wake makes a SQL claim, and the periodic pump makes a dropped
// wake (including a full channel) recover without a process restart.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		for i := 0; i < s.workerCount; i++ {
			go s.worker(ctx)
		}
		go s.taskPump(ctx)
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
		return nil, errors.New("Office 文档解析能力当前不可用")
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
	hasher := sha256.New()
	if err := s.obj.Put(ctx, key, io.TeeReader(r, hasher), size, contentType); err != nil {
		return nil, fmt.Errorf("保存原件: %w", err)
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
	taskID, err := s.st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, snapshot, 3)
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
	snapshot, err := s.BuildVersionSnapshot(ctx, doc)
	if err != nil {
		return err
	}
	snapshot.DocVersion = 0 // assigned atomically by the store
	task, err := s.st.AdvanceDocumentVersionAndCreateTask(ctx, doc.Version, snapshot)
	if err != nil {
		return err
	}
	s.scheduleTask(task.ID)
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
	var embeddingBinding config.RAGEmbeddingCfg
	if err == nil {
		var current *store.RAGDocumentVersionRecord
		current, embeddingBinding, err = s.buildVersionSnapshotAndBinding(workCtx, doc)
		if err == nil && !sameRuntimeProviderContracts(&claim.Version, current) {
			current.DocVersion = 0
			created, ok, supersedeErr := s.st.SupersedeRAGIndexTaskAndCreateVersion(workCtx, claim.Fence, current)
			stopAndWaitHeartbeat()
			if supersedeErr != nil {
				slog.Error("rag: supersede provider-mismatched index task", "task", claim.Fence.TaskID, "error", supersedeErr)
				return
			}
			if ok && created != nil {
				s.scheduleTask(created.ID)
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
		slog.Error("rag: atomic index activation failed", "task", claim.Fence.TaskID, "error", err)
		return
	}
	if !ok {
		slog.Info("rag: skipped activation after index fence was lost", "task", claim.Fence.TaskID,
			"doc_version", claim.Fence.DocVersion, "generation", claim.Fence.ClaimGeneration)
	}
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
				leaseLost.Store(true)
				cancelWork()
				if err != nil && ctx.Err() == nil {
					slog.Error("rag: index heartbeat failed; canceling work", "task", fence.TaskID, "error", err)
				}
				return
			}
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
			slog.Error("rag: persist transient index retry", "task", claim.Fence.TaskID, "error", retryErr)
		} else if ok {
			slog.Warn("rag: transient index failure scheduled for retry", "task", claim.Fence.TaskID,
				"retry", claim.Task.RetryCount+1, "delay", delay, "error", message)
		}
		return
	}
	ok, failErr := s.st.FailRAGIndexTask(parent, claim.Fence, message)
	if failErr != nil {
		slog.Error("rag: persist permanent index failure", "task", claim.Fence.TaskID, "error", failErr)
	} else if ok {
		slog.Error("rag: document indexing failed permanently", "task", claim.Fence.TaskID, "error", message)
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
	if doc.Version != fence.DocVersion || version.DocVersion != fence.DocVersion {
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
	if version.EnrichmentEnabled {
		return activation, errors.New("unsupported: enrichment pipeline is not installed")
	}
	if version.ChunkSize <= 0 || version.ChunkOverlap < 0 || version.ChunkOverlap >= version.ChunkSize {
		return activation, errors.New("invalid immutable chunk contract")
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "loading"}); err != nil {
		return activation, err
	}
	if err := s.vec.EnsureCollection(ctx, kb.ID, version.EmbeddingDimensions); err != nil {
		return activation, fmt.Errorf("准备向量 collection: %w", err)
	}
	parsed, parseErr := s.parser.Parse(ctx, document.Source{
		DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType,
		Size: doc.FileSize, SHA256: version.SourceSHA256,
		Open: func(openCtx context.Context) (io.ReadCloser, error) {
			reader, openErr := s.obj.Get(openCtx, doc.ObjectKey)
			if openErr != nil {
				return nil, fmt.Errorf("读原件: %w", openErr)
			}
			return reader, nil
		},
	}, parse.ParseOptions{Mode: parseMode, ParserVersion: version.ParserVersion})
	if parseErr != nil {
		if parsed != nil {
			if closeErr := parsed.Close(); closeErr != nil {
				parseErr = errors.Join(parseErr, fmt.Errorf("close failed parsed document: %w", closeErr))
			}
		}
		return activation, parseErr
	}
	if parsed == nil {
		return activation, errors.New("parser returned a nil document")
	}
	parsedClosed := false
	defer func() {
		if parsedClosed {
			return
		}
		if closeErr := parsed.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close parsed document: %w", closeErr))
		}
	}()

	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "chunking"}); err != nil {
		return activation, err
	}
	cfg := split.Config{ChunkSize: version.ChunkSize, ChunkOverlap: version.ChunkOverlap}
	chunks := splitParsedDocument(parsed, doc.FileType, cfg)
	pageCount := parsedDocumentPageCount(parsed)
	warningCount := len(parsed.Warnings)
	degraded := false
	for _, warning := range parsed.Warnings {
		degraded = degraded || warning.Degraded
	}
	if closeErr := parsed.Close(); closeErr != nil {
		parsedClosed = true
		return activation, fmt.Errorf("关闭解析文档: %w", closeErr)
	}
	parsedClosed = true
	if len(chunks) == 0 {
		return activation, errors.New("分块结果为空")
	}

	texts := make([]string, len(chunks))
	totalTokens := 0
	for i, chunk := range chunks {
		texts[i] = chunk.SearchContent
		if texts[i] == "" {
			texts[i] = chunk.Content
		}
		if !s.cfg.Limits.SearchContentWithinLimit(texts[i]) {
			return activation, fmt.Errorf("SearchContent exceeds byte limit at chunk %d", chunk.Index)
		}
		totalTokens += chunk.Tokens
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "embedding", Current: 0, Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return activation, err
	}
	embedder := embed.New(embeddingBinding.Endpoint, embeddingBinding.APIKey,
		version.EmbeddingModel, version.EmbeddingDimensions)
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return activation, err
	}

	vectorChunks := make([]vector.ChunkData, len(chunks))
	sqlChunks := make([]store.RAGChunkRecord, len(chunks))
	now := time.Now().UTC()
	for i, chunk := range chunks {
		vectorChunks[i] = vector.ChunkData{
			DocID:         doc.ID,
			Index:         chunk.Index,
			Content:       chunk.Content,
			SearchContent: texts[i],
			SectionTitle:  chunk.SectionTitle,
			PageNum:       chunk.PageNum,
			DocVersion:    fence.DocVersion,
			Vector:        vectors[i],
		}
		location, _ := json.Marshal(struct {
			PageNum int `json:"pageNum,omitempty"`
		}{PageNum: chunk.PageNum})
		sqlChunks[i] = store.RAGChunkRecord{
			KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion, ChunkIndex: chunk.Index,
			SectionTitle: chunk.SectionTitle, LocationJSON: string(location), RawContent: chunk.Content,
			SearchContent: texts[i], TokenCount: chunk.Tokens, CreatedAt: now,
		}
	}
	for start := 0; start < len(sqlChunks); start += 200 {
		end := min(start+200, len(sqlChunks))
		if err := s.st.PutRAGChunks(ctx, sqlChunks[start:end]); err != nil {
			return activation, fmt.Errorf("写入 chunk catalog: %w", err)
		}
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "indexing", Current: len(chunks), Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return activation, err
	}
	valid, err := s.st.CheckRAGIndexFence(ctx, fence)
	if err != nil {
		return activation, err
	}
	if !valid {
		return activation, errIndexFenceLost
	}
	if err := s.vec.UpsertChunks(ctx, kb.ID, vectorChunks); err != nil {
		return activation, fmt.Errorf("写入向量库: %w", err)
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "finalizing", Current: len(chunks), Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return activation, err
	}
	activation = store.RAGIndexActivation{
		VersionResult: store.RAGDocumentVersionResult{
			Status: store.RAGDocumentVersionDone, PageCount: pageCount,
			AssetCount: len(parsed.Assets), Degraded: degraded, WarningCount: warningCount,
		},
		ChunkCount: len(chunks), TokenCount: totalTokens,
	}
	return activation, nil
}

func splitParsedDocument(parsed *document.ParsedDocument, format string, cfg split.Config) []split.Chunk {
	if parsed == nil {
		return nil
	}
	format = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
	var chunks []split.Chunk
	for _, unit := range parsed.Units {
		var unitChunks []split.Chunk
		switch format {
		case "md", "markdown", "docx", "pptx", "xlsx":
			unitChunks = split.Markdown(unit.Markdown, cfg)
		default:
			pageNumber := 0
			if unit.Location.Kind == document.LocationPage {
				pageNumber = unit.Location.Index
			}
			unitChunks = split.SlidingWindow(unit.Markdown, cfg, "", pageNumber)
		}
		for _, chunk := range unitChunks {
			chunk.Index = len(chunks)
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

func parsedDocumentPageCount(parsed *document.ParsedDocument) int {
	if parsed == nil {
		return 0
	}
	pageCount := 0
	for _, unit := range parsed.Units {
		if unit.Location.Kind == document.LocationDocument && pageCount == 0 {
			pageCount = 1
		}
		if unit.Location.Kind == document.LocationPage && unit.Location.Index > pageCount {
			pageCount = unit.Location.Index
		}
	}
	for _, warning := range parsed.Warnings {
		if warning.Location != nil && warning.Location.Kind == document.LocationPage && warning.Location.Index > pageCount {
			pageCount = warning.Location.Index
		}
	}
	return pageCount
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
