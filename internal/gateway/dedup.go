package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/qs3c/bkclaw/internal/bus"
)

const dedupTTL = 60 * time.Second

// dedupEntry 记录消息首次被看到的时间。
type dedupEntry struct {
	seenAt time.Time
}

// isDuplicate 报告此入站消息是否已在 dedupTTL 窗口内被看到。
// 对于没有可用去重键的消息（web/api/webhook 调用者，MessageID 为空且不是群组 PeerKind），
// 返回 false（= "处理它"）。
//
// 两种不同的键策略，因为失败模式不同：
//
//   - 群组：Telegram 超级群组将相同的逻辑消息以*不同*的 message_id 传递给每个 bot，
//     因此 message_id 无法跨 bot 副本去重。改为使用 (channel, chatID, userID, text-hash) 作为键 —
//     同一个说话者在同一个房间 60 秒内说相同的内容，绝大多数是重新投递，而不是真正的重复。
//   - DM：每个支持的 IM 通道（微信/Telegram/Discord/LINE/飞书/Slack）都会发出稳定的
//     每会话 message_id。使用 (channel, accountID, messageID) 作为键，这样相同的入站消息
//     被两次推送到总线上 — 由孤立的轮询器、多副本租约竞争或 IM 服务器重试导致 —
//     第二个副本在此丢弃，而不是向下游扩散到重复的 LLM 回复。
//
// 去重是每个进程的内存操作（sync.Map）。跨副本协调仍依赖于 channel_leases TTL，
// 确保一次只有一个轮询适配器存活；如果出现漂移（时钟偏差、续租挂起），
// 每个副本仍然抑制自己的重复拉取，但两个副本可能各自发出一个回复。
// 共享存储（Redis/DB 在 (channel, accountID, messageID) 上唯一）是适当的修复方式 —
// 这超出了本函数的作用域。
func (g *Gateway) isDuplicate(msg bus.InboundMessage) bool {
	if msg.PeerKind == "group" {
		key := fmt.Sprintf("group:%s:%s:%s:%x", msg.Channel, msg.ChatID, msg.UserID, hashString(msg.Text))
		_, loaded := g.dedup.LoadOrStore(key, dedupEntry{seenAt: time.Now()})
		return loaded
	}
	// DM 路径。某些入站来源没有稳定的 MessageID
	//（web SSE 轮次、/api/chat 完成、webhook 扇出）：没有可用的键，直接放行 —
	// 这些路径没有 IM 通道所具有的重复轮询故障模式。
	if msg.MessageID == "" {
		return false
	}
	key := fmt.Sprintf("dm:%s:%s:%s", msg.Channel, msg.AccountID, msg.MessageID)
	_, loaded := g.dedup.LoadOrStore(key, dedupEntry{seenAt: time.Now()})
	return loaded
}

// cleanupDedup 定期从去重缓存中移除过期的条目。
func (g *Gateway) cleanupDedup(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			g.dedup.Range(func(key, value any) bool {
				entry := value.(dedupEntry)
				if now.Sub(entry.seenAt) > dedupTTL {
					g.dedup.Delete(key)
				}
				return true
			})
		}
	}
}

func hashString(s string) uint32 {
	var h uint32
	for _, c := range s {
		h = h*31 + uint32(c)
	}
	return h
}
