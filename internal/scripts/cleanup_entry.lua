#!lua flags=allow-oom
-- cleanup_entry (DESIGN §A4.5; SPEC STA-3, STA-4, STA-5, STA-9, STA-10, N1): one atomic step
-- for the ix member ARGV[3], which re-checks every entry of its key. After the prelude's checks:
-- the entry gone or not yet due (skipped: nothing written, not even hwm, §A4.8 rule 8), a
-- member that fails is_enc (the integrity hold), the per-key KEYS (Go passes placeholders for
-- such a member, so it reaches the hold first, §A4.1), the quarantine flag (the entry moves past
-- this pass), check_key (a bad key is quarantined and its entry moved), then STA-4 eligibility:
-- the bucket removed with its markers and its entry, or its due markers removed (STA-3) and the
-- entry re-scored above t. Every list and value the writes need is built before the first write.
K.epoch, K.ctl, K.ix, K.q, K.k, K.mi = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6]
local member = ARGV[3]
local cap_units, rate_tokens, push_us = tonumber(ARGV[4]), tonumber(ARGV[5]), tonumber(ARGV[6])
local entered, refusal = enter(tonumber(ARGV[7]), tonumber(ARGV[8]))
if not entered then return refusal end
local t = T

-- split_markers_by_expiry splits the bucket's markers at budget time at: the fields (m:<g>:<s>)
-- and the mi members (<g>:<s>) of the markers due (expiry <= at, STA-3), and the expiries of
-- the rest.
local function split_markers_by_expiry(bucket, at)
  local due_fields, due_members, rest = {}, {}, {}
  for _, mk in ipairs(bucket.markers) do
    if mk.expiry <= at then
      due_fields[#due_fields + 1] = 'm:' .. mk.gs
      due_members[#due_members + 1] = mk.gs
    else
      rest[#rest + 1] = mk.expiry
    end
  end
  return due_fields, due_members, rest
end

-- first_expiry and last_expiry: the earliest and the latest of the expiries, nil for none.
local function first_expiry(expiries)
  local v = nil
  for _, x in ipairs(expiries) do
    if v == nil or x < v then v = x end
  end
  return v
end

local function last_expiry(expiries)
  local v = nil
  for _, x in ipairs(expiries) do
    if v == nil or x > v then v = x end
  end
  return v
end

local ok, sc = pcall(redis.call, 'ZSCORE', K.ix, member)
if not ok then return hold('shared_key_malformed') end
if not sc then return reply(E_SKIPPED, 'gone', 0) end -- another instance did it
if tonumber(sc) > t then return reply(E_SKIPPED, 'not_due', 0) end
if not is_enc(member) then return hold('unattributable_entry') end -- N1
if not assert_keys(member) then return { E_BAD_CALL, 0, 'keys_mismatch' } end
local qok, qv = pcall(redis.call, 'HEXISTS', K.q, member)
if not qok then return hold('shared_key_malformed') end
if qv == 1 then -- quarantined: never removed (STA-4); the entry moves past this pass
  redis.call('ZADD', K.ix, 'XX', t + push_us, member)
  return finish(E_MOVED, 'quarantined', 0)
end
local st, b = check_key(member, cap_units) -- §A3.3, reads only
if st == 'shared' then return hold('shared_key_malformed') end
if st ~= 'ok' then -- 'bad', and b is the reason ('absent' cannot occur: the ix member exists)
  redis.call('HSET', K.q, member, string.format('%s|%d', b, t))
  redis.call('ZADD', K.ix, 'XX', t + push_us, member) -- the entry kept, moved past this pass
  return finish(E_QUARANTINED, b, 0)
end
local u = refill(b.u, cap_units, t - b.t, rate_tokens)
local due_fields, due_members, rest = split_markers_by_expiry(b, t) -- built before any write
if u == cap_units and b.x <= t and #rest == 0 then -- STA-4 eligibility
  redis.call('DEL', K.k, K.mi) -- the bucket, its markers and their mi entries
  redis.call('ZREM', K.ix, member)
  dec_counter('nb', 1)
  dec_counter('nm', b.mc)
  return finish(E_REMOVED, '', b.mc)
end
-- Every value the writes need is computed before the first write.
local bucket_due = math.max(t + ceil_div(cap_units - u, rate_tokens * 1000), b.x, last_expiry(rest) or 0)
local new_score = math.min(bucket_due, first_expiry(rest) or bucket_due) -- always > t
-- ---- writes start here ----
hdel_chunked(K.k, due_fields) -- STA-3 for the markers already expired
zrem_chunked(K.mi, due_members)
if #due_fields > 0 then
  redis.call('HINCRBY', K.k, 'mc', -#due_fields)
  dec_counter('nm', #due_fields)
end
redis.call('ZADD', K.ix, 'XX', new_score, member)
return finish(E_MOVED, '', #due_fields)
