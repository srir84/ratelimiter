#!lua flags=allow-oom
-- hold (DESIGN §A4.1, §A4.9, §A5.7): sets the integrity hold for the expected epoch when the
-- epoch watch sees the store go back in time. An existing hold stays as it is, and the
-- high-water mark is never touched.
K.epoch = KEYS[1]
local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
if not ok or #raw == 0 then return { REFUSED, 0, 'epoch_changed' } end
local e = to_map(raw)
if not record_wellformed(e) or e.id ~= ARGV[1] then return { REFUSED, 0, 'epoch_changed' } end
if e.hold == '' then redis.call('HSET', K.epoch, 'hold', ARGV[2]) end
return { OK, 0, '', 0 }
