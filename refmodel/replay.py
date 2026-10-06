"""Replay S5 JSONL records; main is callable in-process.

G1: Clocks are legal anywhere and set a nonnegative whole-microsecond reading.
G2: Neutral and validation_error acquires are legal anywhere and change no state.
Validation errors are checked solely against API-1, without needing a model.
G3: allowed, limited, capacity_error and clock_error require a current epoch.
G4: Each epoch needs a policy since the previous epoch (or log start). Legal
records may intervene. Epoch IDs must be nonempty and fresh; a pending policy
at end of log is malformed. A new epoch resets the controlled reading to origin.
G5: Neutral records carry empty deduction_id and epoch_id.
G6: Deduction IDs are never reused, even across epochs.
Malformed records exit 2; the first mismatch exits 1 with expected and actual.
"""

import base64
import binascii
import json
import sys
from pathlib import Path

from refmodel import RecordError
from refmodel.model import Decision, Model, Policy

NEUTRAL = {
    "not_ready",
    "config_error",
    "busy",
    "cancelled",
    "unavailable",
    "degraded",
    "outcome_unknown",
}
RESULTS = NEUTRAL | {"allowed", "limited", "capacity_error", "validation_error", "clock_error"}
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


def replay(path: Path) -> int:
    policy: Policy | None = None
    model: Model | None = None
    epoch = ""
    clock = 0
    ids: set[str] = set()
    epochs: set[str] = set()
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
        kind = string(record, "type", line)
        if kind == "policy":
            fields = set(Policy.__dataclass_fields__)
            shape(record, fields, line)
            try:
                policy = Policy(**{f: integer(record, f, line) for f in fields})
            except RecordError as error:
                raise RecordError(f"line {line}", "policy", str(error)) from error
            model = None
        elif kind == "epoch":
            shape(record, {"epoch_id", "origin_ns"}, line)
            epoch = string(record, "epoch_id", line)
            if policy is None or not epoch or epoch in epochs:
                raise RecordError(f"line {line}", "epoch_id", "needs policy and fresh nonempty ID")
            clock = integer(record, "origin_ns", line)
            try:
                model = Model(policy, clock)
            except RecordError as error:
                raise RecordError(f"line {line}", "origin_ns", str(error)) from error
            epochs.add(epoch)
            policy = None
        elif kind == "clock":
            shape(record, {"value_ns"}, line)
            clock = integer(record, "value_ns", line)
            if clock < 0 or clock % 1000:
                raise RecordError(
                    f"line {line}", "value_ns", "expected nonnegative microsecond time"
                )
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
            if result in NEUTRAL | {"validation_error"}:
                for field, identifier in (("deduction_id", deduction), ("epoch_id", actual_epoch)):
                    if identifier != "":
                        print(f"line {line}: {field}: expected '', actual {identifier!r}")
                        return 1
            if result in NEUTRAL:
                # No model transition: these outcomes are opaque in the S5 bridge.
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
            checks: list[tuple[str, object, object]] = [("result", expected.result, result)]
            checks.append(("u", expected.u, actual["u"]))
            checks.extend((f, getattr(expected, f), actual[f]) for f in NUMBERS if f != "u")
            checks.append(("epoch_id", epoch if expected.result == "allowed" else "", actual_epoch))
            checks.append(("deduction_id", expected.result == "allowed", bool(deduction)))
            if deduction and deduction in ids:
                checks.append(("deduction_id unique", True, False))
            for field, wanted, got in checks:
                if wanted != got:
                    print(f"line {line}: {field}: expected {wanted!r}, actual {got!r}")
                    return 1
            if deduction:
                ids.add(deduction)
        else:
            raise RecordError(f"line {line}", "type", f"unsupported record {kind!r}")
    if policy is not None:
        raise RecordError("end of log", "epoch", "policy without epoch")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 1:
        print("arguments: field path: expected one oplog.jsonl")
        return 2
    try:
        return replay(Path(args[0]))
    except (RecordError, OSError, UnicodeError) as error:
        print(str(error))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
