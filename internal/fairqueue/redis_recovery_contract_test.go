package fairqueue

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	redisv9 "github.com/redis/go-redis/v9"
)

func redisTestBeginNormalRecovery(
	t *testing.T,
	ctx context.Context,
	fixture *redisIntegrationFixture,
) RecoveryFence {
	t.Helper()
	lock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	fence, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, lock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock() error = %v", err)
	}
	return fence
}

func TestRedisRecoveryReapRejectsForgedTenantKeyWithoutMutation(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	ready := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()

	const tenantAlice = "tenant-alice"
	const tenantBob = "tenant-bob"
	if err := fixture.coordinator.Activate(ctx, fixture.resource, ready, tenantAlice); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	attempt := redisTestRandomHex(t, 16)
	limits := CapacityLimits{
		GlobalConcurrency:       4,
		PerUserBaseConcurrency:  2,
		PerUserBurstConcurrency: 4,
		BorrowEnabled:           true,
	}
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, ready, tenantAlice, attempt, limits, 5*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional() = (%q,%v)", decision, err)
	}

	root := redisTestRoot(fixture)
	token := "p:" + attempt
	globalKey := root + "inflight"
	provisionalKey := root + "provisional"
	metadataKey := root + "reservation_meta"
	stableKey := root + "inflight:stable_index"
	aliceHash, err := TenantHash(fixture.resource, tenantAlice)
	if err != nil {
		t.Fatalf("TenantHash(alice) error = %v", err)
	}
	bobHash, err := TenantHash(fixture.resource, tenantBob)
	if err != nil {
		t.Fatalf("TenantHash(bob) error = %v", err)
	}
	aliceKey := root + "inflight:tenant:" + aliceHash
	bobKey := root + "inflight:tenant:" + bobHash
	redisNow, err := fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	expired := float64(redisNow.UnixMilli() - 1)
	for _, key := range []string{globalKey, provisionalKey, bobKey} {
		if err := fixture.raw.ZAdd(ctx, key, redisv9.Z{Score: expired, Member: token}).Err(); err != nil {
			t.Fatalf("ZADD forged reservation member error = %v", err)
		}
	}
	forgedMetadata, err := json.Marshal(map[string]string{
		"tenant":     tenantAlice,
		"tenant_key": bobKey,
		"kind":       "P",
		"decision":   string(ReservationRegular),
	})
	if err != nil {
		t.Fatalf("marshal forged metadata: %v", err)
	}
	if err := fixture.raw.HSet(ctx, metadataKey, token, forgedMetadata).Err(); err != nil {
		t.Fatalf("HSET forged metadata error = %v", err)
	}

	fence := redisTestBeginNormalRecovery(t, ctx, fixture)
	type snapshot struct {
		Global      []redisv9.Z
		Provisional []redisv9.Z
		Stable      []redisv9.Z
		Alice       []redisv9.Z
		Bob         []redisv9.Z
		Metadata    map[string]string
	}
	readSnapshot := func() snapshot {
		t.Helper()
		readZSet := func(key string) []redisv9.Z {
			values, readErr := fixture.raw.ZRangeWithScores(ctx, key, 0, -1).Result()
			if readErr != nil {
				t.Fatalf("ZRANGE %q error = %v", key, readErr)
			}
			return values
		}
		metadata, readErr := fixture.raw.HGetAll(ctx, metadataKey).Result()
		if readErr != nil {
			t.Fatalf("HGETALL metadata error = %v", readErr)
		}
		return snapshot{
			Global:      readZSet(globalKey),
			Provisional: readZSet(provisionalKey),
			Stable:      readZSet(stableKey),
			Alice:       readZSet(aliceKey),
			Bob:         readZSet(bobKey),
			Metadata:    metadata,
		}
	}
	before := readSnapshot()
	if result, err := fixture.coordinator.RecoveryReapExpired(
		ctx, fixture.resource, fence, 10,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("RecoveryReapExpired() = (%#v,%v), want ErrCoordinationCorrupt", result, err)
	}
	after := readSnapshot()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected forged tenant key mutated reservation state:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRedisRecoveryReapRejectsNonCanonicalTurnWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		corruptToken      bool
		corruptGeneration bool
	}{
		{name: "token", corruptToken: true},
		{name: "generation", corruptGeneration: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newRedisIntegrationFixture(t)
			ready := redisTestInitializeReady(t, fixture, redisTestWriterA)
			ctx, cancel := redisTestContext(t)
			defer cancel()
			const tenant = "tenant-turn"
			if err := fixture.coordinator.Activate(ctx, fixture.resource, ready, tenant); err != nil {
				t.Fatalf("Activate() error = %v", err)
			}
			token := ProcessingTurnToken(redisTestRandomHex(t, 16))
			turn, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, ready, token, 5*time.Second)
			if err != nil || !ok {
				t.Fatalf("NextTurn() = (%#v,%v,%v)", turn, ok, err)
			}
			root := redisTestRoot(fixture)
			turnsKey := root + "processing_turns"
			turnTenantKey := root + "turn_to_tenant"
			turnGenerationKey := root + "turn_to_generation"
			tenantTurnKey := root + "tenant_to_turn"
			member := string(token)
			redisNow, err := fixture.raw.Time(ctx).Result()
			if err != nil {
				t.Fatalf("Redis TIME error = %v", err)
			}
			expired := float64(redisNow.UnixMilli() - 1)
			if testCase.corruptToken {
				const invalidToken = "not-canonical"
				if err := fixture.raw.ZRem(ctx, turnsKey, member).Err(); err != nil {
					t.Fatalf("ZREM original turn error = %v", err)
				}
				if err := fixture.raw.HDel(ctx, turnTenantKey, member).Err(); err != nil {
					t.Fatalf("HDEL original tenant mapping error = %v", err)
				}
				if err := fixture.raw.HDel(ctx, turnGenerationKey, member).Err(); err != nil {
					t.Fatalf("HDEL original generation mapping error = %v", err)
				}
				member = invalidToken
			}
			generation := "1"
			if testCase.corruptGeneration {
				generation = "01"
			}
			if err := fixture.raw.ZAdd(ctx, turnsKey, redisv9.Z{Score: expired, Member: member}).Err(); err != nil {
				t.Fatalf("ZADD corrupt turn error = %v", err)
			}
			if err := fixture.raw.HSet(ctx, turnTenantKey, member, tenant).Err(); err != nil {
				t.Fatalf("HSET turn tenant error = %v", err)
			}
			if err := fixture.raw.HSet(ctx, turnGenerationKey, member, generation).Err(); err != nil {
				t.Fatalf("HSET turn generation error = %v", err)
			}
			if err := fixture.raw.HSet(ctx, tenantTurnKey, tenant, member).Err(); err != nil {
				t.Fatalf("HSET tenant turn error = %v", err)
			}

			fence := redisTestBeginNormalRecovery(t, ctx, fixture)
			type snapshot struct {
				Turns          []redisv9.Z
				TurnTenant     map[string]string
				TurnGeneration map[string]string
				TenantTurn     map[string]string
			}
			readSnapshot := func() snapshot {
				t.Helper()
				turns, readErr := fixture.raw.ZRangeWithScores(ctx, turnsKey, 0, -1).Result()
				if readErr != nil {
					t.Fatalf("ZRANGE turns error = %v", readErr)
				}
				turnTenant, readErr := fixture.raw.HGetAll(ctx, turnTenantKey).Result()
				if readErr != nil {
					t.Fatalf("HGETALL turn tenant error = %v", readErr)
				}
				turnGeneration, readErr := fixture.raw.HGetAll(ctx, turnGenerationKey).Result()
				if readErr != nil {
					t.Fatalf("HGETALL turn generation error = %v", readErr)
				}
				tenantTurn, readErr := fixture.raw.HGetAll(ctx, tenantTurnKey).Result()
				if readErr != nil {
					t.Fatalf("HGETALL tenant turn error = %v", readErr)
				}
				return snapshot{
					Turns: turns, TurnTenant: turnTenant,
					TurnGeneration: turnGeneration, TenantTurn: tenantTurn,
				}
			}
			before := readSnapshot()
			if result, err := fixture.coordinator.RecoveryReapExpired(
				ctx, fixture.resource, fence, 10,
			); !errors.Is(err, ErrCoordinationCorrupt) {
				t.Fatalf("RecoveryReapExpired() = (%#v,%v), want ErrCoordinationCorrupt", result, err)
			}
			after := readSnapshot()
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected noncanonical turn mutated state:\nbefore=%#v\nafter=%#v", before, after)
			}
		})
	}
}
