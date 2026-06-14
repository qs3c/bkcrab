package store

import (
	"context"
	"database/sql"
	"time"
)

// --- 频道租约 ---
//
// 轮询/持久连接频道适配器的跨进程单例门控。模式是每个 (channel, account_id)
// 一行；持有者将其 instanceID 写入 holder_id 并在周期性 tick 上续约 expires_at。
// 想要接管的对等方必须等到 expires_at 过去——此时相同的 upsert 查询原子性地
// 将该行旋转到新持有者。

// AcquireChannelLease 尝试获取 (channel, accountID) 的 `ttl` 租约。
// 仅当行不存在、已由 holderID 持有（续约）或已过期（抢占）时返回 true。
// 在竞争中失败的并发获取者得到 (false, nil)——不是错误。
func (d *DBStore) AcquireChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	expires := now.Add(ttl)
	if d.dialect == mysqlDialect {
		res, err := d.db.ExecContext(ctx,
			`INSERT INTO channel_leases (channel, account_id, holder_id, expires_at)
				VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  holder_id=IF(channel_leases.expires_at < ?, VALUES(holder_id), channel_leases.holder_id),
				  expires_at=IF(channel_leases.holder_id = VALUES(holder_id), VALUES(expires_at), channel_leases.expires_at)`,
			channel, accountID, holderID, expires, now)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	if d.dialect == "postgres" {
		// ON CONFLICT 仅在前持有者的租约已过期或我们已持有时（续约）更新行。
		// WHERE 子句至关重要——没有它，第二个实例会在其 INSERT 冲突的瞬间窃取租约。
		res, err := d.db.ExecContext(ctx,
			`INSERT INTO channel_leases (channel, account_id, holder_id, expires_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (channel, account_id) DO UPDATE
				SET holder_id = EXCLUDED.holder_id, expires_at = EXCLUDED.expires_at
				WHERE channel_leases.expires_at < $5 OR channel_leases.holder_id = $3`,
			channel, accountID, holderID, expires, now)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	// SQLite 路径：ON CONFLICT DO UPDATE ... WHERE 在 modernc.org/sqlite
	//（SQLite 3.24+）中支持。语义与上面的 PG 分支相同；占位符语法不同。
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO channel_leases (channel, account_id, holder_id, expires_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (channel, account_id) DO UPDATE
			SET holder_id = excluded.holder_id, expires_at = excluded.expires_at
			WHERE channel_leases.expires_at < ? OR channel_leases.holder_id = ?`,
		channel, accountID, holderID, expires, now, holderID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RenewChannelLease 扩展已持有的租约。当行的 holder_id 不再匹配时返回 false
// （不是错误）——意味着前持有者的 TTL 已过，并且在对等方在我们离线时接管了。
// 调用方必须将 false 视为"立即停止轮询"：对等方现在正在驱动此 (channel, account_id)
// 对的入站流量。
func (d *DBStore) RenewChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	expires := time.Now().Add(ttl)
	var res sql.Result
	var err error
	if d.dialect == "postgres" {
		res, err = d.db.ExecContext(ctx,
			`UPDATE channel_leases SET expires_at = $1
				WHERE channel = $2 AND account_id = $3 AND holder_id = $4`,
			expires, channel, accountID, holderID)
	} else {
		res, err = d.db.ExecContext(ctx,
			`UPDATE channel_leases SET expires_at = ?
				WHERE channel = ? AND account_id = ? AND holder_id = ?`,
			expires, channel, accountID, holderID)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseChannelLease 自愿放弃租约，以便对等方可以在其下一次获取尝试中
// 接手而无需等待 TTL。受 holder_id 限制，因此来自被驱逐实例的过时 Release
// 不会意外地使当前持有者的行失效。
func (d *DBStore) ReleaseChannelLease(ctx context.Context, channel, accountID, holderID string) error {
	var err error
	if d.dialect == "postgres" {
		_, err = d.db.ExecContext(ctx,
			`DELETE FROM channel_leases WHERE channel = $1 AND account_id = $2 AND holder_id = $3`,
			channel, accountID, holderID)
	} else {
		_, err = d.db.ExecContext(ctx,
			`DELETE FROM channel_leases WHERE channel = ? AND account_id = ? AND holder_id = ?`,
			channel, accountID, holderID)
	}
	return err
}
