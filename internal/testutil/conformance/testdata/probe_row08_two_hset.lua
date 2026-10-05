#!lua
-- Row 8: two writes, which the AOF wraps in MULTI ... EXEC.
redis.call('HSET', KEYS[1], 'a', '1')
redis.call('HSET', KEYS[1], 'b', '2')
return 1
