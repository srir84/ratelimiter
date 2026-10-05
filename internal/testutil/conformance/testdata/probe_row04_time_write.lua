#!lua
-- Row 4: TIME inside a writing script.
local t = redis.call('TIME')
redis.call('HSET', KEYS[1], 'sec', t[1])
return t
