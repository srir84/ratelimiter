#!lua
-- The shape of acquire (DESIGN §A4.1): KEYS epoch, ctl, ix, q, k:<enc>, mi:<enc>; ARGV
-- epoch_id, fp, enc, cap_units, rate_period_ns, rate_tokens, lifetime_us, max_tracked,
-- floor_hwm, floor_gen. It writes the bucket, the key's ix member and a counter.
redis.call('HSET', KEYS[5], 'u', ARGV[4], 't', ARGV[9], 'g', ARGV[10], 's', ARGV[6], 'x', ARGV[7], 'mc', '0')
redis.call('ZADD', KEYS[3], ARGV[9], ARGV[3])
return redis.call('HINCRBY', KEYS[1], 'n', 1)
