#!lua
-- Row 11: HDEL of ARGV[1] fields f1..fN through unpack.
local fields = {}
for i = 1, tonumber(ARGV[1]) do
  fields[i] = string.format('f%d', i)
end
return redis.call('HDEL', KEYS[1], unpack(fields))
