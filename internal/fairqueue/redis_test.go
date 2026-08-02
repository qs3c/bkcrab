package fairqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	redisv9 "github.com/redis/go-redis/v9"
)

const (
	redisTestWriterA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	redisTestWriterB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type redisIntegrationFixture struct {
	coordinator *Redis
	raw         *redisv9.Client
	resource    string
	prefix      string
}

type redisTopologyProbeClient struct {
	clusterInfo string
	infoErr     error
	role        any
	roleErr     error

	doCalls       int
	scanCalls     int
	scriptCalls   int
	closeCalls    int
	scriptResults []redisTestScriptResult
}

type redisTestScriptResult struct {
	value any
	err   error
}

func (c *redisTopologyProbeClient) nextScriptResult() *redisv9.Cmd {
	c.scriptCalls++
	if len(c.scriptResults) == 0 {
		return redisv9.NewCmdResult(nil, nil)
	}
	result := c.scriptResults[0]
	c.scriptResults = c.scriptResults[1:]
	return redisv9.NewCmdResult(result.value, result.err)
}

func (c *redisTopologyProbeClient) Info(context.Context, ...string) *redisv9.StringCmd {
	return redisv9.NewStringResult(c.clusterInfo, c.infoErr)
}

func (c *redisTopologyProbeClient) Do(context.Context, ...any) *redisv9.Cmd {
	c.doCalls++
	return redisv9.NewCmdResult(c.role, c.roleErr)
}

func (c *redisTopologyProbeClient) Scan(context.Context, uint64, string, int64) *redisv9.ScanCmd {
	c.scanCalls++
	return redisv9.NewScanCmdResult(nil, 0, nil)
}

func (c *redisTopologyProbeClient) Eval(context.Context, string, []string, ...interface{}) *redisv9.Cmd {
	return c.nextScriptResult()
}

func (c *redisTopologyProbeClient) EvalSha(context.Context, string, []string, ...interface{}) *redisv9.Cmd {
	return c.nextScriptResult()
}

func (c *redisTopologyProbeClient) EvalRO(context.Context, string, []string, ...interface{}) *redisv9.Cmd {
	c.scriptCalls++
	return redisv9.NewCmdResult(nil, nil)
}

func (c *redisTopologyProbeClient) EvalShaRO(context.Context, string, []string, ...interface{}) *redisv9.Cmd {
	c.scriptCalls++
	return redisv9.NewCmdResult(nil, nil)
}

func (c *redisTopologyProbeClient) ScriptExists(context.Context, ...string) *redisv9.BoolSliceCmd {
	c.scriptCalls++
	return redisv9.NewBoolSliceResult(nil, nil)
}

func (c *redisTopologyProbeClient) ScriptLoad(context.Context, string) *redisv9.StringCmd {
	c.scriptCalls++
	return redisv9.NewStringResult("", nil)
}

func (c *redisTopologyProbeClient) Close() error {
	c.closeCalls++
	return nil
}

type redisFixedTokenSource struct {
	token string
	err   error
}

func (s redisFixedTokenSource) Next() (string, error) { return s.token, s.err }

func newRedisIntegrationFixture(t *testing.T) *redisIntegrationFixture {
	t.Helper()
	unique := redisTestRandomHex(t, 8)
	return newRedisIntegrationFixtureWithNamespace(
		t,
		"bkcrab:test:fairqueue:"+unique+":",
		"test.redis."+unique,
	)
}

func newRedisIntegrationFixtureWithNamespace(t *testing.T, prefix, resource string) *redisIntegrationFixture {
	t.Helper()
	addr := os.Getenv("BKCRAB_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("BKCRAB_TEST_REDIS_ADDR is not set")
	}
	database := 0
	if rawDatabase := os.Getenv("BKCRAB_TEST_REDIS_DB"); rawDatabase != "" {
		parsed, err := strconv.Atoi(rawDatabase)
		if err != nil {
			t.Fatalf("parse BKCRAB_TEST_REDIS_DB: %v", err)
		}
		database = parsed
	}
	password := os.Getenv("BKCRAB_TEST_REDIS_PASSWORD")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	coordinator, err := NewRedis(ctx, RedisOptions{
		Addr:             addr,
		Password:         password,
		DB:               database,
		KeyPrefix:        prefix,
		OperationTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedis() error = %v", err)
	}
	raw := redisv9.NewClient(&redisv9.Options{Addr: addr, Password: password, DB: database})
	fixture := &redisIntegrationFixture{
		coordinator: coordinator,
		raw:         raw,
		resource:    resource,
		prefix:      prefix,
	}
	t.Cleanup(func() {
		_ = coordinator.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		var cursor uint64
		for {
			root := prefix + "fair:{" + resource + "}:"
			keys, next, scanErr := raw.Scan(cleanupCtx, cursor, root+"*", 200).Result()
			if scanErr != nil {
				break
			}
			if len(keys) > 0 {
				_ = raw.Del(cleanupCtx, keys...).Err()
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		_ = raw.Close()
	})
	return fixture
}

func redisTestRandomHex(t *testing.T, bytes int) string {
	t.Helper()
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	return hex.EncodeToString(buffer)
}

func redisTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 20*time.Second)
}

func redisTestInitializeReady(t *testing.T, fixture *redisIntegrationFixture, writer string) ResourceFence {
	t.Helper()
	ctx, cancel := redisTestContext(t)
	defer cancel()
	lock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	recoveryFence, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, writer, lock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock() error = %v", err)
	}
	redisTestCompleteCommonRecovery(t, ctx, fixture, recoveryFence)
	fence, err := fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, writer)
	if err != nil {
		t.Fatalf("ObserveReadyFence() after FinishRecovery error = %v", err)
	}
	return fence
}

func redisTestCompleteCommonRecovery(
	t *testing.T,
	ctx context.Context,
	fixture *redisIntegrationFixture,
	fence RecoveryFence,
) {
	t.Helper()
	cleanup, err := fixture.coordinator.RecoveryReapExpired(ctx, fixture.resource, fence, 100)
	if err != nil {
		t.Fatalf("RecoveryReapExpired() error = %v", err)
	}
	if cleanup.RemainingProvisionals != 0 || cleanup.RemainingTurns != 0 {
		t.Fatalf("recovery cleanup not drained: %#v", cleanup)
	}
	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("ResetResource() error = %v", err)
	}
	if err := fixture.coordinator.SetRecoveryHighWater(ctx, fixture.resource, fence, "test-high-water"); err != nil {
		t.Fatalf("SetRecoveryHighWater() error = %v", err)
	}
	for _, pass := range []RecoveryPass{
		RecoveryPassKnownTenants,
		RecoveryPassDispatched,
		RecoveryPassRunning,
	} {
		if err := fixture.coordinator.MarkRecoveryPass(
			ctx, fixture.resource, fence, pass, 1, true, 0,
		); err != nil {
			t.Fatalf("MarkRecoveryPass(%s) error = %v", pass, err)
		}
	}
	if err := fixture.coordinator.FinishRecovery(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("FinishRecovery() error = %v", err)
	}
}

func redisTestFinishPreparedRecovery(
	t *testing.T,
	ctx context.Context,
	fixture *redisIntegrationFixture,
	fence RecoveryFence,
) {
	t.Helper()
	cleanup, err := fixture.coordinator.RecoveryReapExpired(ctx, fixture.resource, fence, 100)
	if err != nil {
		t.Fatalf("RecoveryReapExpired() error = %v", err)
	}
	if cleanup.RemainingProvisionals != 0 || cleanup.RemainingTurns != 0 {
		t.Fatalf("recovery cleanup not drained: %#v", cleanup)
	}
	if err := fixture.coordinator.SetRecoveryHighWater(ctx, fixture.resource, fence, "test-high-water"); err != nil {
		t.Fatalf("SetRecoveryHighWater() error = %v", err)
	}
	for _, pass := range []RecoveryPass{
		RecoveryPassKnownTenants,
		RecoveryPassDispatched,
		RecoveryPassRunning,
	} {
		if err := fixture.coordinator.MarkRecoveryPass(
			ctx, fixture.resource, fence, pass, 1, true, 0,
		); err != nil {
			t.Fatalf("MarkRecoveryPass(%s) error = %v", pass, err)
		}
	}
	if err := fixture.coordinator.FinishRecovery(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("FinishRecovery() error = %v", err)
	}
}

func redisTestEventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition was not satisfied before timeout")
	}
}

func redisTestRoot(fixture *redisIntegrationFixture) string {
	return fixture.prefix + "fair:{" + fixture.resource + "}:"
}

func redisTestExpireSortedSetMembers(
	t *testing.T,
	ctx context.Context,
	fixture *redisIntegrationFixture,
	suffix string,
) {
	t.Helper()
	root := redisTestRoot(fixture)
	key := root + suffix
	members, err := fixture.raw.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		t.Fatalf("ZRANGE(%s) error = %v", suffix, err)
	}
	redisNow, err := fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	expiredScore := float64(redisNow.UnixMilli() - 1)
	for _, member := range members {
		targets := []string{key}
		if suffix == "provisional" {
			targets = append(targets, root+"inflight")
			var cursor uint64
			var tenantKeys []string
			for {
				keys, next, scanErr := fixture.raw.Scan(ctx, cursor, root+"inflight:tenant:*", 100).Result()
				if scanErr != nil {
					t.Fatalf("SCAN tenant inflight keys error = %v", scanErr)
				}
				for _, tenantKey := range keys {
					if _, scoreErr := fixture.raw.ZScore(ctx, tenantKey, member).Result(); scoreErr == nil {
						tenantKeys = append(tenantKeys, tenantKey)
					} else if !errors.Is(scoreErr, redisv9.Nil) {
						t.Fatalf("ZSCORE tenant inflight member error = %v", scoreErr)
					}
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
			if len(tenantKeys) != 1 {
				t.Fatalf("provisional member %q belongs to %d tenant indexes, want one", member, len(tenantKeys))
			}
			targets = append(targets, tenantKeys[0])
		}
		for _, target := range targets {
			if err := fixture.raw.ZAdd(ctx, target, redisv9.Z{
				Score:  expiredScore,
				Member: member,
			}).Err(); err != nil {
				t.Fatalf("expire %s member %q in %q: %v", suffix, member, target, err)
			}
		}
	}
}

func redisTestExpireRecoveryLock(t *testing.T, ctx context.Context, fixture *redisIntegrationFixture) {
	t.Helper()
	key := redisTestRoot(fixture) + "recovery_lock"
	if err := fixture.raw.PExpire(ctx, key, time.Millisecond).Err(); err != nil {
		t.Fatalf("PEXPIRE recovery lock error = %v", err)
	}
	redisTestEventually(t, time.Second, func() bool {
		exists, err := fixture.raw.Exists(ctx, key).Result()
		return err == nil && exists == 0
	})
}

func redisTestRequireFenceError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("operation unexpectedly succeeded")
	}
	if !errors.Is(err, ErrFenceMismatch) &&
		!errors.Is(err, ErrResourceNotReady) &&
		!errors.Is(err, ErrRecoveryOwnerStale) {
		t.Fatalf("error = %v, want a typed fence/owner/not-ready category", err)
	}
}

func TestRedisFenceAndStableTokenContracts(t *testing.T) {
	validFence := ResourceFence{Epoch: strings.Repeat("a", 32), WriterFingerprint: redisTestWriterA}
	if err := validFence.Validate(); err != nil {
		t.Fatalf("valid ResourceFence rejected: %v", err)
	}
	for _, invalid := range []ResourceFence{
		{},
		{Epoch: strings.Repeat("A", 32), WriterFingerprint: redisTestWriterA},
		{Epoch: strings.Repeat("a", 32), WriterFingerprint: strings.Repeat("b", 63)},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid ResourceFence accepted: %#v", invalid)
		}
	}

	normal := RecoveryFence{
		ResourceFence: validFence,
		OwnerToken:    strings.Repeat("c", 32),
		Kind:          RecoveryNormal,
	}
	if err := normal.Validate(); err != nil {
		t.Fatalf("normal RecoveryFence rejected: %v", err)
	}
	special := normal
	special.Kind = RecoveryRabbitRepair
	special.OperationID = strings.Repeat("d", 32)
	if err := special.Validate(); err != nil {
		t.Fatalf("special RecoveryFence rejected: %v", err)
	}
	special.OperationID = ""
	if err := special.Validate(); err == nil {
		t.Fatal("special RecoveryFence without operation ID accepted")
	}

	token, err := StableReservationToken("rag.index", "42", 7)
	if err != nil {
		t.Fatalf("StableReservationToken() error = %v", err)
	}
	want := "r:c21df3f20cd249778d1876d7959666722e9f034b06cf15c42349dfb07a6d0f9c"
	if token != want {
		t.Fatalf("StableReservationToken() = %q, want %q", token, want)
	}
	if _, err := StableReservationToken("rag.index", "42", 0); err == nil {
		t.Fatal("zero claim generation accepted")
	}
}

func TestRedisKeyBuilderUsesCanonicalNamespaceAndSingleHashTag(t *testing.T) {
	keys, err := buildRedisKeyspace("bkcrab:", "rag.index")
	if err != nil {
		t.Fatalf("buildRedisKeyspace() error = %v", err)
	}
	if keys.root != "bkcrab:fair:{rag.index}:" {
		t.Fatalf("key root = %q", keys.root)
	}
	wants := map[string]string{
		"control":               "bkcrab:fair:{rag.index}:resource",
		"recovery lock":         "bkcrab:fair:{rag.index}:recovery_lock",
		"progress":              "bkcrab:fair:{rag.index}:recovery_progress",
		"ring":                  "bkcrab:fair:{rag.index}:ring",
		"ring members":          "bkcrab:fair:{rag.index}:ring_members",
		"active users":          "bkcrab:fair:{rag.index}:active_users",
		"known users":           "bkcrab:fair:{rag.index}:known_users",
		"activation generation": "bkcrab:fair:{rag.index}:activation_gen",
		"processing turns":      "bkcrab:fair:{rag.index}:processing_turns",
		"global inflight":       "bkcrab:fair:{rag.index}:inflight",
		"provisional":           "bkcrab:fair:{rag.index}:provisional",
		"reservation metadata":  "bkcrab:fair:{rag.index}:reservation_meta",
	}
	gots := map[string]string{
		"control":               keys.control,
		"recovery lock":         keys.recoveryLock,
		"progress":              keys.progress,
		"ring":                  keys.ring,
		"ring members":          keys.ringMembers,
		"active users":          keys.activeUsers,
		"known users":           keys.knownUsers,
		"activation generation": keys.activationGeneration,
		"processing turns":      keys.processingTurns,
		"global inflight":       keys.globalInflight,
		"provisional":           keys.provisional,
		"reservation metadata":  keys.reservationMetadata,
	}
	for name, want := range wants {
		if got := gots[name]; got != want {
			t.Errorf("%s key = %q, want %q", name, got, want)
		}
	}
	tenantKey, err := keys.tenantInflight("rag.index", "tenant-a")
	if err != nil {
		t.Fatalf("tenantInflight() error = %v", err)
	}
	all := append(keys.all(), tenantKey)
	for _, key := range all {
		open := strings.IndexByte(key, '{')
		close := strings.IndexByte(key, '}')
		if open < 0 || close < open || key[open:close+1] != "{rag.index}" ||
			strings.Contains(key[close+1:], "{") {
			t.Errorf("key %q does not use one first hash tag {rag.index}", key)
		}
	}
	for _, unsafe := range []string{"", " bkcrab:", "bkcrab:{x}:", "bkcrab:*:", "bkcrab:?:", "bkcrab:[x]:", "bkcrab:\\:"} {
		if _, err := buildRedisKeyspace(unsafe, "rag.index"); !errors.Is(err, ErrInvalidModel) {
			t.Errorf("buildRedisKeyspace(%q) error = %v, want ErrInvalidModel", unsafe, err)
		}
	}
}

func TestRedisScriptAndDependencyErrorClassification(t *testing.T) {
	for _, test := range []struct {
		code string
		want error
	}{
		{redisResultNotReady, ErrResourceNotReady},
		{redisResultFenceMismatch, ErrFenceMismatch},
		{redisResultInvalidState, ErrFenceMismatch},
		{redisResultRecoveryOwnerStale, ErrRecoveryOwnerStale},
		{redisResultCoordinationCorrupt, ErrCoordinationCorrupt},
		{"unknown-code-with-sensitive-detail", ErrCoordinationCorrupt},
	} {
		err := redisScriptError(test.code)
		if !errors.Is(err, test.want) {
			t.Errorf("redisScriptError(%q) = %v, want %v", test.code, err, test.want)
		}
		if strings.Contains(err.Error(), test.code) {
			t.Errorf("redisScriptError(%q) leaked the raw result: %v", test.code, err)
		}
	}
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{"canceled", context.Canceled, context.Canceled},
		{"deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		{"other", errors.New("redis://user:secret@example.invalid tenant-a token-a"), nil},
	} {
		classified := redisDependencyError(test.name, test.err)
		if !errors.Is(classified, ErrDependencyUnavailable) {
			t.Errorf("redisDependencyError(%s) = %v, want ErrDependencyUnavailable", test.name, classified)
		}
		if test.want != nil && !errors.Is(classified, test.want) {
			t.Errorf("redisDependencyError(%s) = %v, want %v", test.name, classified, test.want)
		}
		for _, secret := range []string{"secret", "tenant-a", "token-a", "example.invalid"} {
			if strings.Contains(classified.Error(), secret) {
				t.Errorf("redisDependencyError(%s) leaked %q: %v", test.name, secret, classified)
			}
		}
	}
}

func TestRedisRejectsUnsafeKeyPrefixBeforeNetworkIO(t *testing.T) {
	for _, prefix := range []string{
		"bkcrab:{escape}:",
		"bkcrab:}:illegal:",
		"bkcrab:*:glob:",
		"bkcrab:?:glob:",
		"bkcrab:[abc]:glob:",
	} {
		t.Run(prefix, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("net.Listen() error = %v", err)
			}
			defer listener.Close()
			secret := "redis-test-password-must-not-leak"
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			client, err := NewRedis(ctx, RedisOptions{
				Addr:             listener.Addr().String(),
				Password:         secret,
				KeyPrefix:        prefix,
				OperationTimeout: 100 * time.Millisecond,
			})
			if client != nil {
				_ = client.Close()
				t.Fatal("NewRedis() returned a client for an unsafe key prefix")
			}
			if !errors.Is(err, ErrInvalidModel) {
				t.Fatalf("NewRedis() error = %v, want ErrInvalidModel", err)
			}
			message := err.Error()
			for _, forbidden := range []string{listener.Addr().String(), secret, prefix} {
				if strings.Contains(message, forbidden) {
					t.Fatalf("NewRedis() error leaked sensitive configuration %q: %v", forbidden, err)
				}
			}

			tcpListener, ok := listener.(*net.TCPListener)
			if !ok {
				t.Fatalf("listener type = %T, want *net.TCPListener", listener)
			}
			if deadlineErr := tcpListener.SetDeadline(time.Now().Add(30 * time.Millisecond)); deadlineErr != nil {
				t.Fatalf("SetDeadline() error = %v", deadlineErr)
			}
			connection, acceptErr := tcpListener.Accept()
			if acceptErr == nil {
				_ = connection.Close()
				t.Fatal("NewRedis() performed network I/O before rejecting the key prefix")
			}
			var networkError net.Error
			if !errors.As(acceptErr, &networkError) || !networkError.Timeout() {
				t.Fatalf("Accept() error = %v, want a timeout proving zero connections", acceptErr)
			}
		})
	}
}

func TestRedisTopologyProbeRejectsClusterBeforeRoleOrCoordinationIO(t *testing.T) {
	options := RedisOptions{
		Addr:             "redis.invalid:6379",
		KeyPrefix:        "bkcrab:",
		OperationTimeout: time.Second,
	}
	cluster := &redisTopologyProbeClient{
		clusterInfo: "# Cluster\r\ncluster_enabled:1\r\n",
		role:        []interface{}{"master"},
	}
	coordinator, err := newRedis(
		context.Background(),
		options,
		cluster,
		redisFixedTokenSource{token: strings.Repeat("a", 32)},
	)
	if coordinator != nil || !errors.Is(err, ErrUnsupportedTopology) {
		t.Fatalf("newRedis(cluster) = (%#v,%v), want ErrUnsupportedTopology", coordinator, err)
	}
	if cluster.doCalls != 0 || cluster.scanCalls != 0 || cluster.scriptCalls != 0 {
		t.Fatalf("cluster probe continued after INFO: do=%d scan=%d script=%d", cluster.doCalls, cluster.scanCalls, cluster.scriptCalls)
	}

	replica := &redisTopologyProbeClient{
		clusterInfo: "# Cluster\r\ncluster_enabled:0\r\n",
		role:        []interface{}{"slave", "127.0.0.1", int64(6379)},
	}
	coordinator, err = newRedis(
		context.Background(),
		options,
		replica,
		redisFixedTokenSource{token: strings.Repeat("a", 32)},
	)
	if coordinator != nil || !errors.Is(err, ErrUnsupportedTopology) {
		t.Fatalf("newRedis(replica) = (%#v,%v), want ErrUnsupportedTopology", coordinator, err)
	}
	if replica.doCalls != 1 || replica.scanCalls != 0 || replica.scriptCalls != 0 {
		t.Fatalf("replica probe I/O = do:%d scan:%d script:%d", replica.doCalls, replica.scanCalls, replica.scriptCalls)
	}

	primary := &redisTopologyProbeClient{
		clusterInfo: "# Cluster\r\ncluster_enabled:0\r\n",
		role:        []interface{}{"master", int64(0), []interface{}{}},
	}
	coordinator, err = newRedis(
		context.Background(),
		options,
		primary,
		redisFixedTokenSource{token: strings.Repeat("a", 32)},
	)
	if err != nil || coordinator == nil {
		t.Fatalf("newRedis(primary) = (%#v,%v)", coordinator, err)
	}
	if primary.doCalls != 1 || primary.scanCalls != 0 || primary.scriptCalls != 0 {
		t.Fatalf("primary constructor performed coordination I/O: do=%d scan=%d script=%d", primary.doCalls, primary.scanCalls, primary.scriptCalls)
	}
}

func TestRedisStandaloneProbe(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	topology, err := fixture.coordinator.InspectRedisTopology(ctx)
	if err != nil {
		t.Fatalf("InspectRedisTopology() error = %v", err)
	}
	if topology.Mode != RedisDeploymentStandalone || !topology.WritablePrimary ||
		!topology.SupportsFairQueue() {
		t.Fatalf("topology = %#v, want standalone writable primary", topology)
	}
}

func TestRedisProbeResourceHealthIsReadOnlyAndSanitized(t *testing.T) {
	epoch := strings.Repeat("1", 32)
	client := &redisTopologyProbeClient{
		clusterInfo: "# Cluster\r\ncluster_enabled:0\r\n",
		role:        []interface{}{"master", int64(0), []interface{}{}},
		scriptResults: []redisTestScriptResult{
			{value: []interface{}{"OK", "1", "READY", epoch, "1", redisTestWriterA, "NONE", "", "", "NONE"}},
			{value: []interface{}{"OK", "2", "3"}},
		},
	}
	coordinator := &Redis{
		options: RedisOptions{KeyPrefix: "bkcrab:", OperationTimeout: time.Second},
		client:  client, tokens: redisFixedTokenSource{token: strings.Repeat("a", 32)},
	}

	probe, err := coordinator.ProbeResourceHealth(context.Background(), "rag.index")
	if err != nil {
		t.Fatalf("ProbeResourceHealth() error = %v", err)
	}
	if probe.Resource != "rag.index" ||
		probe.Topology != (RedisTopology{Mode: RedisDeploymentStandalone, WritablePrimary: true}) ||
		!probe.Control.Present || probe.Control.State != ResourceReady ||
		probe.Control.Epoch != epoch || probe.Control.WriterFingerprint != redisTestWriterA ||
		probe.ProvisionalCount != 2 || probe.ProcessingCount != 3 {
		t.Fatalf("ProbeResourceHealth() = %+v", probe)
	}
	if client.doCalls != 1 || client.scanCalls != 0 || client.scriptCalls != 2 {
		t.Fatalf("probe I/O = do:%d scan:%d script:%d", client.doCalls, client.scanCalls, client.scriptCalls)
	}
	encoded := fmt.Sprintf("%+v", probe)
	for _, forbidden := range []string{"redis.invalid", "password", "bkcrab:", "tenant-", "task-", "owner-token"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("resource probe leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestRedisProbeResourceHealthFailsClosed(t *testing.T) {
	tests := []struct {
		name        string
		client      *redisTopologyProbeClient
		want        error
		wantDoCalls int
		wantScripts int
	}{
		{
			name:   "topology dependency unavailable",
			client: &redisTopologyProbeClient{infoErr: context.DeadlineExceeded},
			want:   context.DeadlineExceeded,
		},
		{
			name: "unsupported cluster topology",
			client: &redisTopologyProbeClient{
				clusterInfo: "# Cluster\r\ncluster_enabled:1\r\n",
				role:        []interface{}{"master"},
			},
			want: ErrUnsupportedTopology,
		},
		{
			name: "corrupt control",
			client: &redisTopologyProbeClient{
				clusterInfo:   "# Cluster\r\ncluster_enabled:0\r\n",
				role:          []interface{}{"master"},
				scriptResults: []redisTestScriptResult{{value: []interface{}{redisResultCoordinationCorrupt}}},
			},
			want: ErrCoordinationCorrupt, wantDoCalls: 1, wantScripts: 1,
		},
		{
			name: "malformed counts",
			client: &redisTopologyProbeClient{
				clusterInfo: "# Cluster\r\ncluster_enabled:0\r\n",
				role:        []interface{}{"master"},
				scriptResults: []redisTestScriptResult{
					{value: []interface{}{"OK", "0"}},
					{value: []interface{}{"OK", "not-a-count", "0"}},
				},
			},
			want: ErrCoordinationCorrupt, wantDoCalls: 1, wantScripts: 2,
		},
		{
			name: "negative counts",
			client: &redisTopologyProbeClient{
				clusterInfo: "# Cluster\r\ncluster_enabled:0\r\n",
				role:        []interface{}{"master"},
				scriptResults: []redisTestScriptResult{
					{value: []interface{}{"OK", "0"}},
					{value: []interface{}{"OK", "-1", "0"}},
				},
			},
			want: ErrCoordinationCorrupt, wantDoCalls: 1, wantScripts: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := &Redis{
				options: RedisOptions{KeyPrefix: "bkcrab:", OperationTimeout: time.Second},
				client:  test.client, tokens: redisFixedTokenSource{token: strings.Repeat("a", 32)},
			}
			probe, err := coordinator.ProbeResourceHealth(context.Background(), "rag.index")
			if probe != (RedisResourceHealthProbe{}) || !errors.Is(err, test.want) {
				t.Fatalf("ProbeResourceHealth() = %+v, %v, want %v", probe, err, test.want)
			}
			if test.want == context.DeadlineExceeded && !errors.Is(err, ErrDependencyUnavailable) {
				t.Fatalf("dependency probe error = %v, want ErrDependencyUnavailable", err)
			}
			if test.client.doCalls != test.wantDoCalls || test.client.scriptCalls != test.wantScripts || test.client.scanCalls != 0 {
				t.Fatalf("failed probe I/O = do:%d script:%d scan:%d", test.client.doCalls, test.client.scriptCalls, test.client.scanCalls)
			}
		})
	}
}

func TestRedisProbeResourceHealthRejectsInvalidResourceBeforeIO(t *testing.T) {
	client := &redisTopologyProbeClient{}
	coordinator := &Redis{
		options: RedisOptions{KeyPrefix: "bkcrab:", OperationTimeout: time.Second},
		client:  client, tokens: redisFixedTokenSource{token: strings.Repeat("a", 32)},
	}
	probe, err := coordinator.ProbeResourceHealth(context.Background(), "invalid resource")
	if probe != (RedisResourceHealthProbe{}) || !errors.Is(err, ErrInvalidModel) {
		t.Fatalf("invalid resource probe = %+v, %v", probe, err)
	}
	if client.doCalls != 0 || client.scriptCalls != 0 || client.scanCalls != 0 {
		t.Fatalf("invalid resource performed I/O: do=%d script=%d scan=%d", client.doCalls, client.scriptCalls, client.scanCalls)
	}
}

func TestRedisKeyNamespaceUsesExactResourceHashTag(t *testing.T) {
	unique := redisTestRandomHex(t, 8)
	resource := "test.redis." + unique
	fixture := newRedisIntegrationFixtureWithNamespace(t, "bkcrab:", resource)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	if err := fixture.coordinator.Activate(ctx, resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	if _, err := fixture.coordinator.AcquireProvisional(
		ctx, resource, fence, "tenant-a", redisTestRandomHex(t, 16), limits, time.Second,
	); err != nil {
		t.Fatalf("AcquireProvisional() error = %v", err)
	}

	root := "bkcrab:fair:{" + resource + "}:"
	var cursor uint64
	seen := make(map[string]bool)
	for {
		keys, next, err := fixture.raw.Scan(ctx, cursor, root+"*", 100).Result()
		if err != nil {
			t.Fatalf("SCAN key namespace error = %v", err)
		}
		for _, key := range keys {
			if !strings.HasPrefix(key, root) {
				t.Fatalf("key %q does not start with exact root %q", key, root)
			}
			open := strings.IndexByte(key, '{')
			close := strings.IndexByte(key, '}')
			if open < 0 || close < open || key[open:close+1] != "{"+resource+"}" ||
				strings.Contains(key[close+1:], "{") {
				t.Fatalf("key %q does not have the resource as its first and only hash tag", key)
			}
			seen[strings.TrimPrefix(key, root)] = true
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	tenantHash, err := TenantHash(resource, "tenant-a")
	if err != nil {
		t.Fatalf("TenantHash() error = %v", err)
	}
	for _, suffix := range []string{
		"resource",
		"ring",
		"ring_members",
		"active_users",
		"known_users",
		"activation_gen",
		"inflight",
		"inflight:tenant:" + tenantHash,
		"reservation_meta",
		"provisional",
	} {
		if !seen[suffix] {
			t.Errorf("expected key %q was not created; suffixes = %#v", root+suffix, seen)
		}
	}
}

func TestRedisReadyFenceFailsClosed(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	ctx, cancel := redisTestContext(t)
	defer cancel()

	if _, err := fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, redisTestWriterA); !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("ObserveReadyFence() missing control error = %v, want ErrResourceNotReady", err)
	}
	missingFence := ResourceFence{Epoch: strings.Repeat("a", 32), WriterFingerprint: redisTestWriterA}
	if err := fixture.coordinator.Activate(ctx, fixture.resource, missingFence, "tenant-a"); !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("Activate() missing control error = %v, want ErrResourceNotReady", err)
	}

	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	if err := fixture.coordinator.CheckReadyFence(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("CheckReadyFence() error = %v", err)
	}
	if _, err := fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, redisTestWriterB); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("ObserveReadyFence() wrong writer error = %v, want ErrFenceMismatch", err)
	}
	wrongEpoch := fence
	wrongEpoch.Epoch = strings.Repeat("f", 32)
	if err := fixture.coordinator.EnsureKnownTenant(ctx, fixture.resource, wrongEpoch, "tenant-a"); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("EnsureKnownTenant() wrong epoch error = %v, want ErrFenceMismatch", err)
	}
	wrongWriter := fence
	wrongWriter.WriterFingerprint = redisTestWriterB
	if _, _, err := fixture.coordinator.NextTurn(
		ctx, fixture.resource, wrongWriter, ProcessingTurnToken(redisTestRandomHex(t, 16)), time.Second,
	); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("NextTurn() wrong writer error = %v, want ErrFenceMismatch", err)
	}
}

func TestRedisSpecialBeginCanRehydrateMissingControlWithPersistedIdentity(t *testing.T) {
	for _, kind := range []RecoveryKind{
		RecoveryRabbitRepair,
		RecoveryWriterRebind,
		RecoveryForceRebuild,
	} {
		t.Run(string(kind), func(t *testing.T) {
			fixture := newRedisIntegrationFixture(t)
			ctx, cancel := redisTestContext(t)
			defer cancel()
			lock, err := fixture.coordinator.AcquireRecoveryLock(
				ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
			)
			if err != nil {
				t.Fatalf("AcquireRecoveryLock() error = %v", err)
			}
			operationID := redisTestRandomHex(t, 16)
			var fence RecoveryFence
			switch kind {
			case RecoveryRabbitRepair:
				fence, err = fixture.coordinator.BeginRabbitRepairWithLock(
					ctx, fixture.resource, redisTestWriterA, operationID, lock, 5*time.Second,
				)
			case RecoveryWriterRebind:
				fence, err = fixture.coordinator.BeginWriterRebindWithLock(
					ctx,
					fixture.resource,
					redisTestWriterA,
					redisTestWriterB,
					operationID,
					lock,
					5*time.Second,
				)
			case RecoveryForceRebuild:
				deadline, deadlineErr := fixture.coordinator.ComputeForceRebuildDeadlineWithLock(
					ctx, fixture.resource, lock, time.Second,
				)
				if deadlineErr != nil {
					t.Fatalf("ComputeForceRebuildDeadlineWithLock() error = %v", deadlineErr)
				}
				fence, err = fixture.coordinator.BeginForceRebuildWithLock(
					ctx,
					fixture.resource,
					redisTestWriterA,
					operationID,
					deadline.NotBefore.UnixMilli(),
					lock,
					5*time.Second,
				)
			}
			if err != nil {
				t.Fatalf("Begin %s from missing control error = %v", kind, err)
			}
			if fence.Kind != kind || fence.OperationID != operationID ||
				fence.OwnerToken != lock.OwnerToken {
				t.Fatalf("rehydrated fence = %#v", fence)
			}
			snapshot, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
			if err != nil {
				t.Fatalf("InspectRecoveryStart() error = %v", err)
			}
			if !snapshot.Present || snapshot.State != ResourceRecovering ||
				snapshot.Kind != kind || snapshot.OperationID != operationID || snapshot.Progress == nil {
				t.Fatalf("rehydrated control = %#v", snapshot)
			}
		})
	}
}

func TestRedisOperationTimeoutIsTypedUnavailable(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Millisecond))
	defer cancel()
	err := fixture.coordinator.CheckReadyFence(ctx, fixture.resource, fence)
	if !errors.Is(err, ErrDependencyUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CheckReadyFence(expired context) error = %v, want dependency unavailable + deadline", err)
	}
}

func TestRedisRingRoundRobinAndActivationGenerationRace(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()

	for _, tenant := range []string{"tenant-a", "tenant-b", "tenant-c"} {
		if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, tenant); err != nil {
			t.Fatalf("Activate(%s) error = %v", tenant, err)
		}
	}
	var sequence []string
	for index := 0; index < 5; index++ {
		token := ProcessingTurnToken(redisTestRandomHex(t, 16))
		turn, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, token, time.Second)
		if err != nil || !ok {
			t.Fatalf("NextTurn(%d) = (%#v,%v,%v), want a turn", index, turn, ok, err)
		}
		sequence = append(sequence, turn.TenantID)
		if err := fixture.coordinator.RotateOrDeactivate(
			ctx, fixture.resource, fence, token, turn.ObservedActivationGeneration, true,
		); err != nil {
			t.Fatalf("RotateOrDeactivate(%d) error = %v", index, err)
		}
	}
	if got, want := strings.Join(sequence, ","), "tenant-a,tenant-b,tenant-c,tenant-a,tenant-b"; got != want {
		t.Fatalf("round-robin sequence = %q, want %q", got, want)
	}

	// Drain C from the ring, then exercise a publish-vs-empty observation race
	// on A in an otherwise fresh resource. Activate while A owns its turn must
	// advance the generation without inserting a duplicate ring member.
	turnToken := ProcessingTurnToken(redisTestRandomHex(t, 16))
	turn, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, turnToken, time.Second)
	if err != nil || !ok {
		t.Fatalf("NextTurn() for generation race = (%#v,%v,%v)", turn, ok, err)
	}
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, turn.TenantID); err != nil {
		t.Fatalf("Activate() during processing error = %v", err)
	}
	if err := fixture.coordinator.RotateOrDeactivate(
		ctx, fixture.resource, fence, turnToken, turn.ObservedActivationGeneration, false,
	); err != nil {
		t.Fatalf("RotateOrDeactivate() generation race error = %v", err)
	}

	seen := 0
	for index := 0; index < 3; index++ {
		token := ProcessingTurnToken(redisTestRandomHex(t, 16))
		next, exists, nextErr := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, token, time.Second)
		if nextErr != nil || !exists {
			t.Fatalf("NextTurn() after generation race = (%#v,%v,%v)", next, exists, nextErr)
		}
		if next.TenantID == turn.TenantID {
			seen++
		}
		if err := fixture.coordinator.RotateOrDeactivate(
			ctx, fixture.resource, fence, token, next.ObservedActivationGeneration, false,
		); err != nil {
			t.Fatalf("RotateOrDeactivate() drain error = %v", err)
		}
	}
	if seen != 1 {
		t.Fatalf("tenant %q appeared %d times after processing activation, want exactly once", turn.TenantID, seen)
	}
	if _, exists, err := fixture.coordinator.NextTurn(
		ctx, fixture.resource, fence, ProcessingTurnToken(redisTestRandomHex(t, 16)), time.Second,
	); err != nil || exists {
		t.Fatalf("NextTurn() after draining ring = (_, %v, %v), want empty", exists, err)
	}
}

func TestRedisConcurrentEnsureActiveAndCapacityNeverDuplicateOrOverbook(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()

	var ensureWait sync.WaitGroup
	ensureErrors := make(chan error, 32)
	for index := 0; index < 32; index++ {
		ensureWait.Add(1)
		go func() {
			defer ensureWait.Done()
			ensureErrors <- fixture.coordinator.EnsureActive(
				ctx, fixture.resource, fence, "tenant-a",
			)
		}()
	}
	ensureWait.Wait()
	close(ensureErrors)
	for err := range ensureErrors {
		if err != nil {
			t.Fatalf("concurrent EnsureActive() error = %v", err)
		}
	}
	turnToken := ProcessingTurnToken(redisTestRandomHex(t, 16))
	turn, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, turnToken, 5*time.Second)
	if err != nil || !ok || turn.TenantID != "tenant-a" {
		t.Fatalf("NextTurn() after concurrent EnsureActive = (%#v,%v,%v)", turn, ok, err)
	}
	if duplicate, exists, err := fixture.coordinator.NextTurn(
		ctx, fixture.resource, fence, ProcessingTurnToken(redisTestRandomHex(t, 16)), 5*time.Second,
	); err != nil || exists {
		t.Fatalf("concurrent duplicate NextTurn() = (%#v,%v,%v), want empty while processing", duplicate, exists, err)
	}
	if err := fixture.coordinator.RotateOrDeactivate(
		ctx, fixture.resource, fence, turnToken, turn.ObservedActivationGeneration, true,
	); err != nil {
		t.Fatalf("RotateOrDeactivate() error = %v", err)
	}

	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	type acquireResult struct {
		attempt  string
		decision ReservationDecision
		err      error
	}
	results := make(chan acquireResult, 32)
	var acquireWait sync.WaitGroup
	for index := 0; index < 32; index++ {
		attempt := redisTestRandomHex(t, 16)
		acquireWait.Add(1)
		go func() {
			defer acquireWait.Done()
			decision, acquireErr := fixture.coordinator.AcquireProvisional(
				ctx, fixture.resource, fence, "tenant-a", attempt, limits, 5*time.Second,
			)
			results <- acquireResult{attempt: attempt, decision: decision, err: acquireErr}
		}()
	}
	acquireWait.Wait()
	close(results)
	granted := 0
	var grantedAttempts []string
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent AcquireProvisional() error = %v", result.err)
		}
		switch result.decision {
		case ReservationRegular, ReservationBorrowed:
			granted++
			grantedAttempts = append(grantedAttempts, result.attempt)
		case ReservationDeniedGlobalFull:
		default:
			t.Fatalf("unexpected concurrent reservation decision %q", result.decision)
		}
	}
	if granted != limits.GlobalConcurrency {
		t.Fatalf("concurrent grants = %d, want exactly global limit %d", granted, limits.GlobalConcurrency)
	}
	globalCount, err := fixture.raw.ZCard(ctx, redisTestRoot(fixture)+"inflight").Result()
	if err != nil {
		t.Fatalf("ZCARD global inflight error = %v", err)
	}
	if globalCount != int64(limits.GlobalConcurrency) {
		t.Fatalf("global inflight = %d, want %d", globalCount, limits.GlobalConcurrency)
	}
	for _, attempt := range grantedAttempts {
		if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-a", attempt); err != nil {
			t.Fatalf("Release(%s) error = %v", attempt, err)
		}
	}
}

func TestRedisTurnLeaseReapIsIdempotentAndStaleTokenCannotMutate(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()

	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	oldToken := ProcessingTurnToken(redisTestRandomHex(t, 16))
	turn, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, oldToken, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("NextTurn() = (%#v,%v,%v)", turn, ok, err)
	}
	redisTestExpireSortedSetMembers(t, ctx, fixture, "processing_turns")
	redisTestEventually(t, 2*time.Second, func() bool {
		result, reapErr := fixture.coordinator.ReapExpiredTurnsAndProvisionals(
			ctx, fixture.resource, fence, 10,
		)
		return reapErr == nil && result.RemovedTurns == 1
	})
	second, err := fixture.coordinator.ReapExpiredTurnsAndProvisionals(ctx, fixture.resource, fence, 10)
	if err != nil {
		t.Fatalf("second ReapExpiredTurnsAndProvisionals() error = %v", err)
	}
	if second.RemovedTurns != 0 {
		t.Fatalf("second reap removed %d turns, want zero", second.RemovedTurns)
	}

	newToken := ProcessingTurnToken(redisTestRandomHex(t, 16))
	newTurn, ok, err := fixture.coordinator.NextTurn(ctx, fixture.resource, fence, newToken, time.Second)
	if err != nil || !ok || newTurn.TenantID != "tenant-a" {
		t.Fatalf("NextTurn() after reap = (%#v,%v,%v)", newTurn, ok, err)
	}
	if err := fixture.coordinator.RotateOrDeactivate(
		ctx, fixture.resource, fence, oldToken, turn.ObservedActivationGeneration, false,
	); err == nil {
		t.Fatal("stale turn token rotated the new turn")
	}
	if err := fixture.coordinator.RotateOrDeactivate(
		ctx, fixture.resource, fence, newToken, newTurn.ObservedActivationGeneration, false,
	); err != nil {
		t.Fatalf("current turn failed after stale-token attempt: %v", err)
	}
}

func TestRedisReservationLimitsBorrowBindRenewAndRelease(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	limits := CapacityLimits{
		GlobalConcurrency:       4,
		PerUserBaseConcurrency:  2,
		PerUserBurstConcurrency: 4,
		BorrowEnabled:           true,
	}

	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate(tenant-a) error = %v", err)
	}
	attempts := make([]string, 4)
	for index := range attempts {
		attempts[index] = redisTestRandomHex(t, 16)
		decision, err := fixture.coordinator.AcquireProvisional(
			ctx, fixture.resource, fence, "tenant-a", attempts[index], limits, time.Second,
		)
		if err != nil {
			t.Fatalf("AcquireProvisional(%d) error = %v", index, err)
		}
		want := ReservationRegular
		if index >= 2 {
			want = ReservationBorrowed
		}
		if decision != want {
			t.Fatalf("AcquireProvisional(%d) decision = %q, want %q", index, decision, want)
		}
	}
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", redisTestRandomHex(t, 16), limits, time.Second,
	); err != nil || decision != ReservationDeniedGlobalFull {
		t.Fatalf("fifth AcquireProvisional() = (%q,%v), want global-full", decision, err)
	}

	stable, err := StableReservationToken(fixture.resource, "1", 1)
	if err != nil {
		t.Fatalf("StableReservationToken() error = %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-a", attempts[0], stable, 2*time.Second,
	); err != nil {
		t.Fatalf("BindReservation() error = %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-a", attempts[0], stable, 2*time.Second,
	); err != nil {
		t.Fatalf("duplicate BindReservation() error = %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-b", attempts[1], stable, 2*time.Second,
	); err == nil {
		t.Fatal("wrong tenant bound a provisional reservation")
	}

	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-b", stable); err == nil {
		t.Fatal("wrong tenant released a stable reservation")
	}
	if err := fixture.coordinator.RenewStable(
		ctx, fixture.resource, fence, "tenant-a", stable, 3*time.Second,
	); err != nil {
		t.Fatalf("RenewStable() error = %v", err)
	}
	page, err := fixture.coordinator.ListReadyStableInflight(ctx, fixture.resource, fence, "", 10)
	if err != nil {
		t.Fatalf("ListReadyStableInflight() error = %v", err)
	}
	if err := page.Validate("", 10); err != nil {
		t.Fatalf("stable inflight page invalid: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].StableToken != stable || page.Items[0].TenantID != "tenant-a" {
		t.Fatalf("stable inflight page = %#v, want one bound token", page)
	}
	redisNow, err := fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	if remaining := time.UnixMilli(page.Items[0].ExpiresAtUnixMS).Sub(redisNow); remaining < 2500*time.Millisecond {
		t.Fatalf("renewed stable TTL remaining = %s, want nearly full 3s", remaining)
	}

	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-b"); err != nil {
		t.Fatalf("Activate(tenant-b) error = %v", err)
	}
	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-b", attempts[3]); err == nil {
		t.Fatal("wrong tenant released a provisional reservation")
	}
	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-a", attempts[3]); err != nil {
		t.Fatalf("Release(provisional) error = %v", err)
	}
	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-a", attempts[3]); err != nil {
		t.Fatalf("duplicate Release(provisional) error = %v", err)
	}
	if err := fixture.coordinator.Release(
		ctx, fixture.resource, fence, "tenant-a", redisTestRandomHex(t, 16),
	); err != nil {
		t.Fatalf("Release(missing provisional) error = %v", err)
	}
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", redisTestRandomHex(t, 16), limits, time.Second,
	); err != nil || decision != ReservationDeniedCompetition {
		t.Fatalf("competing tenant borrow = (%q,%v), want denied competition", decision, err)
	}
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-b", redisTestRandomHex(t, 16), limits, time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("tenant-b regular reservation = (%q,%v)", decision, err)
	}

	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-a", stable); err != nil {
		t.Fatalf("Release(stable) error = %v", err)
	}
	if err := fixture.coordinator.Release(ctx, fixture.resource, fence, "tenant-a", stable); err != nil {
		t.Fatalf("duplicate Release(stable) error = %v", err)
	}
	reconciledStable, err := StableReservationToken(fixture.resource, "2", 9)
	if err != nil {
		t.Fatalf("StableReservationToken(reconciled) error = %v", err)
	}
	if err := fixture.coordinator.EnsureReadyStableInflight(
		ctx, fixture.resource, fence, "tenant-a", reconciledStable, 2*time.Second,
	); err != nil {
		t.Fatalf("EnsureReadyStableInflight() without provisional error = %v", err)
	}
	if err := fixture.coordinator.EnsureReadyStableInflight(
		ctx, fixture.resource, fence, "tenant-a", reconciledStable, 2*time.Second,
	); err != nil {
		t.Fatalf("duplicate EnsureReadyStableInflight() error = %v", err)
	}
	reconciledPage, err := fixture.coordinator.ListReadyStableInflight(
		ctx, fixture.resource, fence, "", 10,
	)
	if err != nil {
		t.Fatalf("ListReadyStableInflight() after ensure error = %v", err)
	}
	foundReconciled := false
	for _, item := range reconciledPage.Items {
		foundReconciled = foundReconciled || item.StableToken == reconciledStable
	}
	if !foundReconciled {
		t.Fatalf("ensured stable token absent from page: %#v", reconciledPage)
	}
	if err := fixture.coordinator.Release(
		ctx, fixture.resource, fence, "tenant-a", reconciledStable,
	); err != nil {
		t.Fatalf("Release(reconciled stable) error = %v", err)
	}
}

func TestRedisBindUsesFreshServerTimeTTLAndExpiredProvisionalReaps(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}

	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	attempt := redisTestRandomHex(t, 16)
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", attempt, limits, 5*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional() = (%q,%v)", decision, err)
	}
	root := redisTestRoot(fixture)
	members, err := fixture.raw.ZRange(ctx, root+"provisional", 0, -1).Result()
	if err != nil || len(members) != 1 {
		t.Fatalf("provisional members = (%#v,%v), want one", members, err)
	}
	redisNow, err := fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	nearExpiry := float64(redisNow.Add(400 * time.Millisecond).UnixMilli())
	tenantHash, err := TenantHash(fixture.resource, "tenant-a")
	if err != nil {
		t.Fatalf("TenantHash() error = %v", err)
	}
	for _, key := range []string{
		root + "provisional",
		root + "inflight",
		root + "inflight:tenant:" + tenantHash,
	} {
		if err := fixture.raw.ZAdd(ctx, key, redisv9.Z{Score: nearExpiry, Member: members[0]}).Err(); err != nil {
			t.Fatalf("move provisional near expiry in %q: %v", key, err)
		}
	}
	stable, err := StableReservationToken(fixture.resource, "11", 3)
	if err != nil {
		t.Fatalf("StableReservationToken() error = %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, fence, "tenant-a", attempt, stable, 2*time.Second,
	); err != nil {
		t.Fatalf("BindReservation() near provisional expiry error = %v", err)
	}
	page, err := fixture.coordinator.ListReadyStableInflight(ctx, fixture.resource, fence, "", 10)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("ListReadyStableInflight() = (%#v,%v)", page, err)
	}
	redisNow, err = fixture.raw.Time(ctx).Result()
	if err != nil {
		t.Fatalf("Redis TIME error = %v", err)
	}
	if remaining := time.UnixMilli(page.Items[0].ExpiresAtUnixMS).Sub(redisNow); remaining < 1500*time.Millisecond {
		t.Fatalf("stable token inherited provisional expiry; remaining = %s", remaining)
	}

	expiringAttempt := redisTestRandomHex(t, 16)
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, fence, "tenant-a", expiringAttempt, limits, 5*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional(expiring) = (%q,%v)", decision, err)
	}
	redisTestExpireSortedSetMembers(t, ctx, fixture, "provisional")
	redisTestEventually(t, 2*time.Second, func() bool {
		result, reapErr := fixture.coordinator.ReapExpiredTurnsAndProvisionals(
			ctx, fixture.resource, fence, 10,
		)
		return reapErr == nil && result.RemovedProvisionals == 1
	})
	result, err := fixture.coordinator.ReapExpiredTurnsAndProvisionals(ctx, fixture.resource, fence, 10)
	if err != nil {
		t.Fatalf("second ReapExpiredTurnsAndProvisionals() error = %v", err)
	}
	if result.RemovedProvisionals != 0 {
		t.Fatalf("second reap removed %d provisionals, want zero", result.RemovedProvisionals)
	}
}

func TestRedisExpiredCleanupCorruptMetadataHasZeroPartialMutation(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	fence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}
	if err := fixture.coordinator.Activate(ctx, fixture.resource, fence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	for index := 0; index < 2; index++ {
		if decision, err := fixture.coordinator.AcquireProvisional(
			ctx,
			fixture.resource,
			fence,
			"tenant-a",
			redisTestRandomHex(t, 16),
			limits,
			5*time.Second,
		); err != nil || decision != ReservationRegular {
			t.Fatalf("AcquireProvisional(%d) = (%q,%v)", index, decision, err)
		}
	}
	root := fixture.prefix + "fair:{" + fixture.resource + "}:"
	provisionalKey := root + "provisional"
	globalKey := root + "inflight"
	metadataKey := root + "reservation_meta"
	tenantHash, err := TenantHash(fixture.resource, "tenant-a")
	if err != nil {
		t.Fatalf("TenantHash() error = %v", err)
	}
	tenantKey := root + "inflight:tenant:" + tenantHash
	ordered, err := fixture.raw.ZRange(ctx, provisionalKey, 0, -1).Result()
	if err != nil || len(ordered) != 2 {
		t.Fatalf("provisional members = (%#v,%v), want two", ordered, err)
	}
	originalMetadata, err := fixture.raw.HGet(ctx, metadataKey, ordered[1]).Result()
	if err != nil {
		t.Fatalf("HGET metadata for second provisional error = %v", err)
	}
	if err := fixture.raw.HSet(ctx, metadataKey, ordered[1], "corrupt").Err(); err != nil {
		t.Fatalf("corrupt reservation metadata: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = fixture.raw.HSet(cleanupCtx, metadataKey, ordered[1], originalMetadata).Err()
	})
	recoveryLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	recoveryFence, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, recoveryLock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock() error = %v", err)
	}

	type cleanupSnapshot struct {
		Global      []redisv9.Z
		Tenant      []redisv9.Z
		Provisional []redisv9.Z
		Metadata    map[string]string
	}
	snapshot := func() cleanupSnapshot {
		global, globalErr := fixture.raw.ZRangeWithScores(ctx, globalKey, 0, -1).Result()
		if globalErr != nil {
			t.Fatalf("ZRANGE global error = %v", globalErr)
		}
		tenant, tenantErr := fixture.raw.ZRangeWithScores(ctx, tenantKey, 0, -1).Result()
		if tenantErr != nil {
			t.Fatalf("ZRANGE tenant error = %v", tenantErr)
		}
		provisionals, provisionalErr := fixture.raw.ZRangeWithScores(ctx, provisionalKey, 0, -1).Result()
		if provisionalErr != nil {
			t.Fatalf("ZRANGE provisional error = %v", provisionalErr)
		}
		metadata, metadataErr := fixture.raw.HGetAll(ctx, metadataKey).Result()
		if metadataErr != nil {
			t.Fatalf("HGETALL metadata error = %v", metadataErr)
		}
		return cleanupSnapshot{Global: global, Tenant: tenant, Provisional: provisionals, Metadata: metadata}
	}

	redisTestExpireSortedSetMembers(t, ctx, fixture, "provisional")
	before := snapshot()
	if result, err := fixture.coordinator.RecoveryReapExpired(
		ctx, fixture.resource, recoveryFence, 10,
	); !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("RecoveryReapExpired() = (%#v,%v), want ErrCoordinationCorrupt", result, err)
	}
	after := snapshot()
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("cleanup partially mutated state before detecting later corruption:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRedisRecoveryOwnerEpochCleanupAndKnownOnlyBorrow(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	readyFence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}

	if err := fixture.coordinator.Activate(ctx, fixture.resource, readyFence, "tenant-a"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	turnToken := ProcessingTurnToken(redisTestRandomHex(t, 16))
	if _, ok, err := fixture.coordinator.NextTurn(
		ctx, fixture.resource, readyFence, turnToken, 5*time.Second,
	); err != nil || !ok {
		t.Fatalf("NextTurn() = (_, %v, %v)", ok, err)
	}
	provisional := redisTestRandomHex(t, 16)
	if decision, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, readyFence, "tenant-a", provisional, limits, 5*time.Second,
	); err != nil || decision != ReservationRegular {
		t.Fatalf("AcquireProvisional() = (%q,%v)", decision, err)
	}

	oldLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock(old) error = %v", err)
	}
	oldRecovery, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, oldLock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock(old) error = %v", err)
	}
	if oldRecovery.Epoch == readyFence.Epoch {
		t.Fatal("recovery did not rotate the resource epoch")
	}
	if err := fixture.coordinator.Activate(ctx, fixture.resource, readyFence, "tenant-b"); !errors.Is(err, ErrResourceNotReady) && !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("old READY fence Activate() error = %v", err)
	}
	cleanup, err := fixture.coordinator.RecoveryReapExpired(ctx, fixture.resource, oldRecovery, 10)
	if err != nil {
		t.Fatalf("RecoveryReapExpired(before expiry) error = %v", err)
	}
	if cleanup.RemovedProvisionals != 0 || cleanup.RemovedTurns != 0 ||
		cleanup.RemainingProvisionals != 1 || cleanup.RemainingTurns != 1 {
		t.Fatalf("cleanup before expiry = %#v, want both members retained", cleanup)
	}

	redisTestExpireRecoveryLock(t, ctx, fixture)
	newLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock(new) error = %v", err)
	}
	newRecovery, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, newLock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock(takeover) error = %v", err)
	}
	if newRecovery.Epoch == oldRecovery.Epoch {
		t.Fatal("recovery takeover reused the old resource epoch")
	}
	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, oldRecovery); err == nil {
		t.Fatal("stale recovery owner reset the new owner's resource")
	}
	redisTestExpireSortedSetMembers(t, ctx, fixture, "provisional")
	redisTestExpireSortedSetMembers(t, ctx, fixture, "processing_turns")
	redisTestEventually(t, 2*time.Second, func() bool {
		result, reapErr := fixture.coordinator.RecoveryReapExpired(ctx, fixture.resource, newRecovery, 10)
		return reapErr == nil && result.RemovedProvisionals == 1 && result.RemovedTurns == 1 &&
			result.RemainingProvisionals == 0 && result.RemainingTurns == 0
	})
	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, newRecovery); err != nil {
		t.Fatalf("ResetResource(new owner) error = %v", err)
	}
	if err := fixture.coordinator.RestoreKnownTenant(
		ctx, fixture.resource, newRecovery, "tenant-known-only",
	); err != nil {
		t.Fatalf("RestoreKnownTenant() error = %v", err)
	}
	if err := fixture.coordinator.RestoreActiveTenant(
		ctx, fixture.resource, newRecovery, "tenant-a",
	); err != nil {
		t.Fatalf("RestoreActiveTenant() error = %v", err)
	}
	redisTestFinishPreparedRecovery(t, ctx, fixture, newRecovery)

	readyFence, err = fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, redisTestWriterA)
	if err != nil {
		t.Fatalf("ObserveReadyFence() after takeover error = %v", err)
	}
	for index, want := range []ReservationDecision{ReservationRegular, ReservationRegular, ReservationBorrowed} {
		decision, acquireErr := fixture.coordinator.AcquireProvisional(
			ctx, fixture.resource, readyFence, "tenant-a", redisTestRandomHex(t, 16), limits, time.Second,
		)
		if acquireErr != nil || decision != want {
			t.Fatalf("AcquireProvisional(%d) = (%q,%v), want %q", index, decision, acquireErr, want)
		}
	}
}

func TestRedisFinishRecoveryRejectsIncompleteProgressWithoutMutation(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	_ = redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
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
	assertRejectedWithoutMutation := func(stage string) {
		t.Helper()
		before, inspectErr := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
		if inspectErr != nil {
			t.Fatalf("%s: InspectRecoveryStart(before) error = %v", stage, inspectErr)
		}
		if finishErr := fixture.coordinator.FinishRecovery(ctx, fixture.resource, fence); finishErr == nil {
			t.Fatalf("%s: FinishRecovery() unexpectedly succeeded", stage)
		}
		after, inspectErr := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
		if inspectErr != nil {
			t.Fatalf("%s: InspectRecoveryStart(after) error = %v", stage, inspectErr)
		}
		if !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: rejected FinishRecovery mutated state:\nbefore=%#v\nafter=%#v", stage, before, after)
		}
	}

	assertRejectedWithoutMutation("missing high-water")
	if err := fixture.coordinator.SetRecoveryHighWater(
		ctx, fixture.resource, fence, "test-high-water",
	); err != nil {
		t.Fatalf("SetRecoveryHighWater() error = %v", err)
	}
	assertRejectedWithoutMutation("missing all passes")
	for _, pass := range []RecoveryPass{RecoveryPassKnownTenants, RecoveryPassDispatched} {
		if err := fixture.coordinator.MarkRecoveryPass(
			ctx, fixture.resource, fence, pass, 1, true, 0,
		); err != nil {
			t.Fatalf("MarkRecoveryPass(%s) error = %v", pass, err)
		}
	}
	assertRejectedWithoutMutation("missing running pass")
	if err := fixture.coordinator.MarkRecoveryPass(
		ctx, fixture.resource, fence, RecoveryPassRunning, 1, true, 0,
	); err != nil {
		t.Fatalf("MarkRecoveryPass(RUNNING) error = %v", err)
	}
	if err := fixture.coordinator.FinishRecovery(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("FinishRecovery() after all gates error = %v", err)
	}
}

func TestRedisRenewRecoveryLockAndFenceRequireCurrentOwner(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	_ = redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	lock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	if err := fixture.coordinator.RenewRecoveryLock(
		ctx, fixture.resource, lock, 5*time.Second,
	); err != nil {
		t.Fatalf("RenewRecoveryLock() error = %v", err)
	}
	staleLock := RecoveryLock{OwnerToken: redisTestRandomHex(t, 16)}
	if err := fixture.coordinator.RenewRecoveryLock(
		ctx, fixture.resource, staleLock, 10*time.Second,
	); !errors.Is(err, ErrRecoveryOwnerStale) {
		t.Fatalf("RenewRecoveryLock(stale) error = %v, want ErrRecoveryOwnerStale", err)
	}
	beforeBeginTTL, err := fixture.raw.PTTL(ctx, redisTestRoot(fixture)+"recovery_lock").Result()
	if err != nil || beforeBeginTTL < 4*time.Second || beforeBeginTTL > 5*time.Second {
		t.Fatalf("raw-lock TTL after renew = %s, error = %v", beforeBeginTTL, err)
	}
	fence, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, lock, time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock() error = %v", err)
	}
	if err := fixture.coordinator.RenewRecovery(
		ctx, fixture.resource, fence, 5*time.Second,
	); err != nil {
		t.Fatalf("RenewRecovery() error = %v", err)
	}
	staleFence := fence
	staleFence.OwnerToken = staleLock.OwnerToken
	if err := fixture.coordinator.RenewRecovery(
		ctx, fixture.resource, staleFence, 10*time.Second,
	); !errors.Is(err, ErrRecoveryOwnerStale) && !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("RenewRecovery(stale) error = %v, want typed stale/fence", err)
	}
	afterBeginTTL, err := fixture.raw.PTTL(ctx, redisTestRoot(fixture)+"recovery_lock").Result()
	if err != nil || afterBeginTTL < 4*time.Second || afterBeginTTL > 5*time.Second {
		t.Fatalf("recovery-lock TTL after fenced renew = %s, error = %v", afterBeginTTL, err)
	}
	redisTestCompleteCommonRecovery(t, ctx, fixture, fence)
}

func TestRedisRecoveryOwnedKeyPaginationAndRestoreStable(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	readyFence := redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	limits := CapacityLimits{GlobalConcurrency: 4, PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true}

	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if err := fixture.coordinator.Activate(ctx, fixture.resource, readyFence, tenant); err != nil {
			t.Fatalf("Activate(%s) error = %v", tenant, err)
		}
	}
	attempt := redisTestRandomHex(t, 16)
	if _, err := fixture.coordinator.AcquireProvisional(
		ctx, fixture.resource, readyFence, "tenant-a", attempt, limits, 5*time.Second,
	); err != nil {
		t.Fatalf("AcquireProvisional() error = %v", err)
	}
	stable, err := StableReservationToken(fixture.resource, "21", 4)
	if err != nil {
		t.Fatalf("StableReservationToken() error = %v", err)
	}
	if err := fixture.coordinator.BindReservation(
		ctx, fixture.resource, readyFence, "tenant-a", attempt, stable, 5*time.Second,
	); err != nil {
		t.Fatalf("BindReservation() error = %v", err)
	}
	root := fixture.prefix + "fair:{" + fixture.resource + "}:"
	otherResource := fixture.resource + ".other"
	otherKey := fixture.prefix + "fair:{" + otherResource + "}:ring"
	if err := fixture.raw.Set(ctx, otherKey, "other-resource-sentinel", 0).Err(); err != nil {
		t.Fatalf("create other-resource sentinel key: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		_ = fixture.raw.Del(cleanupCtx, otherKey).Err()
	})

	lock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	recoveryFence, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, lock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRecoveryWithLock() error = %v", err)
	}
	criticalKeys := []string{root + "resource", root + "recovery_lock", root + "recovery_progress"}
	initialPage, err := fixture.coordinator.ListOwnedResourceKeys(
		ctx, fixture.resource, recoveryFence, "", 100,
	)
	if err != nil {
		t.Fatalf("initial ListOwnedResourceKeys() error = %v", err)
	}
	if len(initialPage.Items) == 0 {
		t.Fatal("initial ListOwnedResourceKeys() returned no rebuildable key")
	}
	for _, item := range initialPage.Items {
		for _, forbidden := range append(append([]string{}, criticalKeys...), otherKey) {
			if item.Key == forbidden {
				t.Fatalf("ListOwnedResourceKeys() returned excluded key %q", item.Key)
			}
		}
	}
	allowedKey := initialPage.Items[0].Key
	if err := fixture.coordinator.DeleteOwnedResourceKeys(
		ctx,
		fixture.resource,
		recoveryFence,
		[]RedisKeyRef{{Key: allowedKey}, {Key: root + "resource"}},
	); err == nil {
		t.Fatal("DeleteOwnedResourceKeys() accepted a batch containing the control key")
	}
	for _, preserved := range append(append([]string{allowedKey}, criticalKeys...), otherKey) {
		exists, existsErr := fixture.raw.Exists(ctx, preserved).Result()
		if existsErr != nil {
			t.Fatalf("EXISTS(%q) error = %v", preserved, existsErr)
		}
		if exists != 1 {
			t.Fatalf("rejected delete batch mutated protected/earlier key %q", preserved)
		}
	}

	var cursor string
	listed := 0
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		page, listErr := fixture.coordinator.ListOwnedResourceKeys(
			ctx, fixture.resource, recoveryFence, cursor, 1,
		)
		if listErr != nil {
			t.Fatalf("ListOwnedResourceKeys(%q) error = %v", cursor, listErr)
		}
		if validateErr := page.Validate(cursor, 1); validateErr != nil {
			t.Fatalf("owned-key page invalid: %v", validateErr)
		}
		if len(page.Items) > 0 {
			listed += len(page.Items)
			if deleteErr := fixture.coordinator.DeleteOwnedResourceKeys(
				ctx, fixture.resource, recoveryFence, page.Items,
			); deleteErr != nil {
				t.Fatalf("DeleteOwnedResourceKeys() error = %v", deleteErr)
			}
		}
		if page.Done {
			break
		}
		cursor = page.NextCursor
		if pageNumber == 99 {
			t.Fatal("owned-key pagination did not terminate")
		}
	}
	if listed == 0 {
		t.Fatal("ListOwnedResourceKeys() found no rebuildable scheduling keys")
	}
	if err := fixture.coordinator.CheckRecoveryLock(ctx, fixture.resource, lock); err != nil {
		t.Fatalf("owned-key deletion removed recovery lock: %v", err)
	}
	if value, err := fixture.raw.Get(ctx, otherKey).Result(); err != nil || value != "other-resource-sentinel" {
		t.Fatalf("owned-key deletion changed other resource key: value=%q error=%v", value, err)
	}
	if _, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock); err != nil {
		t.Fatalf("owned-key deletion removed/corrupted control: %v", err)
	}

	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, recoveryFence); err != nil {
		t.Fatalf("ResetResource() error = %v", err)
	}
	if err := fixture.coordinator.RestoreKnownTenant(ctx, fixture.resource, recoveryFence, "tenant-a"); err != nil {
		t.Fatalf("RestoreKnownTenant() error = %v", err)
	}
	if err := fixture.coordinator.RestoreActiveTenant(ctx, fixture.resource, recoveryFence, "tenant-a"); err != nil {
		t.Fatalf("RestoreActiveTenant() error = %v", err)
	}
	if err := fixture.coordinator.RestoreInflight(
		ctx, fixture.resource, recoveryFence, "tenant-a", stable, 3*time.Second,
	); err != nil {
		t.Fatalf("RestoreInflight() error = %v", err)
	}
	page, err := fixture.coordinator.ListRecoveryStableInflight(
		ctx, fixture.resource, recoveryFence, "", 10,
	)
	if err != nil || len(page.Items) != 1 || page.Items[0].StableToken != stable {
		t.Fatalf("ListRecoveryStableInflight() = (%#v,%v)", page, err)
	}
	if err := fixture.coordinator.DeleteRecoveryStableInflight(
		ctx, fixture.resource, recoveryFence, page.Items[0],
	); err != nil {
		t.Fatalf("DeleteRecoveryStableInflight() error = %v", err)
	}
	empty, err := fixture.coordinator.ListRecoveryStableInflight(
		ctx, fixture.resource, recoveryFence, "", 10,
	)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("stable inflight after delete = (%#v,%v)", empty, err)
	}
	redisTestFinishPreparedRecovery(t, ctx, fixture, recoveryFence)
}

func TestRedisRabbitRepairFencingAndAtomicFinish(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	_ = redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	operationID := redisTestRandomHex(t, 16)
	lock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	preflight, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
	if err != nil || !preflight.Present || preflight.State != ResourceReady || preflight.Kind != RecoveryNone {
		t.Fatalf("InspectRecoveryStart() = (%#v,%v), want READY/NONE", preflight, err)
	}
	fence, err := fixture.coordinator.BeginRabbitRepairWithLock(
		ctx, fixture.resource, redisTestWriterA, operationID, lock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRabbitRepairWithLock() error = %v", err)
	}
	beforeWrongFence, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
	if err != nil {
		t.Fatalf("InspectRecoveryStart() before wrong-fence calls error = %v", err)
	}
	wrongKind := fence
	wrongKind.Kind = RecoveryForceRebuild
	if err := fixture.coordinator.MarkForceDeletePassComplete(
		ctx, fixture.resource, wrongKind,
	); err == nil {
		t.Fatal("wrong-kind recovery fence mutated Rabbit repair progress")
	}
	wrongID := fence
	wrongID.OperationID = redisTestRandomHex(t, 16)
	if err := fixture.coordinator.SetRabbitRepairHighWater(
		ctx, fixture.resource, wrongID, "wrong-operation-high-water",
	); err == nil {
		t.Fatal("wrong-operation-ID recovery fence mutated Rabbit repair progress")
	}
	afterWrongFence, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
	if err != nil {
		t.Fatalf("InspectRecoveryStart() after wrong-fence calls error = %v", err)
	}
	if !reflect.DeepEqual(afterWrongFence, beforeWrongFence) {
		t.Fatalf("wrong kind/operation ID partially mutated recovery state:\nbefore=%#v\nafter=%#v", beforeWrongFence, afterWrongFence)
	}
	redisTestExpireRecoveryLock(t, ctx, fixture)
	takeoverLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock(takeover) error = %v", err)
	}
	if _, err := fixture.coordinator.BeginRabbitRepairWithLock(
		ctx,
		fixture.resource,
		redisTestWriterA,
		redisTestRandomHex(t, 16),
		takeoverLock,
		5*time.Second,
	); err == nil {
		t.Fatal("Rabbit repair takeover accepted a different operation ID")
	}
	takeoverFence, err := fixture.coordinator.BeginRabbitRepairWithLock(
		ctx, fixture.resource, redisTestWriterA, operationID, takeoverLock, 5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginRabbitRepairWithLock(takeover) error = %v", err)
	}
	if takeoverFence.Epoch == fence.Epoch {
		t.Fatal("Rabbit repair takeover reused the prior owner's epoch")
	}
	if err := fixture.coordinator.SetRabbitRepairHighWater(
		ctx, fixture.resource, fence, "stale-owner-high-water",
	); err == nil {
		t.Fatal("stale Rabbit repair owner mutated takeover progress")
	}
	lock = takeoverLock
	fence = takeoverFence
	if err := fixture.coordinator.SetRabbitRepairHighWater(
		ctx, fixture.resource, fence, "rabbit-high-water",
	); err != nil {
		t.Fatalf("SetRabbitRepairHighWater() error = %v", err)
	}
	if err := fixture.coordinator.MarkRabbitRepairPassComplete(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("MarkRabbitRepairPassComplete() error = %v", err)
	}
	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("ResetResource() error = %v", err)
	}
	redisTestFinishPreparedRecovery(t, ctx, fixture, fence)

	newLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() immediately after Finish error = %v", err)
	}
	if err := fixture.coordinator.ReleaseRecoveryLock(ctx, fixture.resource, lock); err == nil {
		t.Fatal("old owner compare-delete unexpectedly succeeded against the new lock")
	}
	if err := fixture.coordinator.CheckRecoveryLock(ctx, fixture.resource, newLock); err != nil {
		t.Fatalf("old owner deleted the new recovery lock: %v", err)
	}
	ready, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, newLock)
	if err != nil {
		t.Fatalf("InspectRecoveryStart() after finish error = %v", err)
	}
	if ready.State != ResourceReady || ready.Kind != RecoveryNone || ready.OperationID != "" ||
		ready.Progress != nil || ready.LastCompletedOperationID != operationID {
		t.Fatalf("finished Rabbit control = %#v", ready)
	}
}

func TestRedisWriterRebindStartMatrixAndFinish(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	_ = redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	lock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() error = %v", err)
	}
	if _, err := fixture.coordinator.BeginWriterRebindWithLock(
		ctx,
		fixture.resource,
		strings.Repeat("c", 64),
		redisTestWriterB,
		redisTestRandomHex(t, 16),
		lock,
		5*time.Second,
	); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("BeginWriterRebindWithLock(wrong old writer) error = %v, want ErrFenceMismatch", err)
	}
	stillReady, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, lock)
	if err != nil || stillReady.State != ResourceReady || stillReady.WriterFingerprint != redisTestWriterA {
		t.Fatalf("failed rebind mutated control: (%#v,%v)", stillReady, err)
	}
	operationID := redisTestRandomHex(t, 16)
	fence, err := fixture.coordinator.BeginWriterRebindWithLock(
		ctx,
		fixture.resource,
		redisTestWriterA,
		redisTestWriterB,
		operationID,
		lock,
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginWriterRebindWithLock() error = %v", err)
	}
	if fence.WriterFingerprint != redisTestWriterB || fence.Kind != RecoveryWriterRebind {
		t.Fatalf("writer rebind fence = %#v", fence)
	}
	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("ResetResource() error = %v", err)
	}
	redisTestFinishPreparedRecovery(t, ctx, fixture, fence)
	if _, err := fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, redisTestWriterA); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("old writer ObserveReadyFence() error = %v, want ErrFenceMismatch", err)
	}
	readyFence, err := fixture.coordinator.ObserveReadyFence(ctx, fixture.resource, redisTestWriterB)
	if err != nil {
		t.Fatalf("new writer ObserveReadyFence() error = %v", err)
	}
	if readyFence.Epoch != fence.Epoch {
		t.Fatalf("READY epoch = %q, want rebind epoch %q", readyFence.Epoch, fence.Epoch)
	}
}

func TestRedisForceRebuildRequiresNormalRecoveryAndDeadline(t *testing.T) {
	fixture := newRedisIntegrationFixture(t)
	_ = redisTestInitializeReady(t, fixture, redisTestWriterA)
	ctx, cancel := redisTestContext(t)
	defer cancel()
	readyLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock(READY force attempt) error = %v", err)
	}
	readyDeadline, err := fixture.coordinator.ComputeForceRebuildDeadlineWithLock(
		ctx, fixture.resource, readyLock, time.Second,
	)
	if err != nil {
		t.Fatalf("ComputeForceRebuildDeadlineWithLock(READY) error = %v", err)
	}
	if _, err := fixture.coordinator.BeginForceRebuildWithLock(
		ctx,
		fixture.resource,
		redisTestWriterA,
		redisTestRandomHex(t, 16),
		readyDeadline.NotBefore.UnixMilli(),
		readyLock,
		5*time.Second,
	); !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("BeginForceRebuildWithLock(READY) error = %v, want ErrResourceNotReady", err)
	}
	stillReady, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, readyLock)
	if err != nil || stillReady.State != ResourceReady || stillReady.Kind != RecoveryNone {
		t.Fatalf("rejected READY force start mutated control: (%#v,%v)", stillReady, err)
	}
	if err := fixture.coordinator.ReleaseRecoveryLock(ctx, fixture.resource, readyLock); err != nil {
		t.Fatalf("ReleaseRecoveryLock(READY force attempt) error = %v", err)
	}
	oldLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock(old) error = %v", err)
	}
	if _, err := fixture.coordinator.BeginRecoveryWithLock(
		ctx, fixture.resource, redisTestWriterA, oldLock, 5*time.Second,
	); err != nil {
		t.Fatalf("BeginRecoveryWithLock() error = %v", err)
	}
	redisTestExpireRecoveryLock(t, ctx, fixture)
	forceLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock(force) error = %v", err)
	}
	deadline, err := fixture.coordinator.ComputeForceRebuildDeadlineWithLock(
		ctx, fixture.resource, forceLock, time.Second,
	)
	if err != nil {
		t.Fatalf("ComputeForceRebuildDeadlineWithLock() error = %v", err)
	}
	if err := deadline.Validate(time.Second); err != nil {
		t.Fatalf("force rebuild deadline invalid: %v", err)
	}
	operationID := redisTestRandomHex(t, 16)
	fence, err := fixture.coordinator.BeginForceRebuildWithLock(
		ctx,
		fixture.resource,
		redisTestWriterA,
		operationID,
		deadline.NotBefore.UnixMilli(),
		forceLock,
		5*time.Second,
	)
	if err != nil {
		t.Fatalf("BeginForceRebuildWithLock() error = %v", err)
	}
	if err := fixture.coordinator.ResetResource(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("ResetResource() error = %v", err)
	}
	if err := fixture.coordinator.MarkForceDeletePassComplete(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("MarkForceDeletePassComplete() error = %v", err)
	}
	if err := fixture.coordinator.SetRecoveryHighWater(ctx, fixture.resource, fence, "test-high-water"); err != nil {
		t.Fatalf("SetRecoveryHighWater() error = %v", err)
	}
	for _, pass := range []RecoveryPass{RecoveryPassKnownTenants, RecoveryPassDispatched, RecoveryPassRunning} {
		if err := fixture.coordinator.MarkRecoveryPass(
			ctx, fixture.resource, fence, pass, 1, true, 0,
		); err != nil {
			t.Fatalf("MarkRecoveryPass(%s) error = %v", pass, err)
		}
	}
	if err := fixture.coordinator.FinishRecovery(ctx, fixture.resource, fence); err == nil {
		t.Fatal("FinishRecovery() succeeded before force not-before deadline")
	}
	redisTestEventually(t, 3*time.Second, func() bool {
		redisNow, redisTimeErr := fixture.raw.Time(ctx).Result()
		return redisTimeErr == nil && !redisNow.Before(deadline.NotBefore)
	})
	if err := fixture.coordinator.FinishRecovery(ctx, fixture.resource, fence); err != nil {
		t.Fatalf("FinishRecovery() after force deadline error = %v", err)
	}
	newLock, err := fixture.coordinator.AcquireRecoveryLock(
		ctx, fixture.resource, redisTestRandomHex(t, 16), 5*time.Second,
	)
	if err != nil {
		t.Fatalf("AcquireRecoveryLock() after force finish error = %v", err)
	}
	ready, err := fixture.coordinator.InspectRecoveryStart(ctx, fixture.resource, newLock)
	if err != nil {
		t.Fatalf("InspectRecoveryStart() error = %v", err)
	}
	if ready.State != ResourceReady || ready.Kind != RecoveryNone ||
		ready.LastCompletedOperationID != operationID {
		t.Fatalf("force rebuild READY control = %#v", ready)
	}
}

func TestRedisCloseIsIdempotent(t *testing.T) {
	addr := os.Getenv("BKCRAB_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("BKCRAB_TEST_REDIS_ADDR is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewRedis(ctx, RedisOptions{
		Addr:             addr,
		Password:         os.Getenv("BKCRAB_TEST_REDIS_PASSWORD"),
		KeyPrefix:        "bkcrab:test:fairqueue:close:" + redisTestRandomHex(t, 8) + ":",
		OperationTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewRedis() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}

func TestRedisErrorCategoriesRemainDistinct(t *testing.T) {
	categories := []error{
		ErrDependencyUnavailable,
		ErrUnsupportedTopology,
		ErrResourceNotReady,
		ErrFenceMismatch,
		ErrRecoveryOwnerStale,
		ErrCoordinationCorrupt,
	}
	for index, category := range categories {
		wrapped := fmt.Errorf("redis adapter detail: %w", category)
		if !errors.Is(wrapped, category) {
			t.Fatalf("wrapped error does not retain %v", category)
		}
		for otherIndex, other := range categories {
			if index != otherIndex && errors.Is(wrapped, other) {
				t.Fatalf("error category %v aliases %v", category, other)
			}
		}
	}
}
