-- A fixture prelude (TestReplies): the reply code names and the helpers. A helper's own
-- body is a template, read at its calls; hold's reply is a literal, which read_epoch lists.
local E, T = nil, nil

local TWO53 = 9007199254740992
local TBOUND = TWO53 - 8796093022208
local HEALTHY = 0
local E_INTEGRITY_EPOCH = 22
local E_INTEGRITY_KEY = 23
local OK = 40
local REFUSED = 41

local function reply(code, reason, ...)
  return { code, T, reason, ... }
end

local function finish(code, reason, ...)
  return { code, T, reason, ... }
end

local function quarantine(reason)
  return { E_INTEGRITY_KEY, T, reason }
end

local function hold(reason)
  E.hold = reason
  return { E_INTEGRITY_EPOCH, 0, 'integrity_hold' }
end

-- The checker's data entries (replyData): to_map's m and check_key's b are tables a return gives
-- as data, built empty or from name = value fields, then filled in by index.
local function to_map(raw)
  local m = {}
  for i = 1, #raw, 2 do
    m[raw[i]] = raw[i + 1]
  end
  return m
end

local function check_key(kr)
  local b = { markers = {}, marker_at = {} }
  for _, f in ipairs({ 'u', 't' }) do
    b[f] = tonumber(kr[f])
  end
  return 'ok', b, to_map(kr)
end
