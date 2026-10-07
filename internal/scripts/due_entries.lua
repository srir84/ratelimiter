#!lua flags=allow-oom
-- due_entries (DESIGN §A4.5; SPEC STA-5, CLK-4): one page of the cleanup index for the pass
-- procedure. After the prelude's checks, the members of ix whose score is at most
-- min(T0, cutoff) (cutoff 0 = none), at most limit of them in score order, T0 being this call's
-- budget time. The page stores T0 as the high-water mark, so every later step of the pass has
-- t >= T0 >= the pass's cutoff. The reply drops the scores but the oldest, which is 0 when it
-- fails is_score, so a tampered score cannot fail the §A4.9 range check and stall the pass.
K.epoch, K.ctl, K.ix = KEYS[1], KEYS[2], KEYS[3]
local limit, cutoff = tonumber(ARGV[3]), tonumber(ARGV[4])
local entered, refusal = enter(tonumber(ARGV[5]), tonumber(ARGV[6]))
if not entered then return refusal end
local t = T

local hi = t -- T0 of this call
if cutoff > 0 and cutoff < hi then hi = cutoff end
local ok, m = pcall(redis.call, 'ZRANGE', K.ix, '-inf', hi, 'BYSCORE', 'LIMIT', 0, limit, 'WITHSCORES')
if not ok then return hold('shared_key_malformed') end
local oldest = 0
if #m > 0 and is_score(m[2]) then oldest = tonumber(m[2]) end
local page = { E_PAGE, t, '', oldest } -- the members follow, all before the write (§A4.8 rule 3)
for i = 1, #m, 2 do
  page[#page + 1] = m[i]
end
write_hwm()
return page
