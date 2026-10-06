#!lua flags=allow-oom
-- TestReplies's own table declares st (40 '' and 41 'epoch_changed') and gone (41 'epoch_changed')
-- for hold. With such a code and a literal reason, every code of the source needs a row with it.
local st, gone = OK, REFUSED
if #KEYS == 0 then st = REFUSED end
if #KEYS == 1 then return { gone, 0, 'epoch_changed' } end -- 41 'epoch_changed' is a row
if #KEYS == 2 then return { st, 0, 'epoch_changed' } end -- want: 40 'epoch_changed' is no row
if #KEYS == 3 then return { st, 0, '' } end -- want: 40 '' is a row, 41 '' is not
if #KEYS == 4 then return { gone, 0, '' } end -- want: 41 '' is no row
-- Inside a function, an integer-first table is a reply when a return gives it; otherwise data.
local function changed()
  local scratch = { 41, 0, 'not_returned' }
  st = scratch[1]
  return false, { 41, 0, 'epoch_changed' }
end
if not changed() then return reply(OK, '', 0) end
return finish(OK, '', 0)
