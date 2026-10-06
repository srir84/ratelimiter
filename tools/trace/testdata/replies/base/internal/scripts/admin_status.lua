#!lua flags=no-writes
-- admin_status's rows: a return of a table, a numeric code, return false, {…} in a local
-- function, and a reply built in a variable that is returned at the end.
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if ok and #raw == 0 then return { REFUSED, 0, 'epoch_record_missing' } end
if not ok then return { 41, 0, 'epoch_record_malformed' } end
-- The return above ended at its end, so this table is no reply: a name, not a code, comes first.
local pair, names = 1, { code_name, 0, '' }
local function changed()
  return false, { REFUSED, 0, 'epoch_changed' }
end
local q = redis.pcall('HLEN', KEYS[2])
if type(q) == 'table' then return { REFUSED, 0, 'shared_key_malformed' } end
local cols = { 'id', 'fp', 'hold' }
-- A table no return gives is data, whatever comes first. A name a return gives is the local in
-- scope there: columns' count and list are its own numbers, not the top level's tables, the one
-- declared before columns (shadowed) or the one declared after it (not yet in scope).
local count = { cols, 0, '' }
local function columns()
  local count = #cols
  local list = count
  return list, count
end
local function same(count)
  return count -- the parameter, not the top level's count
end
local list = { cols, 0, '' }
local out = { OK, 0, '', raw[1], changed, cols, pair, names, columns, same, count, list }
out[#out + 1] = q
return out
