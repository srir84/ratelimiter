#!lua flags=no-writes
local id = redis.call('HGET', KEYS[1], 'id')
local n = redis.call('UNLINK', KEYS[2])
return { 1, id, n }
