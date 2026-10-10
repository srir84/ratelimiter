"""The report builder (IMPLEMENTATION_PLAN H5 steps 2 and 3; DESIGN §B8, §B9, §C8; HARNESS §7).

Streaming design (§C8, plan choice): every *.jsonl file is read one line at a time with one
json.loads per line, and only counters and integer sample arrays (array('q'): scheduling lag,
got-conn wait, latencies, limiter, DB and refund times) are kept; each array is sorted once, at
the end, for exact nearest-rank percentiles. Budget numbers stay Python integers, and the
clocks are never mixed: decision_ns is budget time and is never subtracted from a monotonic or
wall-clock value (`clocks` names the clock behind every field).

hitgen's planned_ns and sent_ns are monotonic offsets from its t0; the wall-clock time of its
"sending" log line (hitgen.stderr, written just before it takes t0) places them on the wall
clock, where context.json's fault and rewrite windows and the API's arrival_ns live.
"""

import contextlib
import hashlib
import json
from array import array
from collections.abc import Sequence
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from refmodel import RecordError
from refmodel.check_bud11 import check_run
from refmodel.model import TBOUND
from report import InputError
from report import runfiles as rf
from report.stats import (
    NS_PER_S,
    nearest_rank,
    new_samples,
    per_second,
    percentiles,
    rate,
    step_bounds,
    step_of,
)

# §B9 numbers.
WARMUP_NS = 5 * NS_PER_S  # observation window: each step minus its first 5 s
ALLOWED_LAG_NS = 5_000_000  # step p99 of sent_ns − planned_ns above this: generator-limited
LATENCY_ALLOWANCE_NS = 50_000_000  # latency threshold: the baseline step's window p99 + 50 ms
ERROR_RATE_FLOOR = 0.001  # error threshold: max(0.1 %, 2 × the baseline step's rate)
SPIKE_RECOVERY_NS = 30 * NS_PER_S  # spike deadline: the spike step's end + 30 s
SPIKE_WINDOW_NS = 60 * NS_PER_S  # spike window: [deadline, deadline + 60 s]
# §B6, §A7 item 5: evidence limits.
MAX_OUTAGE_NS = 5 * NS_PER_S
MAX_OUTAGE_SEGMENTS = 1

# The 5xx that is not an unexpected error (§B9, §B3): a refusal after acquire.
ERR15_CODE = "service_aborted_after_acquire"
# The unknown outcomes a passing capacity step may not have (§B9, PRF-11): ERR-9 and ERR-16.
ERR9_CODE = "limiter_outcome_unknown"
ERR16_CODE = "application_outcome_unknown"

LABEL_TEST = "test-build scenario, not a benchmark"  # §B5
LABEL_BENCH = "benchmark"

REFUND_RESULTS = ("applied", "already_applied", "receipt_expired", "receipt_invalid", "unresolved")
REFUND_REASONS = ("", "closed", "retention", "expired")
# Acquire result kinds (SPEC; internal/core types.go), as hitapi writes them.
ACQUIRE_RESULTS = frozenset(
    {
        "allowed",
        "degraded",
        "limited",
        "validation_error",
        "clock_error",
        "busy",
        "cancelled",
        "config_error",
        "not_ready",
        "state_integrity_error",
        "capacity_error",
        "unavailable",
        "outcome_unknown",
        "receipt_error",
    }
)
# Limiter decision-time classes (PRF-3): every other acquire result is an error.
LIMITER_CLASSES = ("allowed", "limited", "degraded", "error")

# The HARNESS ERR table (§3) by (HTTP status, error code); ERR-1, ERR-2, ERR-3 and ERR-12d
# also need the acquire result and existed (err_row).
ERR_ROWS = (
    "ERR-1",
    "ERR-2",
    "ERR-3",
    "ERR-4",
    "ERR-5",
    "ERR-6",
    "ERR-7",
    "ERR-8",
    "ERR-9",
    "ERR-9a",
    "ERR-10",
    "ERR-11",
    "ERR-12",
    "ERR-12a",
    "ERR-12b",
    "ERR-12c",
    "ERR-12d",
    "ERR-13",
    "ERR-14",
    "ERR-15",
    "ERR-16",
)
_ERR_BY_CODE = {
    (429, "rate_limited"): "ERR-4",
    (404, "not_found"): "ERR-6",
    (405, "method_not_allowed"): "ERR-6",
    (503, "service_overloaded"): "ERR-7",
    (503, "limiter_unavailable"): "ERR-8",
    (503, "limiter_outcome_unknown"): "ERR-9",
    (503, "limiter_deadline_exceeded"): "ERR-9a",
    (503, "limiter_capacity_exceeded"): "ERR-10",
    (503, "limiter_state_integrity_error"): "ERR-11",
    (503, "limiter_config_error"): "ERR-12",
    (503, "limiter_not_ready"): "ERR-12a",
    (503, "limiter_clock_error"): "ERR-12b",
    (503, "limiter_receipt_error"): "ERR-12c",
    (500, "internal_error"): "ERR-13",
    (503, "database_unavailable"): "ERR-14",
    (503, ERR15_CODE): "ERR-15",
    (503, "application_outcome_unknown"): "ERR-16",
}

# The rows of a successful request, whose row was created or found (HTTP-10): ERR-1, ERR-2,
# and ERR-3, whose DB row is "as ERR-1/2" (HARNESS §3). Their existed flag, not the row, tells a
# new row from a duplicate avoided (ApiAcc.new_rows, ApiAcc.duplicate_rows).
SUCCESS_ROWS = frozenset({"ERR-1", "ERR-2", "ERR-3"})

# PRF-2's other result names, by the ERR rows that count them.
PRF2_NAMES = (
    ("degraded_admissions", ("ERR-3",)),
    ("quota_rejections", ("ERR-4",)),
    ("capacity_refusals", ("ERR-7", "ERR-10")),
    ("integrity_failures", ("ERR-11",)),
    ("unavailable", ("ERR-8",)),
    ("unknown_acquires", ("ERR-9",)),
    ("unknown_application_outcomes", ("ERR-16",)),
    ("server_failures", ("ERR-13", "ERR-14", "ERR-15")),
)


def window_start(bounds: Sequence[int], k: int) -> int:
    """Where step k's observation window starts (§B9: the step minus its first 5 s); a step of
    at most 5 s has an empty window, starting at its end."""
    return min(bounds[k] + WARMUP_NS, bounds[k + 1])


def err_row(status: int, error: str, result: str, existed: bool) -> str | None:
    """The ERR row of one request record, None when it matches no row."""
    if status == 200 and error == "":
        if result == "degraded":
            return "ERR-3"
        return "ERR-2" if existed else "ERR-1"
    if status == 400 and error == "invalid_request":
        return "ERR-12d" if result == "validation_error" else "ERR-5"
    return _ERR_BY_CODE.get((status, error))


def sha256_hex(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


# Windows on the wall clock.


@dataclass(frozen=True)
class Window:
    start_ns: int
    end_ns: int
    source: str

    def overlaps(self, start_ns: int, end_ns: int) -> bool:
        return start_ns <= self.end_ns and end_ns >= self.start_ns

    def to_json(self) -> dict[str, object]:
        return {"start_ns": self.start_ns, "end_ns": self.end_ns, "source": self.source}


def _fold_pairs(pairs: list[tuple[str, object]]) -> dict[str, object]:
    """A /_test/faults body's members as hitapi's JSON decoder reads them: names matched
    without regard to case, the last of a repeated name winning."""
    out: dict[str, object] = {}
    for key, value in pairs:
        out[key.casefold()] = value
    return out


@dataclass(frozen=True)
class FaultBody:
    target: str
    mode: str
    duration_ms: int


_INT64_MAX = (1 << 63) - 1
_INT64_MIN = -(1 << 63)


def fault_body(cmd: str) -> FaultBody | None:
    """A [[faults]] cmd that is a /_test/faults JSON body, decoded into hitapi's field types
    (harness/httpapi faultRequest, as the orchestrator's preflight reads it): None for a
    faultctl command or a body hitapi refuses (not one JSON object, or a field of the wrong
    type). Unknown members are ignored; null leaves a field empty."""
    if cmd.startswith("faultctl "):
        return None
    try:
        value = json.loads(cmd, object_pairs_hook=_fold_pairs)
    except (ValueError, RecursionError):
        return None
    if not isinstance(value, dict):
        return None
    strings = {}
    for key in ("target", "mode", "match", "at"):
        member = value.get(key)
        if member is not None and not isinstance(member, str):
            return None
        strings[key] = member or ""
    numbers = {}
    for key in ("count", "duration_ms", "delay_ms"):
        member = value.get(key)
        if member is None:
            numbers[key] = 0
        elif type(member) is int and _INT64_MIN <= member <= _INT64_MAX:
            numbers[key] = member
        else:
            return None  # hitapi's decoder refuses the body: it arms nothing
    return FaultBody(
        target=strings["target"], mode=strings["mode"], duration_ms=numbers["duration_ms"]
    )


def is_store_outage_cmd(cmd: str) -> bool:
    """Whether a fault is a faultctl store down or store pause, whose outage faultctl measures."""
    args = cmd.split() if cmd.startswith("faultctl ") else []
    return len(args) >= 3 and args[1] == "store" and args[2] in ("down", "pause")


def is_outage_segment(cmd: str) -> bool:
    """Whether a fault arms a store outage segment (§A7 item 5; the orchestrator's preflight
    rule): a faultctl store down or store pause, or a body with target store, mode refuse and a
    duration_ms above 0, however many instances it goes to."""
    if cmd.startswith("faultctl "):
        return is_store_outage_cmd(cmd)
    body = fault_body(cmd)
    return (
        body is not None
        and body.target == "store"
        and body.mode == "refuse"
        and body.duration_ms > 0
    )


def fault_windows(ctx: rf.Context, p_ns: int) -> list[Window]:
    """Each fault that ran (start_ns set) as the wall-clock window [start_ns, end_ns + P] (§B9
    unexpected errors; the plan's choice of window), from context.json's actual times."""
    return [
        Window(f.start_ns, max(f.end_ns, f.start_ns) + p_ns, f.cmd)
        for f in ctx.faults
        if f.start_ns > 0
    ]


# Per-step accumulators.

# §B9 verdicts on one attempt-1 request (GenAcc.classify).
TRANSPORT, UNEXPECTED, ERR15, FAULTED, NOT_ERROR = "transport", "unexpected", "err15", "faulted", ""
# End-to-end latency classes from gen.jsonl (PRF-3): attempt 1 by status, and the retries.
E2E_CLASSES = ("all", "success", "limited", "error", "retries")


@dataclass
class Unexpected:
    """§B9 unexpected errors among attempt-1 requests."""

    attempts: int = 0  # the denominator
    unexpected: int = 0
    transport: int = 0  # no HTTP response: a timeout or a transport error (unexpected)
    server_errors: int = 0  # 5xx, before the exclusions
    err15: int = 0  # 5xx service_aborted_after_acquire: excluded
    faulted: int = 0  # other 5xx overlapping a fault window: excluded

    def count(self, verdict: str) -> None:
        self.attempts += 1
        if verdict == TRANSPORT:
            self.transport += 1
            self.unexpected += 1
        elif verdict == UNEXPECTED:
            self.server_errors += 1
            self.unexpected += 1
        elif verdict == ERR15:
            self.server_errors += 1
            self.err15 += 1
        elif verdict == FAULTED:
            self.server_errors += 1
            self.faulted += 1

    def to_json(self) -> dict[str, object]:
        return {
            "attempts": self.attempts,
            "unexpected": self.unexpected,
            "rate": rate(self.unexpected, self.attempts),
            "transport_errors": self.transport,
            "server_errors": self.server_errors,
            "excluded_service_aborted_after_acquire": self.err15,
            "excluded_fault_window": self.faulted,
        }


@dataclass
class StepAcc:
    """One load step's counters and samples; gen.jsonl lines join the step their planned_ns
    falls in, API request records the step their arrival_ns falls in on hitgen's clock."""

    scheduled: int = 0  # schedule.jsonl entries
    planned: int = 0  # of them, planned before any stop_early stop (the summary's planned)
    initial: int = 0  # attempt-1 lines of initial entries
    duplicates: int = 0  # attempt-1 lines of duplicate entries (TRF-6)
    sent: int = 0  # attempt-1 lines written (sent_ns set)
    retries: int = 0  # retry lines (after 429: TRF-5, TRF-7)
    status_200: int = 0  # 200 responses, every attempt
    status_429: int = 0  # 429 responses, every attempt
    unknown_acquires: int = 0  # 503 limiter_outcome_unknown (ERR-9), every attempt
    unknown_outcomes: int = 0  # 503 application_outcome_unknown (ERR-16), every attempt
    lags: array[int] = field(default_factory=new_samples)  # sent_ns − planned_ns, attempt 1
    got_conn: array[int] = field(default_factory=new_samples)  # got_conn_wait_ns, attempt 1
    window_latency: array[int] = field(default_factory=new_samples)  # §B9 latency population
    window: Unexpected = field(default_factory=Unexpected)  # §B9 errors in the window
    api_requests: int = 0
    api_new_rows: int = 0  # a successful request that created its row (PRF-11 unique rows)
    api_allowed: int = 0
    api_limited: int = 0

    def unsent(self) -> int:
        """The step's planned first attempts never written: generator drops and failures before
        the write (PRF-1)."""
        return max(self.planned - self.sent, 0)


@dataclass
class MaxLast:
    """The largest and the last value of a series of readings."""

    max: float | None = None
    last: float | None = None
    readings: int = 0

    def add(self, value: float) -> None:
        self.readings += 1
        self.last = value
        if self.max is None or value > self.max:
            self.max = value

    def to_json(self) -> dict[str, object]:
        return {"max": _num(self.max), "last": _num(self.last), "readings": self.readings}


def _num(value: float | None) -> int | float | None:
    """A reading as JSON: an integral value as an integer."""
    if value is None:
        return None
    if value == int(value) and abs(value) < 2**53:
        return int(value)
    return value


@dataclass
class ProcStats:
    """One process's CPU % and RSS readings. The CPU sum is exact (a Fraction), so the mean of
    finite readings is finite however large they are, and rounds exactly to 2 places."""

    cpu_sum: Fraction = field(default_factory=Fraction)
    cpu_max: float | None = None
    cpu_n: int = 0
    rss_sum: int = 0
    rss_max: int | None = None
    rss_n: int = 0

    def add(self, cpu: float | None, rss: int | None) -> None:
        if cpu is not None:
            self.cpu_sum += Fraction(cpu)
            self.cpu_n += 1
            self.cpu_max = cpu if self.cpu_max is None else max(self.cpu_max, cpu)
        if rss is not None:
            self.rss_sum += rss
            self.rss_n += 1
            self.rss_max = rss if self.rss_max is None else max(self.rss_max, rss)

    def to_json(self) -> dict[str, object]:
        mean = float(round(self.cpu_sum / self.cpu_n, 2)) if self.cpu_n else None
        return {
            "cpu_pct_mean": mean,
            "cpu_pct_max": self.cpu_max,
            "rss_bytes_mean": self.rss_sum // self.rss_n if self.rss_n else None,
            "rss_bytes_max": self.rss_max,
            "cpu_readings": self.cpu_n,
            "rss_readings": self.rss_n,
        }


# samples.jsonl (H3's line format; DESIGN §B7).

SAMPLE_KEYS = frozenset(
    {
        "t_ns",
        "procs",
        "procs_error",
        "postgres",
        "postgres_error",
        "apis",
        "status",
        "status_error",
        "store",
        "store_error",
    }
)
PROC_KEYS = frozenset({"role", "pid", "cpu", "rss", "error"})
PG_KEYS = frozenset({"pid", "processes", "cpu", "rss"})
API_SAMPLE_KEYS = frozenset(
    {"instance", "addr", "health", "health_error", "metrics", "metrics_error"}
)
STORE_KEYS = frozenset({"persistence", "clients"})
# The rladmin status --json counts (ADM-4) the report follows.
STATUS_COUNTS = ("tracked_buckets", "live_markers", "quarantined_keys")
# The /metrics series the report follows per instance (PRF-4; DESIGN §A5.11), and the
# families whose every labelled series it follows.
METRIC_SERIES = (
    "rl_tracked_buckets",
    "rl_live_markers",
    "rl_quarantined_keys",
    "rl_pending_refunds",
    "rl_cleanup_lag_seconds",
    "rl_clock_lag_microseconds",
    "rl_listener_drops_total",
)
METRIC_FAMILIES = (
    "rl_cleanup_entries_total{",
    "rl_cleanup_passes_total{",
    "rl_cleanup_stops_total{",
)
HEALTH_COUNTS = ("in_flight", "listener_drops", "log_drops")


def _opt_cpu(rec: dict[str, object], where: str) -> float | None:
    return rf.get_opt_number(rec, "cpu", where)


@dataclass
class SamplesAcc:
    samples: int = 0
    procs: dict[str, ProcStats] = field(default_factory=dict)
    # Every count, read or not (a count never read has max and last null: PRF-4's fields stay).
    status: dict[str, MaxLast] = field(
        default_factory=lambda: {key: MaxLast() for key in STATUS_COUNTS}
    )
    instances: dict[int, dict[str, MaxLast]] = field(default_factory=dict)
    health_reads: int = 0
    fixtures_on: int = 0
    controlled_clock: int = 0
    rewrite_runs: list[Window] = field(default_factory=list)

    def read(self, path: Path) -> None:
        run_start: int | None = None
        run_last = 0
        for where, rec in rf.iter_jsonl(path):
            rf.exact_keys(rec, SAMPLE_KEYS, where)
            t_ns = rf.get_int(rec, "t_ns", where)
            self.samples += 1
            for key in ("procs_error", "postgres_error", "status_error", "store_error"):
                rf.get_opt_str(rec, key, where)
            for i, value in enumerate(rf.get_opt_list(rec, "procs", where)):
                self._proc(rf.as_obj(value, where, "procs"), f"{where}: procs[{i}]")
            pg = rf.get_opt_obj(rec, "postgres", where)
            if pg is not None:
                rf.exact_keys(pg, PG_KEYS, f"{where}: postgres")
                rf.get_int(pg, "pid", f"{where}: postgres", 1)
                rf.get_int(pg, "processes", f"{where}: postgres")
                stats = self.procs.setdefault("postgres", ProcStats())
                pw = f"{where}: postgres"
                stats.add(_opt_cpu(pg, pw), rf.get_int(pg, "rss", pw))
            for i, value in enumerate(rf.get_list(rec, "apis", where)):
                self._api(rf.as_obj(value, where, "apis"), f"{where}: apis[{i}]")
            status = rf.get_opt_obj(rec, "status", where)
            if status is not None:
                for key in STATUS_COUNTS:
                    self.status[key].add(rf.get_int(status, key, f"{where}: status"))
            rewriting = self._store(rf.get_opt_obj(rec, "store", where), f"{where}: store")
            if rewriting:
                if run_start is None:
                    run_start = t_ns
                run_last = t_ns
            elif run_start is not None:
                self.rewrite_runs.append(Window(run_start, run_last, "samples"))
                run_start = None
        if run_start is not None:
            self.rewrite_runs.append(Window(run_start, run_last, "samples"))

    def _proc(self, rec: dict[str, object], where: str) -> None:
        keys = PROC_KEYS | {"instance"} if "instance" in rec else PROC_KEYS
        rf.exact_keys(rec, keys, where)
        role = rf.get_str(rec, "role", where)
        rf.get_int(rec, "pid", where, 1)
        rf.get_opt_str(rec, "error", where)
        name = role
        if "instance" in rec:
            name = f"{role}-{rf.get_int(rec, 'instance', where, 1)}"
        stats = self.procs.setdefault(name, ProcStats())
        stats.add(_opt_cpu(rec, where), rf.get_opt_int(rec, "rss", where))

    def _api(self, rec: dict[str, object], where: str) -> None:
        rf.exact_keys(rec, API_SAMPLE_KEYS, where)
        instance = rf.get_int(rec, "instance", where, 1)
        rf.get_str(rec, "addr", where)
        rf.get_opt_str(rec, "health_error", where)
        rf.get_opt_str(rec, "metrics_error", where)
        series = self.instances.setdefault(instance, {})
        health = rf.get_opt_obj(rec, "health", where)
        if health is not None:
            hw = f"{where}: health"
            self.health_reads += 1
            if rf.get_bool(health, "fixtures", hw):
                self.fixtures_on += 1
            if rf.get_str(health, "clock_source", hw) != "store":
                self.controlled_clock += 1
            for key in HEALTH_COUNTS:
                series.setdefault(key, MaxLast()).add(rf.get_int(health, key, hw))
        metrics = rf.get_opt_obj(rec, "metrics", where)
        if metrics is not None:
            mw = f"{where}: metrics"
            for name, value in metrics.items():
                if value is None:
                    continue
                if type(value) is not int and type(value) is not float:
                    raise InputError(mw, name, "want a number or null")
                if name in METRIC_SERIES or name.startswith(METRIC_FAMILIES):
                    series.setdefault(name, MaxLast()).add(value)

    def _store(self, store: dict[str, object] | None, where: str) -> bool:
        """Whether the store's INFO showed aof_rewrite_in_progress:1; a failed read is not."""
        if store is None:
            return False
        rf.exact_keys(store, STORE_KEYS, where)
        for section in STORE_KEYS:
            values = rf.get_obj(store, section, where)
            for key, value in values.items():
                if not isinstance(value, str):
                    raise InputError(f"{where}: {section}", key, "want a string")
        persistence = rf.get_obj(store, "persistence", where)
        return persistence.get("aof_rewrite_in_progress") == "1"


# gen.jsonl (§B4 "Record"; cmd/hitgen attemptLine and summary).

GEN_KEYS = frozenset(
    {
        "kind",
        "planned_ns",
        "sent_ns",
        "got_conn_wait_ns",
        "instance",
        "user",
        "request_id",
        "duplicate_of",
        "attempt",
        "hint_source",
        "hint_ms",
        "jitter_ns",
        "status",
        "error",
        "latency",
        "arrival_ns",
        "decision_ns",
    }
)
SUMMARY_KEYS = frozenset(
    {
        "kind",
        "planned",
        "launched",
        "unsent",
        "log_drops",
        "stopped_after_step",
        "excluded_step",
        "skipped_no_eligible",
    }
)
HINT_SOURCES = (None, "retry_after_ms", "Retry-After")
KIND_INITIAL, KIND_DUPLICATE, KIND_RETRY = "initial", "duplicate", "retry"


def _opt_nonneg(rec: dict[str, object], key: str, where: str) -> int | None:
    """An integer from 0 to 2^63 − 1 (int64, what array('q') holds), or null."""
    value = rec[key]
    if value is None:
        return None
    if type(value) is not int or not 0 <= value <= _INT64_MAX:
        raise InputError(where, key, "want an integer from 0 to 2^63 − 1, or null")
    return value


@dataclass(frozen=True)
class Summary:
    planned: int
    launched: int
    unsent: int
    log_drops: int
    stopped_after_step: int | None
    excluded_step: int | None
    skipped_no_eligible: int

    def to_json(self) -> dict[str, object]:
        return {
            "planned": self.planned,
            "launched": self.launched,
            "unsent": self.unsent,
            "log_drops": self.log_drops,
            "stopped_after_step": self.stopped_after_step,
            "excluded_step": self.excluded_step,
            "skipped_no_eligible": self.skipped_no_eligible,
        }


def read_summary(rec: dict[str, object], where: str, steps: int) -> Summary:
    rf.exact_keys(rec, SUMMARY_KEYS, where)
    stopped = rf.get_opt_int(rec, "stopped_after_step", where)
    excluded = rf.get_opt_int(rec, "excluded_step", where)
    if (stopped is None) != (excluded is None):
        raise InputError(where, "excluded_step", "set exactly when stopped_after_step is")
    if stopped is not None and excluded is not None and not stopped < excluded < steps:
        raise InputError(where, "excluded_step", "want a step after stopped_after_step")
    return Summary(
        planned=rf.get_int(rec, "planned", where),
        launched=rf.get_int(rec, "launched", where),
        unsent=rf.get_int(rec, "unsent", where),
        log_drops=rf.get_int(rec, "log_drops", where),
        stopped_after_step=stopped,
        excluded_step=excluded,
        skipped_no_eligible=rf.get_int(rec, "skipped_no_eligible", where),
    )


@dataclass(frozen=True)
class Attempt:
    """One checked gen.jsonl attempt line: the fields the report reads."""

    kind: str
    planned_ns: int
    sent_ns: int | None
    got_conn_wait_ns: int | None
    instance: int
    user: str
    status: int
    error: str | None
    latency: int | None


def check_attempt(rec: dict[str, object], where: str, instances: int) -> Attempt:
    """Validates a gen.jsonl attempt line against the golden shape and hitgen's rules: the
    first attempt carries no hint, a retry names its hint and jitter, latency is set exactly
    when the request was written, and a response implies a written request."""
    if rec.keys() != GEN_KEYS:
        rf.exact_keys(rec, GEN_KEYS, where)
    kind = rec["kind"]
    if kind != KIND_INITIAL and kind != KIND_DUPLICATE and kind != KIND_RETRY:
        raise InputError(where, "kind", f"{kind!r}: want initial, duplicate, retry or summary")
    kind_s = str(kind)
    planned = rec["planned_ns"]
    if type(planned) is not int or not 0 <= planned <= _INT64_MAX:
        raise InputError(where, "planned_ns", "want an integer from 0 to 2^63 − 1")
    sent = _opt_nonneg(rec, "sent_ns", where)
    # The scheduling lag goes into an int64 sample array.
    if sent is not None and not _INT64_MIN <= sent - planned <= _INT64_MAX:
        raise InputError(where, "sent_ns", "sent_ns − planned_ns beyond int64")
    latency = _opt_nonneg(rec, "latency", where)
    got_conn = _opt_nonneg(rec, "got_conn_wait_ns", where)
    _opt_nonneg(rec, "arrival_ns", where)
    _opt_nonneg(rec, "decision_ns", where)
    hint_ms = _opt_nonneg(rec, "hint_ms", where)
    jitter = _opt_nonneg(rec, "jitter_ns", where)
    duplicate_of = _opt_nonneg(rec, "duplicate_of", where)
    instance = rec["instance"]
    if type(instance) is not int or not 1 <= instance <= instances:
        raise InputError(where, "instance", f"want 1 to {instances}")
    user = rec["user"]
    if not isinstance(user, str) or not user:
        raise InputError(where, "user", "want a non-empty string")
    request_id = rec["request_id"]
    if not isinstance(request_id, str) or not request_id:
        raise InputError(where, "request_id", "want a non-empty string")
    attempt = rec["attempt"]
    if type(attempt) is not int or attempt < 1:
        raise InputError(where, "attempt", "want an integer of at least 1")
    if (kind_s == KIND_RETRY) != (attempt > 1):
        raise InputError(where, "attempt", f"{attempt} on a {kind_s} line")
    if attempt == 1 and (kind_s == KIND_DUPLICATE) != (duplicate_of is not None):
        raise InputError(where, "duplicate_of", "set exactly on a duplicate entry's lines")
    hint_source = rec["hint_source"]
    if hint_source not in HINT_SOURCES:
        raise InputError(where, "hint_source", f"{hint_source!r}: want null or a hint source")
    if attempt == 1 and (hint_source is not None or hint_ms is not None or jitter is not None):
        raise InputError(where, "hint_source", "a first attempt has no hint or jitter")
    if attempt > 1 and (hint_source is None or hint_ms is None or jitter is None):
        raise InputError(where, "hint_source", "a retry names its hint and jitter")
    status = rec["status"]
    if type(status) is not int or not (status == 0 or 100 <= status <= 599):
        raise InputError(where, "status", "want 0 or an HTTP status")
    error = rec["error"]
    if error is not None and not isinstance(error, str):
        raise InputError(where, "error", "want a string or null")
    if (sent is None) != (latency is None):
        raise InputError(where, "latency", "set exactly when sent_ns is")
    if status != 0 and sent is None:
        raise InputError(where, "sent_ns", "a response to a request never written")
    if status == 0 and error is None:
        raise InputError(where, "error", "an attempt with no response names its failure")
    return Attempt(kind_s, planned, sent, got_conn, instance, user, status, error, latency)


# The spike recovery (§B9 "Spike recovery", PRF-6; plan H5 step 4).


@dataclass(frozen=True)
class SpikeParams:
    """A spike scenario's recovery deadline and window, on hitgen's clock (ns from its t0, as
    planned_ns and the step bounds): deadline = the spike step's end + 30 s, window = [deadline,
    deadline + 60 s], judged against the post-spike step's thresholds. report/thresholds.py
    records them with the thresholds, before the limiter run (PRF-6)."""

    spike_step: int
    post_spike_step: int
    spike_end_ns: int
    deadline_ns: int
    window_start_ns: int
    window_end_ns: int

    def to_json(self) -> dict[str, object]:
        return {
            "spike_step": self.spike_step,
            "post_spike_step": self.post_spike_step,
            "spike_end_ns": self.spike_end_ns,
            "deadline_ns": self.deadline_ns,
            "window_start_ns": self.window_start_ns,
            "window_end_ns": self.window_end_ns,
        }


SPIKE_KEYS = (
    "spike_step",
    "post_spike_step",
    "spike_end_ns",
    "deadline_ns",
    "window_start_ns",
    "window_end_ns",
)


def spike_params(sc: rf.Scenario, bounds: Sequence[int]) -> SpikeParams | None:
    """The recovery deadline and window of a scenario with verdict spike and a spike_step (the
    scenario reader keeps spike_step before the last step, so a post-spike step exists); None
    for any other scenario."""
    if sc.verdict != "spike" or sc.spike_step is None:
        return None
    s = sc.spike_step
    end = bounds[s + 1]
    deadline = end + SPIKE_RECOVERY_NS
    return SpikeParams(s, s + 1, end, deadline, deadline, deadline + SPIKE_WINDOW_NS)


def spike_from_json(rec: dict[str, object], where: str, n_steps: int) -> SpikeParams:
    """Recorded spike parameters (a thresholds file's spike object), checked for shape."""
    values = [rf.get_int(rec, key, where) for key in SPIKE_KEYS]
    params = SpikeParams(*values)
    if not params.spike_step < params.post_spike_step < n_steps:
        raise InputError(where, "post_spike_step", "want a step of the scenario after spike_step")
    return params


@dataclass
class SpikeAcc:
    """The spike recovery's evidence in gen.jsonl, one attempt-1 line at a time. The attempt-1
    requests planned in the window (closed, [deadline, deadline + 60 s], as §B9 writes it) make
    its latency population (those with an HTTP response) and its error denominator, as a step's
    observation window does.

    The outstanding rule, the orchestrator's reading of plan H5 step 4 under PRF-6 (the spike's
    backlog clears by the deadline; read literally, the step would also count the ordinary
    requests still in flight at the deadline): an attempt-1 request planned before cutoff_ns
    (the deadline − the post-spike step's latency_threshold_ns from the frozen thresholds) is
    outstanding when no response or failure was logged by the deadline. A written one ended at
    sent_ns + latency (latency runs from the write to the response or the failure, both on
    hitgen's clock), so it is outstanding when that is after the deadline; one written and
    planned in [cutoff_ns, deadline) is not judged. Every attempt-1 request planned before the
    deadline and never written (sent_ns null) is outstanding, inside [cutoff_ns, deadline) too;
    an entry with no line at all (a generator drop) is counted from the schedule
    (ScheduleAcc)."""

    params: SpikeParams
    cutoff_ns: int  # spike_cutoff: the deadline − the post-spike step's latency threshold
    latency: array[int] = field(default_factory=new_samples)
    window: Unexpected = field(default_factory=Unexpected)
    written_in_window: int = 0  # window attempt-1 lines written (sent_ns set)
    lines_before: int = 0  # attempt-1 lines planned before the deadline
    unwritten: int = 0  # of them, never written (sent_ns null): outstanding
    finished: int = 0  # of them, written, planned before cutoff_ns and ended by the deadline
    late: int = 0  # of them, written, planned before cutoff_ns and ended after the deadline
    not_judged: int = 0  # of them, written and planned in [cutoff_ns, deadline)

    def add(self, a: Attempt, verdict: str) -> None:
        p = self.params
        if a.planned_ns < p.deadline_ns:
            self.lines_before += 1
            if a.sent_ns is None or a.latency is None:
                self.unwritten += 1
            elif a.planned_ns >= self.cutoff_ns:
                self.not_judged += 1
            elif a.sent_ns + a.latency <= p.deadline_ns:
                self.finished += 1
            else:
                self.late += 1
        if p.window_start_ns <= a.planned_ns <= p.window_end_ns:
            self.window.count(verdict)
            if a.sent_ns is not None:
                self.written_in_window += 1
            if a.status != 0 and a.latency is not None:
                self.latency.append(a.latency)


@dataclass
class GenAcc:
    """What gen.jsonl gives the report. Attempt 1 (an initial or a duplicate entry's first
    attempt) makes the §B9 populations: its latency when it got an HTTP response (status ≠ 0;
    every written attempt has a latency, to its response or to its failure, so the filter is on
    the status), and the error denominator; retries are counted apart (TRF-7)."""

    bounds: Sequence[int]
    instances: int
    t0_wall_ns: int
    faults: Sequence[Window]
    rewrites: Sequence[Window]
    steps: list[StepAcc]
    unexpected: Unexpected = field(default_factory=Unexpected)
    outcomes: dict[tuple[str, str], int] = field(default_factory=dict)
    transport: dict[tuple[str, str], int] = field(default_factory=dict)
    per_instance: list[int] = field(default_factory=list)
    users: dict[str, int] = field(default_factory=dict)
    user_instances: dict[str, int] = field(default_factory=dict)
    e2e: dict[str, array[int]] = field(default_factory=dict)
    rewrite_latency: array[int] = field(default_factory=new_samples)
    spike: SpikeAcc | None = None  # a spike scenario's recovery evidence
    summary: Summary | None = None
    lines: int = 0

    def read(self, path: Path) -> None:
        bounds, steps, spike = self.bounds, self.steps, self.spike
        window_from = [window_start(bounds, k) for k in range(len(steps))]
        self.per_instance = [0] * (self.instances + 1)
        for name in E2E_CLASSES:
            self.e2e[name] = new_samples()
        e2e = self.e2e
        outcomes, users, masks = self.outcomes, self.users, self.user_instances
        for where, rec in rf.iter_jsonl(path):
            if self.summary is not None:
                raise InputError(where, "kind", "a line after the summary")
            if rec.get("kind") == "summary":
                self.summary = read_summary(rec, where, len(steps))
                continue
            self.lines += 1
            a = check_attempt(rec, where, self.instances)
            k = step_of(bounds, a.planned_ns)
            st = steps[k]
            status = a.status
            okey = (a.kind, str(status) if a.error is None else f"{status} {a.error}")
            outcomes[okey] = outcomes.get(okey, 0) + 1
            if status == 200:
                st.status_200 += 1
            elif status == 429:
                st.status_429 += 1
            elif status == 0:
                tkey = (a.kind, str(a.error))
                self.transport[tkey] = self.transport.get(tkey, 0) + 1
            elif a.error == ERR9_CODE:
                st.unknown_acquires += 1
            elif a.error == ERR16_CODE:
                st.unknown_outcomes += 1
            responded = status != 0 and a.latency is not None and a.sent_ns is not None
            if a.kind == KIND_RETRY:
                st.retries += 1
                if responded and a.latency is not None:
                    e2e["retries"].append(a.latency)
                continue
            # Attempt 1: the §B9 populations.
            if a.kind == KIND_INITIAL:
                st.initial += 1
            else:
                st.duplicates += 1
            self.per_instance[a.instance] += 1
            users[a.user] = users.get(a.user, 0) + 1
            masks[a.user] = masks.get(a.user, 0) | (1 << a.instance)
            if a.sent_ns is not None:
                st.sent += 1
                st.lags.append(a.sent_ns - a.planned_ns)
            if a.got_conn_wait_ns is not None:
                st.got_conn.append(a.got_conn_wait_ns)
            in_window = a.planned_ns >= window_from[k]
            if responded and a.latency is not None and a.sent_ns is not None:
                e2e["all"].append(a.latency)
                if status == 200:
                    e2e["success"].append(a.latency)
                elif status == 429:
                    e2e["limited"].append(a.latency)
                else:
                    e2e["error"].append(a.latency)
                if in_window:
                    st.window_latency.append(a.latency)
                if self.rewrites:
                    begin = self.t0_wall_ns + a.sent_ns
                    end = begin + a.latency
                    if any(w.overlaps(begin, end) for w in self.rewrites):
                        self.rewrite_latency.append(a.latency)
            verdict = self.classify(a)
            self.unexpected.count(verdict)
            if in_window:
                st.window.count(verdict)
            if spike is not None:
                spike.add(a, verdict)

    def classify(self, a: Attempt) -> str:
        """§B9 for one attempt-1 request: TRANSPORT (no HTTP response: a timeout or a transport
        error, always unexpected), UNEXPECTED (a 5xx that neither an injected fault nor a
        refusal after acquire explains), ERR15 (a 5xx service_aborted_after_acquire), FAULTED
        (any other 5xx whose wall-clock span, from its write to its response, overlaps a fault
        window), or NOT_ERROR."""
        if a.status == 0:
            return TRANSPORT
        if not 500 <= a.status <= 599:
            return NOT_ERROR
        if a.error == ERR15_CODE:
            return ERR15
        if self.faults and a.sent_ns is not None and a.latency is not None:
            begin = self.t0_wall_ns + a.sent_ns
            end = begin + a.latency
            if any(w.overlaps(begin, end) for w in self.faults):
                return FAULTED
        return UNEXPECTED

    def free(self) -> None:
        """Drops the sample arrays once they are summarized."""
        self.e2e = {}
        self.rewrite_latency = new_samples()
        if self.spike is not None:
            self.spike.latency = new_samples()
        for st in self.steps:
            st.lags, st.got_conn, st.window_latency = new_samples(), new_samples(), new_samples()


# api-N.jsonl (DESIGN §B8; harness/httpapi records.go; the golden lines).

API_KEYS = {
    "decision": frozenset({"kind", "deduction_id", "user_id", "decision_ns", "result"}),
    "request": frozenset(
        {
            "kind",
            "run_id",
            "epoch_id",
            "instance",
            "user_id",
            "request_id",
            "arrival_ns",
            "decision_ns",
            "deduction_id",
            "result",
            "existed",
            "http_status",
            "error",
            "limiter_ns",
            "db_ns",
            "total_ns",
        }
    ),
    "release": frozenset({"kind", "deduction_id", "release_mono_ns"}),
    "refund": frozenset(
        {
            "kind",
            "deduction_id",
            "result",
            "reason",
            "attempts",
            "credit_decision_ns",
            "release_mono_ns",
            "final_mono_ns",
        }
    ),
}
LIFECYCLE_KEYS = {
    "start": frozenset(
        {
            "kind",
            "event",
            "max_in_flight",
            "queue_len",
            "max_release_delay_ns",
            "dependency_deadline_ns",
            "drain_timeout_ns",
            "nofile_limit",
        }
    ),
    "ready": frozenset({"kind", "event"}),
    "stop": frozenset({"kind", "event", "listener_drops", "log_drops"}),
    "pre_kill": frozenset({"kind", "event", "listener_drops", "log_drops", "read_ok"}),
}


def _budget_ns(rec: dict[str, object], key: str, where: str) -> int:
    """A budget time (decision_ns, credit_decision_ns): the store's clock in µs × 1,000, below
    TBOUND, as refmodel's BUD-11a check reads it (API-4); 0 where the record has none."""
    value = rf.get_int(rec, key, where)
    if value % 1_000 or value >= TBOUND:
        raise InputError(where, key, f"{value}: want budget time, µs × 1,000 below {TBOUND}")
    return value


@dataclass
class ApiAcc:
    bounds: Sequence[int]
    t0_wall_ns: int
    steps: list[StepAcc]
    err: dict[str, int] = field(default_factory=lambda: dict.fromkeys(ERR_ROWS, 0))
    unmatched: dict[str, int] = field(default_factory=dict)
    requests: int = 0
    new_rows: int = 0  # successful requests (SUCCESS_ROWS) with existed false
    duplicate_rows: int = 0  # successful requests with existed true
    per_instance: dict[int, int] = field(default_factory=dict)
    decisions: dict[str, int] = field(default_factory=lambda: {"allowed": 0, "degraded": 0})
    releases: int = 0
    refunds: dict[str, int] = field(default_factory=lambda: dict.fromkeys(REFUND_RESULTS, 0))
    unresolved_reasons: dict[str, int] = field(default_factory=dict)
    limiter: dict[str, array[int]] = field(
        default_factory=lambda: {c: new_samples() for c in LIMITER_CLASSES}
    )
    db: array[int] = field(default_factory=new_samples)
    total: array[int] = field(default_factory=new_samples)
    refund_ns: dict[str, array[int]] = field(
        default_factory=lambda: {r: new_samples() for r in ("all", *REFUND_RESULTS)}
    )
    listener_drops: int = 0
    log_drops: int = 0
    stop_lines: int = 0
    pre_kill_lines: int = 0
    pre_kill_unread: int = 0
    segments: int = 0
    segments_unread: int = 0  # process segments with no stop and no pre_kill line
    lines: int = 0

    def read(self, path: Path, instance: int) -> None:
        """One instance's run log. Each process segment (from a start line, or from the file's
        first line, whatever it is) must end with a stop or a pre_kill line, which carries its
        drop counts (HAR-13); a segment without one, an empty file included, counts in
        segments_unread."""
        self.per_instance.setdefault(instance, 0)
        started = terminal = False
        for where, rec in rf.iter_jsonl(path):
            self.lines += 1
            kind = rec.get("kind")
            if kind == "lifecycle":
                event = rf.get_str(rec, "event", where)
                keys = LIFECYCLE_KEYS.get(event)
                if keys is None:
                    raise InputError(where, "event", f"{event!r}: not a lifecycle event")
                rf.exact_keys(rec, keys, where)
                if event == "start":
                    for key in sorted(keys - {"kind", "event"}):
                        rf.get_int(rec, key, where)
                    if started and not terminal:
                        self.segments_unread += 1
                    self.segments += 1
                    started, terminal = True, False
                    continue
                if not started:  # the file's implicit first segment
                    self.segments += 1
                    started = True
                if event in ("stop", "pre_kill"):
                    self.listener_drops += rf.get_int(rec, "listener_drops", where)
                    self.log_drops += rf.get_int(rec, "log_drops", where)
                    terminal = True
                    if event == "stop":
                        self.stop_lines += 1
                    else:
                        self.pre_kill_lines += 1
                        if not rf.get_bool(rec, "read_ok", where):
                            self.pre_kill_unread += 1
                continue
            keys = API_KEYS.get(kind) if isinstance(kind, str) else None
            if keys is None:
                raise InputError(where, "kind", f"{kind!r}: not a run-log record kind")
            if rec.keys() != keys:
                rf.exact_keys(rec, keys, where)
            if not started:
                self.segments += 1
                started, terminal = True, False
            if kind == "request":
                self._request(rec, where, instance)
            elif kind == "decision":
                rf.get_str(rec, "deduction_id", where)
                rf.get_str(rec, "user_id", where)
                _budget_ns(rec, "decision_ns", where)
                result = rf.get_str(rec, "result", where)
                if result not in self.decisions:
                    raise InputError(where, "result", f"{result!r}: want allowed or degraded")
                self.decisions[result] += 1
            elif kind == "release":
                rf.get_str(rec, "deduction_id", where)
                rf.get_int(rec, "release_mono_ns", where)
                self.releases += 1
            else:
                self._refund(rec, where)
        if not started:  # an empty file: a process that logged nothing, not even its drops
            self.segments += 1
        if not terminal:
            self.segments_unread += 1

    def _request(self, rec: dict[str, object], where: str, instance: int) -> None:
        for key in ("run_id", "epoch_id", "user_id", "request_id", "deduction_id"):
            rf.get_str(rec, key, where)
        if rf.get_int(rec, "instance", where, 1) != instance:
            raise InputError(where, "instance", f"want {instance}, the file's instance")
        arrival = rf.get_int(rec, "arrival_ns", where)
        _budget_ns(rec, "decision_ns", where)
        result = rf.get_str(rec, "result", where)
        if result and result not in ACQUIRE_RESULTS:
            raise InputError(where, "result", f"{result!r}: not an acquire result")
        existed = rf.get_bool(rec, "existed", where)
        status = rf.get_int(rec, "http_status", where, 100)
        if status > 599:
            raise InputError(where, "http_status", f"{status}: want an HTTP status")
        error = rf.get_str(rec, "error", where)
        limiter_ns = rf.get_int(rec, "limiter_ns", where)
        db_ns = rf.get_int(rec, "db_ns", where)
        total_ns = rf.get_int(rec, "total_ns", where)
        self.requests += 1
        self.per_instance[instance] += 1
        row = err_row(status, error, result, existed)
        if row is None:
            key = f"{status} {error}".strip()
            self.unmatched[key] = self.unmatched.get(key, 0) + 1
        else:
            self.err[row] += 1
        if result:
            cls = result if result in ("allowed", "limited", "degraded") else "error"
            self.limiter[cls].append(limiter_ns)
        if db_ns > 0:
            self.db.append(db_ns)
        self.total.append(total_ns)
        st = self.steps[step_of(self.bounds, arrival - self.t0_wall_ns)]
        st.api_requests += 1
        if row in SUCCESS_ROWS:
            if existed:
                self.duplicate_rows += 1
            else:
                self.new_rows += 1
                st.api_new_rows += 1
        if result == "allowed":
            st.api_allowed += 1
        elif result == "limited":
            st.api_limited += 1

    def _refund(self, rec: dict[str, object], where: str) -> None:
        rf.get_str(rec, "deduction_id", where)
        result = rf.get_str(rec, "result", where)
        if result not in self.refunds:
            raise InputError(where, "result", f"{result!r}: not a refund result")
        reason = rf.get_str(rec, "reason", where)
        if reason not in REFUND_REASONS:
            raise InputError(where, "reason", f"{reason!r}: not an unresolved reason")
        rf.get_int(rec, "attempts", where)
        _budget_ns(rec, "credit_decision_ns", where)
        released = rf.get_int(rec, "release_mono_ns", where)
        final = rf.get_int(rec, "final_mono_ns", where)
        if final < released:
            raise InputError(where, "final_mono_ns", "before release_mono_ns")
        # The refund time goes into an int64 sample array.
        if final - released > _INT64_MAX:
            raise InputError(where, "final_mono_ns", "final_mono_ns − release_mono_ns beyond int64")
        self.refunds[result] += 1
        if result == "unresolved":
            self.unresolved_reasons[reason] = self.unresolved_reasons.get(reason, 0) + 1
        # Release to final, both on the limiter's monotonic clock (PRF-3 refund time).
        self.refund_ns["all"].append(final - released)
        self.refund_ns[result].append(final - released)

    def free(self) -> None:
        """Drops the sample arrays once they are summarized."""
        self.limiter, self.refund_ns = {}, {}
        self.db, self.total = new_samples(), new_samples()


# schedule.jsonl (§B4; cmd/hitgen scheduleLine): the planned arrivals, hashed as read.

SCHEDULE_KEYS = frozenset(
    {"i", "planned_ns", "step", "user", "request_id", "instance", "kind", "duplicate_of"}
)


@dataclass
class ScheduleAcc:
    sha256: str = ""
    entries: int = 0
    before_deadline: int = 0  # planned entries planned before the spike deadline
    in_spike_window: int = 0  # planned entries planned in the spike window

    def read(
        self,
        path: Path,
        bounds: Sequence[int],
        steps: list[StepAcc],
        planned: int | None,
        instances: int,
        spike: SpikeParams | None = None,
    ) -> None:
        """Counts each step's entries, and those among the first `planned` (the summary's
        planned: the entries planned before a stop_early stop), of which, for a spike, those
        planned before its deadline and in its window; checks the order and that each entry's
        step is the one its planned_ns falls in under the scenario's steps."""
        digest = hashlib.sha256()
        previous = 0
        for where, rec in rf.iter_jsonl(path, digest.update):
            if rec.keys() != SCHEDULE_KEYS:
                rf.exact_keys(rec, SCHEDULE_KEYS, where)
            if rec["i"] != self.entries or type(rec["i"]) is not int:
                raise InputError(where, "i", f"want {self.entries}: entries in order")
            planned_ns = rf.get_int(rec, "planned_ns", where)
            if planned_ns < previous:
                raise InputError(where, "planned_ns", "before the previous entry's")
            previous = planned_ns
            step = rf.get_int(rec, "step", where)
            if step != step_of(bounds, planned_ns) or planned_ns >= bounds[-1]:
                raise InputError(where, "step", f"{step}: not the scenario step of its planned_ns")
            if rf.get_str(rec, "kind", where) not in (KIND_INITIAL, KIND_DUPLICATE):
                raise InputError(where, "kind", "want initial or duplicate")
            for key in ("user", "request_id"):
                if not rf.get_str(rec, key, where):
                    raise InputError(where, key, "want a non-empty string")
            if not 1 <= rf.get_int(rec, "instance", where, 1) <= instances:
                raise InputError(where, "instance", f"want 1 to {instances}")
            rf.get_opt_int(rec, "duplicate_of", where)
            steps[step].scheduled += 1
            if planned is None or self.entries < planned:
                steps[step].planned += 1
                if spike is not None:
                    if planned_ns < spike.deadline_ns:
                        self.before_deadline += 1
                    if spike.window_start_ns <= planned_ns <= spike.window_end_ns:
                        self.in_spike_window += 1
            self.entries += 1
        self.sha256 = digest.hexdigest()


# The frozen thresholds (§B9; report/thresholds.py writes them, H3's orchestrator copies them into
# the run directory as thresholds.json and records their SHA-256 in context.json).


@dataclass(frozen=True)
class StepThreshold:
    latency_ns: int  # the most the step's window p99 may be
    error_rate: float  # the most the step's window unexpected-error rate may be


@dataclass(frozen=True)
class Thresholds:
    """A thresholds file as hitgen reads it (cmd/hitgen loadThresholds: a non-empty "steps" list,
    each naming its 0-based "step" and either "generator_limited": true, no thresholds, or both
    "latency_threshold_ns" and "error_rate_threshold" in [0, 1]; a step it does not name has
    none), with what report/thresholds.py records beside them: "created_ns", the baseline that
    froze them ("baseline_run_id") and its schedule ("schedule_sha256"), and the spike recovery
    parameters ("spike", null for other scenarios). The steps also stand for the baseline that
    set them (§B9 step 3, PRF-7): a step with thresholds had a baseline step that ran in full
    and was not generator-limited, one marked generator_limited had a generator-limited one, and
    one not named had none."""

    sha256: str
    created_ns: int
    baseline_run_id: str | None  # the baseline that froze them; None when the file names none
    schedule_sha256: str | None  # the baseline's schedule; None when the file names none
    steps: dict[int, StepThreshold]  # the steps with thresholds
    limited: frozenset[int]  # the steps marked generator_limited
    spike: SpikeParams | None

    def baseline_step(self, k: int) -> tuple[bool, bool]:
        """Step k's (baseline step exists, baseline step generator-limited)."""
        return (k in self.steps or k in self.limited, k in self.limited)


def parse_thresholds(data: bytes, where: str, n_steps: int) -> Thresholds:
    """A thresholds file's bytes, checked for the shape hitgen and the report read."""
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as err:
        raise InputError(where, "json", "not UTF-8") from err
    doc = rf.decode(text, where)
    if not isinstance(doc, dict):
        raise InputError(where, "json", "not a JSON object")
    created = rf.get_int(doc, "created_ns", where)
    raw_steps = rf.get_list(doc, "steps", where)
    if not raw_steps:
        raise InputError(where, "steps", "none (hitgen refuses such a file)")
    steps: dict[int, StepThreshold] = {}
    limited: set[int] = set()
    for i, value in enumerate(raw_steps):
        sw = f"{where}: steps[{i}]"
        rec = rf.as_obj(value, sw, "steps")
        k = rf.get_int(rec, "step", sw)
        if k >= n_steps:
            raise InputError(sw, "step", f"{k}: not a step of the scenario (0 to {n_steps - 1})")
        if k in steps or k in limited:
            raise InputError(sw, "step", f"step {k} appears twice")
        if rec.get("generator_limited") is not None and rf.get_bool(rec, "generator_limited", sw):
            limited.add(k)
            continue
        latency = rf.get_int(rec, "latency_threshold_ns", sw, 1)
        error_rate = rf.get_number(rec, "error_rate_threshold", sw)
        if not 0 <= error_rate <= 1:
            raise InputError(sw, "error_rate_threshold", f"{error_rate}: want 0 to 1")
        steps[k] = StepThreshold(latency, error_rate)
    raw_spike = rf.get_opt_obj(doc, "spike", where)
    spike = None if raw_spike is None else spike_from_json(raw_spike, f"{where}: spike", n_steps)
    schedule = rf.get_opt_str(doc, "schedule_sha256", where)
    baseline = rf.get_opt_str(doc, "baseline_run_id", where) if "baseline_run_id" in doc else None
    if baseline is not None and not rf.RUN_ID.fullmatch(baseline):
        raise InputError(where, "baseline_run_id", f"{baseline!r}: not a run ID")
    return Thresholds(
        sha256_hex(data), created, baseline, schedule, steps, frozenset(limited), spike
    )


def run_thresholds(run_dir: Path, n_steps: int) -> Thresholds | None:
    """The run's copy of its frozen thresholds (runs/<id>/thresholds.json), None when the run
    copied none. A copy that is there but malformed is malformed evidence (InputError)."""
    path = run_dir.joinpath(rf.THRESHOLDS)
    if not path.exists():
        return None
    return parse_thresholds(rf.read_bytes(path), rf.THRESHOLDS, n_steps)


def same_schedule(th: Thresholds | None, ctx: rf.Context) -> Thresholds | None:
    """th when it was frozen from this run's schedule (its schedule_sha256 is context.json's),
    else None: a comparison is valid only on the same schedule (plan H5 step 6)."""
    if th is None or th.schedule_sha256 is None or th.schedule_sha256 != ctx.schedule_sha256:
        return None
    return th


def spike_cutoff(params: SpikeParams, th: Thresholds | None) -> int:
    """The outstanding rule's cutoff (SpikeAcc): the deadline − the post-spike step's
    latency_threshold_ns in the frozen thresholds th (None when the run has none it can use),
    or the deadline itself when they give that step none (the spike is not met then anyway)."""
    limits = th.steps.get(params.post_spike_step) if th is not None else None
    return params.deadline_ns - (limits.latency_ns if limits is not None else 0)


# The report's sections.


@dataclass
class Inputs:
    run_dir: Path
    ctx: rf.Context
    cfg: rf.RunConfig
    scenario: rf.Scenario
    bounds: list[int]
    hitgen: rf.HitgenLog
    samples: SamplesAcc
    gen: GenAcc
    api: ApiAcc
    schedule: ScheduleAcc
    steps: list[StepAcc]
    db_rows: int
    pids: list[dict[str, object]] | None
    fault_windows: list[Window]
    rewrite_windows: list[Window]
    spike: SpikeParams | None = None
    thresholds: Thresholds | None = None  # the run's copy of its frozen thresholds


def is_bench_limiter(ctx: rf.Context, cfg: rf.RunConfig) -> bool:
    """Whether the run is a benchmark limiter run (run_kind bench, the limiter on): the runs that
    compare against a baseline, and, with verdict capacity or spike, give capacity verdicts."""
    return ctx.run_kind == "bench" and not ctx.baseline and cfg.limiter_enabled


def is_capacity_run(ctx: rf.Context, cfg: rf.RunConfig, sc: rf.Scenario) -> bool:
    """Whether the run gets capacity verdicts: a benchmark limiter run of a scenario with verdict
    capacity or spike."""
    return is_bench_limiter(ctx, cfg) and sc.verdict in ("capacity", "spike")


def _lag_judged_p99(st: StepAcc, ordered: Sequence[int]) -> int | None:
    """The step's p99 scheduling lag as hitgen's stop_early judges it: over every planned first
    attempt, an unwritten one (a generator drop or a failure before the write) counting as above
    any bound (None); 0 for a step with no planned first attempt."""
    total = max(st.planned, len(ordered))
    if total == 0:
        return 0
    rank = (99 * total + 99) // 100
    return ordered[rank - 1] if rank <= len(ordered) else None


def load_section(inp: Inputs) -> dict[str, object]:
    sc, summary = inp.scenario, inp.gen.summary
    stopped = summary.stopped_after_step if summary is not None else None
    excluded = summary.excluded_step if summary is not None else None
    steps_json = []
    all_lags, all_got_conn = new_samples(), new_samples()
    totals = dict.fromkeys(
        ("scheduled", "planned", "sent", "initial", "duplicates", "retries_429"), 0
    )
    for k, st in enumerate(inp.steps):
        ordered = sorted(st.lags)
        lag = {
            "count": len(ordered),
            "p50": nearest_rank(ordered, 50) if ordered else None,
            "p95": nearest_rank(ordered, 95) if ordered else None,
            "p99": nearest_rank(ordered, 99) if ordered else None,
        }
        judged = _lag_judged_p99(st, ordered)
        got_conn = percentiles(st.got_conn)
        all_lags.extend(st.lags)
        all_got_conn.extend(st.got_conn)
        duration_ns = inp.bounds[k + 1] - inp.bounds[k]
        if summary is None:
            ran: str | None = None
        elif excluded is None or k < excluded:
            ran = "full"
        elif k == excluded:
            ran = "partial"
        else:
            ran = "none"
        steps_json.append(
            {
                "step": k,
                "rate_per_s": sc.steps[k].rate_per_s,
                "duration_s": sc.steps[k].duration_s,
                "start_ns": inp.bounds[k],
                "end_ns": inp.bounds[k + 1],
                "ran": ran,
                "scheduled": st.scheduled,
                "planned": st.planned,
                "sent": st.sent,
                "initial": st.initial,
                "duplicates": st.duplicates,
                "retries_429": st.retries,
                "unsent": st.unsent(),
                "achieved_sent_per_s": per_second(st.sent, duration_ns),
                "offered_per_s": per_second(st.planned, duration_ns),
                "sched_lag_ns": lag,
                "got_conn_wait_ns_p99": got_conn["p99"],
                "generator_limited": judged is None or judged > ALLOWED_LAG_NS,
                "status_200": st.status_200,
                "status_429": st.status_429,
                "unknown_acquires": st.unknown_acquires,
                "unknown_application_outcomes": st.unknown_outcomes,
            }
        )
        totals["scheduled"] += st.scheduled
        totals["planned"] += st.planned
        totals["sent"] += st.sent
        totals["initial"] += st.initial
        totals["duplicates"] += st.duplicates
        totals["retries_429"] += st.retries
    total: dict[str, object] = dict(totals)
    total["unsent"] = summary.unsent if summary is not None else totals["planned"] - totals["sent"]
    total["sched_lag_ns"] = percentiles(all_lags)
    total["got_conn_wait_ns_p99"] = percentiles(all_got_conn)["p99"]
    return {
        "steps": steps_json,
        "total": total,
        "stopped_after_step": stopped,
        "excluded_step": excluded,
        "summary": summary.to_json() if summary is not None else None,
        "retry_drops": inp.hitgen.retry_drops,
        "generator_drops": inp.hitgen.generator_drops,
        "rule": (
            "steps by planned_ns; sent = first attempts written (sent_ns set); unsent = "
            "planned − sent; generator_limited: step p99 of sent_ns − planned_ns over every "
            "planned first attempt, an unwritten one counting above any bound, > 5 ms (§B9)"
        ),
    }


def results_section(inp: Inputs) -> dict[str, object]:
    api, gen = inp.api, inp.gen
    outcomes: dict[str, dict[str, int]] = {KIND_INITIAL: {}, KIND_DUPLICATE: {}, KIND_RETRY: {}}
    for (kind, outcome), count in sorted(gen.outcomes.items()):
        outcomes[kind][outcome] = count
    transport: dict[str, dict[str, int]] = {}
    for (kind, error), count in sorted(gen.transport.items()):
        transport.setdefault(kind, {})[error] = count
    transport_total = sum(gen.transport.values())
    prf2: dict[str, object] = {
        "new_rows": api.new_rows,
        "duplicate_rows_avoided": api.duplicate_rows,
    }
    prf2.update((name, sum(api.err[row] for row in rows)) for name, rows in PRF2_NAMES)
    prf2["transport_errors"] = transport_total
    prf2["expired_refunds"] = api.refunds["receipt_expired"]
    prf2["unresolved_refunds"] = api.refunds["unresolved"]
    prf2["db_rows"] = inp.db_rows
    return {
        "requests": api.requests,
        "by_err": dict(api.err),
        "unmatched": dict(sorted(api.unmatched.items())),
        "attempt_outcomes": outcomes,
        "transport_errors": {"total": transport_total, "by_kind": transport},
        "decisions": dict(api.decisions),
        "releases": api.releases,
        "refunds": dict(api.refunds),
        "unresolved_reasons": dict(sorted(api.unresolved_reasons.items())),
        "db_rows": inp.db_rows,
        "prf2": prf2,
        "unexpected_errors": {
            "rule": (
                "attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or "
                "a 5xx other than service_aborted_after_acquire whose wall-clock span, from its "
                "write to its response, overlaps no fault window [start_ns, end_ns + P]; steps "
                "count their observation window: the step minus its first 5 s, by planned_ns"
            ),
            "fault_windows": [w.to_json() for w in inp.fault_windows],
            "total": gen.unexpected.to_json(),
            "steps": [
                {
                    "step": k,
                    "window_start_ns": window_start(inp.bounds, k),
                    "window_end_ns": inp.bounds[k + 1],
                    **st.window.to_json(),
                }
                for k, st in enumerate(inp.steps)
            ],
        },
        "steps": [
            {
                "step": k,
                "requests": st.api_requests,
                "new_rows": st.api_new_rows,
                "allowed": st.api_allowed,
                "limited": st.api_limited,
            }
            for k, st in enumerate(inp.steps)
        ],
    }


def timing_section(inp: Inputs) -> dict[str, object]:
    api, gen = inp.api, inp.gen
    forced = bool(inp.scenario.forced_aof_rewrites_s)
    rewrite = percentiles(gen.rewrite_latency) if forced else None
    return {
        "limiter_ns": {cls: percentiles(api.limiter[cls]) for cls in LIMITER_CLASSES},
        "db_ns": percentiles(api.db),
        "api_total_ns": percentiles(api.total),
        "refund_ns": {name: percentiles(values) for name, values in api.refund_ns.items()},
        "end_to_end_ns": {name: percentiles(values) for name, values in gen.e2e.items()},
        "steps": [
            {
                "step": k,
                "window_start_ns": window_start(inp.bounds, k),
                "window_end_ns": inp.bounds[k + 1],
                "latency_ns": percentiles(st.window_latency),
            }
            for k, st in enumerate(inp.steps)
        ],
        "rewrite_windows": [w.to_json() for w in inp.rewrite_windows],
        "rewrite_p99_ns": rewrite["p99"] if rewrite is not None else None,
        "rewrite_latency_ns": rewrite,
        "rule": (
            "limiter_ns by acquire result (every result other than allowed, limited and "
            "degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = "
            "final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an "
            "HTTP response, by status (success 200, limited 429, error other), retries apart; "
            "steps: the §B9 latency population over each observation window; rewrite: attempt-1 "
            "responses whose wall-clock span overlaps a rewrite window (TRF-S7)"
        ),
    }


def clocks_section(inp: Inputs) -> dict[str, object]:
    wall = "wall clock, ns since the UTC epoch"
    budget = "budget time: the store's clock in µs × 1,000 (API-4); compared only with budget time"
    hitgen_mono = "hitgen's monotonic clock, ns from its t0"
    api_mono = "hitapi's monotonic clock, ns (durations)"
    limiter_mono = "the limiter's monotonic clock, ns since it was built"
    return {
        "fields": {
            "context.start_ns, end_ns, faults[].start_ns, end_ns, rewrites[]": wall,
            "api request arrival_ns": wall,
            "samples t_ns": wall,
            "api decision_ns, request decision_ns, gen decision_ns": budget,
            "refund credit_decision_ns": budget,
            "gen planned_ns, sent_ns": hitgen_mono,
            "gen latency, got_conn_wait_ns": "hitgen's monotonic clock, ns (durations)",
            "api limiter_ns, db_ns, total_ns": api_mono,
            "release release_mono_ns": "hitapi's monotonic clock, ns since the process started",
            "refund release_mono_ns, final_mono_ns": limiter_mono,
        },
        "t0_wall_ns": inp.hitgen.t0_wall_ns,
        "t0_rule": (
            "hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), "
            "written just before it takes t0; it places planned_ns and sent_ns on the wall "
            "clock for the fault and rewrite windows and arrival_ns on hitgen's steps"
        ),
    }


def resources_section(inp: Inputs) -> dict[str, object]:
    """PRF-4 from samples.jsonl: each process's CPU % and RSS (mean and max over its readings),
    the store's tracked keys, live markers and quarantined keys (rladmin status, ADM-4), and per
    instance the max and last of its /health counts (in_flight, listener and log drops) and of
    its /metrics series (pending refunds, cleanup counts and lag, clock lag, listener drops)."""
    s = inp.samples
    series_rows: list[dict[str, object]] = []
    in_flight_max: list[float] = []
    for instance in sorted(s.instances):
        series = s.instances[instance]
        for name in sorted(series):
            series_rows.append({"instance": instance, "series": name, **series[name].to_json()})
        in_flight = series.get("in_flight")
        if in_flight is not None and in_flight.max is not None:
            in_flight_max.append(in_flight.max)
    return {
        "samples": s.samples,
        "health_reads": s.health_reads,
        "max_in_flight": _num(max(in_flight_max)) if in_flight_max else None,
        "processes": {name: s.procs[name].to_json() for name in sorted(s.procs)},
        "store_state": {name: s.status[name].to_json() for name in sorted(s.status)},
        "instance_series": series_rows,
        "pids": inp.pids,
    }


def traffic_section(inp: Inputs) -> dict[str, object]:
    gen, ctx = inp.gen, inp.ctx
    # TRF-4's busiest user (an object even with no first attempt: its fields stay, user null).
    busiest: dict[str, object] = {"user": None, "first_attempts": 0, "instances": []}
    if gen.users:
        user = min(gen.users, key=lambda u: (-gen.users[u], u))
        mask = gen.user_instances.get(user, 0)
        used = [n for n in range(1, ctx.instances + 1) if mask & (1 << n)]
        busiest = {"user": user, "first_attempts": gen.users[user], "instances": used}
    per_instance = {str(n): gen.per_instance[n] for n in range(1, ctx.instances + 1)}
    return {
        "schedule_path": str(inp.run_dir.joinpath(rf.SCHEDULE)),
        "schedule_sha256": inp.schedule.sha256,
        "schedule_sha256_context": ctx.schedule_sha256,
        "schedule_sha256_matches": inp.schedule.sha256 == ctx.schedule_sha256,
        "schedule_entries": inp.schedule.entries,
        "planned_per_step": [st.scheduled for st in inp.steps],
        "instances": ctx.instances,
        "requests_per_instance": per_instance,
        "served_per_instance": {str(n): c for n, c in sorted(inp.api.per_instance.items())},
        "instances_used": sum(1 for count in gen.per_instance[1:] if count > 0),
        "users": len(gen.users),
        "busiest_user": busiest,
    }


def correctness_section(run_dir: Path) -> dict[str, object]:
    """refmodel.check_bud11's run-directory verdict (plan step 8), called in this process (no
    subprocess, AGENTS.md §4). Evidence it refuses is malformed evidence, as the builder's own
    checks treat it: an InputError keeping refmodel's message (its file, line and field)."""
    try:
        verdict = check_run(run_dir)
    except (RecordError, OSError, UnicodeError) as err:
        raise InputError("refmodel.check_bud11", "evidence", f"refused: {err}") from err
    return dict(verdict)


def _evidence(ok: bool, value: object, detail: str) -> dict[str, object]:
    return {"ok": ok, "value": value, "detail": detail}


def evidence_section(inp: Inputs, correctness: dict[str, object]) -> dict[str, object]:
    """§B8 evidence validity (HAR-13, §A7 item 5, §B6, TRF-S7), with the run's BUD-11a verdict
    (correctness: check_bud11's verdict JSON) not invalid, so the report never says valid when
    its BUD-11a verdict is (HAR-13: incomplete refund evidence)."""
    api, ctx, summary = inp.api, inp.ctx, inp.gen.summary
    verdict = correctness.get("verdict")
    reasons = correctness.get("invalid_reasons")
    bench = ctx.run_kind == "bench"
    outages = [f.outage_ns for f in ctx.faults if f.outage_ns is not None]
    unmeasured = sum(
        1
        for f in ctx.faults
        if f.start_ns > 0 and f.outage_ns is None and is_store_outage_cmd(f.cmd)
    )
    segments = sum(1 for f in ctx.faults if is_outage_segment(f.cmd))
    forced = bool(inp.scenario.forced_aof_rewrites_s)
    listener_ok = api.listener_drops == 0 and api.pre_kill_unread == 0 and api.segments_unread == 0
    fixtures_ok = not bench or (
        inp.samples.health_reads > 0
        and inp.samples.fixtures_on == 0
        and inp.samples.controlled_clock == 0
        and not ctx.faults
    )
    checks = {
        "log_drops": _evidence(
            api.log_drops == 0,
            api.log_drops,
            "log_drops summed over every stop and pre_kill line of every api-N.jsonl",
        ),
        "listener_drops": _evidence(
            listener_ok,
            {
                "sum": api.listener_drops,
                "stop_lines": api.stop_lines,
                "pre_kill_lines": api.pre_kill_lines,
                "pre_kill_unread": api.pre_kill_unread,
                "segments_without_count": api.segments_unread,
            },
            "HAR-13: listener_drops summed over every stop and pre_kill line is 0, every "
            "pre_kill read succeeded (read_ok), and every process ended with one of the two",
        ),
        "fixtures_off": _evidence(
            fixtures_ok,
            {
                "applies": bench,
                "health_reads": inp.samples.health_reads,
                "fixtures_on": inp.samples.fixtures_on,
                "clock_not_store": inp.samples.controlled_clock,
                "faults": len(ctx.faults),
            },
            "benchmark runs: every /health read shows fixtures false on the store clock, at "
            "least one was read, and no fault ran",
        ),
        "outage": _evidence(
            unmeasured == 0 and all(ns <= MAX_OUTAGE_NS for ns in outages),
            {"max_outage_ns": max(outages) if outages else None, "unmeasured": unmeasured},
            "every store outage measured (context.json outage_ns of each faultctl store down or "
            "pause that ran) and at most 5 s",
        ),
        "outage_segments": _evidence(
            segments <= MAX_OUTAGE_SEGMENTS,
            segments,
            "store outage segments armed: faultctl store down or pause, and timed store refusals",
        ),
        "gen_summary": _evidence(
            summary is not None and summary.log_drops == 0,
            summary.log_drops if summary is not None else None,
            "gen.jsonl ends with its summary line, whose log_drops is 0",
        ),
        "rewrite_windows": _evidence(
            not forced or len(inp.rewrite_windows) > 0,
            {"applies": forced, "windows": len(inp.rewrite_windows)},
            "a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window",
        ),
        "correctness": _evidence(
            verdict in ("pass", "violation"),
            {"verdict": verdict, "invalid_reasons": reasons},
            "HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, "
            "not invalid: an invalid verdict means the run's refund evidence is incomplete "
            "(invalid_reasons say why; rerun)",
        ),
    }
    return {"checks": checks, "valid": all(bool(c["ok"]) for c in checks.values())}


def context_section(inp: Inputs) -> dict[str, object]:
    ctx, cfg, sc = inp.ctx, inp.cfg, inp.scenario
    out = dict(ctx.raw)
    out["label"] = LABEL_TEST if ctx.run_kind == "test" else LABEL_BENCH
    out["policy"] = cfg.raw.get("policy")
    out["instance_settings"] = cfg.raw.get("instance")
    out["p_ns"] = cfg.p_ns
    out["limiter_enabled"] = cfg.limiter_enabled
    conformance = ctx.raw.get("conformance")
    floor = conformance.get("fsync") if isinstance(conformance, dict) else None
    out["conformance_floor"] = floor if isinstance(floor, dict) else None
    out["workload"] = {
        "scenario_file": str(sc.path),
        "steps": [{"rate_per_s": s.rate_per_s, "duration_s": s.duration_s} for s in sc.steps],
        "step_bounds_ns": inp.bounds,
        "users": sc.users,
        "instances": sc.instances,
        "verdict": sc.verdict,
        "spike_step": sc.spike_step,
        "forced_aof_rewrites_s": list(sc.forced_aof_rewrites_s),
        "stop_early": sc.stop_early,
    }
    return out


# The baseline comparison (benchmark limiter runs; plan choice for the lookup).


def _baseline_run(ctx: rf.Context, runs_dir: Path) -> tuple[str | None, str | None]:
    """The baseline run of this scenario and seed in runs/index.jsonl: the last baseline line
    before this run's own line (or the last one, when this run has none yet), as (run ID,
    problem). The index is read one line at a time, up to this run's own line, keeping only the
    baseline found so far."""
    index = runs_dir.joinpath(rf.INDEX)
    if not index.exists():
        return None, f"no {index}"
    suffix = f"-{ctx.scenario_name}-{ctx.seed}"
    found: str | None = None
    for line in rf.read_index(index):
        if line.run_id == ctx.run_id:
            break
        if line.baseline and line.scenario == ctx.scenario_name and line.run_id.endswith(suffix):
            found = line.run_id
    if found is None:
        return None, f"no baseline run of {ctx.scenario_name} seed {ctx.seed} in {index}"
    return found, None


STEP_RAN = (None, "full", "partial", "none")  # load.steps[k].ran (null without a gen summary)


@dataclass(frozen=True)
class StepMeasures:
    """One step's measures a baseline comparison reads from a report."""

    p99_ns: int | None  # timing.steps[k].latency_ns.p99: the §B9 window p99
    unexpected_rate: float | None  # results.unexpected_errors.steps[k].rate
    generator_limited: bool  # load.steps[k].generator_limited
    ran: str | None  # load.steps[k].ran: full, partial or none; null without a gen summary
    planned: int  # load.steps[k].planned: entries planned before any stop_early stop

    def ran_in_full(self) -> bool:
        """Whether the step ran in full with planned entries: a step a comparison can use (a
        stop_early run's excluded step ran in part, the steps after it not at all)."""
        return self.ran == "full" and self.planned > 0


def step_measures(report: dict[str, object], where: str) -> list[StepMeasures]:
    """Each step's window p99, unexpected-error rate, generator_limited, ran and planned in a
    report."""
    timing = rf.get_obj(report, "timing", where)
    results = rf.get_obj(report, "results", where)
    load = rf.get_obj(report, "load", where)
    unexpected = rf.get_obj(results, "unexpected_errors", f"{where}: results")
    lat_steps = rf.get_list(timing, "steps", f"{where}: timing")
    err_steps = rf.get_list(unexpected, "steps", f"{where}: results.unexpected_errors")
    load_steps = rf.get_list(load, "steps", f"{where}: load")
    if not len(lat_steps) == len(err_steps) == len(load_steps):
        raise InputError(where, "steps", "load, timing and unexpected-error steps differ in number")
    out = []
    for i, (lat, err, ld) in enumerate(zip(lat_steps, err_steps, load_steps, strict=True)):
        lw = f"{where}: timing.steps[{i}]"
        latency = rf.get_obj(rf.as_obj(lat, lw, "steps"), "latency_ns", lw)
        ew = f"{where}: results.unexpected_errors.steps[{i}]"
        dw = f"{where}: load.steps[{i}]"
        step = rf.as_obj(ld, dw, "steps")
        ran = rf.get_opt_str(step, "ran", dw)
        if ran not in STEP_RAN:
            raise InputError(dw, "ran", f"{ran!r}: want full, partial, none or null")
        out.append(
            StepMeasures(
                p99_ns=rf.get_opt_int(latency, "p99", lw),
                unexpected_rate=rf.get_opt_number(rf.as_obj(err, ew, "steps"), "rate", ew),
                generator_limited=rf.get_bool(step, "generator_limited", dw),
                ran=ran,
                planned=rf.get_int(step, "planned", dw),
            )
        )
    return out


def baseline_section(inp: Inputs, own_steps: Sequence[StepMeasures]) -> dict[str, object] | None:
    """The baseline comparison of a benchmark limiter run (T-RPT-1): the baseline run, its
    schedule_sha256, and each step's window p99 and unexpected-error rate against the
    baseline's (read from its report.json, built by this builder). The baseline is the one that
    froze the thresholds the run copied, thresholds.json's baseline_run_id, so the comparison
    and the verdicts judge against the same run (§B9 step 3, PRF-7: the first baseline sets the
    thresholds); only a run that copied no thresholds takes the baseline run of the same scenario
    and seed found in runs/index.jsonl (plan choice). source says which. baseline_step_exists
    is true only for a baseline step that ran in full with planned entries (baseline_ran says how
    it ran). A problem finding or reading the baseline (thresholds that name none, a bad
    runs/index.jsonl line) is recorded in problem. Null for any other run."""
    ctx = inp.ctx
    if not is_bench_limiter(ctx, inp.cfg):
        return None
    th = inp.thresholds
    out: dict[str, object] = {
        "run_id": None,
        "source": rf.THRESHOLDS if th is not None else f"runs/{rf.INDEX}",
        "schedule_sha256": None,
        "same_schedule": None,
        "steps": [],
        "problem": None,
    }
    runs_dir = inp.run_dir.resolve().parent
    if th is not None:
        run_id = th.baseline_run_id
        problem = None if run_id is not None else f"{rf.THRESHOLDS} names no baseline_run_id"
    else:
        try:
            run_id, problem = _baseline_run(ctx, runs_dir)
        except InputError as err:
            out["problem"] = f"the run index: {err}"
            return out
    if run_id is None:
        out["problem"] = problem
        return out
    out["run_id"] = run_id
    base_dir = runs_dir.joinpath(run_id)
    if not base_dir.is_dir():
        named = "that froze these thresholds" if th is not None else f"that {rf.INDEX} names"
        out["problem"] = f"the baseline run {runs_dir.name}/{run_id} {named} is missing"
        return out
    try:
        base_ctx = rf.read_json(base_dir.joinpath(rf.CONTEXT))
        base_report = rf.read_json(base_dir.joinpath(rf.REPORT_JSON))
        sha = rf.get_opt_str(base_ctx, "schedule_sha256", f"{run_id}/{rf.CONTEXT}")
        base_steps = step_measures(base_report, f"{run_id}/{rf.REPORT_JSON}")
    except InputError as err:
        out["problem"] = f"baseline {run_id}: {err}"
        return out
    out["schedule_sha256"] = sha
    out["same_schedule"] = sha is not None and sha == ctx.schedule_sha256
    steps = []
    for k, own in enumerate(own_steps):
        base = base_steps[k] if k < len(base_steps) else None
        base_p99 = base.p99_ns if base is not None else None
        base_rate = base.unexpected_rate if base is not None else None
        steps.append(
            {
                "step": k,
                "p99_ns": own.p99_ns,
                "baseline_p99_ns": base_p99,
                "p99_delta_ns": (
                    None if own.p99_ns is None or base_p99 is None else own.p99_ns - base_p99
                ),
                "unexpected_rate": own.unexpected_rate,
                "baseline_unexpected_rate": base_rate,
                "unexpected_rate_delta": (
                    None
                    if own.unexpected_rate is None or base_rate is None
                    else own.unexpected_rate - base_rate
                ),
                "baseline_ran": base.ran if base is not None else None,
                "baseline_step_exists": base is not None and base.ran_in_full(),
                "baseline_generator_limited": base.generator_limited if base is not None else None,
            }
        )
    out["steps"] = steps
    return out


def _excluded(m: StepMeasures, base: tuple[bool, bool] | None, attempts: int) -> str | None:
    """Why a step gets no capacity verdict, or None when it gets one (§B9, PRF-8, PRF-11). base is
    the step's (baseline step exists, baseline step generator-limited) from the frozen thresholds
    (Thresholds.baseline_step), None when the run has no thresholds it can use."""
    if m.generator_limited:
        return "generator-limited (PRF-8)"
    if m.ran != "full":
        return f"did not run in full (ran {m.ran})"
    if base is None:
        return "no thresholds to judge it by (capacity.problem)"
    exists, limited = base
    if not exists:
        return "baseline step missing (§B9)"
    if limited:
        return "baseline step generator-limited (§B9)"
    if attempts == 0:
        return "no first attempt in the observation window"
    return None


def capacity_steps(
    inp: Inputs, own: Sequence[StepMeasures], th: Thresholds | None
) -> list[dict[str, object]]:
    """Each step's capacity verdict (plan H5 step 4): pass iff the step is not generator-limited,
    ran in full, its baseline step exists and is not generator-limited, and its window p99 ≤ the
    latency threshold, its window unexpected-error rate ≤ the error threshold, unsent = 0 and no
    ERR-9 or ERR-16 response (every attempt); fail when it gets a verdict and misses any of the
    last four; null (excluded, with the reason) otherwise. The baseline step's conditions come
    from the frozen thresholds th, which the baseline that set them wrote (a step with values
    exists and is not generator-limited, one marked generator_limited is, one not named is
    missing); th is None when the run has none it can use, and then no step gets a verdict.
    Throughput per second over the whole step: offered (planned), achieved (first attempts
    written), and from the API records by arrival, admitted (acquire allowed), 429s (acquire
    limited) and unique rows (a 200 that created its row: ERR-1, or ERR-3 with existed false)
    apart (PRF-10, PRF-11)."""
    out: list[dict[str, object]] = []
    for k, (st, m) in enumerate(zip(inp.steps, own, strict=True)):
        duration_ns = inp.bounds[k + 1] - inp.bounds[k]
        base = th.baseline_step(k) if th is not None else None
        limits = th.steps.get(k) if th is not None else None
        attempts, unexpected = st.window.attempts, st.window.unexpected
        unsent = st.unsent()
        unknown = st.unknown_acquires + st.unknown_outcomes
        latency_ok: bool | None = None
        errors_ok: bool | None = None
        if limits is not None:
            latency_ok = m.p99_ns is not None and m.p99_ns <= limits.latency_ns
            if attempts > 0:
                errors_ok = unexpected / attempts <= limits.error_rate
        excluded = _excluded(m, base, attempts)
        failed: list[str] = []
        verdict: str | None = None
        if excluded is None:
            for name, ok in (
                ("p99", latency_ok),
                ("unexpected_errors", errors_ok),
                ("unsent", unsent == 0),
                ("unknown_outcomes", unknown == 0),
            ):
                if ok is not True:
                    failed.append(name)
            verdict = "fail" if failed else "pass"
        out.append(
            {
                "step": k,
                "ran": m.ran,
                "offered_per_s": per_second(st.planned, duration_ns),
                "achieved_sent_per_s": per_second(st.sent, duration_ns),
                "admitted": st.api_allowed,
                "admitted_per_s": per_second(st.api_allowed, duration_ns),
                "limited_429": st.api_limited,
                "limited_429_per_s": per_second(st.api_limited, duration_ns),
                "unique_rows": st.api_new_rows,
                "unique_rows_per_s": per_second(st.api_new_rows, duration_ns),
                "p99_ns": m.p99_ns,
                "latency_threshold_ns": limits.latency_ns if limits is not None else None,
                "latency_ok": latency_ok,
                "window_attempts": attempts,
                "window_unexpected": unexpected,
                "unexpected_rate": rate(unexpected, attempts),
                "error_rate_threshold": limits.error_rate if limits is not None else None,
                "errors_ok": errors_ok,
                "unsent": unsent,
                "unknown_outcomes": unknown,
                "generator_limited": m.generator_limited,
                "baseline_step_exists": base[0] if base is not None else None,
                "baseline_generator_limited": base[1] if base is not None else None,
                "verdict": verdict,
                "excluded": excluded,
                "failed": failed,
            }
        )
    return out


def spike_section(inp: Inputs, th: Thresholds | None, refused: str | None) -> dict[str, object]:
    """The spike recovery verdict (§B9, PRF-6; plan H5 step 4). met iff the thresholds file
    recorded this deadline and window before the run, gives the post-spike step thresholds, the
    window's p99 and unexpected-error rate are within them (at least one first attempt in the
    window), and no first attempt is outstanding at the deadline. Outstanding, under the
    orchestrator's reading of plan step 4 under PRF-6 (SpikeAcc): a first attempt planned before
    outstanding_cutoff_ns (the deadline − the post-spike step's latency_threshold_ns from the
    frozen thresholds) with no response or failure logged by the deadline, and every first
    attempt planned before the deadline that was never written or (a generator drop) has no
    gen.jsonl line. th is the run's thresholds when it can use them; refused says why it cannot
    use the ones it copied (another schedule's)."""
    params, acc = inp.spike, inp.gen.spike
    if params is None or acc is None:
        return {"met": False, "problems": ["the scenario names no spike_step"]}
    recorded = th.spike if th is not None else None
    limits = th.steps.get(params.post_spike_step) if th is not None else None
    latency = percentiles(acc.latency)
    p99 = latency["p99"]
    attempts, unexpected = acc.window.attempts, acc.window.unexpected
    never_launched = max(inp.schedule.before_deadline - acc.lines_before, 0)
    outstanding = acc.unwritten + acc.late + never_launched
    latency_ok = limits is not None and p99 is not None and p99 <= limits.latency_ns
    errors_ok = limits is not None and attempts > 0 and unexpected / attempts <= limits.error_rate
    problems: list[str] = []
    if refused is not None:
        problems.append(refused)
    else:
        if recorded is None:
            problems.append("no recovery deadline and window recorded in thresholds.json (PRF-6)")
        elif recorded != params:
            problems.append(
                "thresholds.json recorded another deadline or window than the scenario's"
            )
        if limits is None:
            problems.append(f"no thresholds for the post-spike step {params.post_spike_step}")
        else:
            if not latency_ok:
                problems.append("window p99 above the post-spike step's latency threshold")
            if not errors_ok:
                problems.append(
                    "window unexpected-error rate above the post-spike step's threshold"
                )
    if outstanding:
        problems.append(f"first attempts outstanding at the deadline: {outstanding}")
    return {
        **params.to_json(),
        "clock": "hitgen's monotonic clock, ns from its t0 (as planned_ns)",
        "recorded": recorded.to_json() if recorded is not None else None,
        "latency_threshold_ns": limits.latency_ns if limits is not None else None,
        "error_rate_threshold": limits.error_rate if limits is not None else None,
        "window_latency_ns": latency,
        "window_attempts": attempts,
        "window_unexpected": unexpected,
        "window_unexpected_rate": rate(unexpected, attempts),
        "window_planned": inp.schedule.in_spike_window,
        "window_unsent": max(inp.schedule.in_spike_window - acc.written_in_window, 0),
        "outstanding_cutoff_ns": acc.cutoff_ns,
        "planned_before_deadline": inp.schedule.before_deadline,
        "first_attempts_before_deadline": acc.lines_before,
        "finished_by_deadline": acc.finished,
        "not_judged": acc.not_judged,
        "outstanding": outstanding,
        "outstanding_detail": {
            "never_written": acc.unwritten,
            "ended_after_deadline": acc.late,
            "no_line": never_launched,
        },
        "latency_ok": latency_ok,
        "errors_ok": errors_ok,
        "met": not problems,
        "problems": problems,
    }


def capacity_section(inp: Inputs, own: Sequence[StepMeasures]) -> dict[str, object] | None:
    """The capacity and spike verdicts (§B9, PRF-6, PRF-8, PRF-10, PRF-11; plan H5 step 4) of a
    benchmark limiter run whose scenario has verdict capacity or spike; null for any other run.
    The thresholds are the run's copy of the frozen file, used only when they were frozen from
    this run's schedule (their schedule_sha256 is context.json's: a comparison is valid only on
    the same schedule); otherwise problem says why and no step gets a verdict.
    sustainable_capacity is the highest achieved sent rate among the passing steps (null when
    none passes)."""
    sc, ctx = inp.scenario, inp.ctx
    if not is_capacity_run(ctx, inp.cfg, sc):
        return None
    th = inp.thresholds
    usable = same_schedule(th, ctx)
    problem: str | None = None
    if th is None:
        problem = f"no {rf.THRESHOLDS}: the run copied no thresholds"
    elif usable is None:
        problem = (
            f"{rf.THRESHOLDS} was frozen from another schedule (its schedule_sha256 "
            f"{th.schedule_sha256}, this run's {ctx.schedule_sha256}): no verdicts, as a "
            "comparison is valid only on the same schedule"
        )
    steps = capacity_steps(inp, own, usable)
    best: tuple[float, int] | None = None
    for k, row in enumerate(steps):
        achieved = row["achieved_sent_per_s"]
        if row["verdict"] != "pass" or not isinstance(achieved, float):
            continue
        if best is None or achieved > best[0]:
            best = (achieved, k)
    refused = problem if th is not None else None
    return {
        "verdict": sc.verdict,
        "thresholds": None
        if th is None
        else {
            "file": rf.THRESHOLDS,
            "sha256": th.sha256,
            "context_sha256": ctx.thresholds_sha256,
            "created_ns": th.created_ns,
            "baseline_run_id": th.baseline_run_id,
            "schedule_sha256": th.schedule_sha256,
        },
        "problem": problem,
        "steps": steps,
        "sustainable_capacity": best[0] if best is not None else None,
        "sustainable_step": best[1] if best is not None else None,
        "spike": spike_section(inp, usable, refused) if sc.verdict == "spike" else None,
        "rule": (
            "thresholds.json is used only when its schedule_sha256 is this run's; a step passes "
            "iff it is not generator-limited, ran in full, its baseline step exists and is not "
            "generator-limited (from thresholds.json: a step with thresholds exists and is not, "
            "one marked generator_limited is, one not named is missing), its window p99 ≤ "
            "latency_threshold_ns, its window unexpected-error rate ≤ error_rate_threshold, "
            "unsent = 0 and it had no ERR-9 or ERR-16; excluded steps get no verdict; "
            "sustainable_capacity = the highest achieved_sent_per_s of a passing step (PRF-11); "
            "spike: deadline = spike end + 30 s, window = [deadline, deadline + 60 s] by "
            "planned_ns, judged against the post-spike step's thresholds; outstanding at the "
            "deadline (PRF-6, the orchestrator's reading of plan H5 step 4): a first attempt "
            "planned before outstanding_cutoff_ns (deadline − the post-spike step's "
            "latency_threshold_ns) with no response or failure by the deadline, and every first "
            "attempt planned before the deadline never written or never launched"
        ),
    }


# Building.


def read_inputs(run_dir: Path) -> Inputs:
    ctx = rf.read_context(run_dir)
    cfg = rf.read_config(run_dir)
    scenario = rf.read_scenario(ctx, run_dir, sha256_hex)
    bounds = step_bounds([s.duration_s for s in scenario.steps])
    hitgen = rf.read_hitgen_stderr(run_dir)
    samples = SamplesAcc()
    samples.read(run_dir.joinpath(rf.SAMPLES))
    rewrites = [
        Window(r.start_ns, r.end_ns, "context")
        for r in ctx.rewrites
        if r.start_ns > 0 and r.end_ns >= r.start_ns and not r.error
    ]
    rewrites.extend(samples.rewrite_runs)
    faults = fault_windows(ctx, cfg.p_ns)
    steps = [StepAcc() for _ in scenario.steps]
    # Every benchmark limiter run copies its frozen thresholds (§B5): the capacity verdicts judge
    # against them, and the baseline comparison against the baseline that froze them.
    thresholds = run_thresholds(run_dir, len(steps)) if is_bench_limiter(ctx, cfg) else None
    spike = spike_params(scenario, bounds)
    spike_acc = None
    if spike is not None:
        spike_acc = SpikeAcc(spike, spike_cutoff(spike, same_schedule(thresholds, ctx)))
    gen = GenAcc(
        bounds=bounds,
        instances=ctx.instances,
        t0_wall_ns=hitgen.t0_wall_ns,
        faults=faults,
        rewrites=rewrites,
        steps=steps,
        spike=spike_acc,
    )
    gen.read(run_dir.joinpath(rf.GEN))
    schedule = ScheduleAcc()
    schedule.read(
        run_dir.joinpath(rf.SCHEDULE),
        bounds,
        steps,
        gen.summary.planned if gen.summary is not None else None,
        ctx.instances,
        spike,
    )
    api = ApiAcc(bounds=bounds, t0_wall_ns=hitgen.t0_wall_ns, steps=steps)
    present = sorted(p.name for p in run_dir.glob("api-*.jsonl"))
    wanted = [rf.api_file(n) for n in range(1, ctx.instances + 1)]
    if present != sorted(wanted):
        raise InputError(
            str(run_dir), "api-N.jsonl", f"want {', '.join(wanted)}; found {present or 'none'}"
        )
    for n in range(1, ctx.instances + 1):
        api.read(run_dir.joinpath(rf.api_file(n)), n)
    pids = rf.read_pids(run_dir) if run_dir.joinpath(rf.PIDS).exists() else None
    return Inputs(
        run_dir=run_dir,
        ctx=ctx,
        cfg=cfg,
        scenario=scenario,
        bounds=bounds,
        hitgen=hitgen,
        samples=samples,
        gen=gen,
        api=api,
        schedule=schedule,
        steps=steps,
        db_rows=rf.read_db_rows(run_dir),
        pids=pids,
        fault_windows=faults,
        rewrite_windows=rewrites,
        spike=spike,
        thresholds=thresholds,
    )


def build(run_dir: Path) -> dict[str, object]:
    """report.json's content for the run in run_dir (H5 step 2): every section from the run's
    saved evidence only (HAR-9). Raises InputError for missing or malformed evidence."""
    inp = read_inputs(run_dir)
    context = context_section(inp)
    load = load_section(inp)
    results = results_section(inp)
    timing = timing_section(inp)
    own = step_measures({"timing": timing, "results": results, "load": load}, "this run")
    baseline = baseline_section(inp, own)
    capacity = capacity_section(inp, own)
    inp.gen.free()  # every sample array is summarized: free them before the BUD-11a check
    inp.api.free()
    correctness = correctness_section(run_dir)
    evidence = evidence_section(inp, correctness)
    return {
        "run_id": inp.ctx.run_id,
        "context": context,
        "load": load,
        "results": results,
        "timing": timing,
        "clocks": clocks_section(inp),
        "resources": resources_section(inp),
        "traffic": traffic_section(inp),
        "baseline": baseline,
        "correctness": correctness,
        "capacity": capacity,
        "evidence_validity": evidence,
    }


def write_report(run_dir: Path, report: dict[str, object], markdown: str) -> None:
    """report.json (strict JSON: no NaN or Infinity) and report.md in run_dir, each replaced whole
    through a temporary file. Both are encoded, then both temporary files written, before either
    is replaced: a failure before the replacing leaves the old pair and no temporary file."""
    files = [
        (name, text.encode("utf-8"))
        for name, text in (
            (rf.REPORT_JSON, json.dumps(report, indent=2, allow_nan=False) + "\n"),
            (rf.REPORT_MD, markdown),
        )
    ]
    temps = [run_dir.joinpath(name + ".tmp") for name, _ in files]
    try:
        for tmp, (_, data) in zip(temps, files, strict=True):
            tmp.write_bytes(data)
    except OSError:
        for tmp in temps:
            with contextlib.suppress(OSError):  # the write's own error is the one raised
                tmp.unlink(missing_ok=True)
        raise
    for tmp, (name, _) in zip(temps, files, strict=True):
        tmp.replace(run_dir.joinpath(name))
