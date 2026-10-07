"""Replay budget and admin JSONL records; main is callable in-process.

G1: Clocks are legal anywhere and set a nonnegative whole-microsecond reading.
G2: Neutral and validation_error acquires are legal anywhere and change no state.
Neutral results include state_integrity_error: no budget or hwm update and no
model comparison, even before an epoch. G5 requires both IDs to be empty.
Validation errors are checked solely against API-1, without needing a model.
G3: charged/error acquires, cleanup and cleanup_scan records need a current epoch.
DECISIONS clarification: a policy does not end the current epoch; the next epoch
record does. Intervening charged/error records use the previous epoch, if any.
Sequential replay keeps that current-epoch model. Every epoch_id-bearing record
belongs to that epoch; concurrent grouping is handled by check_bud11.
DECISIONS S7: cleanup_scan {t_us} records each pass page read's reply T0.
CLK-4 compares it with max(clock, hwm) // 1000 and stores that budget time,
even if the page has no due entries. Cleanup steps compare their code and
budget time, including gone/not-due skips; those skips store nothing.
Existing quarantines move with code 31; newly detected malformed buckets use 32.
DECISIONS S8: successful restore and clear records are applied in sequence.
Replay may infer quarantine before admin only from a per-key integrity signal
since the current epoch began or that key's last modeled clear: an acquire
state_integrity_error, refund attempt 23 or cleanup 32. Without that evidence
or a modeled quarantine, a successful admin record mismatches not_quarantined.
The log omits the expected fingerprint and epoch-wide hold, so these admin
preconditions cannot be independently compared.
Inspect changes no state or high-water mark. After cleanup 31, until the key's
next allow, applied refund or restore, u and t_us are compared by the balance
they imply at the later of snapshot and model time, with exact capped refill.
Every other inspect compares u and t_us exactly; present, g, s, x_us and the
per-key marker count mc are always exact for modeled bucket state.
Refund attempt 22 is epoch-wide and neutral: no model call, hwm or comparison.
The compact inspect record omits marker expiries, index scores and quarantine;
external malformed/unreadable fields and the store's raw types are not modeled.
Absent bucket fields are empty strings, as admin_inspect returns for missing
fields; present=2 and empty fields remain valid raw snapshots but mismatch the
model's readable stored bucket when one exists.
G4: Each epoch needs a policy since the previous epoch (or log start). Legal
records may intervene. Epoch IDs must be nonempty and fresh; a pending policy
at end of log is malformed. A new epoch resets the controlled reading to origin.
G5: Neutral records carry empty deduction_id and epoch_id.
G6: Deduction IDs are never reused, even across epochs.
Malformed records exit 2; the first mismatch exits 1 with expected and actual.
"""

import base64
import binascii
import copy
import json
import sys
from collections.abc import Iterator
from pathlib import Path

from refmodel import RecordError
from refmodel.model import TBOUND, Decision, Model, Policy
from refmodel.mutants import NAMES, make_model

NEUTRAL = {
    "not_ready",
    "config_error",
    "busy",
    "cancelled",
    "unavailable",
    "degraded",
    "outcome_unknown",
    "state_integrity_error",
}
RESULTS = NEUTRAL | {
    "allowed",
    "limited",
    "capacity_error",
    "validation_error",
    "clock_error",
    "receipt_error",
}
NUMBERS = ("decision_ns", "retry_after_ns", "u", "g", "s", "expiry_us")


def integer(record: dict[str, object], field: str, line: int) -> int:
    value = record.get(field)
    if type(value) is not int:
        raise RecordError(f"line {line}", field, "expected integer")
    return value


def string(record: dict[str, object], field: str, line: int) -> str:
    value = record.get(field)
    if not isinstance(value, str):
        raise RecordError(f"line {line}", field, "expected string")
    return value


def shape(record: dict[str, object], fields: set[str], line: int) -> None:
    if set(record) != fields | {"type"}:
        raise RecordError(f"line {line}", "fields", "missing or unexpected fields")


REFUND_RESULTS = {"applied", "already_applied", "receipt_invalid", "receipt_expired", "unresolved"}
FINAL_CODES = REFUND_RESULTS - {"unresolved"}
UNSENT_CODES = {"unavailable", "cancelled", "not_ready"}

ATTEMPT_CODES = (
    (REFUND_RESULTS - {"unresolved"})
    | (NEUTRAL - {"busy", "degraded"})
    | {"clock_error", "capacity_error", "state_integrity_error"}
)


def decode_key(encoded: str, line: int) -> bytes:
    try:
        key = base64.b64decode(encoded + "=" * (-len(encoded) % 4), altchars=b"-_", validate=True)
    except (ValueError, binascii.Error) as error:
        raise RecordError(f"line {line}", "key_b64url", "invalid base64url") from error
    if base64.urlsafe_b64encode(key).decode().rstrip("=") != encoded:
        raise RecordError(f"line {line}", "key_b64url", "expected canonical unpadded base64url")
    return key


def validate_cleanup(record: dict[str, object], line: int) -> bytes:
    shape(record, {"key_b64url", "t_us", "code"}, line)
    key = decode_key(string(record, "key_b64url", line), line)
    if not 1 <= len(key) <= 256:
        raise RecordError(f"line {line}", "key_b64url", "expected 1..256 UTF-8 bytes")
    try:
        key.decode("utf-8")
    except UnicodeDecodeError as error:
        raise RecordError(f"line {line}", "key_b64url", "expected UTF-8 bytes") from error
    if not 30 <= integer(record, "code", line) <= 33:
        raise RecordError(f"line {line}", "code", "expected cleanup code 30..33")
    if not 0 <= integer(record, "t_us", line) < TBOUND // 1000:
        raise RecordError(f"line {line}", "t_us", "expected bounded budget time")
    return key


def validate_cleanup_scan(record: dict[str, object], line: int) -> int:
    shape(record, {"t_us"}, line)
    t_us = integer(record, "t_us", line)
    if not 0 <= t_us < TBOUND // 1000:
        raise RecordError(f"line {line}", "t_us", "expected bounded budget time")
    return t_us


def admin_key(record: dict[str, object], line: int) -> bytes:
    key = decode_key(string(record, "key_b64url", line), line)
    if not 1 <= len(key) <= 256:
        raise RecordError(f"line {line}", "key_b64url", "expected 1..256 UTF-8 bytes")
    try:
        key.decode("utf-8")
    except UnicodeDecodeError as error:
        raise RecordError(f"line {line}", "key_b64url", "expected UTF-8 bytes") from error
    return key


def admin_time(record: dict[str, object], line: int) -> int:
    t_us = integer(record, "t_us", line)
    if not 0 <= t_us < TBOUND // 1000:
        raise RecordError(f"line {line}", "t_us", "expected bounded budget time")
    return t_us


def validate_restore(record: dict[str, object], line: int, policy: Policy) -> bytes:
    shape(record, {"key_b64url", "balance_units", "t_us", "g", "epoch_id"}, line)
    key = admin_key(record, line)
    balance = integer(record, "balance_units", line)
    if not 0 <= balance <= policy.capacity * policy.rate_period_ns:
        raise RecordError(f"line {line}", "balance_units", "expected balance within capacity")
    admin_time(record, line)
    if not 1 <= integer(record, "g", line) < 2**53:
        raise RecordError(f"line {line}", "g", "expected positive uint53 generation")
    if not string(record, "epoch_id", line):
        raise RecordError(f"line {line}", "epoch_id", "expected nonempty current epoch ID")
    return key


def validate_clear(record: dict[str, object], line: int) -> bytes:
    shape(record, {"key_b64url", "t_us"}, line)
    key = admin_key(record, line)
    admin_time(record, line)
    return key


INSPECT_NUMBERS = ("u", "t_us", "g", "s", "x_us", "mc")


def validate_inspect(record: dict[str, object], line: int) -> bytes:
    shape(record, {"key_b64url", "present", *INSPECT_NUMBERS}, line)
    key = admin_key(record, line)
    if not 0 <= integer(record, "present", line) <= 2:
        raise RecordError(f"line {line}", "present", "expected present code 0..2")
    for field in INSPECT_NUMBERS:
        # A4.6 returns missing, unreadable or non-uint53 fields as ''. These
        # fields are raw snapshots, so malformed stored balances are not capped.
        if record[field] == "":
            continue
        if not 0 <= integer(record, field, line) < 2**53:
            raise RecordError(f"line {line}", field, "expected uint53 integer or empty string")
    return key


SCRIPT_CODES = {
    3: "applied",
    4: "already_applied",
    10: "receipt_invalid",
    11: "receipt_expired",
    20: "config_error",
    21: "clock_error",
    22: "state_integrity_error",
    23: "state_integrity_error",
    24: "capacity_error",
    25: "outcome_unknown",
}


def refund_attempts(record: dict[str, object], line: int) -> list[dict[str, object]]:
    value = record.get("attempts")
    if not isinstance(value, list):
        raise RecordError(f"line {line}", "attempts", "expected array")
    attempts: list[dict[str, object]] = []
    final_seen = False
    for index, item in enumerate(value):
        if final_seen:
            raise RecordError(f"line {line}", "attempts", "attempt after final code")
        if not isinstance(item, dict):
            raise RecordError(f"line {line}", f"attempts[{index}]", "expected object")
        if set(item) != {"code", "t_us", "credited"}:
            raise RecordError(f"line {line}", f"attempts[{index}]", "missing or unexpected fields")
        item = dict(item)
        raw_code = code = item.get("code")
        if type(code) is int:
            item["code"] = SCRIPT_CODES.get(code, "unsupported")
        if string(item, "code", line) not in ATTEMPT_CODES:
            raise RecordError(f"line {line}", f"attempts[{index}].code", "unsupported code")
        code = string(item, "code", line)
        t = integer(item, "t_us", line)
        credited = integer(item, "credited", line)
        if t < 0 or credited < 0:
            raise RecordError(f"line {line}", "attempts", "negative t_us or credited")
        if code not in {"applied", "already_applied"} and credited != 0:
            raise RecordError(f"line {line}", "credited", "code requires zero credit")
        if (
            code in UNSENT_CODES | {"clock_error", "config_error"}
            or raw_code in ("capacity_error", 22, 25)
        ) and t != 0:
            raise RecordError(f"line {line}", "t_us", "code requires zero time")
        final_seen = code in FINAL_CODES
        # Integer 24 is a script reply; the outcome name is a pre-execution OOM.
        item["raw_code"] = raw_code
        attempts.append(item)
    return attempts


def validate_finish(record: dict[str, object], line: int) -> None:
    kind = record["type"]
    fields = {"deduction_id"}
    if kind == "release":
        fields.add("instance")
        string(record, "instance", line)
    elif kind == "refund":
        fields |= {"key_b64url", "g", "s", "expiry_us", "attempts", "result"}
        decode_key(string(record, "key_b64url", line), line)
        for field in ("g", "s", "expiry_us"):
            integer(record, field, line)
        attempts = refund_attempts(record, line)
        result = string(record, "result", line)
        last = attempts[-1]["code"] if attempts else None
        if (
            (last in FINAL_CODES and result != last)
            or (result in {"applied", "already_applied", "receipt_expired"} and last != result)
            or (result == "receipt_invalid" and last not in {None, "receipt_invalid"})
            or (result == "unresolved" and last in FINAL_CODES)
        ):
            raise RecordError(f"line {line}", "result", "differs from last attempt")
        if result not in REFUND_RESULTS:
            raise RecordError(f"line {line}", "result", "unsupported refund result")
    shape(record, fields, line)
    string(record, "deduction_id", line)


def read_records(path: Path) -> list[tuple[int, dict[str, object]]]:
    return list(_read_records(path))


def _read_records(path: Path) -> Iterator[tuple[int, dict[str, object]]]:
    lines = path.read_text(encoding="utf-8", newline="").split("\n")
    if lines[-1] == "":
        lines.pop()
    if not lines:
        raise RecordError("line 1", "record", "expected at least one record")
    for line, raw in enumerate(lines, 1):

        def unique_object(
            pairs: list[tuple[str, object]], record_line: int = line
        ) -> dict[str, object]:
            obj: dict[str, object] = {}
            for field, item in pairs:
                if field in obj:
                    raise RecordError(f"line {record_line}", repr(field), "duplicate member")
                obj[field] = item
            return obj

        try:
            value: object = json.loads(raw, object_pairs_hook=unique_object)
        except (ValueError, RecursionError) as error:
            raise RecordError(f"line {line}", "json", str(error)) from error
        if not isinstance(value, dict) or any(not isinstance(k, str) for k in value):
            raise RecordError(f"line {line}", "record", "expected object")
        record: dict[str, object] = value
        yield line, record


def replay(path: Path, mutant: str = "") -> int:
    policy: Policy | None = None
    model: Model | None = None
    epoch = ""
    clock = 0
    ids: set[str] = set()
    receipts: dict[str, dict[str, object]] = {}
    finished: dict[str, str] = {}
    epochs: set[str] = set()
    refunds: set[str] = set()
    integrity_signals: set[bytes] = set()
    moved_balances: set[bytes] = set()
    records = read_records(path)
    declared_epochs = {
        record.get("epoch_id")
        for _, record in records
        if record.get("type") == "epoch" and isinstance(record.get("epoch_id"), str)
    }
    for line, record in records:
        kind = string(record, "type", line)
        if kind == "policy":
            fields = set(Policy.__dataclass_fields__)
            shape(record, fields, line)
            try:
                policy = Policy(**{f: integer(record, f, line) for f in fields})
            except RecordError as error:
                raise RecordError(f"line {line}", "policy", str(error)) from error
        elif kind == "epoch":
            shape(record, {"epoch_id", "origin_ns"}, line)
            epoch = string(record, "epoch_id", line)
            if policy is None or not epoch or epoch in epochs:
                raise RecordError(f"line {line}", "epoch_id", "needs policy and fresh nonempty ID")
            clock = integer(record, "origin_ns", line)
            try:
                model = make_model(policy, clock, mutant)
            except RecordError as error:
                raise RecordError(f"line {line}", "origin_ns", str(error)) from error
            epochs.add(epoch)
            policy = None
            integrity_signals.clear()
            moved_balances.clear()
        elif kind == "clock":
            shape(record, {"value_ns"}, line)
            clock = integer(record, "value_ns", line)
            if clock < 0 or clock % 1000:
                raise RecordError(
                    f"line {line}", "value_ns", "expected nonnegative microsecond time"
                )
        elif kind == "cleanup_scan":
            t_us = validate_cleanup_scan(record, line)
            if model is None:
                raise RecordError(f"line {line}", "type", "cleanup_scan before epoch")
            budget = max(clock, model.hwm)
            if t_us != budget // 1000:
                print(f"line {line}: t_us: expected {budget // 1000!r}, actual {t_us!r}")
                return 1
            model.hwm = budget
        elif kind == "cleanup":
            key = validate_cleanup(record, line)
            if model is None:
                raise RecordError(f"line {line}", "type", "cleanup before epoch")
            cleanup = model.cleanup_step(key, clock)
            cleanup_checks = [("code", cleanup.code)]
            if cleanup.time_known:
                cleanup_checks.append(("t_us", cleanup.decision_ns // 1000))
            for cleanup_field, cleanup_wanted in cleanup_checks:
                cleanup_got = record[cleanup_field]
                if cleanup_wanted != cleanup_got:
                    print(
                        f"line {line}: {cleanup_field}: expected {cleanup_wanted!r}, "
                        f"actual {cleanup_got!r}"
                    )
                    return 1
            if cleanup.code == 32:
                integrity_signals.add(key)
            if cleanup.code == 31:
                moved_balances.add(key)
            elif cleanup.code == 30:
                moved_balances.discard(key)
        elif kind in {"restore", "clear"}:
            if model is None:
                raise RecordError(f"line {line}", "type", f"{kind} before epoch")
            if kind == "restore":
                key = validate_restore(record, line, model.policy)
                if record["epoch_id"] != epoch:
                    raise RecordError(f"line {line}", "epoch_id", "differs from current epoch")
            else:
                key = validate_clear(record, line)
            # An integrity signal is evidence of an unlogged quarantine write;
            # an admin success alone cannot establish its own precondition.
            if key in integrity_signals:
                model.quarantined.add(key)
            admin = (
                model.restore(key, integer(record, "balance_units", line), clock)
                if kind == "restore"
                else model.clear(key, clock)
            )
            admin_checks: list[tuple[str, object, object]] = [("code", admin.code, 40)]
            admin_checks.append(("t_us", admin.decision_ns // 1000, record["t_us"]))
            if kind == "restore":
                admin_checks.append(("g", admin.g, record["g"]))
            for admin_field, admin_wanted, admin_got in admin_checks:
                if admin_wanted != admin_got:
                    print(
                        f"line {line}: {admin_field}: expected {admin_wanted!r}, "
                        f"actual {admin_got!r}"
                    )
                    return 1
            if kind == "clear":
                integrity_signals.discard(key)
            else:
                moved_balances.discard(key)
        elif kind == "inspect":
            key = validate_inspect(record, line)
            if model is None:
                raise RecordError(f"line {line}", "type", "inspect before epoch")
            stored = model.buckets.get(key)
            snapshot: dict[str, object] = dict.fromkeys(INSPECT_NUMBERS, "")
            snapshot["present"] = int(stored is not None)
            if stored is not None:
                snapshot.update(
                    u=stored.units,
                    t_us=stored.time_ns // 1000,
                    g=stored.generation,
                    s=stored.sequence,
                    x_us=stored.latest_expiry_ns // 1000,
                    mc=sum(marker[0] == key for marker in model.markers),
                )
                if (
                    key in moved_balances
                    and type(record["u"]) is int
                    and type(record["t_us"]) is int
                ):
                    u = integer(record, "u", line)
                    t = integer(record, "t_us", line) * 1000
                    later = max(t, stored.time_ns)
                    cap = model.policy.capacity * model.policy.rate_period_ns
                    rate = model.policy.rate_tokens
                    snapshot["u"] = min(cap, stored.units + (later - stored.time_ns) * rate)
                    record = dict(record, u=min(cap, u + (later - t) * rate))
                    # Both balances are now at the same comparison time.
                    snapshot["t_us"] = record["t_us"]
            for inspect_field, inspect_wanted in snapshot.items():
                inspect_got = record[inspect_field]
                if inspect_wanted != inspect_got:
                    print(
                        f"line {line}: {inspect_field}: expected {inspect_wanted!r}, "
                        f"actual {inspect_got!r}"
                    )
                    return 1
        elif kind == "acquire":
            shape(
                record,
                set(NUMBERS) | {"instance", "key_b64url", "result", "deduction_id", "epoch_id"},
                line,
            )
            string(record, "instance", line)
            encoded = string(record, "key_b64url", line)
            try:
                key = base64.b64decode(
                    encoded + "=" * (-len(encoded) % 4), altchars=b"-_", validate=True
                )
            except (ValueError, binascii.Error) as error:
                raise RecordError(f"line {line}", "key_b64url", "invalid base64url") from error
            if base64.urlsafe_b64encode(key).decode().rstrip("=") != encoded:
                raise RecordError(
                    f"line {line}", "key_b64url", "expected canonical unpadded base64url"
                )
            result = string(record, "result", line)
            if result not in RESULTS:
                raise RecordError(f"line {line}", "result", "unsupported result")
            actual = {f: integer(record, f, line) for f in NUMBERS}
            deduction = string(record, "deduction_id", line)
            actual_epoch = string(record, "epoch_id", line)
            if result == "allowed" and actual_epoch and actual_epoch not in declared_epochs:
                raise RecordError(f"line {line}", "epoch_id", "unknown epoch")
            if result in NEUTRAL | {"validation_error"}:
                for field, identifier in (("deduction_id", deduction), ("epoch_id", actual_epoch)):
                    if identifier != "":
                        print(f"line {line}: {field}: expected '', actual {identifier!r}")
                        return 1
            if result in NEUTRAL:
                # G2/S7: neutral outcomes, including integrity errors, change no state.
                if result == "state_integrity_error" and model is not None:
                    integrity_signals.add(key)
                continue
            if result == "validation_error":
                invalid = not 1 <= len(key) <= 256
                try:
                    key.decode("utf-8")
                except UnicodeDecodeError:
                    invalid = True
                if not invalid:
                    print(f"line {line}: result: expected valid key, actual validation_error")
                    return 1
                expected = Decision("validation_error")
            else:
                if model is None:
                    raise RecordError(f"line {line}", "type", "acquire before epoch")
                expected = model.acquire(key, clock)
            checks: list[tuple[str, object, object]] = [
                (
                    "result",
                    "receipt_error"
                    if result == "receipt_error" and expected.result == "allowed"
                    else expected.result,
                    result,
                )
            ]
            checks.append(("u", expected.u, actual["u"]))
            checks.extend((f, getattr(expected, f), actual[f]) for f in NUMBERS if f != "u")
            checks.append(
                (
                    "epoch_id",
                    epoch if expected.result == "allowed" and result != "receipt_error" else "",
                    actual_epoch,
                )
            )
            checks.append(
                (
                    "deduction_id",
                    expected.result == "allowed" and result != "receipt_error",
                    bool(deduction),
                )
            )
            if deduction and deduction in ids:
                checks.append(("deduction_id unique", True, False))
            for field, wanted, got in checks:
                if wanted != got:
                    print(f"line {line}: {field}: expected {wanted!r}, actual {got!r}")
                    return 1
            if expected.result == "allowed":
                moved_balances.discard(key)
            if deduction:
                ids.add(deduction)
                receipts[deduction] = record
        elif kind in {"release", "done", "refund"}:
            validate_finish(record, line)
            deduction = string(record, "deduction_id", line)
            if not deduction and kind != "refund":
                continue
            receipt = receipts.get(deduction)
            if receipt is None:
                raise RecordError(f"line {line}", "deduction_id", "unknown receipt")
            if kind in {"release", "done"}:
                if kind == "release" and record["instance"] != receipt["instance"]:
                    print(
                        f"line {line}: instance: expected {receipt['instance']!r}, "
                        f"actual {record['instance']!r}"
                    )
                    return 1
                finished.setdefault(deduction, kind)
                continue
            if finished.get(deduction) != "release":
                raise RecordError(f"line {line}", "deduction_id", "refund without release")
            if deduction in refunds:
                raise RecordError(f"line {line}", "deduction_id", "duplicate final refund")
            refunds.add(deduction)
            for field in ("key_b64url", "g", "s", "expiry_us"):
                if record[field] != receipt[field]:
                    print(
                        f"line {line}: {field}: expected {receipt[field]!r}, "
                        f"actual {record[field]!r}"
                    )
                    return 1
            if model is None:
                raise RecordError(f"line {line}", "type", "refund before epoch")
            attempts = refund_attempts(record, line)
            for attempt in attempts:
                if attempt["code"] == "applied" and not (
                    integer(receipt, "decision_ns", line)
                    <= integer(attempt, "t_us", line) * 1000
                    < integer(receipt, "expiry_us", line) * 1000
                ):
                    raise RecordError(f"line {line}", "t_us", "credit outside receipt lifetime")
                if (
                    attempt["code"] == "applied"
                    and integer(attempt, "credited", line) > model.policy.rate_period_ns
                ):
                    raise RecordError(f"line {line}", "credited", "credit out of range")
            last = (
                "receipt_invalid"
                if not attempts and record["result"] == "receipt_invalid"
                else "unresolved"
            )
            for attempt in attempts:
                code = attempt["code"]
                raw_code = attempt["raw_code"]
                if raw_code in {"capacity_error", 22} or code in {
                    "outcome_unknown",
                    "unavailable",
                    "cancelled",
                    "config_error",
                    "not_ready",
                }:
                    if code == "outcome_unknown" and attempts[-1]["code"] == "already_applied":
                        # API-14: the confirmed marker proves this lost reply applied.
                        original = integer(attempts[-1], "credited", line) * 1000
                        budget = max(clock, model.hwm)
                        if original != budget:
                            print(
                                f"line {line}: credit_us: expected {budget // 1000!r}, "
                                f"actual {original // 1000!r}"
                            )
                            return 1
                        key = decode_key(string(record, "key_b64url", line), line)
                        hidden = model.refund(
                            key,
                            integer(record, "g", line),
                            integer(record, "s", line),
                            integer(record, "expiry_us", line),
                            original,
                            epoch_matches=receipt["epoch_id"] == epoch,
                        )
                        if hidden.result not in {"applied", "already_applied"}:
                            print(
                                f"line {line}: code: expected {hidden.result!r}, actual 'applied'"
                            )
                            return 1
                        if hidden.result == "applied":
                            moved_balances.discard(key)
                    continue
                if raw_code == 24:
                    # Probe API-2a through marker lookup without committing credit.
                    probe = copy.deepcopy(model)
                    checked = probe.refund(
                        decode_key(string(record, "key_b64url", line), line),
                        integer(record, "g", line),
                        integer(record, "s", line),
                        integer(record, "expiry_us", line),
                        clock,
                        epoch_matches=receipt["epoch_id"] == epoch,
                    )
                    model.hwm = probe.hwm
                    if checked.result != "applied":
                        print(f"line {line}: code: expected {checked.result!r}, actual {code!r}")
                        return 1
                    budget = max(clock, model.hwm)
                    if attempt["t_us"] != budget // 1000:
                        print(
                            f"line {line}: t_us: expected {budget // 1000!r}, "
                            f"actual {attempt['t_us']!r}"
                        )
                        return 1
                    model.hwm = budget
                    continue
                key = decode_key(string(record, "key_b64url", line), line)
                reply = model.refund(
                    key,
                    integer(record, "g", line),
                    integer(record, "s", line),
                    integer(record, "expiry_us", line),
                    clock,
                    epoch_matches=receipt["epoch_id"] == epoch,
                )
                if reply.result in FINAL_CODES:
                    last = reply.result
                comparisons = (
                    ("code", reply.result, code),
                    (
                        "t_us",
                        model.hwm // 1000 if reply.result != "clock_error" else 0,
                        attempt["t_us"],
                    ),
                    (
                        "credited",
                        reply.decision_ns // 1000
                        if reply.result == "already_applied"
                        else reply.credited,
                        attempt["credited"],
                    ),
                )
                for field, wanted, got in comparisons:
                    if wanted != got:
                        print(f"line {line}: {field}: expected {wanted!r}, actual {got!r}")
                        return 1
                if raw_code == 23:
                    integrity_signals.add(key)
                if reply.result == "applied":
                    moved_balances.discard(key)
            result = string(record, "result", line)
            if result != last:
                print(f"line {line}: result: expected {last!r}, actual {result!r}")
                return 1
        else:
            raise RecordError(f"line {line}", "type", f"unsupported record {kind!r}")
    if policy is not None:
        raise RecordError("end of log", "epoch", "policy without epoch")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    mutant = ""
    if len(args) == 3 and args[0] == "--mutant":
        mutant = args[1]
        args = args[2:]
        if mutant not in NAMES:
            print(f"arguments: field mutant: unknown mutant {mutant!r}")
            return 2
    if len(args) != 1:
        print("arguments: field path: expected one oplog.jsonl")
        return 2
    try:
        return replay(Path(args[0]), mutant)
    except (RecordError, OSError, UnicodeError) as error:
        print(str(error))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
