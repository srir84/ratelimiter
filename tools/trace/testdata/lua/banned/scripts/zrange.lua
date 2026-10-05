return redis.call('zrangebyscore', KEYS[1], 0, 1)
