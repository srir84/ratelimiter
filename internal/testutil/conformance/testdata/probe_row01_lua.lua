#!lua flags=no-writes
-- Row 1: the Lua version, and whether 2^53 + 1 rounds to 2^53 (IEEE doubles).
return { _VERSION, (2 ^ 53 + 1 == 2 ^ 53) and 1 or 0 }
