"""Exact BUD-11a sweep, including possible credits over half-open intervals.

Times are nanoseconds. Known credits are times (one token each), or
(time, units) pairs for explicit balance credits; unknown credits are
(start, end) intervals, each worth one token. A violation names its closed window.
Sorting costs O(n log n); the sweep after sorting is O(n).
DECISIONS clarification: a policy leaves the previous epoch current until the
next epoch record. Charged/error acquires need a current epoch. G2 neutral
acquires, including state_integrity_error, and validation_error are legal
anywhere, even before an epoch, and G5 requires empty deduction_id and epoch_id.
Integrity refusals change no budget or hwm, need no model comparison and add
no charge or credit to any BUD-11a window. Allows,
credits and unknown intervals belong to their own epoch_id, regardless of log
position, validated against all epoch records and each epoch's preceding policy.
DECISIONS S7: cleanup_scan {t_us} records each pass page's due-entry read,
whose reply T0 stores the CLK-4 budget time. Scans require a current epoch and
bounded microsecond time; scans and cleanup steps change no BUD-11a window.
DECISIONS S8: successful restore records credit their explicit balance_units at
t_us in the current epoch; a restore must name that epoch. Clear and inspect
records change no window. A policy and epoch pair begins independent accounting
for every key in the new epoch, including when the policy remains unchanged.
"""

import json
import re
import sys
import tomllib
from collections.abc import Iterator
from dataclasses import dataclass
from pathlib import Path

from refmodel import RecordError
from refmodel.model import TBOUND, Policy
from refmodel.replay import (
    NEUTRAL,
    NUMBERS,
    REFUND_RESULTS,
    RESULTS,
    UNSENT_CODES,
    decode_key,
    integer,
    read_records,
    refund_attempts,
    shape,
    string,
    validate_cleanup,
    validate_cleanup_scan,
    validate_clear,
    validate_finish,
    validate_inspect,
    validate_restore,
)


@dataclass(frozen=True)
class Violation:
    start_ns: int
    end_ns: int
    excess_units: int


def check_window(
    allows: list[int],
    credits: list[int] | list[tuple[int, int]],
    unknown: list[tuple[int, int]],
    policy: Policy,
) -> Violation | None:
    for t in allows:
        if type(t) is not int or t < 0:
            raise RecordError("window", "allows", "expected nonnegative integer times")
    normalized = [
        (credit, policy.rate_period_ns) if isinstance(credit, int) else credit for credit in credits
    ]
    for t, units in normalized:
        if type(t) is not int or type(units) is not int or min(t, units) < 0:
            raise RecordError("window", "credits", "expected nonnegative integer time and units")
    for start, end in unknown:
        if type(start) is not int or type(end) is not int or not 0 <= start < end:
            raise RecordError("window", "unknown", "expected nonempty integer interval")
    times = sorted(allows)
    known = sorted(normalized)
    starts = sorted(start for start, _ in unknown)
    ends = sorted(end for _, end in unknown)
    c = s = e = 0
    ci = si = ei = 0
    minimum: int | None = None
    minimum_time = 0
    rp = policy.rate_period_ns
    cap = policy.capacity * rp
    cached_time = -1
    inclusive_c = 0
    equal = 0
    for j, t in enumerate(times, 1):
        while ci < len(known) and known[ci][0] < t:
            c += known[ci][1]
            ci += 1
        while ei < len(ends) and ends[ei] <= t:
            e += rp
            ei += 1
        g = (j - 1) * rp - c - e - policy.rate_tokens * t
        if minimum is None or g < minimum:
            minimum, minimum_time = g, t
        # Equal-time credits belong to every closed window beginning here.
        if cached_time != t:
            equal = ci
            inclusive_c = c
            while equal < len(known) and known[equal][0] == t:
                inclusive_c += known[equal][1]
                equal += 1
            cached_time = t
        while si < len(starts) and starts[si] <= t:
            s += rp
            si += 1
        h = j * rp - inclusive_c - s - policy.rate_tokens * t
        window_units = h - minimum
        if window_units > cap:
            return Violation(minimum_time, t, window_units - cap)
    return None


def check_oplog(path: Path) -> tuple[str, Violation] | None:
    pending: Policy | None = None
    current = ""
    policies: dict[str, Policy] = {}
    allows: dict[tuple[str, bytes], list[int]] = {}
    credits: dict[tuple[str, bytes], list[tuple[int, int]]] = {}
    receipts: dict[str, dict[str, object]] = {}
    releases: set[str] = set()
    references: dict[str, int] = {}
    finishes: dict[str, str] = {}
    refunds: dict[str, tuple[int, dict[str, object]]] = {}
    charged_records: list[tuple[int, dict[str, object], str]] = []
    for line, record in read_records(path):
        kind = string(record, "type", line)
        if kind == "policy":
            fields = set(Policy.__dataclass_fields__)
            shape(record, fields, line)
            try:
                pending = Policy(**{f: integer(record, f, line) for f in fields})
            except RecordError as error:
                raise RecordError(f"line {line}", "policy", str(error)) from error
        elif kind == "epoch":
            shape(record, {"epoch_id", "origin_ns"}, line)
            current = string(record, "epoch_id", line)
            origin = integer(record, "origin_ns", line)
            if pending is None or not current or current in policies:
                raise RecordError(f"line {line}", "epoch_id", "needs policy and fresh ID")
            from refmodel.model import Model

            try:
                Model(pending, origin)
            except RecordError as error:
                raise RecordError(f"line {line}", "origin_ns", str(error)) from error
            policies[current] = pending
            pending = None
        elif kind == "clock":
            shape(record, {"value_ns"}, line)
            t = integer(record, "value_ns", line)
            if t < 0 or t % 1000:
                raise RecordError(f"line {line}", "value_ns", "expected microsecond time")
        elif kind == "cleanup":
            validate_cleanup(record, line)
            if not current:
                raise RecordError(f"line {line}", "epoch", "needs current epoch")
            # STA-4 removes only full buckets whose possible credits expired;
            # STA-3 removes only expired markers. Neither changes BUD-11a.
        elif kind == "cleanup_scan":
            validate_cleanup_scan(record, line)
            if not current:
                raise RecordError(f"line {line}", "epoch", "needs current epoch")
        elif kind in {"restore", "clear", "inspect"}:
            if not current:
                raise RecordError(f"line {line}", "epoch", "needs current epoch")
            if kind == "restore":
                key = validate_restore(record, line, policies[current])
                if string(record, "epoch_id", line) != current:
                    raise RecordError(f"line {line}", "epoch_id", "needs current epoch")
                credits.setdefault((current, key), []).append(
                    (integer(record, "t_us", line) * 1000, integer(record, "balance_units", line))
                )
            elif kind == "clear":
                validate_clear(record, line)
            else:
                validate_inspect(record, line)
        elif kind == "acquire":
            shape(
                record,
                set(NUMBERS) | {"instance", "key_b64url", "result", "deduction_id", "epoch_id"},
                line,
            )
            key = decode_key(string(record, "key_b64url", line), line)
            string(record, "instance", line)
            result = string(record, "result", line)
            if result not in RESULTS:
                raise RecordError(f"line {line}", "result", "unsupported result")
            for field in NUMBERS:
                integer(record, field, line)
            deduction = string(record, "deduction_id", line)
            epoch = string(record, "epoch_id", line)
            if result not in NEUTRAL | {"validation_error"} and not current:
                raise RecordError(f"line {line}", "epoch_id", "needs current epoch")
            if result in {"allowed", "receipt_error"}:
                if (result == "allowed" and (not epoch or not deduction)) or (
                    result == "receipt_error" and (epoch or deduction)
                ):
                    raise RecordError(f"line {line}", "deduction_id", "invalid charged identity")
                t = integer(record, "decision_ns", line)
                if t < 0 or t % 1000:
                    raise RecordError(f"line {line}", "decision_ns", "expected microsecond time")
                charged_epoch = epoch if result == "allowed" else current
                charged_records.append((line, record, charged_epoch))
                allows.setdefault((charged_epoch, key), []).append(t)
                if deduction:
                    if deduction in receipts:
                        raise RecordError(f"line {line}", "deduction_id", "duplicate identity")
                    receipts[deduction] = record
            elif deduction or epoch:
                raise RecordError(
                    f"line {line}", "deduction_id", "uncharged result needs empty IDs"
                )
        elif kind in {"release", "done", "refund"}:
            validate_finish(record, line)
            deduction = string(record, "deduction_id", line)
            if kind == "release" and not deduction:
                continue
            if not deduction and kind == "done":
                continue
            references[deduction] = line
            if kind in {"release", "done"}:
                finishes.setdefault(deduction, kind)
            if kind == "release" and finishes[deduction] == "release":
                releases.add(deduction)
            elif kind == "refund":
                if deduction in refunds:
                    raise RecordError(f"line {line}", "deduction_id", "duplicate final refund")
                refunds[deduction] = (line, record)
        else:
            raise RecordError(f"line {line}", "type", "unsupported record")
    if pending is not None:
        raise RecordError("end of log", "epoch", "policy without epoch")
    for deduction, line in references.items():
        if deduction not in receipts:
            raise RecordError(f"line {line}", "deduction_id", "unknown receipt")
    for line, record, epoch in charged_records:
        if epoch in policies and integer(record, "expiry_us", line) * 1000 != (
            integer(record, "decision_ns", line) + policies[epoch].receipt_lifetime_ns
        ):
            raise RecordError(f"line {line}", "expiry_us", "does not match receipt lifetime")
    for epoch, _ in allows:
        if epoch not in policies:
            line = next(
                line for line, record in read_records(path) if record.get("epoch_id") == epoch
            )
            raise RecordError(f"line {line}", "epoch_id", "unknown epoch")
    unknown: dict[tuple[str, bytes], list[tuple[int, int]]] = {}
    for deduction in releases | refunds.keys():
        receipt = receipts.get(deduction)
        if receipt is None:
            raise RecordError("end of log", "deduction_id", f"unknown receipt {deduction!r}")
        if deduction not in releases:
            raise RecordError(
                f"line {refunds[deduction][0]}", "deduction_id", "refund without release"
            )
        epoch = string(receipt, "epoch_id", 0)
        key = decode_key(string(receipt, "key_b64url", 0), 0)
        group = (epoch, key)
        policy = policies[epoch]
        final = refunds.get(deduction)
        possible = final is None
        if final is not None:
            line, record = final
            for field in ("key_b64url", "g", "s", "expiry_us"):
                if record[field] != receipt[field]:
                    raise RecordError(f"line {line}", field, "does not match acquire")
            attempts = refund_attempts(record, line)
            result = string(record, "result", line)
            for attempt in attempts:
                if (
                    attempt["code"] == "applied"
                    and integer(attempt, "credited", line) > policy.rate_period_ns
                ):
                    raise RecordError(f"line {line}", "credited", "credit out of range")
            # A named capacity_error (OOM) writes nothing, but the command was SENT.
            possible = result == "receipt_expired" or (
                result == "unresolved" and any(a["raw_code"] not in UNSENT_CODES for a in attempts)
            )
            confirmed = [a for a in attempts if a["code"] in {"applied", "already_applied"}]
            if confirmed and (confirmed[-1] is not attempts[-1] or confirmed[-1]["code"] != result):
                raise RecordError(f"line {line}", "attempts", "confirmed code differs from result")
            if result in {"applied", "already_applied"}:
                if not confirmed:
                    raise RecordError(
                        f"line {line}", "attempts", "needs last confirmed code equal to result"
                    )
                attempt = confirmed[-1]
                t = integer(attempt, "t_us", line) * 1000
                units = integer(attempt, "credited", line)
                if attempt["code"] == "already_applied":
                    t = units * 1000
                    units = policy.rate_period_ns
                if (
                    not integer(receipt, "decision_ns", line)
                    <= t
                    < integer(receipt, "expiry_us", line) * 1000
                ):
                    raise RecordError(
                        f"line {line}", "credit_us", "credit outside receipt lifetime"
                    )
                if (
                    attempt["code"] == "already_applied"
                    and t > integer(attempt, "t_us", line) * 1000
                ):
                    raise RecordError(f"line {line}", "credit_us", "credit after reply time")
                # BUD-11a counts one accepted refund, including a capped
                # zero credit. Actual units remain the replay model's concern.
                credits.setdefault(group, []).append((t, policy.rate_period_ns))
        if possible:
            start = integer(receipt, "decision_ns", 0)
            unknown.setdefault(group, []).append((start, start + policy.receipt_lifetime_ns))
    for group, times in allows.items():
        violation = check_window(
            times, credits.get(group, []), unknown.get(group, []), policies[group[0]]
        )
        if violation is not None:
            return f"epoch {group[0]!r} key {group[1]!r}", violation
    return None


# Run logs are independent of op logs: kind-based golden shapes, one epoch,
# user_id keys, and budget times joined by deduction_id across each whole file.
RUN_FIELDS = {
    "decision": "deduction_id user_id decision_ns result",
    "release": "deduction_id release_mono_ns",
    "refund": (
        "deduction_id result reason attempts credit_decision_ns release_mono_ns final_mono_ns"
    ),
    "request": (
        "run_id epoch_id instance user_id request_id arrival_ns decision_ns deduction_id "
        "result existed http_status error limiter_ns db_ns total_ns"
    ),
    "start": (
        "max_in_flight queue_len max_release_delay_ns dependency_deadline_ns drain_timeout_ns "
        "nofile_limit"
    ),
    "ready": "",
    "stop": "listener_drops log_drops",
    "pre_kill": "listener_drops log_drops read_ok",
}
RUN_STRINGS = {
    "deduction_id",
    "user_id",
    "result",
    "reason",
    "run_id",
    "epoch_id",
    "request_id",
    "error",
}


class RunRecordError(RecordError):
    """Run input error with a printable, unambiguous field name."""

    def __init__(self, record: str, field: str, reason: str) -> None:
        super().__init__(record, repr(field), reason)


def run_integer(record: dict[str, object], field: str, location: str) -> int:
    value = record.get(field)
    if type(value) is not int or value < 0:
        raise RunRecordError(location, field, "expected nonnegative integer")
    if field in {"listener_drops", "log_drops"} and value > 2**63 - 1:
        raise RunRecordError(location, field, "expected count in range 0-9223372036854775807")
    return value


def run_string(record: dict[str, object], field: str, location: str) -> str:
    value = record.get(field)
    if not isinstance(value, str):
        raise RunRecordError(location, field, "expected string")
    try:
        value.encode("utf-8")
    except UnicodeEncodeError as error:
        raise RunRecordError(location, field, "expected UTF-8 string") from error
    return value


def validate_run_record(record: dict[str, object], location: str, instance: int) -> str:
    kind = run_string(record, "kind", location)
    event = run_string(record, "event", location) if kind == "lifecycle" else kind
    if event not in RUN_FIELDS or (
        kind != "lifecycle" and event in {"start", "ready", "stop", "pre_kill"}
    ):
        raise RunRecordError(
            location, "event" if kind == "lifecycle" else "kind", "unsupported record"
        )
    if kind == "lifecycle" and event not in {"start", "ready", "stop", "pre_kill"}:
        raise RunRecordError(location, "event", "unsupported lifecycle event")
    fields = set(RUN_FIELDS[event].split()) | {"kind"}
    if kind == "lifecycle":
        fields.add("event")
    for field in sorted(fields - record.keys()):
        raise RunRecordError(location, field, "missing field")
    for field in sorted(record.keys() - fields):
        raise RunRecordError(location, field, "unexpected field")
    for field in sorted(fields - {"kind", "event"}):
        if field in RUN_STRINGS:
            run_string(record, field, location)
        elif field in {"read_ok", "existed"}:
            if type(record[field]) is not bool:
                raise RunRecordError(location, field, "expected boolean")
        else:
            value = run_integer(record, field, location)
            if field in {"decision_ns", "credit_decision_ns"} and (value % 1000 or value >= TBOUND):
                raise RunRecordError(location, field, "expected bounded microsecond budget time")
    if event == "decision":
        result = run_string(record, "result", location)
        if result not in {"allowed", "degraded"}:
            raise RunRecordError(location, "result", "decision must be allowed or degraded")
        user = run_string(record, "user_id", location)
        if not 1 <= len(user.encode("utf-8")) <= 256:
            raise RunRecordError(location, "user_id", "expected 1-256 UTF-8 bytes")
        deduction = run_string(record, "deduction_id", location)
        if (result == "allowed" and not deduction) or (result == "degraded" and deduction):
            raise RunRecordError(location, "deduction_id", "invalid permit identity")
        if result == "degraded" and record["decision_ns"] != 0:
            raise RunRecordError(location, "decision_ns", "degraded requires zero placeholder")
    elif event == "refund":
        result = run_string(record, "result", location)
        if result not in REFUND_RESULTS:
            raise RunRecordError(location, "result", "unsupported refund result")
        reason = run_string(record, "reason", location)
        if (result == "unresolved" and reason not in {"closed", "retention", "expired"}) or (
            result != "unresolved" and reason != ""
        ):
            raise RunRecordError(location, "reason", "invalid final-event reason")
        if result in {"applied", "already_applied"}:
            if record["attempts"] == 0:
                raise RunRecordError(location, "attempts", "confirmed credit needs a sent attempt")
        elif record["credit_decision_ns"] != 0:
            raise RunRecordError(
                location, "credit_decision_ns", "unknown credit needs zero placeholder"
            )
        if run_integer(record, "final_mono_ns", location) < run_integer(
            record, "release_mono_ns", location
        ):
            raise RunRecordError(location, "final_mono_ns", "final event precedes release")
    elif event == "request":
        if record["instance"] != instance:
            raise RunRecordError(location, "instance", "does not match run-log filename")
        if not 100 <= run_integer(record, "http_status", location) <= 599:
            raise RunRecordError(location, "http_status", "expected HTTP status 100-599")
    elif event == "start" and record["queue_len"] != 0:
        raise RunRecordError(location, "queue_len", "host must not queue requests")
    return event


def read_run_records(path: Path) -> Iterator[tuple[int, dict[str, object]]]:
    try:
        stream = path.open("rb")
    except OSError as error:
        raise RunRecordError(f"{path}: line 1", "file", str(error)) from error
    with stream:
        for line, raw in enumerate(stream, 1):
            location = f"{path}: line {line}"

            def unique_fields(
                pairs: list[tuple[str, object]], location: str = location
            ) -> dict[str, object]:
                fields: dict[str, object] = {}
                for field, value in pairs:
                    if field in fields:
                        raise RunRecordError(location, field, "duplicate JSON field")
                    fields[field] = value
                return fields

            def reject_constant(value: str, location: str = location) -> object:
                raise RunRecordError(location, "json", f"non-JSON number {value}")

            try:
                record = json.loads(
                    raw.decode("utf-8"),
                    object_pairs_hook=unique_fields,
                    parse_constant=reject_constant,
                )
            except RecordError:
                raise
            except UnicodeDecodeError as error:
                raise RunRecordError(location, "json", str(error)) from error
            except (ValueError, RecursionError) as error:
                raise RunRecordError(location, "json", str(error)) from error
            if not isinstance(record, dict):
                raise RunRecordError(location, "json", "expected object")
            yield line, record


def config_source_lines(path: Path) -> Iterator[tuple[int, str]]:
    """Strip comments outside quotes and omit multiline string continuation lines."""
    quote = ""
    depth = 0
    with path.open(encoding="utf-8") as stream:
        for line, raw in enumerate(stream, 1):
            continuation = len(quote) == 3 or depth > 0
            index = 0
            while index < len(raw):
                char = raw[index]
                if quote:
                    if quote.startswith('"') and char == "\\":
                        index += 2
                        continue
                    if raw.startswith(quote, index):
                        index += len(quote)
                        if len(quote) == 3:
                            # TOML permits one or two extra quotes at the closing delimiter.
                            for _ in range(2):
                                if index < len(raw) and raw[index] == quote[0]:
                                    index += 1
                        quote = ""
                        continue
                elif char == "#":
                    break
                elif char in {'"', "'"}:
                    quote = char * 3 if raw.startswith(char * 3, index) else char
                    index += len(quote)
                    continue
                elif char in "[{":
                    depth += 1
                elif char in "]}":
                    depth -= 1
                index += 1
            if not continuation:
                yield line, raw[:index].strip()


def config_location(path: Path, table: str, field: str, *, present: bool = True) -> str:
    # TOML has no source-position API. Use a line only for an unambiguous key
    # or table declaration; a missing key points to its table declaration.
    current: str | None = ""
    table_lines: list[int] = []
    key_lines: list[int] = []
    for line, text in config_source_lines(path):
        if text.startswith("["):
            current = None
            try:
                header = tomllib.loads(text)
            except tomllib.TOMLDecodeError:
                continue
            if len(header) == 1:
                name, contents = next(iter(header.items()))
                if contents == {}:
                    current = name
                    if name == table:
                        table_lines.append(line)
        elif current == table:
            key_match = re.match(r"""("(?:[^"\\]|\\.)*"|'[^']*'|[\w-]+)\s*=""", text)
            if key_match is None:
                continue
            try:
                key = tomllib.loads(f"{key_match.group(1)} = 0")
            except tomllib.TOMLDecodeError:
                continue
            if field in key:
                key_lines.append(line)
    candidates = key_lines if field and present else table_lines
    if len(candidates) == 1:
        return f"{path}: line {candidates[0]}"
    key_path = f"{table}.{field}" if field else table
    return f"{path}: key {key_path}"


def config_failure_line(path: Path) -> int:
    """Locate parser resource failures, which have no TOML source position."""
    lines: list[str] = []
    with path.open(encoding="utf-8") as stream:
        for line, raw in enumerate(stream, 1):
            lines.append(raw)
            try:
                tomllib.loads("".join(lines))
            except RecordError:
                raise
            except tomllib.TOMLDecodeError:
                continue  # An incomplete prefix may become valid on the next line.
            except (ValueError, RecursionError):
                return line
    return 1


def read_run_policy(path: Path) -> Policy:
    try:
        lines: list[str] = []
        with path.open("rb") as stream:
            for line, raw in enumerate(stream, 1):
                try:
                    lines.append(raw.decode("utf-8"))
                except UnicodeDecodeError as error:
                    raise RunRecordError(f"{path}: line {line}", "config", str(error)) from error
        config = tomllib.loads("".join(lines))
    except RecordError:
        raise
    except tomllib.TOMLDecodeError as error:
        syntax_line = getattr(error, "lineno", None)
        if not hasattr(error, "lineno"):
            position = re.search(r"\(at line ([0-9]{1,10}), column [0-9]+\)\Z", str(error))
            syntax_line = int(position.group(1)) if position else None
        location = (
            f"{path}: line {syntax_line}"
            if type(syntax_line) is int and 0 < syntax_line <= 10**9
            else str(path)
        )
        raise RunRecordError(location, "toml", str(error)) from error
    except (ValueError, RecursionError) as error:
        line = config_failure_line(path)
        raise RunRecordError(f"{path}: line {line}", "toml", str(error)) from error
    except (OSError, UnicodeError) as error:
        raise RunRecordError(f"{path}: line 1", "config", str(error)) from error
    values = config.get("policy")
    if not isinstance(values, dict):
        raise RunRecordError(f"{path}: line 1", "policy", "missing policy table")
    fields = {
        "rate_tokens",
        "rate_period_ns",
        "capacity",
        "failure_mode",
        "max_tracked_buckets",
        "max_live_markers",
        "max_markers_per_key",
        "receipt_lifetime_ns",
        "refund_deadline_ns",
        "outage_tolerance_ns",
        "cleanup_interval_ns",
        "clock_source",
    }
    for field, value in values.items():
        location = config_location(path, "policy", field)
        if field not in fields:
            raise RunRecordError(location, field, "unknown config field")
        if field in {"failure_mode", "clock_source"}:
            if not isinstance(value, str) or not value:
                raise RunRecordError(location, field, "expected nonempty string")
            if field == "failure_mode" and value not in {"open", "closed"}:
                raise RunRecordError(location, field, "expected open or closed")
            if field == "clock_source" and value not in {"store", "controlled"}:
                raise RunRecordError(location, field, "expected store or controlled")
        else:
            if type(value) is not int or value < (0 if field == "outage_tolerance_ns" else 1):
                raise RunRecordError(location, field, "expected integer in range")
            if field.endswith("_ns") and value % 1000:
                raise RunRecordError(location, field, "expected microsecond granularity")
    numbers: dict[str, int] = {}
    for field in Policy.__dataclass_fields__:
        if field == "max_tracked_buckets" and field not in values:
            continue
        location = config_location(path, "policy", field, present=field in values)
        numbers[field] = run_integer(values, field, location)
    try:
        return Policy(**numbers)
    except RecordError as error:
        field = str(error).split(": field ", 1)[1].split(":", 1)[0]
        raise RunRecordError(config_location(path, "policy", field), field, str(error)) from error


@dataclass
class RunEvidence:
    allows: dict[str, list[int]]
    credits: dict[str, list[int]]
    unknown: dict[str, list[tuple[int, int]]]
    deductions: set[str]
    invalid_reasons: list[str]
    listener_drops: int = 0
    log_drops: int = 0
    pre_kill_lines: int = 0
    pre_kill_read_ok_false: int = 0


def collect_run_file(path: Path, instance: int, policy: Policy, evidence: RunEvidence) -> None:
    decisions: dict[str, tuple[str, int]] = {}
    releases: dict[str, tuple[int, int, str]] = {}
    refunds: dict[str, tuple[dict[str, object], str]] = {}
    # Synthetic logs may omit start; retain an implicit initial segment.
    segment = 0
    segment_start = 0
    has_terminal = False
    killed_segments: set[int] = set()

    def finish_segment() -> None:
        if not has_terminal:
            location = f"{path.name}: line {segment_start}" if segment_start else path.name
            evidence.invalid_reasons.append(f"{location}: missing stop or pre_kill")

    for line, record in read_run_records(path):
        location = f"{path}: line {line}"
        event = validate_run_record(record, location, instance)
        if event == "decision" and record["result"] == "allowed":
            deduction = run_string(record, "deduction_id", location)
            if deduction in evidence.deductions:
                raise RunRecordError(location, "deduction_id", "duplicate deduction identity")
            evidence.deductions.add(deduction)
            user = run_string(record, "user_id", location)
            time = run_integer(record, "decision_ns", location)
            decisions[deduction] = (user, time)
            evidence.allows.setdefault(user, []).append(time)
        elif event in {"release", "refund"}:
            deduction = run_string(record, "deduction_id", location)
            if event == "release" and not deduction:
                continue  # Degraded permits have neither receipt nor final event.
            if event == "release":
                if deduction in releases:
                    raise RunRecordError(location, "deduction_id", "duplicate release")
                releases[deduction] = (segment, line, location)
            else:
                if deduction in refunds:
                    raise RunRecordError(location, "deduction_id", "duplicate final event")
                refunds[deduction] = (record, location)
        elif event == "start":
            if segment_start or line > 1:
                finish_segment()
            segment += 1
            segment_start = line
            has_terminal = False
        elif event in {"stop", "pre_kill"}:
            has_terminal = True
            evidence.listener_drops += run_integer(record, "listener_drops", location)
            evidence.log_drops += run_integer(record, "log_drops", location)
            if event == "pre_kill":
                killed_segments.add(segment)
                evidence.pre_kill_lines += 1
                if record["read_ok"] is False:
                    evidence.pre_kill_read_ok_false += 1
    finish_segment()
    for deduction in sorted(releases.keys() | refunds.keys()):
        location = releases[deduction][2] if deduction in releases else refunds[deduction][1]
        short_location = location.replace(str(path), path.name, 1)
        if deduction not in decisions:
            evidence.invalid_reasons.append(
                str(
                    RunRecordError(
                        short_location, "deduction_id", "no allowed decision in this instance file"
                    )
                )
            )
            continue
        if deduction not in releases:
            evidence.invalid_reasons.append(
                str(RunRecordError(short_location, "deduction_id", "final event without release"))
            )
            continue
        user, start = decisions[deduction]
        end = start + policy.receipt_lifetime_ns
        final = refunds.get(deduction)
        possible = False
        if final is None:
            possible = releases[deduction][0] in killed_segments
            if not possible:
                evidence.invalid_reasons.append(
                    f"{path.name}: line {releases[deduction][1]}: "
                    "release without final event or segment pre_kill"
                )
        else:
            record, location = final
            result = run_string(record, "result", location)
            if result in {"applied", "already_applied"}:
                time = run_integer(record, "credit_decision_ns", location)
                if not start <= time < end:
                    raise RunRecordError(
                        location, "credit_decision_ns", "credit outside receipt lifetime"
                    )
                evidence.credits.setdefault(user, []).append(time)
            possible = result == "receipt_expired" or (
                result == "unresolved" and run_integer(record, "attempts", location) >= 1
            )
        if possible:
            evidence.unknown.setdefault(user, []).append((start, end))


def check_run(path: Path) -> dict[str, object]:
    """Judge one run epoch with golden record shapes and deduction joins.

    Plan choices from HAR-13 and DESIGN §B8: log drops, missing terminal
    evidence in any process segment, join anomalies, and an open release
    without a pre_kill in its segment invalidate evidence. Invalid outranks violation.
    The four budget policy keys are required; other §B2 policy settings are optional
    but validated when supplied. Other config tables are ignored.
    Instance filenames are contiguous from 1.
    """
    policy = read_run_policy(path.joinpath("config.toml"))
    files: dict[int, Path] = {}
    for file in path.glob("api-*.jsonl"):
        number = file.name.removeprefix("api-").removesuffix(".jsonl")
        if (
            not number.isascii()
            or not number.isdecimal()
            or int(number) < 1
            or str(int(number)) != number
        ):
            raise RunRecordError(f"{file}: line 1", "filename", "expected api-N.jsonl, N >= 1")
        files[int(number)] = file
    if not files or sorted(files) != list(range(1, len(files) + 1)):
        raise RunRecordError(
            f"{path}: line 1", "api-N.jsonl", "expected contiguous instance files starting at 1"
        )
    evidence = RunEvidence({}, {}, {}, set(), [])
    for instance, file in sorted(files.items()):
        collect_run_file(file, instance, policy, evidence)
    reasons = evidence.invalid_reasons
    if evidence.listener_drops:
        reasons.append("listener_drops")
    if evidence.pre_kill_read_ok_false:
        reasons.append("pre_kill_read_ok_false")
    if evidence.log_drops:
        reasons.append("log_drops")
    violation: dict[str, object] | None = None
    if not reasons:
        for user, allows in sorted(evidence.allows.items()):
            window = check_window(
                allows, evidence.credits.get(user, []), evidence.unknown.get(user, []), policy
            )
            if window is not None:
                violation = {
                    "user_id": user,
                    "start_ns": window.start_ns,
                    "end_ns": window.end_ns,
                    "excess_units": window.excess_units,
                }
                break
    return {
        "verdict": "invalid" if reasons else "violation" if violation is not None else "pass",
        "known_credits": sum(len(times) for times in evidence.credits.values()),
        "unknown_credits": sum(len(intervals) for intervals in evidence.unknown.values()),
        "listener_drops": evidence.listener_drops,
        "pre_kill_lines": evidence.pre_kill_lines,
        "pre_kill_read_ok_false": evidence.pre_kill_read_ok_false,
        "invalid_reasons": reasons,
        "violation": violation,
    }


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 1:
        print("arguments: field path: expected one oplog.jsonl")
        return 2
    try:
        path = Path(args[0])
        if path.is_dir():
            verdict = check_run(path)
            print(json.dumps(verdict))
            return {"pass": 0, "violation": 1, "invalid": 2}[str(verdict["verdict"])]
        violation = check_oplog(path)
    except (RecordError, OSError, UnicodeError) as error:
        print(str(error))
        return 2
    if violation is not None:
        group, window = violation
        print(
            f"{group}: window [{window.start_ns}, {window.end_ns}] "
            f"exceeds capacity by at least {window.excess_units} units"
        )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
