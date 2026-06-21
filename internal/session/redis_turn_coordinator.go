package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/qs3c/bkclaw/internal/provider"
)

const (
	defaultRedisTurnPrefix = "bkclaw"
	defaultRedisTurnTTL    = 2 * time.Hour
	defaultRedisStreamTTL  = 24 * time.Hour
)

// RedisTurnOptions configures Redis-backed turn coordination.
type RedisTurnOptions struct {
	KeyPrefix string
	OwnerID   string
	LeaseTTL  time.Duration
	StreamTTL time.Duration
}

type redisTurnRecord struct {
	OwnerID string `json:"ownerId"`
	TurnID  string `json:"turnId"`
	Token   string `json:"token"`
}

type redisOwnerState struct {
	depth       int
	lastSteerID string
	recordValue string
}

// RedisTurnCoordinator shares active turn state and steer buffers across pods.
type RedisTurnCoordinator struct {
	client    *redis.Client
	keyPrefix string
	ownerID   string
	leaseTTL  time.Duration
	streamTTL time.Duration

	mu     sync.Mutex
	states map[string]*redisOwnerState
}

func NewRedisTurnCoordinator(client *redis.Client, opts RedisTurnOptions) *RedisTurnCoordinator {
	prefix := strings.TrimSpace(opts.KeyPrefix)
	if prefix == "" {
		prefix = defaultRedisTurnPrefix
	}
	ownerID := strings.TrimSpace(opts.OwnerID)
	if ownerID == "" {
		ownerID = uuid.NewString()
	}
	leaseTTL := opts.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = defaultRedisTurnTTL
	}
	streamTTL := opts.StreamTTL
	if streamTTL <= 0 {
		streamTTL = defaultRedisStreamTTL
	}
	return &RedisTurnCoordinator{
		client:    client,
		keyPrefix: prefix,
		ownerID:   ownerID,
		leaseTTL:  leaseTTL,
		streamTTL: streamTTL,
		states:    make(map[string]*redisOwnerState),
	}
}

func (c *RedisTurnCoordinator) BeginTurn(ctx context.Context, key TurnKey) (*TurnLease, bool, error) {
	record := redisTurnRecord{
		OwnerID: c.ownerID,
		TurnID:  uuid.NewString(),
		Token:   uuid.NewString(),
	}
	value, err := json.Marshal(record)
	if err != nil {
		return nil, false, err
	}
	ok, err := c.client.SetNX(ctx, c.turnKey(key), string(value), c.leaseTTL).Result()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		record, raw, active, err := c.getRecordValue(ctx, key)
		if err != nil || !active {
			return nil, false, err
		}
		if record.OwnerID == c.ownerID {
			return c.reentrantLease(key, record, raw), true, nil
		}
		return nil, false, nil
	}
	lease := &TurnLease{
		Key:         key,
		TurnID:      record.TurnID,
		Token:       record.Token,
		lastSteerID: "0-0",
		recordValue: string(value),
	}
	c.mu.Lock()
	c.states[c.ownerStateKey(key, record.TurnID)] = &redisOwnerState{
		depth:       1,
		lastSteerID: "0-0",
		recordValue: string(value),
	}
	c.mu.Unlock()
	c.startRenewal(lease)
	return lease, true, nil
}

func (c *RedisTurnCoordinator) PushSteer(ctx context.Context, key TurnKey, msg provider.Message) (bool, error) {
	record, ok, err := c.getRecord(ctx, key)
	if err != nil || !ok {
		return ok, err
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return false, err
	}
	_, err = c.client.XAdd(ctx, &redis.XAddArgs{
		Stream: c.streamKey(key, record.TurnID),
		Values: map[string]any{"message": string(payload)},
	}).Result()
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *RedisTurnCoordinator) DrainSteer(ctx context.Context, lease *TurnLease) ([]provider.Message, error) {
	if lease == nil {
		return nil, nil
	}
	lastID := lease.lastSteerID
	stateKey := c.ownerStateKey(lease.Key, lease.TurnID)
	c.mu.Lock()
	if st := c.states[stateKey]; st != nil {
		lastID = st.lastSteerID
	}
	c.mu.Unlock()
	start := "-"
	if lastID != "" && lastID != "0-0" {
		start = "(" + lastID
	}
	entries, err := c.client.XRangeN(ctx, c.streamKey(lease.Key, lease.TurnID), start, "+", 100).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []provider.Message
	for _, entry := range entries {
		raw, ok := entry.Values["message"].(string)
		if !ok {
			lease.lastSteerID = entry.ID
			continue
		}
		var msg provider.Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			return nil, err
		}
		out = append(out, msg)
		lease.lastSteerID = entry.ID
	}
	c.mu.Lock()
	if st := c.states[stateKey]; st != nil {
		st.lastSteerID = lease.lastSteerID
	}
	c.mu.Unlock()
	return out, nil
}

func (c *RedisTurnCoordinator) EndTurn(ctx context.Context, lease *TurnLease) ([]provider.Message, error) {
	if lease == nil {
		return nil, nil
	}
	if !c.decrementDepth(lease) {
		return nil, nil
	}
	if lease.stopRenew != nil {
		lease.stopRenew()
	}
	leftover, err := c.DrainSteer(ctx, lease)
	if err != nil {
		return nil, err
	}
	if err := redisReleaseTurn.Run(ctx, c.client, []string{c.turnKey(lease.Key)}, lease.recordValue).Err(); err != nil {
		return nil, err
	}
	_ = c.client.Expire(ctx, c.streamKey(lease.Key, lease.TurnID), c.streamTTL).Err()
	c.clearOwnerState(lease)
	return leftover, nil
}

func (c *RedisTurnCoordinator) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

func (c *RedisTurnCoordinator) getRecord(ctx context.Context, key TurnKey) (redisTurnRecord, bool, error) {
	record, _, ok, err := c.getRecordValue(ctx, key)
	return record, ok, err
}

func (c *RedisTurnCoordinator) getRecordValue(ctx context.Context, key TurnKey) (redisTurnRecord, string, bool, error) {
	raw, err := c.client.Get(ctx, c.turnKey(key)).Result()
	if errors.Is(err, redis.Nil) {
		return redisTurnRecord{}, "", false, nil
	}
	if err != nil {
		return redisTurnRecord{}, "", false, err
	}
	var record redisTurnRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return redisTurnRecord{}, "", false, err
	}
	if record.TurnID == "" || record.Token == "" {
		return redisTurnRecord{}, "", false, nil
	}
	return record, raw, true, nil
}

func (c *RedisTurnCoordinator) reentrantLease(key TurnKey, record redisTurnRecord, raw string) *TurnLease {
	stateKey := c.ownerStateKey(key, record.TurnID)
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.states[stateKey]
	if st == nil {
		st = &redisOwnerState{lastSteerID: "0-0", recordValue: raw}
		c.states[stateKey] = st
	}
	st.depth++
	return &TurnLease{
		Key:         key,
		TurnID:      record.TurnID,
		Token:       record.Token,
		lastSteerID: st.lastSteerID,
		recordValue: raw,
	}
}

func (c *RedisTurnCoordinator) decrementDepth(lease *TurnLease) bool {
	stateKey := c.ownerStateKey(lease.Key, lease.TurnID)
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.states[stateKey]
	if st == nil {
		return true
	}
	if st.depth > 1 {
		st.depth--
		lease.lastSteerID = st.lastSteerID
		return false
	}
	st.depth = 0
	lease.lastSteerID = st.lastSteerID
	return true
}

func (c *RedisTurnCoordinator) clearOwnerState(lease *TurnLease) {
	c.mu.Lock()
	delete(c.states, c.ownerStateKey(lease.Key, lease.TurnID))
	c.mu.Unlock()
}

func (c *RedisTurnCoordinator) startRenewal(lease *TurnLease) {
	renewEvery := c.leaseTTL / 3
	if renewEvery <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	lease.stopRenew = cancel
	go func() {
		ticker := time.NewTicker(renewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = redisRenewTurn.Run(context.Background(), c.client, []string{c.turnKey(lease.Key)}, lease.recordValue, int64(c.leaseTTL/time.Millisecond)).Err()
			}
		}
	}()
}

func (c *RedisTurnCoordinator) turnKey(key TurnKey) string {
	return strings.Join([]string{
		c.keyPrefix,
		"turn",
		encodeTurnKeyPart(key.UserID),
		encodeTurnKeyPart(key.AgentID),
		encodeTurnKeyPart(key.SessionKey),
	}, ":")
}

func (c *RedisTurnCoordinator) streamKey(key TurnKey, turnID string) string {
	return strings.Join([]string{
		c.keyPrefix,
		"steer",
		encodeTurnKeyPart(key.UserID),
		encodeTurnKeyPart(key.AgentID),
		encodeTurnKeyPart(key.SessionKey),
		encodeTurnKeyPart(turnID),
	}, ":")
}

func (c *RedisTurnCoordinator) ownerStateKey(key TurnKey, turnID string) string {
	return key.String() + "\x00" + turnID
}

func encodeTurnKeyPart(part string) string {
	if part == "" {
		return "-"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(part))
}

var redisReleaseTurn = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0
`)

var redisRenewTurn = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("PEXPIRE", KEYS[1], ARGV[2])
end
return 0
`)
