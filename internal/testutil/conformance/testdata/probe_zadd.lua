#!lua
-- ZADD [ARGV[1] flag] ARGV[2] score ARGV[3] member on KEYS[1]; returns the member's score
-- afterwards (nil when absent).
if ARGV[1] == '' then
  redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
else
  redis.call('ZADD', KEYS[1], ARGV[1], ARGV[2], ARGV[3])
end
return redis.call('ZSCORE', KEYS[1], ARGV[3])
