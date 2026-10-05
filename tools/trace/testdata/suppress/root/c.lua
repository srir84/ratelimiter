local a = 1 --luacheck: ignore want
local b = 2 -- luacheck: no unused want
-- stylua: ignore want
local c = 3 --- LUACHECK: ignore want
--[[ stylua: ignore ]] -- want
--[==[luacheck: ignore]==] -- want
--[[STYLUA:ignore]] local d = 4 -- want
-- lint:chunked(500) and prose about luacheck pass
return a + b + c + d
