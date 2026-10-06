#!lua flags=allow-oom
-- hold's rows: {40, 0, '', 0} and {41, 0, 'epoch_changed'}, as a table and through the helpers.
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if not ok or #raw == 0 then return { REFUSED, 0, 'epoch_changed' } end
if raw[1] ~= ARGV[1] then return reply(REFUSED, 'epoch_changed') end
return finish(OK, '', 0)

-- A local function named like a prelude helper is a definition, not a reply.
local function hold(why)
  return why
end
