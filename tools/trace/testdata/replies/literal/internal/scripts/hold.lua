#!lua flags=allow-oom
-- Each marked line gives a reply that hold's rows lack, or one the check cannot read.
local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
if not ok then return { REFUSED, 0, 'epoch_changed' } end
if #raw == 0 then return { REFUSED, 0, 'epoch_gone' } end -- want: a reason hold's rows lack
if raw[1] == '' then return reply(OK, 'done') end -- want
if raw[1] == 'a' then return finish(E_INTEGRITY_KEY, 'quarantined') end -- want: a code hold's rows lack
if raw[1] == 'b' then return quarantine('malformed_bucket') end -- want
if raw[1] == 'c' then return hold('store_rollback') end -- want: hold's reply is 22 integrity_hold
if raw[1] == 'd' then return quarantine(raw) end -- want: the table declares no raw for hold
if raw[1] == 'e' then return reply(OK + 1, '') end -- want: a code it cannot read
if raw[1] == 'f' then return reply(OK, raw[2]) end -- want: a reason it cannot read
if raw[1] == 'g' then return { 7, 0, '' } end -- want: an integer code
if raw[1] == 'h' then return { E_UNKNOWN, 0, '' } end -- want: a name that is no constant
if raw[1] == 'i' then return reply(OK) end -- want: no reason
if raw[1] == 'j' then return hold() end -- want: no reason for the hold
if raw[1] == 'k' then return { REFUSED; 0; 'epoch_semicolon' } end -- want: ';' separates fields too
if raw[1] == 'l' then return reply(OK, nil) end -- want: nil is no reason
if raw[1] == 'm' then return reply('40', '') end -- want: a string is no code
local function refuse()
  return false, { REFUSED, 0, 'epoch_missing' } -- want
end
local short = { OK, 0 } -- want: a code name starts it, but it has no reason
local out = { code_name, 0, '' } -- want: returned below, with a code the table declares no set for
-- Inside a function, a table a return gives with an integer first is a reply too.
local function gone()
  return { 41, 0, 'not_a_design_reason' } -- want: a reason hold's rows lack
end
local function gone_later()
  local r = { 41, 0, 'gone_later_reason' } -- want: the function returns r
  return r
end
-- Only the prelude's reply, finish, quarantine and hold build replies from their parameters.
local function refuse_with(why)
  return { REFUSED, 0, why } -- want: built from refuse_with's parameter why
end
local function refuse_code(code)
  return reply(code, 'epoch_changed') -- want: built from refuse_code's parameter code
end
local function vanished(code)
  return false, { code, 0, 'epoch_vanished' } -- want: a parameter first
end
local function quarantine(reason)
  return { E_INTEGRITY_KEY, 0, reason } -- want: a script's own quarantine is no template
end
out[#out + 1] = refuse
out[#out + 1] = short
return out;
