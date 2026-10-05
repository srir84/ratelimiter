#!lua flags=no-writes
-- Row 12: HGET, which rl_lib's ACL list does not include (DESIGN §A2).
return redis.call('HGET', KEYS[1], 'f')
