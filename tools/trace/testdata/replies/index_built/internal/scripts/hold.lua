#!lua flags=allow-oom
-- S4 repair F1, shape 3: a reply filled in by index into an empty table cannot be read, nor one
-- changed by index after its constructor; an append after the reason may follow a reply.
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if not ok then
  local r = {} -- want: returned below, built empty
  r[1] = REFUSED -- want: r is returned, changed by index
  r[2] = 0 -- want
  r[3] = 'not_a_design_reason' -- want
  return r
end
local function appended()
  local r = {} -- want: built empty, then appended to
  r[#r + 1] = REFUSED
  r[#r + 1] = 0
  r[#r + 1] = 'epoch_changed'
  return r
end
-- The data entry names prelude.lua's to_map; a script's own to_map is read like any helper.
local function to_map(list)
  local m = {} -- want: built empty
  for i = 1, #list, 2 do
    m[list[i]] = list[i + 1] -- want: m is returned, changed by index
  end
  return m
end
local out = { OK, 0, '' }
if #raw == 0 then out[1] = REFUSED end -- want: a reply's code changed after its constructor
out[3] = 'epoch_changed' -- want
out[#out + 1] = appended()
out[#out + 1] = to_map(raw)
-- A table no return gives may be filled in by index: data.
local scratch = {}
scratch[1] = REFUSED
scratch[3] = 'not_returned'
return out
