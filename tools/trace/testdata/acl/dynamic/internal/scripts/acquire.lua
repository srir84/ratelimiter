#!lua
local cmd = ARGV[1]
local a = redis.call(cmd, KEYS[1]) -- want: a variable command
local b = 'x' .. redis.call(cmd, KEYS[1]) -- want: the same after a concatenation
local c = redis.call('get' .. 'del', KEYS[1]) -- want: a literal that is only part of the command
local d = redis.call('GET' .. ARGV[1], KEYS[1]) -- want: a literal joined to an argument
local e = redis.pcall('HGET' .. 'ALL', KEYS[1]) -- want: redis.pcall too
local ok, f = pcall(redis.call, 'GET' .. ARGV[2], KEYS[1]) -- want: the pcall form too
local ok2, g = pcall(redis.pcall, 'GET', KEYS[1]) -- want: pcall of redis.pcall is not a named form
local r = redis -- want: redis as a value hides the calls made through it
local h = r.call(ARGV[1], KEYS[1])
local call = redis.call -- want: redis.call as a value
local i = call('FLUSHALL')
local j = redis['call']('GET', KEYS[1]) -- want: an index
local k = redis:call('GET', KEYS[1]) -- want: a method call
local l = redis.call 'TIME' -- want: a call without parentheses
local q = KEYS.pcall(redis.call, 'GET', KEYS[1]) -- want: only the global pcall makes the pcall form
local s = _G.redis.call('FLUSHALL') -- want: _G reaches the store API through a global's name
local u = server.call('GET', KEYS[1]) -- want: server is the API's Valkey name, in no named form
local w = loadstring('return redis.call("FLUSHALL")')() -- want: loadstring compiles a string at run time
local x = load(function() return nil end) -- want: load compiles the pieces a function returns
-- Named forms, a concatenation of two named calls, other redis functions, and redis,
-- server, _G, loadstring or load as a field or method name are not dynamic:
local m = redis.call('GET', KEYS[1]) .. redis.call('HGETALL', KEYS[2])
local n = redis.sha1hex(ARGV[1])
local o = ARGV.redis
local p = KEYS:redis()
local v = ARGV.server or KEYS._G or KEYS:server() or ARGV:_G()
local y = ARGV.loadstring or KEYS.load or KEYS:loadstring() or ARGV:load()
return { a, b, c, d, e, f, g, h, i, j, k, l, m, n, o, p, q, s, u, v, w, x, y, ok, ok2 }
