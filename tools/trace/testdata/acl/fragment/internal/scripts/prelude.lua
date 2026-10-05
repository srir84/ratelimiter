local function drop(key)
  return redis.call('UNLINK', key)
end
