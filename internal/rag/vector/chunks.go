package vector

import (
	"context"
	"fmt"
	"sort"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/entity"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
)

// GetChunks returns exact current-version chunks from the in-memory test
// store. The returned order is stable across both Store implementations.
func (f *Fake) GetChunks(ctx context.Context, kbID string, refs []ChunkRef) ([]ChunkData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return []ChunkData{}, nil
	}

	f.mu.RLock()
	c, err := f.collectionLocked(kbID)
	if err != nil {
		f.mu.RUnlock()
		return nil, err
	}
	chunks := make([]ChunkData, 0, len(refs))
	for _, ref := range refs {
		key := fakeEntryKey{docID: ref.DocID, index: ref.Index, version: ref.DocVersion}
		if chunk, ok := c.entries[key]; ok {
			chunks = append(chunks, cloneChunk(chunk))
		}
	}
	f.mu.RUnlock()
	sortChunkData(chunks)
	return chunks, nil
}

// GetChunks retrieves exact primary keys rather than issuing an ANN search.
// milvusChunkID includes document version, so a reindex racing this request
// cannot silently substitute stale or newer content.
func (m *Milvus) GetChunks(ctx context.Context, kbID string, refs []ChunkRef) ([]ChunkData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return []ChunkData{}, nil
	}

	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, milvusChunkID(ChunkData{
			DocID: ref.DocID, Index: ref.Index, DocVersion: ref.DocVersion,
		}))
	}
	name := ragCollectionName(kbID)
	result, err := m.client.Query(ctx, milvusclient.NewQueryOption(name).
		WithIDs(column.NewColumnVarChar(milvusFieldID, ids)).
		WithOutputFields(
			milvusFieldDocID,
			milvusFieldChunkIndex,
			milvusFieldContent,
			milvusFieldSectionTitle,
			milvusFieldPageNum,
			milvusFieldDocVersion,
		).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, fmt.Errorf("query chunks from milvus collection %s: %w", name, err)
	}
	if result.Err != nil {
		return nil, fmt.Errorf("decode chunks from milvus collection %s: %w", name, result.Err)
	}

	chunks := make([]ChunkData, 0, result.ResultCount)
	for row := 0; row < result.ResultCount; row++ {
		docID, err := milvusStringField(&result, milvusFieldDocID, row)
		if err != nil {
			return nil, err
		}
		chunkIndex, err := milvusInt64Field(&result, milvusFieldChunkIndex, row)
		if err != nil {
			return nil, err
		}
		content, err := milvusStringField(&result, milvusFieldContent, row)
		if err != nil {
			return nil, err
		}
		sectionTitle, err := milvusStringField(&result, milvusFieldSectionTitle, row)
		if err != nil {
			return nil, err
		}
		pageNum, err := milvusInt64Field(&result, milvusFieldPageNum, row)
		if err != nil {
			return nil, err
		}
		docVersion, err := milvusInt64Field(&result, milvusFieldDocVersion, row)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, ChunkData{
			DocID:        docID,
			Index:        int(chunkIndex),
			Content:      content,
			SectionTitle: sectionTitle,
			PageNum:      int(pageNum),
			DocVersion:   int(docVersion),
		})
	}
	sortChunkData(chunks)
	return chunks, nil
}

func sortChunkData(chunks []ChunkData) {
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].DocID != chunks[j].DocID {
			return chunks[i].DocID < chunks[j].DocID
		}
		if chunks[i].DocVersion != chunks[j].DocVersion {
			return chunks[i].DocVersion < chunks[j].DocVersion
		}
		return chunks[i].Index < chunks[j].Index
	})
}
