#!lua
local id = 'e:' .. redis.call('HGET', KEYS[1], 'id')
return { 1, 0, '', id }
