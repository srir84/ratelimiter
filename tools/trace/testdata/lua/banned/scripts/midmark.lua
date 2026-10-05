local t = { 1, 2 }
return unpack(t) -- lint:chunked(500) is not at the end
