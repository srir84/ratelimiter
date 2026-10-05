#!lua flags=no-writes
-- Row 16 (BUSY): spins without writing for ARGV[1] seconds of CPU time. rl_fixture has
-- no TIME, so the loop reads os.clock.
local start = os.clock()
local spins = 0
while os.clock() - start < tonumber(ARGV[1]) do
  spins = spins + 1
end
return spins
