#!lua flags=no-writes
-- read_epoch (DESIGN §A4.1, §A4.9): the epoch record, the high-water mark, the clock and the
-- quarantine count, for the epoch watch, the rollback check and the gauges. It never writes.
-- The state and reason follow load_epoch; every field that validates is returned, also in
-- state 22 ('' for an id or fp that fails is_hex32, 0 for a number that fails is_uint53).
K.epoch, K.ctl, K.q = KEYS[1], KEYS[2], KEYS[3]
local state, reason = HEALTHY, ''
local e, why = load_epoch()
if not e then
  state, reason = E_INTEGRITY_EPOCH, why
  local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
  e = {}
  if ok then e = to_map(raw) end
end

local function num(v)
  if is_uint53(v) then return tonumber(v) end
  return 0
end

local id, fp, hwm = '', '', num(e.hwm)
if is_hex32(e.id) then id = e.id end
if is_hex32(e.fp) then fp = e.fp end
if hwm >= TBOUND then hwm = 0 end
local clock_us = read_clock(e) or 0
local quarantined = ''
if id ~= '' and K.q == 'rl:e:' .. id .. ':q' then
  local qok, n = pcall(redis.call, 'HLEN', K.q)
  if qok then quarantined = n end
end
return { state, 0, reason, id, fp, hwm, num(e.gen), clock_us, num(e.nb), num(e.nm), quarantined }
