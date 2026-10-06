# Fixture design (TestReplies)

The reasons of the fixture scripts' rows, each in one of the forms the check accepts.
`epoch_gone` and `not_a_row` stand here, before any section the check reads, so they count nowhere.

## A3. Data model

### A3.3 Key checks (`check_key`)

| Observation | Outcome |
|---|---|
| `ZSCORE ix` raises an error | integrity hold `shared_key_malformed` |

## A4. Scripts

`epoch_missing` is here too, between the sections, so it counts nowhere either.

### A4.1 Inventory

| Script | Header |
|---|---|
| `hold.lua` | `#!lua flags=allow-oom` |

### A4.2 Common prelude

```lua
local function load_epoch()
  local ok, raw = pcall(redis.call, 'HGETALL', KEYS[1])
  if #raw == 0 then return nil, 'epoch_record_missing' end
  if not ok then return nil, 'epoch_record_malformed' end
  return raw
end
--   hold(reason): HSET epoch hold reason; return {22, 0, 'integrity_hold'}
```

### A4.3 `acquire.lua`

Nothing for the fixture.

### A4.4 `refund.lua`

Nothing for the fixture.

### A4.5 Cleanup

Nothing for the fixture.

### A4.6 Admin scripts

| Script | Steps |
|---|---|
| `admin_start_epoch` | No flag: refuse (`epoch_exists`) a readable record. A missing clock → `{21, 0, 'clock'}` |
| `admin_status` | Read the record raw: empty → `{41, 0, 'epoch_record_missing'}`; `id ≠ expected_id` → epoch_changed |

#### A detail heading inside the section

The section goes on below a deeper heading. An unclosed ` backtick is plain text.

### A4.7 Reclaiming old-epoch data

Nothing for the fixture.

### A4.8 Rules for script authors

Nothing for the fixture.

### A4.9 Reply layouts and result codes

| Code | Name | Reply | Library result |
|---|---|---|---|
| 22 | `E_INTEGRITY_EPOCH` | `{22, 0, reason}` | (`hwm_missing`, ``hwm_malformed``) |
| 40 / 41 | `hold` | `{40, 0, '', 0}` or `{41, 0, 'epoch_changed'}` | rollback check |

## A5. Library internals

### A5.1 Packages

`quarantined` is out of the sections the check reads.
