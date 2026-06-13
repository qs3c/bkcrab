package channels

import (
	"context"
	"log/slog"
	"time"
)

// Leaser 是 Manager 用来门控轮询/持久连接适配器（微信、Telegram、
// Discord、Slack、飞书长连接）的跨进程单例原语。没有 leaser 时，共享
// 同一 bot token 的两个副本都会对上游进行长轮询，用户会收到双重复回复。
//
// Acquire 在调用方（由 holderID 标识）成为 (channel, accountID) 的
// 租约持有者时返回 true；false 表示另一个活跃持有者拥有它。Renew 在
// 租约丢失时返回 false——调用方必须立即停止轮询。Release 主动释放行，
// 以便对等方无需等待 TTL 即可接管。
type Leaser interface {
	Acquire(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, channel, accountID, holderID string) error
}

// 租约生命周期常量。TTL 是租约持有者写入 channel_leases.expires_at 的
// 新鲜度窗口；续约在 TTL 的 1/3 处触发，以便两次连续续约失败仍有时间
// 让第三次在行过期之前完成。重试是对等方在等待活跃持有者死亡时重新
// 尝试 Acquire 的频率。
const (
	leaseTTL           = 30 * time.Second
	leaseRenewInterval = 10 * time.Second
	leaseRetryInterval = 10 * time.Second
)

// NopLeaser 是无单例实现：每次 Acquire 都成功，Renew 总是成功，
// Release 是空操作。用于测试和未接 leaser 的安装（单实例回退）。
// 可安全共享——所有方法都是无状态的。
type NopLeaser struct{}

func (NopLeaser) Acquire(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (NopLeaser) Renew(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (NopLeaser) Release(context.Context, string, string, string) error { return nil }

// runWithLease 将 Channel 的 Start 包装在跨实例单例门控中。生命周期：
//
//  1. 尝试 Acquire (channel, accountID) 租约。如果另一个实例持有它，
//     则休眠并重试，直到 ctx 结束或我们赢得租约。
//  2. 持有时，启动 ch.Start(childCtx) 和续约计时器。
//     续约失败（租约丢失或数据库错误）会取消 childCtx——Start goroutine
//     退出，我们在本地 Release（尽力），然后回到步骤 1。
//  3. 父 ctx 取消时，我们取消 Start goroutine，等待它返回，
//     然后在退出前 Release。
//
// `holderID` 是每进程实例标识符——通常在 Manager 构造时生成的一次 UUID。
// 它必须在同一进程的续约之间保持稳定，否则 RenewChannelLease 会在每次
// tick 上返回 false。
func runWithLease(ctx context.Context, ch Channel, leaser Leaser, holderID string) {
	chName := ch.Name()
	accountID := ch.AccountID()
	logCtx := []any{"channel", chName, "account", accountID, "holder", holderID}

	for {
		if ctx.Err() != nil {
			return
		}
		ok, err := leaser.Acquire(ctx, chName, accountID, holderID, leaseTTL)
		if err != nil {
			slog.Warn("channel lease acquire failed, retrying", append(logCtx, "error", err)...)
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
			continue
		}
		if !ok {
			// 另一个活跃实例持有租约——安静重试。我们故意不在每 tick
			// 处记录日志，以避免永远刷屏待机副本的日志；持有者在获取时
			// 记录自己的 "starting channel" 一次。
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
			continue
		}

		slog.Info("channel lease acquired, starting adapter", logCtx...)
		childCtx, childCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := ch.Start(childCtx); err != nil {
				slog.Error("channel stopped with error", append(logCtx, "error", err)...)
			}
		}()
		renewExit := renewUntilLost(childCtx, leaser, chName, accountID, holderID)

		// 等待以下任一情况：父 ctx 结束、适配器自行退出（例如微信 token 过期），
		// 或续约循环报告租约丢失/出错。
		select {
		case <-ctx.Done():
			childCancel()
			<-done
			// 优雅关闭时尽力释放，以便对等方可以在几秒内接管而不必等待 TTL。
			if err := leaser.Release(context.Background(), chName, accountID, holderID); err != nil {
				slog.Debug("channel lease release failed on shutdown", append(logCtx, "error", err)...)
			}
			return
		case <-done:
			// 适配器自行退出。释放租约以便对等方可以接管（或者如果退出是暂时性的，
			// 下一次循环迭代会重新获取）。
			childCancel()
			<-renewExit
			if err := leaser.Release(context.Background(), chName, accountID, holderID); err != nil {
				slog.Debug("channel lease release failed after adapter exit", append(logCtx, "error", err)...)
			}
			// 进入 for 循环的下一次迭代：下一次迭代尝试重新获取并重启。
			// 如果适配器因永久条件退出（wechat onExpired 回调从管理器
			// 注销渠道），管理器最终会通过 ctx 拆除此 goroutine。
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
		case <-renewExit:
			// 续约失败：要么 DB 拒绝了我们（对等方窃取了租约——在健康 TTL 下
			// 应该不可能，但无论如何都要防护），要么瞬态 DB 错误消耗了
			// 完整的 TTL。无论哪种情况，停止适配器并重新循环。
			slog.Warn("channel lease lost, stopping adapter", logCtx...)
			childCancel()
			<-done
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
		}
	}
}

// renewUntilLost 每 leaseRenewInterval 触发一次，直到 ctx 结束
// （父进程告诉我们停止）或 Renew 报告租约丢失。返回的 channel 在
// goroutine 退出时关闭——调用方在其上 select 以了解何时拆下适配器。
func renewUntilLost(ctx context.Context, leaser Leaser, channel, accountID, holderID string) <-chan struct{} {
	exit := make(chan struct{})
	go func() {
		defer close(exit)
		ticker := time.NewTicker(leaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := leaser.Renew(ctx, channel, accountID, holderID, leaseTTL)
				if err != nil {
					// 瞬态 DB 错误：记录并继续尝试。租约在 leaseTTL 内没有成功续约会过期
				// ——届时对等方窃取，我们的下一次 Renew 返回 ok=false，进入下面的
				// 退出分支。
					slog.Warn("channel lease renew error",
						"channel", channel, "account", accountID, "error", err)
					continue
				}
				if !ok {
					return
				}
			}
		}
	}()
	return exit
}

// sleepOrDone 在完整休眠后返回 true，如果 ctx 先结束则返回 false。
// 让调用方可以干净地退出重试循环，而不必每次都写 select 模板。
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
