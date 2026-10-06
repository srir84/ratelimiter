#!lua flags=allow-oom
-- S4 repair F1, shape 4: reply, finish, quarantine and hold are read only at a call with
-- parentheses; any other use is a reply the check cannot read.
local refuse = reply -- want: an alias of reply
local done = finish -- want: an alias of finish
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if not ok then return refuse(REFUSED, 'not_a_design_reason') end
if #raw == 0 then return done(OK, 'not_a_design_reason') end
local helpers = { flag = quarantine } -- want: a value in a table
local function call(f, ...)
  return f(...)
end
if raw[1] == '' then return call(hold, 'store_rollback') end -- want: passed as an argument
if raw[1] == 'x' then return reply 'epoch_changed' end -- want: called with a string
if raw[1] == 'y' then return hold { 'store_rollback' } end -- want: called with a table
local alias, other = 1, nil
alias, other = 2, hold -- want: assigned in a list
-- A field or a table key of the same name, and a definition, are none of these.
local t = { reply = 1, hold = 2 }
t.finish = t.reply
helpers.quarantine = nil
return { OK, 0, '', t, helpers, alias, other }
