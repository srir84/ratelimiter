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

import sys
from dataclasses import dataclass
from pathlib import Path

from refmodel import RecordError
from refmodel.model import Policy
from refmodel.replay import (
    NEUTRAL,
    NUMBERS,
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


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 1:
        print("arguments: field path: expected one oplog.jsonl")
        return 2
    try:
        violation = check_oplog(Path(args[0]))
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
