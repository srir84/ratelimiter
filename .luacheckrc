-- luacheck for the Valkey scripts (Lua 5.1 sandbox). Rules: AGENTS.md §3, DESIGN §A4.8.
-- tools/trace lints each script as the store receives it (header + prelude + body).
std = "lua51"
read_globals = { "KEYS", "ARGV", "redis" }
-- Missing from the Valkey sandbox, or banned here: a Lua error after a write leaves a
-- partial write (no rollback), and tostring(n) can print a number in exponent form.
not_globals = {
  "print", "require", "module", "dofile", "loadfile", "loadstring", "load",
  "setfenv", "getfenv", "setmetatable", "getmetatable", "rawset", "rawget", "rawequal",
  "tostring", "error", "assert", "xpcall", "collectgarbage", "newproxy", "gcinfo",
  "os", "io", "debug", "coroutine", "package",
}
max_line_length = 120
