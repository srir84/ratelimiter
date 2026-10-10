"""The run-evidence tests (HARNESS §8 T-RPT-1, T-RPT-2; DESIGN §B9, §C5; IMPLEMENTATION_PLAN H5
Tests (b) and steps 9 and 10). They read the four smoke runs make test-report makes, whose IDs it
writes to .out/test-report-runs.json: smoke-ramp and smoke-spike (scenarios/*.toml), each a
benchmark baseline and a benchmark limiter run. Missing runs fail the tests, never skip them;
make test-py leaves them out (marker run_evidence). The store-stall detector's own tests need no
run and carry no marker: make test-py runs them.

Expected values come from HARNESS §7 and §8, DESIGN §B4, §B9 and §B10, the scenario files and the
runs' own saved inputs and logs (schedule.jsonl, db_rows.json, gen.jsonl, samples.jsonl,
api-N.jsonl, the frozen thresholds file, the baseline's report), never from the report builder:
what a report must hold, and what it must agree with."""

import hashlib
import json
import math
import random
import re
import stat
from bisect import bisect_left, bisect_right
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path
from statistics import median_low

import pytest

from report import runfiles as rf
from report import validate
from report.build import METRIC_SERIES

REPO = Path(__file__).resolve().parents[1]
RUNS = REPO.joinpath("runs")
RUN_IDS = REPO.joinpath(".out", "test-report-runs.json")

# The smoke scenarios (scenarios/smoke-*.toml; plan H5 step 9): their steps as (rate, seconds),
# their seed and instances.
STEPS = {
    "smoke-ramp": [(100, 20), (200, 20), (400, 20)],
    "smoke-spike": [(100, 30), (500, 10), (100, 90)],
}
SEED = 1
INSTANCES = 3
SECOND = 1_000_000_000
# DESIGN §B10: P, and the in-flight cap per API (HAR-14; PRF-9: overload within resource bounds).
P_NS = 5 * SECOND
MAX_IN_FLIGHT = 512
# DESIGN §B9: a step whose p99 scheduling lag is above 5 ms is generator-limited (PRF-8).
ALLOWED_LAG_NS = 5_000_000
# DESIGN §B9 "Spike recovery" for smoke-spike, spike_step 1: the spike ends at 30 s + 10 s = 40 s,
# the deadline is 30 s later, the window the 60 s after it, judged against step 2's thresholds.
SPIKE = {
    "spike_step": 1,
    "post_spike_step": 2,
    "spike_end_ns": 40 * SECOND,
    "deadline_ns": 70 * SECOND,
    "window_start_ns": 70 * SECOND,
    "window_end_ns": 130 * SECOND,
}
TEST_LABEL = "test-build scenario, not a benchmark"  # DESIGN §B5: test runs only
# The processes PRF-4 measures: generator, each API, limiter store, harness DB (DESIGN §B7).
PROCESSES = ("hitgen", "hitapi-1", "hitapi-2", "hitapi-3", "valkey-server", "postgres")
# An attempt's explicit outcome (PRF-9): 200, a status with its error code, or a transport error.
OUTCOME = re.compile(r"200|[1-5][0-9]{2} [a-z_]+|0 [a-z_]+")
# PRF-4's limiter state per instance (DESIGN §A5.11 metrics; the report follows each, build.py
# METRIC_SERIES), and the cleanup families each instance must have a series of.
LIMITER_SERIES = (
    "rl_tracked_buckets",
    "rl_live_markers",
    "rl_quarantined_keys",
    "rl_pending_refunds",
    "rl_cleanup_lag_seconds",
    "rl_clock_lag_microseconds",
    "rl_listener_drops_total",
)
CLEANUP_FAMILIES = ("rl_cleanup_entries_total{", "rl_cleanup_passes_total{")
# The store stall (orchestrator decisions, H5 test_t_rpt_2 and the slice review's refined rule:
# on this machine the durable store's F_FULLFSYNC stalls stop it for seconds, so DESIGN §A3.0 row
# 9's 33 ms p99, which §B9's +50 ms allowance assumes, does not hold over a 60 s window). A stall
# span is 1 s long, from a request's arrival, with a median limiter_ns of at least 200 ms over the
# requests arriving in it, and counts only when the run's own store-side evidence agrees: a
# samples.jsonl line inside it whose store_error is the sampler's INFO read timing out (the store
# did not answer it within the read's deadline), which a slowdown in the limiter alone never
# causes. Such a stall overlapping [deadline − 2 s, deadline] relaxes the backlog clause
# (outstanding 0, recorded instead); one overlapping [deadline − 2 s, window end] relaxes met's
# latency part (the window p99 within the threshold, recorded instead).
STALL_SPAN_NS = SECOND
STALL_LEAD_NS = 2 * SECOND
STALL_MEDIAN_NS = 200_000_000
# The sampler's INFO read timing out (cmd/sampler readStore, store_error): its ctx deadline
# passed ("context deadline exceeded"), or the socket deadline it set from that same deadline did
# first ("i/o timeout"), while it connected, authenticated or read the INFO replies.
INFO_TIMEOUT = re.compile(r"INFO[ :].*(context deadline exceeded|i/o timeout)")


def obj(value: object, where: str) -> dict[str, object]:
    assert isinstance(value, dict), f"{where}: {value!r} is not an object"
    return value


def lst(value: object, where: str) -> list[object]:
    assert isinstance(value, list), f"{where}: {value!r} is not a list"
    return value


def count(value: object, where: str) -> int:
    assert type(value) is int and value >= 0, f"{where}: {value!r} is not a count"
    return value


def rate(value: object, where: str) -> float:
    assert type(value) in (int, float), f"{where}: {value!r} is not a number"
    assert isinstance(value, int | float) and math.isfinite(value) and value >= 0, where
    return float(value)


def at(doc: object, path: str) -> object:
    """The value at a dotted path of objects (list items by index)."""
    node = doc
    for part in path.split("."):
        node = node[int(part)] if isinstance(node, list) else obj(node, path)[part]
    return node


def read_json(path: Path) -> dict[str, object]:
    return obj(json.loads(path.read_text(encoding="utf-8")), str(path))


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def run_ids() -> dict[str, str]:
    """make test-report's runs: {"smoke-ramp", "smoke-ramp-baseline", "smoke-spike",
    "smoke-spike-baseline"} → run ID, each run directory present."""
    ids = read_json(RUN_IDS)
    want = {name + suffix for name in STEPS for suffix in ("", "-baseline")}
    assert set(ids) == want, f"{RUN_IDS}: {sorted(ids)}, want {sorted(want)}"
    out: dict[str, str] = {}
    for key, run_id in ids.items():
        assert isinstance(run_id, str) and RUNS.joinpath(run_id).is_dir(), (key, run_id)
        out[key] = run_id
    return out


def schedule_counts(run_dir: Path) -> tuple[int, list[int], dict[int, int]]:
    """From the run's schedule.jsonl: its entries, those per step and those per instance."""
    entries, per_instance = 0, dict.fromkeys(range(1, INSTANCES + 1), 0)
    per_step: dict[int, int] = {}
    with run_dir.joinpath("schedule.jsonl").open(encoding="utf-8") as stream:
        for line in stream:
            entry = obj(json.loads(line), "schedule.jsonl")
            entries += 1
            step, instance = count(entry["step"], "step"), count(entry["instance"], "instance")
            per_step[step] = per_step.get(step, 0) + 1
            per_instance[instance] += 1
    return entries, [per_step.get(k, 0) for k in range(max(per_step) + 1)], per_instance


def check_percentiles(value: object, where: str) -> None:
    """A latency summary {count, p50, p95, p99}: nulls without samples, else ordered counts."""
    summary = obj(value, where)
    n = count(summary["count"], f"{where}.count")
    if n == 0:
        assert [summary["p50"], summary["p95"], summary["p99"]] == [None] * 3, where
        return
    p50, p95, p99 = (count(summary[p], f"{where}.{p}") for p in ("p50", "p95", "p99"))
    assert p50 <= p95 <= p99, where


@dataclass(frozen=True)
class Stall:
    """A store stall's span (on hitgen's clock, ns from its t0), its requests' median limiter_ns
    and their number."""

    start_ns: int
    end_ns: int
    median_ns: int
    requests: int


def store_stall(
    decided: Sequence[tuple[int, int]], timeouts: Sequence[int], from_ns: int, to_ns: int
) -> Stall | None:
    """The first store stall overlapping [from_ns, to_ns] in decided, each request's (arrival on
    hitgen's clock, limiter_ns), confirmed by timeouts, the times (on the same clock) of the
    samples whose INFO read timed out: a 1 s span from a request's arrival whose median
    limiter_ns, over the requests arriving in it (both ends included), is at least 200 ms, with a
    timeout in it (both ends included); None when there is none. The spans are 1 s long, the
    shortest the rule allows: a stall (seconds of slow decisions) fills such a span, while a few
    slow requests among fast ones do not, and a longer span cannot reach back to slow requests
    far from the deadline. The sampler reads the store once a second, a read timing out when the
    store does not answer within 0.8 s, so a stall of about 2 s or more holds a timed-out read in
    some span. The median is the lower middle value (the report's nearest-rank p50). Neither
    input needs any order: the instances' records interleave."""
    ordered = sorted(decided)
    arrivals = [arrival for arrival, _ in ordered]
    times = sorted(timeouts)
    for i, start in enumerate(arrivals):
        if start > to_ns:
            break
        end = start + STALL_SPAN_NS
        if end < from_ns or bisect_left(times, start) == bisect_right(times, end):
            continue
        values = [limiter_ns for _, limiter_ns in ordered[i : bisect_right(arrivals, end)]]
        median = median_low(values)
        if median >= STALL_MEDIAN_NS:
            return Stall(start, end, median, len(values))
    return None


@dataclass(frozen=True)
class StallRelief:
    """What a store stall relaxes in the spike verdict (the refined rule): backlog, the first
    store stall overlapping [deadline − 2 s, deadline] (outstanding recorded, not asserted);
    latency, the first overlapping [deadline − 2 s, window end] (the window p99 recorded, not
    asserted). None where there is none."""

    backlog: Stall | None
    latency: Stall | None


def stall_relief(
    decided: Sequence[tuple[int, int]],
    timeouts: Sequence[int],
    deadline_ns: int,
    window_end_ns: int,
) -> StallRelief:
    """The store stalls the spike verdict allows for: the first stall overlapping [deadline − 2 s,
    window end] relaxes met's latency part, and the backlog clause too when it overlaps
    [deadline − 2 s, deadline], which only the first can (the stalls come in order of start)."""
    stall = store_stall(decided, timeouts, deadline_ns - STALL_LEAD_NS, window_end_ns)
    backlog = stall if stall is not None and stall.start_ns <= deadline_ns else None
    return StallRelief(backlog, stall)


def check_spike(
    spike: dict[str, object], post: dict[str, object], relief: StallRelief
) -> dict[str, object]:
    """The spike recovery by its deadline (PRF-6) in a report's capacity.spike, judged against
    the frozen post-spike step post, with the store stalls relief allows for. Always asserted:
    the thresholds as frozen, the window's median latency and its unexpected errors within them,
    latency_ok and met agreeing with their parts. The backlog cleared by the deadline
    (outstanding 0) is asserted unless relief.backlog, the window p99 within the threshold unless
    relief.latency, and met unless either is relaxed. Returns what it records instead of
    asserting, as JUnit properties (nothing when nothing was relaxed)."""
    assert post["step"] == SPIKE["post_spike_step"] and post["generator_limited"] is False
    threshold = count(post["latency_threshold_ns"], "latency_threshold_ns")
    assert spike["latency_threshold_ns"] == threshold
    assert spike["error_rate_threshold"] == post["error_rate_threshold"]
    window = obj(spike["window_latency_ns"], "window_latency_ns")
    p50, p99 = count(window["p50"], "window p50"), count(window["p99"], "window p99")
    latency_ok = p99 <= threshold
    assert spike["latency_ok"] is latency_ok, spike
    assert p50 <= threshold, f"spike window p50 above the post-spike threshold: {spike}"
    unexpected = rate(spike["window_unexpected_rate"], "window_unexpected_rate")
    error_rate = rate(post["error_rate_threshold"], "error_rate_threshold")
    assert unexpected <= error_rate, f"spike window errors above threshold: {spike}"
    assert spike["errors_ok"] is True, f"spike window errors above threshold: {spike}"
    outstanding = count(spike["outstanding"], "outstanding")
    parts_ok = latency_ok and spike["errors_ok"] is True and outstanding == 0
    assert spike["met"] is parts_ok, f"met disagrees with its parts: {spike}"
    recorded: dict[str, object] = {}
    if relief.backlog is None:
        assert outstanding == 0, f"spike backlog not cleared by its deadline: {spike}"
    else:
        recorded["smoke_spike_outstanding"] = outstanding
    if relief.latency is None:
        assert latency_ok, f"spike window p99 above the post-spike threshold: {spike}"
        assert spike["met"] is True, f"spike recovery not met by its deadline: {spike}"
    else:
        recorded["smoke_spike_met"] = spike["met"]
        recorded["smoke_spike_window_p50_ns"] = p50
        recorded["smoke_spike_window_p99_ns"] = p99
        recorded["smoke_spike_latency_threshold_ns"] = threshold
    return recorded


def store_timeouts(run_dir: Path) -> list[int]:
    """The times, on hitgen's clock (ns from its t0, the wall time of its "sending" line), of the
    run's samples.jsonl lines whose store_error is the sampler's INFO read timing out: the store
    did not answer within the read's deadline."""
    t0 = rf.read_hitgen_stderr(run_dir).t0_wall_ns
    times: list[int] = []
    with run_dir.joinpath("samples.jsonl").open(encoding="utf-8") as stream:
        for line in stream:
            rec = obj(json.loads(line), "samples.jsonl")
            error = rec["store_error"]
            if isinstance(error, str) and INFO_TIMEOUT.match(error):
                times.append(count(rec["t_ns"], "samples.jsonl: t_ns") - t0)
    return times


def api_logs(run_dir: Path) -> tuple[dict[int, list[int]], list[tuple[int, int]]]:
    """From the run's api-N.jsonl files: each instance's lifecycle start lines' max_in_flight,
    and (arrival on hitgen's clock, limiter_ns) of every request record with a limiter decision
    (a result; decision records carry neither time): its arrival_ns (wall clock) less hitgen's
    t0, the wall time of its "sending" line, which anchors every planned_ns."""
    t0 = rf.read_hitgen_stderr(run_dir).t0_wall_ns
    starts: dict[int, list[int]] = {}
    decided: list[tuple[int, int]] = []
    for n in range(1, INSTANCES + 1):
        name = rf.api_file(n)
        starts[n] = []
        with run_dir.joinpath(name).open(encoding="utf-8") as stream:
            for line in stream:
                rec = obj(json.loads(line), name)
                if rec["kind"] == "lifecycle" and rec["event"] == "start":
                    starts[n].append(count(rec["max_in_flight"], f"{name}: max_in_flight"))
                elif rec["kind"] == "request" and rec["result"]:
                    arrival = count(rec["arrival_ns"], f"{name}: arrival_ns")
                    decided.append((arrival - t0, count(rec["limiter_ns"], f"{name}: limiter_ns")))
    return starts, decided


def gen_transport_errors(run_dir: Path) -> int:
    """The run's gen.jsonl attempt lines (initial, duplicate or retry) with status 0: attempts
    that got no HTTP response."""
    errors = 0
    with run_dir.joinpath("gen.jsonl").open(encoding="utf-8") as stream:
        for line in stream:
            rec = obj(json.loads(line), "gen.jsonl")
            assert rec["kind"] in ("initial", "duplicate", "retry", "summary"), rec
            if rec["kind"] != "summary" and rec["status"] == 0:
                errors += 1
    return errors


def sampled_max_in_flight(run_dir: Path) -> int | None:
    """The largest /health in_flight of any instance in the run's samples.jsonl (apis[].health,
    null when a read failed); None without one."""
    best: int | None = None
    with run_dir.joinpath("samples.jsonl").open(encoding="utf-8") as stream:
        for line in stream:
            for item in lst(obj(json.loads(line), "samples.jsonl")["apis"], "apis"):
                health = obj(item, "apis[]")["health"]
                if health is not None:
                    value = count(obj(health, "health")["in_flight"], "health.in_flight")
                    best = value if best is None else max(best, value)
    return best


def check_report(name: str, run_id: str, baseline: bool, base_id: str | None) -> None:
    """One smoke report holds every PRF-1…PRF-5 field (T-RPT-1), agreeing with the run's saved
    inputs, and passes report/validate.py: full mode for a limiter run (its baseline comparison
    included), baseline mode for a baseline."""
    run_dir = RUNS.joinpath(run_id)
    report = read_json(run_dir.joinpath("report.json"))
    context = read_json(run_dir.joinpath("context.json"))
    failures = validate.check(run_dir, "baseline" if baseline else "full")
    assert failures == [], [f.line() for f in failures]
    assert at(report, "evidence_validity.valid") is True
    assert report["run_id"] == run_id == context["run_id"]

    # PRF-5: run context.
    ctx = obj(report["context"], "context")
    assert ctx["run_id"] == run_id and isinstance(ctx["epoch_id"], str) and ctx["epoch_id"]
    assert ctx["run_kind"] == "bench" and context["run_kind"] == "bench"
    assert isinstance(ctx["label"], str) and ctx["label"] and ctx["label"] != TEST_LABEL
    assert at(ctx, "scenario.name") == name and ctx["seed"] == SEED
    assert ctx["p_ns"] == P_NS
    for key in ("hardware", "versions", "store_settings", "postgres_settings", "policy"):
        assert obj(ctx[key], key), key
    assert obj(ctx["instance_settings"], "instance_settings")
    assert obj(ctx["conformance_floor"], "conformance_floor")
    start, end = count(ctx["start_ns"], "start_ns"), count(ctx["end_ns"], "end_ns")
    assert start < end
    assert ctx["faults"] in (None, []), "a benchmark runs no fault"
    schedule_sha = sha256_file(run_dir.joinpath("schedule.jsonl"))
    assert ctx["schedule_sha256"] == context["schedule_sha256"] == schedule_sha
    if baseline:
        assert context["baseline"] is True and ctx["thresholds_sha256"] is None
    else:
        copied = sha256_file(run_dir.joinpath("thresholds.json"))
        assert context["baseline"] is False and ctx["thresholds_sha256"] == copied
    assert obj(ctx["workload"], "workload")

    # PRF-1: planned vs sent arrivals, retries and generator drops apart, scheduling lag.
    entries, per_step, per_instance = schedule_counts(run_dir)
    load = obj(report["load"], "load")
    total = obj(load["total"], "load.total")
    planned, sent = count(total["planned"], "planned"), count(total["sent"], "sent")
    unsent = count(total["unsent"], "unsent")
    assert planned == entries and sent + unsent == planned
    assert count(total["duplicates"], "duplicates") == 0  # the smoke scenarios have none
    retries = count(total["retries_429"], "retries_429")
    assert (retries == 0) if baseline else (retries > 0), "429 retries only with the limiter on"
    check_percentiles(total["sched_lag_ns"], "load.total.sched_lag_ns")
    count(total["got_conn_wait_ns_p99"], "got_conn_wait_ns_p99")
    assert at(load, "summary.planned") == planned and at(load, "summary.log_drops") == 0
    count(load["generator_drops"], "generator_drops")
    steps = lst(load["steps"], "load.steps")
    assert len(steps) == len(STEPS[name]) == len(per_step)
    for k, (step_rate, seconds) in enumerate(STEPS[name]):
        step = obj(steps[k], f"load.steps[{k}]")
        assert step["rate_per_s"] == step_rate and step["duration_s"] == seconds
        assert step["planned"] == per_step[k]
        for key in ("sent", "initial", "duplicates", "retries_429", "unsent"):
            count(step[key], f"load.steps[{k}].{key}")
        check_percentiles(step["sched_lag_ns"], f"load.steps[{k}].sched_lag_ns")
        assert isinstance(step["generator_limited"], bool)

    # PRF-2: results by outcome, transport errors apart, the final row count.
    results = obj(report["results"], "results")
    prf2 = obj(results["prf2"], "results.prf2")
    for key in (
        "new_rows",
        "duplicate_rows_avoided",
        "quota_rejections",
        "degraded_admissions",
        "capacity_refusals",
        "integrity_failures",
        "unavailable",
        "unknown_acquires",
        "unknown_application_outcomes",
        "server_failures",
        "transport_errors",
        "expired_refunds",
        "unresolved_refunds",
        "db_rows",
    ):
        count(prf2[key], f"results.prf2.{key}")
    db_rows = read_json(run_dir.joinpath("db_rows.json"))["db_rows"]
    assert results["db_rows"] == prf2["db_rows"] == db_rows
    assert count(prf2["new_rows"], "new_rows") > 0
    rejections = count(prf2["quota_rejections"], "quota_rejections")
    assert (rejections == 0) if baseline else (rejections > 0)
    assert at(results, "transport_errors.total") == prf2["transport_errors"]
    refunds = obj(results["refunds"], "results.refunds")
    assert set(refunds) == {
        "applied",
        "already_applied",
        "receipt_expired",
        "receipt_invalid",
        "unresolved",
    }

    # PRF-3: limiter decision time per class; database, refund and end-to-end times apart.
    timing = obj(report["timing"], "timing")
    for cls in ("allowed", "limited", "degraded", "error"):
        check_percentiles(at(timing, f"limiter_ns.{cls}"), f"timing.limiter_ns.{cls}")
    decided = [count(at(timing, f"limiter_ns.{c}.count"), c) for c in ("allowed", "limited")]
    assert (decided == [0, 0]) if baseline else (min(decided) > 0), "decisions need the limiter"
    check_percentiles(timing["db_ns"], "timing.db_ns")
    check_percentiles(at(timing, "end_to_end_ns.all"), "timing.end_to_end_ns.all")
    check_percentiles(at(timing, "refund_ns.all"), "timing.refund_ns.all")
    assert len(lst(timing["steps"], "timing.steps")) == len(steps)

    # Arrival (wall clock), decision (budget time) and monotonic durations kept separate.
    fields = obj(at(report, "clocks.fields"), "clocks.fields")
    kinds = {key: text for key, text in fields.items() if isinstance(text, str)}
    arrival = [t for k, t in kinds.items() if "arrival_ns" in k]
    decision = [t for k, t in kinds.items() if "decision_ns" in k]
    durations = [t for k, t in kinds.items() if "limiter_ns" in k or "latency" in k]
    assert arrival and decision and durations
    assert all("wall" in t for t in arrival) and all("budget" in t for t in decision)
    assert all("monotonic" in t for t in durations)
    count(at(report, "clocks.t0_wall_ns"), "clocks.t0_wall_ns")

    # PRF-4: resources of every process, the limiter's state, the in-flight bound.
    resources = obj(report["resources"], "resources")
    processes = obj(resources["processes"], "resources.processes")
    for proc in PROCESSES:
        assert count(at(processes, f"{proc}.rss_bytes_max"), proc) > 0, proc
        rate(at(processes, f"{proc}.cpu_pct_mean"), proc)
    assert count(resources["max_in_flight"], "max_in_flight") <= MAX_IN_FLIGHT
    # The store's tracked keys and live markers (rladmin status, ADM-4), in every run.
    store_state = obj(resources["store_state"], "store_state")
    for key in ("tracked_buckets", "live_markers"):
        count(at(store_state, f"{key}.max"), f"store_state.{key}.max")
    # Per instance: its /health in_flight and, in a limiter run, every /metrics series the report
    # follows (tracked buckets, live and quarantined markers, pending refunds, cleanup lag, clock
    # lag, listener drops) and a series of each cleanup count family, each read (a non-null max).
    rows = [obj(item, "instance_series") for item in lst(resources["instance_series"], "series")]
    for n in range(1, INSTANCES + 1):
        maxima = {str(row["series"]): row["max"] for row in rows if row["instance"] == n}
        assert maxima.get("in_flight") is not None, f"instance {n}: {sorted(maxima)}"
        if baseline:
            continue
        for series_name in sorted({*LIMITER_SERIES, *METRIC_SERIES}):
            assert maxima.get(series_name) is not None, (n, series_name, sorted(maxima))
        for family in CLEANUP_FAMILIES:
            assert any(
                series.startswith(family) and value is not None for series, value in maxima.items()
            ), f"instance {n}: no {family}...}} series read: {sorted(maxima)}"

    # The run-log schedule and routing (TRF-1, TRF-4): request index mod 3 instances.
    traffic = obj(report["traffic"], "traffic")
    assert traffic["schedule_sha256"] == schedule_sha
    assert lst(traffic["planned_per_step"], "planned_per_step") == per_step
    assert traffic["instances"] == INSTANCES and traffic["instances_used"] == INSTANCES
    routed = obj(traffic["requests_per_instance"], "requests_per_instance")
    assert {int(n): c for n, c in routed.items()} == per_instance
    assert max(per_instance.values()) - min(per_instance.values()) <= 1
    assert at(traffic, "busiest_user.instances") == [1, 2, 3]

    # The baseline comparison: the baseline that froze the run's thresholds (DESIGN §B9 step 3,
    # PRF-7), of the same scenario and seed on the same schedule, named in report.json and
    # report.md, as capacity.thresholds names it.
    if baseline:
        return
    assert base_id is not None
    base_dir = RUNS.joinpath(base_id)
    base_context = read_json(base_dir.joinpath("context.json"))
    assert base_context["baseline"] is True and at(base_context, "scenario.name") == name
    assert at(report, "baseline.run_id") == base_id
    assert at(report, "baseline.source") == "thresholds.json"
    assert at(report, "capacity.thresholds.baseline_run_id") == base_id
    markdown = run_dir.joinpath("report.md").read_text(encoding="utf-8")
    assert f"| thresholds.baseline_run_id | {base_id} |" in markdown
    base_schedule = sha256_file(base_dir.joinpath("schedule.jsonl"))
    assert at(report, "baseline.schedule_sha256") == base_schedule == schedule_sha
    # Per step: this run's window p99 and unexpected-error rate against the baseline's, as the
    # baseline's own report gives them, and the deltas (this run's − the baseline's).
    base_report = read_json(base_dir.joinpath("report.json"))
    assert base_report["run_id"] == base_id
    comparison = lst(at(report, "baseline.steps"), "baseline.steps")
    assert len(comparison) == len(steps)
    keys = {
        "p99_ns",
        "baseline_p99_ns",
        "p99_delta_ns",
        "unexpected_rate",
        "baseline_unexpected_rate",
        "unexpected_rate_delta",
    }
    for k, item in enumerate(comparison):
        row = obj(item, f"baseline.steps[{k}]")
        assert row["step"] == k and keys <= set(row), row
        own_p99 = count(at(report, f"timing.steps.{k}.latency_ns.p99"), f"step {k} p99")
        base_p99 = count(at(base_report, f"timing.steps.{k}.latency_ns.p99"), f"base step {k} p99")
        assert row["p99_ns"] == own_p99 and row["baseline_p99_ns"] == base_p99, row
        assert row["p99_delta_ns"] == own_p99 - base_p99, row
        rate_path = f"results.unexpected_errors.steps.{k}.rate"
        own_rate = rate(at(report, rate_path), f"step {k} rate")
        base_rate = rate(at(base_report, rate_path), f"base step {k} rate")
        assert row["unexpected_rate"] == own_rate, row
        assert row["baseline_unexpected_rate"] == base_rate, row
        assert row["unexpected_rate_delta"] == own_rate - base_rate, row


def frozen_baseline(run_id: str) -> str:
    """The baseline run that froze the thresholds the run copied: its thresholds.json's
    baseline_run_id (a second make test-report pass's limiter runs still name the first pass's
    baselines, as the frozen files are never rewritten)."""
    doc = read_json(RUNS.joinpath(run_id, "thresholds.json"))
    base_id = doc["baseline_run_id"]
    assert isinstance(base_id, str) and rf.RUN_ID.fullmatch(base_id), base_id
    return base_id


@pytest.mark.run_evidence
def test_t_rpt_1() -> None:
    """T-RPT-1 on every smoke report: each holds all PRF-1…PRF-5 fields (planned vs sent
    arrivals, scheduling lag, arrival vs decision vs monotonic durations kept separate, the
    schedule and its routing over 3 instances) agreeing with the run's saved inputs; each limiter
    report compares against the baseline that froze its thresholds (its thresholds.json's
    baseline_run_id), whose schedule_sha256 is its own; each baseline passes report/validate.py
    in baseline mode, each limiter run in full mode."""
    ids = run_ids()
    for name in STEPS:
        check_report(name, ids[f"{name}-baseline"], True, None)
        check_report(name, ids[name], False, frozen_baseline(ids[name]))


def check_capacity(name: str, run_id: str, record_property: Callable[[str, object], None]) -> None:
    """T-RPT-2 on one smoke limiter report."""
    run_dir = RUNS.joinpath(run_id)
    report = read_json(run_dir.joinpath("report.json"))
    context = read_json(run_dir.joinpath("context.json"))
    failures = validate.check(run_dir, "full")
    assert failures == [], [f.line() for f in failures]

    # Thresholds (and a spike's deadline) recorded before the run and unchanged (PRF-6, PRF-7):
    # the frozen file, mode 0444, has the bytes the run copied when it started.
    frozen = RUNS.joinpath("thresholds", f"{name}-{SEED}.json")
    assert stat.S_IMODE(frozen.stat().st_mode) == 0o444
    copied = run_dir.joinpath("thresholds.json")
    frozen_sha = sha256_file(frozen)
    assert sha256_file(copied) == context["thresholds_sha256"] == frozen_sha
    assert at(report, "context.thresholds_sha256") == frozen_sha
    assert at(report, "capacity.thresholds.sha256") == frozen_sha
    doc = read_json(frozen)
    created = count(doc["created_ns"], "created_ns")
    assert created < count(context["start_ns"], "start_ns")
    assert doc["schedule_sha256"] == context["schedule_sha256"]
    assert doc["scenario"] == name and doc["seed"] == SEED
    base_context = read_json(RUNS.joinpath(str(doc["baseline_run_id"]), "context.json"))
    assert base_context["baseline"] is True and at(base_context, "scenario.name") == name
    capacity = obj(report["capacity"], "capacity")
    assert capacity["problem"] is None
    spike: dict[str, object] | None = None
    if name == "smoke-spike":
        assert doc["spike"] == SPIKE
        spike = obj(capacity["spike"], "capacity.spike")
        for key in ("deadline_ns", "window_start_ns", "window_end_ns"):
            assert spike[key] == SPIKE[key], key
    else:
        assert doc["spike"] is None and capacity["spike"] is None

    # Generator-limited steps flagged (p99 scheduling lag above 5 ms) and excluded (PRF-8).
    load_steps = lst(at(report, "load.steps"), "load.steps")
    cap_steps = lst(capacity["steps"], "capacity.steps")
    assert len(load_steps) == len(cap_steps) == len(STEPS[name])
    passing: list[float] = []
    for k, (ld, cp) in enumerate(zip(load_steps, cap_steps, strict=True)):
        load_step, cap_step = obj(ld, f"load.steps[{k}]"), obj(cp, f"capacity.steps[{k}]")
        lag_p99 = at(load_step, "sched_lag_ns.p99")
        over = lag_p99 is None or count(lag_p99, "lag p99") > ALLOWED_LAG_NS
        limited = load_step["generator_limited"]
        assert isinstance(limited, bool), k
        # A step over 5 ms is flagged; a flagged one is over 5 ms or left first attempts unsent
        # (an unsent attempt's lag is above any bound).
        assert limited if over else (not limited or count(load_step["unsent"], "unsent") > 0), k
        verdict = cap_step["verdict"]
        assert verdict in ("pass", "fail", None), k
        if limited:
            assert verdict is None, f"step {k} is generator-limited but has verdict {verdict}"
        # Admitted and 429 throughput reported apart (PRF-10), with unique-row throughput.
        for key in ("offered_per_s", "admitted_per_s", "limited_429_per_s", "unique_rows_per_s"):
            rate(cap_step[key], f"capacity.steps[{k}].{key}")
        achieved = rate(cap_step["achieved_sent_per_s"], f"capacity.steps[{k}].achieved")
        assert achieved == load_step["achieved_sent_per_s"]
        if verdict == "pass":
            # PRF-11: achieved load meeting thresholds, no generator drops, no unknown outcome.
            assert load_step["unsent"] == 0 and not load_step["generator_limited"]
            assert load_step["unknown_acquires"] == load_step["unknown_application_outcomes"] == 0
            passing.append(achieved)
    admitted = [rate(obj(s, "step")["admitted_per_s"], "admitted") for s in cap_steps]
    limited_429 = [rate(obj(s, "step")["limited_429_per_s"], "429") for s in cap_steps]
    assert min(admitted) > 0 and sum(limited_429) > 0
    # Sustainable capacity: the highest achieved sent rate among the passing steps (PRF-11).
    assert capacity["sustainable_capacity"] == (max(passing) if passing else None)

    # Overload gave explicit failures within resource bounds (PRF-9): every attempt has an
    # outcome, transport errors counted apart; at most 512 requests in flight per API.
    outcomes = obj(at(report, "results.attempt_outcomes"), "attempt_outcomes")
    for kind, want in (("initial", "load.total.initial"), ("retry", "load.total.retries_429")):
        by_outcome = obj(outcomes[kind], kind)
        assert all(OUTCOME.fullmatch(key) for key in by_outcome), sorted(by_outcome)
        assert sum(count(n, key) for key, n in by_outcome.items()) == at(report, want), kind
    # Recounted from the run's files: transport errors are gen.jsonl's attempt lines with no
    # response; the max in flight is the largest /health in_flight samples.jsonl read, within the
    # cap every instance started with (api-N.jsonl start lines, HAR-14).
    transport = count(at(report, "results.transport_errors.total"), "transport_errors")
    assert transport == at(report, "results.prf2.transport_errors") == gen_transport_errors(run_dir)
    sampled = sampled_max_in_flight(run_dir)
    assert sampled is not None and sampled <= MAX_IN_FLIGHT
    assert count(at(report, "resources.max_in_flight"), "max_in_flight") == sampled
    starts, decided = api_logs(run_dir)
    for n, caps in starts.items():
        assert caps and all(cap == MAX_IN_FLIGHT for cap in caps), f"api-{n}.jsonl starts: {caps}"
    for item in lst(at(report, "resources.instance_series"), "instance_series"):
        entry = obj(item, "instance_series")
        if entry["series"] == "in_flight" and entry["max"] is not None:
            assert count(entry["max"], "in_flight max") <= MAX_IN_FLIGHT

    # The spike recovery by its deadline (PRF-6), judged last: every other check above holds
    # whatever its outcome. Orchestrator decisions (H5, owner delegation, and the slice review's
    # refined rule): on a machine whose disk stalls on F_FULLFSYNC, the durable store
    # (appendfsync always) stops for seconds, so DESIGN §A3.0 row 9's 33 ms p99, which §B9's
    # +50 ms allowance assumes, does not hold. A store stall the run's own evidence shows (slow
    # decisions, and the sampler's INFO read timing out inside them) at the deadline leaves first
    # attempts unanswered there, so the backlog is recorded, not asserted; one at the deadline or
    # in the window makes the window p99 miss the post-spike threshold, so the p99 and met are
    # recorded, not asserted (check_spike). Everything else is asserted either way.
    if spike is not None:
        post = obj(lst(doc["steps"], "steps")[SPIKE["post_spike_step"]], "post-spike step")
        relief = stall_relief(
            decided, store_timeouts(run_dir), SPIKE["deadline_ns"], SPIKE["window_end_ns"]
        )
        record_property("smoke_spike_store_stall_at_deadline", relief.backlog)
        record_property("smoke_spike_store_stall", relief.latency)
        for key, value in check_spike(spike, post, relief).items():
            record_property(key, value)


@pytest.mark.run_evidence
def test_t_rpt_2(record_property: Callable[[str, object], None]) -> None:
    """T-RPT-2 on both smoke limiter reports: the thresholds and the spike's recovery deadline
    were recorded before the run (created_ns before its start, the deadline and window as DESIGN
    §B9 gives them for smoke-spike) and left unchanged (the frozen file, mode 0444, has the SHA-256
    the run recorded and copied); spike recovery met by its deadline: its backlog cleared, unless
    the run's own evidence shows a store stall overlapping [deadline − 2 s, deadline] (slow
    decisions and the sampler's INFO read timing out in them), and its window p99 within the
    post-spike threshold, unless it shows one overlapping [deadline − 2 s, window end], each then
    recorded (orchestrator decision), the window's median latency and errors within threshold
    and met agreeing with its parts either way; generator-limited steps are flagged and get no
    verdict; overload returned explicit failures (every attempt's outcome accounted for, transport
    errors apart, as gen.jsonl counts them) within the in-flight bound (512, each instance's
    configured cap, as samples.jsonl read it); admitted and 429 throughput are reported apart;
    sustainable_capacity is the highest achieved rate of a passing step (PRF-11)."""
    ids = run_ids()
    for name in STEPS:
        check_capacity(name, ids[name], record_property)


# The store-stall rule on synthetic decisions and samples (no run needed, no marker):
# smoke-spike's deadline (70 s) and window end (130 s), a request every 10 ms, a sample every
# second at s + 0.5 s.
MS = 1_000_000
DEADLINE_NS, WINDOW_END_NS = SPIKE["deadline_ns"], SPIKE["window_end_ns"]
LEAD_NS = DEADLINE_NS - 2 * SECOND
QUIET_NS = (14 * MS, 21 * MS, 28 * MS, 35 * MS)  # the limiter's decision time without a stall
Slow = Sequence[tuple[float, float, int]]  # slow spans: [from_s, to_s) and their limiter_ns


def decisions(end_s: int, slow: Slow = (), quiet: int | None = None) -> list[tuple[int, int]]:
    """A request every 10 ms from 0 to end_s s, each with QUIET_NS's limiter time in turn (or
    quiet ns, when given), except those arriving in a slow span [from_s, to_s), which take its
    ns."""
    out: list[tuple[int, int]] = []
    for i in range(end_s * 100 + 1):
        arrival = i * 10 * MS
        limiter_ns = QUIET_NS[i % len(QUIET_NS)] if quiet is None else quiet
        for from_s, to_s, ns in slow:
            if from_s * SECOND <= arrival < to_s * SECOND:
                limiter_ns = ns
        out.append((arrival, limiter_ns))
    return out


def samples_in(slow: Slow, end_s: int = 150) -> list[int]:
    """The samples (one a second, at s + 0.5 s, from 0 to end_s s) inside a slow span
    [from_s, to_s): those whose INFO read times out when the store stalls there."""
    times = [k * SECOND + SECOND // 2 for k in range(end_s)]
    return [t for t in times if any(f * SECOND <= t < to * SECOND for f, to, _ in slow)]


EVERY_SAMPLE = samples_in([(0.0, 150.0, 0)])  # every sample timing out: store evidence anywhere


def relief_of(slow: Slow, timeouts: Sequence[int] | None = None) -> StallRelief:
    """stall_relief on decisions(150, slow), confirmed by timeouts (default: the samples inside
    the slow spans time out)."""
    times = samples_in(slow) if timeouts is None else timeouts
    return stall_relief(decisions(150, slow), times, DEADLINE_NS, WINDOW_END_NS)


# (case, its slow spans, whether its stall overlaps [deadline − 2 s, deadline]). Two cases pin
# the backlog relief's bound on both sides: slow decisions from 0.5 s after the deadline give a
# first stall span starting at the deadline (relief), from 1 s after it a first span starting
# 0.5 s after it (none).
STALLS: list[tuple[str, Slow, bool]] = [
    ("a 4 s stall inside the window", [(75.0, 79.0, 480 * MS)], False),
    ("a stall overlapping the deadline − 2 s, before the window", [(66.0, 69.0, 480 * MS)], True),
    ("a stall ending 0.2 s before the deadline − 2 s", [(65.0, 67.8, 480 * MS)], True),
    ("a stall overlapping the deadline", [(69.0, 73.0, 480 * MS)], True),
    ("a stall whose first span starts at the deadline", [(70.5, 74.5, 480 * MS)], True),
    ("a stall starting 1 s after the deadline", [(71.0, 75.0, 480 * MS)], False),
    ("a stall starting at the window's end", [(129.5, 133.0, 480 * MS)], False),
    ("a 0.6 s stall: most of a 1 s span", [(100.0, 100.6, 480 * MS)], False),
]


@pytest.mark.parametrize(("case", "slow", "at_deadline"), STALLS, ids=[c for c, _, _ in STALLS])
def test_store_stall_detected(case: str, slow: Slow, at_deadline: bool) -> None:
    """A 1 s span whose median limiter_ns is at least 200 ms, with a sample whose INFO read timed
    out in it, overlapping [deadline − 2 s, window end] is a store stall: store_stall finds a 1 s
    span over the stall, more than half of its 101 requests slow. It relaxes met's latency part,
    and the backlog clause only when it overlaps [deadline − 2 s, deadline]."""
    relief = relief_of(slow)
    stall = relief.latency
    assert stall is not None, case
    from_ns, to_ns = int(slow[0][0] * SECOND), int(slow[0][1] * SECOND)
    assert stall.start_ns < to_ns and stall.end_ns > from_ns, stall
    assert stall.end_ns == stall.start_ns + SECOND, stall
    assert stall.start_ns <= WINDOW_END_NS and stall.end_ns >= LEAD_NS, stall
    assert stall.median_ns == 480 * MS and stall.requests == 101, stall
    assert relief.backlog == (stall if at_deadline else None), (case, relief)


QUIET: list[tuple[str, Slow]] = [
    ("no slow request", []),
    (
        "a burst of 30 requests of 2 s among fast ones, and single slow ones",
        [(100.0, 100.3, 2000 * MS), *((float(s), s + 0.005, 500 * MS) for s in range(68, 131, 7))],
    ),
    ("a 0.4 s stall: less than half of any 1 s span", [(90.0, 90.4, 480 * MS)]),
    ("a stall ending before the deadline − 3 s", [(55.0, 60.0, 480 * MS)]),
    ("a stall after the window's end", [(131.0, 136.0, 480 * MS)]),
]


@pytest.mark.parametrize(("case", "slow"), QUIET, ids=[case for case, _ in QUIET])
def test_quiet_window_not_a_stall(case: str, slow: Slow) -> None:
    """Fast decisions with a few slow ones, a slow span shorter than half a second, or a stall
    no span overlapping [deadline − 2 s, window end] can reach, are no stall, even with every
    sample's INFO read timing out."""
    assert relief_of(slow, EVERY_SAMPLE) == StallRelief(None, None), case


def test_uniform_slow_window_not_a_stall() -> None:
    """A uniformly slow window is no stall while its median stays under 200 ms: every request
    150 ms, or 1 ns under 200 ms; at 200 ms every span's median reaches the bound (the first span
    overlapping [deadline − 2 s, window end] is then the stall, at the deadline)."""
    for quiet in (150 * MS, 200 * MS - 1):
        relief = stall_relief(decisions(150, quiet=quiet), EVERY_SAMPLE, DEADLINE_NS, WINDOW_END_NS)
        assert relief == StallRelief(None, None), quiet
    relief = stall_relief(decisions(150, quiet=200 * MS), EVERY_SAMPLE, DEADLINE_NS, WINDOW_END_NS)
    stall = relief.latency
    assert stall is not None and stall.median_ns == 200 * MS, stall
    assert stall.start_ns == LEAD_NS - SECOND and relief.backlog == stall, relief


def test_limiter_slowdown_not_a_store_stall() -> None:
    """Slow decisions alone, at the deadline, are no store stall without the store-side evidence
    (a limiter-only slowdown): no sample's INFO read timed out, or only one outside every slow
    span (after it, or just before it, 0.8 s before its slow decisions start). Nothing is relaxed
    then: a backlog or a window p99 above the threshold fails."""
    slow: Slow = [(67.0, 71.0, 480 * MS)]
    for times in ([], [40 * SECOND + SECOND // 2, 72 * SECOND], [66_200 * MS]):
        relief = relief_of(slow, times)
        assert relief == StallRelief(None, None), times
        with pytest.raises(AssertionError, match="backlog not cleared"):
            check_spike(spike_doc(outstanding=29, p99=500 * MS), POST, relief)
        with pytest.raises(AssertionError, match="p99 above the post-spike threshold"):
            check_spike(spike_doc(p99=500 * MS), POST, relief)


def test_stall_after_deadline_keeps_backlog() -> None:
    """A store stall after the deadline cannot leave a first attempt unanswered at it: the
    backlog clause stays asserted, so 30 attempts outstanding at the deadline fail; only met's
    latency part is relaxed (the window p99 and met recorded)."""
    relief = relief_of([(97.5, 101.5, 480 * MS)])
    assert relief.backlog is None and relief.latency is not None, relief
    with pytest.raises(AssertionError, match="backlog not cleared"):
        check_spike(spike_doc(outstanding=30, p99=500 * MS), POST, relief)
    recorded = check_spike(spike_doc(p99=500 * MS), POST, relief)
    assert recorded == {
        "smoke_spike_met": False,
        "smoke_spike_window_p50_ns": 10 * MS,
        "smoke_spike_window_p99_ns": 500 * MS,
        "smoke_spike_latency_threshold_ns": THRESHOLD_NS,
    }


def test_stall_just_after_deadline_keeps_backlog() -> None:
    """The backlog relief's bound at the decisions' finest step: slow decisions from 0.51 s after
    the deadline, with every sample's INFO read timing out, give a first stall span starting
    10 ms after the deadline (70.01 s). It overlaps the window, so met's latency part is relaxed,
    but not [deadline − 2 s, deadline], so the backlog clause stays asserted: a widening of that
    bound by 10 ms or more (under the 0.5 s the STALLS cases pin) fails here."""
    relief = relief_of([(70.51, 74.51, 480 * MS)], EVERY_SAMPLE)
    stall = relief.latency
    assert stall is not None and stall.start_ns == DEADLINE_NS + 10 * MS, relief
    assert relief.backlog is None, relief
    with pytest.raises(AssertionError, match="backlog not cleared"):
        check_spike(spike_doc(outstanding=30, p99=500 * MS), POST, relief)


def test_stall_at_deadline_relaxes_backlog() -> None:
    """A store stall overlapping [deadline − 2 s, deadline] relaxes the backlog clause and met's
    latency part (both recorded); the rest stays asserted: the window's median latency and its
    errors within the thresholds, met agreeing with its parts."""
    relief = relief_of([(67.0, 71.0, 480 * MS)])
    assert relief.backlog is not None and relief.backlog == relief.latency, relief
    recorded = check_spike(spike_doc(outstanding=29, p99=500 * MS), POST, relief)
    assert recorded["smoke_spike_outstanding"] == 29 and recorded["smoke_spike_met"] is False
    with pytest.raises(AssertionError, match="p50 above"):
        check_spike(spike_doc(outstanding=29, p50=THRESHOLD_NS + 1, p99=500 * MS), POST, relief)
    with pytest.raises(AssertionError, match="errors above threshold"):
        check_spike(spike_doc(unexpected=0.01), POST, relief)
    with pytest.raises(AssertionError, match="met disagrees"):
        check_spike({**spike_doc(outstanding=29), "met": True}, POST, relief)


def test_no_stall_asserts_recovery() -> None:
    """With no store stall, the spike recovery is asserted as the plan says: met, the backlog
    cleared and the window p99 within the post-spike threshold; one first attempt outstanding,
    or the p99 1 ns over the threshold, fails."""
    relief = StallRelief(None, None)
    assert check_spike(spike_doc(), POST, relief) == {}
    assert check_spike(spike_doc(p99=THRESHOLD_NS), POST, relief) == {}
    with pytest.raises(AssertionError, match="backlog not cleared"):
        check_spike(spike_doc(outstanding=1), POST, relief)
    with pytest.raises(AssertionError, match="p99 above the post-spike threshold"):
        check_spike(spike_doc(p99=THRESHOLD_NS + 1), POST, relief)


def interleaved(decided: Sequence[tuple[int, int]]) -> list[tuple[int, int]]:
    """decided as the run's logs give it: the three instances' records (routed in turn), each
    instance's in arrival order, read one log after another (api-1.jsonl, api-2.jsonl,
    api-3.jsonl), so arrivals go back twice."""
    return [d for n in range(INSTANCES) for d in decided[n::INSTANCES]]


ORDER_CASES: list[tuple[str, Slow, Sequence[int] | None]] = [
    *((case, slow, None) for case, slow, _ in STALLS),
    *((case, slow, EVERY_SAMPLE) for case, slow in QUIET),
    ("a limiter-only slowdown at the deadline", [(67.0, 71.0, 480 * MS)], []),
    ("a store stall after the deadline", [(97.5, 101.5, 480 * MS)], None),
]


@pytest.mark.parametrize(
    ("case", "slow", "timeouts"), ORDER_CASES, ids=[c for c, _, _ in ORDER_CASES]
)
def test_stall_rule_order_independent(
    case: str, slow: Slow, timeouts: Sequence[int] | None
) -> None:
    """The stall rule reads decisions and samples in any order: arrival order, the instances'
    logs one after another, and shuffled (the samples reversed or shuffled too) give the same
    relief."""
    decided = decisions(150, slow)
    times = list(samples_in(slow) if timeouts is None else timeouts)
    want = stall_relief(decided, times, DEADLINE_NS, WINDOW_END_NS)
    shuffled, shuffled_times = list(decided), list(times)
    random.Random(7).shuffle(shuffled)
    random.Random(8).shuffle(shuffled_times)
    for got_decided, got_times in ((interleaved(decided), times[::-1]), (shuffled, shuffled_times)):
        got = stall_relief(got_decided, got_times, DEADLINE_NS, WINDOW_END_NS)
        assert got == want, case


def test_info_timeout_errors() -> None:
    """The store-side evidence is the sampler's INFO read timing out (cmd/sampler readStore):
    its deadline passing while it reads, authenticates or sends; a refused connection, a closed
    one or no error is not."""
    for text in (
        "INFO persistence: reading the reply: context deadline exceeded",
        "INFO persistence: reading the reply: reading a reply: read tcp 127.0.0.1:1->"
        "127.0.0.1:6390: i/o timeout",
        "INFO: authenticating to the store at 127.0.0.1:6390 as rl_ops: context deadline exceeded",
        "INFO: sending: write tcp 127.0.0.1:1->127.0.0.1:6390: i/o timeout",
    ):
        assert INFO_TIMEOUT.match(text), text
    for text in (
        "INFO: connecting to the store at 127.0.0.1:6390: dial tcp 127.0.0.1:6390: connect: "
        "connection refused",
        "INFO: EOF",
        "INFO clients: the reply - is not a bulk string",
        "GET /health: context deadline exceeded",
    ):
        assert not INFO_TIMEOUT.match(text), text


# The post-spike step's frozen thresholds and capacity.spike documents for check_spike.
THRESHOLD_NS = 60 * MS
ERROR_RATE = 0.001
POST: dict[str, object] = {
    "step": SPIKE["post_spike_step"],
    "generator_limited": False,
    "latency_threshold_ns": THRESHOLD_NS,
    "error_rate_threshold": ERROR_RATE,
}


def spike_doc(
    *, outstanding: int = 0, p50: int = 10 * MS, p99: int = 20 * MS, unexpected: float = 0.0
) -> dict[str, object]:
    """A capacity.spike judged against POST, its latency_ok, errors_ok and met from its parts."""
    latency_ok, errors_ok = p99 <= THRESHOLD_NS, unexpected <= ERROR_RATE
    return {
        "latency_threshold_ns": THRESHOLD_NS,
        "error_rate_threshold": ERROR_RATE,
        "window_latency_ns": {"count": 6000, "p50": p50, "p95": p99, "p99": p99},
        "latency_ok": latency_ok,
        "window_unexpected_rate": unexpected,
        "errors_ok": errors_ok,
        "outstanding": outstanding,
        "met": latency_ok and errors_ok and outstanding == 0,
    }
