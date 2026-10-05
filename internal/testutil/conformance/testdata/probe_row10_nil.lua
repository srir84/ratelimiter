#!lua flags=no-writes
-- Row 10: a table with a nil converted to a reply stops at the nil.
return { 1, nil, 3 }
