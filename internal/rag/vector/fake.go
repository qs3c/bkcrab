package vector

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"
)

const fakeRRFK = 60

// Fake is an in-memory Store implementation intended for unit tests. It uses
// cosine similarity for the dense route and a deliberately small token scorer
// for the text route, then combines both rankings with RRF just like the
// production contract requires.
type Fake struct {
	mu          sync.RWMutex
	collections map[string]*fakeCollection
}

type fakeCollection struct {
	dims    int
	entries map[fakeEntryKey]ChunkData
	ops     []string
}

type fakeEntryKey struct {
	docID   string
	index   int
	version int
}

type fakeRankedChunk struct {
	key   fakeEntryKey
	chunk ChunkData
	score float64
}

// NewFake returns an empty, concurrency-safe in-memory vector store.
func NewFake() *Fake {
	return &Fake{collections: make(map[string]*fakeCollection)}
}

func (f *Fake) EnsureCollection(ctx context.Context, kbID string, dims int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if kbID == "" {
		return fmt.Errorf("collection id 不能为空")
	}
	if dims <= 0 {
		return fmt.Errorf("collection %s 的向量维度必须大于 0", kbID)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.collections == nil {
		f.collections = make(map[string]*fakeCollection)
	}
	if existing, ok := f.collections[kbID]; ok {
		if existing.dims != dims {
			return fmt.Errorf("collection %s 已存在，维度为 %d，不能改为 %d", kbID, existing.dims, dims)
		}
		return nil
	}
	f.collections[kbID] = &fakeCollection{
		dims:    dims,
		entries: make(map[fakeEntryKey]ChunkData),
	}
	return nil
}

func (f *Fake) UpsertChunks(ctx context.Context, kbID string, chunks []ChunkData) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.collectionLocked(kbID)
	if err != nil {
		return err
	}

	// Validate the whole batch first so a bad vector cannot leave a partial
	// upsert behind.
	for i, chunk := range chunks {
		if len(chunk.Vector) != c.dims {
			return fmt.Errorf("collection %s: chunk %d 的向量维度为 %d，期望 %d", kbID, i, len(chunk.Vector), c.dims)
		}
	}

	versions := make([]int, 0, 1)
	seenVersion := make(map[int]struct{})
	for _, chunk := range chunks {
		key := fakeEntryKey{docID: chunk.DocID, index: chunk.Index, version: chunk.DocVersion}
		c.entries[key] = cloneChunk(chunk)
		if _, seen := seenVersion[chunk.DocVersion]; !seen {
			seenVersion[chunk.DocVersion] = struct{}{}
			versions = append(versions, chunk.DocVersion)
		}
	}
	for _, version := range versions {
		c.ops = append(c.ops, fmt.Sprintf("upsert_v%d", version))
	}
	return nil
}

func (f *Fake) DeleteOldVersions(ctx context.Context, kbID, docID string, keepVersion int) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.collectionLocked(kbID)
	if err != nil {
		return err
	}
	for key := range c.entries {
		if key.docID == docID && key.version < keepVersion {
			delete(c.entries, key)
		}
	}
	c.ops = append(c.ops, fmt.Sprintf("delete_old_v%d", keepVersion))
	return nil
}

func (f *Fake) DeleteDoc(ctx context.Context, kbID, docID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.collectionLocked(kbID)
	if err != nil {
		return err
	}
	for key := range c.entries {
		if key.docID == docID {
			delete(c.entries, key)
		}
	}
	return nil
}

func (f *Fake) DropCollection(ctx context.Context, kbID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.collectionLocked(kbID); err != nil {
		return err
	}
	delete(f.collections, kbID)
	return nil
}

func (f *Fake) HybridSearch(ctx context.Context, kbID string, queryVec []float32, queryText string, topK int) ([]SearchHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return []SearchHit{}, nil
	}

	f.mu.RLock()
	c, err := f.collectionLocked(kbID)
	if err != nil {
		f.mu.RUnlock()
		return nil, err
	}
	if len(queryVec) != c.dims {
		f.mu.RUnlock()
		return nil, fmt.Errorf("collection %s: 查询向量维度为 %d，期望 %d", kbID, len(queryVec), c.dims)
	}
	entries := make([]fakeRankedChunk, 0, len(c.entries))
	for key, chunk := range c.entries {
		entries = append(entries, fakeRankedChunk{key: key, chunk: cloneChunk(chunk)})
	}
	f.mu.RUnlock()

	dense := rankDense(entries, queryVec, topK)
	routes := [][]fakeRankedChunk{dense}
	if terms := queryTerms(queryText); len(terms) > 0 {
		routes = append(routes, rankText(entries, terms, topK))
	}

	fused := make(map[fakeEntryKey]fakeRankedChunk)
	for _, route := range routes {
		for rank, item := range route {
			current, ok := fused[item.key]
			if !ok {
				current.key = item.key
				current.chunk = item.chunk
			}
			current.score += 1.0 / float64(fakeRRFK+rank+1)
			fused[item.key] = current
		}
	}

	merged := make([]fakeRankedChunk, 0, len(fused))
	for _, item := range fused {
		merged = append(merged, item)
	}
	sortRanked(merged)
	if len(merged) > topK {
		merged = merged[:topK]
	}

	hits := make([]SearchHit, 0, len(merged))
	for _, item := range merged {
		hits = append(hits, SearchHit{
			DocID:        item.chunk.DocID,
			ChunkIndex:   item.chunk.Index,
			Content:      item.chunk.Content,
			SectionTitle: item.chunk.SectionTitle,
			PageNum:      item.chunk.PageNum,
			Score:        item.score,
		})
	}
	return hits, nil
}

// HasCollection reports whether kbID has been ensured. It is a test helper for
// service-level lifecycle assertions.
func (f *Fake) HasCollection(kbID string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.collections[kbID]
	return ok
}

// Count returns the number of indexed entities in kbID. Missing collections
// count as zero so test cleanup assertions remain concise.
func (f *Fake) Count(kbID string) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c, ok := f.collections[kbID]
	if !ok {
		return 0
	}
	return len(c.entries)
}

// Ops returns a copy of the write-order log for kbID. Upserts are recorded as
// "upsert_vN" and version cleanup as "delete_old_vN".
func (f *Fake) Ops(kbID string) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	c, ok := f.collections[kbID]
	if !ok {
		return nil
	}
	return append([]string(nil), c.ops...)
}

func (f *Fake) collectionLocked(kbID string) (*fakeCollection, error) {
	c, ok := f.collections[kbID]
	if !ok {
		return nil, fmt.Errorf("collection %s 不存在", kbID)
	}
	return c, nil
}

func cloneChunk(chunk ChunkData) ChunkData {
	chunk.Vector = append([]float32(nil), chunk.Vector...)
	return chunk
}

func rankDense(entries []fakeRankedChunk, query []float32, limit int) []fakeRankedChunk {
	ranked := make([]fakeRankedChunk, len(entries))
	copy(ranked, entries)
	for i := range ranked {
		ranked[i].score = cosine(query, ranked[i].chunk.Vector)
	}
	sortRanked(ranked)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

func rankText(entries []fakeRankedChunk, terms []string, limit int) []fakeRankedChunk {
	ranked := make([]fakeRankedChunk, 0, len(entries))
	for _, item := range entries {
		content := strings.ToLower(item.chunk.Content)
		score := 0
		for _, term := range terms {
			score += strings.Count(content, term)
		}
		if score == 0 {
			continue
		}
		item.score = float64(score)
		ranked = append(ranked, item)
	}
	sortRanked(ranked)
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
}

func sortRanked(ranked []fakeRankedChunk) {
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].key.docID != ranked[j].key.docID {
			return ranked[i].key.docID < ranked[j].key.docID
		}
		if ranked[i].key.version != ranked[j].key.version {
			return ranked[i].key.version > ranked[j].key.version
		}
		return ranked[i].key.index < ranked[j].key.index
	})
}

func cosine(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		normA += av * av
		normB += bv * bv
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// queryTerms emits adjacent Han 2-grams and lower-cased non-Han words. A
// one-character Han run is retained so short Chinese searches still work.
func queryTerms(query string) []string {
	query = strings.ToLower(query)
	var terms []string
	var hanRun, wordRun []rune
	flushHan := func() {
		switch len(hanRun) {
		case 0:
		case 1:
			terms = append(terms, string(hanRun))
		default:
			for i := 0; i+1 < len(hanRun); i++ {
				terms = append(terms, string(hanRun[i:i+2]))
			}
		}
		hanRun = hanRun[:0]
	}
	flushWord := func() {
		if len(wordRun) > 0 {
			terms = append(terms, string(wordRun))
			wordRun = wordRun[:0]
		}
	}

	for _, r := range query {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			hanRun = append(hanRun, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			wordRun = append(wordRun, r)
		default:
			flushHan()
			flushWord()
		}
	}
	flushHan()
	flushWord()
	return terms
}

var _ Store = (*Fake)(nil)
