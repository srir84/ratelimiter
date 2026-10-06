#!lua flags=no-writes
-- admin_inspect (DESIGN §A4.6, §A4.9; SPEC ADM-4): one key's state, read-only. The record
-- checks of admin_status, then the KEYS, then every read pcall'd: a wrong-type ix or q is a
-- refusal (§A4.8 rule 2); a wrong-type k gives present = 2 and a wrong-type mi n = 0; a
-- missing or unreadable bucket field, or one that fails is_uint53, is ''; so is an ix score or
-- marker expiry that fails is_score. Expiries come from mi, ascending.
K.epoch, K.ix, K.q, K.k, K.mi = KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5]
ENC = ARGV[2]
local enc = ENC
local ok, raw = pcall(redis.call, 'HGETALL', K.epoch)
if ok and #raw == 0 then return { REFUSED, 0, 'epoch_record_missing' } end
if not ok or not record_wellformed(to_map(raw)) then return { REFUSED, 0, 'epoch_record_malformed' } end
E = to_map(raw)
if E.id ~= ARGV[1] then return { REFUSED, 0, 'epoch_changed' } end
if not assert_keys(enc) then return { E_BAD_CALL, 0, 'keys_mismatch' } end

local kr = redis.pcall('HGETALL', K.k)
local mr = redis.pcall('ZRANGE', K.mi, 0, -1, 'WITHSCORES')
local zs = redis.pcall('ZSCORE', K.ix, enc)
local qv = redis.pcall('HGET', K.q, enc)
if type(zs) == 'table' or type(qv) == 'table' then return { REFUSED, 0, 'shared_key_malformed' } end

local present, bucket = 0, {}
if kr.err ~= nil then
  present = 2
elseif #kr > 0 then
  present, bucket = 1, to_map(kr)
end
local out = { OK, 0, '', present }
for _, f in ipairs({ 'u', 't', 'g', 's', 'x', 'mc' }) do
  out[#out + 1] = is_uint53(bucket[f]) and tonumber(bucket[f]) or ''
end
out[#out + 1] = is_score(zs) and tonumber(zs) or ''
out[#out + 1] = qv or ''
local expiries = {}
if mr.err == nil then
  for i = 2, #mr, 2 do
    expiries[#expiries + 1] = is_score(mr[i]) and tonumber(mr[i]) or ''
  end
end
out[#out + 1] = #expiries
for _, v in ipairs(expiries) do
  out[#out + 1] = v
end
return out
