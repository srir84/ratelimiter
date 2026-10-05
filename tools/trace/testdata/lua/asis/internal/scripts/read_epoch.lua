#!lua flags=no-writes
local value = redis.call('GET', KEYS[1])
leaked = value
return value
