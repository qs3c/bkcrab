package fairqueue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	redis "github.com/redis/go-redis/v9"
)

const (
	redisRuntimeReservationCleanupBatch = 128
	redisRuntimeTurnScanBatch           = 128
)

const redisRuntimeLuaCommon = `
local NOT_READY = "FQ_NOT_READY"
local FENCE = "FQ_FENCE_MISMATCH"
local CORRUPT = "FQ_COORDINATION_CORRUPT"
local INVALID = "FQ_INVALID_STATE"

local function key_type(key)
  local result = redis.call("TYPE", key)
  if type(result) == "table" then
    return result["ok"]
  end
  return result
end

local function type_is(key, expected)
  local actual = key_type(key)
  return actual == "none" or actual == expected
end

local function lower_hex(value, count)
  return type(value) == "string" and string.len(value) == count and
    string.match(value, "^[0-9a-f]+$") ~= nil
end

local function positive_decimal(value)
  return type(value) == "string" and string.match(value, "^[1-9][0-9]*$") ~= nil
end

local function positive_int64(value)
  if not positive_decimal(value) or string.len(value) > 19 then return false end
  return string.len(value) < 19 or value <= "9223372036854775807"
end

local function redis_now_ms()
  local now = redis.call("TIME")
  return tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
end

local function canonical_control(values)
	  for index = 1, 8 do
    if values[index] == false then return false end
  end
  if not lower_hex(values[4], 32) or not positive_int64(values[5]) or
    not lower_hex(values[6], 64) or
	    (values[7] == "" and values[8] ~= "NONE") or
	    (values[7] ~= "" and (not lower_hex(values[7], 32) or
	      (values[8] ~= "RABBIT_REPAIR" and values[8] ~= "WRITER_REBIND" and
	        values[8] ~= "FORCE_REBUILD"))) then
    return false
  end
  if values[1] == "READY" then
    return values[2] == "NONE" and values[3] == ""
  end
  if values[1] ~= "RECOVERING" then return false end
  if values[2] == "NORMAL" then return values[3] == "" end
  if values[2] ~= "RABBIT_REPAIR" and values[2] ~= "WRITER_REBIND" and
    values[2] ~= "FORCE_REBUILD" then
    return false
  end
  return lower_hex(values[3], 32)
end

local function check_ready(control, progress, epoch, writer)
  local actual_type = key_type(control)
  if actual_type == "none" then
    return NOT_READY
  end
  if actual_type ~= "hash" then
    return CORRUPT
  end
  local values = redis.call("HMGET", control,
    "state", "operation_kind", "operation_id", "epoch",
	    "protocol_version", "writer_fingerprint", "last_completed_operation_id",
	    "last_completed_operation_kind")
  if not canonical_control(values) then return CORRUPT end
  if values[1] == "RECOVERING" then
    return NOT_READY
  end
  if key_type(progress) ~= "none" then return CORRUPT end
  if values[5] ~= "1" or values[4] ~= epoch or values[6] ~= writer then
    return FENCE
  end
  return "OK"
end

local sha256_constants = {
  0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
  0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
  0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
  0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
  0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
  0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
  0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
  0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2
}

local function add32(...)
  local value = 0
  for index = 1, select("#", ...) do value = value + select(index, ...) end
  return bit.tobit(value)
end

local function unsigned32(value)
  if value < 0 then return value + 4294967296 end
  return value
end

local function sha256(message)
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
      words[index] = add32(words[index - 16], sigma0, words[index - 7], sigma1)
    end

    local a,b,c,d,e,f,g,h = hashes[1],hashes[2],hashes[3],hashes[4],hashes[5],hashes[6],hashes[7],hashes[8]
    for index = 1, 64 do
      local choice = bit.bxor(bit.band(e, f), bit.band(bit.bnot(e), g))
      local majority = bit.bxor(bit.band(a, b), bit.band(a, c), bit.band(b, c))
      local sum0 = bit.bxor(bit.ror(a, 2), bit.ror(a, 13), bit.ror(a, 22))
      local sum1 = bit.bxor(bit.ror(e, 6), bit.ror(e, 11), bit.ror(e, 25))
      local first = add32(h, sum1, choice, sha256_constants[index], words[index])
      local second = add32(sum0, majority)
      h,g,f,e,d,c,b,a = g,f,e,add32(d, first),c,b,a,add32(first, second)
    end
    hashes[1],hashes[2],hashes[3],hashes[4] = add32(hashes[1],a),add32(hashes[2],b),add32(hashes[3],c),add32(hashes[4],d)
    hashes[5],hashes[6],hashes[7],hashes[8] = add32(hashes[5],e),add32(hashes[6],f),add32(hashes[7],g),add32(hashes[8],h)
  end
  local encoded = ""
  for index = 1, 8 do encoded = encoded .. string.format("%08x", unsigned32(hashes[index])) end
  return encoded
end

local function valid_tenant_key(key, prefix, resource, tenant)
  if type(key) ~= "string" or string.len(key) ~= string.len(prefix) + 64 then
    return false
  end
  if string.sub(key, 1, string.len(prefix)) ~= prefix then
    return false
  end
  local suffix = string.sub(key, string.len(prefix) + 1)
  return lower_hex(suffix, 64) and suffix == sha256(resource .. string.char(0) .. tenant)
end

local function parse_meta(raw, expected_kind, tenant_prefix, resource)
  if type(raw) ~= "string" then
    return nil
  end
  local ok, value = pcall(cjson.decode, raw)
  if not ok or type(value) ~= "table" or type(value["tenant"]) ~= "string" or
    value["tenant"] == "" or type(value["tenant_key"]) ~= "string" or
    value["kind"] ~= expected_kind or
    not valid_tenant_key(value["tenant_key"], tenant_prefix, resource, value["tenant"]) then
    return nil
  end
  local fields = 0
  for name, _ in pairs(value) do
    fields = fields + 1
    if name ~= "tenant" and name ~= "tenant_key" and name ~= "kind" and name ~= "decision" then
      return nil
    end
  end
  if expected_kind == "P" then
    if fields ~= 4 or (value["decision"] ~= "regular" and value["decision"] ~= "borrowed") then
      return nil
    end
  elseif fields ~= 3 or value["decision"] ~= nil then
    return nil
  end
  return value
end

local function same_score(left, right)
  return left ~= false and right ~= false and tostring(left) == tostring(right)
end

local function reservation_shape_count(global_key, provisional_key, stable_key, metadata_key)
  local global_count = redis.call("ZCARD", global_key)
  local provisional_count = redis.call("ZCARD", provisional_key)
  local stable_count = redis.call("ZCARD", stable_key)
  if global_count ~= provisional_count + stable_count or
    redis.call("HLEN", metadata_key) ~= global_count or
    redis.call("ZCOUNT", stable_key, 0, 0) ~= stable_count then
    return nil
  end
  return global_count
end

local function reservation_closure(global_key, provisional_key, stable_key, metadata_key,
    tenant_prefix, resource, now_ms, current_tenant_key, maximum_count)
  local global_count = reservation_shape_count(global_key, provisional_key, stable_key, metadata_key)
  if global_count == nil or global_count > maximum_count then return nil end
  local global_tokens = redis.call("ZRANGE", global_key, 0, maximum_count)
  if #global_tokens ~= global_count then return nil end

  local tenant_counts = {}
  local live_tenant_counts = {}
  local live_global_count = 0
  for _, token in ipairs(global_tokens) do
    local global_score = redis.call("ZSCORE", global_key, token)
    local provisional_score = redis.call("ZSCORE", provisional_key, token)
    local stable_score = redis.call("ZSCORE", stable_key, token)
    local raw = redis.call("HGET", metadata_key, token)
    local metadata = nil
    if string.sub(token, 1, 2) == "p:" and lower_hex(string.sub(token, 3), 32) then
      metadata = parse_meta(raw, "P", tenant_prefix, resource)
      if not same_score(global_score, provisional_score) or stable_score ~= false then return nil end
    elseif string.sub(token, 1, 2) == "r:" and lower_hex(string.sub(token, 3), 64) then
      metadata = parse_meta(raw, "S", tenant_prefix, resource)
      if provisional_score ~= false or stable_score == false or tonumber(stable_score) ~= 0 then return nil end
    else
      return nil
    end
    if metadata == nil or key_type(metadata["tenant_key"]) ~= "zset" or
      not same_score(global_score, redis.call("ZSCORE", metadata["tenant_key"], token)) then
      return nil
    end
    local tenant_key = metadata["tenant_key"]
    tenant_counts[tenant_key] = (tenant_counts[tenant_key] or 0) + 1
    if tonumber(global_score) > now_ms then
      live_global_count = live_global_count + 1
      live_tenant_counts[tenant_key] = (live_tenant_counts[tenant_key] or 0) + 1
    end
  end
  for tenant_key, count in pairs(tenant_counts) do
    if redis.call("ZCARD", tenant_key) ~= count then return nil end
  end
  if current_tenant_key ~= nil and redis.call("ZCARD", current_tenant_key) ~=
      (tenant_counts[current_tenant_key] or 0) then
    return nil
  end
  local current_live_count = 0
  if current_tenant_key ~= nil then current_live_count = live_tenant_counts[current_tenant_key] or 0 end
  return {live_global_count, current_live_count}
end
`

const redisObserveReadyScript = redisRuntimeLuaCommon + `
local actual_type = key_type(KEYS[1])
if actual_type == "none" then
  return {NOT_READY}
end
if actual_type ~= "hash" then
  return {CORRUPT}
end
local values = redis.call("HMGET", KEYS[1],
  "state", "operation_kind", "operation_id", "epoch",
	  "protocol_version", "writer_fingerprint", "last_completed_operation_id",
	  "last_completed_operation_kind")
if not canonical_control(values) then return {CORRUPT} end
if values[1] == "RECOVERING" then
  return {NOT_READY}
end
if key_type(KEYS[2]) ~= "none" then return {CORRUPT} end
if values[5] ~= "1" or values[6] ~= ARGV[1] then
  return {FENCE}
end
return {"OK", values[4], values[6]}
`

const redisCheckReadyScript = redisRuntimeLuaCommon + `
return {check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])}
`

const redisEnsureKnownScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
if not type_is(KEYS[2], "set") then return {CORRUPT} end
redis.call("SADD", KEYS[2], ARGV[3])
return {"OK"}
`

const redisActivateScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
if not type_is(KEYS[2], "list") or not type_is(KEYS[3], "set") or
  not type_is(KEYS[4], "set") or not type_is(KEYS[5], "set") or
  not type_is(KEYS[6], "hash") or not type_is(KEYS[7], "zset") or
  not type_is(KEYS[8], "hash") or not type_is(KEYS[9], "hash") or
  not type_is(KEYS[10], "hash") then
  return {CORRUPT}
end

local tenant = ARGV[3]
local generation = redis.call("HGET", KEYS[6], tenant)
if generation ~= false and not positive_int64(generation) then return {CORRUPT} end
if generation ~= false and (string.len(generation) > 19 or
    (string.len(generation) == 19 and generation >= "9223372036854775807")) then
  return {CORRUPT}
end
local turn = redis.call("HGET", KEYS[10], tenant)
local member = redis.call("SISMEMBER", KEYS[3], tenant)
local positions = redis.call("LPOS", KEYS[2], tenant, "COUNT", 2)
if (member == 1 and #positions == 0) or (turn ~= false and member == 1) then
  return {CORRUPT}
end
if turn ~= false then
  if redis.call("ZSCORE", KEYS[7], turn) == false or
    redis.call("HGET", KEYS[8], turn) ~= tenant or
    redis.call("HGET", KEYS[9], turn) == false then
    return {CORRUPT}
  end
end

redis.call("SADD", KEYS[5], tenant)
redis.call("HINCRBY", KEYS[6], tenant, 1)
redis.call("SADD", KEYS[4], tenant)
if turn == false and member == 0 then
  redis.call("SADD", KEYS[3], tenant)
  if #positions == 0 then redis.call("RPUSH", KEYS[2], tenant) end
end
return {"OK"}
`

const redisNextTurnScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
if not type_is(KEYS[2], "list") or not type_is(KEYS[3], "set") or
  not type_is(KEYS[4], "set") or not type_is(KEYS[5], "set") or
  not type_is(KEYS[6], "hash") or not type_is(KEYS[7], "zset") or
  not type_is(KEYS[8], "hash") or not type_is(KEYS[9], "hash") or
  not type_is(KEYS[10], "hash") then
  return {CORRUPT}
end
local token = ARGV[3]
if redis.call("ZSCORE", KEYS[7], token) ~= false or
  redis.call("HGET", KEYS[8], token) ~= false or
  redis.call("HGET", KEYS[9], token) ~= false then
  return {INVALID}
end
local list_length = redis.call("LLEN", KEYS[2])
if list_length == 0 then
  if redis.call("SCARD", KEYS[3]) ~= 0 then return {CORRUPT} end
  return {"EMPTY"}
end
local scan_count = math.min(list_length, tonumber(ARGV[5]))
local discarded = {}
local tenant = nil
local generation = nil
for offset = 0, scan_count - 1 do
  local candidate = redis.call("LINDEX", KEYS[2], offset)
  if candidate == false or redis.call("SISMEMBER", KEYS[5], candidate) ~= 1 then return {CORRUPT} end
  local member = redis.call("SISMEMBER", KEYS[3], candidate)
  local active = redis.call("SISMEMBER", KEYS[4], candidate)
  local existing_turn = redis.call("HGET", KEYS[10], candidate)
  if existing_turn ~= false then
    if member ~= 0 or active ~= 1 or redis.call("ZSCORE", KEYS[7], existing_turn) == false or
      redis.call("HGET", KEYS[8], existing_turn) ~= candidate or
      not positive_int64(redis.call("HGET", KEYS[9], existing_turn)) then
      return {CORRUPT}
    end
    table.insert(discarded, candidate)
  elseif active == 1 then
    generation = redis.call("HGET", KEYS[6], candidate)
    if not positive_int64(generation) then return {CORRUPT} end
    tenant = candidate
    break
  else
    if member ~= 0 then return {CORRUPT} end
    table.insert(discarded, candidate)
  end
end

if tenant == nil then
  if list_length <= scan_count and redis.call("SCARD", KEYS[3]) ~= 0 then return {CORRUPT} end
  redis.call("LTRIM", KEYS[2], #discarded, -1)
  return {"EMPTY"}
end
local expiry = redis_now_ms() + tonumber(ARGV[4])

redis.call("LTRIM", KEYS[2], #discarded + 1, -1)
redis.call("SREM", KEYS[3], tenant)
redis.call("ZADD", KEYS[7], expiry, token)
redis.call("HSET", KEYS[8], token, tenant)
redis.call("HSET", KEYS[9], token, generation)
redis.call("HSET", KEYS[10], tenant, token)
return {"OK", tenant, generation}
`

const redisRotateScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
if not type_is(KEYS[2], "list") or not type_is(KEYS[3], "set") or
  not type_is(KEYS[4], "set") or not type_is(KEYS[5], "hash") or
  not type_is(KEYS[6], "zset") or not type_is(KEYS[7], "hash") or
  not type_is(KEYS[8], "hash") or not type_is(KEYS[9], "hash") or
  not type_is(KEYS[10], "set") then
  return {CORRUPT}
end
local token = ARGV[3]
local tenant = redis.call("HGET", KEYS[7], token)
local turn_generation = redis.call("HGET", KEYS[8], token)
local expiry = redis.call("ZSCORE", KEYS[6], token)
if tenant == false or turn_generation == false or expiry == false then return {INVALID} end
if redis.call("HGET", KEYS[9], tenant) ~= token or turn_generation ~= ARGV[4] then
  return {INVALID}
end
if tonumber(expiry) <= redis_now_ms() then return {INVALID} end
local generation = redis.call("HGET", KEYS[5], tenant)
if not positive_int64(generation) or redis.call("SISMEMBER", KEYS[4], tenant) ~= 1 or
  redis.call("SISMEMBER", KEYS[10], tenant) ~= 1 then
  return {CORRUPT}
end
local member = redis.call("SISMEMBER", KEYS[3], tenant)
local positions = redis.call("LPOS", KEYS[2], tenant, "COUNT", 1)
if member ~= 0 then return {CORRUPT} end
local requeue = ARGV[5] == "1" or generation ~= ARGV[4]

redis.call("ZREM", KEYS[6], token)
redis.call("HDEL", KEYS[7], token)
redis.call("HDEL", KEYS[8], token)
redis.call("HDEL", KEYS[9], tenant)
if requeue then
  redis.call("SADD", KEYS[4], tenant)
  redis.call("SADD", KEYS[3], tenant)
  if #positions == 0 then redis.call("RPUSH", KEYS[2], tenant) end
else
  redis.call("SREM", KEYS[4], tenant)
end
return {"OK"}
`

const redisAcquireProvisionalScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
if not type_is(KEYS[2], "set") or not type_is(KEYS[3], "set") or
  not type_is(KEYS[4], "zset") or not type_is(KEYS[5], "zset") or
  not type_is(KEYS[6], "zset") or not type_is(KEYS[7], "zset") or
  not type_is(KEYS[8], "hash") then
  return {CORRUPT}
end
local tenant = ARGV[3]
local token = ARGV[4]
if redis.call("SISMEMBER", KEYS[2], tenant) ~= 1 or
  redis.call("SISMEMBER", KEYS[3], tenant) ~= 1 then
  return {CORRUPT}
end
local now = redis_now_ms()
local closure = reservation_closure(KEYS[4], KEYS[6], KEYS[7], KEYS[8],
  ARGV[10], ARGV[11], now, KEYS[5], tonumber(ARGV[13]))
if closure == nil then return {CORRUPT} end
local global_count = closure[1]
local tenant_count = closure[2]
local expired = redis.call("ZRANGEBYSCORE", KEYS[4], "-inf", now, "LIMIT", 0, tonumber(ARGV[12]))
if #expired > 0 then
  local expired_metadata = {}
  local expired_kinds = {}
  for index, expired_token in ipairs(expired) do
    local expired_global = redis.call("ZSCORE", KEYS[4], expired_token)
    local expired_provisional = redis.call("ZSCORE", KEYS[6], expired_token)
    local expired_stable = redis.call("ZSCORE", KEYS[7], expired_token)
    local expired_raw = redis.call("HGET", KEYS[8], expired_token)
    local expired_meta = nil
    local expired_kind = ""
    if string.sub(expired_token, 1, 2) == "p:" and lower_hex(string.sub(expired_token, 3), 32) then
      expired_kind = "P"
      expired_meta = parse_meta(expired_raw, "P", ARGV[10], ARGV[11])
      if not same_score(expired_global, expired_provisional) or expired_stable ~= false then
        return {CORRUPT}
      end
    elseif string.sub(expired_token, 1, 2) == "r:" and lower_hex(string.sub(expired_token, 3), 64) then
      expired_kind = "S"
      expired_meta = parse_meta(expired_raw, "S", ARGV[10], ARGV[11])
      if expired_provisional ~= false or expired_stable == false or tonumber(expired_stable) ~= 0 then
        return {CORRUPT}
      end
    else
      return {CORRUPT}
    end
    if expired_meta == nil or key_type(expired_meta["tenant_key"]) ~= "zset" or
      not same_score(expired_global, redis.call("ZSCORE", expired_meta["tenant_key"], expired_token)) then
      return {CORRUPT}
    end
    expired_metadata[index] = expired_meta
    expired_kinds[index] = expired_kind
  end
  for index, expired_token in ipairs(expired) do
    redis.call("ZREM", KEYS[4], expired_token)
    redis.call("ZREM", expired_metadata[index]["tenant_key"], expired_token)
    if expired_kinds[index] == "P" then
      redis.call("ZREM", KEYS[6], expired_token)
    else
      redis.call("ZREM", KEYS[7], expired_token)
    end
    redis.call("HDEL", KEYS[8], expired_token)
  end
end
local global_score = redis.call("ZSCORE", KEYS[4], token)
local tenant_score = redis.call("ZSCORE", KEYS[5], token)
local provisional_score = redis.call("ZSCORE", KEYS[6], token)
local stable_score = redis.call("ZSCORE", KEYS[7], token)
local raw = redis.call("HGET", KEYS[8], token)
local any = global_score ~= false or tenant_score ~= false or provisional_score ~= false or
  stable_score ~= false or raw ~= false
if any then
  local meta = parse_meta(raw, "P", ARGV[10], ARGV[11])
  if meta == nil then return {CORRUPT} end
  if meta["tenant"] ~= tenant or meta["tenant_key"] ~= KEYS[5] then return {INVALID} end
  if not same_score(global_score, tenant_score) or not same_score(global_score, provisional_score) or
    stable_score ~= false then
    return {CORRUPT}
  end
  if tonumber(provisional_score) <= now then return {INVALID} end
  return {meta["decision"]}
end

local active_count = redis.call("SCARD", KEYS[2])
if tenant_count > global_count or active_count < 1 then return {CORRUPT} end
local decision = ""
if global_count >= tonumber(ARGV[6]) then
  decision = "denied-global-full"
elseif tenant_count >= tonumber(ARGV[8]) then
  decision = "denied-tenant-burst"
elseif tenant_count < tonumber(ARGV[7]) then
  decision = "regular"
elseif ARGV[9] ~= "1" then
  decision = "denied-borrow-disabled"
elseif active_count == 1 then
  decision = "borrowed"
else
  decision = "denied-competing-active-tenant"
end
if decision ~= "regular" and decision ~= "borrowed" then return {decision} end

local expiry = now + tonumber(ARGV[5])
local metadata = cjson.encode({tenant=tenant, tenant_key=KEYS[5], kind="P", decision=decision})
redis.call("ZADD", KEYS[4], expiry, token)
redis.call("ZADD", KEYS[5], expiry, token)
redis.call("ZADD", KEYS[6], expiry, token)
redis.call("HSET", KEYS[8], token, metadata)
return {decision}
`

const redisBindReservationScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
for index = 2, 6 do
  local expected = "zset"
  if index == 6 then expected = "hash" end
  if not type_is(KEYS[index], expected) then return {CORRUPT} end
end
local tenant = ARGV[3]
local provisional = ARGV[4]
local stable = ARGV[5]
local prefix = ARGV[7]

local p_global = redis.call("ZSCORE", KEYS[2], provisional)
local p_tenant = redis.call("ZSCORE", KEYS[3], provisional)
local p_provisional = redis.call("ZSCORE", KEYS[4], provisional)
local p_stable = redis.call("ZSCORE", KEYS[5], provisional)
local p_raw = redis.call("HGET", KEYS[6], provisional)
local p_any = p_global ~= false or p_tenant ~= false or p_provisional ~= false or p_stable ~= false or p_raw ~= false
local p_full = same_score(p_global, p_tenant) and same_score(p_global, p_provisional) and p_stable == false and p_raw ~= false
local p_meta = nil
if p_raw ~= false then p_meta = parse_meta(p_raw, "P", prefix, ARGV[8]) end
if p_any and (not p_full or p_meta == nil) then return {CORRUPT} end
if p_meta ~= nil and (p_meta["tenant"] ~= tenant or p_meta["tenant_key"] ~= KEYS[3]) then return {INVALID} end

local s_global = redis.call("ZSCORE", KEYS[2], stable)
local s_tenant = redis.call("ZSCORE", KEYS[3], stable)
local s_provisional = redis.call("ZSCORE", KEYS[4], stable)
local s_index = redis.call("ZSCORE", KEYS[5], stable)
local s_raw = redis.call("HGET", KEYS[6], stable)
local s_any = s_global ~= false or s_tenant ~= false or s_provisional ~= false or s_index ~= false or s_raw ~= false
local s_full = same_score(s_global, s_tenant) and s_provisional == false and s_index ~= false and tonumber(s_index) == 0 and s_raw ~= false
local s_meta = nil
if s_raw ~= false then s_meta = parse_meta(s_raw, "S", prefix, ARGV[8]) end
if s_any and (not s_full or s_meta == nil) then return {CORRUPT} end
if s_meta ~= nil and (s_meta["tenant"] ~= tenant or s_meta["tenant_key"] ~= KEYS[3]) then return {INVALID} end
if p_any and s_any then return {CORRUPT} end
if not p_any and not s_any then return {INVALID} end

local now = redis_now_ms()
if (p_any and tonumber(p_global) <= now) or (s_any and tonumber(s_global) <= now) then
  return {INVALID}
end
if s_any then return {"OK"} end
local expiry = now + tonumber(ARGV[6])
redis.call("ZREM", KEYS[2], provisional)
redis.call("ZREM", KEYS[3], provisional)
redis.call("ZREM", KEYS[4], provisional)
redis.call("HDEL", KEYS[6], provisional)
redis.call("ZADD", KEYS[2], expiry, stable)
redis.call("ZADD", KEYS[3], expiry, stable)
redis.call("ZADD", KEYS[5], 0, stable)
redis.call("HSET", KEYS[6], stable, cjson.encode({tenant=tenant, tenant_key=KEYS[3], kind="S"}))
return {"OK"}
`

const redisStableWriteScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
for index = 2, 6 do
  local expected = "zset"
  if index == 6 then expected = "hash" end
  if not type_is(KEYS[index], expected) then return {CORRUPT} end
end
local tenant = ARGV[3]
local stable = ARGV[4]
local prefix = ARGV[7]
local global_score = redis.call("ZSCORE", KEYS[2], stable)
local tenant_score = redis.call("ZSCORE", KEYS[3], stable)
local provisional_score = redis.call("ZSCORE", KEYS[4], stable)
local stable_score = redis.call("ZSCORE", KEYS[5], stable)
local raw = redis.call("HGET", KEYS[6], stable)
local any = global_score ~= false or tenant_score ~= false or provisional_score ~= false or stable_score ~= false or raw ~= false
local full = same_score(global_score, tenant_score) and provisional_score == false and
  stable_score ~= false and tonumber(stable_score) == 0 and raw ~= false
local meta = nil
if raw ~= false then meta = parse_meta(raw, "S", prefix, ARGV[8]) end
if any and (not full or meta == nil) then return {CORRUPT} end
if meta ~= nil and (meta["tenant"] ~= tenant or meta["tenant_key"] ~= KEYS[3]) then return {INVALID} end
if not any and ARGV[6] ~= "1" then return {CORRUPT} end

local now = redis_now_ms()
if any and ARGV[6] ~= "1" and tonumber(global_score) <= now then return {INVALID} end
local expiry = now + tonumber(ARGV[5])
redis.call("ZADD", KEYS[2], expiry, stable)
redis.call("ZADD", KEYS[3], expiry, stable)
redis.call("ZADD", KEYS[5], 0, stable)
redis.call("HSET", KEYS[6], stable, cjson.encode({tenant=tenant, tenant_key=KEYS[3], kind="S"}))
return {"OK"}
`

const redisReleaseReservationScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
for index = 2, 6 do
  local expected = "zset"
  if index == 6 then expected = "hash" end
  if not type_is(KEYS[index], expected) then return {CORRUPT} end
end
local tenant = ARGV[3]
local token = ARGV[4]
local kind = ARGV[5]
local prefix = ARGV[6]
local global_score = redis.call("ZSCORE", KEYS[2], token)
local tenant_score = redis.call("ZSCORE", KEYS[3], token)
local provisional_score = redis.call("ZSCORE", KEYS[4], token)
local stable_score = redis.call("ZSCORE", KEYS[5], token)
local raw = redis.call("HGET", KEYS[6], token)
local any = global_score ~= false or tenant_score ~= false or provisional_score ~= false or stable_score ~= false or raw ~= false
if not any then return {"OK"} end
if raw == false then return {CORRUPT} end
local meta = parse_meta(raw, kind, prefix, ARGV[7])
if meta == nil then return {CORRUPT} end
if meta["tenant"] ~= tenant or meta["tenant_key"] ~= KEYS[3] then return {INVALID} end
if not same_score(global_score, tenant_score) then return {CORRUPT} end
if kind == "P" then
  if not same_score(global_score, provisional_score) or stable_score ~= false then return {CORRUPT} end
else
  if provisional_score ~= false or stable_score == false or tonumber(stable_score) ~= 0 then return {CORRUPT} end
end
redis.call("ZREM", KEYS[2], token)
redis.call("ZREM", KEYS[3], token)
if kind == "P" then redis.call("ZREM", KEYS[4], token) else redis.call("ZREM", KEYS[5], token) end
redis.call("HDEL", KEYS[6], token)
return {"OK"}
`

const redisListStableScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
for index = 2, 5 do
  local expected = "zset"
  if index == 5 then expected = "hash" end
  if not type_is(KEYS[index], expected) then return {CORRUPT} end
end
local prefix = ARGV[5]
if reservation_shape_count(KEYS[2], KEYS[3], KEYS[4], KEYS[5]) == nil then return {CORRUPT} end
local minimum = "-"
if ARGV[3] ~= "" then minimum = "(" .. ARGV[3] end
local requested = tonumber(ARGV[4])
local tokens = redis.call("ZRANGEBYLEX", KEYS[4], minimum, "+", "LIMIT", 0, requested + 1)
local count = #tokens
local returned = count
local done = "1"
local next_cursor = ""
if count > requested then
  returned = requested
  done = "0"
  -- Lua arrays are one-based: this is the last returned member.  The next
  -- page uses an exclusive lower bound, so no member is skipped.
  next_cursor = tokens[requested]
end
local result = {"OK", done, next_cursor, tostring(returned)}
for index = 1, returned do
  local token = tokens[index]
  if not lower_hex(string.sub(token, 3), 64) or string.sub(token, 1, 2) ~= "r:" then return {CORRUPT} end
  local global_score = redis.call("ZSCORE", KEYS[2], token)
  local provisional_score = redis.call("ZSCORE", KEYS[3], token)
  local index_score = redis.call("ZSCORE", KEYS[4], token)
  local raw = redis.call("HGET", KEYS[5], token)
  local meta = parse_meta(raw, "S", prefix, ARGV[6])
  if global_score == false or tonumber(global_score) <= 0 or provisional_score ~= false or
    index_score == false or tonumber(index_score) ~= 0 or meta == nil or
    not type_is(meta["tenant_key"], "zset") or
    not same_score(global_score, redis.call("ZSCORE", meta["tenant_key"], token)) then
    return {CORRUPT}
  end
  table.insert(result, token)
  table.insert(result, meta["tenant"])
  table.insert(result, string.format("%.0f", tonumber(global_score)))
end
return result
`

const redisReapExpiredScript = redisRuntimeLuaCommon + `
local ready = check_ready(KEYS[1], KEYS[#KEYS], ARGV[1], ARGV[2])
if ready ~= "OK" then return {ready} end
local expected_types = {"zset", "hash", "hash", "hash", "list", "set", "set", "hash", "zset", "zset", "zset", "hash"}
for index = 2, 13 do
  if not type_is(KEYS[index], expected_types[index - 1]) then return {CORRUPT} end
end
if not type_is(KEYS[14], "set") then return {CORRUPT} end
local now = redis_now_ms()
local limit = tonumber(ARGV[3])
local prefix = ARGV[4]
local provisionals = redis.call("ZRANGEBYSCORE", KEYS[11], "-inf", now, "LIMIT", 0, limit)
local remaining = limit - #provisionals
local turns = {}
if remaining > 0 then
  turns = redis.call("ZRANGEBYSCORE", KEYS[2], "-inf", now, "LIMIT", 0, remaining)
end

local provisional_meta = {}
for index, token in ipairs(provisionals) do
  if string.sub(token, 1, 2) ~= "p:" or not lower_hex(string.sub(token, 3), 32) then return {CORRUPT} end
  local score = redis.call("ZSCORE", KEYS[11], token)
  local global_score = redis.call("ZSCORE", KEYS[10], token)
  local stable_score = redis.call("ZSCORE", KEYS[12], token)
  local raw = redis.call("HGET", KEYS[13], token)
  local meta = parse_meta(raw, "P", prefix, ARGV[5])
	  if score == false or tonumber(score) > now or not same_score(score, global_score) or
	    stable_score ~= false or meta == nil or not type_is(meta["tenant_key"], "zset") or
	    not same_score(score, redis.call("ZSCORE", meta["tenant_key"], token)) then
    return {CORRUPT}
  end
  provisional_meta[index] = meta
end

local turn_tenants = {}
local turn_has_position = {}
for index, token in ipairs(turns) do
  if not lower_hex(token, 32) then return {CORRUPT} end
  local score = redis.call("ZSCORE", KEYS[2], token)
  local tenant = redis.call("HGET", KEYS[3], token)
  local generation = redis.call("HGET", KEYS[4], token)
  if score == false or tonumber(score) > now or tenant == false or not positive_int64(generation) or
    redis.call("HGET", KEYS[5], tenant) ~= token or
    redis.call("SISMEMBER", KEYS[8], tenant) ~= 1 or
    redis.call("SISMEMBER", KEYS[14], tenant) ~= 1 then
    return {CORRUPT}
  end
  local current_generation = redis.call("HGET", KEYS[9], tenant)
  if not positive_int64(current_generation) then return {CORRUPT} end
  local member = redis.call("SISMEMBER", KEYS[7], tenant)
  local positions = redis.call("LPOS", KEYS[6], tenant, "COUNT", 1)
  if member ~= 0 then return {CORRUPT} end
  turn_tenants[index] = tenant
  turn_has_position[index] = #positions > 0
end

for index, token in ipairs(provisionals) do
  local meta = provisional_meta[index]
  redis.call("ZREM", KEYS[10], token)
  redis.call("ZREM", meta["tenant_key"], token)
  redis.call("ZREM", KEYS[11], token)
  redis.call("HDEL", KEYS[13], token)
end
for index, token in ipairs(turns) do
  local tenant = turn_tenants[index]
  redis.call("ZREM", KEYS[2], token)
  redis.call("HDEL", KEYS[3], token)
  redis.call("HDEL", KEYS[4], token)
  redis.call("HDEL", KEYS[5], tenant)
  redis.call("SADD", KEYS[7], tenant)
  if not turn_has_position[index] then redis.call("RPUSH", KEYS[6], tenant) end
end
return {"OK", tostring(#provisionals), tostring(#turns),
  tostring(redis.call("ZCARD", KEYS[11])), tostring(redis.call("ZCARD", KEYS[2]))}
`

func (r *Redis) ObserveReadyFence(ctx context.Context, resource, expectedWriter string) (ResourceFence, error) {
	if err := validateRuntimeWriter(expectedWriter); err != nil {
		return ResourceFence{}, err
	}
	keys, err := r.runtimeKeyspace(resource)
	if err != nil {
		return ResourceFence{}, err
	}
	values, err := r.runRuntimeScript(ctx, "observe ready fence", redisObserveReadyScript, []string{keys.control, keys.progress}, expectedWriter)
	if err != nil {
		return ResourceFence{}, err
	}
	if err := redisRuntimeResultError(values); err != nil {
		return ResourceFence{}, err
	}
	if len(values) != 3 {
		return ResourceFence{}, ErrCoordinationCorrupt
	}
	epoch, epochOK := redisResultString(values[1])
	writer, writerOK := redisResultString(values[2])
	fence := ResourceFence{Epoch: epoch, WriterFingerprint: writer}
	if !epochOK || !writerOK || fence.Validate() != nil {
		return ResourceFence{}, ErrCoordinationCorrupt
	}
	return fence, nil
}

func (r *Redis) CheckReadyFence(ctx context.Context, resource string, fence ResourceFence) error {
	keys, err := r.validRuntimeFence(resource, fence)
	if err != nil {
		return err
	}
	values, err := r.runRuntimeScript(ctx, "check ready fence", redisCheckReadyScript, []string{keys.control, keys.progress}, fence.Epoch, fence.WriterFingerprint)
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) Activate(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	return r.ensureRuntimeActive(ctx, resource, fence, tenant)
}

func (r *Redis) EnsureKnownTenant(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	keys, err := r.validRuntimeTenant(resource, fence, tenant)
	if err != nil {
		return err
	}
	values, err := r.runRuntimeScript(ctx, "ensure known tenant", redisEnsureKnownScript,
		[]string{keys.control, keys.knownUsers, keys.progress}, fence.Epoch, fence.WriterFingerprint, tenant)
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) EnsureActive(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	return r.ensureRuntimeActive(ctx, resource, fence, tenant)
}

func (r *Redis) ensureRuntimeActive(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	keys, err := r.validRuntimeTenant(resource, fence, tenant)
	if err != nil {
		return err
	}
	values, err := r.runRuntimeScript(ctx, "activate tenant", redisActivateScript, []string{
		keys.control, keys.ring, keys.ringMembers, keys.activeUsers, keys.knownUsers,
		keys.activationGeneration, keys.processingTurns, keys.turnToTenant,
		keys.turnToGeneration, keys.tenantToTurn, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, tenant)
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) NextTurn(ctx context.Context, resource string, fence ResourceFence, turnToken ProcessingTurnToken, ttl time.Duration) (ProcessingTurn, bool, error) {
	keys, err := r.validRuntimeFence(resource, fence)
	if err != nil {
		return ProcessingTurn{}, false, err
	}
	if err := turnToken.Validate(); err != nil {
		return ProcessingTurn{}, false, fmt.Errorf("%w: invalid processing turn token", ErrInvalidModel)
	}
	ttlMS, err := redisRuntimeTTLMilliseconds(ttl)
	if err != nil {
		return ProcessingTurn{}, false, err
	}
	values, err := r.runRuntimeScript(ctx, "acquire processing turn", redisNextTurnScript, []string{
		keys.control, keys.ring, keys.ringMembers, keys.activeUsers, keys.knownUsers,
		keys.activationGeneration, keys.processingTurns, keys.turnToTenant,
		keys.turnToGeneration, keys.tenantToTurn, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, string(turnToken), ttlMS, redisRuntimeTurnScanBatch)
	if err != nil {
		return ProcessingTurn{}, false, err
	}
	code, ok := redisRuntimeCode(values)
	if !ok {
		return ProcessingTurn{}, false, ErrCoordinationCorrupt
	}
	if code == redisResultEmpty {
		if len(values) != 1 {
			return ProcessingTurn{}, false, ErrCoordinationCorrupt
		}
		return ProcessingTurn{}, false, nil
	}
	if code != redisResultOK {
		return ProcessingTurn{}, false, redisScriptError(code)
	}
	if len(values) != 3 {
		return ProcessingTurn{}, false, ErrCoordinationCorrupt
	}
	tenant, tenantOK := redisResultString(values[1])
	generation, generationOK := redisResultUint64(values[2])
	turn := ProcessingTurn{Token: turnToken, TenantID: tenant, ObservedActivationGeneration: generation}
	if !tenantOK || !generationOK || turn.Validate() != nil {
		return ProcessingTurn{}, false, ErrCoordinationCorrupt
	}
	return turn, true, nil
}

func (r *Redis) RotateOrDeactivate(ctx context.Context, resource string, fence ResourceFence, turnToken ProcessingTurnToken, observedGeneration uint64, hasReady bool) error {
	keys, err := r.validRuntimeFence(resource, fence)
	if err != nil {
		return err
	}
	if turnToken.Validate() != nil || observedGeneration == 0 || observedGeneration > uint64(^uint64(0)>>1) {
		return fmt.Errorf("%w: invalid processing turn", ErrInvalidModel)
	}
	values, err := r.runRuntimeScript(ctx, "settle processing turn", redisRotateScript, []string{
		keys.control, keys.ring, keys.ringMembers, keys.activeUsers,
		keys.activationGeneration, keys.processingTurns, keys.turnToTenant,
		keys.turnToGeneration, keys.tenantToTurn, keys.knownUsers, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, string(turnToken), strconv.FormatUint(observedGeneration, 10), redisRuntimeBool(hasReady))
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) AcquireProvisional(ctx context.Context, resource string, fence ResourceFence, tenant, attemptID string, limits CapacityLimits, ttl time.Duration) (ReservationDecision, error) {
	keys, tenantKey, err := r.validRuntimeReservation(resource, fence, tenant)
	if err != nil {
		return "", err
	}
	if !lowerHex32Pattern.MatchString(attemptID) {
		return "", fmt.Errorf("%w: invalid provisional attempt ID", ErrInvalidModel)
	}
	if err := limits.Validate(); err != nil {
		return "", fmt.Errorf("%w: invalid capacity limits", ErrInvalidModel)
	}
	if limits.GlobalConcurrency > MaxRecoveryPageLimit {
		return "", fmt.Errorf("%w: global concurrency exceeds the bounded Redis scan limit", ErrInvalidModel)
	}
	ttlMS, err := redisRuntimeTTLMilliseconds(ttl)
	if err != nil {
		return "", err
	}
	values, err := r.runRuntimeScript(ctx, "acquire provisional reservation", redisAcquireProvisionalScript, []string{
		keys.control, keys.activeUsers, keys.knownUsers, keys.globalInflight,
		tenantKey, keys.provisional, keys.stableIndex, keys.reservationMetadata, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, tenant, "p:"+attemptID, ttlMS,
		limits.GlobalConcurrency, limits.PerUserBaseConcurrency, limits.PerUserBurstConcurrency,
		redisRuntimeBool(limits.BorrowEnabled), keys.root+"inflight:tenant:", resource,
		redisRuntimeReservationCleanupBatch, MaxRecoveryPageLimit)
	if err != nil {
		return "", err
	}
	code, ok := redisRuntimeCode(values)
	if !ok || len(values) != 1 {
		return "", ErrCoordinationCorrupt
	}
	decision := ReservationDecision(code)
	switch decision {
	case ReservationRegular, ReservationBorrowed,
		ReservationDeniedGlobalFull, ReservationDeniedTenantBurst,
		ReservationDeniedCompetition, ReservationDeniedBorrowOff:
		return decision, nil
	default:
		return "", redisScriptError(code)
	}
}

func (r *Redis) BindReservation(ctx context.Context, resource string, fence ResourceFence, tenant, attemptID, stableToken string, ttl time.Duration) error {
	keys, tenantKey, err := r.validRuntimeReservation(resource, fence, tenant)
	if err != nil {
		return err
	}
	if !lowerHex32Pattern.MatchString(attemptID) || !stableReservationPattern.MatchString(stableToken) {
		return fmt.Errorf("%w: invalid reservation token", ErrInvalidModel)
	}
	ttlMS, err := redisRuntimeTTLMilliseconds(ttl)
	if err != nil {
		return err
	}
	values, err := r.runRuntimeScript(ctx, "bind reservation", redisBindReservationScript, []string{
		keys.control, keys.globalInflight, tenantKey, keys.provisional,
		keys.stableIndex, keys.reservationMetadata, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, tenant, "p:"+attemptID, stableToken, ttlMS,
		keys.root+"inflight:tenant:", resource)
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) RenewStable(ctx context.Context, resource string, fence ResourceFence, tenant, stableToken string, ttl time.Duration) error {
	return r.writeReadyStable(ctx, resource, fence, tenant, stableToken, ttl, false)
}

func (r *Redis) EnsureReadyStableInflight(ctx context.Context, resource string, fence ResourceFence, tenant, stableToken string, ttl time.Duration) error {
	return r.writeReadyStable(ctx, resource, fence, tenant, stableToken, ttl, true)
}

func (r *Redis) writeReadyStable(ctx context.Context, resource string, fence ResourceFence, tenant, stableToken string, ttl time.Duration, allowCreate bool) error {
	keys, tenantKey, err := r.validRuntimeReservation(resource, fence, tenant)
	if err != nil {
		return err
	}
	if !stableReservationPattern.MatchString(stableToken) {
		return fmt.Errorf("%w: invalid stable reservation token", ErrInvalidModel)
	}
	ttlMS, err := redisRuntimeTTLMilliseconds(ttl)
	if err != nil {
		return err
	}
	values, err := r.runRuntimeScript(ctx, "write stable reservation", redisStableWriteScript, []string{
		keys.control, keys.globalInflight, tenantKey, keys.provisional,
		keys.stableIndex, keys.reservationMetadata, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, tenant, stableToken, ttlMS,
		redisRuntimeBool(allowCreate), keys.root+"inflight:tenant:", resource)
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) Release(ctx context.Context, resource string, fence ResourceFence, tenant, token string) error {
	keys, tenantKey, err := r.validRuntimeReservation(resource, fence, tenant)
	if err != nil {
		return err
	}
	kind := ""
	switch {
	case lowerHex32Pattern.MatchString(token):
		token = "p:" + token
		kind = "P"
	case len(token) == 34 && token[:2] == "p:" && lowerHex32Pattern.MatchString(token[2:]):
		kind = "P"
	case stableReservationPattern.MatchString(token):
		kind = "S"
	default:
		return fmt.Errorf("%w: invalid reservation token", ErrInvalidModel)
	}
	values, err := r.runRuntimeScript(ctx, "release reservation", redisReleaseReservationScript, []string{
		keys.control, keys.globalInflight, tenantKey, keys.provisional,
		keys.stableIndex, keys.reservationMetadata, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, tenant, token, kind,
		keys.root+"inflight:tenant:", resource)
	if err != nil {
		return err
	}
	return redisRuntimeExpectOK(values)
}

func (r *Redis) ListReadyStableInflight(ctx context.Context, resource string, fence ResourceFence, cursor string, limit int) (RecoveryPage[ReservationRef], error) {
	keys, err := r.validRuntimeFence(resource, fence)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	if err := ValidatePageLimit(limit); err != nil {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid stable reservation page limit", ErrInvalidModel)
	}
	if err := ValidateCursor(cursor); err != nil || (cursor != "" && !stableReservationPattern.MatchString(cursor)) {
		return RecoveryPage[ReservationRef]{}, fmt.Errorf("%w: invalid stable reservation cursor", ErrInvalidModel)
	}
	values, err := r.runRuntimeScript(ctx, "list stable reservations", redisListStableScript, []string{
		keys.control, keys.globalInflight, keys.provisional, keys.stableIndex, keys.reservationMetadata, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, cursor, limit, keys.root+"inflight:tenant:", resource)
	if err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	if err := redisRuntimeResultError(values); err != nil {
		return RecoveryPage[ReservationRef]{}, err
	}
	if len(values) < 4 {
		return RecoveryPage[ReservationRef]{}, ErrCoordinationCorrupt
	}
	doneRaw, doneOK := redisResultString(values[1])
	next, nextOK := redisResultString(values[2])
	count, countOK := redisResultInt64(values[3])
	if !doneOK || !nextOK || !countOK || count < 0 || count > int64(limit) || len(values) != 4+int(count)*3 {
		return RecoveryPage[ReservationRef]{}, ErrCoordinationCorrupt
	}
	page := RecoveryPage[ReservationRef]{Done: doneRaw == "1", NextCursor: next, Items: make([]ReservationRef, 0, count)}
	if doneRaw != "0" && doneRaw != "1" {
		return RecoveryPage[ReservationRef]{}, ErrCoordinationCorrupt
	}
	for index := 0; index < int(count); index++ {
		base := 4 + index*3
		stable, stableOK := redisResultString(values[base])
		tenant, tenantOK := redisResultString(values[base+1])
		expires, expiresOK := redisResultInt64(values[base+2])
		item := ReservationRef{TenantID: tenant, StableToken: stable, ExpiresAtUnixMS: expires}
		if !stableOK || !tenantOK || !expiresOK || item.Validate() != nil {
			return RecoveryPage[ReservationRef]{}, ErrCoordinationCorrupt
		}
		page.Items = append(page.Items, item)
	}
	if page.Validate(cursor, limit) != nil {
		return RecoveryPage[ReservationRef]{}, ErrCoordinationCorrupt
	}
	return page, nil
}

func (r *Redis) ReapExpiredTurnsAndProvisionals(ctx context.Context, resource string, fence ResourceFence, limit int) (RecoveryCleanupResult, error) {
	keys, err := r.validRuntimeFence(resource, fence)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	if err := ValidatePageLimit(limit); err != nil {
		return RecoveryCleanupResult{}, fmt.Errorf("%w: invalid cleanup limit", ErrInvalidModel)
	}
	values, err := r.runRuntimeScript(ctx, "reap expired runtime leases", redisReapExpiredScript, []string{
		keys.control, keys.processingTurns, keys.turnToTenant, keys.turnToGeneration,
		keys.tenantToTurn, keys.ring, keys.ringMembers, keys.activeUsers,
		keys.activationGeneration, keys.globalInflight, keys.provisional,
		keys.stableIndex, keys.reservationMetadata, keys.knownUsers, keys.progress,
	}, fence.Epoch, fence.WriterFingerprint, limit, keys.root+"inflight:tenant:", resource)
	if err != nil {
		return RecoveryCleanupResult{}, err
	}
	if err := redisRuntimeResultError(values); err != nil {
		return RecoveryCleanupResult{}, err
	}
	if len(values) != 5 {
		return RecoveryCleanupResult{}, ErrCoordinationCorrupt
	}
	parsed := make([]int64, 4)
	for index := range parsed {
		value, ok := redisResultInt64(values[index+1])
		if !ok || value < 0 {
			return RecoveryCleanupResult{}, ErrCoordinationCorrupt
		}
		parsed[index] = value
	}
	result := RecoveryCleanupResult{
		RemovedProvisionals: parsed[0], RemovedTurns: parsed[1],
		RemainingProvisionals: parsed[2], RemainingTurns: parsed[3],
	}
	if result.Validate() != nil {
		return RecoveryCleanupResult{}, ErrCoordinationCorrupt
	}
	return result, nil
}

func (r *Redis) runtimeKeyspace(resource string) (redisKeyspace, error) {
	if r == nil || r.client == nil {
		return redisKeyspace{}, fmt.Errorf("%w: Redis dependency is required", ErrInvalidModel)
	}
	return buildRedisKeyspace(r.options.KeyPrefix, resource)
}

func (r *Redis) validRuntimeFence(resource string, fence ResourceFence) (redisKeyspace, error) {
	keys, err := r.runtimeKeyspace(resource)
	if err != nil {
		return redisKeyspace{}, err
	}
	if fence.Validate() != nil {
		return redisKeyspace{}, fmt.Errorf("%w: invalid READY fence", ErrInvalidModel)
	}
	return keys, nil
}

func (r *Redis) validRuntimeTenant(resource string, fence ResourceFence, tenant string) (redisKeyspace, error) {
	keys, err := r.validRuntimeFence(resource, fence)
	if err != nil {
		return redisKeyspace{}, err
	}
	if ValidateTenantID(tenant) != nil {
		return redisKeyspace{}, fmt.Errorf("%w: invalid reservation tenant", ErrInvalidModel)
	}
	return keys, nil
}

func (r *Redis) validRuntimeReservation(resource string, fence ResourceFence, tenant string) (redisKeyspace, string, error) {
	keys, err := r.validRuntimeTenant(resource, fence, tenant)
	if err != nil {
		return redisKeyspace{}, "", err
	}
	tenantKey, err := keys.tenantInflight(resource, tenant)
	if err != nil {
		return redisKeyspace{}, "", err
	}
	return keys, tenantKey, nil
}

func validateRuntimeWriter(writer string) error {
	if !lowerHex64Pattern.MatchString(writer) {
		return fmt.Errorf("%w: invalid writer fingerprint", ErrInvalidModel)
	}
	return nil
}

func redisRuntimeTTLMilliseconds(ttl time.Duration) (int64, error) {
	if ttl <= 0 || ttl > maxResourceDuration {
		return 0, fmt.Errorf("%w: invalid Redis lease duration", ErrInvalidModel)
	}
	milliseconds := int64(ttl / time.Millisecond)
	if ttl%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds <= 0 {
		milliseconds = 1
	}
	return milliseconds, nil
}

func redisRuntimeBool(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func (r *Redis) runRuntimeScript(ctx context.Context, operation, source string, keys []string, args ...any) ([]interface{}, error) {
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	result, err := redis.NewScript(source).Run(opCtx, r.client, keys, args...).Result()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(opCtx.Err(), context.Canceled) || errors.Is(opCtx.Err(), context.DeadlineExceeded) {
			if opCtx.Err() != nil {
				return nil, redisDependencyError(operation, opCtx.Err())
			}
			return nil, redisDependencyError(operation, err)
		}
		if redisTopologyServerError(err) {
			return nil, fmt.Errorf("%w: Redis deployment no longer supports fair coordination", ErrUnsupportedTopology)
		}
		var serverError redis.Error
		if errors.As(err, &serverError) {
			message := strings.ToUpper(serverError.Error())
			for _, marker := range []string{
				"WRONGTYPE", "ERROR RUNNING SCRIPT", "USER_SCRIPT:",
				"INVALID LUA", "ATTEMPT TO", "BAD ARGUMENT",
			} {
				if strings.Contains(message, marker) {
					return nil, fmt.Errorf("%w: Redis script rejected coordination state", ErrCoordinationCorrupt)
				}
			}
		}
		return nil, redisDependencyError(operation, err)
	}
	values, ok := result.([]interface{})
	if !ok {
		return nil, ErrCoordinationCorrupt
	}
	return values, nil
}

func redisRuntimeCode(values []interface{}) (string, bool) {
	if len(values) == 0 {
		return "", false
	}
	return redisResultString(values[0])
}

func redisRuntimeResultError(values []interface{}) error {
	code, ok := redisRuntimeCode(values)
	if !ok {
		return ErrCoordinationCorrupt
	}
	if code != redisResultOK {
		return redisScriptError(code)
	}
	return nil
}

func redisRuntimeExpectOK(values []interface{}) error {
	if len(values) != 1 {
		return ErrCoordinationCorrupt
	}
	return redisRuntimeResultError(values)
}

func redisResultUint64(value any) (uint64, bool) {
	text, ok := redisResultString(value)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseUint(text, 10, 64)
	return parsed, err == nil && parsed > 0
}
