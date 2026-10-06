#!lua flags=allow-oom
-- S4 repair F1, shape 2: a table a return gives whose code is an expression or a string cannot
-- be read, at the top level, in a function, or through a local.
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if not ok then return { ARGV[3] == '1' and OK or REFUSED, 0, 'epoch_changed' } end -- want: an expression
if #raw == 0 then return ({ (REFUSED), 0, 'epoch_changed' }) end -- want: (REFUSED) is an expression too
local function pick()
  return { ARGV[3] == '1' and OK or REFUSED, 0, 'not_a_design_reason' } -- want: inside a function
end
local function built()
  local r = { REFUSED + 0, 0, 'epoch_changed' } -- want: built returns r
  return r
end
local function strings()
  return { 'x', 0, 'epoch_changed' } -- want: a string is no code
end
local function keyed()
  return { [1] = REFUSED, [2] = 0, [3] = 'epoch_changed' } -- want: a key is no code
end
-- An expression code in a table no return gives is data.
local scratch = { ARGV[3] == '1' and OK or REFUSED, 0, 'not_returned' }
return { OK, 0, '', pick(), built(), strings(), keyed(), scratch }
