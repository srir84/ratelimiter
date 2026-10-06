#!lua
local n = 1
local order = 'GT'
if n == 1 then
  return reply(OK, '')
end
redis.call('HSET', KEYS[1], 'f', order)
if n == 2 then redis.call('DEL', KEYS[1])
end
pcall(redis.call, 'HSET', KEYS[1], 'p', 1)
redis.pcall('HDEL', KEYS[1], 'q')
redis.call('ZADD', KEYS[2], 'XX', 1, 'm')
write_hwm()
return finish(REFUSED, 'no')
