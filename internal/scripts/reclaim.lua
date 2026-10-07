#!lua flags=allow-oom
-- reclaim (DESIGN §A4.7; SPEC ADM-1 reclamation): unlinks old-epoch keys. It reads rl:epoch
-- again: when the record is missing or not well-formed it unlinks nothing; otherwise it UNLINKs,
-- one call per key, each candidate in KEYS[2..] whose rl:e:<id>: prefix differs from the
-- current epoch's, so new-epoch data is never deleted; a candidate with no rl:e:<id>: prefix
-- (rl:epoch, rl:ctl:time, any other key) is never unlinked. The reply counts the keys the calls
-- unlinked (a candidate already gone counts 0).
K.epoch = KEYS[1]
local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
if not ok or #raw == 0 then return { OK, 0, '', 0 } end
local e = to_map(raw)
if not record_wellformed(e) then return { OK, 0, '', 0 } end
local current = 'rl:e:' .. e.id .. ':'
local doomed = {} -- built before the first write (§A4.8 rule 3)
for i = 2, #KEYS do
  local ns = string.match(KEYS[i], '^(rl:e:[^:]*:)') -- the candidate's rl:e:<id>: prefix
  if ns and ns ~= current then doomed[#doomed + 1] = KEYS[i] end
end
local unlinked = 0
for _, key in ipairs(doomed) do
  unlinked = unlinked + redis.call('UNLINK', key)
end
return { OK, 0, '', unlinked }
