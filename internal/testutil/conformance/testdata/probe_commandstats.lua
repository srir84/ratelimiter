#!lua
-- INFO commandstats: 1 HSET and 2 HGET per run (DESIGN §C2).
redis.call('HSET', KEYS[1], 'f', '1')
redis.call('HGET', KEYS[1], 'f')
return redis.call('HGET', KEYS[1], 'f')
