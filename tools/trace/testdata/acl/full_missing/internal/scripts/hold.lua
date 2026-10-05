#!lua flags=allow-oom
return redis.call('GET', KEYS[1])
