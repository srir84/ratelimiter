#!lua flags=no-writes
-- read_epoch's code is the non-literal state, which the table declares; num, a helper, returns
-- a number built from its parameter, never a table.
local state, reason = HEALTHY, ''
local why = 'epoch_record_missing'
if #KEYS == 0 then
  state, reason = E_INTEGRITY_EPOCH, why
end
local function num(v)
  return tonumber(v) or 0
end
return { state, 0, reason, '', '', num(0) }
