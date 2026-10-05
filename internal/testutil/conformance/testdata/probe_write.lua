#!lua
-- A writing script like a limiter step (DESIGN §A4.8 rule 1: plain #!lua, refused
-- before it runs under OOM). KEYS[1] hash; ARGV[1] field, ARGV[2] value.
-- Returns the hash's write count.
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
return redis.call('HINCRBY', KEYS[1], 'n', 1)
