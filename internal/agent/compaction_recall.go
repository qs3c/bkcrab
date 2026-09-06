package agent

import (
	"context"
	"crypto/sha256"
	"log/slog"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

// toolRefStore 提供摘要标注 msg_ref 所需的索引，以及重复 ID 的原文核验。
type toolRefStore interface {
	ListSessionToolRefs(ctx context.Context, userID, agentID, sessionKey, chatterUserID string) ([]store.ToolMsgRef, error)
	GetSessionToolMessage(ctx context.Context, userID, agentID, sessionKey, chatterUserID string, seq int64) (*store.SessionToolMessage, error)
}

// toolSeqIndex 保留一个 tool_call_id 的所有候选 seq。工作集可能只剩归档的
// 一部分，不能按 ID 的出现次序或“最新一条”猜测某条结果属于哪个 seq。
type toolSeqIndex map[string]*toolSeqCandidates

type toolSeqCandidates struct {
	seqs     []int64
	resolved bool
	byResult map[toolResultIdentity]int64
}

// 只缓存原文摘要，避免为重复 ID 长期持有多份大工具输出。
type toolResultIdentity struct {
	name          string
	contentSHA256 [sha256.Size]byte
}

// unknownToolSeq 表示"这条工具消息查不到 seq",摘要因此不写 msg_ref 行。
// 用 -1 而不是 0:seq 从 0 开始计数,0 是会话第一条消息的合法门牌号。
const unknownToolSeq int64 = -1

// buildToolSeqIndex 在一次压缩开始时建一次索引。查询失败不阻断压缩——
// 代价只是这一轮的摘要少一行 msg_ref,上下文照常被压下去。
//
// chatterUserID 传空:压缩要为它手上工作集里的每条工具消息标注门牌号,而
// 工作集本来就不区分发言人。聊天者隔离属于读取侧(recall_tool_result),
// 在这里施加只会让群聊里一部分摘要莫名其妙缺 msg_ref。
func buildToolSeqIndex(opts CompactOptions) toolSeqIndex {
	if opts.ToolRefStore == nil ||
		opts.RecallUserID == "" || opts.RecallAgentID == "" || opts.RecallSessionKey == "" {
		return nil
	}
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	refs, err := opts.ToolRefStore.ListSessionToolRefs(
		ctx, opts.RecallUserID, opts.RecallAgentID, opts.RecallSessionKey, "")
	if err != nil {
		slog.Warn("failed to index tool message seqs for recall", "error", err)
		return nil
	}
	idx := make(toolSeqIndex, len(refs))
	for _, ref := range refs {
		if ref.ToolCallID == "" {
			continue
		}
		if idx[ref.ToolCallID] == nil {
			idx[ref.ToolCallID] = &toolSeqCandidates{}
		}
		entry := idx[ref.ToolCallID]
		entry.seqs = append(entry.seqs, ref.Seq)
	}
	return idx
}

// seqFor 对唯一 ID 直接返回 seq；重复 ID 只在首次实际裁剪时读取候选原文，
// 按工具名和内容匹配。查不到、读取失败或内容仍有歧义时不发布回溯编号。
func (idx toolSeqIndex) seqFor(msg provider.Message, opts CompactOptions) int64 {
	entry := idx[msg.ToolCallID]
	if entry == nil {
		return unknownToolSeq
	}
	if len(entry.seqs) == 1 {
		return entry.seqs[0]
	}
	if !entry.resolved {
		entry.resolved = true
		entry.byResult = entry.resolveOriginals(opts)
	}
	key := toolResultIdentity{name: msg.Name, contentSHA256: sha256.Sum256([]byte(msg.Content))}
	seq, ok := entry.byResult[key]
	if !ok {
		return unknownToolSeq
	}
	return seq
}

func (entry *toolSeqCandidates) resolveOriginals(opts CompactOptions) map[toolResultIdentity]int64 {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	byResult := make(map[toolResultIdentity]int64, len(entry.seqs))
	for _, seq := range entry.seqs {
		rec, err := opts.ToolRefStore.GetSessionToolMessage(ctx,
			opts.RecallUserID, opts.RecallAgentID, opts.RecallSessionKey, "", seq)
		if err != nil {
			slog.Warn("failed to resolve duplicate tool message refs", "error", err)
			// 未读完所有候选，就不能保证已经找到的匹配是唯一的。
			return nil
		}
		key := toolResultIdentity{name: rec.Name, contentSHA256: sha256.Sum256([]byte(rec.Content))}
		if _, exists := byResult[key]; exists {
			byResult[key] = unknownToolSeq
		} else {
			byResult[key] = rec.Seq
		}
	}
	return byResult
}
