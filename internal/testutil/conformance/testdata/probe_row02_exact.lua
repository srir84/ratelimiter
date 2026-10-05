#!lua
-- Row 2: ARGV[1] as a number passed to redis.call (HSET value, ZADD score), returned as
-- an integer reply, and read back as stored. KEYS[1] hash, KEYS[2] sorted set.
local n = tonumber(ARGV[1])
redis.call('HSET', KEYS[1], 'n', n)
redis.call('ZADD', KEYS[2], n, 'm')
local h = redis.call('HGETALL', KEYS[1])
return { n, h[2], redis.call('ZSCORE', KEYS[2], 'm') }
