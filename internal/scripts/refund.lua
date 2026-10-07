#!lua
-- refund (DESIGN §A4.4; SPEC API-2a steps 2-9): after the prelude's steps 2 and 3, the receipt's
-- epoch, its expiry, the quarantine flag, check_key, the generation and sequence, the marker and
-- the marker caps; then refill, credit, and the marker that holds the credit's budget time.
-- Step 1 (signature, format, canonical decimal g and s) already passed in Go.
K.epoch, K.ctl, K.ix, K.q, K.k, K.mi = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
ENC = ARGV[3]
local r_epoch, g_str, s_str, expiry_us = ARGV[4], ARGV[5], ARGV[6], tonumber(ARGV[7])
local cap_units, rate_period_ns, rate_tokens = tonumber(ARGV[8]), tonumber(ARGV[9]), tonumber(ARGV[10])
local max_markers, max_markers_per_key = tonumber(ARGV[11]), tonumber(ARGV[12])
local entered, refusal = enter(tonumber(ARGV[13]), tonumber(ARGV[14]), ENC)
if not entered then return refusal end
local e, t, enc = E, T, ENC

if r_epoch ~= e.id then return finish(E_RECEIPT_INVALID, 'epoch') end -- step 4
if t >= expiry_us then return finish(E_RECEIPT_EXPIRED, 'expired') end -- step 5
local qok, qv = pcall(redis.call, 'HEXISTS', K.q, enc)
if not qok then return hold('shared_key_malformed') end
if qv == 1 then return finish(E_INTEGRITY_KEY, 'quarantined') end -- step 6
local st, b = check_key(enc, cap_units) -- §A3.3, reads only
if st == 'shared' then return hold('shared_key_malformed') end
if st == 'bad' then return quarantine(b) end -- b is the reason
if st == 'absent' then return quarantine('refund_bucket_missing') end -- STA-9 bullet 4, API-2b
if tonumber(g_str) ~= b.g or tonumber(s_str) < 1 or tonumber(s_str) > b.s then
  return finish(E_RECEIPT_INVALID, 'generation_or_sequence') -- step 7
end
local gs = g_str .. ':' .. s_str -- g_str, s_str: canonical decimal strings, as the marker field holds them
local mval = b.marker_at[gs]
if mval then return finish(E_ALREADY_APPLIED, '', mval.credit) end -- step 8 (check_key verified its mi entry)
if b.mc >= max_markers_per_key then return finish(E_CAPACITY, 'key_markers') end -- step 9, per key
if tonumber(e.nm) >= max_markers then return finish(E_CAPACITY, 'live_markers') end -- step 9, epoch-wide
-- ---- every check done; writes start here ----
local u = refill(b.u, cap_units, t - b.t, rate_tokens)
local credited = math.min(cap_units, u + rate_period_ns) - u -- may be 0
redis.call('HSET', K.k, 'u', u + credited, 't', t, 'm:' .. gs, string.format('%d:%d', expiry_us, t), 'mc', b.mc + 1)
redis.call('ZADD', K.mi, expiry_us, gs)
redis.call('ZADD', K.ix, 'LT', expiry_us, enc)
redis.call('HINCRBY', K.epoch, 'nm', 1)
write_hwm()
return { E_APPLIED, t, '', credited }
