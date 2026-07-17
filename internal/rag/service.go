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

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
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
	Workers      int
}

type Service struct {
	st          store.Store
	vec         vector.Store
	obj         objects.Store
	cfg         config.RAGCfg
	userCfg     UserEmbedCfgFn
	queryLLM    QueryLLMFn
	tasks       chan int64
	workerCount int
	startOnce   sync.Once
	kbLocks     sync.Map // map[string]*sync.RWMutex; deletion waits for in-flight KB work
	docLocks    sync.Map // map[string]*sync.Mutex; one index mutation per document
}

func New(d Deps) *Service {
	d.Cfg.ApplyDefaults()
	if d.Workers <= 0 {
		d.Workers = 2
	}
	return &Service{
		st:          d.Store,
		vec:         d.Vector,
		obj:         d.Objects,
		cfg:         d.Cfg,
		userCfg:     d.UserEmbedCfg,
		queryLLM:    d.QueryLLM,
		tasks:       make(chan int64, 256),
		workerCount: d.Workers,
	}
}

func (s *Service) MaxFileMB() int { return s.cfg.Limits.MaxFileMB }

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

func (s *Service) embedderForKB(ctx context.Context, kb *store.RAGKBRecord) *embed.Client {
	cfg, _ := s.resolveEmbedding(ctx, kb.UserID)
	return embed.New(cfg.Endpoint, cfg.APIKey, kb.EmbedModel, kb.EmbedDims)
}

func (s *Service) CreateKB(ctx context.Context, userID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	name = strings.TrimSpace(name)
	if userID == "" {
		return nil, ErrForbidden
	}
	if name == "" {
		return nil, errors.New("知识库名称不能为空")
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
		ID:            "kb_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		UserID:        userID,
		Name:          name,
		Description:   strings.TrimSpace(description),
		EmbedProvider: provider,
		EmbedModel:    embedCfg.Model,
		EmbedDims:     embedCfg.Dims,
		ChunkSize:     chunkSize,
		ChunkOverlap:  chunkOverlap,
		Status:        "active",
	}
	if err := s.st.CreateRAGKB(ctx, kb); err != nil {
		return nil, err
	}
	if err := s.vec.EnsureCollection(ctx, kb.ID, kb.EmbedDims); err != nil {
		_ = s.st.DeleteRAGKB(ctx, kb.ID)
		return nil, fmt.Errorf("创建向量 collection: %w", err)
	}
	return kb, nil
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
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
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
	if err := s.st.UpdateRAGKB(ctx, kb); err != nil {
		return nil, err
	}
	return kb, nil
}

func (s *Service) DeleteKB(ctx context.Context, ownerID, kbID string) error {
	// Holding the exclusive KB lock makes deletion wait for local uploads,
	// reindexes, document deletions, and indexing workers already in flight.
	// Once status is set to deleting, subsequently queued workers abandon the
	// document when they acquire the shared lock and re-read the KB row.
	kbLock := s.kbMutex(kbID)
	kbLock.Lock()
	defer kbLock.Unlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	kb.Status = "deleting"
	if err := s.st.UpdateRAGKB(ctx, kb); err != nil {
		return err
	}
	if err := s.vec.DropCollection(ctx, kbID); err != nil {
		// Keep the deleting row as a durable retry handle. Removing the only DB
		// record here would make leaked vectors impossible to clean up safely.
		return fmt.Errorf("删除向量 collection: %w", err)
	}
	if err := s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/", kb.UserID, kbID)); err != nil {
		return fmt.Errorf("删除知识库原件: %w", err)
	}
	return s.st.DeleteRAGKB(ctx, kbID)
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
