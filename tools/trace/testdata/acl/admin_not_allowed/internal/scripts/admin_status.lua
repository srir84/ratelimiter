#!lua flags=no-writes
local n = redis.call('UNLINK', KEYS[1])
return { 1, n }
