-- The shared prelude (DESIGN §A4.2, §A3.1, §A3.3). The loader sends each script's header line,
-- then this file, then the rest of the script. Helpers read only E (the epoch record), T (the
-- budget time), ENC (the budget key's enc) and K (the script's KEYS by name), besides their own
-- arguments; each script sets the K fields it has, from its §A4.1 KEYS order, before anything else.
local E, T, ENC, K = nil, nil, nil, {}

local TWO53 = 9007199254740992
local TBOUND = TWO53 - 8796093022208 -- 2^53 - 2^43 (§A3.1)
local CHUNK = 500 -- list arguments per command, at most (§A4.8 rule 3)

-- Reply codes (§A4.9).
local E_PAGE = 0 -- a due_entries page
local HEALTHY = 0 -- read_epoch's state when the epoch is healthy
local E_ALLOWED = 1
local E_LIMITED = 2
local E_APPLIED = 3
local E_ALREADY_APPLIED = 4
local E_RECEIPT_INVALID = 10
local E_RECEIPT_EXPIRED = 11
local E_CONFIG = 20
local E_CLOCK = 21
local E_INTEGRITY_EPOCH = 22
local E_INTEGRITY_KEY = 23
local E_CAPACITY = 24
local E_BAD_CALL = 25
local E_REMOVED = 30
local E_MOVED = 31
local E_QUARANTINED = 32
local E_SKIPPED = 33
local OK = 40
local REFUSED = 41

-- is_uint53: 1-16 decimal digits, no leading zero unless "0", value below 2^53 (§A4.2).
local function is_uint53(s)
  if type(s) ~= 'string' or #s > 16 or not string.find(s, '^%d+$') then return false end
  if #s > 1 and string.sub(s, 1, 1) == '0' then return false end
  return tonumber(s) < TWO53
end

-- is_hex32: 32 lowercase hex characters (§A4.2).
local function is_hex32(s)
  return type(s) == 'string' and #s == 32 and string.find(s, '^[0-9a-f]+$') ~= nil
end

-- is_score: read as a number, an integer in [0, 2^53), whatever the store's formatting (§A4.2).
local function is_score(s)
  local n = tonumber(s)
  return n ~= nil and n >= 0 and n < TWO53 and n == math.floor(n)
end

-- is_enc: 1-342 characters of [A-Za-z0-9_-], length mod 4 ~= 1, and canonical: decoding then
-- re-encoding gives the same string, so the bits the last character carries past the encoded
-- bytes are zero (§A3.2; Go internal/keys.IsEnc).
local B64URL = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_'
local function is_enc(s)
  if type(s) ~= 'string' or #s < 1 or #s > 342 or #s % 4 == 1 then return false end
  if not string.find(s, '^[A-Za-z0-9_%-]+$') then return false end
  local v = string.find(B64URL, string.sub(s, -1), 1, true) - 1
  if #s % 4 == 2 then return v % 16 == 0 end
  if #s % 4 == 3 then return v % 4 == 0 end
  return true
end

-- to_map turns a HGETALL reply into a field -> value table.
local function to_map(raw)
  local m = {}
  for i = 1, #raw, 2 do
    m[raw[i]] = raw[i + 1]
  end
  return m
end

-- record_wellformed: the epoch record minus hwm and the hold's value (§A4.2).
local function record_wellformed(e)
  return is_hex32(e.id)
    and is_hex32(e.fp)
    and e.schema == '1'
    and (e.clock == 'store' or e.clock == 'controlled')
    and is_uint53(e.gen)
    and is_uint53(e.nb)
    and is_uint53(e.nm)
    and is_uint53(e.origin)
    and is_uint53(e.created)
    and e.hold ~= nil
end

-- load_epoch never writes: the record, or nil and the epoch-wide reason (§A4.2).
local function load_epoch()
  local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
  if not ok then return nil, 'epoch_record_malformed' end
  if #raw == 0 then return nil, 'epoch_record_missing' end
  local e = to_map(raw)
  if not record_wellformed(e) then return nil, 'epoch_record_malformed' end
  if e.hwm == nil then return nil, 'hwm_missing' end
  if not is_uint53(e.hwm) or tonumber(e.hwm) >= TBOUND then return nil, 'hwm_malformed' end
  if e.hold ~= '' then return nil, 'integrity_hold' end
  return e
end

-- read_clock: the store's clock or the controlled time, in µs; nil -> clock_error (CLK-1, CLK-2,
-- CLK-8).
local function read_clock(e)
  local v
  if e.clock == 'controlled' then
    local ok, s = pcall(redis.call, 'GET', K.ctl)
    if not ok or not is_uint53(s) then return nil end
    v = tonumber(s)
  else
    local tm = redis.call('TIME')
    v = tonumber(tm[1]) * 1000000 + tonumber(tm[2])
  end
  if v >= TBOUND then return nil end
  return v
end

-- refill: exact, with no overflow and no rounding; all arguments are integers below 2^53 (§A3.1).
local function refill(u, cap, elapsed_us, rate)
  if elapsed_us <= 0 or u >= cap then return u end
  local missing = cap - u -- < 2^52
  local ns = elapsed_us * 1000 -- exact if the true value < 2^53
  if ns >= TWO53 or ns >= missing then return cap end -- true product >= 2^53 > missing
  local add = ns * rate
  if add >= TWO53 or add >= missing then return cap end
  return u + add
end

-- ceil_div: a >= 0, b > 0, a + b < 2^53 (§A3.1).
local function ceil_div(a, b)
  local q = math.floor(a / b)
  while q * b < a do
    q = q + 1
  end
  while q > 0 and (q - 1) * b >= a do
    q = q - 1
  end
  return q
end

-- write_hwm stores the budget time when it is later than the high-water mark (CLK-4).
local function write_hwm()
  if T > tonumber(E.hwm) then redis.call('HSET', K.epoch, 'hwm', T) end
end

-- finish stores the high-water mark, then replies; reply writes nothing (§A4.8 rule 8).
local function finish(code, reason, ...)
  write_hwm()
  return { code, T, reason, ... }
end

local function reply(code, reason, ...)
  return { code, T, reason, ... }
end

-- quarantine flags the key (STA-10) and stores the high-water mark.
local function quarantine(reason)
  redis.call('HSET', K.q, ENC, string.format('%s|%d', reason, T))
  write_hwm()
  return { E_INTEGRITY_KEY, T, reason }
end

-- hold sets the integrity hold (STA-9); the high-water mark is not advanced.
local function hold(reason)
  redis.call('HSET', K.epoch, 'hold', reason)
  return { E_INTEGRITY_EPOCH, 0, 'integrity_hold' }
end

-- dec_counter takes n off the record's counter f, never below 0 (§A3.3). A decrement of 0
-- (n = 0 or E[f] = 0, sent as '0' even when it is Lua's -0) succeeds on the pinned store.
local function dec_counter(f, n)
  local d = math.min(n, tonumber(E[f]))
  redis.call('HINCRBY', K.epoch, f, -d)
  E[f] = string.format('%d', tonumber(E[f]) - d)
end

-- hdel_chunked deletes the fields in list from the hash key, one HDEL per CHUNK fields at most
-- (§A4.8 rule 3, §A3.0 row 11). The caller builds list before its first write; an empty list
-- sends nothing.
local function hdel_chunked(key, list)
  for i = 1, #list, CHUNK do
    redis.call('HDEL', key, unpack(list, i, math.min(i + CHUNK - 1, #list))) -- lint:chunked(500)
  end
end

-- zrem_chunked removes the members in list from the sorted set key, one ZREM per CHUNK members
-- at most, as hdel_chunked does.
local function zrem_chunked(key, list)
  for i = 1, #list, CHUNK do
    redis.call('ZREM', key, unpack(list, i, math.min(i + CHUNK - 1, #list))) -- lint:chunked(500)
  end
end

-- assert_keys: the per-key KEYS, and the shared ix and q, belong to the epoch and enc (§A4.1).
local function assert_keys(enc)
  local ns = 'rl:e:' .. E.id .. ':'
  return K.ix == ns .. 'ix' and K.q == ns .. 'q' and K.k == ns .. 'k:' .. enc and K.mi == ns .. 'mi:' .. enc
end

-- check_key (§A3.3): the three reads first, then the rows top to bottom; the first match
-- decides. It returns 'absent'; 'bad' and a quarantine reason; 'shared' (the integrity hold
-- shared_key_malformed); or 'ok', the bucket and its ix score. The bucket holds the fields u, t,
-- g, s, x and mc as numbers, markers (each {gs, expiry, credit}, in field order), marker_at
-- (gs -> marker) and first_marker_expiry (nil without markers).
local function check_key(enc, cap_units)
  local kr = redis.pcall('HGETALL', K.k)
  local mr = redis.pcall('ZRANGE', K.mi, 0, -1, 'WITHSCORES')
  local zs = redis.pcall('ZSCORE', K.ix, enc)
  local k_err, mi_err, ix_err = kr.err ~= nil, mr.err ~= nil, type(zs) == 'table'
  local mi_exists = mi_err or #mr > 0
  if not k_err and #kr == 0 then
    if not mi_exists and not ix_err and not zs then return 'absent' end
    if mi_exists then return 'bad', 'marker_index_without_bucket' end
    if not ix_err then return 'bad', 'index_without_bucket' end
    return 'shared'
  end
  if k_err then return 'bad', 'malformed_bucket' end
  local fields, marks = {}, {}
  for i = 1, #kr, 2 do
    local f = kr[i]
    if f == 'u' or f == 't' or f == 'g' or f == 's' or f == 'x' or f == 'mc' then
      fields[#fields + 1] = f
    elseif string.sub(f, 1, 2) == 'm:' then
      marks[#marks + 1] = i
    else
      return 'bad', 'malformed_bucket'
    end
  end
  if #fields == 0 then return 'bad', 'marker_without_bucket' end
  if #fields < 6 then return 'bad', 'malformed_bucket' end
  local raw = to_map(kr)
  local b = { markers = {}, marker_at = {} }
  for _, f in ipairs({ 'u', 't', 'g', 's', 'x', 'mc' }) do
    if not is_uint53(raw[f]) then return 'bad', 'malformed_bucket' end
    b[f] = tonumber(raw[f])
  end
  if b.u > cap_units then return 'bad', 'malformed_bucket' end
  -- g < 1 is out of range too (owner decision at S4's §5 stop): gen is incremented before
  -- use, so only tampering leaves g = 0, and an allowed reply needs g >= 1 (§A4.9).
  if b.g < 1 then return 'bad', 'malformed_bucket' end
  for _, i in ipairs(marks) do
    local g, s = string.match(kr[i], '^m:(%d+):(%d+)$')
    local expiry, credit = string.match(kr[i + 1], '^(%d+):(%d+)$')
    if not (is_uint53(g) and is_uint53(s) and is_uint53(expiry) and is_uint53(credit)) then
      return 'bad', 'malformed_marker'
    end
    if tonumber(g) ~= b.g then return 'bad', 'malformed_marker' end
    local m = { gs = g .. ':' .. s, expiry = tonumber(expiry), credit = tonumber(credit) }
    b.markers[#b.markers + 1] = m
    b.marker_at[m.gs] = m
    if b.first_marker_expiry == nil or m.expiry < b.first_marker_expiry then b.first_marker_expiry = m.expiry end
  end
  if #marks ~= b.mc then return 'bad', 'marker_count' end
  if mi_err then return 'bad', 'marker_index_mismatch' end
  local members = 0
  for i = 1, #mr, 2 do
    local g, s = string.match(mr[i], '^(%d+):(%d+)$')
    if not (is_uint53(g) and is_uint53(s)) then return 'bad', 'marker_index_mismatch' end
    local m = b.marker_at[mr[i]]
    if m == nil or tonumber(mr[i + 1]) ~= m.expiry then return 'bad', 'marker_index_mismatch' end
    members = members + 1
  end
  if members ~= #marks then return 'bad', 'marker_index_mismatch' end
  if not ix_err and not zs then return 'bad', 'bucket_without_index' end
  if ix_err then return 'shared' end
  if not is_score(zs) then return 'bad', 'malformed_index' end
  return 'ok', b, tonumber(zs)
end

-- enter runs the prelude steps in order (API-1a steps 1 and 2): the record, its id and fp,
-- the per-key KEYS when enc is given (right after the epoch check, §A4.1), the floors, the
-- clock. It sets E and T and returns true, or false and a finished reply.
local function enter(floor_hwm, floor_gen, enc)
  local e, why = load_epoch()
  if not e then return false, { E_INTEGRITY_EPOCH, 0, why } end
  if e.id ~= ARGV[1] or e.fp ~= ARGV[2] then return false, { E_CONFIG, 0, 'epoch_or_fingerprint' } end
  E = e
  if enc ~= nil and not assert_keys(enc) then return false, { E_BAD_CALL, 0, 'keys_mismatch' } end
  if tonumber(e.hwm) < floor_hwm or tonumber(e.gen) < floor_gen then return false, hold('store_rollback') end
  local now = read_clock(e)
  if not now then return false, { E_CLOCK, 0, 'clock' } end
  T = math.max(now, tonumber(e.hwm)) -- budget time (CLK-4)
  return true
end
