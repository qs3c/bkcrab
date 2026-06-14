// Package usage 记录每(天、用户、代理、会话、模型)的 LLM token 消耗，
// 并为管理员提供"谁消耗了多少"的查询。
//
// 数据单向流动：每次成功的 provider.Chat / ChatStream 调用
// 触发一次 RecordTokens()；管理员仪表板通过 Top* / Totals
// 读取聚合结果。
//
// 存在两种实现：
//   - MemMeter：进程内 map。轻量，重启后丢失状态。
//     适用于单元测试和独立开发运行。
//   - SQLMeter：对 Store 使用的同一数据库进行 UPSERT 到 token_usage_daily。
//     生产环境的管理员端点读取的就是它。
//
// 空的 user_id（管理员拥有的/由 cron 触发的代理）在写入时保留，
// 处理程序在输出时将其渲染为"system"。
package usage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tokens 是一次 Chat 调用的 token 记账。镜像 provider.Usage，
// 但放在此处以使 usage 包不依赖于 provider。
// RequestCount 每次调用固定为 1；由 meter 累加。
type Tokens struct {
	Input         int
	Output        int
	CacheRead     int
	CacheCreation int
}

// Total 返回 input + output + cache（模型计费的所有内容）。
func (t Tokens) Total() int64 {
	return int64(t.Input) + int64(t.Output) + int64(t.CacheRead) + int64(t.CacheCreation)
}

// Range 是用于查询的 UTC 半开天窗口。两端在天粒度上都是包含的
//（meter 每天存储一行）。
type Range struct {
	Since time.Time // first day to include (UTC, day-truncated)
	Until time.Time // last day to include  (UTC, day-truncated)
}

// LastN 返回 [今天-(n-1) … 今天] 范围，调用者可以查询
// "最近 1/7/30 天"而无需考虑时区。
func LastN(n int) Range {
	today := dayBucket(time.Now())
	return Range{Since: today.AddDate(0, 0, -(n - 1)), Until: today}
}

// Totals 是某个范围内的概要数字：每种类型一行，加上窗口内的
// 总 request_count。
type Totals struct {
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CacheRead     int64 `json:"cacheReadTokens"`
	CacheCreation int64 `json:"cacheCreationTokens"`
	Requests      int64 `json:"requestCount"`
}

// Rank 是按代理或按用户排行榜中的一行。
type Rank struct {
	Key      string `json:"key"`    // agent_id or user_id ("" → "system" on render)
	Tokens   int64  `json:"tokens"` // input+output+cache combined
	Input    int64  `json:"inputTokens"`
	Output   int64  `json:"outputTokens"`
	Requests int64  `json:"requestCount"`
}

// Meter 是记录 + 回读接口。
type Meter interface {
	// RecordTokens 将一次 Chat 调用的 token 计数累加到
	// (today, userID, agentID, sessionKey, provider, model) 桶中。
	// provider 是按代理覆盖的键（例如 "anthropic-messages"）
	// 或当代理使用共享 provider 时为 ""；model 是不带前缀的裸模型 ID。
	// 将两者分开，以便仪表板无需在 SQL 中解析 "<prov>/<model>" 字符串
	// 即可回答"按 provider 统计的 token"。零计数仍会增加 request_count，
	// 以便我们可以回答"多少次调用"。
	RecordTokens(ctx context.Context, userID, agentID, sessionKey, provider, model string, t Tokens) error
	// Totals 返回某个范围内的聚合 token 计数。
	Totals(ctx context.Context, r Range) (Totals, error)
	// TopAgents 返回按总 token 数排序的前 N 个代理。
	TopAgents(ctx context.Context, r Range, limit int) ([]Rank, error)
	// TopUsers 返回按总 token 数排序的前 N 个用户。
	TopUsers(ctx context.Context, r Range, limit int) ([]Rank, error)
	// SessionsForAgent 返回某个代理的按会话汇总的 token。
	// 支撑按代理的"Token Usage"标签页——所有者询问
	// "我的哪些聊天消耗最多"；表格就是答案。
	// 可选的 userID 将范围限定为一个聊天者（当代理是公共的
	// 且你只想看自己的会话时有用）；传入 "" 包含所有聊天者。
	SessionsForAgent(ctx context.Context, agentID, userID string, r Range, limit int) ([]Rank, error)
	Close() error
}

// dayBucket 将时间截断到 UTC 午夜。通过 LastN 间接导出；
// 测试可以通过辅助函数调用它。
func dayBucket(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// --------------------------------------------------------------------
// MemMeter
// --------------------------------------------------------------------

type memKey struct {
	day        time.Time
	userID     string
	agentID    string
	sessionKey string
	provider   string
	model      string
}

type memCell struct {
	input, output, cacheRead, cacheCreate int64
	requests                              int64
}

// MemMeter 将所有数据保存在 map 中。进程重启后丢失，
// 这对开发/测试可以接受，但对生产环境的管理员仪表板无用。
type MemMeter struct {
	mu   sync.Mutex
	data map[memKey]*memCell
}

func NewMemMeter() *MemMeter {
	return &MemMeter{data: make(map[memKey]*memCell)}
}

func (m *MemMeter) RecordTokens(_ context.Context, userID, agentID, sessionKey, provider, model string, t Tokens) error {
	k := memKey{
		day:        dayBucket(time.Now()),
		userID:     userID,
		agentID:    agentID,
		sessionKey: sessionKey,
		provider:   provider,
		model:      model,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.data[k]
	if !ok {
		c = &memCell{}
		m.data[k] = c
	}
	c.input += int64(t.Input)
	c.output += int64(t.Output)
	c.cacheRead += int64(t.CacheRead)
	c.cacheCreate += int64(t.CacheCreation)
	c.requests++
	return nil
}

func (m *MemMeter) Totals(_ context.Context, r Range) (Totals, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out Totals
	for k, c := range m.data {
		if !inRange(k.day, r) {
			continue
		}
		out.Input += c.input
		out.Output += c.output
		out.CacheRead += c.cacheRead
		out.CacheCreation += c.cacheCreate
		out.Requests += c.requests
	}
	return out, nil
}

func (m *MemMeter) TopAgents(_ context.Context, r Range, limit int) ([]Rank, error) {
	return m.rank(r, limit, func(k memKey) string { return k.agentID })
}

func (m *MemMeter) TopUsers(_ context.Context, r Range, limit int) ([]Rank, error) {
	return m.rank(r, limit, func(k memKey) string { return k.userID })
}

func (m *MemMeter) SessionsForAgent(_ context.Context, agentID, userID string, r Range, limit int) ([]Rank, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agg := map[string]*Rank{}
	for k, c := range m.data {
		if k.agentID != agentID {
			continue
		}
		if userID != "" && k.userID != userID {
			continue
		}
		if !inRange(k.day, r) {
			continue
		}
		row, ok := agg[k.sessionKey]
		if !ok {
			row = &Rank{Key: k.sessionKey}
			agg[k.sessionKey] = row
		}
		row.Input += c.input
		row.Output += c.output
		row.Tokens += c.input + c.output + c.cacheRead + c.cacheCreate
		row.Requests += c.requests
	}
	out := make([]Rank, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tokens > out[j].Tokens })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemMeter) rank(r Range, limit int, key func(memKey) string) ([]Rank, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agg := map[string]*Rank{}
	for k, c := range m.data {
		if !inRange(k.day, r) {
			continue
		}
		id := key(k)
		row, ok := agg[id]
		if !ok {
			row = &Rank{Key: id}
			agg[id] = row
		}
		row.Input += c.input
		row.Output += c.output
		row.Tokens += c.input + c.output + c.cacheRead + c.cacheCreate
		row.Requests += c.requests
	}
	out := make([]Rank, 0, len(agg))
	for _, v := range agg {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tokens > out[j].Tokens })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemMeter) Close() error { return nil }

func inRange(day time.Time, r Range) bool {
	if !r.Since.IsZero() && day.Before(r.Since) {
		return false
	}
	if !r.Until.IsZero() && day.After(r.Until) {
		return false
	}
	return true
}

// --------------------------------------------------------------------
// SQLMeter
// --------------------------------------------------------------------

// SQLMeter 使用 UPSERT 语义写入 token_usage_daily。
// 同时适用于 SQLite 和 Postgres——它们都支持
// `INSERT … ON CONFLICT (…) DO UPDATE SET …` 的相同列引用形式。
//
// 表结构由 store/database.go 的迁移块拥有（参见 migrateTokenUsageDaily）。
// SQLMeter 是上层的薄查询层。
type SQLMeter struct {
	db      *sql.DB
	dialect string // "mysql" | "postgres" | "sqlite"
}

// NewSQLMeter 包装一个已打开的 *sql.DB。调用者（网关启动时）提供
// 与 Store 相同的 db+dialect，以便共享连接池并遵守
// SetMaxOpenConns 的调优设置。
func NewSQLMeter(db *sql.DB, dialect string) *SQLMeter {
	return &SQLMeter{db: db, dialect: dialect}
}

func (s *SQLMeter) Close() error { return nil } // 连接池由 store 拥有

// placeholders 为 postgres 生成 $1,$2,…，为 sqlite 生成 ?,?,…。
func (s *SQLMeter) ph(i int) string {
	if s.dialect == "postgres" {
		return fmt.Sprintf("$%d", i)
	}
	return "?"
}

// rebind 将在 postgres 上运行时用 ? 占位符编写的查询重写为
// $1..$N。保持查询字符串可读。
func (s *SQLMeter) rebind(q string) string {
	if s.dialect != "postgres" {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// dayParam 返回要绑定到 day 列的值。SQLite 将 DATE 存储为
// TEXT 'YYYY-MM-DD'；Postgres 直接接受 time.Time。
func (s *SQLMeter) dayParam(t time.Time) any {
	if s.dialect == "sqlite" {
		return t.Format("2006-01-02")
	}
	return t
}

func (s *SQLMeter) RecordTokens(ctx context.Context, userID, agentID, sessionKey, provider, model string, t Tokens) error {
	day := s.dayParam(dayBucket(time.Now()))
	if s.dialect == "mysql" {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO token_usage_daily
				(day, user_id, agent_id, session_key, provider, model,
				 input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, request_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
			ON DUPLICATE KEY UPDATE
				input_tokens        = token_usage_daily.input_tokens        + VALUES(input_tokens),
				output_tokens       = token_usage_daily.output_tokens       + VALUES(output_tokens),
				cache_read_tokens   = token_usage_daily.cache_read_tokens   + VALUES(cache_read_tokens),
				cache_create_tokens = token_usage_daily.cache_create_tokens + VALUES(cache_create_tokens),
				request_count       = token_usage_daily.request_count + 1`,
			day, userID, agentID, sessionKey, provider, model,
			t.Input, t.Output, t.CacheRead, t.CacheCreation,
		)
		return err
	}
	// 两种方言都支持这个六列冲突目标和 EXCLUDED 引用。
	// 我们额外将 request_count 增加 1。
	q := s.rebind(`
		INSERT INTO token_usage_daily
			(day, user_id, agent_id, session_key, provider, model,
			 input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, request_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (day, user_id, agent_id, session_key, provider, model) DO UPDATE SET
			input_tokens         = token_usage_daily.input_tokens         + EXCLUDED.input_tokens,
			output_tokens        = token_usage_daily.output_tokens        + EXCLUDED.output_tokens,
			cache_read_tokens    = token_usage_daily.cache_read_tokens    + EXCLUDED.cache_read_tokens,
			cache_create_tokens  = token_usage_daily.cache_create_tokens  + EXCLUDED.cache_create_tokens,
			request_count        = token_usage_daily.request_count        + 1`)
	_, err := s.db.ExecContext(ctx, q,
		day, userID, agentID, sessionKey, provider, model,
		t.Input, t.Output, t.CacheRead, t.CacheCreation,
	)
	return err
}

func (s *SQLMeter) Totals(ctx context.Context, r Range) (Totals, error) {
	q := s.rebind(`
		SELECT
			COALESCE(SUM(input_tokens),0),
			COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(cache_create_tokens),0),
			COALESCE(SUM(request_count),0)
		FROM token_usage_daily
		WHERE day BETWEEN ? AND ?`)
	row := s.db.QueryRowContext(ctx, q, s.dayParam(r.Since), s.dayParam(r.Until))
	var out Totals
	if err := row.Scan(&out.Input, &out.Output, &out.CacheRead, &out.CacheCreation, &out.Requests); err != nil {
		return Totals{}, err
	}
	return out, nil
}

func (s *SQLMeter) TopAgents(ctx context.Context, r Range, limit int) ([]Rank, error) {
	return s.topBy(ctx, r, limit, "agent_id")
}

func (s *SQLMeter) TopUsers(ctx context.Context, r Range, limit int) ([]Rank, error) {
	return s.topBy(ctx, r, limit, "user_id")
}

func (s *SQLMeter) SessionsForAgent(ctx context.Context, agentID, userID string, r Range, limit int) ([]Rank, error) {
	if limit <= 0 {
		limit = 50
	}
	// userID 是可选的——为空时不约束它。查询的两个变体
	// 保持预处理语句形式简洁，而不是在 WHERE 中构建 NULL 检查。
	if userID == "" {
		q := s.rebind(`
			SELECT session_key AS key,
				COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_create_tokens),0) AS tokens,
				COALESCE(SUM(input_tokens),0)  AS input_tokens,
				COALESCE(SUM(output_tokens),0) AS output_tokens,
				COALESCE(SUM(request_count),0) AS requests
			FROM token_usage_daily
			WHERE agent_id = ? AND day BETWEEN ? AND ?
			GROUP BY session_key
			ORDER BY tokens DESC
			LIMIT ?`)
		return s.scanRanks(ctx, q, agentID, s.dayParam(r.Since), s.dayParam(r.Until), limit)
	}
	q := s.rebind(`
		SELECT session_key AS key,
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_create_tokens),0) AS tokens,
			COALESCE(SUM(input_tokens),0)  AS input_tokens,
			COALESCE(SUM(output_tokens),0) AS output_tokens,
			COALESCE(SUM(request_count),0) AS requests
		FROM token_usage_daily
		WHERE agent_id = ? AND user_id = ? AND day BETWEEN ? AND ?
		GROUP BY session_key
		ORDER BY tokens DESC
		LIMIT ?`)
	return s.scanRanks(ctx, q, agentID, userID, s.dayParam(r.Since), s.dayParam(r.Until), limit)
}

// scanRanks 是 SessionsForAgent 和 topBy 共享的行迭代器——
// 它们仅在 WHERE/GROUP BY 上不同，因此扫描样板代码被提取出来。
func (s *SQLMeter) scanRanks(ctx context.Context, q string, args ...any) ([]Rank, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rank
	for rows.Next() {
		var r Rank
		if err := rows.Scan(&r.Key, &r.Tokens, &r.Input, &r.Output, &r.Requests); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLMeter) topBy(ctx context.Context, r Range, limit int, col string) ([]Rank, error) {
	if limit <= 0 {
		limit = 20
	}
	// col 是来自 TopAgents/TopUsers 的硬编码常量——永远不是用户提供的
	// ——所以这里的拼接是安全的。
	q := s.rebind(`
		SELECT ` + col + ` AS key,
			COALESCE(SUM(input_tokens + output_tokens + cache_read_tokens + cache_create_tokens),0) AS tokens,
			COALESCE(SUM(input_tokens),0)  AS input_tokens,
			COALESCE(SUM(output_tokens),0) AS output_tokens,
			COALESCE(SUM(request_count),0) AS requests
		FROM token_usage_daily
		WHERE day BETWEEN ? AND ?
		GROUP BY ` + col + `
		ORDER BY tokens DESC
		LIMIT ?`)
	return s.scanRanks(ctx, q, s.dayParam(r.Since), s.dayParam(r.Until), limit)
}
