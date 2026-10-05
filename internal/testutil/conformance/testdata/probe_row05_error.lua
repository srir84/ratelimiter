#!lua
-- Row 5: a write, then a Lua error. The write is not rolled back.
redis.call('HSET', KEYS[1], 'f', '1')
error('probe error after a write')
