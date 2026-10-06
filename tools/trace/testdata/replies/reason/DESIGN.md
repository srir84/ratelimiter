# Fixture design (TestReplies, reasons)

`shared_key_malformed` is backticked here, before any section the check reads, so it counts nowhere.

## A3. Data model

### A3.3 Key checks (`check_key`)

A shared key of the wrong type sets the hold shared_key_malformed (a bare word, not backticked).

### A3.4 A sibling of §A3.3

A heading at §A3.3's level ends it, so `epoch_record_malformed` does not count here.

## A4. Scripts

`shared_key_malformed` again, between the sections.

### A4.1 Inventory

Nothing for the fixture.

### A4.2 Common prelude

```lua
local function load_epoch()
  local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
  if #raw == 0 then return nil, 'epoch_record_missing' end
  return raw
end
--   hold(reason): HSET epoch hold reason; return {22, 0, 'integrity_hold'}
```

```text
A block that is not Lua: 'epoch_record_malformed' and 'shared_key_malformed' do not count here.
```lua is no closing fence: a closing fence holds only the fence characters.
```

### A4.3 `acquire.lua`

Two spans that are not one token: `shared_key_malformed hold` and `x.shared_key_malformed`; and a
longer token, `shared_key_malformed_x`.

### A4.4 `refund.lua`

Nothing for the fixture.

### A4.5 Cleanup

Nothing for the fixture.

### A4.6 Admin scripts

| Script | Steps |
|---|---|
| `admin_status` | Read the record raw: empty → `{41, 0, 'epoch_record_missing'}`; else epoch_changed |

A run of two backticks closes no single-backtick span: `epoch_record_malformed``x`.

### A4.8 Rules for script authors

Section A4.7 is missing from this fixture.

### A4.9 Reply layouts and result codes

| Code | Name | Reply | Library result |
|---|---|---|---|
| 22 | `E_INTEGRITY_EPOCH` | `{22, 0, reason}` | (`hwm_missing`, `hwm_malformed`) |
| 40 / 41 | `hold` | `{40, 0, '', 0}` or `{41, 0, 'epoch_changed'}` | rollback check |

## A5. Library internals

### A5.1 Packages

`shared_key_malformed` and `epoch_record_malformed` are out of the sections the check reads.

```lua
-- A Lua block out of the sections: 'shared_key_malformed' does not count here.
local reason = 'epoch_record_malformed'
```
