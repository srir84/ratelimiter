#!lua flags=allow-oom
-- S4 repair F1, shape 1: a table a return gives with a name first is a reply in a function too,
-- so its code must be a code name or a non-literal the reply table declares for the script.
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if not ok or #raw == 0 then return { REFUSED, 0, 'epoch_changed' } end
local function refusal()
  local code = 99
  return { code, 0, 'epoch_changed' } -- want: code, a local, is no code name and no declared name
end
local function stale()
  local code = REFUSED
  return { code, 0, 'not_a_design_reason' } -- want: the same, holding a code name's value
end
local cols = { 'id', 'fp', 'hold' }
local function three()
  return { cols, cols, cols } -- want: an upvalue first
end
local function columns()
  local list = { cols, cols, cols } -- want: columns returns list
  return list
end
local function looped()
  for _ = 1, 2 do
    cols = cols or {}
  end
  return { cols, cols, cols } -- want: a helper's table of names is no longer data once returned
end
local function either(cond)
  local code = 41
  return cond and { code, 0, 'epoch_changed' } or false -- want: an operand of and/or is returned
end
local function relay()
  local r = { cols, 0, 'epoch_changed' } -- want: relay returns s, which holds r
  local s = r
  return s
end
-- Lua's scoping decides which local a returned name means.
local function branches(c)
  local r = { cols, 0, 'epoch_changed' } -- want: the else branch returns this r, not the if branch's
  if c then
    local r = 0
    return r
  else
    return r
  end
end
local outer = { cols, 0, 'epoch_changed' } -- want: shadowed returns it through local outer = outer
local function shadowed()
  local outer = outer
  return outer
end
local function after_loop(list)
  local v = { cols, 0, 'epoch_changed' } -- want: after the loop, v is this local again
  for _, v in ipairs(list) do
    if v then break end
  end
  return v
end
if raw[1] == ARGV[1] then return refusal() end
return { OK, 0, '', stale(), three(), columns(), looped(), either(true), relay(), branches(true), shadowed(), after_loop({}) }
