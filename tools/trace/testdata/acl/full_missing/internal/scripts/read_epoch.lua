#!lua flags=no-writes
return redis.call('GET', KEYS[1])
