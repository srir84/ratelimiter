local OK = 40
local REFUSED = 41

local function reply(code, reason)
  return { code, 0, reason }
end

local function finish(code, reason)
  return { code, 0, reason }
end
