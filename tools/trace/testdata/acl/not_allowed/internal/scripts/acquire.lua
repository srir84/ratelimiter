#!lua
local v = redis.call('HGET', KEYS[1], 'u')
return { 1, 0, '', v }
