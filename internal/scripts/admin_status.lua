#!lua flags=no-writes
-- admin_status (DESIGN §A4.6, §A4.9; SPEC ADM-4): read-only; works under a hold. Every read is
-- pcall'd, and a wrong-type q is a refusal, never a hold (§A4.8 rule 2). hwm is '' when it is
-- missing or fails is_uint53, and shown as stored otherwise, also at or above TBOUND.
K.epoch, K.q = KEYS[1], KEYS[2]
local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
if ok and #raw == 0 then return { REFUSED, 0, 'epoch_record_missing' } end
if not ok or not record_wellformed(to_map(raw)) then return { REFUSED, 0, 'epoch_record_malformed' } end
local e = to_map(raw)
if e.id ~= ARGV[1] then return { REFUSED, 0, 'epoch_changed' } end
local quarantined = redis.pcall('HLEN', K.q)
if type(quarantined) == 'table' then return { REFUSED, 0, 'shared_key_malformed' } end
local hwm = ''
if is_uint53(e.hwm) then hwm = tonumber(e.hwm) end
return { OK, 0, '', e.id, e.fp, e.hold, hwm, tonumber(e.nb), tonumber(e.nm), quarantined }
