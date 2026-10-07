#!lua flags=allow-oom
-- admin_clear (DESIGN §A4.6; SPEC ADM-3): lifts the quarantine of a key whose bucket and every
-- entry are consistent. The fence and the per-key KEYS as for admin_restore; the q read (an
-- error sets the hold shared_key_malformed, §A4.8 rule 2) and the quarantine flag; check_key
-- (§A3.3): shared sets the hold, and any outcome but ok refuses with its reason, bucket_missing
-- for an absent key (ADM-3 needs a bucket); then the clock. The writes: the q field deleted,
-- the ix score set to t (ZADD XX undoes the quarantine push, so the key is reconsidered at
-- once), and hwm.
K.epoch, K.ctl, K.ix, K.q, K.k, K.mi = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
ENC = ARGV[3]
local enc, cap_units = ENC, tonumber(ARGV[4])
local why
E, why = load_epoch()
if not E then return { REFUSED, 0, why } end
if E.id ~= ARGV[1] then return { REFUSED, 0, 'epoch_changed' } end
if E.fp ~= ARGV[2] then return { REFUSED, 0, 'fingerprint_mismatch' } end
if not assert_keys(enc) then return { E_BAD_CALL, 0, 'keys_mismatch' } end
local qok, qv = pcall(redis.call, 'HEXISTS', K.q, enc)
if not qok then return hold('shared_key_malformed') end
if qv ~= 1 then return { REFUSED, 0, 'not_quarantined' } end
local st, reason = check_key(enc, cap_units) -- §A3.3, reads only
if st == 'shared' then return hold('shared_key_malformed') end
if st == 'absent' then return { REFUSED, 0, 'bucket_missing' } end
if st ~= 'ok' then return { REFUSED, 0, reason } end -- 'bad': reason is check_key's
local now = read_clock(E)
if not now then return { E_CLOCK, 0, 'clock' } end
T = math.max(now, tonumber(E.hwm)) -- budget time (CLK-4)
-- ---- every check done; writes start here ----
redis.call('HDEL', K.q, enc)
redis.call('ZADD', K.ix, 'XX', T, enc)
return finish(OK, '', 0)
