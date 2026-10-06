#!lua
-- acquire (DESIGN §A4.3; SPEC API-1a): after the prelude's steps 1 and 2, the quarantine
-- flag, check_key, the tracked-bucket capacity for a new bucket, then refill and allow or limit.
K.epoch, K.ctl, K.ix, K.q, K.k, K.mi = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
ENC = ARGV[3]
local cap_units, rate_period_ns, rate_tokens = tonumber(ARGV[4]), tonumber(ARGV[5]), tonumber(ARGV[6])
local lifetime_us, max_tracked = tonumber(ARGV[7]), tonumber(ARGV[8])
local entered, refusal = enter(tonumber(ARGV[9]), tonumber(ARGV[10]), ENC)
if not entered then return refusal end
local e, t, enc = E, T, ENC

local qok, qv = pcall(redis.call, 'HEXISTS', K.q, enc)
if not qok then return hold('shared_key_malformed') end
if qv == 1 then return reply(E_INTEGRITY_KEY, 'quarantined') end -- step 3 (writes nothing)
local st, b = check_key(enc, cap_units) -- §A3.3, reads only
if st == 'shared' then return hold('shared_key_malformed') end
if st == 'bad' then return quarantine(b) end -- b is the reason
if st == 'absent' then -- expected absence (STA-8)
  if tonumber(e.nb) >= max_tracked then return reply(E_CAPACITY, 'tracked_buckets') end -- step 4 (writes nothing)
  -- ---- every check done; writes start here ----
  local g = redis.call('HINCRBY', K.epoch, 'gen', 1)
  redis.call('HINCRBY', K.epoch, 'nb', 1)
  local u, exp = cap_units - rate_period_ns, t + lifetime_us -- new bucket starts full (BUD-6)
  redis.call('HSET', K.k, 'u', u, 't', t, 'g', g, 's', 1, 'x', exp, 'mc', 0)
  redis.call('ZADD', K.ix, exp, enc)
  write_hwm()
  return { E_ALLOWED, t, '', u, g, 1, exp }
end
local u = refill(b.u, cap_units, t - b.t, rate_tokens)
if u < rate_period_ns then return reply(E_LIMITED, '', u) end -- limited: writes nothing (CLK-4)
-- ---- every check done; writes start here ----
u = u - rate_period_ns
local s, exp = b.s + 1, t + lifetime_us
local x = math.max(b.x, exp)
redis.call('HSET', K.k, 'u', u, 't', t, 's', s, 'x', x)
redis.call('ZADD', K.ix, 'GT', math.min(x, b.first_marker_expiry or x), enc)
write_hwm()
return { E_ALLOWED, t, '', u, b.g, s, exp }
