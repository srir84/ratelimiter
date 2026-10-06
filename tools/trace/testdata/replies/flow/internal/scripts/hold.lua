#!lua flags=allow-oom
-- S4 repair F1: what the value flow cannot follow, beyond the four shapes, and what it never
-- needs to (a call's result, a parameter's value, KEYS, ARGV, a code name, an operator's result).
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
local rec = { reply = { REFUSED, 0, 'epoch_changed' } }
local function field()
  return rec.reply -- want: a field of a local that holds a table
end
local function element()
  local list = { { 41, 0, 'not_a_design_reason' } }
  return (list)[1] -- want: an element of a local that holds a table
end
local function outside()
  return E -- want: E is no local of this file (the prelude's)
end
local function outside_field()
  return K.epoch -- want: a field of no local of this file
end
E = { REFUSED, 0, 'epoch_changed' } -- want: a table assigned to no local of this file
local held = rec
T = held -- want: a local assigned to no local of this file
local function passes(p)
  if p then return p.x end
  if not ok then return tonumber(ARGV[1]) end
  if #raw == 0 then return KEYS[1], ARGV, OK end
  return to_map(raw).id, raw[1], #raw + 1, raw[1] == 'x', not p
end
local function broken()
  local z = f(1 -- want: a call that never closes: the statement cannot be parsed
end
local function broken_return()
  return g(2 -- want: the same in a return
end
return { OK, 0, '', field(), element(), outside(), outside_field(), passes(1), broken(), broken_return() }
