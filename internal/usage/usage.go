// Package usage records LLM token consumption per (day, user, agent,
// session, model) and answers admin "who burned what" queries.
//
// The data flows in one direction: every successful provider.Chat /
// ChatStream call lands one RecordTokens() invocation; the admin
// dashboard reads aggregates back out via Top* / Totals.
//
// Two implementations exist:
//   - MemMeter: in-process map. Cheap, loses state on restart. Useful
//     for unit tests and stand-alone dev runs.
//   - SQLMeter: UPSERTs into token_usage_daily on the same DB the
//     Store uses. This is what the prod admin endpoint reads from.
//
// Empty user_id (admin-owned / cron-fired agents) is preserved on
// write — handlers render it as "system" on the way out.
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

// Tokens is one Chat call's token accounting. Mirrors provider.Usage
// but lives here so the usage package doesn't depend on provider.
// RequestCount is always 1 per call; the meter accumulates it.
type Tokens struct {
	Input         int
	Output        int
	CacheRead     int
	CacheCreation int
}

// Total returns input + output + cache (everything the model billed for).
func (t Tokens) Total() int64 {
	return int64(t.Input) + int64(t.Output) + int64(t.CacheRead) + int64(t.CacheCreation)
}

// Range is a UTC half-open day window used for queries. Both ends are
// inclusive at day granularity (the meter stores one row per day).
type Range struct {
	Since time.Time // first day to include (UTC, day-truncated)
	Until time.Time // last day to include  (UTC, day-truncated)
}

// LastN returns the [today-(n-1) … today] range so callers can ask for
// "last 1/7/30 days" without thinking about timezones.
func LastN(n int) Range {
	today := dayBucket(time.Now())
	return Range{Since: today.AddDate(0, 0, -(n - 1)), Until: today}
}

// Totals is the headline numbers for a range: one row per kind, plus
// total request_count across the window.
type Totals struct {
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CacheRead     int64 `json:"cacheReadTokens"`
	CacheCreation int64 `json:"cacheCreationTokens"`
	Requests      int64 `json:"requestCount"`
}

// Rank is one row of a per-agent or per-user leaderboard.
type Rank struct {
	Key      string `json:"key"`   // agent_id or user_id ("" → "system" on render)
	Tokens   int64  `json:"tokens"` // input+output+cache combined
	Input    int64  `json:"inputTokens"`
	Output   int64  `json:"outputTokens"`
	Requests int64  `json:"requestCount"`
}

// Meter is the recording + readback interface.
type Meter interface {
	// RecordTokens adds one Chat call's token counts onto the
	// (today, userID, agentID, sessionKey, provider, model) bucket.
	// provider is the per-agent override key (e.g.
	// "anthropic-messages") or "" when the agent uses the shared
	// provider; model is the bare model id with no prefix. Splitting
	// the two so the dashboard can answer "tokens by provider"
	// without parsing "<prov>/<model>" strings in SQL. Zero counts
	// still bump request_count so we can answer "how many calls".
	RecordTokens(ctx context.Context, userID, agentID, sessionKey, provider, model string, t Tokens) error
	// Totals returns the aggregate token counts for a range.
	Totals(ctx context.Context, r Range) (Totals, error)
	// TopAgents returns the top-N agents by total tokens.
	TopAgents(ctx context.Context, r Range, limit int) ([]Rank, error)
	// TopUsers returns the top-N users by total tokens.
	TopUsers(ctx context.Context, r Range, limit int) ([]Rank, error)
	// SessionsForAgent returns per-session token rollups for one
	// agent. Backs the per-agent "Token Usage" tab — owner asks
	// "which of my chats burned the most"; the table is the answer.
	// Optional userID scopes to one chatter (useful when the agent
	// is public and you only want your own sessions); pass "" to
	// include all chatters.
	SessionsForAgent(ctx context.Context, agentID, userID string, r Range, limit int) ([]Rank, error)
	Close() error
}

// dayBucket truncates a time to UTC midnight. Exported indirectly via
// LastN; tests can call it through the helper.
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

// MemMeter keeps everything in a map. Lost on process restart, which is
// fine for dev / tests but useless for the admin dashboard in prod.
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

// SQLMeter writes to token_usage_daily using UPSERT semantics. Works on
// both SQLite and Postgres — they both support
// `INSERT … ON CONFLICT (…) DO UPDATE SET …` with the same column-ref
// form.
//
// The table schema is owned by store/database.go's migration block (see
// migrateTokenUsageDaily). SQLMeter is a thin query layer on top.
type SQLMeter struct {
	db      *sql.DB
	dialect string // "postgres" | "sqlite"
}

// NewSQLMeter wraps an open *sql.DB. The caller (gateway boot) supplies
// the same db+dialect the Store was built on so we share the connection
// pool and respect SetMaxOpenConns tuning.
func NewSQLMeter(db *sql.DB, dialect string) *SQLMeter {
	return &SQLMeter{db: db, dialect: dialect}
}

func (s *SQLMeter) Close() error { return nil } // pool owned by store

// placeholders generates $1,$2,… for postgres and ?,?,… for sqlite.
func (s *SQLMeter) ph(i int) string {
	if s.dialect == "postgres" {
		return fmt.Sprintf("$%d", i)
	}
	return "?"
}

// rebind rewrites a query written with ? placeholders to $1..$N when
// running on postgres. Keeps query strings readable.
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

// dayParam returns the value to bind for a day column. SQLite stores
// DATE as TEXT 'YYYY-MM-DD'; Postgres accepts time.Time directly.
func (s *SQLMeter) dayParam(t time.Time) any {
	if s.dialect == "sqlite" {
		return t.Format("2006-01-02")
	}
	return t
}

func (s *SQLMeter) RecordTokens(ctx context.Context, userID, agentID, sessionKey, provider, model string, t Tokens) error {
	day := s.dayParam(dayBucket(time.Now()))
	// Both dialects support this six-column conflict target and the
	// EXCLUDED reference. We additionally bump request_count by 1.
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
	// userID is optional — when blank we don't constrain on it. Two
	// variants of the query keep the prepared statement form clean
	// rather than building NULL-checks into the WHERE.
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

// scanRanks is the shared row-iterator for SessionsForAgent and
// topBy — they only differ in WHERE/GROUP BY, so the scan boilerplate
// is factored out.
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
	// col is a hardcoded constant from TopAgents/TopUsers — never
	// user-supplied — so concatenation is safe here.
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
