#!lua
return 0xE-redis.call('INCR', KEYS[1])
