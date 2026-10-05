#!lua flags=no-writes
-- A read-only script (DESIGN §A4.8 rule 1). Returns whether KEYS[1] has field ARGV[1].
return redis.call('HEXISTS', KEYS[1], ARGV[1])
