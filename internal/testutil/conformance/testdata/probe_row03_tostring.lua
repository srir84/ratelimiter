#!lua flags=no-writes
-- Row 3: tostring loses digits of a 16-digit number; string.format('%d', n) does not.
local n = 1760000000000123
return { tostring(n), string.format('%d', n) }
