-- A fixture prelude (TestReplies, index_built): the base prelude's helpers and data entries,
-- and check_key again, whose b now holds a table with a code first, which the entry never exempts.
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

local function to_map(raw)
  local m = {}
  for i = 1, #raw, 2 do
    m[raw[i]] = raw[i + 1]
  end
  return m
end

local function check_key(kr)
  local b = { markers = {}, marker_at = {} }
  if kr then
    local code = OK
    b = { code, 0, 'not_a_design_reason' } -- want: the entry exempts only b's empty and name = value tables
  end
  return 'ok', b, to_map(kr)
end
