package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ChannelInbox persists ingress until it has been handed to the message bus.
// Completion is NOT an acknowledgement of agent execution or outbound delivery.
type ChannelInbox interface {
	SaveChannelInbox(context.Context, string, string, string) error
	ClaimChannelInbox(context.Context, string, time.Duration) (*ChannelInboxRecord, error)
	CompleteChannelInbox(context.Context, string, string) error
	PruneChannelInbox(context.Context, string, time.Time) error
}

type ChannelInboxRecord struct {
	ID, Payload, ClaimToken string
}

func channelInboxTableSQL(dialect string) string {
	payload := "TEXT"
	index := ""
	suffix := ""
	if dialect == mysqlDialect {
		payload = "MEDIUMTEXT"
		index = ", KEY idx_channel_inbox_pending (account_id, done_ms, available_ms)"
		suffix = " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
	}
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS channel_inbox (
		id VARCHAR(64) PRIMARY KEY,
		account_id VARCHAR(128) NOT NULL,
		payload %s NOT NULL,
		created_ms BIGINT NOT NULL,
		available_ms BIGINT NOT NULL DEFAULT 0,
		claim_token VARCHAR(64) NOT NULL DEFAULT '',
		done_ms BIGINT NOT NULL DEFAULT 0%s
	)%s`, payload, index, suffix)
}

func (d *DBStore) SaveChannelInbox(ctx context.Context, account, messageID, payload string) error {
	sum := sha256.Sum256([]byte(account + "\x00" + messageID))
	id := hex.EncodeToString(sum[:])
	q := fmt.Sprintf(`INSERT INTO channel_inbox (id, account_id, payload, created_ms) VALUES (%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	if d.dialect == mysqlDialect {
		q += ` ON DUPLICATE KEY UPDATE id=id`
	} else {
		q += ` ON CONFLICT (id) DO NOTHING`
	}
	_, err := d.db.ExecContext(ctx, q, id, account, payload, time.Now().UnixMilli())
	return err
}

func (d *DBStore) ClaimChannelInbox(ctx context.Context, account string, ttl time.Duration) (*ChannelInboxRecord, error) {
	now := time.Now().UnixMilli()
	var rec ChannelInboxRecord
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id, payload FROM channel_inbox WHERE account_id=%s AND done_ms=0 AND available_ms<=%s ORDER BY created_ms,id LIMIT 1`, d.ph(1), d.ph(2)), account, now).Scan(&rec.ID, &rec.Payload)
	if err != nil {
		return nil, err
	}
	rec.ClaimToken = uuid.NewString()
	res, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE channel_inbox SET claim_token=%s, available_ms=%s WHERE id=%s AND done_ms=0 AND available_ms<=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), rec.ClaimToken, now+ttl.Milliseconds(), rec.ID, now)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrNotFound
	}
	return &rec, nil
}

func (d *DBStore) CompleteChannelInbox(ctx context.Context, id, claimToken string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE channel_inbox SET done_ms=%s, payload='' WHERE id=%s AND claim_token=%s AND done_ms=0`, d.ph(1), d.ph(2), d.ph(3)), time.Now().UnixMilli(), id, claimToken)
	return err
}

func (d *DBStore) PruneChannelInbox(ctx context.Context, account string, before time.Time) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM channel_inbox WHERE account_id=%s AND done_ms>0 AND done_ms<%s`, d.ph(1), d.ph(2)), account, before.UnixMilli())
	return err
}
