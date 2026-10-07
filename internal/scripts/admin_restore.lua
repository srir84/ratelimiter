#!lua flags=allow-oom
-- admin_restore (DESIGN §A4.6; SPEC ADM-2): gives a quarantined key a new bucket with the
-- operator's balance, as a new generation. The fence (load_epoch, the expected id, the
-- fingerprint), the per-key KEYS (§A4.1), the q and ix reads (an error sets the hold
-- shared_key_malformed, §A4.8 rule 2), the quarantine flag, the balance's range, the clock;
-- then the markers are counted as max(m:* fields of k, ZCARD mi), 0 for what cannot be read.
-- The writes replace every entry attributable to the key (§A3.2): k and mi deleted, the ix
-- member removed, nm less the count, nb + 1 when the key had no ix member (§A3.3), a new
-- generation, the bucket u = balance, t, g, s = 0, x = t + lifetime_us (§D2 decision 5),
-- mc = 0, its ix member at t, and hwm. The q flag stays: admin_clear lifts it (ADM-3).
K.epoch, K.ctl, K.ix, K.q, K.k, K.mi = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
ENC = ARGV[3]
local enc, balance = ENC, ARGV[4]
local cap_units, lifetime_us = tonumber(ARGV[5]), tonumber(ARGV[6])
local why
E, why = load_epoch()
if not E then return { REFUSED, 0, why } end
if E.id ~= ARGV[1] then return { REFUSED, 0, 'epoch_changed' } end
if E.fp ~= ARGV[2] then return { REFUSED, 0, 'fingerprint_mismatch' } end
if not assert_keys(enc) then return { E_BAD_CALL, 0, 'keys_mismatch' } end
local qok, qv = pcall(redis.call, 'HEXISTS', K.q, enc)
local zok, zs = pcall(redis.call, 'ZSCORE', K.ix, enc)
if not qok or not zok then return hold('shared_key_malformed') end
if qv ~= 1 then return { REFUSED, 0, 'not_quarantined' } end
if not is_uint53(balance) or tonumber(balance) > cap_units then return { REFUSED, 0, 'balance_out_of_range' } end
local now = read_clock(E)
if not now then return { E_CLOCK, 0, 'clock' } end
T = math.max(now, tonumber(E.hwm)) -- budget time (CLK-4)
local t = T

-- The markers to take off nm: the m:* fields of k and the members of mi, whichever are more;
-- a key of the wrong type counts 0 (DEL removes it all the same).
local fields = 0
local kr = redis.pcall('HGETALL', K.k)
if kr.err == nil then
  for i = 1, #kr, 2 do
    if string.sub(kr[i], 1, 2) == 'm:' then fields = fields + 1 end
  end
end
local members = redis.pcall('ZCARD', K.mi)
if type(members) ~= 'number' then members = 0 end
local count, x = math.max(fields, members), t + lifetime_us -- every value the writes need
-- ---- every check done; writes start here ----
redis.call('DEL', K.k, K.mi)
redis.call('ZREM', K.ix, enc)
dec_counter('nm', count)
if not zs then redis.call('HINCRBY', K.epoch, 'nb', 1) end
local g = redis.call('HINCRBY', K.epoch, 'gen', 1)
redis.call('HSET', K.k, 'u', balance, 't', t, 'g', g, 's', 0, 'x', x, 'mc', 0)
redis.call('ZADD', K.ix, t, enc)
return finish(OK, '', g)
