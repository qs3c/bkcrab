package fairqueue

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	redis "github.com/redis/go-redis/v9"
)

func TestRedisRuntimeRejectsReadyControlWithResidualProgress(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()

	root := redisTestRoot(fixture)
	if err := fixture.raw.HSet(ctx, root+"recovery_progress", "orphan", "1").Err(); err != nil {
		t.Fatalf("create residual recovery progress: %v", err)
	}
	if err := fixture.coordinator.CheckReadyFence(ctx, fixture.resource, fence); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("CheckReadyFence() error = %v, want ErrCoordinationCorrupt", err)
	}
	if _, err := fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, redisTestWriterA); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("ObserveReadyFence() error = %v, want ErrCoordinationCorrupt", err)
	}
	if err := fixture.coordinator.EnsureKnownTenant(ctx, fixture.resource, fence, "tenant-a"); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("EnsureKnownTenant() error = %v, want ErrCoordinationCorrupt", err)
	}
	if count, err := fixture.raw.SCard(ctx, root+"known_users").Result(); err != nil || count != 0 {
		t.Fatalf("rejected READY mutation changed known users: count=%d error=%v", count, err)
	}
}

func TestRedisRuntimeExpiredReservationFencesBindAndRenew(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	attempt := redisTestRandomHex(t, 16)
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", attempt, limits, 5*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional() = (%q,%v)", decision, err)
	}
	root := redisTestRoot(fixture)
	tenantHash, err := TenantHash(fixture.resource, "tenant-a")
	if err != nil {
		t.Fatalf("TenantHash() error = %v", err)
	}
	tenantKey := root + "inflight:tenant:" + tenantHash
	provisional := "p:" + attempt
	redisNow, err := fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	expired := float64(redisNow.UnixMilli() - 1)
	for _, key := range []string{root + "inflight", tenantKey, root + "provisional"} {
		if err := fixture.raw.ZAdd(ctx, key, redis.Z{Score: expired, Member: provisional}).Err(); err != nil {
			t.Fatalf("expire provisional in %q: %v", key, err)
		}
	}
	stable, err := StableReservationToken(fixture.resource, "31", 1)
	if err != nil {
		t.Fatalf("StableReservationToken() error = %v", err)
	}
	beforeMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-a", attempt, stable, 3*time.Second,
	); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("BindReservation(expired) error = %v, want ErrFenceMismatch", err)
	}
	afterMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
	if !reflect.DeepEqual(afterMeta, beforeMeta) {
		t.Fatalf("expired BindReservation partially mutated metadata: before=%#v after=%#v", beforeMeta, afterMeta)
	}
	if exists, _ := fixture.raw.ZScore(ctx, root+"inflight", stable).Result(); exists != 0 {
		t.Fatalf("expired provisional was promoted with score %v", exists)
	}

	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-a", attempt); err != nil {
		t.Fatalf("Release(expired provisional) error = %v", err)
	}
	if err := fixture.coordinator.EnsureReadyStableInflight(
		ctx, fixture.resource, fence, "tenant-a", stable, 3*time.Second,
	); err != nil {
		t.Fatalf("EnsureReadyStableInflight() error = %v", err)
	}
	for _, key := range []string{root + "inflight", tenantKey} {
		if err := fixture.raw.ZAdd(ctx, key, redis.Z{Score: expired, Member: stable}).Err(); err != nil {
			t.Fatalf("expire stable in %q: %v", key, err)
		}
	}
	if err := fixture.coordinator.RenewStable(
		ctx, fixture.resource, fence, "tenant-a", stable, 3*time.Second,
	); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("RenewStable(expired) error = %v, want ErrFenceMismatch", err)
	}
	if score, err := fixture.raw.ZScore(ctx, root+"inflight", stable).Result(); err != nil || score != expired {
		t.Fatalf("expired RenewStable changed score: score=%v error=%v", score, err)
	}
	replacementAttempt := redisTestRandomHex(t, 16)
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", replacementAttempt, limits, 3*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional(expired stable cleanup) = (%q,%v), want regular", decision, err)
	}
	if _, err := fixture.raw.ZScore(ctx, root+"inflight", stable).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("bounded acquire cleanup retained expired stable: %v", err)
	}
	if err := fixture.coordinator.Release(
		ctx, fixture.resource, fence, "tenant-a", replacementAttempt,
	); err != nil {
		t.Fatalf("Release(replacement provisional) error = %v", err)
	}
	if err := fixture.coordinator.EnsureReadyStableInflight(
		ctx, fixture.resource, fence, "tenant-a", stable, 3*time.Second,
	); err != nil {
		t.Fatalf("EnsureReadyStableInflight(expired existing) error = %v", err)
	}
	if score, err := fixture.raw.ZScore(ctx, root+"inflight", stable).Result(); err != nil || score <= expired {
		t.Fatalf("EnsureReadyStableInflight did not restore expiry: score=%v error=%v", score, err)
	}
}

func TestRedisRuntimeAcquireAuditsFullCapacityBeforeDenial(t *testing.T) {
	for _, corruption := range []string{"metadata", "tenant index"} {
		t.Run(corruption, func(t *testing.T) {
			fixture := newRedisIntegrationFixture(t)
			fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
			ctx, cancel := redisTestContext(t)
			defer cancel()
			if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
				t.Fatalf("Activate() error = %v", err)
			}
			limits := CapacityLimits{GlobalConcurrency: 1, PerUserBaseConcurrency: 1, PerUserBurstConcurrency: 1, BorrowEnabled: true}
			attempt := redisTestRandomHex(t, 16)
			if decision, err := fixture.coordinator.AcquireProvisional(
				ctx, fixture.resource, fence, "tenant-a", attempt, limits, 5*time.Second,
			); err != nil || decision != ReservationRegular {
				t.Fatalf("AcquireProvisional(first) = (%q,%v)", decision, err)
			}
			root := redisTestRoot(fixture)
			tenantHash, err := TenantHash(fixture.resource, "tenant-a")
			if err != nil {
				t.Fatalf("TenantHash() error = %v", err)
			}
			tenantKey := root + "inflight:tenant:" + tenantHash
			switch corruption {
			case "metadata":
				if err := fixture.raw.HSet(ctx, root+"reservation_meta", "p:"+attempt, "{").Err(); err != nil {
					t.Fatalf("corrupt reservation metadata: %v", err)
				}
			case "tenant index":
				if err := fixture.raw.ZRem(ctx, tenantKey, "p:"+attempt).Err(); err != nil {
					t.Fatalf("corrupt tenant reservation index: %v", err)
				}
			}

			beforeGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
			beforeProvisional, _ := fixture.raw.ZRangeWithScores(ctx, root+"provisional", 0, -1).Result()
			beforeTenant, _ := fixture.raw.ZRangeWithScores(ctx, tenantKey, 0, -1).Result()
			beforeMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
			second := redisTestRandomHex(t, 16)
			if _, err := fixture.coordinator.AcquireProvisional(
				ctx, fixture.resource, fence, "tenant-a", second, limits, 5*time.Second,
			); !errors.Is(err, ErrCoordinationCorrupt) {
				t.Fatalf("AcquireProvisional(full corrupt state) error = %v, want ErrCoordinationCorrupt", err)
			}
			afterGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
			afterProvisional, _ := fixture.raw.ZRangeWithScores(ctx, root+"provisional", 0, -1).Result()
			afterTenant, _ := fixture.raw.ZRangeWithScores(ctx, tenantKey, 0, -1).Result()
			afterMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
			if !reflect.DeepEqual(afterGlobal, beforeGlobal) ||
				!reflect.DeepEqual(afterProvisional, beforeProvisional) ||
				!reflect.DeepEqual(afterTenant, beforeTenant) ||
				!reflect.DeepEqual(afterMeta, beforeMeta) {
				t.Fatalf("corrupt full-capacity AcquireProvisional partially mutated state")
			}
		})
	}
}

func TestRedisRuntimeRepeatedBindDoesNotRenewStable(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	limits := CapacityLimits{GlobalConcurrency: 2, PerUserBaseConcurrency: 1, PerUserBurstConcurrency: 2, BorrowEnabled: true}
	attempt := redisTestRandomHex(t, 16)
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", attempt, limits, 10*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional() = (%q,%v)", decision, err)
	}
	stable, err := StableReservationToken(fixture.resource, "bind-idempotency", 1)
	if err != nil {
		t.Fatalf("StableReservationToken() error = %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-a", attempt, stable, 10*time.Second,
	); err != nil {
		t.Fatalf("BindReservation(first) error = %v", err)
	}
	root := redisTestRoot(fixture)
	tenantHash, err := TenantHash(fixture.resource, "tenant-a")
	if err != nil {
		t.Fatalf("TenantHash() error = %v", err)
	}
	tenantKey := root + "inflight:tenant:" + tenantHash
	beforeGlobal, err := fixture.raw.ZScore(ctx, root+"inflight", stable).Result()
	if err != nil {
		t.Fatalf("read stable global score: %v", err)
	}
	beforeTenant, err := fixture.raw.ZScore(ctx, tenantKey, stable).Result()
	if err != nil {
		t.Fatalf("read stable tenant score: %v", err)
	}
	beforeMetadata, err := fixture.raw.HGet(ctx, root+"reservation_meta", stable).Result()
	if err != nil {
		t.Fatalf("read stable metadata: %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-a", attempt, stable, time.Minute,
	); err != nil {
		t.Fatalf("BindReservation(retry) error = %v", err)
	}
	afterGlobal, _ := fixture.raw.ZScore(ctx, root+"inflight", stable).Result()
	afterTenant, _ := fixture.raw.ZScore(ctx, tenantKey, stable).Result()
	afterMetadata, _ := fixture.raw.HGet(ctx, root+"reservation_meta", stable).Result()
	if afterGlobal != beforeGlobal || afterTenant != beforeTenant || afterMetadata != beforeMetadata {
		t.Fatalf("repeated BindReservation mutated stable reservation")
	}
}

func TestRedisRuntimeAcquireRequiresCompleteLiveReservationIndexes(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	first := redisTestRandomHex(t, 16)
	if _, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", first, limits, 5*time.Second,
	); err != nil {
		t.Fatalf("AcquireProvisional(first) error = %v", err)
	}
	root := redisTestRoot(fixture)
	tenantHash, _ := TenantHash(fixture.resource, "tenant-a")
	tenantKey := root + "inflight:tenant:" + tenantHash
	if err := fixture.raw.ZRem(ctx, tenantKey, "p:"+first).Err(); err != nil {
		t.Fatalf("remove tenant reservation index: %v", err)
	}
	beforeGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	beforeMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
	second := redisTestRandomHex(t, 16)
	if _, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", second, limits, 5*time.Second,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("AcquireProvisional(partial indexes) error = %v, want ErrCoordinationCorrupt", err)
	}
	afterGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	afterMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
	if !reflect.DeepEqual(afterGlobal, beforeGlobal) || !reflect.DeepEqual(afterMeta, beforeMeta) {
		t.Fatalf("corrupt AcquireProvisional partially mutated state")
	}
	if _, err := fixture.raw.ZScore(ctx, root+"inflight", "p:"+second).Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("corrupt AcquireProvisional created new token: %v", err)
	}
}

func TestRedisRuntimeCleanupRequiresEqualProvisionalScores(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	limits := CapacityLimits{GlobalConcurrency: 2, PerUserBaseConcurrency: 1, PerUserBurstConcurrency: 2, BorrowEnabled: true}
	attempt := redisTestRandomHex(t, 16)
	if _, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", attempt, limits, 5*time.Second,
	); err != nil {
		t.Fatalf("AcquireProvisional() error = %v", err)
	}
	root := redisTestRoot(fixture)
	redisNow, _ := fixture.raw.Time(ctx).Result()
	if err := fixture.raw.ZAdd(ctx, root+"provisional", redis.Z{
		Score: float64(redisNow.UnixMilli() - 1), Member: "p:" + attempt,
	}).Err(); err != nil {
		t.Fatalf("create mismatched provisional expiry: %v", err)
	}
	beforeGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	beforeProvisional, _ := fixture.raw.ZRangeWithScores(ctx, root+"provisional", 0, -1).Result()
	if _, err := fixture.coordinator.ReapExpiredTurnsAndProvisionals(
		ctx, fixture.resource, fence, 10,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("ReapExpiredTurnsAndProvisionals(score mismatch) error = %v, want ErrCoordinationCorrupt", err)
	}
	afterGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	afterProvisional, _ := fixture.raw.ZRangeWithScores(ctx, root+"provisional", 0, -1).Result()
	if !reflect.DeepEqual(afterGlobal, beforeGlobal) || !reflect.DeepEqual(afterProvisional, beforeProvisional) {
		t.Fatal("rejected cleanup partially mutated reservation indexes")
	}
}

func TestRedisRuntimeAcquirePreflightsClosureBeforeExpiredCleanup(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 4, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	expiredAttempt := redisTestRandomHex(t, 16)
	liveAttempt := redisTestRandomHex(t, 16)
	for _, attempt := range []string{expiredAttempt, liveAttempt} {
		if decision, err := fixture.coordinator.AcquireProvisional(
			ctx, fixture.resource, fence, "tenant-a", attempt, limits, 10*time.Second,
		); err != nil || decision != ReservationRegular {
			t.Fatalf("AcquireProvisional(%q) = (%q,%v)", attempt, decision, err)
		}
	}
	root := redisTestRoot(fixture)
	tenantHash, err := TenantHash(fixture.resource, "tenant-a")
	if err != nil {
		t.Fatalf("TenantHash() error = %v", err)
	}
	tenantKey := root + "inflight:tenant:" + tenantHash
	redisNow, err := fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	expired := float64(redisNow.UnixMilli() - 1)
	for _, key := range []string{root + "inflight", tenantKey, root + "provisional"} {
		if err := fixture.raw.ZAdd(ctx, key, redis.Z{Score: expired, Member: "p:" + expiredAttempt}).Err(); err != nil {
			t.Fatalf("expire provisional in %q: %v", key, err)
		}
	}
	if err := fixture.raw.HSet(ctx, root+"reservation_meta", "p:"+liveAttempt, "{").Err(); err != nil {
		t.Fatalf("corrupt live reservation metadata: %v", err)
	}

	beforeGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	beforeProvisional, _ := fixture.raw.ZRangeWithScores(ctx, root+"provisional", 0, -1).Result()
	beforeTenant, _ := fixture.raw.ZRangeWithScores(ctx, tenantKey, 0, -1).Result()
	beforeMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
	candidate := redisTestRandomHex(t, 16)
	if _, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", candidate, limits, 10*time.Second,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("AcquireProvisional(expired plus live corruption) error = %v, want ErrCoordinationCorrupt", err)
	}
	afterGlobal, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	afterProvisional, _ := fixture.raw.ZRangeWithScores(ctx, root+"provisional", 0, -1).Result()
	afterTenant, _ := fixture.raw.ZRangeWithScores(ctx, tenantKey, 0, -1).Result()
	afterMeta, _ := fixture.raw.HGetAll(ctx, root+"reservation_meta").Result()
	if !reflect.DeepEqual(afterGlobal, beforeGlobal) ||
		!reflect.DeepEqual(afterProvisional, beforeProvisional) ||
		!reflect.DeepEqual(afterTenant, beforeTenant) ||
		!reflect.DeepEqual(afterMeta, beforeMeta) {
		t.Fatal("AcquireProvisional mutated expired indexes before detecting later corruption")
	}
}

func TestRedisRuntimeNextTurnConsumesHistoricalDuplicateFragments(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	root := redisTestRoot(fixture)
	if err := fixture.raw.RPush(ctx, root+"ring", "tenant-a").Err(); err != nil {
		t.Fatalf("append historical duplicate ring fragment: %v", err)
	}
	firstToken := ProcessingTurnToken(redisTestRandomHex(t, 16))
	first, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, firstToken, 5*time.Second)
	if err != nil || !ok || first.TenantID != "tenant-a" {
		t.Fatalf("NextTurn(first duplicate) = (%#v,%v,%v)", first, ok, err)
	}
	if _, ok, err := fixture.coordinator.NextTurn(
		ctx, fixture.resource, fence, ProcessingTurnToken(redisTestRandomHex(t, 16)), 5*time.Second,
	); err != nil || ok {
		t.Fatalf("NextTurn(stale duplicate) = (_, %v, %v), want bounded cleanup without a second owner", ok, err)
	}
	if length, err := fixture.raw.LLen(ctx, root+"ring").Result(); err != nil || length != 0 {
		t.Fatalf("historical duplicate remained in ring: length=%d error=%v", length, err)
	}
	if err := fixture.coordinator.RotateOrDeactivate(
		ctx, fixture.resource, fence, firstToken, first.ObservedActivationGeneration, true,
	); err != nil {
		t.Fatalf("RotateOrDeactivate() after duplicate cleanup error = %v", err)
	}
	next, ok, err := fixture.coordinator.NextTurn(
		ctx, fixture.resource, fence, ProcessingTurnToken(redisTestRandomHex(t, 16)), 5*time.Second,
	)
	if err != nil || !ok || next.TenantID != "tenant-a" {
		t.Fatalf("NextTurn(after duplicate cleanup) = (%#v,%v,%v)", next, ok, err)
	}
}

func TestRedisRuntimeListRejectsStableIndexAndTenantHashCorruption(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	root := redisTestRoot(fixture)
	stable, _ := StableReservationToken(fixture.resource, "41", 1)
	if err := fixture.coordinator.EnsureReadyStableInflight(
		ctx, fixture.resource, fence, "tenant-a", stable, 5*time.Second,
	); err != nil {
		t.Fatalf("EnsureReadyStableInflight() error = %v", err)
	}
	if err := fixture.raw.ZAdd(ctx, root+"inflight:stable_index", redis.Z{Score: 1, Member: stable}).Err(); err != nil {
		t.Fatalf("corrupt stable index score: %v", err)
	}
	if _, err := fixture.coordinator.ListReadyStableInflight(
		ctx, fixture.resource, fence, "", 10,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("ListReadyStableInflight(stable score) error = %v, want ErrCoordinationCorrupt", err)
	}
	if err := fixture.raw.ZAdd(ctx, root+"inflight:stable_index", redis.Z{Score: 0, Member: stable}).Err(); err != nil {
		t.Fatalf("restore stable index score: %v", err)
	}
	wrongKey := root + "inflight:tenant:" + strings.Repeat("f", 64)
	corruptMetadata := `{"tenant":"tenant-a","tenant_key":"` + wrongKey + `","kind":"S"}`
	if err := fixture.raw.HSet(ctx, root+"reservation_meta", stable, corruptMetadata).Err(); err != nil {
		t.Fatalf("corrupt tenant key metadata: %v", err)
	}
	before, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	if _, err := fixture.coordinator.ListReadyStableInflight(
		ctx, fixture.resource, fence, "", 10,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("ListReadyStableInflight(tenant hash) error = %v, want ErrCoordinationCorrupt", err)
	}
	after, _ := fixture.raw.ZRangeWithScores(ctx, root+"inflight", 0, -1).Result()
	if !reflect.DeepEqual(after, before) {
		t.Fatal("corrupt stable list partially mutated inflight state")
	}
}
