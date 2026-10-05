#!lua flags=no-writes
-- Row 4: TIME inside a no-writes script.
return redis.call('TIME')
