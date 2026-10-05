#!lua
local e = redis.call('HGETALL', KEYS[1])
local t = redis.call('TIME')
local ok, ix = pcall(redis.call, 'ZSCORE', KEYS[3], ARGV[3])
local q = redis.pcall("HEXISTS", KEYS[4], ARGV[3])
-- redis.call(cmd) in a comment is not a call
return { 1, t[1], '', #e, ok and ix or 0, q }
