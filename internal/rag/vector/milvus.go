package vector

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/index"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

const (
	milvusFieldID           = "id"
	milvusFieldDocID        = "doc_id"
	milvusFieldChunkIndex   = "chunk_index"
	milvusFieldSectionTitle = "section_title"
	milvusFieldPageNum      = "page_num"
	milvusFieldDocVersion   = "doc_version"
	milvusFieldContent      = "content"
	milvusFieldSparse       = "content_sparse"
	milvusFieldEmbedding    = "embedding"
)

var milvusOutputFields = []string{
	milvusFieldDocID,
	milvusFieldChunkIndex,
	milvusFieldContent,
	milvusFieldSectionTitle,
	milvusFieldPageNum,
}

// Milvus is the production Store implementation. The v2 SDK client is safe
// for concurrent use; ensureMu only serializes the non-atomic
// HasCollection/CreateCollection sequence.
type Milvus struct {
	client   *milvusclient.Client
	ensureMu sync.Mutex
	ready    sync.Map // collection name -> struct{}, loaded during this process
}

// NewMilvus connects to a Milvus instance. Address accepts the SDK's usual
// host:port or URL forms.
func NewMilvus(ctx context.Context, addr, user, pass string) (*Milvus, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, fmt.Errorf("milvus address 不能为空")
	}
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{
		Address:  addr,
		Username: user,
		Password: pass,
	})
	if err != nil {
		return nil, fmt.Errorf("connect milvus: %w", err)
	}
	return &Milvus{client: client}, nil
}

// Close releases the underlying SDK connection.
func (m *Milvus) Close(ctx context.Context) error {
	if m == nil || m.client == nil {
		return nil
	}
	return m.client.Close(ctx)
}

func (m *Milvus) EnsureCollection(ctx context.Context, kbID string, dims int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if kbID == "" {
		return fmt.Errorf("collection id 不能为空")
	}
	if dims <= 0 {
		return fmt.Errorf("collection %s 的向量维度必须大于 0", kbID)
	}

	m.ensureMu.Lock()
	defer m.ensureMu.Unlock()

	name := ragCollectionName(kbID)
	if _, ok := m.ready.Load(name); ok {
		return nil
	}

	exists, err := m.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
	if err != nil {
		return fmt.Errorf("check milvus collection %s: %w", name, err)
	}
	if exists {
		if err := m.loadCollection(ctx, name); err != nil {
			return err
		}
		m.ready.Store(name, struct{}{})
		return nil
	}

	if err := m.client.CreateCollection(ctx, milvusclient.NewCreateCollectionOption(name, ragMilvusSchema(dims)).
		WithConsistencyLevel(entity.ClStrong)); err != nil {
		return fmt.Errorf("create milvus collection %s: %w", name, err)
	}

	cleanup := func(stage string, cause error) error {
		// Best effort: remove a partially configured collection so a later
		// EnsureCollection call can retry from a clean state.
		_ = m.client.DropCollection(ctx, milvusclient.NewDropCollectionOption(name))
		return fmt.Errorf("%s for milvus collection %s: %w", stage, name, cause)
	}

	if err := m.createIndex(ctx, name, milvusFieldEmbedding,
		index.NewAutoIndex(entity.COSINE), milvusFieldEmbedding+"_idx"); err != nil {
		return cleanup("create dense index", err)
	}
	if err := m.createIndex(ctx, name, milvusFieldSparse,
		index.NewSparseInvertedIndex(entity.BM25, 0), milvusFieldSparse+"_idx"); err != nil {
		return cleanup("create BM25 index", err)
	}

	if err := m.loadCollection(ctx, name); err != nil {
		return cleanup("load", err)
	}
	m.ready.Store(name, struct{}{})
	return nil
}

func (m *Milvus) loadCollection(ctx context.Context, name string) error {
	loadTask, err := m.client.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(name))
	if err != nil {
		return fmt.Errorf("load milvus collection %s: %w", name, err)
	}
	if err := loadTask.Await(ctx); err != nil {
		return fmt.Errorf("await milvus collection %s load: %w", name, err)
	}
	return nil
}

func (m *Milvus) createIndex(ctx context.Context, collectionName, fieldName string, idx index.Index, indexName string) error {
	task, err := m.client.CreateIndex(ctx,
		milvusclient.NewCreateIndexOption(collectionName, fieldName, idx).WithIndexName(indexName))
	if err != nil {
		return err
	}
	return task.Await(ctx)
}

func (m *Milvus) UpsertChunks(ctx context.Context, kbID string, chunks []ChunkData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	dims := len(chunks[0].Vector)
	if dims == 0 {
		return fmt.Errorf("collection %s: chunk 向量不能为空", kbID)
	}

	ids := make([]string, 0, len(chunks))
	docIDs := make([]string, 0, len(chunks))
	chunkIndexes := make([]int64, 0, len(chunks))
	sectionTitles := make([]string, 0, len(chunks))
	pageNums := make([]int64, 0, len(chunks))
	docVersions := make([]int64, 0, len(chunks))
	contents := make([]string, 0, len(chunks))
	vectors := make([][]float32, 0, len(chunks))

	for i, chunk := range chunks {
		if len(chunk.Vector) != dims {
			return fmt.Errorf("collection %s: chunk %d 的向量维度为 %d，期望 %d", kbID, i, len(chunk.Vector), dims)
		}
		ids = append(ids, milvusChunkID(chunk))
		docIDs = append(docIDs, chunk.DocID)
		chunkIndexes = append(chunkIndexes, int64(chunk.Index))
		sectionTitles = append(sectionTitles, chunk.SectionTitle)
		pageNums = append(pageNums, int64(chunk.PageNum))
		docVersions = append(docVersions, int64(chunk.DocVersion))
		contents = append(contents, chunk.Content)
		vectors = append(vectors, append([]float32(nil), chunk.Vector...))
	}

	option := milvusclient.NewColumnBasedInsertOption(ragCollectionName(kbID)).
		WithVarcharColumn(milvusFieldID, ids).
		WithVarcharColumn(milvusFieldDocID, docIDs).
		WithInt64Column(milvusFieldChunkIndex, chunkIndexes).
		WithVarcharColumn(milvusFieldSectionTitle, sectionTitles).
		WithInt64Column(milvusFieldPageNum, pageNums).
		WithInt64Column(milvusFieldDocVersion, docVersions).
		WithVarcharColumn(milvusFieldContent, contents).
		WithFloatVectorColumn(milvusFieldEmbedding, dims, vectors)
	if _, err := m.client.Upsert(ctx, option); err != nil {
		return fmt.Errorf("upsert chunks into %s: %w", ragCollectionName(kbID), err)
	}
	return nil
}

func (m *Milvus) DeleteOldVersions(ctx context.Context, kbID, docID string, keepVersion int) error {
	expr := fmt.Sprintf(`%s == "%s" && %s < %d`,
		milvusFieldDocID, escapeMilvusExprString(docID), milvusFieldDocVersion, keepVersion)
	return m.deleteByExpr(ctx, kbID, expr)
}

func (m *Milvus) DeleteDoc(ctx context.Context, kbID, docID string) error {
	expr := fmt.Sprintf(`%s == "%s"`, milvusFieldDocID, escapeMilvusExprString(docID))
	return m.deleteByExpr(ctx, kbID, expr)
}

func (m *Milvus) deleteByExpr(ctx context.Context, kbID, expr string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := ragCollectionName(kbID)
	if _, err := m.client.Delete(ctx, milvusclient.NewDeleteOption(name).WithExpr(expr)); err != nil {
		return fmt.Errorf("delete from %s: %w", name, err)
	}
	return nil
}

func (m *Milvus) DropCollection(ctx context.Context, kbID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := ragCollectionName(kbID)
	exists, err := m.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
	if err != nil {
		return fmt.Errorf("check milvus collection %s: %w", name, err)
	}
	if !exists {
		m.ready.Delete(name)
		return nil
	}
	if err := m.client.DropCollection(ctx, milvusclient.NewDropCollectionOption(name)); err != nil {
		// A concurrent delete after HasCollection is also a successful outcome.
		stillExists, checkErr := m.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
		if checkErr == nil && !stillExists {
			return nil
		}
		return fmt.Errorf("drop milvus collection %s: %w", name, err)
	}
	m.ready.Delete(name)
	return nil
}

func (m *Milvus) HybridSearch(ctx context.Context, kbID string, queryVec []float32, queryText string, topK int) ([]SearchHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return []SearchHit{}, nil
	}
	if len(queryVec) == 0 {
		return nil, fmt.Errorf("collection %s: 查询向量不能为空", kbID)
	}

	name := ragCollectionName(kbID)
	dense := milvusclient.NewAnnRequest(milvusFieldEmbedding, topK, entity.FloatVector(queryVec))
	var (
		resultSets []milvusclient.ResultSet
		err        error
	)
	if strings.TrimSpace(queryText) == "" {
		resultSets, err = m.client.Search(ctx,
			milvusclient.NewSearchOption(name, topK, []entity.Vector{entity.FloatVector(queryVec)}).
				WithANNSField(milvusFieldEmbedding).
				WithOutputFields(milvusOutputFields...).
				WithConsistencyLevel(entity.ClStrong))
	} else {
		text := milvusclient.NewAnnRequest(milvusFieldSparse, topK, entity.Text(queryText))
		resultSets, err = m.client.HybridSearch(ctx,
			milvusclient.NewHybridSearchOption(name, topK, dense, text).
				WithReranker(milvusclient.NewRRFReranker().WithK(60)).
				WithOutputFields(milvusOutputFields...).
				WithConsistencyLevel(entity.ClStrong))
	}
	if err != nil {
		return nil, fmt.Errorf("search milvus collection %s: %w", name, err)
	}
	return milvusResultHits(resultSets)
}

func milvusResultHits(resultSets []milvusclient.ResultSet) ([]SearchHit, error) {
	var hits []SearchHit
	for setIndex := range resultSets {
		result := &resultSets[setIndex]
		if result.Err != nil {
			return nil, fmt.Errorf("decode milvus result set %d: %w", setIndex, result.Err)
		}
		if result.ResultCount == 0 {
			continue
		}
		if len(result.Scores) < result.ResultCount {
			return nil, fmt.Errorf("milvus result set %d has %d rows but %d scores", setIndex, result.ResultCount, len(result.Scores))
		}
		for row := 0; row < result.ResultCount; row++ {
			docID, err := milvusStringField(result, milvusFieldDocID, row)
			if err != nil {
				return nil, err
			}
			chunkIndex, err := milvusInt64Field(result, milvusFieldChunkIndex, row)
			if err != nil {
				return nil, err
			}
			content, err := milvusStringField(result, milvusFieldContent, row)
			if err != nil {
				return nil, err
			}
			sectionTitle, err := milvusStringField(result, milvusFieldSectionTitle, row)
			if err != nil {
				return nil, err
			}
			pageNum, err := milvusInt64Field(result, milvusFieldPageNum, row)
			if err != nil {
				return nil, err
			}
			hits = append(hits, SearchHit{
				DocID:        docID,
				ChunkIndex:   int(chunkIndex),
				Content:      content,
				SectionTitle: sectionTitle,
				PageNum:      int(pageNum),
				Score:        float64(result.Scores[row]),
			})
		}
	}
	return hits, nil
}

func milvusStringField(result *milvusclient.ResultSet, field string, row int) (string, error) {
	col := result.GetColumn(field)
	if col == nil {
		return "", fmt.Errorf("milvus result is missing field %s", field)
	}
	value, err := col.GetAsString(row)
	if err != nil {
		return "", fmt.Errorf("read milvus field %s row %d: %w", field, row, err)
	}
	return value, nil
}

func milvusInt64Field(result *milvusclient.ResultSet, field string, row int) (int64, error) {
	col := result.GetColumn(field)
	if col == nil {
		return 0, fmt.Errorf("milvus result is missing field %s", field)
	}
	value, err := col.GetAsInt64(row)
	if err != nil {
		return 0, fmt.Errorf("read milvus field %s row %d: %w", field, row, err)
	}
	return value, nil
}

func ragMilvusSchema(dims int) *entity.Schema {
	return entity.NewSchema().WithDynamicFieldEnabled(false).
		WithField(entity.NewField().WithName(milvusFieldID).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(128).
			WithIsPrimaryKey(true).WithIsAutoID(false)).
		WithField(entity.NewField().WithName(milvusFieldDocID).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(128)).
		WithField(entity.NewField().WithName(milvusFieldChunkIndex).
			WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(milvusFieldSectionTitle).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(512)).
		WithField(entity.NewField().WithName(milvusFieldPageNum).
			WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(milvusFieldDocVersion).
			WithDataType(entity.FieldTypeInt64)).
		WithField(entity.NewField().WithName(milvusFieldContent).
			WithDataType(entity.FieldTypeVarChar).WithMaxLength(65535).
			WithEnableAnalyzer(true).
			WithAnalyzerParams(map[string]any{"type": "chinese"})).
		WithField(entity.NewField().WithName(milvusFieldSparse).
			WithDataType(entity.FieldTypeSparseVector)).
		WithField(entity.NewField().WithName(milvusFieldEmbedding).
			WithDataType(entity.FieldTypeFloatVector).WithDim(int64(dims))).
		WithFunction(entity.NewFunction().
			WithName("content_bm25").
			WithInputFields(milvusFieldContent).
			WithOutputFields(milvusFieldSparse).
			WithType(entity.FunctionTypeBM25))
}

func ragCollectionName(kbID string) string {
	var b strings.Builder
	b.Grow(len(kbID) + len("rag_"))
	b.WriteString("rag_")
	for _, r := range kbID {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func milvusChunkID(chunk ChunkData) string {
	return fmt.Sprintf("%s_%d_%d", chunk.DocID, chunk.DocVersion, chunk.Index)
}

func escapeMilvusExprString(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

var _ Store = (*Milvus)(nil)
