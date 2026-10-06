#!lua flags=allow-oom
-- A table that closes with the wrong bracket, then a call that never closes.
local t = { OK, 0, '' ) -- want: the table cannot be read
return reply(OK, '' -- want: the arguments cannot be read
end -- a stray end: the check reads on
