// Package rag is the RAG module facade. It owns knowledge-base management,
// document ingestion, and retrieval; callers never access the backing stores
// directly.
package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/rerank"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

var (
	ErrNotFound  = errors.New("知识库或文档不存在")
	ErrForbidden = errors.New("无权访问该知识库")
	ErrQuota     = errors.New("超出配额限制")
)

// UserEmbedCfgFn resolves a user-level embedding override. A false return
// means the system embedding configuration should be used.
type UserEmbedCfgFn func(ctx context.Context, userID string) (config.RAGEmbeddingCfg, bool)

// QueryLLMFn is the narrow LLM boundary used by retrieval query planning.
// The gateway resolves the effective user model and provider; the RAG package
// owns the prompt, output validation, and fallback behavior.
type QueryLLMFn func(ctx context.Context, userID, systemPrompt, userPrompt string) (string, error)

type Deps struct {
	Store        store.Store
	Vector       vector.Store
	Objects      objects.Store
	Cfg          config.RAGCfg
	UserEmbedCfg UserEmbedCfgFn
	QueryLLM     QueryLLMFn
	Reranker     rerank.Reranker
	Parser       parse.Parser
	Primitives   parse.PrimitiveExtractor
	PageVision   vision.PageTranscriber
	ImageVision  vision.ImageTranscriber
	Enricher     enrich.Enricher
	Tokenizer    enrich.Tokenizer
	// Telemetry receives privacy-safe, closed-schema operational events. When
	// omitted, the service uses the default structured logger.
	Telemetry telemetry.Recorder
	// OfficeAvailable reads the background-probed, three-golden-gated
	// capability snapshot. Upload paths must not synchronously probe sidecar.
	OfficeAvailable func() bool
	Workers         int
}

type Service struct {
	st              store.Store
	vec             vector.Store
	obj             objects.Store
	cfg             config.RAGCfg
	userCfg         UserEmbedCfgFn
	queryLLM        QueryLLMFn
	reranker        rerank.Reranker
	parser          parse.Parser
	primitives      parse.PrimitiveExtractor
	pageVision      vision.PageTranscriber
	imageVision     vision.ImageTranscriber
	enricher        enrich.Enricher
	tokenizer       enrich.Tokenizer
	telemetry       telemetry.Recorder
	officeAvailable func() bool
	tasks           chan int64
	workerCount     int
	workerID        string

	// The in-memory channel is only a latency hint. SQL claim/lease state is
	// authoritative and pollInterval guarantees recovery after a dropped hint.
	pollInterval                    time.Duration
	leaseDuration                   time.Duration
	heartbeatInterval               time.Duration
	gcGracePeriod                   time.Duration
	stagingArtifactTTL              time.Duration
	maxCacheFingerprintsPerDocument int
	cacheSweepCursorMu              sync.Mutex
	cacheSweepCursor                string
	deletingDocumentSweepCursor     string
	deletingKBSweepCursor           string
	startOnce                       sync.Once
	kbLocks                         sync.Map // map[string]*sync.RWMutex; deletion waits for in-flight KB work
	docLocks                        sync.Map // map[string]*sync.Mutex; one index mutation per document
}

func New(d Deps) *Service {
	d.Cfg.ApplyDefaults()
	recorder := d.Telemetry
	if recorder == nil {
		recorder = telemetry.NewSlogRecorder(nil)
	}
	if d.Workers <= 0 {
		d.Workers = 2
	}
	if d.Parser == nil {
		d.Parser = parse.NewLocalParser(
			d.Primitives, d.Cfg.Limits.MaxPagesPerDocument, d.Cfg.Limits.MaxExtractedBytes,
		)
	}
	if local, ok := d.Parser.(*parse.LocalParser); ok {
		// Parser routing limits are system policy, not mutable document input.
		// Fill zero values even for a caller-supplied LocalParser so gateway and
		// tests cannot accidentally bypass the configured document ceilings.
		if local.MaxPages <= 0 {
			local.MaxPages = d.Cfg.Limits.MaxPagesPerDocument
		}
		if local.MaxAssets <= 0 {
			local.MaxAssets = d.Cfg.Limits.MaxAssetsPerDocument
		}
		if local.MaxVisionPages <= 0 {
			local.MaxVisionPages = d.Cfg.Limits.MaxVisionPagesPerDocument
		}
		if local.MaxVisionAssets <= 0 {
			local.MaxVisionAssets = d.Cfg.Limits.MaxVisionAssetsPerDocument
		}
		if local.MaxExtractedBytes <= 0 {
			local.MaxExtractedBytes = d.Cfg.Limits.MaxExtractedBytes
		}
		if local.MaxAssetBytes <= 0 {
			local.MaxAssetBytes = d.Cfg.Limits.MaxAssetBytes
		}
		if local.MaxVisionInputBytes <= 0 {
			local.MaxVisionInputBytes = d.Cfg.Limits.MaxVisionInputBytes
		}
		if local.MaxImagePixels <= 0 {
			local.MaxImagePixels = d.Cfg.Limits.MaxImagePixels
		}
		if local.VisionImageMaxEdge <= 0 {
			local.VisionImageMaxEdge = d.Cfg.Limits.DisplayMaxEdge
		}
	}
	// Components that expose the narrow recorder hook share the same sink as
	// the orchestrator. This keeps one correlation stream without widening any
	// parser/provider interface to accept arbitrary log attributes.
	for _, component := range []any{d.Parser, d.Primitives, d.PageVision, d.ImageVision, d.Enricher} {
		if observable, ok := component.(interface{ SetRecorder(telemetry.Recorder) }); ok {
			observable.SetRecorder(recorder)
		}
	}
	return &Service{
		st:                              d.Store,
		vec:                             d.Vector,
		obj:                             d.Objects,
		cfg:                             d.Cfg,
		userCfg:                         d.UserEmbedCfg,
		queryLLM:                        d.QueryLLM,
		reranker:                        d.Reranker,
		parser:                          d.Parser,
		primitives:                      d.Primitives,
		pageVision:                      d.PageVision,
		imageVision:                     d.ImageVision,
		enricher:                        d.Enricher,
		tokenizer:                       d.Tokenizer,
		telemetry:                       recorder,
		officeAvailable:                 d.OfficeAvailable,
		tasks:                           make(chan int64, 256),
		workerCount:                     d.Workers,
		workerID:                        "rag-" + uuid.NewString(),
		pollInterval:                    time.Second,
		leaseDuration:                   time.Minute,
		heartbeatInterval:               20 * time.Second,
		gcGracePeriod:                   time.Duration(d.Cfg.Limits.IndexGCGracePeriod) * time.Second,
		stagingArtifactTTL:              time.Duration(d.Cfg.Limits.StagingArtifactTTL) * time.Second,
		maxCacheFingerprintsPerDocument: d.Cfg.Limits.MaxCacheFingerprintsPerDocument,
	}
}

// newTaskDocumentAIBudget binds one immutable version snapshot and claim fence
// to the durable façade shared by page vision, Office image vision, repairs and
// text enrichment. It owns no process-local spend counters.
func (s *Service) newTaskDocumentAIBudget(
	claim *store.RAGIndexClaim,
	userID string,
) (*vision.TaskDocumentAIBudget, error) {
	if s == nil || s.st == nil || claim == nil || strings.TrimSpace(userID) == "" {
		return nil, errors.New("RAG DocumentAI budget requires store, claim, and user")
	}
	version := claim.Version
	return vision.NewTaskDocumentAIBudget(s.st, vision.TaskBudgetConfig{
		Fence: claim.Fence, UserID: userID,
		TaskLimits: store.RAGDocumentAILimits{
			MaxRequests:     int64(version.MaxDocumentAIRequests),
			MaxTokens:       version.MaxDocumentAITokens,
			MaxCostMicroUSD: version.MaxDocumentAICostMicroUSD,
		},
		UserLimits: store.RAGDocumentAILimits{
			MaxRequests:     int64(s.cfg.Limits.MaxDocumentAIRequestsPerUserPerDay),
			MaxTokens:       s.cfg.Limits.MaxDocumentAITokensPerUserPerDay,
			MaxCostMicroUSD: microUSD(s.cfg.Limits.MaxEstimatedDocumentAICostPerUserPerDayUSD),
		},
		ReservationTTL: time.Duration(s.cfg.DocumentAI.TimeoutMS)*time.Millisecond + time.Minute,
		Recorder:       s.telemetry,
	}), nil
}

func (s *Service) documentAIReconcileLoop(ctx context.Context) {
	if s == nil || s.st == nil {
		return
	}
	timeout := time.Duration(s.cfg.DocumentAI.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := timeout / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	reconcile := func() {
		now := time.Now().UTC()
		count, err := s.st.ReconcileRAGDocumentAIUsage(ctx, now, now.Add(-timeout-time.Minute), 100)
		fields := telemetry.Fields{
			Operation: "usage_reconcile", Transition: "reconcile", Outcome: "ok", SuccessCount: count,
		}
		if err != nil {
			fields.Outcome = "error"
			switch {
			case errors.Is(err, context.Canceled):
				fields.ErrorCode = "canceled"
			case errors.Is(err, context.DeadlineExceeded):
				fields.ErrorCode = "timeout"
			default:
				fields.ErrorCode = "store_error"
			}
		}
		telemetry.Emit(ctx, s.telemetry, telemetry.EventDocumentAIBudget, fields)
	}
	reconcile()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func (s *Service) MaxFileMB() int { return s.cfg.Limits.MaxFileMB }

// Config returns the immutable system RAG configuration snapshot captured at
// service construction. Callers must not mutate slice fields on the result.
func (s *Service) Config() config.RAGCfg { return s.cfg }

// Close releases optional backend resources. Worker shutdown is controlled by
// the context passed to Start; vector backends such as Milvus additionally own
// a client connection that should be closed during gateway shutdown.
func (s *Service) Close(ctx context.Context) error {
	closer, ok := s.vec.(interface{ Close(context.Context) error })
	if !ok {
		return nil
	}
	return closer.Close(ctx)
}

func (s *Service) kbMutex(kbID string) *sync.RWMutex {
	lock, _ := s.kbLocks.LoadOrStore(kbID, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

func (s *Service) docMutex(docID string) *sync.Mutex {
	lock, _ := s.docLocks.LoadOrStore(docID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *Service) resolveEmbedding(ctx context.Context, userID string) (config.RAGEmbeddingCfg, string) {
	if s.userCfg != nil {
		if c, ok := s.userCfg(ctx, userID); ok && c.Endpoint != "" && c.Model != "" && c.Dims > 0 {
			return c, "user"
		}
	}
	return s.cfg.Embedding, "system"
}

func (s *Service) embeddingConfigForKB(ctx context.Context, kb *store.RAGKBRecord) (config.RAGEmbeddingCfg, error) {
	if kb == nil {
		return config.RAGEmbeddingCfg{}, errors.New("embedding KB is nil")
	}
	var cfg config.RAGEmbeddingCfg
	switch kb.EmbedProvider {
	case "user":
		if s.userCfg == nil {
			return cfg, errors.New("KB 绑定的用户 embedding 配置不可用")
		}
		var ok bool
		cfg, ok = s.userCfg(ctx, kb.UserID)
		if !ok {
			return cfg, errors.New("KB 绑定的用户 embedding 配置不可用")
		}
	default:
		cfg = s.cfg.Embedding
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return cfg, errors.New("KB 绑定的 embedding endpoint 不可用")
	}
	return cfg, nil
}

func (s *Service) embedderForKB(ctx context.Context, kb *store.RAGKBRecord) (*embed.Client, error) {
	cfg, err := s.embeddingConfigForKB(ctx, kb)
	if err != nil {
		return nil, err
	}
	return embed.New(cfg.Endpoint, cfg.APIKey, kb.EmbedModel, kb.EmbedDims), nil
}

func (s *Service) requireActiveUser(ctx context.Context, userID string) error {
	user, err := s.st.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(user.Status, "active") {
		return ErrForbidden
	}
	return nil
}

type KBParsingOptions struct {
	ParseMode         config.ParseMode
	EnrichmentEnabled bool
}

func (s *Service) CreateKB(ctx context.Context, userID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	return s.CreateKBWithOptions(ctx, userID, name, description, chunkSize, chunkOverlap, KBParsingOptions{
		ParseMode: config.ParseModeStandard,
	})
}

func (s *Service) CreateKBWithOptions(
	ctx context.Context,
	userID, name, description string,
	chunkSize, chunkOverlap int,
	options KBParsingOptions,
) (*store.RAGKBRecord, error) {
	name = strings.TrimSpace(name)
	if userID == "" {
		return nil, ErrForbidden
	}
	if err := s.requireActiveUser(ctx, userID); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("知识库名称不能为空")
	}
	if !options.ParseMode.Valid() {
		return nil, errors.New("parseMode 必须是 standard 或 auto")
	}
	existing, err := s.st.ListRAGKBsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= s.cfg.Limits.MaxKBsPerUser {
		return nil, fmt.Errorf("%w: 每用户最多 %d 个知识库", ErrQuota, s.cfg.Limits.MaxKBsPerUser)
	}
	embedCfg, provider := s.resolveEmbedding(ctx, userID)
	if embedCfg.Endpoint == "" || embedCfg.Model == "" || embedCfg.Dims <= 0 {
		return nil, errors.New("embedding 未配置，请先在系统或用户设置中配置")
	}
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if chunkOverlap <= 0 || chunkOverlap >= chunkSize {
		chunkOverlap = min(64, chunkSize/8)
		if chunkOverlap <= 0 {
			chunkOverlap = 1
		}
	}

	kb := &store.RAGKBRecord{
		ID:                "kb_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		UserID:            userID,
		Name:              name,
		Description:       strings.TrimSpace(description),
		EmbedProvider:     provider,
		EmbedModel:        embedCfg.Model,
		EmbedDims:         embedCfg.Dims,
		ChunkSize:         chunkSize,
		ChunkOverlap:      chunkOverlap,
		ParseMode:         string(options.ParseMode),
		EnrichmentEnabled: options.EnrichmentEnabled,
		Status:            store.RAGKBStatusProvisioning,
	}
	kbLock := s.kbMutex(kb.ID)
	kbLock.Lock()
	defer kbLock.Unlock()
	fence, err := s.st.BeginRAGKBProvisioning(
		ctx, kb, s.workerID+"-kb", s.leaseDuration, s.cfg.Limits.MaxKBsPerUser,
	)
	if err != nil {
		return nil, mapKBProvisioningStoreError(err, s.cfg.Limits.MaxKBsPerUser)
	}
	if err := s.ensureProvisionedKBCollection(ctx, kb, *fence); err != nil {
		if cleanupErr := s.abandonKBProvisioning(ctx, *fence); cleanupErr != nil {
			logLifecycleFailure("abandon_kb_provision", kb.ID, cleanupErr)
		}
		return nil, fmt.Errorf("创建向量 collection: %w", err)
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := s.abandonKBProvisioning(ctx, *fence); cleanupErr != nil {
			logLifecycleFailure("cancel_kb_provision", kb.ID, cleanupErr)
		}
		return nil, err
	}
	active, activated, err := s.st.ActivateRAGKBProvisioning(ctx, *fence)
	if err != nil || !activated {
		if cleanupErr := s.abandonKBProvisioning(ctx, *fence); cleanupErr != nil {
			logLifecycleFailure("finalize_kb_provision", kb.ID, cleanupErr)
		}
		if err != nil {
			return nil, mapKBProvisioningStoreError(err, s.cfg.Limits.MaxKBsPerUser)
		}
		return nil, errRAGKBProvisionFenceLost
	}
	return active, nil
}

// GetKB enforces ownership. An empty ownerID is reserved for explicitly
// privileged internal/admin paths and skips the ownership check.
func (s *Service) GetKB(ctx context.Context, ownerID, kbID string) (*store.RAGKBRecord, error) {
	kb, err := s.st.GetRAGKB(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ownerID != "" && kb.UserID != ownerID {
		return nil, ErrForbidden
	}
	return kb, nil
}

func (s *Service) ListKBs(ctx context.Context, userID string) ([]store.RAGKBRecord, error) {
	return s.st.ListRAGKBsByUser(ctx, userID)
}

func (s *Service) UpdateKB(ctx context.Context, ownerID, kbID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	return s.updateKB(ctx, ownerID, kbID, name, description, chunkSize, chunkOverlap, nil)
}

func (s *Service) UpdateKBWithOptions(
	ctx context.Context,
	ownerID, kbID, name, description string,
	chunkSize, chunkOverlap int,
	options KBParsingOptions,
) (*store.RAGKBRecord, error) {
	if !options.ParseMode.Valid() {
		return nil, errors.New("parseMode 必须是 standard 或 auto")
	}
	return s.updateKB(ctx, ownerID, kbID, name, description, chunkSize, chunkOverlap, &options)
}

func (s *Service) updateKB(
	ctx context.Context,
	ownerID, kbID, name, description string,
	chunkSize, chunkOverlap int,
	options *KBParsingOptions,
) (*store.RAGKBRecord, error) {
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
	if !strings.EqualFold(kb.Status, "active") {
		return nil, errors.New("知识库正在删除中")
	}
	if strings.TrimSpace(name) != "" {
		kb.Name = strings.TrimSpace(name)
	}
	kb.Description = strings.TrimSpace(description)
	if chunkSize > 0 {
		kb.ChunkSize = chunkSize
	}
	if chunkOverlap > 0 && chunkOverlap < kb.ChunkSize {
		kb.ChunkOverlap = chunkOverlap
	}
	if kb.ChunkOverlap >= kb.ChunkSize {
		return nil, errors.New("chunkOverlap 必须小于 chunkSize")
	}
	if options != nil {
		kb.ParseMode = string(options.ParseMode)
		kb.EnrichmentEnabled = options.EnrichmentEnabled
	}
	if err := s.st.UpdateRAGKB(ctx, kb); err != nil {
		return nil, err
	}
	return kb, nil
}

func (s *Service) DeleteKB(ctx context.Context, ownerID, kbID string) error {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	return s.deleteKBRecord(ctx, kb)
}

func (s *Service) GetDocument(ctx context.Context, ownerID, kbID, docID string) (*store.RAGDocumentRecord, error) {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return nil, err
	}
	doc, err := s.st.GetRAGDocument(ctx, docID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && doc.KBID != kbID) {
		return nil, ErrNotFound
	}
	return doc, err
}

func (s *Service) ListDocuments(ctx context.Context, ownerID, kbID string) ([]store.RAGDocumentRecord, error) {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return nil, err
	}
	return s.st.ListRAGDocumentsByKB(ctx, kbID)
}
