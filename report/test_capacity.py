"""The capacity and spike verdicts (IMPLEMENTATION_PLAN H5 step 4; DESIGN §B9; HARNESS PRF-6,
PRF-8, PRF-10, PRF-11). Expected values are hand-computed from the lines each test writes."""

import json
from pathlib import Path

import pytest

from report import InputError
from report.__main__ import main as build_main
from report.build import build
from report.synthetic import (
    SECOND,
    T0_WALL_NS,
    Row,
    attempt,
    copy_thresholds,
    lifecycle,
    lifecycle_start,
    ok_row,
    request,
    rows_run,
    schedule_sha256_of,
    summary,
    thresholds_text,
    write_index,
)

MS = 1_000_000
US = 1_000
CREATED_NS = T0_WALL_NS - 3600 * SECOND


def ok(planned_ns: int, lag_ns: int = 100 * US, latency_ns: int = 2 * MS) -> Row:
    return ok_row(planned_ns, lag_ns, latency_ns)


def bench_run(
    root: Path,
    run_id: str,
    steps: list[tuple[float, float]],
    rows: list[Row],
    *,
    name: str = "cap",
    baseline: bool = False,
    verdict: str = "capacity",
    spike_step: int | None = None,
    tail: dict[str, object] | None = None,
    retries: list[dict[str, object]] | None = None,
) -> Path:
    """A one-instance benchmark run of scenario name (verdict capacity by default), seed 1."""
    return rows_run(
        root,
        run_id,
        steps,
        rows,
        name=name,
        baseline=baseline,
        verdict=verdict,
        spike_step=spike_step,
        tail=tail,
        retries=retries,
    )


def write_thresholds(
    path: Path,
    run_dir: Path,
    steps: dict[int, tuple[int, float] | None],
    spike: dict[str, object] | None = None,
) -> Path:
    """A frozen thresholds file written by hand (report/thresholds.py's shape), frozen from the
    schedule run_dir ran."""
    text = thresholds_text(steps, spike, CREATED_NS, schedule_sha256_of(run_dir))
    path.write_text(text, encoding="utf-8")
    return path


def capacity(report: dict[str, object]) -> dict[str, object]:
    value = report["capacity"]
    assert isinstance(value, dict)
    return value


def step_rows(cap: dict[str, object]) -> list[dict[str, object]]:
    rows = cap["steps"]
    assert isinstance(rows, list)
    return rows


def window(k: int, n: int) -> list[int]:
    """n entries in step k (10 s steps) inside its observation window: from 5 s on, 0.5 s apart."""
    return [k * 10 * SECOND + 5 * SECOND + j * SECOND // 2 for j in range(n)]


def planned_order(rows: list[Row]) -> list[Row]:
    """rows in planned order (a stable sort: rows planned at the same time keep their order)."""
    return sorted(rows, key=lambda r: r[0])


SPIKE_STEPS = [(1.0, 30.0), (5.0, 10.0), (1.0, 100.0)]  # spike_step 1: its end 40 s
SPIKE = {
    "spike_step": 1,
    "post_spike_step": 2,
    "spike_end_ns": 40 * SECOND,
    "deadline_ns": 70 * SECOND,
    "window_start_ns": 70 * SECOND,
    "window_end_ns": 130 * SECOND,
}
# The post-spike step's thresholds are its own (20 ms, 25 %): the spike step's (1 s, 100 %) and
# the first step's (5 ms, 0.1 %) would judge the window otherwise.
SPIKE_LIMITS: dict[int, tuple[int, float] | None] = {
    0: (5 * MS, 0.001),
    1: (1 * SECOND, 1.0),
    2: (20 * MS, 0.25),
}
# The outstanding rule's cutoff: the deadline − the post-spike step's latency threshold (20 ms).
CUTOFF_NS = 69_980 * MS


def spike_rows() -> list[Row]:
    """The window [70 s, 130 s] holds 4 first attempts, 1 of them unexpected (rate 25 %, the
    threshold), p99 20 ms (the threshold): the one at the deadline, at 100 s and 101 s, and at
    the window's end. A fast 503 1 ms before the deadline and a slow 503 1 ns after the window's
    end are outside it (in it, either would miss the thresholds). Of the 3 attempts planned
    before the deadline, the 2 planned before the cutoff (69.98 s) ended by it; the fast 503 at
    69.999 s is not judged."""
    unknown = "database_unavailable"
    return [
        ok(50 * SECOND, latency_ns=1 * MS),
        ok(69_900 * MS, latency_ns=10 * MS),
        (69_999 * MS, 100 * US, 500 * US, 503, unknown, True),
        ok(70 * SECOND, latency_ns=20 * MS),
        (100 * SECOND, 100 * US, 3 * MS, 503, unknown, True),
        ok(101 * SECOND),
        ok(130 * SECOND),
        (130 * SECOND + 1, 100 * US, 500 * MS, 503, unknown, True),
    ]


def spike_capacity(
    root: Path,
    run_id: str,
    rows: list[Row],
    limits: dict[int, tuple[int, float] | None],
    spike: dict[str, object] | None,
    *,
    copied: bool = True,
) -> dict[str, object]:
    """The capacity section of a benchmark limiter spike run with rows and frozen thresholds of
    limits and recorded spike parameters (null for None); none at all unless copied."""
    unsent = sum(1 for r in rows if r[1] is None or not r[5])
    run_dir = bench_run(
        root,
        run_id,
        SPIKE_STEPS,
        rows,
        name="spk",
        verdict="spike",
        spike_step=1,
        tail=summary(len(rows), unsent=unsent),
    )
    if copied:
        frozen = write_thresholds(root.joinpath(f"{run_id}.json"), run_dir, limits, spike)
        copy_thresholds(run_dir, frozen)
    return capacity(build(run_dir))


def spike_report(root: Path, run_id: str, rows: list[Row]) -> dict[str, object]:
    """The spike verdict of a benchmark limiter spike run with rows and the frozen thresholds."""
    spike = spike_capacity(root, run_id, rows, SPIKE_LIMITS, dict(SPIKE))["spike"]
    assert isinstance(spike, dict)
    return spike


def test_sustainable_and_spike(tmp_path: Path) -> None:
    """Generator-limited steps and steps whose baseline step is missing or generator-limited get
    no verdict, so sustainable_capacity is the highest achieved rate among the others that pass
    (PRF-8, PRF-11); the baseline step's conditions are the frozen thresholds' (the baseline that
    set them), not the latest baseline's. The spike window is [end + 30 s, end + 90 s], judged
    against the post-spike step's thresholds; a first attempt planned before the deadline and
    still outstanding at it leaves the spike not met (PRF-6), under the orchestrator's reading of
    plan step 4: a written one counts when planned before the deadline − the post-spike step's
    latency threshold (the cutoff) and unanswered at the deadline, an unsent one whenever it was
    planned before the deadline."""
    # Six 10 s steps with 2, 9, 3, 8, 6 and 7 entries (achieved 0.2 … 0.9/s). The run's step 1
    # has a 6 ms lag (generator-limited). The frozen thresholds mark step 3 generator-limited and
    # name no step 4 or 5 (the baseline that set them stopped early); the latest baseline, run
    # later on the same schedule, has a 6 ms lag in step 2 and ran every step in full, which
    # changes nothing.
    counts = [2, 9, 3, 8, 6, 7]
    rows: list[Row] = []
    base_rows: list[Row] = []
    for k, n in enumerate(counts):
        for j, ns in enumerate(window(k, n)):
            rows.append(ok(ns, lag_ns=6 * MS if (k, j) == (1, 0) else 100 * US))
            base_rows.append(ok(ns, lag_ns=6 * MS if (k, j) == (2, 0) else 100 * US))
    base_dir = bench_run(
        tmp_path, "20261007-110000-cap-1", [(1.0, 10.0)] * 6, base_rows, baseline=True
    )
    assert build_main([str(base_dir)]) == 0
    run_dir = bench_run(tmp_path, "20261007-120000-cap-1", [(1.0, 10.0)] * 6, rows)
    write_index(tmp_path, [{"scenario": "cap", "baseline": True, "run_id": base_dir.name}])
    limits: dict[int, tuple[int, float] | None] = {
        0: (SECOND, 0.5),
        1: (SECOND, 0.5),
        2: (SECOND, 0.5),
        3: None,
    }
    copy_thresholds(run_dir, write_thresholds(tmp_path.joinpath("cap-1.json"), run_dir, limits))
    cap = capacity(build(run_dir))
    got = [(s["verdict"], s["excluded"], s["achieved_sent_per_s"]) for s in step_rows(cap)]
    assert got == [
        ("pass", None, 0.2),
        (None, "generator-limited (PRF-8)", 0.9),
        ("pass", None, 0.3),
        (None, "baseline step generator-limited (§B9)", 0.8),
        (None, "baseline step missing (§B9)", 0.6),
        (None, "baseline step missing (§B9)", 0.7),
    ]
    assert [
        (s["baseline_step_exists"], s["baseline_generator_limited"]) for s in step_rows(cap)
    ] == [
        (True, False),
        (True, False),
        (True, False),
        (True, True),
        (False, False),
        (False, False),
    ]
    assert (cap["sustainable_capacity"], cap["sustainable_step"]) == (0.3, 2)
    assert cap["spike"] is None

    # The spike: deadline 70 s, window [70 s, 130 s] (closed), met.
    spike = spike_report(tmp_path, "20261007-120001-spk-1", spike_rows())
    assert {k: spike[k] for k in SPIKE} == SPIKE
    assert spike["recorded"] == SPIKE
    assert (spike["window_attempts"], spike["window_unexpected"]) == (4, 1)
    assert spike["window_latency_ns"] == {"count": 4, "p50": 2 * MS, "p95": 20 * MS, "p99": 20 * MS}
    assert (spike["window_planned"], spike["window_unsent"]) == (4, 0)
    assert (spike["latency_threshold_ns"], spike["error_rate_threshold"]) == (20 * MS, 0.25)
    assert spike["outstanding_cutoff_ns"] == CUTOFF_NS
    assert (spike["planned_before_deadline"], spike["finished_by_deadline"]) == (3, 2)
    assert spike["not_judged"] == 1
    assert spike["outstanding"] == 0
    assert spike["problems"] == []
    assert spike["met"] is True
    # A window p99 of 25 ms misses the post-spike step's 20 ms (the spike step's 1 s would not).
    slow = spike_rows()
    slow[3] = ok(70 * SECOND, latency_ns=25 * MS)
    spike = spike_report(tmp_path, "20261007-120002-spk-1", slow)
    assert spike["met"] is False
    assert spike["problems"] == ["window p99 above the post-spike step's latency threshold"]

    # Outstanding at the deadline (the cutoff is 69.98 s).
    outstanding_cases: list[tuple[Row, dict[str, int]]] = [
        # Planned at 60 s, before the cutoff, answered 1 ns after the deadline: outstanding.
        (
            ok(60 * SECOND, latency_ns=10 * SECOND - 100 * US + 1),
            {"never_written": 0, "ended_after_deadline": 1, "no_line": 0},
        ),
        # Planned at 69.99 s, inside the last threshold window, answered at 70.0051 s, after the
        # deadline: not judged, so the spike is met.
        (
            ok(69_990 * MS, latency_ns=15 * MS),
            {"never_written": 0, "ended_after_deadline": 0, "no_line": 0},
        ),
        # Planned at 69.99 s and never written: an unsent one counts whenever it was planned
        # before the deadline.
        (
            (69_990 * MS, None, 0, 0, "transport_error", True),
            {"never_written": 1, "ended_after_deadline": 0, "no_line": 0},
        ),
    ]
    for n, (extra, detail) in enumerate(outstanding_cases):
        rows = planned_order([*spike_rows(), extra])
        spike = spike_report(tmp_path, f"20261007-12000{3 + n}-spk-1", rows)
        count = sum(detail.values())
        assert spike["outstanding_detail"] == detail, n
        assert (spike["outstanding"], spike["met"]) == (count, count == 0), n
        assert spike["problems"] == (
            [f"first attempts outstanding at the deadline: {count}"] if count else []
        ), n


@pytest.mark.parametrize(
    ("case", "extra", "outstanding"),
    [
        # Written 100 µs after 60 s, ending exactly at the deadline: answered by it.
        ("ends at the deadline", ok(60 * SECOND, latency_ns=10 * SECOND - 100 * US), None),
        # Ending 1 ns after the deadline: still outstanding at it.
        (
            "ends after the deadline",
            ok(60 * SECOND, latency_ns=10 * SECOND - 100 * US + 1),
            {"never_written": 0, "ended_after_deadline": 1, "no_line": 0},
        ),
        # Never written (sent_ns null): no response or failure was logged by the deadline.
        (
            "never written",
            (65 * SECOND, None, 0, 0, "transport_error", True),
            {"never_written": 1, "ended_after_deadline": 0, "no_line": 0},
        ),
        # Planned but never launched (a generator drop): no gen.jsonl line at all.
        (
            "never launched",
            (66 * SECOND, None, 0, 0, None, False),
            {"never_written": 0, "ended_after_deadline": 0, "no_line": 1},
        ),
        # Planned 1 ns before the cutoff, answered 20 ms + 1 ns after its write: after the deadline.
        (
            "planned just before the cutoff, answered after the deadline",
            ok(CUTOFF_NS - 1, latency_ns=20 * MS + 1),
            {"never_written": 0, "ended_after_deadline": 1, "no_line": 0},
        ),
        # Planned at the cutoff, answered after the deadline: not judged.
        ("planned at the cutoff", ok(CUTOFF_NS, latency_ns=20 * MS + 1), None),
        # A generator drop planned after the cutoff, before the deadline: still outstanding.
        (
            "never launched after the cutoff",
            (69_990 * MS, None, 0, 0, None, False),
            {"never_written": 0, "ended_after_deadline": 0, "no_line": 1},
        ),
    ],
)
def test_spike_outstanding(
    tmp_path: Path, case: str, extra: Row, outstanding: dict[str, int] | None
) -> None:
    """met requires that no first attempt is outstanding at the deadline (PRF-6, the
    orchestrator's reading of plan step 4): one planned before the cutoff (the deadline − the
    post-spike step's latency threshold) that ended after the deadline is; one planned at or
    after the cutoff is not judged; one never written and a planned entry never launched are,
    wherever before the deadline they were planned; one that ended exactly at the deadline is
    not."""
    rows = planned_order([*spike_rows(), extra])
    spike = spike_report(tmp_path, "20261007-120013-spk-1", rows)
    assert spike["planned_before_deadline"] == 4, case
    if outstanding is None:
        assert (spike["outstanding"], spike["met"]) == (0, True), case
        return
    assert spike["outstanding_detail"] == outstanding, case
    assert spike["outstanding"] == 1, case
    assert spike["met"] is False, case
    assert spike["problems"] == ["first attempts outstanding at the deadline: 1"], case


def test_spike_retries_apart(tmp_path: Path) -> None:
    """Only first attempts count (§B9; plan step 4: attempt-1 requests): a 429 planned at 69.9 s
    whose attempt 2 is written at 70.5 s, after the deadline, and a 429 in the window whose slow
    retry (500 ms) is planned in it leave met, the outstanding count and the window's counts as
    they are without the retry lines."""
    rows = planned_order(
        [
            *spike_rows(),
            (69_900 * MS, 100 * US, 1 * MS, 429, "rate_limited", True),
            (101_500 * MS, 100 * US, 1 * MS, 429, "rate_limited", True),
        ]
    )
    early = next(i for i, r in enumerate(rows) if r[3] == 429 and r[0] == 69_900 * MS)
    late = next(i for i, r in enumerate(rows) if r[3] == 429 and r[0] == 101_500 * MS)
    retries = [
        attempt(early, 69_900 * MS, attempt_no=2, sent_ns=70_500 * MS, latency=2 * MS),
        attempt(late, 101_500 * MS, attempt_no=2, sent_ns=102_500 * MS, latency=500 * MS),
    ]
    keys = ("met", "outstanding", "window_attempts", "window_unexpected", "window_latency_ns")
    got = []
    for n, lines in enumerate((None, retries)):
        run_dir = bench_run(
            tmp_path,
            f"20261007-12002{n}-spk-1",
            SPIKE_STEPS,
            rows,
            name="spk",
            verdict="spike",
            spike_step=1,
            tail=summary(len(rows)),
            retries=lines,
        )
        frozen = tmp_path.joinpath(f"spk-{n}.json")
        copy_thresholds(run_dir, write_thresholds(frozen, run_dir, SPIKE_LIMITS, dict(SPIKE)))
        report = build(run_dir)
        load = report["load"]
        assert isinstance(load, dict)
        assert load["steps"][2]["retries_429"] == (0 if lines is None else 2)
        spike = capacity(report)["spike"]
        assert isinstance(spike, dict)
        got.append({k: spike[k] for k in keys})
    # The window: 5 first attempts (the 429 at 101.5 s among them), 1 unexpected, p99 20 ms.
    assert got[0] == {
        "met": True,
        "outstanding": 0,
        "window_attempts": 5,
        "window_unexpected": 1,
        "window_latency_ns": {"count": 5, "p50": 2 * MS, "p95": 20 * MS, "p99": 20 * MS},
    }
    assert got[1] == got[0]


def test_spike_recorded_parameters(tmp_path: Path) -> None:
    """The spike is met only against the deadline and window recorded before the run (PRF-6)
    and the post-spike step's thresholds: none recorded, others recorded, a post-spike step
    without thresholds, or no thresholds file at all, and it is not met."""
    cases: list[tuple[dict[int, tuple[int, float] | None], dict[str, object] | None, list[str]]] = [
        (
            SPIKE_LIMITS,
            None,
            ["no recovery deadline and window recorded in thresholds.json (PRF-6)"],
        ),
        (
            SPIKE_LIMITS,
            {**SPIKE, "deadline_ns": 70 * SECOND + 1, "window_start_ns": 70 * SECOND + 1},
            ["thresholds.json recorded another deadline or window than the scenario's"],
        ),
        (
            {**SPIKE_LIMITS, 2: None},
            dict(SPIKE),
            ["no thresholds for the post-spike step 2"],
        ),
    ]
    for n, (limits, spike, problems) in enumerate(cases):
        cap = spike_capacity(tmp_path, f"20261007-12001{n}-spk-1", spike_rows(), limits, spike)
        got = cap["spike"]
        assert isinstance(got, dict)
        assert (got["met"], got["problems"]) == (False, problems), n
    # No thresholds file: no step verdicts, and the spike has neither its recorded parameters nor
    # the post-spike step's thresholds.
    cap = spike_capacity(
        tmp_path, "20261007-120019-spk-1", spike_rows(), SPIKE_LIMITS, None, copied=False
    )
    assert cap["thresholds"] is None
    assert cap["problem"] == "no thresholds.json: the run copied no thresholds"
    assert [s["verdict"] for s in step_rows(cap)] == [None, None, None]
    got = cap["spike"]
    assert isinstance(got, dict)
    assert got["met"] is False
    assert got["problems"] == [
        "no recovery deadline and window recorded in thresholds.json (PRF-6)",
        "no thresholds for the post-spike step 2",
    ]


@pytest.mark.parametrize("schedule", ["e" * 64, None], ids=["another schedule", "null"])
def test_thresholds_of_another_schedule(tmp_path: Path, schedule: str | None) -> None:
    """Thresholds frozen from another schedule (a scenario edited in place keeps its name and
    seed, so its old frozen file is reused), or with a null one, judge nothing: capacity.problem
    says so, no step gets a verdict and the spike is not met (a comparison is valid only on the
    same schedule, plan H5 step 6)."""
    rows = spike_rows()
    run_dir = bench_run(
        tmp_path,
        "20261007-120030-spk-1",
        SPIKE_STEPS,
        rows,
        name="spk",
        verdict="spike",
        spike_step=1,
        tail=summary(len(rows)),
    )
    frozen = tmp_path.joinpath("spk-1.json")
    frozen.write_text(thresholds_text(SPIKE_LIMITS, SPIKE, CREATED_NS, schedule), encoding="utf-8")
    copy_thresholds(run_dir, frozen)
    cap = capacity(build(run_dir))
    problem = (
        f"thresholds.json was frozen from another schedule (its schedule_sha256 {schedule}, this "
        f"run's {schedule_sha256_of(run_dir)}): no verdicts, as a comparison is valid only on "
        "the same schedule"
    )
    assert cap["problem"] == problem
    excluded = "no thresholds to judge it by (capacity.problem)"
    assert [(s["verdict"], s["excluded"]) for s in step_rows(cap)] == [(None, excluded)] * 3
    assert (cap["sustainable_capacity"], cap["sustainable_step"]) == (None, None)
    spike = cap["spike"]
    assert isinstance(spike, dict)
    assert (spike["met"], spike["problems"]) == (False, [problem])


def test_step_verdict_conditions(tmp_path: Path) -> None:
    """Each condition of a step's verdict on its own (plan H5 step 4): window p99 and unexpected
    error rate at their thresholds pass, 1 ns or one error more fails; one unsent first attempt,
    one ERR-9 or one ERR-16 response fails; a step the thresholds do not name (its baseline step
    missing) or mark generator_limited (its baseline step generator-limited), one with no first
    attempt in its window, and one that ran in part get no verdict. Of two passing steps with the
    same achieved rate the first is the sustainable step. Throughput counts admitted, 429s and
    rows created (PRF-10, PRF-11)."""
    unknown, err9, err16 = (
        "database_unavailable",
        "limiter_outcome_unknown",
        "application_outcome_unknown",
    )

    def at(k: int, j: int) -> int:
        return k * 10 * SECOND + 5 * SECOND + j * SECOND

    rows: list[Row] = [
        # 0: p99 10 ms (the threshold), 1 of 4 unexpected (25 %, the threshold): pass.
        ok(at(0, 0), latency_ns=10 * MS),
        (at(0, 1), 100 * US, 2 * MS, 503, unknown, True),
        ok(at(0, 2)),
        ok(at(0, 3)),
        # 1: p99 10 ms + 1 ns.
        ok(at(1, 0), latency_ns=10 * MS + 1),
        ok(at(1, 1)),
        # 2: 2 of 4 unexpected.
        (at(2, 0), 100 * US, 2 * MS, 503, unknown, True),
        (at(2, 1), 100 * US, 2 * MS, 503, unknown, True),
        ok(at(2, 2)),
        ok(at(2, 3)),
        # 3: 100 entries, one never launched (no line): unsent 1. With 100 planned the p99 lag is
        # the 99th written one (0.1 ms), so the step is not generator-limited.
        *[
            ok(35 * SECOND + j * 50 * MS)
            if j != 50
            else (35 * SECOND + j * 50 * MS, None, 0, 0, None, False)
            for j in range(100)
        ],
        # 4 and 5: an ERR-9, an ERR-16 (each 1 of 4 unexpected, within 25 %).
        (at(4, 0), 100 * US, 2 * MS, 503, err9, True),
        ok(at(4, 1)),
        ok(at(4, 2)),
        ok(at(4, 3)),
        (at(5, 0), 100 * US, 2 * MS, 503, err16, True),
        ok(at(5, 1)),
        ok(at(5, 2)),
        ok(at(5, 3)),
        # 6 and 7: no thresholds (step 6 not named, step 7 marked generator_limited).
        ok(at(6, 0)),
        ok(at(7, 0)),
        # 8: only in its first 5 s.
        ok(80 * SECOND + SECOND),
        # 9: passes at step 0's achieved rate.
        ok(at(9, 0)),
        ok(at(9, 1)),
        ok(at(9, 2)),
        ok(at(9, 3)),
        # 10: the run stopped (stop_early) after step 9: its first entry planned, not the rest.
        ok(at(10, 0)),
        ok(at(10, 1)),
    ]
    steps = [(1.0, 10.0)] * 11
    base_dir = bench_run(
        tmp_path,
        "20261007-110000-cap-1",
        steps,
        [ok(r[0]) for r in rows],
        baseline=True,
    )
    assert build_main([str(base_dir)]) == 0
    # Requests arriving in step 0: 3 allowed (2 rows created, 1 existed) and 2 limited.
    api: list[dict[str, object]] = [lifecycle_start(), lifecycle("ready")]
    for n, (status, result, existed) in enumerate(
        [
            (200, "allowed", False),
            (200, "allowed", False),
            (200, "allowed", True),
            (429, "limited", False),
            (429, "limited", False),
        ]
    ):
        api.append(
            request(
                1,
                f"q{n}",
                arrival_ns=T0_WALL_NS + 6 * SECOND,
                status=status,
                error="rate_limited" if status == 429 else "",
                result=result,
                existed=existed,
            )
        )
    api.append(lifecycle("stop"))
    logged = [(*r[:5], r[5] and i < len(rows) - 1) for i, r in enumerate(rows)]
    run_dir = rows_run(
        tmp_path,
        "20261007-120000-cap-1",
        steps,
        logged,
        name="cap",
        verdict="capacity",
        tail=summary(len(rows) - 1, unsent=1, stopped_after_step=9, excluded_step=10),
        api=api,
    )
    write_index(tmp_path, [{"scenario": "cap", "baseline": True, "run_id": base_dir.name}])
    limits: dict[int, tuple[int, float] | None] = dict.fromkeys(range(11), (10 * MS, 0.25))
    del limits[6]
    limits[7] = None
    copy_thresholds(run_dir, write_thresholds(tmp_path.joinpath("cap-1.json"), run_dir, limits))
    cap = capacity(build(run_dir))
    rows_out = step_rows(cap)
    assert [(s["verdict"], s["excluded"], s["failed"]) for s in rows_out] == [
        ("pass", None, []),
        ("fail", None, ["p99"]),
        ("fail", None, ["unexpected_errors"]),
        ("fail", None, ["unsent"]),
        ("fail", None, ["unknown_outcomes"]),
        ("fail", None, ["unknown_outcomes"]),
        (None, "baseline step missing (§B9)", []),
        (None, "baseline step generator-limited (§B9)", []),
        (None, "no first attempt in the observation window", []),
        ("pass", None, []),
        (None, "did not run in full (ran partial)", []),
    ]
    assert (cap["sustainable_capacity"], cap["sustainable_step"]) == (0.4, 0)
    first = rows_out[0]
    assert {k: first[k] for k in ("p99_ns", "latency_threshold_ns", "latency_ok")} == {
        "p99_ns": 10 * MS,
        "latency_threshold_ns": 10 * MS,
        "latency_ok": True,
    }
    assert (first["window_attempts"], first["window_unexpected"], first["errors_ok"]) == (
        4,
        1,
        True,
    )
    assert (first["unexpected_rate"], first["error_rate_threshold"]) == (0.25, 0.25)
    assert (rows_out[1]["latency_ok"], rows_out[2]["errors_ok"]) == (False, False)
    assert (rows_out[3]["unsent"], rows_out[3]["offered_per_s"]) == (1, 10.0)
    assert (rows_out[3]["generator_limited"], rows_out[3]["achieved_sent_per_s"]) == (False, 9.9)
    assert (rows_out[4]["unknown_outcomes"], rows_out[5]["unknown_outcomes"]) == (1, 1)
    assert {
        k: first[k]
        for k in (
            "offered_per_s",
            "achieved_sent_per_s",
            "admitted",
            "admitted_per_s",
            "limited_429",
            "limited_429_per_s",
            "unique_rows",
            "unique_rows_per_s",
        )
    } == {
        "offered_per_s": 0.4,
        "achieved_sent_per_s": 0.4,
        "admitted": 3,
        "admitted_per_s": 0.3,
        "limited_429": 2,
        "limited_429_per_s": 0.2,
        "unique_rows": 2,
        "unique_rows_per_s": 0.2,
    }


def test_degraded_rows_count_as_rows(tmp_path: Path) -> None:
    """A successful degraded request (ERR-3: 200, degraded) has its DB row "as ERR-1/2" (HARNESS
    §3): with existed false it is a new row (PRF-2 new_rows, the step's new_rows and its
    unique-row throughput, PRF-11), with existed true a duplicate row avoided; each stays a
    degraded admission. A degraded request whose DB work failed (ERR-14) is neither."""
    api: list[dict[str, object]] = [lifecycle_start(), lifecycle("ready")]
    for n, (at_s, status, error, result, existed) in enumerate(
        [
            (1, 200, "", "allowed", False),  # ERR-1, step 0
            (2, 200, "", "degraded", False),  # ERR-3, a new row, step 0
            (3, 200, "", "degraded", False),  # ERR-3, a new row, step 0
            (4, 200, "", "degraded", True),  # ERR-3, its row existed, step 0
            (5, 503, "database_unavailable", "degraded", False),  # ERR-14, no row
            (12, 200, "", "allowed", True),  # ERR-2, step 1
            (13, 200, "", "degraded", False),  # ERR-3, a new row, step 1
        ]
    ):
        api.append(
            request(
                1,
                f"q{n}",
                arrival_ns=T0_WALL_NS + at_s * SECOND,
                status=status,
                error=error,
                result=result,
                existed=existed,
            )
        )
    api.append(lifecycle("stop"))
    run_dir = rows_run(
        tmp_path,
        "20261007-120000-deg-1",
        [(1.0, 10.0)] * 2,
        [ok(6 * SECOND), ok(16 * SECOND)],
        name="deg",
        verdict="capacity",
        api=api,
    )
    report = build(run_dir)
    results = report["results"]
    assert isinstance(results, dict)
    prf2 = results["prf2"]
    assert isinstance(prf2, dict)
    assert {k: prf2[k] for k in ("new_rows", "duplicate_rows_avoided", "degraded_admissions")} == {
        "new_rows": 4,
        "duplicate_rows_avoided": 2,
        "degraded_admissions": 4,
    }
    by_err = results["by_err"]
    assert isinstance(by_err, dict)
    assert (by_err["ERR-1"], by_err["ERR-2"], by_err["ERR-3"], by_err["ERR-14"]) == (1, 1, 4, 1)
    results_steps = results["steps"]
    assert isinstance(results_steps, list)
    assert [s["new_rows"] for s in results_steps] == [3, 1]
    rows_out = step_rows(capacity(report))
    assert [(s["unique_rows"], s["unique_rows_per_s"]) for s in rows_out] == [(3, 0.3), (1, 0.1)]


def test_capacity_only_for_bench_limiter_runs(tmp_path: Path) -> None:
    """capacity is null for a test-build run and a baseline (whatever their verdict) and for a
    benchmark limiter run with verdict none; a benchmark limiter capacity run has it."""
    cases = [
        ("test", False, "capacity", False),
        ("bench", True, "capacity", False),
        ("bench", False, "none", False),
        ("bench", False, "capacity", True),
    ]
    for n, (run_kind, baseline, verdict, has) in enumerate(cases):
        run_dir = rows_run(
            tmp_path,
            f"2026100{n}-120000-only-1",
            [(1.0, 20.0)],
            [ok(6 * SECOND)],
            name="only",
            run_kind=run_kind,
            baseline=baseline,
            verdict=verdict,
        )
        assert (build(run_dir)["capacity"] is not None) is has, (run_kind, baseline, verdict)


MALFORMED: list[tuple[str, dict[str, object] | str, str]] = [
    ("not JSON", '{"steps": [', "json"),
    (
        "no created_ns",
        {"steps": [{"step": 0, "generator_limited": True}], "spike": None},
        "created_ns",
    ),
    ("no steps", {"created_ns": 1, "steps": [], "spike": None}, "steps"),
    (
        "a step outside the scenario",
        {"created_ns": 1, "steps": [{"step": 1, "generator_limited": True}], "spike": None},
        "step",
    ),
    (
        "a step twice",
        {"created_ns": 1, "steps": [{"step": 0, "generator_limited": True}] * 2, "spike": None},
        "step",
    ),
    (
        "generator_limited not a boolean",
        {"created_ns": 1, "steps": [{"step": 0, "generator_limited": "yes"}], "spike": None},
        "generator_limited",
    ),
    (
        "a latency threshold of 0",
        {
            "created_ns": 1,
            "steps": [{"step": 0, "latency_threshold_ns": 0, "error_rate_threshold": 0.1}],
            "spike": None,
        },
        "latency_threshold_ns",
    ),
    (
        "a latency threshold not an integer",
        {
            "created_ns": 1,
            "steps": [{"step": 0, "latency_threshold_ns": 1.5, "error_rate_threshold": 0.1}],
            "spike": None,
        },
        "latency_threshold_ns",
    ),
    (
        "an error-rate threshold above 1",
        {
            "created_ns": 1,
            "steps": [{"step": 0, "latency_threshold_ns": 5, "error_rate_threshold": 1.5}],
            "spike": None,
        },
        "error_rate_threshold",
    ),
    (
        "a spike whose post-spike step is not after it",
        {
            "created_ns": 1,
            "steps": [{"step": 0, "generator_limited": True}],
            "spike": {**SPIKE, "spike_step": 0, "post_spike_step": 0},
        },
        "post_spike_step",
    ),
    (
        "a schedule_sha256 that is not a string",
        {
            "created_ns": 1,
            "steps": [{"step": 0, "generator_limited": True}],
            "spike": None,
            "schedule_sha256": 7,
        },
        "schedule_sha256",
    ),
    (
        "a spike without its window end",
        {
            "created_ns": 1,
            "steps": [{"step": 0, "generator_limited": True}],
            "spike": {k: v for k, v in SPIKE.items() if k != "window_end_ns"},
        },
        "window_end_ns",
    ),
]


@pytest.mark.parametrize(("case", "doc", "field"), MALFORMED, ids=[m[0] for m in MALFORMED])
def test_malformed_thresholds(
    tmp_path: Path, case: str, doc: dict[str, object] | str, field: str
) -> None:
    """A copied thresholds file that is not the shape hitgen and the report read is malformed
    evidence: an InputError naming thresholds.json and the field."""
    run_dir = bench_run(tmp_path, "20261007-120000-bad-1", [(1.0, 20.0)], [ok(6 * SECOND)])
    text = doc if isinstance(doc, str) else json.dumps(doc)
    run_dir.joinpath("thresholds.json").write_text(text, encoding="utf-8")
    with pytest.raises(InputError) as caught:
        build(run_dir)
    assert caught.value.record.startswith("thresholds.json"), case
    assert caught.value.field == field, case


def test_spike_window_population(tmp_path: Path) -> None:
    """In the spike window, as in a step's observation window, every first attempt counts in the
    error denominator, a timeout and a failure before the write as unexpected errors, but only
    the attempts with an HTTP response are in the latency population; the one never written is
    unsent."""
    rows = spike_rows()
    rows[6:6] = [
        (110 * SECOND, 100 * US, 30 * SECOND, 0, "timeout", True),
        (111 * SECOND, None, 0, 0, "transport_error", True),
    ]
    spike = spike_report(tmp_path, "20261007-120020-spk-1", rows)
    assert (spike["window_attempts"], spike["window_unexpected"]) == (6, 3)
    assert spike["window_latency_ns"] == {"count": 4, "p50": 2 * MS, "p95": 20 * MS, "p99": 20 * MS}
    assert (spike["window_planned"], spike["window_unsent"]) == (6, 1)
    assert spike["met"] is False
    assert spike["problems"] == [
        "window unexpected-error rate above the post-spike step's threshold"
    ]
