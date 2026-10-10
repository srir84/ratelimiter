"""report/validate.py (IMPLEMENTATION_PLAN H5 step 6; DESIGN §C5, §B8, §B9; HARNESS T-RPT-1,
T-RPT-2). The required keys and fields are listed here by hand, from the plan's report.json keys
and HARNESS PRF-1…PRF-5; each value check is broken by changing one value of a valid run."""

import ast
import hashlib
import io
import json
import re
import shutil
import sys
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path

import pytest

from report import validate
from report.__main__ import main as build_main
from report.synthetic import (
    SECOND,
    T0_WALL_NS,
    Row,
    RunSpec,
    attempt,
    copy_thresholds,
    entry,
    lifecycle,
    lifecycle_start,
    ok_row,
    rows_run,
    sample,
    schedule_sha256_of,
    summary,
    update_context,
    write_index,
    write_run,
)
from report.thresholds import freeze

MS = 1_000_000
CREATED_NS = T0_WALL_NS - 3600 * SECOND  # before the synthetic runs' start_ns (t0 − 10 s)

# The steps of every scenario here (for a spike, spike_step 1, its end at 40 s: deadline 70 s,
# window [70 s, 130 s]).
STEPS = [(1.0, 30.0), (5.0, 10.0), (1.0, 100.0)]
ENTRIES = [
    6,
    10,
    20,
    36,
    38,
    50,
    69,
    75,
    100,
    130,
    135,
]  # s; the limiter run answers each 200 in 2 ms
GL_ENTRY = 36  # the limiter run writes it 6 ms late: its step 1 is generator-limited
# The baseline answers step 0's entries in 10 ms, step 1's in 100 ms and step 2's in 2 ms, so the
# frozen latency thresholds differ per step: 60 ms, 150 ms and 52 ms (p99 + 50 ms).
BASE_LATENCY_NS = (10 * MS, 100 * MS, 2 * MS)
# The spike's outstanding cutoff: the deadline − the post-spike step's latency threshold.
CUTOFF_NS = 70 * SECOND - 52 * MS
BOTH = ("capacity", "spike")


@dataclass(frozen=True)
class Runs:
    root: Path
    base: Path  # the baseline run
    run: Path  # the limiter run
    frozen: Path  # runs/thresholds/<name>-1.json
    verdict: str


def rows(late: int | None, baseline: bool) -> list[Row]:
    out: list[Row] = []
    for s in ENTRIES:
        step = 0 if s < 30 else 1 if s < 40 else 2
        latency = BASE_LATENCY_NS[step] if baseline else 2 * MS
        out.append(ok_row(s * SECOND, 6 * MS if s == late else 100_000, latency))
    return out


def valid_runs(root: Path, verdict: str = "spike", name: str = "vld") -> Runs:
    """A valid benchmark limiter run of a scenario with verdict (spike, capacity or none) and its
    baseline: the same schedule, thresholds frozen from the baseline before the run, copied into
    the run, both reports built. Steps 0 and 2 pass (achieved 0.1/s and 0.06/s), step 1 is
    generator-limited, and a spike run's spike is met."""
    spike_step = 1 if verdict == "spike" else None
    base = rows_run(
        root,
        f"20261007-110000-{name}-1",
        STEPS,
        rows(None, True),
        name=name,
        baseline=True,
        verdict=verdict,
        spike_step=spike_step,
    )
    assert build_main([str(base)]) == 0
    frozen = root.joinpath("runs", "thresholds", f"{name}-1.json")
    freeze(base, frozen, created_ns=CREATED_NS)
    run = rows_run(
        root,
        f"20261007-120000-{name}-1",
        STEPS,
        rows(GL_ENTRY, False),
        name=name,
        verdict=verdict,
        spike_step=spike_step,
    )
    copy_thresholds(run, frozen)
    write_index(root, [{"scenario": name, "baseline": True, "run_id": base.name}])
    assert build_main([str(run)]) == 0
    return Runs(root, base, run, frozen, verdict)


def run_validate(run_dir: Path, *args: str) -> tuple[int, list[str]]:
    out = io.StringIO()
    code = validate.main([str(run_dir), *args], out)
    return code, out.getvalue().splitlines()


def failed(lines: list[str]) -> set[str]:
    """The checks a validate run names as failed."""
    return {
        line.split(":", 1)[0].removeprefix("FAIL ") for line in lines if line.startswith("FAIL ")
    }


def read_json(path: Path) -> dict[str, object]:
    doc = json.loads(path.read_text(encoding="utf-8"))
    assert isinstance(doc, dict)
    return doc


def _node(doc: object, parts: list[str]) -> object:
    """The value at parts (a number is a list index)."""
    node = doc
    for part in parts:
        if isinstance(node, list):
            node = node[int(part)]
        else:
            assert isinstance(node, dict)
            node = node[part]
    return node


def edit_report(run_dir: Path, path: str, value: object) -> None:
    """Sets the report.json value at a dotted path (a number is a list index)."""
    report = read_json(run_dir.joinpath("report.json"))
    *parents, last = path.split(".")
    node = _node(report, parents)
    if isinstance(node, list):
        node[int(last)] = value
    else:
        assert isinstance(node, dict)
        node[last] = value
    run_dir.joinpath("report.json").write_text(json.dumps(report, indent=2) + "\n")


def drop_report_key(run_dir: Path, path: str) -> None:
    """Removes the report.json key at a dotted path (a number is a list index)."""
    report = read_json(run_dir.joinpath("report.json"))
    *parents, last = path.split(".")
    node = _node(report, parents)
    assert isinstance(node, dict)
    del node[last]
    run_dir.joinpath("report.json").write_text(json.dumps(report, indent=2) + "\n")


def set_context(run_dir: Path, path: str, value: object) -> None:
    """Sets the context.json value at a dotted path of objects."""
    ctx = read_json(run_dir.joinpath("context.json"))
    *parents, last = path.split(".")
    node = _node(ctx, parents)
    assert isinstance(node, dict)
    node[last] = value
    run_dir.joinpath("context.json").write_text(json.dumps(ctx, indent=2) + "\n")


def edit_scenario(run_dir: Path, edit: Callable[[str], str], pin: bool = True) -> None:
    """Rewrites the scenario file run_dir's context.json names with edit and, when pin is set,
    records its new SHA-256 there (as a run of the edited file would)."""
    ctx = read_json(run_dir.joinpath("context.json"))
    scenario = ctx["scenario"]
    assert isinstance(scenario, dict)
    path = Path(str(scenario["path"]))
    text = edit(path.read_text(encoding="utf-8"))
    path.write_text(text, encoding="utf-8")
    if pin:
        set_context(run_dir, "scenario.sha256", hashlib.sha256(text.encode()).hexdigest())


def forced_rewrites(text: str) -> str:
    """The scenario with forced_aof_rewrites_s = [10.0] (a top-level key, before every table)."""
    return "forced_aof_rewrites_s = [10.0]\n" + text


def drop_context_key(run_dir: Path, path: str) -> None:
    """Removes the context.json key at a dotted path of objects."""
    ctx = read_json(run_dir.joinpath("context.json"))
    *parents, last = path.split(".")
    node = _node(ctx, parents)
    assert isinstance(node, dict)
    del node[last]
    run_dir.joinpath("context.json").write_text(json.dumps(ctx, indent=2) + "\n")


def bogus_verdict(text: str) -> str:
    """The scenario with verdict = "bogus" in place of its verdict line."""
    edited, n = re.subn(r'^verdict = "[a-z]+"$', 'verdict = "bogus"', text, flags=re.MULTILINE)
    assert n == 1
    return edited


def forced_not_a_list(value: str) -> Callable[[str], str]:
    """An edit that sets forced_aof_rewrites_s to value, a TOML value that is no list."""
    return lambda text: f"forced_aof_rewrites_s = {value}\n" + text


@pytest.mark.parametrize("verdict", ["capacity", "spike", "none"])
def test_valid_run_passes(tmp_path: Path, verdict: str) -> None:
    """The synthetic runs are valid: the limiter run in full mode (a capacity or spike run's
    T-RPT-2 checks included, none for a verdict none run) and the baseline in baseline mode."""
    runs = valid_runs(tmp_path, verdict)
    code, lines = run_validate(runs.run)
    assert (code, lines) == (0, [f"validate: {runs.run}: valid (full mode)"])
    code, lines = run_validate(runs.base, "--mode", "baseline")
    assert (code, lines) == (0, [f"validate: {runs.base}: valid (baseline mode)"])
    frozen = read_json(runs.frozen)["steps"]
    assert isinstance(frozen, list)
    assert [s["latency_threshold_ns"] for s in frozen] == [60 * MS, 150 * MS, 52 * MS]
    report = read_json(runs.run.joinpath("report.json"))
    capacity = report["capacity"]
    if verdict == "none":
        assert capacity is None
        return
    assert isinstance(capacity, dict)
    assert [s["verdict"] for s in capacity["steps"]] == ["pass", None, "pass"]
    assert capacity["sustainable_capacity"] == 0.1
    if verdict == "capacity":
        assert capacity["spike"] is None
        return
    assert capacity["spike"]["met"] is True
    assert capacity["spike"]["outstanding_cutoff_ns"] == CUTOFF_NS


TOP_KEYS = (
    "context",
    "load",
    "results",
    "timing",
    "clocks",
    "resources",
    "traffic",
    "baseline",
    "correctness",
    "capacity",
    "evidence_validity",
)


def pcts(path: str) -> tuple[str, ...]:
    """The nearest-rank p50, p95 and p99 of the summary at path."""
    return tuple(f"{path}.{p}" for p in ("p50", "p95", "p99"))


# HARNESS §3's ERR rows, each counted in results.by_err.
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
# HARNESS PRF-1…PRF-5 and the plan's step 2 sections, as fields of report.json, nested ones
# included (each percentile, ERR row, refund result, end-to-end class and per-step field).
FIELDS = (
    # PRF-5: hardware, versions, run and epoch IDs, policy, limits, P, seed, workload, start/end,
    # faults; run kind and label, store and PostgreSQL settings, schedule and thresholds hashes,
    # conformance floor.
    "context.run_id",
    "context.epoch_id",
    "context.run_kind",
    "context.label",
    "context.scenario",
    "context.seed",
    "context.hardware",
    "context.versions",
    "context.store_settings",
    "context.postgres_settings",
    "context.policy",
    "context.instance_settings",
    "context.p_ns",
    "context.start_ns",
    "context.end_ns",
    "context.faults",
    "context.schedule_sha256",
    "context.thresholds_sha256",
    "context.workload",
    "context.conformance_floor",
    # PRF-1: planned vs sent arrivals, retries, generator drops, scheduling lag; per step too.
    "load.steps",
    "load.total.planned",
    "load.total.sent",
    "load.total.initial",
    "load.total.duplicates",
    "load.total.retries_429",
    "load.total.unsent",
    "load.total.sched_lag_ns",
    *pcts("load.total.sched_lag_ns"),
    "load.total.got_conn_wait_ns_p99",
    "load.stopped_after_step",
    "load.generator_drops",
    "load.steps.0.planned",
    "load.steps.0.sent",
    "load.steps.0.initial",
    "load.steps.0.duplicates",
    "load.steps.0.retries_429",
    "load.steps.0.unsent",
    "load.steps.0.sched_lag_ns",
    *pcts("load.steps.0.sched_lag_ns"),
    "load.steps.0.got_conn_wait_ns_p99",
    "load.steps.0.generator_limited",
    # PRF-2: the results by kind and by attempt outcome, transport errors, refunds, the final DB
    # row count.
    "results.requests",
    "results.by_err",
    *(f"results.by_err.{row}" for row in ERR_ROWS),
    "results.attempt_outcomes",
    "results.attempt_outcomes.initial",
    "results.attempt_outcomes.duplicate",
    "results.attempt_outcomes.retry",
    "results.decisions",
    "results.refunds",
    "results.refunds.applied",
    "results.refunds.already_applied",
    "results.refunds.receipt_expired",
    "results.refunds.receipt_invalid",
    "results.refunds.unresolved",
    "results.transport_errors",
    "results.transport_errors.total",
    "results.unexpected_errors",
    "results.unexpected_errors.total",
    "results.unexpected_errors.steps",
    "results.unexpected_errors.steps.0.attempts",
    "results.unexpected_errors.steps.0.unexpected",
    "results.unexpected_errors.steps.0.rate",
    "results.db_rows",
    "results.prf2.new_rows",
    "results.prf2.duplicate_rows_avoided",
    "results.prf2.quota_rejections",
    "results.prf2.degraded_admissions",
    "results.prf2.capacity_refusals",
    "results.prf2.integrity_failures",
    "results.prf2.unavailable",
    "results.prf2.unknown_acquires",
    "results.prf2.unknown_application_outcomes",
    "results.prf2.server_failures",
    "results.prf2.transport_errors",
    "results.prf2.expired_refunds",
    "results.prf2.unresolved_refunds",
    "results.prf2.db_rows",
    # PRF-3: limiter decision time per class, DB, refund and end-to-end time.
    "timing.limiter_ns.allowed",
    "timing.limiter_ns.limited",
    "timing.limiter_ns.degraded",
    "timing.limiter_ns.error",
    *pcts("timing.limiter_ns.allowed"),
    *pcts("timing.limiter_ns.limited"),
    *pcts("timing.limiter_ns.degraded"),
    *pcts("timing.limiter_ns.error"),
    "timing.db_ns",
    *pcts("timing.db_ns"),
    "timing.refund_ns",
    *pcts("timing.refund_ns.all"),
    "timing.end_to_end_ns",
    *pcts("timing.end_to_end_ns.all"),
    *pcts("timing.end_to_end_ns.success"),
    *pcts("timing.end_to_end_ns.limited"),
    *pcts("timing.end_to_end_ns.error"),
    *pcts("timing.end_to_end_ns.retries"),
    "timing.steps",
    *pcts("timing.steps.0.latency_ns"),
    "timing.rewrite_p99_ns",
    # Arrival, decision and monotonic times kept apart.
    "clocks.fields",
    "clocks.t0_wall_ns",
    # PRF-4: per process CPU and memory, store state, instance series.
    "resources.processes",
    "resources.processes.valkey-server.cpu_pct_mean",
    "resources.processes.valkey-server.cpu_pct_max",
    "resources.processes.valkey-server.rss_bytes_mean",
    "resources.processes.valkey-server.rss_bytes_max",
    "resources.store_state",
    "resources.store_state.tracked_buckets.max",
    "resources.store_state.tracked_buckets.last",
    "resources.store_state.live_markers.max",
    "resources.store_state.live_markers.last",
    "resources.instance_series",
    "resources.instance_series.0.instance",
    "resources.instance_series.0.series",
    "resources.instance_series.0.max",
    "resources.instance_series.0.last",
    "resources.max_in_flight",
    # The run-log schedule and routing.
    "traffic.schedule_path",
    "traffic.schedule_sha256",
    "traffic.planned_per_step",
    "traffic.requests_per_instance",
    "traffic.instances_used",
    "traffic.busiest_user",
    "traffic.busiest_user.user",
    "traffic.busiest_user.instances",
    # The baseline comparison.
    "baseline.run_id",
    "baseline.schedule_sha256",
    "baseline.steps",
    "baseline.steps.0.p99_ns",
    "baseline.steps.0.baseline_p99_ns",
    "baseline.steps.0.p99_delta_ns",
    "baseline.steps.0.unexpected_rate",
    "baseline.steps.0.baseline_unexpected_rate",
    "baseline.steps.0.unexpected_rate_delta",
    # The BUD-11a verdict JSON, every field check_bud11 writes in each of its shapes.
    "correctness.verdict",
    "correctness.known_credits",
    "correctness.unknown_credits",
    "correctness.listener_drops",
    "correctness.pre_kill_lines",
    "correctness.pre_kill_read_ok_false",
    "correctness.invalid_reasons",
    "correctness.violation",
)
# A capacity or spike run's capacity: per step offered, achieved, admitted, 429 and unique-row
# throughput, p99 and unexpected-error rate against their thresholds, the verdict (PRF-10,
# PRF-11); sustainable_capacity.
CAPACITY_FIELDS = (
    "capacity.steps",
    "capacity.sustainable_capacity",
    "capacity.steps.0.offered_per_s",
    "capacity.steps.0.achieved_sent_per_s",
    "capacity.steps.0.admitted_per_s",
    "capacity.steps.0.limited_429_per_s",
    "capacity.steps.0.unique_rows_per_s",
    "capacity.steps.0.p99_ns",
    "capacity.steps.0.latency_threshold_ns",
    "capacity.steps.0.unexpected_rate",
    "capacity.steps.0.error_rate_threshold",
    "capacity.steps.0.verdict",
)
# A spike run's deadline, window and met.
SPIKE_FIELDS = (
    "capacity.spike.deadline_ns",
    "capacity.spike.window_start_ns",
    "capacity.spike.window_end_ns",
    "capacity.spike.met",
)
# check_bud11's violation shape (verdict violation): the window that broke BUD-11a (its violation
# is null in the pass and invalid shapes).
VIOLATION = {"user_id": "u-1", "start_ns": 0, "end_ns": SECOND, "excess_units": 1}
VIOLATION_FIELDS = tuple(f"correctness.violation.{name}" for name in VIOLATION)
# The sections whose fields baseline mode requires (the T-RPT-1 sections but baseline), and those
# full mode requires of a run_kind = test run (those and correctness; plan H5 step 6, §C5).
BASELINE_MODE_SECTIONS = ("context", "load", "results", "timing", "clocks", "resources", "traffic")
TEST_RUN_SECTIONS = (*BASELINE_MODE_SECTIONS, "correctness")


def fields_in(sections: tuple[str, ...]) -> list[str]:
    """The FIELDS inside these sections, nested ones included."""
    return [key for key in FIELDS if key.split(".", 1)[0] in sections]


def violation_shaped(run_dir: Path) -> None:
    """Gives run_dir's report check_bud11's violation shape, its evidence still valid: verdict
    violation with the window that broke BUD-11a, and that verdict recorded in
    evidence_validity."""
    edit_report(run_dir, "correctness.verdict", "violation")
    edit_report(run_dir, "correctness.violation", VIOLATION)
    edit_report(run_dir, "evidence_validity.checks.correctness.value.verdict", "violation")


def each_required(run_dir: Path, keys: Iterable[str], *args: str) -> None:
    """run_dir's report passes validate.py (run with args), and removing any one of keys makes
    it exit 1 naming required_keys."""
    good = run_dir.joinpath("report.json").read_bytes()
    assert run_validate(run_dir, *args)[0] == 0
    for key in keys:
        drop_report_key(run_dir, key)
        code, lines = run_validate(run_dir, *args)
        assert code == 1, key
        assert "required_keys" in failed(lines), (key, lines)
        run_dir.joinpath("report.json").write_bytes(good)


@pytest.mark.parametrize("verdict", BOTH)
def test_missing_field_fails(tmp_path: Path, verdict: str) -> None:
    """Removing each required key or field makes validate.py exit 1: in full mode on a benchmark
    limiter run (capacity and spike) every key and field, with capacity's fields, a spike run's
    spike fields and, in check_bud11's violation shape, the violation's; in baseline mode every
    key but baseline (and correctness and capacity, which are full mode's), and every field of the
    sections it requires, nested ones included. A run_kind = test report without baseline or
    capacity passes full mode, and removing any other required key from it still exits 1, as
    does removing any field of its sections, nested ones and the violation's included. Each
    removed field fails required_keys."""
    runs = valid_runs(tmp_path, verdict)
    spike = SPIKE_FIELDS if verdict == "spike" else ()
    each_required(runs.run, (*TOP_KEYS, *FIELDS, *CAPACITY_FIELDS, *spike))
    violation_shaped(runs.run)
    each_required(runs.run, VIOLATION_FIELDS)

    base_good = runs.base.joinpath("report.json").read_bytes()
    for key in TOP_KEYS:
        drop_report_key(runs.base, key)
        code, _ = run_validate(runs.base, "--mode", "baseline")
        assert code == (0 if key in ("baseline", "correctness", "capacity") else 1), key
        runs.base.joinpath("report.json").write_bytes(base_good)
    each_required(runs.base, fields_in(BASELINE_MODE_SECTIONS), "--mode", "baseline")

    test_run = rows_run(
        tmp_path,
        "20261007-130000-tst-1",
        [(1.0, 20.0)],
        [ok_row(6 * SECOND)],
        name="tst",
        run_kind="test",
    )
    assert build_main([str(test_run)]) == 0
    drop_report_key(test_run, "baseline")
    drop_report_key(test_run, "capacity")
    assert run_validate(test_run)[0] == 0
    test_good = test_run.joinpath("report.json").read_bytes()
    for key in TOP_KEYS:
        if key in ("baseline", "capacity"):
            continue
        drop_report_key(test_run, key)
        assert run_validate(test_run)[0] == 1, key
        test_run.joinpath("report.json").write_bytes(test_good)
    each_required(test_run, fields_in(TEST_RUN_SECTIONS))
    violation_shaped(test_run)
    each_required(test_run, VIOLATION_FIELDS)


def _frozen_other(runs: Runs) -> None:
    """The frozen file no longer has the copied file's bytes (created_ns 1 ns later)."""
    doc = read_json(runs.frozen)
    doc["created_ns"] = CREATED_NS + 1
    runs.frozen.unlink()  # mode 0444: replaced, as a tampering would
    runs.frozen.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")


def _created_at_start(runs: Runs) -> None:
    """The run started when the thresholds were created (created_ns = start_ns)."""
    update_context(runs.run, start_ns=CREATED_NS)


def _replace_thresholds(runs: Runs, text: str) -> None:
    """Both thresholds files hold text, and context.json records its SHA-256 (the hash check
    passes): the file itself is what is wrong."""
    runs.frozen.unlink()
    runs.frozen.write_text(text, encoding="utf-8")
    copy_thresholds(runs.run, runs.frozen)


def _thresholds(edit: Callable[[dict[str, object]], None]) -> Callable[[Runs], None]:
    """A change that edits the frozen thresholds (both files, their hash recorded)."""

    def change(runs: Runs) -> None:
        doc = read_json(runs.frozen)
        edit(doc)
        _replace_thresholds(runs, json.dumps(doc, indent=2) + "\n")

    return change


def _steps(
    edit: Callable[[list[dict[str, object]]], list[dict[str, object]]],
) -> Callable[[dict[str, object]], None]:
    def change(doc: dict[str, object]) -> None:
        steps = doc["steps"]
        assert isinstance(steps, list)
        doc["steps"] = edit(steps)

    return change


def _edited_before_run(edit: Callable[[dict[str, object]], None]) -> Callable[[Runs], None]:
    """The frozen file edited before the limiter run (created_ns and schedule_sha256 kept): the
    run copied the edited bytes and recorded their hash, so all three hashes agree, and its report
    is built against them."""

    def change(runs: Runs) -> None:
        _thresholds(edit)(runs)
        assert build_main([str(runs.run)]) == 0

    return change


def _step_value(step: int, key: str, value: Callable[[object], object]) -> Callable[[Runs], None]:
    """Step step's key in the frozen thresholds set to value(its value), before the run."""

    def edit(steps: list[dict[str, object]]) -> list[dict[str, object]]:
        return [{**s, key: value(s[key])} if s["step"] == step else s for s in steps]

    return _edited_before_run(_steps(edit))


def _relaxed_latency(value: object) -> object:
    assert type(value) is int
    return value + 5 * SECOND


def _baseline_run_other(runs: Runs) -> None:
    """The thresholds name the limiter run as the baseline that set them."""
    _thresholds(lambda doc: doc.update(baseline_run_id=runs.run.name))(runs)


def _report(path: str, value: object) -> Callable[[Runs], None]:
    def change(runs: Runs) -> None:
        edit_report(runs.run, path, value)

    return change


def _reports(*edits: tuple[str, object]) -> Callable[[Runs], None]:
    def change(runs: Runs) -> None:
        for path, value in edits:
            edit_report(runs.run, path, value)

    return change


def _verdict_none_claimed(runs: Runs) -> None:
    """report.json claims verdict none (no T-RPT-2 checks), and the thresholds hash is wrong."""
    edit_report(runs.run, "context.workload.verdict", "none")
    update_context(runs.run, thresholds_sha256="0" * 64)


def _base_scenario_other(runs: Runs) -> None:
    set_context(runs.base, "scenario.name", "other")


EVIDENCE = "evidence_validity.checks"
# check_bud11's reason for a release with no final event in a segment that ended with stop.
RELEASE_OPEN = "api-1.jsonl: line 3: release without final event or segment pre_kill"
# Step 0 recomputed as not passing: its verdict and sustainable_capacity (then step 2's 0.06/s).
RECOMPUTED = {"capacity_verdicts", "sustainable_capacity"}


def both(checks: set[str]) -> dict[str, set[str]]:
    return dict.fromkeys(BOTH, checks)


def every(checks: set[str]) -> dict[str, set[str]]:
    return dict.fromkeys(("capacity", "spike", "none"), checks)


def spike_only(checks: set[str]) -> dict[str, set[str]]:
    return {"spike": checks}


def base_changed(checks: set[str]) -> dict[str, set[str]]:
    """A change to the baseline run itself fails the comparison's checks on every benchmark
    limiter run, and thresholds_file too on capacity and spike runs: their thresholds name that
    baseline as the one that set them."""
    return {**both(checks | {"thresholds_file"}), "none": checks}


# (case, the change, the checks that fail for each verdict the case runs on): each changes one
# value of the valid runs. The T-RPT-2 cases run on capacity and spike runs alike, the baseline
# comparison's on verdict none runs too.
VALUE_CHECKS: list[tuple[str, Callable[[Runs], None], dict[str, set[str]]]] = [
    (
        "thresholds hash differs from context.thresholds_sha256",
        lambda runs: update_context(runs.run, thresholds_sha256="0" * 64),
        both({"thresholds_hash"}),
    ),
    ("thresholds hash differs from the frozen file's", _frozen_other, both({"thresholds_hash"})),
    ("created_ns not before the run's start_ns", _created_at_start, both({"thresholds_created"})),
    (
        "thresholds frozen from another schedule",
        _thresholds(lambda doc: doc.update(schedule_sha256="e" * 64)),
        both({"thresholds_schedule"}),
    ),
    (
        "a latency threshold relaxed before the run, all three hashes agreeing",
        _step_value(2, "latency_threshold_ns", _relaxed_latency),
        both({"thresholds_file"}),
    ),
    (
        "an error-rate threshold relaxed before the run, all three hashes agreeing",
        _step_value(0, "error_rate_threshold", lambda _: 0.5),
        both({"thresholds_file"}),
    ),
    (
        "thresholds naming a baseline_run_id that is no baseline",
        _baseline_run_other,
        both({"thresholds_file"}),
    ),
    (
        "a verdict on a generator-limited step",
        _report("capacity.steps.1.verdict", "pass"),
        both({"generator_limited_no_verdict", "capacity_verdicts"}),
    ),
    (
        "a wrong sustainable_capacity",
        _report("capacity.sustainable_capacity", 0.06),
        both({"sustainable_capacity"}),
    ),
    (
        "both passing steps with a negative achieved rate, sustainable_capacity the larger",
        _reports(
            ("load.steps.0.achieved_sent_per_s", -0.1),
            ("load.steps.2.achieved_sent_per_s", -0.06),
            ("capacity.sustainable_capacity", -0.06),
        ),
        both({"sustainable_capacity"}),
    ),
    (
        "a step over 5 ms of scheduling lag not flagged generator-limited",
        _reports(
            ("load.steps.1.generator_limited", False),
            ("capacity.steps.1.verdict", "pass"),
            ("capacity.sustainable_capacity", 0.2),
        ),
        both({"generator_limited_lag"}),
    ),
    (
        "a baseline with a different schedule_sha256",
        lambda runs: update_context(runs.base, schedule_sha256="f" * 64),
        base_changed({"baseline_schedule"}),
    ),
    (
        "the report's baseline schedule_sha256 differs",
        _report("baseline.schedule_sha256", "f" * 64),
        every({"baseline_schedule"}),
    ),
    (
        "a baseline run that is no baseline",
        lambda runs: update_context(runs.base, baseline=False),
        base_changed({"baseline_run"}),
    ),
    ("a baseline of another scenario", _base_scenario_other, base_changed({"baseline_run"})),
    (
        "a baseline of another seed",
        lambda runs: update_context(runs.base, seed=2),
        base_changed({"baseline_run"}),
    ),
    (
        "the run itself as its baseline",
        lambda runs: edit_report(runs.run, "baseline.run_id", runs.run.name),
        every({"baseline_run"}),
    ),
    (
        "a report.json of another run",
        _report("run_id", "20261007-130000-vld-1"),
        every({"run_id"}),
    ),
    (
        "a run the orchestrator marked failed",
        lambda runs: update_context(runs.run, outcome="failed"),
        every({"outcome"}),
    ),
    (
        "a scenario file other than the one the run pinned",
        lambda runs: edit_scenario(runs.run, lambda text: text + "# edited\n", pin=False),
        every({"scenario_file"}),
    ),
    (
        "a pinned scenario whose verdict is not capacity, spike or none",
        lambda runs: edit_scenario(runs.run, bogus_verdict),
        every({"scenario_file"}),
    ),
    (
        "a pinned scenario whose forced_aof_rewrites_s is a number",
        lambda runs: edit_scenario(runs.run, forced_not_a_list("5")),
        every({"scenario_file"}),
    ),
    (
        "a pinned scenario whose forced_aof_rewrites_s is a string",
        lambda runs: edit_scenario(runs.run, forced_not_a_list('"180"')),
        every({"scenario_file"}),
    ),
    (
        "a context.json scenario without its path",
        lambda runs: drop_context_key(runs.run, "scenario.path"),
        every({"scenario_file"}),
    ),
    (
        "a context.json scenario without its sha256",
        lambda runs: drop_context_key(runs.run, "scenario.sha256"),
        every({"scenario_file"}),
    ),
    (
        "report.json claiming verdict none",
        _verdict_none_claimed,
        both({"thresholds_hash"}),
    ),
    (
        "a step verdict not as recomputed",
        _report("capacity.steps.2.verdict", "fail"),
        both({"capacity_verdicts"}),
    ),
    (
        "step 2 passing with a p99 within step 0's threshold but above its own",
        _report("timing.steps.2.latency_ns.p99", 55 * MS),
        both({"capacity_verdicts"}),
    ),
    (
        "a spike deadline other than the recorded one",
        _report("capacity.spike.deadline_ns", 70 * SECOND + 1),
        spike_only({"spike_deadline", "spike_met"}),
    ),
    (
        "spike met with an outstanding attempt",
        _report("capacity.spike.outstanding", 1),
        spike_only({"spike_met"}),
    ),
    (
        "an outstanding cutoff other than the deadline − the post-spike threshold",
        _report("capacity.spike.outstanding_cutoff_ns", 70 * SECOND),
        spike_only({"spike_met"}),
    ),
    (
        "spike met with a window p99 within the spike step's threshold, above the post-spike's",
        _report("capacity.spike.window_latency_ns.p99", 55 * MS),
        spike_only({"spike_met"}),
    ),
    (
        "log drops",
        _report(f"{EVIDENCE}.log_drops.value", 1),
        spike_only({"evidence_validity.log_drops"}),
    ),
    (
        "listener drops",
        _report(f"{EVIDENCE}.listener_drops.value.sum", 1),
        spike_only({"evidence_validity.listener_drops"}),
    ),
    (
        "fixtures on in a benchmark",
        _report(f"{EVIDENCE}.fixtures_off.value.fixtures_on", 1),
        spike_only({"evidence_validity.fixtures_off"}),
    ),
    (
        "outage over 5 s",
        _report(f"{EVIDENCE}.outage.value.max_outage_ns", 5 * SECOND + 1),
        spike_only({"evidence_validity.outage"}),
    ),
    (
        "two outage segments",
        _report(f"{EVIDENCE}.outage_segments.value", 2),
        spike_only({"evidence_validity.outage_segments"}),
    ),
    (
        "gen.jsonl summary missing",
        _report(f"{EVIDENCE}.gen_summary.value", None),
        spike_only({"evidence_validity.gen_summary"}),
    ),
    (
        "gen.jsonl summary with a log drop",
        _report(f"{EVIDENCE}.gen_summary.value", 1),
        spike_only({"evidence_validity.gen_summary"}),
    ),
    (
        "no rewrite window with forced_aof_rewrites_s set in the scenario file",
        lambda runs: edit_scenario(runs.run, forced_rewrites),
        spike_only({"evidence_validity.rewrite_windows"}),
    ),
    (
        "a pre_kill read failed",
        _report(f"{EVIDENCE}.listener_drops.value.pre_kill_unread", 1),
        spike_only({"evidence_validity.listener_drops"}),
    ),
    (
        "a process segment without its drop count",
        _report(f"{EVIDENCE}.listener_drops.value.segments_without_count", 1),
        spike_only({"evidence_validity.listener_drops"}),
    ),
    (
        "a benchmark on the controlled clock",
        _report(f"{EVIDENCE}.fixtures_off.value.clock_not_store", 1),
        spike_only({"evidence_validity.fixtures_off"}),
    ),
    (
        "a benchmark with a fault",
        _report(f"{EVIDENCE}.fixtures_off.value.faults", 1),
        spike_only({"evidence_validity.fixtures_off"}),
    ),
    (
        "a benchmark without a health read",
        _report(f"{EVIDENCE}.fixtures_off.value.health_reads", 0),
        spike_only({"evidence_validity.fixtures_off"}),
    ),
    (
        "fixtures_off not applied to a benchmark",
        _report(f"{EVIDENCE}.fixtures_off.value.applies", False),
        spike_only({"evidence_validity.fixtures_off"}),
    ),
    (
        "an outage not measured",
        _report(f"{EVIDENCE}.outage.value.unmeasured", 1),
        spike_only({"evidence_validity.outage"}),
    ),
    (
        "a BUD-11a verdict invalid (HAR-13), recorded as such",
        _reports(
            ("correctness.verdict", "invalid"),
            ("correctness.invalid_reasons", [RELEASE_OPEN]),
            (
                f"{EVIDENCE}.correctness.value",
                {"verdict": "invalid", "invalid_reasons": [RELEASE_OPEN]},
            ),
        ),
        spike_only({"evidence_validity.correctness"}),
    ),
    (
        "a BUD-11a verdict invalid, its evidence check recording pass",
        _reports(
            ("correctness.verdict", "invalid"),
            ("correctness.invalid_reasons", [RELEASE_OPEN]),
        ),
        spike_only({"evidence_validity.correctness"}),
    ),
    (
        "a BUD-11a verdict other than the one its evidence check records",
        _report(f"{EVIDENCE}.correctness.value.verdict", "violation"),
        spike_only({"evidence_validity.correctness"}),
    ),
    (
        "capacity null",
        _report("capacity", None),
        {
            "capacity": {"required_keys", "capacity_verdicts"},
            "spike": {"required_keys", "capacity_verdicts", "spike_deadline"},
        },
    ),
    (
        "baseline null on a limiter run",
        _report("baseline", None),
        every({"required_keys", "baseline_schedule"}),
    ),
    *[
        (f"a passing step with {name}", change, both(RECOMPUTED))
        for name, change in (
            ("an unsent first attempt", _report("load.steps.0.unsent", 1)),
            (
                "its window p99 above its threshold",
                _report("timing.steps.0.latency_ns.p99", 60 * MS + 1),
            ),
            (
                "an unexpected error above the threshold",
                _report("results.unexpected_errors.steps.0.unexpected", 1),
            ),
            ("an ERR-9", _report("load.steps.0.unknown_acquires", 1)),
            ("an ERR-16", _report("load.steps.0.unknown_application_outcomes", 1)),
            (
                "no first attempt in its window",
                _report("results.unexpected_errors.steps.0.attempts", 0),
            ),
            ("a partial run", _report("load.steps.0.ran", "partial")),
        )
    ],
    # The thresholds' baseline-step conditions judge the verdicts; a file the baseline did not
    # write so is not its thresholds either.
    *[
        (f"a passing step with {name}", change, both(RECOMPUTED | {"thresholds_file"}))
        for name, change in (
            (
                "no baseline step (the thresholds name no step 0)",
                _thresholds(_steps(lambda steps: [s for s in steps if s["step"] != 0])),
            ),
            (
                "a generator-limited baseline step (the thresholds mark step 0)",
                _thresholds(
                    _steps(
                        lambda steps: [
                            {"step": 0, "generator_limited": True} if s["step"] == 0 else s
                            for s in steps
                        ]
                    )
                ),
            ),
        )
    ],
    (
        "spike met with its window p99 above the threshold",
        _report("capacity.spike.window_latency_ns.p99", 52 * MS + 1),
        spike_only({"spike_met"}),
    ),
    (
        "spike met with an unexpected error in its window",
        _report("capacity.spike.window_unexpected", 1),
        spike_only({"spike_met"}),
    ),
    (
        "spike met with no first attempt in its window",
        _report("capacity.spike.window_attempts", 0),
        spike_only({"spike_met"}),
    ),
    (
        "a check not ok",
        _report(f"{EVIDENCE}.log_drops.ok", False),
        spike_only({"evidence_validity.log_drops"}),
    ),
    (
        "evidence not valid",
        _report("evidence_validity.valid", False),
        spike_only({"evidence_validity.valid"}),
    ),
]
VALUE_CASES = [
    (verdict, case, change, checks)
    for case, change, by_verdict in VALUE_CHECKS
    for verdict, checks in by_verdict.items()
]


@pytest.mark.parametrize(
    ("verdict", "case", "change", "checks"),
    VALUE_CASES,
    ids=[f"{verdict}: {case}" for verdict, case, _, _ in VALUE_CASES],
)
def test_each_value_check_fails(
    tmp_path: Path, verdict: str, case: str, change: Callable[[Runs], None], checks: set[str]
) -> None:
    """A valid synthetic run directory exits 0; changing one value for one check of step 6
    exits 1 naming that check (T-RPT-2's checks on capacity and spike runs, the baseline
    comparison on every benchmark limiter run, the run's own evidence, each §B8 evidence
    check)."""
    runs = valid_runs(tmp_path, verdict)
    assert run_validate(runs.run)[0] == 0
    change(runs)
    code, lines = run_validate(runs.run)
    assert code == 1, case
    assert failed(lines) == checks, (case, lines)
    assert lines[-1] == f"validate: {runs.run}: invalid (full mode)"


# (case, the change, the checks that fail) on the baseline run, in baseline mode.
def _baseline_bud11_invalid(base: Path) -> None:
    """The baseline's evidence_validity records an invalid BUD-11a verdict (its ok left true),
    and its correctness section, which baseline mode does not need, is gone: the recorded value
    alone is judged."""
    value = {"verdict": "invalid", "invalid_reasons": [RELEASE_OPEN]}
    edit_report(base, f"{EVIDENCE}.correctness.value", value)
    drop_report_key(base, "correctness")


BASELINE_CHECKS: list[tuple[str, Callable[[Path], None], set[str]]] = [
    ("marked failed", lambda base: update_context(base, outcome="failed"), {"outcome"}),
    (
        "a report.json of another run",
        lambda base: edit_report(base, "run_id", "20261007-130000-vld-1"),
        {"run_id"},
    ),
    (
        "a step over 5 ms of scheduling lag not flagged generator-limited",
        lambda base: edit_report(base, "load.steps.0.sched_lag_ns.p99", 5 * MS + 1),
        {"generator_limited_lag"},
    ),
    (
        "a scenario file other than the one the run pinned",
        lambda base: edit_scenario(base, lambda text: text + "# edited\n", pin=False),
        {"scenario_file"},
    ),
    (
        "a recorded BUD-11a verdict invalid, with no correctness section (full mode's)",
        _baseline_bud11_invalid,
        {"evidence_validity.correctness"},
    ),
]


@pytest.mark.parametrize(
    ("case", "change", "checks"), BASELINE_CHECKS, ids=[c for c, _, _ in BASELINE_CHECKS]
)
def test_baseline_mode_checks_the_run(
    tmp_path: Path, case: str, change: Callable[[Path], None], checks: set[str]
) -> None:
    """Baseline mode checks the run itself too: a baseline the orchestrator marked failed, a
    report.json of another run, a step whose recorded lag says generator-limited but whose flag
    does not, or a scenario file that is not the pinned one, is invalid (and so freezes
    nothing)."""
    runs = valid_runs(tmp_path)
    change(runs.base)
    code, lines = run_validate(runs.base, "--mode", "baseline")
    assert code == 1, case
    assert failed(lines) == checks, (case, lines)


# Each PRF-5 item a benchmark run records (cmd/orchestrator missingPRF5), made null or empty.
PRF5: list[tuple[str, object]] = [
    ("hardware", None),
    ("os", {}),
    ("versions.go", ""),
    ("versions.valkey", ""),
    ("versions.postgres", ""),
    ("store_settings", {}),
    ("postgres_settings", None),
    ("conformance", None),
    ("settings", {}),
    ("schedule_sha256", None),
    ("thresholds_sha256", None),
]


@pytest.mark.parametrize(("item", "value"), PRF5, ids=[item for item, _ in PRF5])
def test_prf5_items(tmp_path: Path, item: str, value: object) -> None:
    """A benchmark run must have recorded each PRF-5 item: one null or empty fails context_prf5
    naming it, in full mode and (but the limiter run's thresholds_sha256) in baseline mode for
    the baseline; a test run needs none of them."""
    runs = valid_runs(tmp_path)
    set_context(runs.run, item, value)
    code, lines = run_validate(runs.run)
    assert code == 1, item
    prf5 = [line for line in lines if line.startswith("FAIL context_prf5:")]
    assert prf5 == [f"FAIL context_prf5: context.json lacks {item} (PRF-5)"], lines
    if item != "thresholds_sha256":
        set_context(runs.base, item, value)
        code, lines = run_validate(runs.base, "--mode", "baseline")
        assert (code, failed(lines)) == (1, {"context_prf5"}), lines
    test_run = rows_run(
        tmp_path,
        "20261007-130000-tst-1",
        [(1.0, 20.0)],
        [ok_row(6 * SECOND)],
        name="tst",
        run_kind="test",
    )
    assert build_main([str(test_run)]) == 0
    set_context(test_run, item, value)
    assert run_validate(test_run)[0] == 0


def test_unreadable_report_fails(tmp_path: Path) -> None:
    """No report.json, or one that is not a JSON object, is a failed check, not a crash."""
    runs = valid_runs(tmp_path)
    runs.run.joinpath("report.json").write_text("[1]\n", encoding="utf-8")
    code, lines = run_validate(runs.run)
    assert (code, failed(lines)) == (1, {"report_json"})
    runs.run.joinpath("report.json").unlink()
    code, lines = run_validate(runs.run)
    assert (code, failed(lines)) == (1, {"report_json"})


# (case, a change of report.json's text, the checks that fail).
OUT_OF_RANGE: list[tuple[str, Callable[[str], str] | tuple[str, object], set[str]]] = [
    (
        "a count beyond a float's range",
        ("results.unexpected_errors.steps.0.unexpected", 10**400),
        {"capacity_verdicts"},
    ),
    (
        "a spike window count beyond a float's range",
        ("capacity.spike.window_unexpected", 10**400),
        {"spike_met"},
    ),
    ("a negative count", ("results.unexpected_errors.steps.0.attempts", -3), {"capacity_verdicts"}),
    (
        "a negative outage",
        (f"{EVIDENCE}.outage.value.max_outage_ns", -1),
        {"evidence_validity.outage"},
    ),
    (
        "a number beyond a float's range",
        lambda text: text.replace('"sustainable_capacity": 0.1', '"sustainable_capacity": 1e999'),
        {"report_json"},
    ),
]


@pytest.mark.parametrize(
    ("case", "change", "checks"), OUT_OF_RANGE, ids=[c for c, _, _ in OUT_OF_RANGE]
)
def test_numbers_out_of_range(
    tmp_path: Path,
    case: str,
    change: Callable[[str], str] | tuple[str, object],
    checks: set[str],
) -> None:
    """Integers are read from 0 to 2^63 − 1 and floats finite: a value out of range fails the
    check that reads it (never a traceback), and 1e999 is no number at all."""
    runs = valid_runs(tmp_path)
    if isinstance(change, tuple):
        edit_report(runs.run, *change)
    else:
        path = runs.run.joinpath("report.json")
        text = path.read_text(encoding="utf-8")
        assert change(text) != text
        path.write_text(change(text), encoding="utf-8")
    code, lines = run_validate(runs.run)
    assert code == 1, case
    assert failed(lines) == checks, (case, lines)


# (case, the change, the detail of the one FAIL scenario_file line; {path} is the scenario file).
PIN_CASES: list[tuple[str, Callable[[Runs], None], str]] = [
    (
        "no path",
        lambda runs: drop_context_key(runs.run, "scenario.path"),
        "context.json scenario: path missing, sha256 {sha}: want both",
    ),
    (
        "an empty path",
        lambda runs: set_context(runs.run, "scenario.path", ""),
        'context.json scenario: path "", sha256 {sha}: want both',
    ),
    (
        "no sha256",
        lambda runs: drop_context_key(runs.run, "scenario.sha256"),
        'context.json scenario: path "{path}", sha256 missing: want both',
    ),
    (
        "a sha256 that is no string",
        lambda runs: set_context(runs.run, "scenario.sha256", 5),
        'context.json scenario: path "{path}", sha256 5: want both',
    ),
    (
        "verdict bogus",
        lambda runs: edit_scenario(runs.run, bogus_verdict),
        "{path}: verdict 'bogus', forced_aof_rewrites_s []: want capacity, spike or none, and "
        "a list",
    ),
    (
        "forced_aof_rewrites_s a number",
        lambda runs: edit_scenario(runs.run, forced_not_a_list("5")),
        "{path}: verdict 'spike', forced_aof_rewrites_s 5: want capacity, spike or none, and "
        "a list",
    ),
    (
        "forced_aof_rewrites_s a string",
        lambda runs: edit_scenario(runs.run, forced_not_a_list('"180"')),
        "{path}: verdict 'spike', forced_aof_rewrites_s '180': want capacity, spike or none, and "
        "a list",
    ),
]


@pytest.mark.parametrize(("case", "change", "detail"), PIN_CASES, ids=[c for c, _, _ in PIN_CASES])
def test_scenario_pin_refused(
    tmp_path: Path, case: str, change: Callable[[Runs], None], detail: str
) -> None:
    """read_pins refuses a context.json scenario without a non-empty path and a string sha256
    before it reads any file, and a pinned scenario file whose verdict is not capacity, spike or
    none or whose forced_aof_rewrites_s is not a list: one FAIL scenario_file line naming why."""
    runs = valid_runs(tmp_path)
    scenario = read_json(runs.run.joinpath("context.json"))["scenario"]
    assert isinstance(scenario, dict)
    change(runs)
    sha = read_json(runs.run.joinpath("context.json"))["scenario"]
    assert isinstance(sha, dict)
    want = detail.format(path=scenario["path"], sha=json.dumps(sha.get("sha256")))
    code, lines = run_validate(runs.run)
    assert code == 1, case
    assert lines == [f"FAIL scenario_file: {want}", f"validate: {runs.run}: invalid (full mode)"]


@pytest.mark.parametrize("name", ["_vld", "-vld"])
def test_scenario_names(tmp_path: Path, name: str) -> None:
    """A scenario name harness/config accepts ([A-Za-z0-9_-]{1,64}) validates, one starting with
    _ or - among them."""
    runs = valid_runs(tmp_path, name=name)
    code, lines = run_validate(runs.run)
    assert (code, lines) == (0, [f"validate: {runs.run}: valid (full mode)"])


def test_baseline_mode_needs_a_baseline_run(tmp_path: Path) -> None:
    """Baseline mode checks a baseline run (limiter off) only: a limiter run fails it."""
    runs = valid_runs(tmp_path)
    code, lines = run_validate(runs.run, "--mode", "baseline")
    assert (code, failed(lines)) == (1, {"mode"})


def test_imports_standard_library_only() -> None:
    """validate.py imports only the standard library, so `python3 report/validate.py` runs as a
    script (no package on its path) and its recomputation shares no code with the builder."""
    tree = ast.parse(Path(validate.__file__).read_text(encoding="utf-8"))
    modules: set[str] = set()
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            modules.update(alias.name.split(".")[0] for alias in node.names)
        elif isinstance(node, ast.ImportFrom):
            assert node.level == 0 and node.module is not None
            modules.add(node.module.split(".")[0])
    assert modules
    assert modules <= sys.stdlib_module_names, modules - sys.stdlib_module_names


def test_boundaries_pass(tmp_path: Path) -> None:
    """Values on their bounds still pass: a 5 s outage, one outage segment, one /health read, a
    window p99 equal to its threshold for a step (step 0's 60 ms: the baseline's 10 ms + 50 ms)
    and for the spike (the post-spike step's 52 ms), an unexpected-error rate equal to its
    threshold (1 in 1,000 = 0.1 %) for a step and for the spike, and one rewrite window with
    forced_aof_rewrites_s set in the scenario file."""
    runs = valid_runs(tmp_path)
    for path, value in (
        (f"{EVIDENCE}.outage.value.max_outage_ns", 5 * SECOND),
        (f"{EVIDENCE}.outage_segments.value", 1),
        (f"{EVIDENCE}.fixtures_off.value.health_reads", 1),
        ("timing.steps.0.latency_ns.p99", 60 * MS),
        ("capacity.spike.window_latency_ns.p99", 52 * MS),
        ("results.unexpected_errors.steps.0.attempts", 1000),
        ("results.unexpected_errors.steps.0.unexpected", 1),
        ("capacity.spike.window_attempts", 1000),
        ("capacity.spike.window_unexpected", 1),
        (f"{EVIDENCE}.rewrite_windows.value.windows", 1),
    ):
        edit_report(runs.run, path, value)
        code, lines = run_validate(runs.run)
        assert code == 0, (path, lines)
    edit_scenario(runs.run, forced_rewrites)
    code, lines = run_validate(runs.run)
    assert code == 0, ("forced_aof_rewrites_s", lines)


# (case, report.json edits, the verdicts it runs on): values between two steps' thresholds that
# are right when each step is judged by its own and the spike by the post-spike step's; and the
# latest baseline's step flags in report.json's baseline comparison, which judge nothing (the
# frozen thresholds give each step's baseline-step conditions).
OWN_THRESHOLDS: list[tuple[str, list[tuple[str, object]], tuple[str, ...]]] = [
    (
        "the baseline comparison's step 0 missing and generator-limited",
        [
            ("baseline.steps.0.baseline_step_exists", False),
            ("baseline.steps.0.baseline_generator_limited", True),
        ],
        BOTH,
    ),
    (
        "a spike window p99 above the post-spike step's 52 ms, within the spike step's 150 ms: "
        "not met",
        [("capacity.spike.window_latency_ns.p99", 55 * MS), ("capacity.spike.met", False)],
        ("spike",),
    ),
    (
        "step 2's p99 above its own 52 ms, within step 0's 60 ms: fail",
        [("timing.steps.2.latency_ns.p99", 55 * MS), ("capacity.steps.2.verdict", "fail")],
        BOTH,
    ),
    (
        "step 0's p99 within its own 60 ms, above step 2's 52 ms: pass",
        [("timing.steps.0.latency_ns.p99", 55 * MS)],
        BOTH,
    ),
]
OWN_CASES = [
    (verdict, case, edits) for case, edits, verdicts in OWN_THRESHOLDS for verdict in verdicts
]


@pytest.mark.parametrize(
    ("verdict", "case", "edits"), OWN_CASES, ids=[f"{v}: {c}" for v, c, _ in OWN_CASES]
)
def test_own_thresholds(
    tmp_path: Path, verdict: str, case: str, edits: list[tuple[str, object]]
) -> None:
    """Each step's verdict is recomputed against its own thresholds and the spike against the
    post-spike step's (the valid runs' differ per step: 60 ms, 150 ms and 52 ms), with each
    step's baseline-step conditions from the frozen thresholds, not from the baseline comparison:
    a report judging each so still validates."""
    runs = valid_runs(tmp_path, verdict)
    for path, value in edits:
        edit_report(runs.run, path, value)
    code, lines = run_validate(runs.run)
    assert (code, lines) == (0, [f"validate: {runs.run}: valid (full mode)"]), case


def _limits_doc(steps: list[object], created: bool = True) -> dict[str, object]:
    doc: dict[str, object] = {"steps": steps, "spike": None}
    if created:
        doc["created_ns"] = CREATED_NS
    return doc


THRESHOLD_DEFECTS: list[tuple[str, dict[str, object] | str, set[str]]] = [
    ("not JSON", '{"steps": [', {"thresholds_file"}),
    ("no steps", _limits_doc([]), {"thresholds_file"}),
    ("a step that is not an object", _limits_doc([3]), {"thresholds_file"}),
    (
        "a step twice",
        _limits_doc([{"step": 0, "generator_limited": True}] * 2),
        {"thresholds_file"},
    ),
    (
        "generator_limited not a boolean",
        _limits_doc([{"step": 0, "generator_limited": "yes"}]),
        {"thresholds_file"},
    ),
    (
        "a latency threshold of 0",
        _limits_doc([{"step": 0, "latency_threshold_ns": 0, "error_rate_threshold": 0.1}]),
        {"thresholds_file"},
    ),
    (
        "an error-rate threshold of 2",
        _limits_doc([{"step": 0, "latency_threshold_ns": 5, "error_rate_threshold": 2}]),
        {"thresholds_file"},
    ),
    (
        "an error-rate threshold that is not a number",
        _limits_doc([{"step": 0, "latency_threshold_ns": 5, "error_rate_threshold": "0.1"}]),
        {"thresholds_file"},
    ),
    (
        "no created_ns",
        _limits_doc([{"step": 0, "generator_limited": True}], created=False),
        {
            "thresholds_created",
            "thresholds_file",  # no baseline_run_id: not the thresholds a baseline set
            "capacity_verdicts",
            "sustainable_capacity",
            "spike_deadline",
            "spike_met",
        },
    ),
]


@pytest.mark.parametrize(
    ("case", "doc", "checks"), THRESHOLD_DEFECTS, ids=[c for c, _, _ in THRESHOLD_DEFECTS]
)
def test_thresholds_file_defects(
    tmp_path: Path, case: str, doc: dict[str, object] | str, checks: set[str]
) -> None:
    """A thresholds file of the wrong shape fails, even when its hash is the recorded one and it
    names this run's schedule."""
    runs = valid_runs(tmp_path)
    schedule = {"schedule_sha256": schedule_sha256_of(runs.run)}
    _replace_thresholds(runs, doc if isinstance(doc, str) else json.dumps({**doc, **schedule}))
    code, lines = run_validate(runs.run)
    assert code == 1, case
    assert failed(lines) == checks, (case, lines)


@pytest.mark.parametrize("verdict", ["capacity", "none"])
def test_baseline_is_the_thresholds_baseline(tmp_path: Path, verdict: str) -> None:
    """A benchmark limiter run compares against the baseline that froze the thresholds it copied
    (DESIGN §B9 step 3, PRF-7), thresholds.json's baseline_run_id, not the newest baseline in
    runs/index.jsonl: after a second baseline of the same scenario, seed and schedule (its
    latencies 7 ms), the comparison still names and reads the first (its window p99s 10, 100 and
    2 ms), as capacity.thresholds does, both in report.json and in report.md, and the report
    passes validate.py. Thresholds that name no baseline_run_id leave the comparison a problem."""
    runs = valid_runs(tmp_path, verdict)
    second = rows_run(
        tmp_path,
        "20261007-115000-vld-1",
        STEPS,
        [ok_row(s * SECOND, 100_000, 7 * MS) for s in ENTRIES],
        name="vld",
        baseline=True,
        verdict=verdict,
        spike_step=None,
    )
    assert build_main([str(second)]) == 0
    write_index(
        tmp_path,
        [
            {"scenario": "vld", "baseline": True, "run_id": runs.base.name},
            {"scenario": "vld", "baseline": True, "run_id": second.name},
        ],
    )
    assert build_main([str(runs.run)]) == 0
    report = read_json(runs.run.joinpath("report.json"))
    baseline = report["baseline"]
    assert isinstance(baseline, dict)
    assert (baseline["run_id"], baseline["source"], baseline["problem"]) == (
        runs.base.name,
        "thresholds.json",
        None,
    )
    assert [s["baseline_p99_ns"] for s in baseline["steps"]] == [10 * MS, 100 * MS, 2 * MS]
    markdown = runs.run.joinpath("report.md").read_text(encoding="utf-8")
    assert f"| run_id | {runs.base.name} |" in markdown
    if verdict == "capacity":
        capacity = report["capacity"]
        assert isinstance(capacity, dict)
        assert capacity["thresholds"]["baseline_run_id"] == runs.base.name
        assert f"| thresholds.baseline_run_id | {runs.base.name} |" in markdown
    assert run_validate(runs.run) == (0, [f"validate: {runs.run}: valid (full mode)"])

    doc = read_json(runs.frozen)
    del doc["baseline_run_id"]
    _replace_thresholds(runs, json.dumps(doc, indent=2) + "\n")
    assert build_main([str(runs.run)]) == 0
    baseline = read_json(runs.run.joinpath("report.json"))["baseline"]
    assert isinstance(baseline, dict)
    assert (baseline["run_id"], baseline["problem"]) == (
        None,
        "thresholds.json names no baseline_run_id",
    )


def test_bud11_invalid_run_fails(tmp_path: Path) -> None:
    """A run whose BUD-11a evidence check_bud11 judges invalid (HAR-13: here a release with no
    final event in a process segment that ended with stop) builds a report that says so in
    evidence_validity (its correctness check failed, naming the reason, and valid false), and
    validate.py fails it, so report.json, report.md and validate.py agree."""
    runs = valid_runs(tmp_path, "capacity")
    log = runs.run.joinpath("api-1.jsonl")
    lines = log.read_text(encoding="utf-8").splitlines(keepends=True)
    decision = {
        "kind": "decision",
        "deduction_id": "d0",
        "user_id": "u000000",
        "decision_ns": T0_WALL_NS + MS,
        "result": "allowed",
    }
    release = {"kind": "release", "deduction_id": "d0", "release_mono_ns": 9 * SECOND}
    assert json.loads(lines[-1])["event"] == "stop"
    lines[-1:-1] = [json.dumps(decision) + "\n", json.dumps(release) + "\n"]
    log.write_text("".join(lines), encoding="utf-8")
    assert build_main([str(runs.run)]) == 0
    report = read_json(runs.run.joinpath("report.json"))
    reason = "api-1.jsonl: line 4: release without final event or segment pre_kill"
    correctness = report["correctness"]
    assert isinstance(correctness, dict) and correctness["verdict"] == "invalid"
    evidence = report["evidence_validity"]
    assert isinstance(evidence, dict)
    assert evidence["checks"]["correctness"]["ok"] is False
    assert evidence["checks"]["correctness"]["value"] == {
        "verdict": "invalid",
        "invalid_reasons": [reason],
    }
    assert evidence["valid"] is False
    markdown = runs.run.joinpath("report.md").read_text(encoding="utf-8")
    assert "| valid | false |" in markdown and "| valid | true |" not in markdown
    code, lines_out = run_validate(runs.run)
    assert code == 1
    assert failed(lines_out) == {"evidence_validity.correctness", "evidence_validity.valid"}
    assert any(reason in line for line in lines_out), lines_out


def test_missing_baseline_run_named(tmp_path: Path) -> None:
    """A frozen thresholds file needs the baseline run that froze it while it is in use: with
    runs/<baseline_run_id>/ gone, validate.py fails the limiter run naming that cause, not a bare
    "No such file" error, for both the frozen values (thresholds_file) and the comparison
    (baseline_schedule), and a report built again records the same cause."""
    runs = valid_runs(tmp_path, "capacity")
    shutil.rmtree(runs.base)
    code, lines = run_validate(runs.run)
    assert code == 1
    assert failed(lines) == {"thresholds_file", "baseline_schedule"}, lines
    cause = f"the baseline run runs/{runs.base.name} that froze these thresholds is missing"
    assert any(line.startswith(f"FAIL thresholds_file: {cause}: keep it") for line in lines), lines
    compared = f"FAIL baseline_schedule: the baseline run runs/{runs.base.name} the report compares"
    assert any(line.startswith(compared) for line in lines), lines
    assert not any("No such file" in line or "Errno" in line for line in lines), lines
    assert build_main([str(runs.run)]) == 0
    baseline = read_json(runs.run.joinpath("report.json"))["baseline"]
    assert isinstance(baseline, dict)
    assert (baseline["run_id"], baseline["problem"]) == (runs.base.name, cause)


def test_fault_times_required(tmp_path: Path) -> None:
    """A run with faults records each one's actual start and end (PRF-5; plan H5 step 2's
    context): a test run with a store pause passes full mode, and removing the fault's cmd,
    start_ns or end_ns from report.json's context makes validate.py exit 1 naming
    required_keys; a run with no fault (faults null) needs none."""
    pause = {
        "at_s": 2,
        "cmd": "faultctl store pause 4.5s",
        "instance": None,
        "start_ns": T0_WALL_NS + 2 * SECOND,
        "end_ns": T0_WALL_NS + 6_500 * MS,
        "outage_start_ns": T0_WALL_NS + 2 * SECOND,
        "outage_ns": 4_600 * MS,
        "targets": None,
    }
    spec = RunSpec(name="flt", instances=1, steps=[(1.0, 20.0)], faults=[pause])
    run = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[attempt(0, 6 * SECOND), summary(1)],
        apis={1: [lifecycle_start(), lifecycle("ready"), lifecycle("stop")]},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    assert build_main([str(run)]) == 0
    assert run_validate(run)[0] == 0
    good = run.joinpath("report.json").read_bytes()
    for key in ("cmd", "start_ns", "end_ns"):
        drop_report_key(run, f"context.faults.0.{key}")
        code, lines = run_validate(run)
        assert (code, failed(lines)) == (1, {"required_keys"}), (key, lines)
        run.joinpath("report.json").write_bytes(good)
    edit_report(run, "context.faults", None)
    assert run_validate(run)[0] == 0
