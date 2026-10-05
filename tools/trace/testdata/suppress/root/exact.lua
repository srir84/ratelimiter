-- Directives exactly as the tools honour them, so no marker fits on their lines: the
-- test lists them. stylua honours a comment line that is exactly its directive, with
-- Unicode white space trimmed, wherever it sits in a long comment of any level:
--[[
stylua: ignore
]]
local a = { 1, 2 }
--[==[ prose first,
  stylua: ignore
then prose ]==]
local b = { 3, 4 }
-- stylua: ignore
local c = { 5, 6 }
--stylua: ignore
local d = { 7, 8 }
-- luacheck honours a short comment whose text starts with its directive once ASCII
-- white space, the vertical tab among it, is trimmed:
--luacheck: ignore
-- A numeral takes a sign only right after its leading digits' e or E, never after
-- 0x…, so the comment starts at the first dash after 0xe:
local n = 0xe--luacheck: ignore
return a, b, c, d, n
