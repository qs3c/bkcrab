package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type labUser struct {
	Index      int
	ID         string
	Username   string
	TenantHash string
	KBID       string
	client     *apiClient
}

func (u *labUser) public() labUserReport {
	return labUserReport{Index: u.Index, ID: u.ID, Username: u.Username, TenantHash: u.TenantHash, KBID: u.KBID}
}

type fairQueueHealth struct {
	Enabled  bool   `json:"enabled"`
	Status   string `json:"status"`
	Mode     string `json:"mode"`
	GateOpen bool   `json:"gateOpen"`
	Rabbit   struct {
		Status     string `json:"status"`
		ReadyDepth int64  `json:"readyDepthSample"`
		DLQDepth   int64  `json:"dlqDepthSample"`
	} `json:"rabbit"`
	Redis struct {
		Status           string `json:"status"`
		ResourceState    string `json:"resourceState"`
		ActiveCount      int64  `json:"activeCount"`
		RingCount        int64  `json:"ringCount"`
		RingMemberCount  int64  `json:"ringMemberCount"`
		GlobalInflight   int64  `json:"globalInflight"`
		ProvisionalCount int64  `json:"provisionalCount"`
		StableCount      int64  `json:"stableCount"`
		ProcessingCount  int64  `json:"processingCount"`
	} `json:"redis"`
}

type redisObservation struct {
	Active         int64            `json:"active"`
	Ring           int64            `json:"ring"`
	RingMembers    int64            `json:"ringMembers"`
	GlobalInflight int64            `json:"globalInflight"`
	Provisional    int64            `json:"provisional"`
	Stable         int64            `json:"stable"`
	Processing     int64            `json:"processing"`
	TenantInflight map[string]int64 `json:"tenantInflight"`
}

type rabbitQueueObservation struct {
	Name         string `json:"name"`
	TenantHash   string `json:"tenantHash,omitempty"`
	Messages     int64  `json:"messages"`
	Ready        int64  `json:"ready"`
	Unacked      int64  `json:"unacked"`
	Consumers    int64  `json:"consumers"`
	PublishTotal int64  `json:"publishTotal"`
	DeliverTotal int64  `json:"deliverTotal"`
}

type observationSample struct {
	At     time.Time                `json:"at"`
	Health *fairQueueHealth         `json:"health,omitempty"`
	Redis  *redisObservation        `json:"redis,omitempty"`
	Rabbit []rabbitQueueObservation `json:"rabbit,omitempty"`
	Errors []string                 `json:"errors,omitempty"`
}

type labObserver struct {
	admin       *apiClient
	users       []*labUser
	redis       *redis.Client
	redisPrefix string
	rabbitURL   *url.URL
	rabbitUser  string
	rabbitPass  string
	rabbitVHost string
	rabbitHTTP  *http.Client
	closeOnce   sync.Once
}

func newLabObserver(cfg labConfig, admin *apiClient, users []*labUser) (*labObserver, error) {
	observer := &labObserver{admin: admin, users: users, redisPrefix: cfg.RedisPrefix, rabbitVHost: cfg.RabbitVHost, rabbitHTTP: &http.Client{Timeout: 10 * time.Second}}
	if cfg.RedisAddr != "" {
		password := os.Getenv("BKCRAB_LAB_REDIS_PASSWORD")
		if password == "" {
			return nil, fmt.Errorf("set BKCRAB_LAB_REDIS_PASSWORD when -redis-addr is used")
		}
		observer.redis = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: password, DB: cfg.RedisDB})
	}
	if cfg.RabbitURL != "" {
		parsed, err := url.Parse(strings.TrimRight(cfg.RabbitURL, "/"))
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, fmt.Errorf("invalid -rabbit-management-url %q", cfg.RabbitURL)
		}
		observer.rabbitUser = os.Getenv("BKCRAB_LAB_RABBIT_USER")
		observer.rabbitPass = os.Getenv("BKCRAB_LAB_RABBIT_PASSWORD")
		if observer.rabbitUser == "" || observer.rabbitPass == "" {
			return nil, fmt.Errorf("set BKCRAB_LAB_RABBIT_USER and BKCRAB_LAB_RABBIT_PASSWORD when RabbitMQ observation is enabled")
		}
		observer.rabbitURL = parsed
	}
	return observer, nil
}

func (o *labObserver) close() {
	o.closeOnce.Do(func() {
		if o.redis != nil {
			_ = o.redis.Close()
		}
	})
}

var redisCleanupLabTenantScript = redis.NewScript(`
local known_type = redis.call('TYPE', KEYS[1]).ok
local active_type = redis.call('TYPE', KEYS[2]).ok
local ring_type = redis.call('TYPE', KEYS[3]).ok
local tenant_turn_type = redis.call('TYPE', KEYS[4]).ok
local inflight_type = redis.call('TYPE', KEYS[5]).ok
if (known_type ~= 'none' and known_type ~= 'set') or
   (active_type ~= 'none' and active_type ~= 'set') or
   (ring_type ~= 'none' and ring_type ~= 'set') or
   (tenant_turn_type ~= 'none' and tenant_turn_type ~= 'hash') or
   (inflight_type ~= 'none' and inflight_type ~= 'zset') then
  return {'INVALID_TYPE'}
end
if redis.call('SISMEMBER', KEYS[2], ARGV[1]) ~= 0 or
   redis.call('SISMEMBER', KEYS[3], ARGV[1]) ~= 0 or
   redis.call('HEXISTS', KEYS[4], ARGV[1]) ~= 0 or
   redis.call('ZCARD', KEYS[5]) ~= 0 then
  return {'BUSY'}
end
redis.call('SREM', KEYS[1], ARGV[1])
redis.call('DEL', KEYS[5])
return {'OK'}
`)

// cleanupTenants removes only the exact test tenant artifacts after the
// canonical user deletion has succeeded. Redis cleanup is atomic and refuses
// an active/ring/inflight tenant. RabbitMQ cleanup uses if-empty+if-unused, so
// neither observer can destroy live work if a lifecycle invariant regresses.
func (o *labObserver) cleanupTenants(ctx context.Context, report *labReport) {
	for _, user := range o.users {
		if !report.userCleaned(user.ID) {
			continue
		}
		if o.redis != nil {
			root := o.redisPrefix + "fair:{rag.index}:"
			result, err := redisCleanupLabTenantScript.Run(ctx, o.redis, []string{
				root + "known_users", root + "active_users", root + "ring_members",
				root + "tenant_to_turn", root + "inflight:tenant:" + user.TenantHash,
			}, user.ID).StringSlice()
			if err != nil || len(result) != 1 || result[0] != "OK" {
				report.addError(fmt.Sprintf("cleanup Redis tenant %s: result=%v error=%v", user.Username, result, err))
			} else {
				report.markRedisCleaned(user.ID)
			}
		}
		if o.rabbitURL != nil {
			queue := "bkcrab.fair.q.rag.index." + user.TenantHash
			if err := o.deleteRabbitQueue(ctx, queue); err != nil {
				report.addError(fmt.Sprintf("cleanup RabbitMQ tenant %s: %v", user.Username, err))
			} else {
				report.markRabbitCleaned(user.ID)
			}
		}
	}
}

func (o *labObserver) deleteRabbitQueue(ctx context.Context, queue string) error {
	endpoint := *o.rabbitURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/queues/" + url.PathEscape(o.rabbitVHost) + "/" + url.PathEscape(queue)
	query := endpoint.Query()
	query.Set("if-empty", "true")
	query.Set("if-unused", "true")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint.String(), nil)
	if err != nil {
		return err
	}
	request.SetBasicAuth(o.rabbitUser, o.rabbitPass)
	response, err := o.rabbitHTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("management API returned %d", response.StatusCode)
}

func (o *labObserver) run(ctx context.Context, interval time.Duration, report *labReport) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report.addSample(o.sample(ctx))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (o *labObserver) sample(ctx context.Context) observationSample {
	sample := observationSample{At: time.Now().UTC()}
	health, err := o.admin.fairQueueHealth(ctx)
	if err != nil {
		sample.Errors = append(sample.Errors, "health: "+err.Error())
	} else {
		sample.Health = &health
	}
	if o.redis != nil {
		observation, redisErr := o.sampleRedis(ctx)
		if redisErr != nil {
			sample.Errors = append(sample.Errors, "redis: "+redisErr.Error())
		} else {
			sample.Redis = &observation
		}
	}
	if o.rabbitURL != nil {
		queues, rabbitErr := o.sampleRabbit(ctx)
		if rabbitErr != nil {
			sample.Errors = append(sample.Errors, "rabbit: "+rabbitErr.Error())
		} else {
			sample.Rabbit = queues
		}
	}
	return sample
}

func (o *labObserver) sampleRedis(ctx context.Context) (redisObservation, error) {
	root := o.redisPrefix + "fair:{rag.index}:"
	pipeline := o.redis.Pipeline()
	active := pipeline.SCard(ctx, root+"active_users")
	ring := pipeline.LLen(ctx, root+"ring")
	members := pipeline.SCard(ctx, root+"ring_members")
	inflight := pipeline.ZCard(ctx, root+"inflight")
	provisional := pipeline.ZCard(ctx, root+"provisional")
	stable := pipeline.ZCard(ctx, root+"inflight:stable_index")
	processing := pipeline.ZCard(ctx, root+"processing_turns")
	tenantCommands := make(map[string]*redis.IntCmd, len(o.users))
	for _, user := range o.users {
		tenantCommands[user.TenantHash] = pipeline.ZCard(ctx, root+"inflight:tenant:"+user.TenantHash)
	}
	if _, err := pipeline.Exec(ctx); err != nil && err != redis.Nil {
		return redisObservation{}, err
	}
	result := redisObservation{
		Active: active.Val(), Ring: ring.Val(), RingMembers: members.Val(),
		GlobalInflight: inflight.Val(), Provisional: provisional.Val(), Stable: stable.Val(), Processing: processing.Val(),
		TenantInflight: make(map[string]int64, len(tenantCommands)),
	}
	for hash, command := range tenantCommands {
		result.TenantInflight[hash] = command.Val()
	}
	return result, nil
}

func (o *labObserver) sampleRabbit(ctx context.Context) ([]rabbitQueueObservation, error) {
	endpoint := *o.rabbitURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/queues/" + url.PathEscape(o.rabbitVHost)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	request.SetBasicAuth(o.rabbitUser, o.rabbitPass)
	response, err := o.rabbitHTTP.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("management API returned %d", response.StatusCode)
	}
	var rows []struct {
		Name      string `json:"name"`
		Messages  int64  `json:"messages"`
		Ready     int64  `json:"messages_ready"`
		Unacked   int64  `json:"messages_unacknowledged"`
		Consumers int64  `json:"consumers"`
		Stats     struct {
			Publish int64 `json:"publish"`
			Deliver int64 `json:"deliver_get"`
		} `json:"message_stats"`
	}
	if err := json.NewDecoder(response.Body).Decode(&rows); err != nil {
		return nil, err
	}
	hashByQueue := make(map[string]string, len(o.users))
	for _, user := range o.users {
		hashByQueue["bkcrab.fair.q.rag.index."+user.TenantHash] = user.TenantHash
	}
	result := make([]rabbitQueueObservation, 0, len(rows))
	for _, row := range rows {
		if !strings.HasPrefix(row.Name, "bkcrab.fair.q.rag.index.") && row.Name != "bkcrab.fair.dlq.rag.index" {
			continue
		}
		result = append(result, rabbitQueueObservation{
			Name: row.Name, TenantHash: hashByQueue[row.Name], Messages: row.Messages,
			Ready: row.Ready, Unacked: row.Unacked, Consumers: row.Consumers,
			PublishTotal: row.Stats.Publish, DeliverTotal: row.Stats.Deliver,
		})
	}
	return result, nil
}

func tenantHash(resource, tenant string) string {
	digest := sha256.Sum256([]byte(resource + "\x00" + tenant))
	return hex.EncodeToString(digest[:])
}
