-- A fixture prelude whose enter replies as only acquire may, and acquire.lua is absent here.
local E, T = nil, nil

local HEALTHY = 0
local E_BAD_CALL = 25
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

local function enter()
  local why = 'epoch_record_missing'
  if E == nil then return false, { E_BAD_CALL, 0, 'keys_mismatch' } end -- want: only acquire lists it
  if T == nil then return false, { E_INTEGRITY_EPOCH, 0, why } end -- want: only acquire declares why
  return true
end

local function read(state)
  local held = state
  if held then return false, { E_INTEGRITY_KEY, 0, held } end -- want: no script declares held
  return false, { E_INTEGRITY_KEY, 0, state } -- want: built from read's parameter state, and read is no template
end

local function code_of(state)
  return state
end

local function quarantined()
  local state = code_of(0)
  return { E_INTEGRITY_KEY, 0, state } -- want: read_epoch declares state with codes 0 and 22, not 23
end

-- A prelude literal must be one row of some script: the code and the reason together.
local function gone()
  if E then return false, { E_INTEGRITY_EPOCH, 0, 'hwm_gone' } end -- want: read_epoch has code 22, no script this reason
  return false, { E_INTEGRITY_KEY, 0, 'epoch_changed' } -- want: hold has this reason, no script code 23 with it
end

local function relabel()
  local state = code_of(22)
  return reply(state, 'hwm_missing') -- want: read_epoch's state holds 0 too, and 0 'hwm_missing' is no row
end
