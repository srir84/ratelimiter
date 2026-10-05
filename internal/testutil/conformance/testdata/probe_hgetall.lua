#!lua flags=no-writes
-- Row 15 (crash): reads back one writer's hash.
return redis.call('HGETALL', KEYS[1])
