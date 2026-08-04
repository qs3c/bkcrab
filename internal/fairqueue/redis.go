package fairqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	redis "github.com/redis/go-redis/v9"
)

const (
	redisDefaultOperationTimeout = 15 * time.Second
	redisMaximumPrefixBytes      = 512
	redisScanCountMultiplier     = 4

	redisResultOK                  = "OK"
	redisResultEmpty               = "EMPTY"
	redisResultNotReady            = "FQ_NOT_READY"
	redisResultFenceMismatch       = "FQ_FENCE_MISMATCH"
	redisResultRecoveryOwnerStale  = "FQ_RECOVERY_OWNER_STALE"
	redisResultCoordinationCorrupt = "FQ_COORDINATION_CORRUPT"
	redisResultInvalidState        = "FQ_INVALID_STATE"
)

const redisRawLockFenceLua = `
local function fq_hex(value, length)
  return type(value) == 'string' and string.len(value) == length and
    string.match(value, '^[0-9a-f]+$') ~= nil
end
local function fq_key_type(key)
  local result = redis.call('TYPE', key)
  if type(result) == 'table' then return result.ok end
  return result
end
local function fq_type_is(key, expected)
  local actual = fq_key_type(key)
  return actual == 'none' or actual == expected
end
local function fq_uint(value, positive)
  if type(value) ~= 'string' or not string.match(value, '^[0-9]+$') then return false end
  if string.len(value) > 1 and string.sub(value, 1, 1) == '0' then return false end
  if positive and value == '0' then return false end
  if string.len(value) > 19 or
      (string.len(value) == 19 and value > '9223372036854775807') then return false end
  return true
end
local function fq_bool(value)
  return value == '0' or value == '1'
end
local function fq_high_water(value)
  if type(value) ~= 'string' or string.len(value) > 191 then return false end
  if value ~= '' and (string.byte(value, 1) == 32 or string.byte(value, -1) == 32) then return false end
  for index = 1, string.len(value) do
    local byte = string.byte(value, index)
    if byte < 32 or byte > 126 then return false end
  end
  return true
end
local function fq_raw_lock(key, expected_owner)
  local key_type = fq_key_type(key)
  if key_type == 'none' then return 'FQ_RECOVERY_OWNER_STALE' end
  if key_type ~= 'string' then return 'FQ_COORDINATION_CORRUPT' end
  local owner = redis.call('GET', key)
  if not fq_hex(owner, 32) then return 'FQ_COORDINATION_CORRUPT' end
  local ttl = redis.call('PTTL', key)
  if not ttl or ttl <= 0 then return 'FQ_COORDINATION_CORRUPT' end
  if owner ~= expected_owner then return 'FQ_RECOVERY_OWNER_STALE' end
  return nil
end
`

const redisRecoveryFenceLua = redisRawLockFenceLua + `
local function fq_recovery_fence()
  local lock_failure = fq_raw_lock(KEYS[2], ARGV[3])
  if lock_failure then return lock_failure end
  if redis.call('EXISTS', KEYS[1]) ~= 1 then
    return 'FQ_NOT_READY'
  end
	  local control = redis.call('HMGET', KEYS[1],
	    'epoch', 'state', 'operation_kind', 'operation_id',
	    'protocol_version', 'writer_fingerprint', 'last_completed_operation_id',
	    'last_completed_operation_kind')
	  if not control[1] or not control[2] or not control[3] or control[4] == false or
	      not control[5] or not control[6] or control[7] == false or control[8] == false then
	    return 'FQ_COORDINATION_CORRUPT'
	  end
  if not tonumber(control[5]) or tonumber(control[5]) < 1 or
      math.floor(tonumber(control[5])) ~= tonumber(control[5]) then
    return 'FQ_COORDINATION_CORRUPT'
  end
  if control[5] ~= '1' then
    return 'FQ_FENCE_MISMATCH'
  end
	  if not fq_hex(control[1], 32) or not fq_hex(control[6], 64) or
	      (control[7] == '' and control[8] ~= 'NONE') or
	      (control[7] ~= '' and (not fq_hex(control[7], 32) or
	        (control[8] ~= 'RABBIT_REPAIR' and control[8] ~= 'WRITER_REBIND' and
	          control[8] ~= 'FORCE_REBUILD'))) then
    return 'FQ_COORDINATION_CORRUPT'
  end
  if control[2] ~= 'RECOVERING' then
    return 'FQ_NOT_READY'
  end
  if control[1] ~= ARGV[1] or control[6] ~= ARGV[2] or
      control[3] ~= ARGV[4] or control[4] ~= ARGV[5] then
    return 'FQ_FENCE_MISMATCH'
  end
  if control[3] ~= 'NORMAL' and control[7] == control[4] then
    return 'FQ_COORDINATION_CORRUPT'
  end
  if redis.call('EXISTS', KEYS[3]) ~= 1 then
    return 'FQ_COORDINATION_CORRUPT'
  end
  local progress = redis.call('HMGET', KEYS[3],
    'epoch', 'operation_kind', 'operation_id', 'high_water',
    'known_cycle', 'known_complete', 'known_diff',
    'dispatched_cycle', 'dispatched_complete', 'dispatched_diff',
    'running_cycle', 'running_complete', 'running_diff',
    'repair_high_water', 'repair_complete',
    'rebind_original_writer', 'rebind_target_writer',
    'force_not_before_ms', 'force_delete_complete')
  for index = 1, 19 do
    if progress[index] == false then return 'FQ_COORDINATION_CORRUPT' end
  end
  if progress[1] ~= ARGV[1] or progress[2] ~= ARGV[4] or progress[3] ~= ARGV[5] then
    return 'FQ_COORDINATION_CORRUPT'
  end
  if not fq_high_water(progress[4]) then return 'FQ_COORDINATION_CORRUPT' end
  for _, index in ipairs({5, 7, 8, 10, 11, 13}) do
    if not fq_uint(progress[index], false) then return 'FQ_COORDINATION_CORRUPT' end
  end
  for _, index in ipairs({6, 9, 12, 15, 19}) do
    if not fq_bool(progress[index]) then return 'FQ_COORDINATION_CORRUPT' end
  end
  if (progress[6] == '1' and progress[5] == '0') or
      (progress[9] == '1' and progress[8] == '0') or
      (progress[12] == '1' and progress[11] == '0') or
      (progress[4] == '' and
        (progress[6] == '1' or progress[9] == '1' or progress[12] == '1')) then
    return 'FQ_COORDINATION_CORRUPT'
  end
  if ARGV[4] == 'NORMAL' then
    if ARGV[5] ~= '' or progress[14] ~= '' or progress[15] ~= '0' or
        progress[16] ~= '' or progress[17] ~= '' or progress[18] ~= '' or progress[19] ~= '0' then
      return 'FQ_COORDINATION_CORRUPT'
    end
  elseif ARGV[4] == 'RABBIT_REPAIR' then
    if not fq_hex(ARGV[5], 32) or not fq_high_water(progress[14]) or
        (progress[15] == '1' and progress[14] == '') or progress[16] ~= '' or
        progress[17] ~= '' or progress[18] ~= '' or progress[19] ~= '0' then
      return 'FQ_COORDINATION_CORRUPT'
    end
  elseif ARGV[4] == 'WRITER_REBIND' then
    if not fq_hex(ARGV[5], 32) or progress[14] ~= '' or progress[15] ~= '0' or
        not fq_hex(progress[16], 64) or not fq_hex(progress[17], 64) or
        progress[16] == progress[17] or progress[17] ~= ARGV[2] or
        progress[18] ~= '' or progress[19] ~= '0' then
      return 'FQ_COORDINATION_CORRUPT'
    end
  elseif ARGV[4] == 'FORCE_REBUILD' then
    if not fq_hex(ARGV[5], 32) or progress[14] ~= '' or progress[15] ~= '0' or
        progress[16] ~= '' or progress[17] ~= '' or not fq_uint(progress[18], true) then
      return 'FQ_COORDINATION_CORRUPT'
    end
  else
    return 'FQ_FENCE_MISMATCH'
  end
  return nil
end
`

// Redis Lua exposes SHA-1 but TenantHash is a SHA-256 protocol identity. Keep
// this bounded, byte-oriented implementation next to the one cleanup script
// that must validate metadata before touching a dynamically named tenant key.
const redisSHA256Lua = `
local fq_sha256_constants = {
  0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
  0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
  0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
  0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
  0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
  0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
  0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
  0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2
}
local function fq_sha256_add32(...)
  local value = 0
  for index = 1, select('#', ...) do value = value + select(index, ...) end
  return bit.tobit(value)
end
local function fq_sha256_unsigned32(value)
  if value < 0 then return value + 4294967296 end
  return value
end
local function fq_sha256(message)
  local bytes = {string.byte(message, 1, string.len(message))}
  local bit_length = #bytes * 8
  table.insert(bytes, 0x80)
  while (#bytes % 64) ~= 56 do table.insert(bytes, 0) end
  for shift = 7, 0, -1 do
    table.insert(bytes, math.floor(bit_length / (2 ^ (shift * 8))) % 256)
  end
  local hashes = {
    0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,
    0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19
  }
  for offset = 1, #bytes, 64 do
    local words = {}
    for index = 0, 15 do
      local position = offset + index * 4
      words[index + 1] = bit.tobit(bytes[position] * 16777216 + bytes[position + 1] * 65536 +
        bytes[position + 2] * 256 + bytes[position + 3])
    end
    for index = 17, 64 do
      local left = words[index - 15]
      local right = words[index - 2]
      local sigma0 = bit.bxor(bit.ror(left, 7), bit.ror(left, 18), bit.rshift(left, 3))
      local sigma1 = bit.bxor(bit.ror(right, 17), bit.ror(right, 19), bit.rshift(right, 10))
      words[index] = fq_sha256_add32(words[index - 16], sigma0, words[index - 7], sigma1)
    end
    local a,b,c,d,e,f,g,h = hashes[1],hashes[2],hashes[3],hashes[4],hashes[5],hashes[6],hashes[7],hashes[8]
    for index = 1, 64 do
      local choice = bit.bxor(bit.band(e, f), bit.band(bit.bnot(e), g))
      local majority = bit.bxor(bit.band(a, b), bit.band(a, c), bit.band(b, c))
      local sum0 = bit.bxor(bit.ror(a, 2), bit.ror(a, 13), bit.ror(a, 22))
      local sum1 = bit.bxor(bit.ror(e, 6), bit.ror(e, 11), bit.ror(e, 25))
      local first = fq_sha256_add32(h, sum1, choice, fq_sha256_constants[index], words[index])
      local second = fq_sha256_add32(sum0, majority)
      h,g,f,e,d,c,b,a = g,f,e,fq_sha256_add32(d, first),c,b,a,fq_sha256_add32(first, second)
    end
    hashes[1],hashes[2],hashes[3],hashes[4] =
      fq_sha256_add32(hashes[1],a),fq_sha256_add32(hashes[2],b),
      fq_sha256_add32(hashes[3],c),fq_sha256_add32(hashes[4],d)
    hashes[5],hashes[6],hashes[7],hashes[8] =
      fq_sha256_add32(hashes[5],e),fq_sha256_add32(hashes[6],f),
      fq_sha256_add32(hashes[7],g),fq_sha256_add32(hashes[8],h)
  end
  local encoded = ''
  for index = 1, 8 do
    encoded = encoded .. string.format('%08x', fq_sha256_unsigned32(hashes[index]))
  end
  return encoded
end
`

type RedisOptions struct {
	Addr             string
	Password         string
	DB               int
	KeyPrefix        string
	OperationTimeout time.Duration
	Telemetry        TelemetrySink
}

// RedisResourceHealthProbe is a read-only, resource-scoped observation. The
// raw control identity is retained so the gateway can derive short diagnostic
// fingerprints; callers must never expose its epoch or operation IDs without
// hashing them first. Connection settings, tenants, tasks, and owner tokens
// are deliberately absent.
type RedisResourceHealthProbe struct {
	Resource         string                  `json:"resource"`
	Topology         RedisTopology           `json:"topology"`
	Control          RecoveryControlSnapshot `json:"control"`
	ActiveCount      int64                   `json:"active_count"`
	RingCount        int64                   `json:"ring_count"`
	RingMemberCount  int64                   `json:"ring_member_count"`
	GlobalInflight   int64                   `json:"global_inflight"`
	ProvisionalCount int64                   `json:"provisional_count"`
	StableCount      int64                   `json:"stable_count"`
	ProcessingCount  int64                   `json:"processing_count"`
}

var redisResourceHealthCountsScript = redis.NewScript(redisRawLockFenceLua + `
local provisional_type = fq_key_type(KEYS[1])
local processing_type = fq_key_type(KEYS[2])
local active_type = fq_key_type(KEYS[3])
local ring_type = fq_key_type(KEYS[4])
local members_type = fq_key_type(KEYS[5])
local stable_type = fq_key_type(KEYS[6])
local inflight_type = fq_key_type(KEYS[7])
if (provisional_type ~= 'none' and provisional_type ~= 'zset') or
   (processing_type ~= 'none' and processing_type ~= 'zset') or
   (active_type ~= 'none' and active_type ~= 'set') or
   (ring_type ~= 'none' and ring_type ~= 'list') or
   (members_type ~= 'none' and members_type ~= 'set') or
   (stable_type ~= 'none' and stable_type ~= 'zset') or
   (inflight_type ~= 'none' and inflight_type ~= 'zset') then
  return {'FQ_COORDINATION_CORRUPT'}
end
return {'OK', tostring(redis.call('ZCARD', KEYS[1])), tostring(redis.call('ZCARD', KEYS[2])),
  tostring(redis.call('SCARD', KEYS[3])), tostring(redis.call('LLEN', KEYS[4])),
  tostring(redis.call('SCARD', KEYS[5])), tostring(redis.call('ZCARD', KEYS[6])),
  tostring(redis.call('ZCARD', KEYS[7]))}
`)

// Redis implements the rebuildable fair-scheduling coordination boundary.
// The authoritative task and recovery journals remain outside Redis.
type Redis struct {
	options   RedisOptions
	client    redisClient
	tokens    redisTokenSource
	telemetry TelemetrySink

	closeMu sync.Mutex
	closed  bool
}

var (
	_ Coordinator    = (*Redis)(nil)
	_ RedisInspector = (*Redis)(nil)
)

type redisClient interface {
	redis.Scripter
	Info(context.Context, ...string) *redis.StringCmd
	Do(context.Context, ...any) *redis.Cmd
	Scan(context.Context, uint64, string, int64) *redis.ScanCmd
	Close() error
}

type redisTokenSource interface {
	Next() (string, error)
}

type cryptoRedisTokens struct{}

func (cryptoRedisTokens) Next() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type redisKeyspace struct {
	root                 string
	control              string
	recoveryLock         string
	progress             string
	ring                 string
	ringMembers          string
	activeUsers          string
	knownUsers           string
	activationGeneration string
	processingTurns      string
	turnToTenant         string
	turnToGeneration     string
	tenantToTurn         string
	globalInflight       string
	provisional          string
	stableIndex          string
	reservationMetadata  string
}

func buildRedisKeyspace(prefix, resource string) (redisKeyspace, error) {
	if err := validateRedisKeyPrefix(prefix); err != nil {
		return redisKeyspace{}, err
	}
	if err := ValidateResource(resource); err != nil {
		return redisKeyspace{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	root := prefix + "fair:{" + resource + "}:"
	keys := redisKeyspace{
		root:                 root,
		control:              root + "resource",
		recoveryLock:         root + "recovery_lock",
		progress:             root + "recovery_progress",
		ring:                 root + "ring",
		ringMembers:          root + "ring_members",
		activeUsers:          root + "active_users",
		knownUsers:           root + "known_users",
		activationGeneration: root + "activation_gen",
		processingTurns:      root + "processing_turns",
		turnToTenant:         root + "turn_to_tenant",
		turnToGeneration:     root + "turn_to_generation",
		tenantToTurn:         root + "tenant_to_turn",
		globalInflight:       root + "inflight",
		provisional:          root + "provisional",
		stableIndex:          root + "inflight:stable_index",
		reservationMetadata:  root + "reservation_meta",
	}
	for _, key := range keys.all() {
		if len(key) > maxRedisKeyBytes {
			return redisKeyspace{}, fmt.Errorf("%w: Redis key is too long", ErrInvalidModel)
		}
	}
	return keys, nil
}

func (k redisKeyspace) all() []string {
	return []string{
		k.control, k.recoveryLock, k.progress, k.ring, k.ringMembers,
		k.activeUsers, k.knownUsers, k.activationGeneration,
		k.processingTurns, k.turnToTenant, k.turnToGeneration, k.tenantToTurn,
		k.globalInflight, k.provisional, k.stableIndex, k.reservationMetadata,
	}
}

func (k redisKeyspace) tenantInflight(resource, tenant string) (string, error) {
	hash, err := TenantHash(resource, tenant)
	if err != nil {
		return "", fmt.Errorf("%w: invalid reservation tenant", ErrInvalidModel)
	}
	key := k.root + "inflight:tenant:" + hash
	if len(key) > maxRedisKeyBytes {
		return "", fmt.Errorf("%w: Redis key is too long", ErrInvalidModel)
	}
	return key, nil
}

func validateRedisKeyPrefix(prefix string) error {
	if prefix == "" || prefix != strings.TrimSpace(prefix) || len(prefix) > redisMaximumPrefixBytes || !utf8.ValidString(prefix) {
		return fmt.Errorf("%w: invalid Redis key prefix", ErrInvalidModel)
	}
	if strings.ContainsAny(prefix, "{}*?[]\\") {
		return fmt.Errorf("%w: unsafe Redis key prefix", ErrInvalidModel)
	}
	for _, character := range prefix {
		if character == 0 || unicode.IsControl(character) {
			return fmt.Errorf("%w: unsafe Redis key prefix", ErrInvalidModel)
		}
	}
	return nil
}

func NewRedis(ctx context.Context, options RedisOptions) (*Redis, error) {
	validated, err := validateRedisOptions(options)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(&redis.Options{
		Addr:                  validated.Addr,
		Password:              validated.Password,
		DB:                    validated.DB,
		DialTimeout:           validated.OperationTimeout,
		ReadTimeout:           validated.OperationTimeout,
		WriteTimeout:          validated.OperationTimeout,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	})
	coordinator, err := newRedis(ctx, validated, client, cryptoRedisTokens{})
	if err != nil {
		_ = client.Close()
		return nil, err
	}
	return coordinator, nil
}

func newRedis(ctx context.Context, options RedisOptions, client redisClient, tokens redisTokenSource) (*Redis, error) {
	validated, err := validateRedisOptions(options)
	if err != nil {
		return nil, err
	}
	if client == nil || tokens == nil {
		return nil, fmt.Errorf("%w: Redis dependency is required", ErrInvalidModel)
	}
	// Retain only non-secret runtime settings. Connection credentials and the
	// endpoint are owned by go-redis and must not be duplicated in coordinator
	// state or accidentally surfaced by diagnostics.
	coordinator := &Redis{
		options: RedisOptions{
			KeyPrefix:        validated.KeyPrefix,
			OperationTimeout: validated.OperationTimeout,
		},
		client:    client,
		tokens:    tokens,
		telemetry: validated.Telemetry,
	}
	topology, err := coordinator.inspectRedisTopology(ctx)
	if err != nil {
		return nil, err
	}
	if !topology.SupportsFairQueue() {
		return nil, fmt.Errorf("%w: Redis must be a standalone writable primary", ErrUnsupportedTopology)
	}
	return coordinator, nil
}

func validateRedisOptions(options RedisOptions) (RedisOptions, error) {
	if options.Addr == "" || options.Addr != strings.TrimSpace(options.Addr) {
		return RedisOptions{}, fmt.Errorf("%w: Redis address is required", ErrInvalidModel)
	}
	if options.DB < 0 {
		return RedisOptions{}, fmt.Errorf("%w: Redis database must be non-negative", ErrInvalidModel)
	}
	if err := validateRedisKeyPrefix(options.KeyPrefix); err != nil {
		return RedisOptions{}, err
	}
	if options.OperationTimeout == 0 {
		options.OperationTimeout = redisDefaultOperationTimeout
	}
	if options.OperationTimeout < 0 || options.OperationTimeout > maxResourceDuration {
		return RedisOptions{}, fmt.Errorf("%w: Redis operation timeout is invalid", ErrInvalidModel)
	}
	return options, nil
}

func (r *Redis) operationContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		return nil, nil, fmt.Errorf("%w: nil context", ErrInvalidModel)
	}
	if err := parent.Err(); err != nil {
		return nil, nil, redisDependencyError("operation", err)
	}
	ctx, cancel := context.WithTimeout(parent, r.options.OperationTimeout)
	return ctx, cancel, nil
}

func (r *Redis) InspectRedisTopology(ctx context.Context) (RedisTopology, error) {
	return r.inspectRedisTopology(ctx)
}

// ProbeResourceHealth verifies the live Redis deployment topology, reads the
// resource control/progress snapshot, and counts only the two bounded runtime
// lease sets needed by operational health. Every Redis command is read-only.
func (r *Redis) ProbeResourceHealth(ctx context.Context, resource string) (probe RedisResourceHealthProbe, err error) {
	if r == nil {
		return RedisResourceHealthProbe{}, fmt.Errorf("%w: nil Redis health probe", ErrInvalidModel)
	}
	if err := ValidateResource(resource); err != nil {
		return RedisResourceHealthProbe{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	defer func() {
		outcome := "ready"
		if err != nil {
			outcome = "unavailable"
		}
		EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryDependencyTransition, Resource: resource, Outcome: outcome, Dependency: "redis"})
	}()
	topology, err := r.InspectRedisTopology(ctx)
	if err != nil {
		return RedisResourceHealthProbe{}, err
	}
	if !topology.SupportsFairQueue() {
		return RedisResourceHealthProbe{}, fmt.Errorf("%w: Redis must be a standalone writable primary", ErrUnsupportedTopology)
	}
	control, err := r.InspectRecoveryControl(ctx, resource)
	if err != nil {
		return RedisResourceHealthProbe{}, err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RedisResourceHealthProbe{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RedisResourceHealthProbe{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "inspect resource health counts", redisResourceHealthCountsScript,
		[]string{keys.provisional, keys.processingTurns, keys.activeUsers, keys.ring,
			keys.ringMembers, keys.stableIndex, keys.globalInflight})
	if err != nil {
		return RedisResourceHealthProbe{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return RedisResourceHealthProbe{}, err
	}
	if len(values) != 8 {
		return RedisResourceHealthProbe{}, fmt.Errorf("%w: malformed Redis health count response", ErrCoordinationCorrupt)
	}
	code, ok := redisResultString(values[0])
	if !ok {
		return RedisResourceHealthProbe{}, fmt.Errorf("%w: malformed Redis health count response", ErrCoordinationCorrupt)
	}
	if code != redisResultOK {
		return RedisResourceHealthProbe{}, redisScriptError(code)
	}
	counts := make([]int64, 7)
	for index := range counts {
		value, valid := redisResultInt64(values[index+1])
		if !valid || value < 0 {
			return RedisResourceHealthProbe{}, fmt.Errorf("%w: invalid Redis health count response", ErrCoordinationCorrupt)
		}
		counts[index] = value
	}
	provisional, processing, active, ring, members, stable, global := counts[0], counts[1], counts[2], counts[3], counts[4], counts[5], counts[6]
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryReservation, Resource: resource, Outcome: "ok", ReservationKind: "provisional", Dependency: "redis", Value: provisional})
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryReservation, Resource: resource, Outcome: "ok", ReservationKind: "stable", Dependency: "redis", Value: stable})
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryProcessingTurn, Resource: resource, Outcome: "ok", ReservationKind: "processing", Dependency: "redis", Value: processing})
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryActiveTenants, Resource: resource, Outcome: "ok", Dependency: "redis", Value: active})
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryRing, Resource: resource, Outcome: "ok", Dependency: "redis", Value: ring})
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryRingMembers, Resource: resource, Outcome: "ok", Dependency: "redis", Value: members})
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryGlobalInflight, Resource: resource, Outcome: "ok", Dependency: "redis", Value: global})
	return RedisResourceHealthProbe{
		Resource: resource, Topology: topology, Control: control,
		ActiveCount: active, RingCount: ring, RingMemberCount: members, GlobalInflight: global,
		ProvisionalCount: provisional, StableCount: stable, ProcessingCount: processing,
	}, nil
}

func (r *Redis) inspectRedisTopology(ctx context.Context) (RedisTopology, error) {
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RedisTopology{}, err
	}
	defer cancel()

	clusterInfo, err := r.client.Info(opCtx, "cluster").Result()
	if err != nil {
		return RedisTopology{}, redisDependencyError("topology probe", err)
	}
	clusterEnabled, ok := redisInfoValue(clusterInfo, "cluster_enabled")
	if !ok || (clusterEnabled != "0" && clusterEnabled != "1") {
		return RedisTopology{}, fmt.Errorf("%w: invalid Redis cluster information", ErrCoordinationCorrupt)
	}
	if clusterEnabled == "1" {
		return RedisTopology{Mode: RedisDeploymentCluster, WritablePrimary: false}, nil
	}

	role, err := r.client.Do(opCtx, "ROLE").Result()
	if err != nil {
		return RedisTopology{}, redisDependencyError("topology probe", err)
	}
	roleValues, ok := role.([]interface{})
	if !ok || len(roleValues) == 0 {
		return RedisTopology{}, fmt.Errorf("%w: invalid Redis role response", ErrCoordinationCorrupt)
	}
	roleName, ok := redisResultString(roleValues[0])
	if !ok {
		return RedisTopology{}, fmt.Errorf("%w: invalid Redis role response", ErrCoordinationCorrupt)
	}
	return RedisTopology{
		Mode:            RedisDeploymentStandalone,
		WritablePrimary: strings.EqualFold(roleName, "master"),
	}, nil
}

func redisInfoValue(info, name string) (string, bool) {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, name+":") {
			return strings.TrimPrefix(line, name+":"), true
		}
	}
	return "", false
}

func (r *Redis) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return nil
	}
	if err := r.client.Close(); err != nil {
		return redisDependencyError("close", err)
	}
	r.closed = true
	return nil
}

func redisDependencyError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(ErrDependencyUnavailable, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrDependencyUnavailable, context.DeadlineExceeded)
	}
	if redisTopologyServerError(err) {
		return fmt.Errorf("%w: Redis topology changed", ErrUnsupportedTopology)
	}
	return fmt.Errorf("%w: Redis %s failed", ErrDependencyUnavailable, operation)
}

func redisTopologyServerError(err error) bool {
	var serverError redis.Error
	if !errors.As(err, &serverError) {
		return false
	}
	message := strings.ToUpper(strings.TrimPrefix(serverError.Error(), "ERR "))
	for _, prefix := range []string{"MOVED ", "ASK ", "CROSSSLOT", "READONLY", "MASTERDOWN"} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
}

func redisScriptError(code string) error {
	switch code {
	case redisResultNotReady:
		return ErrResourceNotReady
	case redisResultFenceMismatch, redisResultInvalidState:
		return ErrFenceMismatch
	case redisResultRecoveryOwnerStale:
		return ErrRecoveryOwnerStale
	case redisResultCoordinationCorrupt:
		return ErrCoordinationCorrupt
	default:
		return fmt.Errorf("%w: unknown Redis script result", ErrCoordinationCorrupt)
	}
}

func redisResultString(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	default:
		return "", false
	}
}

func redisResultInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	case []byte:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func redisDurationMilliseconds(value time.Duration) (int64, error) {
	if value <= 0 || value > maxResourceDuration {
		return 0, fmt.Errorf("%w: Redis TTL is invalid", ErrInvalidModel)
	}
	milliseconds := value.Milliseconds()
	if value%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds <= 0 {
		milliseconds = 1
	}
	return milliseconds, nil
}

func validateRedisWriter(writer string) error {
	if err := (WriterIdentity{Fingerprint: writer}).Validate(); err != nil {
		return fmt.Errorf("%w: invalid Redis writer identity", ErrInvalidModel)
	}
	return nil
}

func validateRedisOperationID(operationID string) error {
	if !lowerHex32Pattern.MatchString(operationID) {
		return fmt.Errorf("%w: invalid recovery operation ID", ErrInvalidModel)
	}
	return nil
}

func (r *Redis) resourceKeys(resource string) (redisKeyspace, error) {
	return buildRedisKeyspace(r.options.KeyPrefix, resource)
}

func recoveryScriptArguments(fence RecoveryFence, extra ...any) []any {
	arguments := []any{
		fence.Epoch,
		fence.WriterFingerprint,
		fence.OwnerToken,
		string(fence.Kind),
		fence.OperationID,
	}
	return append(arguments, extra...)
}

func (r *Redis) runScript(
	ctx context.Context,
	operation string,
	script *redis.Script,
	keys []string,
	arguments ...any,
) (any, error) {
	result, err := script.Run(ctx, r.client, keys, arguments...).Result()
	if err != nil {
		return nil, redisScriptExecutionError(operation, err)
	}
	return result, nil
}

func redisScriptExecutionError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return redisDependencyError(operation, err)
	}
	if redisTopologyServerError(err) {
		return fmt.Errorf("%w: Redis topology changed", ErrUnsupportedTopology)
	}
	var serverError redis.Error
	if errors.As(err, &serverError) {
		message := strings.TrimPrefix(serverError.Error(), "ERR ")
		for _, prefix := range []string{
			"WRONGTYPE", "Error running script", "user_script:",
			"value is not", "invalid Lua", "attempt to", "bad argument",
		} {
			if strings.HasPrefix(message, prefix) || strings.Contains(message, prefix) {
				return fmt.Errorf("%w: Redis script rejected coordination data", ErrCoordinationCorrupt)
			}
		}
	}
	return redisDependencyError(operation, err)
}

func redisScalarResult(result any) (string, error) {
	code, ok := redisResultString(result)
	if !ok {
		return "", fmt.Errorf("%w: malformed Redis script result", ErrCoordinationCorrupt)
	}
	return code, nil
}

func redisArrayResult(result any) ([]interface{}, error) {
	values, ok := result.([]interface{})
	if !ok || len(values) == 0 {
		return nil, fmt.Errorf("%w: malformed Redis script result", ErrCoordinationCorrupt)
	}
	return values, nil
}

func redisExpectOK(result any) error {
	code, err := redisScalarResult(result)
	if err != nil {
		return err
	}
	if code == redisResultOK {
		return nil
	}
	return redisScriptError(code)
}

var (
	redisAcquireRawLockScript = redis.NewScript(redisRawLockFenceLua + `
local key_type = fq_key_type(KEYS[1])
if key_type == 'none' then
  local acquired = redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2])
  if acquired then return 'OK' end
  return 'FQ_NOT_READY'
end
if key_type ~= 'string' then return 'FQ_COORDINATION_CORRUPT' end
local owner = redis.call('GET', KEYS[1])
if not fq_hex(owner, 32) then return 'FQ_COORDINATION_CORRUPT' end
local ttl = redis.call('PTTL', KEYS[1])
if not ttl or ttl <= 0 then return 'FQ_COORDINATION_CORRUPT' end
return 'FQ_NOT_READY'
`)
	redisCheckRawLockScript = redis.NewScript(redisRawLockFenceLua + `
local failure = fq_raw_lock(KEYS[1], ARGV[1])
if failure then return failure end
return 'OK'
`)
	redisRenewRawLockScript = redis.NewScript(redisRawLockFenceLua + `
local failure = fq_raw_lock(KEYS[1], ARGV[1])
if failure then return failure end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 'OK'
`)
	redisReleaseRawLockScript = redis.NewScript(redisRawLockFenceLua + `
local failure = fq_raw_lock(KEYS[1], ARGV[1])
if failure then return failure end
redis.call('DEL', KEYS[1])
return 'OK'
`)
	redisInspectRecoveryScript = redis.NewScript(redisRawLockFenceLua + `
local lock_failure = fq_raw_lock(KEYS[2], ARGV[1])
if lock_failure then return {lock_failure} end
if redis.call('EXISTS', KEYS[1]) ~= 1 then
  if redis.call('EXISTS', KEYS[3]) ~= 0 then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  return {'OK', '0'}
end
local control = redis.call('HMGET', KEYS[1],
  'state', 'epoch', 'protocol_version', 'writer_fingerprint',
  'operation_kind', 'operation_id', 'last_completed_operation_id',
  'last_completed_operation_kind')
for i = 1, 8 do
  if control[i] == false then
    return {'FQ_COORDINATION_CORRUPT'}
  end
end
if not tonumber(control[3]) or tonumber(control[3]) < 1 or
    math.floor(tonumber(control[3])) ~= tonumber(control[3]) or
    not fq_hex(control[2], 32) or not fq_hex(control[4], 64) or
    (control[7] == '' and control[8] ~= 'NONE') or
    (control[7] ~= '' and (not fq_hex(control[7], 32) or
      (control[8] ~= 'RABBIT_REPAIR' and control[8] ~= 'WRITER_REBIND' and
        control[8] ~= 'FORCE_REBUILD'))) then
  return {'FQ_COORDINATION_CORRUPT'}
end
if control[3] ~= ARGV[2] then return {'FQ_FENCE_MISMATCH'} end
if control[1] == 'READY' then
  if control[5] ~= 'NONE' or control[6] ~= '' or redis.call('EXISTS', KEYS[3]) ~= 0 then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  return {'OK', '1', control[1], control[2], control[3], control[4],
    control[5], control[6], control[7], control[8]}
end
if control[1] ~= 'RECOVERING' or control[5] == 'NONE' or redis.call('EXISTS', KEYS[3]) ~= 1 then
  return {'FQ_COORDINATION_CORRUPT'}
end
if control[5] ~= 'NORMAL' and control[7] == control[6] then
  return {'FQ_COORDINATION_CORRUPT'}
end
local progress = redis.call('HMGET', KEYS[3],
  'epoch', 'operation_kind', 'operation_id', 'high_water',
  'known_cycle', 'known_complete', 'known_diff',
  'dispatched_cycle', 'dispatched_complete', 'dispatched_diff',
  'running_cycle', 'running_complete', 'running_diff',
  'repair_high_water', 'repair_complete',
  'rebind_original_writer', 'rebind_target_writer',
  'force_not_before_ms', 'force_delete_complete')
for i = 1, 19 do
  if progress[i] == false then return {'FQ_COORDINATION_CORRUPT'} end
end
if progress[1] ~= control[2] or progress[2] ~= control[5] or progress[3] ~= control[6] then
  return {'FQ_COORDINATION_CORRUPT'}
end
local result = {'OK', '1', control[1], control[2], control[3], control[4],
  control[5], control[6], control[7], control[8]}
for i = 1, 19 do table.insert(result, progress[i]) end
return result
`)
	redisForceDeadlineScript = redis.NewScript(redisRawLockFenceLua + `
local lock_failure = fq_raw_lock(KEYS[1], ARGV[1])
if lock_failure then return {lock_failure} end
local now = redis.call('TIME')
return {'OK', now[1], now[2]}
`)
	redisBeginRecoveryScript = redis.NewScript(redisRawLockFenceLua + `
local lock_failure = fq_raw_lock(KEYS[2], ARGV[1])
if lock_failure then return {lock_failure} end
local requested_kind = ARGV[4]
local operation_id = ARGV[5]
local writer_a = ARGV[6]
local writer_b = ARGV[7]
local force_not_before = ARGV[8]
local exists = redis.call('EXISTS', KEYS[1])
local initialize = false
local takeover = false
local target_writer = writer_a
local last_completed = ''
local last_completed_kind = 'NONE'
local control = nil

local function canonical_uint(value, positive)
  if type(value) ~= 'string' or not string.match(value, '^[0-9]+$') then return false end
  if string.len(value) > 1 and string.sub(value, 1, 1) == '0' then return false end
  if positive and value == '0' then return false end
  if string.len(value) > 19 or
      (string.len(value) == 19 and value > '9223372036854775807') then return false end
  return true
end
local function canonical_bool(value)
  return value == '0' or value == '1'
end
local function canonical_high_water(value)
  if type(value) ~= 'string' or string.len(value) > 191 then return false end
  if value ~= '' and (string.byte(value, 1) == 32 or string.byte(value, -1) == 32) then return false end
  for index = 1, string.len(value) do
    local byte = string.byte(value, index)
    if byte < 32 or byte > 126 then return false end
  end
  return true
end
local function canonical_progress(expected_epoch, expected_kind, expected_operation, expected_writer)
  if redis.call('EXISTS', KEYS[3]) ~= 1 then return false end
  local values = redis.call('HMGET', KEYS[3],
    'epoch', 'operation_kind', 'operation_id', 'high_water',
    'known_cycle', 'known_complete', 'known_diff',
    'dispatched_cycle', 'dispatched_complete', 'dispatched_diff',
    'running_cycle', 'running_complete', 'running_diff',
    'repair_high_water', 'repair_complete',
    'rebind_original_writer', 'rebind_target_writer',
    'force_not_before_ms', 'force_delete_complete')
  for index = 1, 19 do if values[index] == false then return false end end
  if values[1] ~= expected_epoch or values[2] ~= expected_kind or
      values[3] ~= expected_operation or not canonical_high_water(values[4]) then return false end
  for _, index in ipairs({5, 7, 8, 10, 11, 13}) do
    if not canonical_uint(values[index], false) then return false end
  end
  for _, index in ipairs({6, 9, 12, 15, 19}) do
    if not canonical_bool(values[index]) then return false end
  end
  if (values[6] == '1' and values[5] == '0') or
      (values[9] == '1' and values[8] == '0') or
      (values[12] == '1' and values[11] == '0') or
      (values[4] == '' and (values[6] == '1' or values[9] == '1' or values[12] == '1')) then
    return false
  end
  if expected_kind == 'NORMAL' then
    return expected_operation == '' and values[14] == '' and values[15] == '0' and
      values[16] == '' and values[17] == '' and values[18] == '' and values[19] == '0'
  elseif expected_kind == 'RABBIT_REPAIR' then
    return fq_hex(expected_operation, 32) and canonical_high_water(values[14]) and
      (values[15] == '0' or values[14] ~= '') and values[16] == '' and values[17] == '' and
      values[18] == '' and values[19] == '0'
  elseif expected_kind == 'WRITER_REBIND' then
    return fq_hex(expected_operation, 32) and values[14] == '' and values[15] == '0' and
      fq_hex(values[16], 64) and fq_hex(values[17], 64) and values[16] ~= values[17] and
      values[17] == expected_writer and values[18] == '' and values[19] == '0'
  elseif expected_kind == 'FORCE_REBUILD' then
    return fq_hex(expected_operation, 32) and values[14] == '' and values[15] == '0' and
      values[16] == '' and values[17] == '' and canonical_uint(values[18], true)
  end
  return false
end

if exists == 0 then
  if redis.call('EXISTS', KEYS[3]) ~= 0 then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  initialize = true
  if requested_kind == 'WRITER_REBIND' then target_writer = writer_b end
else
  control = redis.call('HMGET', KEYS[1],
    'epoch', 'state', 'operation_kind', 'operation_id',
	    'protocol_version', 'writer_fingerprint', 'last_completed_operation_id',
	    'last_completed_operation_kind')
	  for i = 1, 8 do
	    if control[i] == false then return {'FQ_COORDINATION_CORRUPT'} end
	  end
	  if not fq_hex(control[1], 32) or not fq_hex(control[6], 64) or
	      (control[7] == '' and control[8] ~= 'NONE') or
	      (control[7] ~= '' and (not fq_hex(control[7], 32) or
	        (control[8] ~= 'RABBIT_REPAIR' and control[8] ~= 'WRITER_REBIND' and
	          control[8] ~= 'FORCE_REBUILD'))) or
	      not canonical_uint(control[5], true) then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  if control[5] ~= ARGV[9] then return {'FQ_FENCE_MISMATCH'} end
  if control[2] == 'READY' then
    if control[3] ~= 'NONE' or control[4] ~= '' then return {'FQ_COORDINATION_CORRUPT'} end
  elseif control[2] == 'RECOVERING' then
    if control[3] == 'NORMAL' then
      if control[4] ~= '' then return {'FQ_COORDINATION_CORRUPT'} end
    elseif control[3] ~= 'RABBIT_REPAIR' and control[3] ~= 'WRITER_REBIND' and
        control[3] ~= 'FORCE_REBUILD' then
      return {'FQ_COORDINATION_CORRUPT'}
    elseif not fq_hex(control[4], 32) then
      return {'FQ_COORDINATION_CORRUPT'}
    end
    if control[3] ~= 'NORMAL' and control[7] == control[4] then
      return {'FQ_COORDINATION_CORRUPT'}
    end
    if not canonical_progress(control[1], control[3], control[4], control[6]) then
      return {'FQ_COORDINATION_CORRUPT'}
    end
  else
    return {'FQ_COORDINATION_CORRUPT'}
  end
	  last_completed = control[7]
	  last_completed_kind = control[8]

  if control[2] == 'READY' then
    if control[3] ~= 'NONE' or control[4] ~= '' or redis.call('EXISTS', KEYS[3]) ~= 0 then
      return {'FQ_COORDINATION_CORRUPT'}
    end
    if requested_kind == 'FORCE_REBUILD' then return {'FQ_NOT_READY'} end
    if requested_kind == 'WRITER_REBIND' then
      if control[6] ~= writer_a then return {'FQ_FENCE_MISMATCH'} end
      if last_completed == operation_id then return {'FQ_INVALID_STATE'} end
      target_writer = writer_b
    else
      if control[6] ~= writer_a then return {'FQ_FENCE_MISMATCH'} end
      if requested_kind ~= 'NORMAL' and last_completed == operation_id then
        return {'FQ_INVALID_STATE'}
      end
      target_writer = writer_a
    end
    initialize = true
  elseif control[2] == 'RECOVERING' then
    if requested_kind == 'FORCE_REBUILD' and control[3] == 'NORMAL' then
      if control[6] ~= writer_a or control[4] ~= '' then return {'FQ_FENCE_MISMATCH'} end
      initialize = true
      target_writer = writer_a
    elseif control[3] == requested_kind and control[4] == operation_id then
      if requested_kind == 'WRITER_REBIND' then
        if control[6] ~= writer_b then return {'FQ_FENCE_MISMATCH'} end
        local writers = redis.call('HMGET', KEYS[3], 'rebind_original_writer', 'rebind_target_writer')
        if writers[1] ~= writer_a or writers[2] ~= writer_b then return {'FQ_FENCE_MISMATCH'} end
        target_writer = writer_b
      else
        if control[6] ~= writer_a then return {'FQ_FENCE_MISMATCH'} end
        target_writer = writer_a
      end
      if requested_kind == 'FORCE_REBUILD' then
        local existing_not_before = redis.call('HGET', KEYS[3], 'force_not_before_ms')
        if existing_not_before ~= force_not_before then return {'FQ_FENCE_MISMATCH'} end
      end
      takeover = true
    else
      return {'FQ_FENCE_MISMATCH'}
    end
  else
    return {'FQ_COORDINATION_CORRUPT'}
  end
end

if requested_kind ~= 'NORMAL' and requested_kind ~= 'RABBIT_REPAIR' and
    requested_kind ~= 'WRITER_REBIND' and requested_kind ~= 'FORCE_REBUILD' then
  return {'FQ_INVALID_STATE'}
end
if requested_kind == 'NORMAL' and operation_id ~= '' then return {'FQ_INVALID_STATE'} end
if requested_kind ~= 'NORMAL' and operation_id == '' then return {'FQ_INVALID_STATE'} end

redis.call('HSET', KEYS[1],
  'epoch', ARGV[2],
  'state', 'RECOVERING',
  'operation_kind', requested_kind,
  'operation_id', operation_id,
  'last_completed_operation_id', last_completed,
	  'last_completed_operation_kind', last_completed_kind,
  'protocol_version', ARGV[9],
  'writer_fingerprint', target_writer)
if initialize then
  redis.call('DEL', KEYS[3])
  redis.call('HSET', KEYS[3],
    'epoch', ARGV[2],
    'operation_kind', requested_kind,
    'operation_id', operation_id,
    'high_water', '',
    'known_cycle', '0', 'known_complete', '0', 'known_diff', '0',
    'dispatched_cycle', '0', 'dispatched_complete', '0', 'dispatched_diff', '0',
    'running_cycle', '0', 'running_complete', '0', 'running_diff', '0',
    'repair_high_water', '', 'repair_complete', '0',
    'rebind_original_writer', '', 'rebind_target_writer', '',
    'force_not_before_ms', '', 'force_delete_complete', '0')
  if requested_kind == 'WRITER_REBIND' then
    redis.call('HSET', KEYS[3],
      'rebind_original_writer', writer_a, 'rebind_target_writer', writer_b)
  elseif requested_kind == 'FORCE_REBUILD' then
    redis.call('HSET', KEYS[3], 'force_not_before_ms', force_not_before)
  end
elseif takeover then
  redis.call('HSET', KEYS[3], 'epoch', ARGV[2])
end
redis.call('PEXPIRE', KEYS[2], ARGV[3])
return {'OK', ARGV[2], target_writer}
`)
	redisRenewRecoveryScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
redis.call('PEXPIRE', KEYS[2], ARGV[6])
return 'OK'
`)
	redisCheckRecoveryScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
return 'OK'
`)
	redisResetResourceScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
if redis.call('ZCARD', KEYS[9]) ~= 0 or redis.call('ZCARD', KEYS[16]) ~= 0 then
  return 'FQ_NOT_READY'
end
for index = 4, #KEYS do
  redis.call('DEL', KEYS[index])
end
return 'OK'
`)
	redisSetHighWaterScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
local current = redis.call('HGET', KEYS[3], 'high_water')
if current and current ~= '' and current ~= ARGV[6] then
  return 'FQ_FENCE_MISMATCH'
end
if not current or current == '' then
  redis.call('HSET', KEYS[3],
    'high_water', ARGV[6],
    'known_cycle', '0', 'known_complete', '0', 'known_diff', '0',
    'dispatched_cycle', '0', 'dispatched_complete', '0', 'dispatched_diff', '0',
    'running_cycle', '0', 'running_complete', '0', 'running_diff', '0')
end
return 'OK'
`)
	redisMarkRecoveryPassScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
if redis.call('HGET', KEYS[3], 'high_water') == '' then return 'FQ_NOT_READY' end
local prefix = ARGV[6]
if prefix ~= 'known' and prefix ~= 'dispatched' and prefix ~= 'running' then
  return 'FQ_INVALID_STATE'
end
local current_cycle = tonumber(redis.call('HGET', KEYS[3], prefix .. '_cycle'))
local current_complete = redis.call('HGET', KEYS[3], prefix .. '_complete')
local current_diff = tonumber(redis.call('HGET', KEYS[3], prefix .. '_diff'))
local requested_cycle = tonumber(ARGV[7])
local requested_complete = ARGV[8]
local requested_diff = tonumber(ARGV[9])
if not current_cycle or not current_diff or not requested_cycle or not requested_diff then
  return 'FQ_COORDINATION_CORRUPT'
end
if requested_cycle < current_cycle then return 'FQ_FENCE_MISMATCH' end
if requested_cycle == current_cycle and current_complete == '1' then
  if requested_complete == '1' and requested_diff == current_diff then return 'OK' end
  return 'FQ_FENCE_MISMATCH'
end
redis.call('HSET', KEYS[3],
  prefix .. '_cycle', ARGV[7],
  prefix .. '_complete', requested_complete,
  prefix .. '_diff', ARGV[9])
return 'OK'
`)
	redisSetRabbitHighWaterScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
if ARGV[4] ~= 'RABBIT_REPAIR' then return 'FQ_FENCE_MISMATCH' end
local current = redis.call('HGET', KEYS[3], 'repair_high_water')
if current and current ~= '' and current ~= ARGV[6] then return 'FQ_FENCE_MISMATCH' end
if not current or current == '' then
  redis.call('HSET', KEYS[3], 'repair_high_water', ARGV[6], 'repair_complete', '0')
end
return 'OK'
`)
	redisMarkRabbitCompleteScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
if ARGV[4] ~= 'RABBIT_REPAIR' then return 'FQ_FENCE_MISMATCH' end
local high_water = redis.call('HGET', KEYS[3], 'repair_high_water')
if not high_water or high_water == '' then return 'FQ_NOT_READY' end
redis.call('HSET', KEYS[3], 'repair_complete', '1')
return 'OK'
`)
	redisMarkForceDeleteCompleteScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
if ARGV[4] ~= 'FORCE_REBUILD' then return 'FQ_FENCE_MISMATCH' end
redis.call('HSET', KEYS[3], 'force_delete_complete', '1')
return 'OK'
`)
	redisRecoveryReapExpiredScript = redis.NewScript(redisSHA256Lua + redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return {failure} end
local expected_types = {'list', 'set', 'set', 'zset', 'hash', 'hash', 'hash', 'zset', 'zset', 'hash', 'hash', 'zset', 'set'}
for index = 4, 16 do
  if not fq_type_is(KEYS[index], expected_types[index - 3]) then
    return {'FQ_COORDINATION_CORRUPT'}
  end
end
local now_parts = redis.call('TIME')
local now_ms = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
local limit = tonumber(ARGV[6])
if not limit or limit < 1 then return {'FQ_INVALID_STATE'} end
local provisionals = redis.call('ZRANGEBYSCORE', KEYS[11], '-inf', now_ms, 'LIMIT', 0, limit)
local remaining = limit - #provisionals
local turns = {}
if remaining > 0 then
  turns = redis.call('ZRANGEBYSCORE', KEYS[7], '-inf', now_ms, 'LIMIT', 0, remaining)
end
local turn_data = {}
local provisional_data = {}

for index, token in ipairs(turns) do
  local tenant = redis.call('HGET', KEYS[8], token)
  local generation = redis.call('HGET', KEYS[9], token)
  local turn_score = redis.call('ZSCORE', KEYS[7], token)
  if not fq_hex(token, 32) or not tenant or tenant == '' or string.len(tenant) > 480 or
      not fq_uint(generation, true) or not turn_score or not tonumber(turn_score) or
      tonumber(turn_score) <= 0 or math.floor(tonumber(turn_score)) ~= tonumber(turn_score) or
      redis.call('HGET', KEYS[10], tenant) ~= token or
      tonumber(turn_score) > now_ms then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local current_generation = redis.call('HGET', KEYS[14], tenant)
  local ring_positions = redis.call('LPOS', KEYS[4], tenant, 'COUNT', 2)
  if not fq_uint(current_generation, true) or tonumber(current_generation) < tonumber(generation) or
      redis.call('SISMEMBER', KEYS[16], tenant) ~= 1 or
      redis.call('SISMEMBER', KEYS[6], tenant) ~= 1 or
      redis.call('SISMEMBER', KEYS[5], tenant) ~= 0 or #ring_positions ~= 0 then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  table.insert(turn_data, {token, tenant})
end

for index, token in ipairs(provisionals) do
  if string.len(token) ~= 34 or string.sub(token, 1, 2) ~= 'p:' or
      not string.match(string.sub(token, 3), '^[0-9a-f]+$') then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local raw = redis.call('HGET', KEYS[13], token)
  if not raw then return {'FQ_COORDINATION_CORRUPT'} end
  local decoded, metadata = pcall(cjson.decode, raw)
  if not decoded or type(metadata) ~= 'table' or metadata.kind ~= 'P' or
      type(metadata.tenant) ~= 'string' or metadata.tenant == '' or
      string.len(metadata.tenant) > 480 or type(metadata.tenant_key) ~= 'string' or
      type(metadata.decision) ~= 'string' then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local field_count = 0
  for name, value in pairs(metadata) do
    if name ~= 'tenant' and name ~= 'tenant_key' and name ~= 'kind' and name ~= 'decision' then
      return {'FQ_COORDINATION_CORRUPT'}
    end
    field_count = field_count + 1
  end
  if field_count ~= 4 or
      (metadata.decision ~= 'regular' and metadata.decision ~= 'borrowed') then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local tenant_key = metadata.tenant_key
  local key_prefix = ARGV[7]
  local suffix = string.sub(tenant_key, string.len(key_prefix) + 1)
  if string.sub(tenant_key, 1, string.len(key_prefix)) ~= key_prefix or
      string.len(suffix) ~= 64 or not string.match(suffix, '^[0-9a-f]+$') or
      suffix ~= fq_sha256(ARGV[8] .. string.char(0) .. metadata.tenant) then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local provisional_score = redis.call('ZSCORE', KEYS[11], token)
  local global_score = redis.call('ZSCORE', KEYS[12], token)
  local tenant_score = redis.call('ZSCORE', tenant_key, token)
  local stable_score = redis.call('ZSCORE', KEYS[15], token)
  if not provisional_score or not global_score or not tenant_score or
      stable_score or
      not tonumber(provisional_score) or tonumber(provisional_score) <= 0 or
      math.floor(tonumber(provisional_score)) ~= tonumber(provisional_score) or
      not tonumber(global_score) or math.floor(tonumber(global_score)) ~= tonumber(global_score) or
      not tonumber(tenant_score) or math.floor(tonumber(tenant_score)) ~= tonumber(tenant_score) or
      tonumber(provisional_score) ~= tonumber(global_score) or
      tonumber(provisional_score) ~= tonumber(tenant_score) then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  table.insert(provisional_data, {token, tenant_key})
end

for index, item in ipairs(turn_data) do
  local token = item[1]
  local tenant = item[2]
  redis.call('ZREM', KEYS[7], token)
  redis.call('HDEL', KEYS[8], token)
  redis.call('HDEL', KEYS[9], token)
  redis.call('HDEL', KEYS[10], tenant)
  if redis.call('SISMEMBER', KEYS[6], tenant) == 1 and
      redis.call('SADD', KEYS[5], tenant) == 1 then
    redis.call('RPUSH', KEYS[4], tenant)
  end
end
for index, item in ipairs(provisional_data) do
  local token = item[1]
  local tenant_key = item[2]
  redis.call('ZREM', KEYS[11], token)
  redis.call('ZREM', KEYS[12], token)
  redis.call('ZREM', tenant_key, token)
  redis.call('HDEL', KEYS[13], token)
end
return {'OK', tostring(#provisional_data), tostring(#turn_data),
  tostring(redis.call('ZCARD', KEYS[11])), tostring(redis.call('ZCARD', KEYS[7]))}
`)
	redisRestoreKnownScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
if not fq_type_is(KEYS[4], 'set') then return 'FQ_COORDINATION_CORRUPT' end
redis.call('SADD', KEYS[4], ARGV[6])
return 'OK'
`)
	redisRestoreActiveScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
local expected_types = {'set', 'set', 'hash', 'list', 'set', 'hash'}
for index = 4, 9 do
  if not fq_type_is(KEYS[index], expected_types[index - 3]) then
    return 'FQ_COORDINATION_CORRUPT'
  end
end
if redis.call('HGET', KEYS[9], ARGV[6]) then return 'FQ_COORDINATION_CORRUPT' end
local known = redis.call('SISMEMBER', KEYS[4], ARGV[6])
local active = redis.call('SISMEMBER', KEYS[5], ARGV[6])
local member = redis.call('SISMEMBER', KEYS[8], ARGV[6])
local positions = redis.call('LPOS', KEYS[7], ARGV[6], 'COUNT', 2)
local generation = redis.call('HGET', KEYS[6], ARGV[6])
if (member == 1 and #positions ~= 1) or (member == 0 and #positions ~= 0) or
    (active == 1 and (known ~= 1 or member ~= 1 or not fq_uint(generation, true))) or
    (active == 0 and member ~= 0) or
    (known == 0 and generation) or
    (generation and not fq_uint(generation, true)) then
  return 'FQ_COORDINATION_CORRUPT'
end
if active == 1 then return 'OK' end
if generation == '9223372036854775807' then return 'FQ_COORDINATION_CORRUPT' end
redis.call('HINCRBY', KEYS[6], ARGV[6], 1)
redis.call('SADD', KEYS[4], ARGV[6])
redis.call('SADD', KEYS[5], ARGV[6])
if redis.call('SADD', KEYS[8], ARGV[6]) ~= 1 then
  return 'FQ_COORDINATION_CORRUPT'
end
redis.call('RPUSH', KEYS[7], ARGV[6])
return 'OK'
`)
	redisRestoreInflightScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
for index = 4, 6 do
  if not fq_type_is(KEYS[index], 'zset') then return 'FQ_COORDINATION_CORRUPT' end
end
if not fq_type_is(KEYS[7], 'hash') then return 'FQ_COORDINATION_CORRUPT' end
if not fq_type_is(KEYS[8], 'zset') then return 'FQ_COORDINATION_CORRUPT' end
if redis.call('ZSCORE', KEYS[8], ARGV[7]) then return 'FQ_COORDINATION_CORRUPT' end
local raw = redis.call('HGET', KEYS[7], ARGV[7])
if raw then
  local decoded, metadata = pcall(cjson.decode, raw)
  if not decoded or type(metadata) ~= 'table' or metadata.kind ~= 'S' or
      metadata.tenant ~= ARGV[6] or metadata.tenant_key ~= KEYS[4] then
    return 'FQ_FENCE_MISMATCH'
  end
  local field_count = 0
  for name, value in pairs(metadata) do
    if name ~= 'tenant' and name ~= 'tenant_key' and name ~= 'kind' then
      return 'FQ_COORDINATION_CORRUPT'
    end
    field_count = field_count + 1
  end
  if field_count ~= 3 then return 'FQ_COORDINATION_CORRUPT' end
end
local now_parts = redis.call('TIME')
local expires_at = tonumber(now_parts[1]) * 1000 +
  math.floor(tonumber(now_parts[2]) / 1000) + tonumber(ARGV[8])
local metadata = cjson.encode({tenant=ARGV[6], tenant_key=KEYS[4], kind='S'})
redis.call('ZADD', KEYS[5], expires_at, ARGV[7])
redis.call('ZADD', KEYS[4], expires_at, ARGV[7])
redis.call('ZADD', KEYS[6], 0, ARGV[7])
redis.call('HSET', KEYS[7], ARGV[7], metadata)
return 'OK'
`)
	redisListRecoveryStableScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return {failure} end
for index = 4, 5 do
  if not fq_type_is(KEYS[index], 'zset') then return {'FQ_COORDINATION_CORRUPT'} end
end
if not fq_type_is(KEYS[6], 'hash') or not fq_type_is(KEYS[7], 'zset') then
  return {'FQ_COORDINATION_CORRUPT'}
end
if redis.call('ZCOUNT', KEYS[4], 0, 0) ~= redis.call('ZCARD', KEYS[4]) then
  return {'FQ_COORDINATION_CORRUPT'}
end
local minimum = '-'
if ARGV[6] ~= '' then minimum = '(' .. ARGV[6] end
local requested = tonumber(ARGV[7])
if not requested or requested < 1 then return {'FQ_INVALID_STATE'} end
local tokens = redis.call('ZRANGEBYLEX', KEYS[4], minimum, '+', 'LIMIT', 0, requested + 1)
local count = #tokens
local has_more = '0'
if count > requested then
  has_more = '1'
  count = requested
end
local result = {'OK', has_more}
for index = 1, count do
  local token = tokens[index]
  if string.len(token) ~= 66 or string.sub(token, 1, 2) ~= 'r:' or
      not string.match(string.sub(token, 3), '^[0-9a-f]+$') then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local raw = redis.call('HGET', KEYS[6], token)
  if not raw then return {'FQ_COORDINATION_CORRUPT'} end
  local decoded, metadata = pcall(cjson.decode, raw)
  if not decoded or type(metadata) ~= 'table' or metadata.kind ~= 'S' or
      type(metadata.tenant) ~= 'string' or metadata.tenant == '' or
      type(metadata.tenant_key) ~= 'string' then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local field_count = 0
  for name, value in pairs(metadata) do
    if name ~= 'tenant' and name ~= 'tenant_key' and name ~= 'kind' then
      return {'FQ_COORDINATION_CORRUPT'}
    end
    field_count = field_count + 1
  end
  local tenant_suffix = string.sub(metadata.tenant_key, string.len(ARGV[8]) + 1)
  if field_count ~= 3 or
      string.sub(metadata.tenant_key, 1, string.len(ARGV[8])) ~= ARGV[8] or
      string.len(tenant_suffix) ~= 64 or not string.match(tenant_suffix, '^[0-9a-f]+$') then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  local expires_at = redis.call('ZSCORE', KEYS[5], token)
  local tenant_expires = redis.call('ZSCORE', metadata.tenant_key, token)
  local index_score = redis.call('ZSCORE', KEYS[4], token)
  local provisional_score = redis.call('ZSCORE', KEYS[7], token)
  if not expires_at or not tenant_expires or not index_score or
      tonumber(index_score) ~= 0 or provisional_score or
      tonumber(expires_at) ~= tonumber(tenant_expires) then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  table.insert(result, token)
  table.insert(result, metadata.tenant)
  table.insert(result, metadata.tenant_key)
  table.insert(result, tostring(math.floor(tonumber(expires_at))))
end
return result
`)
	redisDeleteRecoveryStableScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
local raw = redis.call('HGET', KEYS[6], ARGV[7])
local global_score = redis.call('ZSCORE', KEYS[5], ARGV[7])
local index_score = redis.call('ZSCORE', KEYS[4], ARGV[7])
local provisional_score = redis.call('ZSCORE', KEYS[7], ARGV[7])
local tenant_score = redis.call('ZSCORE', KEYS[8], ARGV[7])
if not raw and not global_score and not index_score and not provisional_score and not tenant_score then
  return 'OK'
end
if not raw or not global_score or not index_score or provisional_score or not tenant_score then
  return 'FQ_COORDINATION_CORRUPT'
end
local decoded, metadata = pcall(cjson.decode, raw)
if not decoded or type(metadata) ~= 'table' or metadata.kind ~= 'S' or
    metadata.tenant ~= ARGV[6] or metadata.tenant_key ~= KEYS[8] then
  return 'FQ_FENCE_MISMATCH'
end
local field_count = 0
for name, value in pairs(metadata) do
  if name ~= 'tenant' and name ~= 'tenant_key' and name ~= 'kind' then
    return 'FQ_COORDINATION_CORRUPT'
  end
  field_count = field_count + 1
end
if field_count ~= 3 or tonumber(index_score) ~= 0 or
    tonumber(global_score) ~= tonumber(tenant_score) then
  return 'FQ_COORDINATION_CORRUPT'
end
if math.floor(tonumber(global_score)) ~= tonumber(ARGV[8]) or
    math.floor(tonumber(tenant_score)) ~= tonumber(ARGV[8]) then
  return 'FQ_FENCE_MISMATCH'
end
redis.call('ZREM', KEYS[4], ARGV[7])
redis.call('ZREM', KEYS[5], ARGV[7])
redis.call('ZREM', KEYS[8], ARGV[7])
redis.call('HDEL', KEYS[6], ARGV[7])
return 'OK'
`)
	redisDeleteOwnedKeysScript = redis.NewScript(redisRecoveryFenceLua + `
local failure = fq_recovery_fence()
if failure then return failure end
for index = 4, #KEYS do
  local key = KEYS[index]
  if string.sub(key, 1, string.len(ARGV[6])) ~= ARGV[6] or
      key == ARGV[7] or key == ARGV[8] or key == ARGV[9] then
    return 'FQ_INVALID_STATE'
  end
end
for index = 4, #KEYS do redis.call('DEL', KEYS[index]) end
return 'OK'
`)
	redisFinishRecoveryScript = redis.NewScript(redisRecoveryFenceLua + `
if redis.call('EXISTS', KEYS[1]) == 1 then
  local completed = redis.call('HMGET', KEYS[1],
    'epoch', 'state', 'operation_kind', 'operation_id',
	    'protocol_version', 'writer_fingerprint', 'last_completed_operation_id',
	    'last_completed_operation_kind')
  if completed[1] == ARGV[1] and completed[2] == 'READY' and completed[6] == ARGV[2] then
	    for index = 1, 8 do
      if completed[index] == false then return 'FQ_COORDINATION_CORRUPT' end
    end
    if completed[3] ~= 'NONE' or completed[4] ~= '' or not fq_hex(completed[1], 32) or
        not fq_hex(completed[6], 64) or
	        (completed[7] == '' and completed[8] ~= 'NONE') or
	        (completed[7] ~= '' and (not fq_hex(completed[7], 32) or
	          (completed[8] ~= 'RABBIT_REPAIR' and completed[8] ~= 'WRITER_REBIND' and
	            completed[8] ~= 'FORCE_REBUILD'))) or
        not tonumber(completed[5]) or tonumber(completed[5]) < 1 or
        math.floor(tonumber(completed[5])) ~= tonumber(completed[5]) then
      return 'FQ_COORDINATION_CORRUPT'
    end
    if redis.call('EXISTS', KEYS[3]) ~= 0 then return 'FQ_COORDINATION_CORRUPT' end
    if completed[5] ~= '1' then return 'FQ_FENCE_MISMATCH' end
    if (ARGV[4] == 'NORMAL' and ARGV[5] == '') or
	        (ARGV[4] ~= 'NORMAL' and completed[7] == ARGV[5] and completed[8] == ARGV[4]) then
      return 'OK'
    end
    return 'FQ_FENCE_MISMATCH'
  end
end
local failure = fq_recovery_fence()
if failure then return failure end
if redis.call('ZCARD', KEYS[4]) ~= 0 or redis.call('ZCARD', KEYS[5]) ~= 0 then
  return 'FQ_NOT_READY'
end
if not fq_type_is(KEYS[6], 'list') or not fq_type_is(KEYS[7], 'set') or
    not fq_type_is(KEYS[8], 'set') or not fq_type_is(KEYS[9], 'set') or
    not fq_type_is(KEYS[10], 'hash') or not fq_type_is(KEYS[11], 'hash') or
    not fq_type_is(KEYS[12], 'hash') or not fq_type_is(KEYS[13], 'hash') then
  return 'FQ_COORDINATION_CORRUPT'
end
if redis.call('HLEN', KEYS[10]) ~= 0 or redis.call('HLEN', KEYS[11]) ~= 0 or
    redis.call('HLEN', KEYS[12]) ~= 0 then
  return 'FQ_COORDINATION_CORRUPT'
end
local ring_length = redis.call('LLEN', KEYS[6])
local member_count = redis.call('SCARD', KEYS[7])
local active_count = redis.call('SCARD', KEYS[8])
if ring_length ~= member_count or ring_length ~= active_count then
  return 'FQ_COORDINATION_CORRUPT'
end
local seen = {}
local ring = redis.call('LRANGE', KEYS[6], 0, -1)
for index, tenant in ipairs(ring) do
  if tenant == '' or string.len(tenant) > 480 or seen[tenant] or
      redis.call('SISMEMBER', KEYS[7], tenant) ~= 1 or
      redis.call('SISMEMBER', KEYS[8], tenant) ~= 1 or
      redis.call('SISMEMBER', KEYS[9], tenant) ~= 1 or
      not fq_uint(redis.call('HGET', KEYS[13], tenant), true) then
    return 'FQ_COORDINATION_CORRUPT'
  end
  seen[tenant] = true
end
local common = redis.call('HMGET', KEYS[3],
  'high_water', 'known_complete', 'known_diff',
  'dispatched_complete', 'dispatched_diff', 'running_complete', 'running_diff')
if not common[1] or common[1] == '' or common[2] ~= '1' or common[3] ~= '0' or
    common[4] ~= '1' or common[5] ~= '0' or common[6] ~= '1' or common[7] ~= '0' then
  return 'FQ_NOT_READY'
end
if ARGV[4] == 'RABBIT_REPAIR' then
  local repair = redis.call('HMGET', KEYS[3], 'repair_high_water', 'repair_complete')
  if not repair[1] or repair[1] == '' or repair[2] ~= '1' then return 'FQ_NOT_READY' end
elseif ARGV[4] == 'WRITER_REBIND' then
  local writers = redis.call('HMGET', KEYS[3], 'rebind_original_writer', 'rebind_target_writer')
  if not writers[1] or writers[1] == '' or not writers[2] or writers[2] == '' or
      writers[1] == writers[2] or writers[2] ~= ARGV[2] then
    return 'FQ_COORDINATION_CORRUPT'
  end
elseif ARGV[4] == 'FORCE_REBUILD' then
  local force = redis.call('HMGET', KEYS[3], 'force_not_before_ms', 'force_delete_complete')
  if not force[1] or not tonumber(force[1]) or tonumber(force[1]) <= 0 or force[2] ~= '1' then
    return 'FQ_NOT_READY'
  end
  local now_parts = redis.call('TIME')
  local now_ms = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
  if now_ms < tonumber(force[1]) then return 'FQ_NOT_READY' end
elseif ARGV[4] ~= 'NORMAL' then
  return 'FQ_FENCE_MISMATCH'
end
local last_completed = redis.call('HGET', KEYS[1], 'last_completed_operation_id') or ''
local last_completed_kind = redis.call('HGET', KEYS[1], 'last_completed_operation_kind') or 'NONE'
if ARGV[4] ~= 'NORMAL' then
  last_completed = ARGV[5]
  last_completed_kind = ARGV[4]
end
redis.call('HSET', KEYS[1],
  'state', 'READY', 'operation_kind', 'NONE', 'operation_id', '',
	  'last_completed_operation_id', last_completed,
	  'last_completed_operation_kind', last_completed_kind)
redis.call('DEL', KEYS[3])
redis.call('DEL', KEYS[2])
return 'OK'
`)
)

func (r *Redis) AcquireRecoveryLock(
	ctx context.Context,
	resource, owner string,
	ttl time.Duration,
) (RecoveryLock, error) {
	if err := ValidateResource(resource); err != nil {
		return RecoveryLock{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	lock := RecoveryLock{OwnerToken: owner}
	if err := lock.Validate(); err != nil {
		return RecoveryLock{}, fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	ttlMilliseconds, err := redisDurationMilliseconds(ttl)
	if err != nil {
		return RecoveryLock{}, err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryLock{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryLock{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "acquire recovery lock", redisAcquireRawLockScript,
		[]string{keys.recoveryLock}, owner, ttlMilliseconds)
	if err != nil {
		return RecoveryLock{}, err
	}
	if err := redisExpectOK(result); err != nil {
		return RecoveryLock{}, err
	}
	return lock, nil
}

func (r *Redis) CheckRecoveryLock(ctx context.Context, resource string, lock RecoveryLock) error {
	if err := ValidateResource(resource); err != nil {
		return fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "check recovery lock", redisCheckRawLockScript,
		[]string{keys.recoveryLock}, lock.OwnerToken)
	if err != nil {
		return err
	}
	return redisExpectOK(result)
}

func (r *Redis) RenewRecoveryLock(
	ctx context.Context,
	resource string,
	lock RecoveryLock,
	ttl time.Duration,
) error {
	if err := ValidateResource(resource); err != nil {
		return fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	ttlMilliseconds, err := redisDurationMilliseconds(ttl)
	if err != nil {
		return err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "renew recovery lock", redisRenewRawLockScript,
		[]string{keys.recoveryLock}, lock.OwnerToken, ttlMilliseconds)
	if err != nil {
		return err
	}
	return redisExpectOK(result)
}

func (r *Redis) ReleaseRecoveryLock(ctx context.Context, resource string, lock RecoveryLock) error {
	if err := ValidateResource(resource); err != nil {
		return fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := lock.Validate(); err != nil {
		return fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "release recovery lock", redisReleaseRawLockScript,
		[]string{keys.recoveryLock}, lock.OwnerToken)
	if err != nil {
		return err
	}
	return redisExpectOK(result)
}

func redisArrayString(values []interface{}, index int) (string, error) {
	if index < 0 || index >= len(values) {
		return "", fmt.Errorf("%w: truncated Redis script result", ErrCoordinationCorrupt)
	}
	value, ok := redisResultString(values[index])
	if !ok {
		return "", fmt.Errorf("%w: malformed Redis script result", ErrCoordinationCorrupt)
	}
	return value, nil
}

func redisArrayInt64(values []interface{}, index int, emptyAllowed bool) (int64, error) {
	if index < 0 || index >= len(values) {
		return 0, fmt.Errorf("%w: truncated Redis script result", ErrCoordinationCorrupt)
	}
	value, err := redisArrayString(values, index)
	if err != nil {
		if parsed, ok := redisResultInt64(values[index]); ok {
			return parsed, nil
		}
		return 0, err
	}
	if value == "" && emptyAllowed {
		return 0, nil
	}
	parsed, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("%w: malformed Redis integer result", ErrCoordinationCorrupt)
	}
	return parsed, nil
}

func redisArrayBool(values []interface{}, index int) (bool, error) {
	value, err := redisArrayString(values, index)
	if err != nil {
		return false, err
	}
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%w: malformed Redis boolean result", ErrCoordinationCorrupt)
	}
}

func parseRecoverySnapshot(values []interface{}) (RecoveryControlSnapshot, error) {
	code, err := redisArrayString(values, 0)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	if code != redisResultOK {
		return RecoveryControlSnapshot{}, redisScriptError(code)
	}
	present, err := redisArrayString(values, 1)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	if present == "0" {
		if len(values) != 2 {
			return RecoveryControlSnapshot{}, fmt.Errorf("%w: malformed missing-control response", ErrCoordinationCorrupt)
		}
		return RecoveryControlSnapshot{}, nil
	}
	if present != "1" || len(values) < 10 {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: malformed Redis control response", ErrCoordinationCorrupt)
	}
	state, err := redisArrayString(values, 2)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	epoch, err := redisArrayString(values, 3)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	protocol, err := redisArrayInt64(values, 4, false)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	writer, err := redisArrayString(values, 5)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	kind, err := redisArrayString(values, 6)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	operationID, err := redisArrayString(values, 7)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	lastCompleted, err := redisArrayString(values, 8)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	lastCompletedKind, err := redisArrayString(values, 9)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	snapshot := RecoveryControlSnapshot{
		Present:                    true,
		State:                      ResourceState(state),
		Epoch:                      epoch,
		ProtocolVersion:            int(protocol),
		WriterFingerprint:          writer,
		Kind:                       RecoveryKind(kind),
		OperationID:                operationID,
		LastCompletedOperationID:   lastCompleted,
		LastCompletedOperationKind: RecoveryKind(lastCompletedKind),
	}
	if snapshot.State == ResourceReady {
		if len(values) != 10 {
			return RecoveryControlSnapshot{}, fmt.Errorf("%w: READY response retains progress", ErrCoordinationCorrupt)
		}
		if err := snapshot.Validate(); err != nil {
			return RecoveryControlSnapshot{}, fmt.Errorf("%w: invalid Redis control", ErrCoordinationCorrupt)
		}
		return snapshot, nil
	}
	if len(values) != 29 {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: malformed Redis recovery progress", ErrCoordinationCorrupt)
	}
	progressEpoch, err := redisArrayString(values, 10)
	if err != nil || progressEpoch != snapshot.Epoch {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: recovery progress epoch mismatch", ErrCoordinationCorrupt)
	}
	progressKind, err := redisArrayString(values, 11)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	progressOperation, err := redisArrayString(values, 12)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	highWater, err := redisArrayString(values, 13)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	readPass := func(start int) (RecoveryPassProgress, error) {
		cycle, cycleErr := redisArrayInt64(values, start, true)
		if cycleErr != nil || cycle < 0 {
			return RecoveryPassProgress{}, fmt.Errorf("%w: malformed recovery cycle", ErrCoordinationCorrupt)
		}
		complete, completeErr := redisArrayBool(values, start+1)
		if completeErr != nil {
			return RecoveryPassProgress{}, completeErr
		}
		diff, diffErr := redisArrayInt64(values, start+2, true)
		if diffErr != nil || diff < 0 {
			return RecoveryPassProgress{}, fmt.Errorf("%w: malformed recovery diff", ErrCoordinationCorrupt)
		}
		return RecoveryPassProgress{Cycle: uint64(cycle), Complete: complete, DiffCount: diff}, nil
	}
	known, err := readPass(14)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	dispatched, err := readPass(17)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	running, err := readPass(20)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	progress := RecoveryProgress{
		Kind:         RecoveryKind(progressKind),
		OperationID:  progressOperation,
		HighWater:    highWater,
		KnownTenants: known,
		Dispatched:   dispatched,
		Running:      running,
	}
	switch progress.Kind {
	case RecoveryRabbitRepair:
		repairHighWater, valueErr := redisArrayString(values, 23)
		if valueErr != nil {
			return RecoveryControlSnapshot{}, valueErr
		}
		repairComplete, valueErr := redisArrayBool(values, 24)
		if valueErr != nil {
			return RecoveryControlSnapshot{}, valueErr
		}
		progress.RabbitRepair = &RabbitRepairProgress{
			RepairHighWater: repairHighWater, RepairPassComplete: repairComplete,
		}
	case RecoveryWriterRebind:
		original, valueErr := redisArrayString(values, 25)
		if valueErr != nil {
			return RecoveryControlSnapshot{}, valueErr
		}
		target, valueErr := redisArrayString(values, 26)
		if valueErr != nil {
			return RecoveryControlSnapshot{}, valueErr
		}
		progress.WriterRebind = &WriterRebindProgress{
			OriginalWriterFingerprint: original, TargetWriterFingerprint: target,
		}
	case RecoveryForceRebuild:
		notBefore, valueErr := redisArrayInt64(values, 27, false)
		if valueErr != nil {
			return RecoveryControlSnapshot{}, valueErr
		}
		deleteComplete, valueErr := redisArrayBool(values, 28)
		if valueErr != nil {
			return RecoveryControlSnapshot{}, valueErr
		}
		progress.ForceRebuild = &ForceRebuildProgress{
			NotBeforeUnixMS: notBefore, DeletePassComplete: deleteComplete,
		}
	}
	snapshot.Progress = &progress
	if err := snapshot.Validate(); err != nil {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: invalid Redis recovery snapshot", ErrCoordinationCorrupt)
	}
	return snapshot, nil
}

func (r *Redis) InspectRecoveryStart(
	ctx context.Context,
	resource string,
	lock RecoveryLock,
) (RecoveryControlSnapshot, error) {
	if err := ValidateResource(resource); err != nil {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := lock.Validate(); err != nil {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "inspect recovery start", redisInspectRecoveryScript,
		[]string{keys.control, keys.recoveryLock, keys.progress}, lock.OwnerToken, strconv.Itoa(ProtocolVersion))
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	return parseRecoverySnapshot(values)
}

func (r *Redis) ComputeForceRebuildDeadlineWithLock(
	ctx context.Context,
	resource string,
	lock RecoveryLock,
	minimumDelay time.Duration,
) (ForceRebuildDeadline, error) {
	if err := ValidateResource(resource); err != nil {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := lock.Validate(); err != nil {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	if minimumDelay <= 0 || minimumDelay > maxResourceDuration {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: invalid force rebuild delay", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return ForceRebuildDeadline{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return ForceRebuildDeadline{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "compute force rebuild deadline", redisForceDeadlineScript,
		[]string{keys.recoveryLock}, lock.OwnerToken)
	if err != nil {
		return ForceRebuildDeadline{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return ForceRebuildDeadline{}, err
	}
	code, err := redisArrayString(values, 0)
	if err != nil {
		return ForceRebuildDeadline{}, err
	}
	if code != redisResultOK {
		return ForceRebuildDeadline{}, redisScriptError(code)
	}
	if len(values) != 3 {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: malformed Redis TIME response", ErrCoordinationCorrupt)
	}
	seconds, err := redisArrayInt64(values, 1, false)
	if err != nil {
		return ForceRebuildDeadline{}, err
	}
	if seconds <= 0 {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: malformed Redis TIME response", ErrCoordinationCorrupt)
	}
	microseconds, err := redisArrayInt64(values, 2, false)
	if err != nil || microseconds < 0 || microseconds >= int64(time.Second/time.Microsecond) {
		return ForceRebuildDeadline{}, fmt.Errorf("%w: malformed Redis TIME response", ErrCoordinationCorrupt)
	}
	return NewForceRebuildDeadline(time.Unix(seconds, microseconds*int64(time.Microsecond)).UTC(), minimumDelay)
}

func (r *Redis) nextEpoch() (string, error) {
	epoch, err := r.tokens.Next()
	if err != nil {
		return "", redisDependencyError("generate recovery epoch", err)
	}
	if !lowerHex32Pattern.MatchString(epoch) {
		return "", fmt.Errorf("%w: invalid recovery epoch source", ErrCoordinationCorrupt)
	}
	return epoch, nil
}

func (r *Redis) beginRecoveryWithLock(
	ctx context.Context,
	resource string,
	writerA, writerB string,
	kind RecoveryKind,
	operationID string,
	forceNotBeforeUnixMS int64,
	lock RecoveryLock,
	ttl time.Duration,
) (RecoveryFence, error) {
	if err := ValidateResource(resource); err != nil {
		return RecoveryFence{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := validateRedisWriter(writerA); err != nil {
		return RecoveryFence{}, err
	}
	if kind == RecoveryWriterRebind {
		if err := validateRedisWriter(writerB); err != nil {
			return RecoveryFence{}, err
		}
		if writerA == writerB {
			return RecoveryFence{}, fmt.Errorf("%w: recovery writers must differ", ErrInvalidModel)
		}
	} else if writerB != "" {
		return RecoveryFence{}, fmt.Errorf("%w: unexpected recovery writer", ErrInvalidModel)
	}
	if kind == RecoveryNormal {
		if operationID != "" || forceNotBeforeUnixMS != 0 {
			return RecoveryFence{}, fmt.Errorf("%w: invalid normal recovery identity", ErrInvalidModel)
		}
	} else {
		if err := validateRedisOperationID(operationID); err != nil {
			return RecoveryFence{}, err
		}
		if kind == RecoveryForceRebuild {
			if forceNotBeforeUnixMS <= 0 {
				return RecoveryFence{}, fmt.Errorf("%w: invalid force rebuild deadline", ErrInvalidModel)
			}
		} else if forceNotBeforeUnixMS != 0 {
			return RecoveryFence{}, fmt.Errorf("%w: unexpected force rebuild deadline", ErrInvalidModel)
		}
	}
	if kind != RecoveryNormal && !kind.special() {
		return RecoveryFence{}, fmt.Errorf("%w: invalid recovery kind", ErrInvalidModel)
	}
	if err := lock.Validate(); err != nil {
		return RecoveryFence{}, fmt.Errorf("%w: invalid recovery owner", ErrInvalidModel)
	}
	ttlMilliseconds, err := redisDurationMilliseconds(ttl)
	if err != nil {
		return RecoveryFence{}, err
	}
	epoch, err := r.nextEpoch()
	if err != nil {
		return RecoveryFence{}, err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryFence{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryFence{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "begin recovery", redisBeginRecoveryScript,
		[]string{keys.control, keys.recoveryLock, keys.progress},
		lock.OwnerToken, epoch, ttlMilliseconds, string(kind), operationID,
		writerA, writerB, forceNotBeforeUnixMS, strconv.Itoa(ProtocolVersion))
	if err != nil {
		return RecoveryFence{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return RecoveryFence{}, err
	}
	code, err := redisArrayString(values, 0)
	if err != nil {
		return RecoveryFence{}, err
	}
	if code != redisResultOK {
		return RecoveryFence{}, redisScriptError(code)
	}
	if len(values) != 3 {
		return RecoveryFence{}, fmt.Errorf("%w: malformed begin-recovery response", ErrCoordinationCorrupt)
	}
	returnedEpoch, err := redisArrayString(values, 1)
	if err != nil || returnedEpoch != epoch {
		return RecoveryFence{}, fmt.Errorf("%w: begin-recovery epoch mismatch", ErrCoordinationCorrupt)
	}
	writer, err := redisArrayString(values, 2)
	if err != nil {
		return RecoveryFence{}, err
	}
	fence := RecoveryFence{
		ResourceFence: ResourceFence{Epoch: epoch, WriterFingerprint: writer},
		OwnerToken:    lock.OwnerToken,
		Kind:          kind,
		OperationID:   operationID,
	}
	if err := fence.Validate(); err != nil {
		return RecoveryFence{}, fmt.Errorf("%w: invalid begin-recovery fence", ErrCoordinationCorrupt)
	}
	return fence, nil
}

func (r *Redis) BeginRecoveryWithLock(
	ctx context.Context,
	resource, writer string,
	lock RecoveryLock,
	ttl time.Duration,
) (RecoveryFence, error) {
	return r.beginRecoveryWithLock(ctx, resource, writer, "", RecoveryNormal, "", 0, lock, ttl)
}

func (r *Redis) BeginRabbitRepairWithLock(
	ctx context.Context,
	resource, writer, operationID string,
	lock RecoveryLock,
	ttl time.Duration,
) (RecoveryFence, error) {
	return r.beginRecoveryWithLock(ctx, resource, writer, "", RecoveryRabbitRepair, operationID, 0, lock, ttl)
}

func (r *Redis) BeginWriterRebindWithLock(
	ctx context.Context,
	resource, originalWriter, targetWriter, operationID string,
	lock RecoveryLock,
	ttl time.Duration,
) (RecoveryFence, error) {
	return r.beginRecoveryWithLock(ctx, resource, originalWriter, targetWriter,
		RecoveryWriterRebind, operationID, 0, lock, ttl)
}

func (r *Redis) BeginForceRebuildWithLock(
	ctx context.Context,
	resource, writer, operationID string,
	notBeforeUnixMS int64,
	lock RecoveryLock,
	ttl time.Duration,
) (RecoveryFence, error) {
	return r.beginRecoveryWithLock(ctx, resource, writer, "", RecoveryForceRebuild,
		operationID, notBeforeUnixMS, lock, ttl)
}

func validateRecoveryCall(resource string, fence RecoveryFence) error {
	if err := ValidateResource(resource); err != nil {
		return fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	if err := fence.Validate(); err != nil {
		return fmt.Errorf("%w: invalid recovery fence", ErrInvalidModel)
	}
	return nil
}

func (r *Redis) runRecoveryMutation(
	ctx context.Context,
	operation, resource string,
	fence RecoveryFence,
	script *redis.Script,
	keys []string,
	extra ...any,
) error {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	result, err := r.runScript(opCtx, operation, script, keys,
		recoveryScriptArguments(fence, extra...)...)
	if err != nil {
		return err
	}
	return redisExpectOK(result)
}

func (r *Redis) RenewRecovery(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	ttl time.Duration,
) error {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return err
	}
	ttlMilliseconds, err := redisDurationMilliseconds(ttl)
	if err != nil {
		return err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "renew recovery", resource, fence,
		redisRenewRecoveryScript, []string{keys.control, keys.recoveryLock, keys.progress}, ttlMilliseconds)
}

func (r *Redis) RecoveryReapExpired(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	limit int,
) (RecoveryCleanupResult, error) {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return RecoveryCleanupResult{}, err
	}
	if err := ValidatePageLimit(limit); err != nil {
		return RecoveryCleanupResult{}, fmt.Errorf("%w: invalid recovery cleanup limit", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "reap expired recovery state", redisRecoveryReapExpiredScript,
		[]string{
			keys.control, keys.recoveryLock, keys.progress,
			keys.ring, keys.ringMembers, keys.activeUsers,
			keys.processingTurns, keys.turnToTenant, keys.turnToGeneration,
			keys.tenantToTurn, keys.provisional, keys.globalInflight,
			keys.reservationMetadata, keys.activationGeneration, keys.stableIndex,
			keys.knownUsers,
		}, recoveryScriptArguments(fence, limit, keys.root+"inflight:tenant:", resource)...)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	code, err := redisArrayString(values, 0)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	if code != redisResultOK {
		return RecoveryCleanupResult{}, redisScriptError(code)
	}
	if len(values) != 5 {
		return RecoveryCleanupResult{}, fmt.Errorf("%w: malformed recovery cleanup response", ErrCoordinationCorrupt)
	}
	removedProvisionals, err := redisArrayInt64(values, 1, false)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	removedTurns, err := redisArrayInt64(values, 2, false)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	remainingProvisionals, err := redisArrayInt64(values, 3, false)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	remainingTurns, err := redisArrayInt64(values, 4, false)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	cleanup := RecoveryCleanupResult{
		RemovedProvisionals:   removedProvisionals,
		RemovedTurns:          removedTurns,
		RemainingProvisionals: remainingProvisionals,
		RemainingTurns:        remainingTurns,
	}
	if err := cleanup.Validate(); err != nil {
		return RecoveryCleanupResult{}, fmt.Errorf("%w: invalid recovery cleanup response", ErrCoordinationCorrupt)
	}
	return cleanup, nil
}

func (r *Redis) ResetResource(ctx context.Context, resource string, fence RecoveryFence) error {
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "reset recovery resource", resource, fence,
		redisResetResourceScript, []string{
			keys.control, keys.recoveryLock, keys.progress,
			keys.ring, keys.ringMembers, keys.activeUsers, keys.knownUsers,
			keys.activationGeneration, keys.processingTurns, keys.turnToTenant,
			keys.turnToGeneration, keys.tenantToTurn, keys.globalInflight,
			keys.stableIndex, keys.reservationMetadata, keys.provisional,
		})
}

func (r *Redis) SetRecoveryHighWater(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	highWater string,
) error {
	if err := ValidateHighWater(highWater); err != nil {
		return fmt.Errorf("%w: invalid recovery high water", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "set recovery high water", resource, fence,
		redisSetHighWaterScript, []string{keys.control, keys.recoveryLock, keys.progress}, highWater)
}

func (r *Redis) MarkRecoveryPass(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	pass RecoveryPass,
	cycle uint64,
	complete bool,
	diffCount int64,
) error {
	if err := pass.Validate(); err != nil || cycle == 0 || diffCount < 0 {
		return fmt.Errorf("%w: invalid recovery pass update", ErrInvalidModel)
	}
	prefix := ""
	switch pass {
	case RecoveryPassKnownTenants:
		prefix = "known"
	case RecoveryPassDispatched:
		prefix = "dispatched"
	case RecoveryPassRunning:
		prefix = "running"
	}
	completeValue := "0"
	if complete {
		completeValue = "1"
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "mark recovery pass", resource, fence,
		redisMarkRecoveryPassScript, []string{keys.control, keys.recoveryLock, keys.progress},
		prefix, cycle, completeValue, diffCount)
}

func (r *Redis) SetRabbitRepairHighWater(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	highWater string,
) error {
	if err := ValidateHighWater(highWater); err != nil {
		return fmt.Errorf("%w: invalid Rabbit repair high water", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "set Rabbit repair high water", resource, fence,
		redisSetRabbitHighWaterScript, []string{keys.control, keys.recoveryLock, keys.progress}, highWater)
}

func (r *Redis) MarkRabbitRepairPassComplete(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
) error {
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "complete Rabbit repair pass", resource, fence,
		redisMarkRabbitCompleteScript, []string{keys.control, keys.recoveryLock, keys.progress})
}

func (r *Redis) MarkForceDeletePassComplete(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
) error {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return err
	}
	if fence.Kind != RecoveryForceRebuild {
		return ErrFenceMismatch
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	check, err := r.runScript(opCtx, "fence force delete pass", redisCheckRecoveryScript,
		[]string{keys.control, keys.recoveryLock, keys.progress}, recoveryScriptArguments(fence)...)
	if err != nil {
		return err
	}
	if err := redisExpectOK(check); err != nil {
		return err
	}
	if err := r.ensureNoOwnedResourceKeys(opCtx, keys); err != nil {
		return err
	}
	result, err := r.runScript(opCtx, "complete force delete pass", redisMarkForceDeleteCompleteScript,
		[]string{keys.control, keys.recoveryLock, keys.progress}, recoveryScriptArguments(fence)...)
	if err != nil {
		return err
	}
	return redisExpectOK(result)
}

func redisOwnedKey(keys redisKeyspace, key string) bool {
	return strings.HasPrefix(key, keys.root) && key != keys.control &&
		key != keys.recoveryLock && key != keys.progress
}

func (r *Redis) ensureNoOwnedResourceKeys(ctx context.Context, keys redisKeyspace) error {
	var cursor uint64
	for {
		found, next, err := r.client.Scan(ctx, cursor, keys.root+"*", 100).Result()
		if err != nil {
			return redisDependencyError("scan recovery keys", err)
		}
		for _, key := range found {
			if redisOwnedKey(keys, key) {
				return ErrResourceNotReady
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (r *Redis) RestoreKnownTenant(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	tenant string,
) error {
	if err := ValidateTenantID(tenant); err != nil {
		return fmt.Errorf("%w: invalid recovery tenant", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "restore known tenant", resource, fence,
		redisRestoreKnownScript,
		[]string{keys.control, keys.recoveryLock, keys.progress, keys.knownUsers}, tenant)
}

func (r *Redis) RestoreActiveTenant(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	tenant string,
) error {
	if err := ValidateTenantID(tenant); err != nil {
		return fmt.Errorf("%w: invalid recovery tenant", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "restore active tenant", resource, fence,
		redisRestoreActiveScript, []string{
			keys.control, keys.recoveryLock, keys.progress, keys.knownUsers,
			keys.activeUsers, keys.activationGeneration, keys.ring,
			keys.ringMembers, keys.tenantToTurn,
		}, tenant)
}

func (r *Redis) RestoreInflight(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	tenant, stableToken string,
	ttl time.Duration,
) error {
	if err := ValidateTenantID(tenant); err != nil || !stableReservationPattern.MatchString(stableToken) {
		return fmt.Errorf("%w: invalid recovery reservation", ErrInvalidModel)
	}
	ttlMilliseconds, err := redisDurationMilliseconds(ttl)
	if err != nil {
		return err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	tenantKey, err := keys.tenantInflight(resource, tenant)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "restore stable reservation", resource, fence,
		redisRestoreInflightScript,
		[]string{
			keys.control, keys.recoveryLock, keys.progress, tenantKey,
			keys.globalInflight, keys.stableIndex, keys.reservationMetadata,
			keys.provisional,
		}, tenant, stableToken, ttlMilliseconds)
}

func (r *Redis) ListRecoveryStableInflight(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	cursor string,
	limit int,
) (RecoveryPage[ReservationRef], error) {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	if err := ValidateCursor(cursor); err != nil {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid reservation cursor", ErrInvalidModel)
	}
	if cursor != "" && !stableReservationPattern.MatchString(cursor) {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid reservation cursor", ErrInvalidModel)
	}
	if err := ValidatePageLimit(limit); err != nil {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid reservation page limit", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "list recovery stable reservations", redisListRecoveryStableScript,
		[]string{
			keys.control, keys.recoveryLock, keys.progress,
			keys.stableIndex, keys.globalInflight, keys.reservationMetadata,
			keys.provisional,
		}, recoveryScriptArguments(fence, cursor, limit, keys.root+"inflight:tenant:")...)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	code, err := redisArrayString(values, 0)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	if code != redisResultOK {
		return RecoveryPage[ReservationRef]{}, redisScriptError(code)
	}
	if len(values) < 2 || (len(values)-2)%4 != 0 {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: malformed reservation page", ErrCoordinationCorrupt)
	}
	hasMore, err := redisArrayString(values, 1)
	if err != nil || (hasMore != "0" && hasMore != "1") {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: malformed reservation page", ErrCoordinationCorrupt)
	}
	items := make([]ReservationRef, 0, (len(values)-2)/4)
	for index := 2; index < len(values); index += 4 {
		token, itemErr := redisArrayString(values, index)
		if itemErr != nil {
			return RecoveryPage[ReservationRef]{}, itemErr
		}
		tenant, itemErr := redisArrayString(values, index+1)
		if itemErr != nil {
			return RecoveryPage[ReservationRef]{}, itemErr
		}
		tenantKey, itemErr := redisArrayString(values, index+2)
		if itemErr != nil {
			return RecoveryPage[ReservationRef]{}, itemErr
		}
		expectedTenantKey, itemErr := keys.tenantInflight(resource, tenant)
		if itemErr != nil || tenantKey != expectedTenantKey {
			return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: reservation tenant key mismatch", ErrCoordinationCorrupt)
		}
		expiresAt, itemErr := redisArrayInt64(values, index+3, false)
		if itemErr != nil {
			return RecoveryPage[ReservationRef]{}, itemErr
		}
		item := ReservationRef{TenantID: tenant, StableToken: token, ExpiresAtUnixMS: expiresAt}
		if itemErr = item.Validate(); itemErr != nil {
			return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid reservation page item", ErrCoordinationCorrupt)
		}
		items = append(items, item)
	}
	page := RecoveryPage[ReservationRef]{Items: items, Done: hasMore == "0"}
	if hasMore == "1" {
		if len(items) == 0 {
			return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: non-advancing reservation page", ErrCoordinationCorrupt)
		}
		page.NextCursor = items[len(items)-1].StableToken
	}
	if err := page.Validate(cursor, limit); err != nil {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid reservation page", ErrCoordinationCorrupt)
	}
	return page, nil
}

func (r *Redis) DeleteRecoveryStableInflight(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	ref ReservationRef,
) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%w: invalid recovery reservation", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	tenantKey, err := keys.tenantInflight(resource, ref.TenantID)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "delete recovery stable reservation", resource, fence,
		redisDeleteRecoveryStableScript,
		[]string{
			keys.control, keys.recoveryLock, keys.progress,
			keys.stableIndex, keys.globalInflight, keys.reservationMetadata,
			keys.provisional, tenantKey,
		}, ref.TenantID, ref.StableToken, ref.ExpiresAtUnixMS)
}

func parseRedisScanCursor(cursor string) (uint64, string, error) {
	if cursor == "" {
		return 0, "a", nil
	}
	parts := strings.SplitN(cursor, ":", 2)
	parsed, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%w: invalid owned-key cursor", ErrInvalidModel)
	}
	toggle := "a"
	if len(parts) == 2 {
		if parts[1] != "a" && parts[1] != "b" {
			return 0, "", fmt.Errorf("%w: invalid owned-key cursor", ErrInvalidModel)
		}
		toggle = parts[1]
	}
	return parsed, toggle, nil
}

func (r *Redis) ListOwnedResourceKeys(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	cursor string,
	limit int,
) (RecoveryPage[RedisKeyRef], error) {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return RecoveryPage[RedisKeyRef]{}, err
	}
	if err := ValidateCursor(cursor); err != nil {
		return RecoveryPage[RedisKeyRef]{}, fmt.Errorf("%w: invalid owned-key cursor", ErrInvalidModel)
	}
	if err := ValidatePageLimit(limit); err != nil {
		return RecoveryPage[RedisKeyRef]{}, fmt.Errorf("%w: invalid owned-key page limit", ErrInvalidModel)
	}
	scanCursor, toggle, err := parseRedisScanCursor(cursor)
	if err != nil {
		return RecoveryPage[RedisKeyRef]{}, err
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryPage[RedisKeyRef]{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryPage[RedisKeyRef]{}, err
	}
	defer cancel()
	// Fence immediately before the read-only SCAN. Delete remains separately
	// atomic and fenced, so expiration between list and delete cannot mutate.
	check, err := r.runScript(opCtx, "fence owned-key scan", redisCheckRecoveryScript,
		[]string{keys.control, keys.recoveryLock, keys.progress},
		recoveryScriptArguments(fence)...)
	if err != nil {
		return RecoveryPage[RedisKeyRef]{}, err
	}
	if err := redisExpectOK(check); err != nil {
		return RecoveryPage[RedisKeyRef]{}, err
	}
	found, next, err := r.client.Scan(opCtx, scanCursor, keys.root+"*", int64(limit*redisScanCountMultiplier)).Result()
	if err != nil {
		return RecoveryPage[RedisKeyRef]{}, redisDependencyError("scan recovery keys", err)
	}
	items := make([]RedisKeyRef, 0, min(limit, len(found)))
	truncated := false
	for _, key := range found {
		if !redisOwnedKey(keys, key) {
			continue
		}
		if len(items) == limit {
			truncated = true
			continue
		}
		item := RedisKeyRef{Key: key}
		if err := item.Validate(); err != nil {
			return RecoveryPage[RedisKeyRef]{}, fmt.Errorf("%w: invalid owned Redis key", ErrCoordinationCorrupt)
		}
		items = append(items, item)
	}
	page := RecoveryPage[RedisKeyRef]{Items: items, Done: next == 0 && !truncated}
	if !page.Done {
		if truncated {
			// Redis COUNT is a hint and one SCAN bucket may exceed the page
			// limit. Revisit the same input cursor after the caller deletes
			// this page, rather than advancing past discarded overflow.
			next = scanCursor
		}
		nextToggle := "a"
		if toggle == "a" {
			nextToggle = "b"
		}
		page.NextCursor = strconv.FormatUint(next, 10) + ":" + nextToggle
		if page.NextCursor == cursor {
			page.NextCursor = strconv.FormatUint(next, 10) + ":" + toggle
		}
	}
	if err := page.Validate(cursor, limit); err != nil {
		return RecoveryPage[RedisKeyRef]{}, fmt.Errorf("%w: invalid owned-key page", ErrCoordinationCorrupt)
	}
	return page, nil
}

func (r *Redis) DeleteOwnedResourceKeys(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	refs []RedisKeyRef,
) error {
	if err := validateRecoveryCall(resource, fence); err != nil {
		return err
	}
	if len(refs) > maxRecoveryPageSize {
		return fmt.Errorf("%w: too many owned keys", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(refs))
	deleteKeys := make([]string, 0, len(refs))
	for _, ref := range refs {
		if err := ref.Validate(); err != nil || !redisOwnedKey(keys, ref.Key) {
			return fmt.Errorf("%w: key is outside the rebuildable resource namespace", ErrInvalidModel)
		}
		if _, duplicate := seen[ref.Key]; duplicate {
			continue
		}
		seen[ref.Key] = struct{}{}
		deleteKeys = append(deleteKeys, ref.Key)
	}
	scriptKeys := []string{keys.control, keys.recoveryLock, keys.progress}
	scriptKeys = append(scriptKeys, deleteKeys...)
	extra := []any{keys.root, keys.control, keys.recoveryLock, keys.progress}
	return r.runRecoveryMutation(ctx, "delete owned recovery keys", resource, fence,
		redisDeleteOwnedKeysScript, scriptKeys, extra...)
}

func (r *Redis) FinishRecovery(ctx context.Context, resource string, fence RecoveryFence) error {
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return err
	}
	return r.runRecoveryMutation(ctx, "finish recovery", resource, fence,
		redisFinishRecoveryScript,
		[]string{
			keys.control, keys.recoveryLock, keys.progress,
			keys.provisional, keys.processingTurns, keys.ring,
			keys.ringMembers, keys.activeUsers, keys.knownUsers,
			keys.turnToTenant, keys.turnToGeneration, keys.tenantToTurn,
			keys.activationGeneration,
		})
}
