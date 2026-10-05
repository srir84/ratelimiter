#!lua flags=allow-oom
-- probe_write with allow-oom, like the maintenance scripts (DESIGN §A4.8 rule 1).
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
return redis.call('HINCRBY', KEYS[1], 'n', 1)
