-- A fixture prelude (TestReplies, sources): the base prelude and a probe. A helper's own
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

-- A prelude site through a source with a literal reason: some script must declare the source
-- with a row of that reason for every code it holds.
local function probe()
  local st, gone = OK, REFUSED
  if E then return reply(gone, 'epoch_changed') end -- hold declares gone, with 41 only
  return reply(st, 'epoch_changed') -- want: hold's st holds 40 too
end
