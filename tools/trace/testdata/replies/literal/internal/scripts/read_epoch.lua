#!lua flags=no-writes
-- state holds codes 0 and 22 (the table's source), so a code 23 reply through it is wrong.
local state, reason = HEALTHY, ''
if #KEYS == 0 then return { E_INTEGRITY_KEY, 0, state } end -- want
-- With state's code and a literal reason, each code state holds needs a row with that reason.
if #KEYS == 1 then return { state, 0, '' } end -- want: 0 '' is a row, 22 '' is not
if #KEYS == 2 then return { state, 0, 'hwm_missing' } end -- want: 22 'hwm_missing' is a row, 0 'hwm_missing' is not
-- num, a helper of the script, builds a table from its parameter: no template outside the prelude.
local function num(v)
  return { v, 0, 'x' } -- want: built from num's parameter v
end
return { state, 0, reason, num(0) }
