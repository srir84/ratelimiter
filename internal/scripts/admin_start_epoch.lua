#!lua flags=allow-oom
-- admin_start_epoch (DESIGN §A4.6; SPEC ADM-1). A record is readable when HGETALL succeeds, it is
-- not empty and it is well-formed (the high-water mark and the hold's value are ignored). With
-- no flag it refuses a readable record; with the replace flag it needs a readable record of the
-- expected epoch. The clock is read with the new clock setting.
K.epoch, K.ctl = KEYS[1], KEYS[2]
local new_id, fp, schema, clock, replace, expected_id = ARGV[1], ARGV[2], ARGV[3], ARGV[4], ARGV[5], ARGV[6]
local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
local readable = ok and #raw > 0 and record_wellformed(to_map(raw))
if replace == '1' then
  if ok and #raw == 0 then return { REFUSED, 0, 'epoch_record_missing' } end
  if not readable then return { REFUSED, 0, 'epoch_record_malformed' } end
  if to_map(raw).id ~= expected_id then return { REFUSED, 0, 'epoch_changed' } end
elseif readable then
  return { REFUSED, 0, 'epoch_exists' }
end
local t = read_clock({ clock = clock })
if not t then return { E_CLOCK, 0, 'clock' } end
-- ---- every check done; writes start here ----
redis.call('DEL', K.epoch)
redis.call(
  'HSET',
  K.epoch,
  'id',
  new_id,
  'fp',
  fp,
  'schema',
  schema,
  'clock',
  clock,
  'hold',
  '',
  'hwm',
  t,
  'origin',
  t,
  'created',
  t,
  'gen',
  0,
  'nb',
  0,
  'nm',
  0
)
return { OK, t, '', new_id }
