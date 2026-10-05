#!lua flags=allow-oom
-- One UNLINK per KEYS entry, like reclaim (DESIGN §A4.7). Returns the number removed.
local n = 0
for i = 1, #KEYS do
  n = n + redis.call('UNLINK', KEYS[i])
end
return n
