"""report/build.py (IMPLEMENTATION_PLAN H5 steps 2 and 3). Expected values are hand-computed
from the lines each test writes, or from the golden run-log lines (DESIGN §C5)."""

import json
import tracemalloc
from pathlib import Path

import pytest

from report import InputError
from report import runfiles as rf
from report.__main__ import main
from report.build import _baseline_run, build
from report.synthetic import (
    SECOND,
    T0_WALL_NS,
    RunSpec,
    attempt,
    entry,
    hitgen_stderr,
    lifecycle,
    lifecycle_start,
    request,
    sample,
    summary,
    write_run,
)

GOLDEN = Path(__file__).resolve().parents[1].joinpath("harness", "runlog", "testdata", "golden")
MS = 1_000_000


def golden_lines(name: str) -> list[str]:
    """A golden file's lines, read (never written: AGENTS.md §5)."""
    return GOLDEN.joinpath(name).read_text(encoding="utf-8").splitlines(keepends=True)


def quiet_api() -> list[dict[str, object]]:
    """An instance's run log with no request: start, ready, stop."""
    return [lifecycle_start(), lifecycle("ready"), lifecycle("stop")]


def section(report: dict[str, object], *path: str) -> dict[str, object]:
    value: object = report
    for key in path:
        assert isinstance(value, dict), path
        value = value[key]
    assert isinstance(value, dict), path
    return value


def test_unexpected_errors(tmp_path: Path) -> None:
    """§B9: ERR-15 and 5xx inside a fault window [start_ns, end_ns + P] are excluded, in its + P
    tail too and when only the start of their span is inside; transport errors (timeouts and
    connection failures, written or not) are included; retries are not in the denominator; a
    step's window leaves out its first 5 s. The window's latency population is attempt 1 with an
    HTTP response: the retry, the written transport failures and the line before the window,
    each slower than any response in it, stay out."""
    pause = {
        "at_s": 10,
        "cmd": "faultctl store pause 4.5s",
        "instance": None,
        "start_ns": T0_WALL_NS + 10 * SECOND,
        "end_ns": T0_WALL_NS + 14_500 * MS,
        "outage_start_ns": T0_WALL_NS + 10 * SECOND,
        "outage_ns": 4_600 * MS,
        "targets": None,
    }
    spec = RunSpec(name="unexpected", instances=1, steps=[(1.0, 20.0)], faults=[pause])
    # (planned_ns, status, error, lag_ns, latency): sent lag_ns after planned_ns (None: never
    # written), answered latency later. With P = 5 s the fault window is [10 s, 19.5 s] after t0.
    plan: list[tuple[int, int, str | None, int | None, int]] = [
        (1_000 * MS, 503, "database_unavailable", 100_000, 100 * MS),  # 0: before the window
        (6_000 * MS, 200, None, 100_000, 3 * MS),  # 1: success
        (6_500 * MS, 500, "internal_error", 100_000, 1 * MS),  # 2: 5xx ending before the fault
        (7_000 * MS, 429, "rate_limited", 100_000, 4 * MS),  # 3: limited (a retry follows)
        (8_000 * MS, 503, "service_aborted_after_acquire", 100_000, 2 * MS),  # 4: ERR-15
        (9_998_900_000, 503, "limiter_outcome_unknown", 100_000, 6 * MS),  # 5: [9.999, 10.005] s
        (12_000 * MS, 503, "limiter_unavailable", 100_000, 5 * MS),  # 6: inside the fault window
        (13_000 * MS, 0, "transport_error", 100_000, 50 * MS),  # 7: transport, written
        (15_000 * MS, 0, "timeout", 100_000, 60 * MS),  # 8: timeout, written
        (16_000 * MS, 503, "limiter_unavailable", 100_000, 8 * MS),  # 9: in the + P tail
        (17_000 * MS, 0, "transport_error", None, 0),  # 10: never written
        (19_495 * MS, 503, "database_unavailable", 100_000, 9 * MS),  # 11: [19.4951, 19.5041] s
        (19_600 * MS, 503, "database_unavailable", 100_000, 7 * MS),  # 12: after the window
    ]
    schedule = [entry(i, planned) for i, (planned, _, _, _, _) in enumerate(plan)]
    gen: list[dict[str, object]] = [
        attempt(i, planned, status=status, error=error, lag_ns=lag, latency=latency)
        for i, (planned, status, error, lag, latency) in enumerate(plan)
    ]
    gen.append(
        attempt(
            3,
            7_000 * MS,
            attempt_no=2,
            sent_ns=8_200 * MS,
            status=503,
            error="database_unavailable",
            latency=90 * MS,
        )
    )
    gen.append(summary(13, unsent=1))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    report = build(run_dir)
    unexpected = section(report, "results", "unexpected_errors")
    assert unexpected["fault_windows"] == [
        {
            "start_ns": T0_WALL_NS + 10 * SECOND,
            "end_ns": T0_WALL_NS + 19_500 * MS,
            "source": "faultctl store pause 4.5s",
        }
    ]
    # Attempt 1 only: 13 lines. Unexpected: transports 7, 8, 10 and 5xx 0, 2, 12. Excluded: the
    # ERR-15 line 4, and 5, 6, 9, 11 in the fault window (5 and 11 by the start of their span).
    assert unexpected["total"] == {
        "attempts": 13,
        "unexpected": 6,
        "rate": 6 / 13,
        "transport_errors": 3,
        "server_errors": 8,
        "excluded_service_aborted_after_acquire": 1,
        "excluded_fault_window": 4,
    }
    # The window [5 s, 20 s) leaves out line 0.
    steps = unexpected["steps"]
    assert isinstance(steps, list)
    assert steps[0] == {
        "step": 0,
        "window_start_ns": 5 * SECOND,
        "window_end_ns": 20 * SECOND,
        "attempts": 12,
        "unexpected": 5,
        "rate": 5 / 12,
        "transport_errors": 3,
        "server_errors": 7,
        "excluded_service_aborted_after_acquire": 1,
        "excluded_fault_window": 4,
    }
    # The window's population: the responses of lines 1-6, 9, 11 and 12, 1 ms to 9 ms; nearest
    # ranks 5 (p50) and 9 (p95, p99) of 9.
    timing_steps = section(report, "timing")["steps"]
    assert timing_steps == [
        {
            "step": 0,
            "window_start_ns": 5 * SECOND,
            "window_end_ns": 20 * SECOND,
            "latency_ns": {"count": 9, "p50": 5 * MS, "p95": 9 * MS, "p99": 9 * MS},
        }
    ]
    load = section(report, "load", "total")
    assert (load["planned"], load["sent"], load["unsent"], load["retries_429"]) == (13, 12, 1, 1)


def test_two_step_windows(tmp_path: Path) -> None:
    """§B9 per step: each attempt joins the step its planned_ns falls in, and each step's window
    leaves out that step's own first 5 s (step 1's window is [25 s, 40 s))."""
    spec = RunSpec(name="windows", instances=1, steps=[(1.0, 20.0), (1.0, 20.0)])
    # (planned_ns, status, error, latency); every attempt is sent 0.1 ms after planned_ns.
    plan: list[tuple[int, int, str | None, int]] = [
        (2 * SECOND, 500, "internal_error", 11 * MS),  # 0: step 0, before its window
        (6 * SECOND, 200, None, 1 * MS),  # 1: step 0's window
        (8 * SECOND, 503, "database_unavailable", 2 * MS),  # 2: step 0's window, unexpected
        (21 * SECOND, 503, "limiter_unavailable", 12 * MS),  # 3: step 1, before its window
        (26 * SECOND, 200, None, 3 * MS),  # 4: step 1's window
        (30 * SECOND, 0, "transport_error", 13 * MS),  # 5: step 1's window, transport
        (31 * SECOND, 503, "database_unavailable", 4 * MS),  # 6: step 1's window, unexpected
        (33 * SECOND, 429, "rate_limited", 5 * MS),  # 7: step 1's window
    ]
    schedule = [
        entry(i, planned, step=planned // (20 * SECOND))
        for i, (planned, _, _, _) in enumerate(plan)
    ]
    gen: list[dict[str, object]] = [
        attempt(i, planned, status=status, error=error, latency=latency)
        for i, (planned, status, error, latency) in enumerate(plan)
    ]
    gen.append(summary(len(plan)))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    report = build(run_dir)
    # Step 0: responses 1 ms, 2 ms (ranks 1, 2, 2 of 2); step 1: 3 ms, 4 ms, 5 ms (ranks 2, 3, 3).
    assert section(report, "timing")["steps"] == [
        {
            "step": 0,
            "window_start_ns": 5 * SECOND,
            "window_end_ns": 20 * SECOND,
            "latency_ns": {"count": 2, "p50": 1 * MS, "p95": 2 * MS, "p99": 2 * MS},
        },
        {
            "step": 1,
            "window_start_ns": 25 * SECOND,
            "window_end_ns": 40 * SECOND,
            "latency_ns": {"count": 3, "p50": 4 * MS, "p95": 5 * MS, "p99": 5 * MS},
        },
    ]
    unexpected = section(report, "results", "unexpected_errors")
    assert unexpected["steps"] == [
        {
            "step": 0,
            "window_start_ns": 5 * SECOND,
            "window_end_ns": 20 * SECOND,
            "attempts": 2,
            "unexpected": 1,
            "rate": 0.5,
            "transport_errors": 0,
            "server_errors": 1,
            "excluded_service_aborted_after_acquire": 0,
            "excluded_fault_window": 0,
        },
        {
            "step": 1,
            "window_start_ns": 25 * SECOND,
            "window_end_ns": 40 * SECOND,
            "attempts": 4,
            "unexpected": 2,
            "rate": 0.5,
            "transport_errors": 1,
            "server_errors": 1,
            "excluded_service_aborted_after_acquire": 0,
            "excluded_fault_window": 0,
        },
    ]
    # Over the whole run: lines 0, 2, 3, 6 (5xx) and 5 (transport) of 8.
    assert unexpected["total"] == {
        "attempts": 8,
        "unexpected": 5,
        "rate": 5 / 8,
        "transport_errors": 1,
        "server_errors": 4,
        "excluded_service_aborted_after_acquire": 0,
        "excluded_fault_window": 0,
    }


def test_reads_golden_runlog(tmp_path: Path) -> None:
    """The golden api.jsonl (as api-1.jsonl), gen.jsonl and samples.jsonl, with the other files
    the builder needs, build with no input error."""
    spec = RunSpec(name="golden", run_kind="test", instances=3, steps=[(100.0, 10.0)])
    schedule = [
        entry(0, 12 * MS, user="u000003", instance=1),
        entry(1, 19 * MS, user="u000000", instance=2),
        entry(2, 27 * MS, user="u000001", instance=3),
    ]
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=golden_lines("gen.jsonl"),
        apis={1: golden_lines("api.jsonl"), 2: quiet_api(), 3: quiet_api()},
        samples=golden_lines("samples.jsonl"),
    )
    report = build(run_dir)
    results = section(report, "results")
    # One request: allowed, then 503 database_unavailable (ERR-14).
    assert results["requests"] == 1
    by_err = section(report, "results", "by_err")
    assert by_err["ERR-14"] == 1
    assert sum(v for v in by_err.values() if isinstance(v, int)) == 1
    allowed = section(report, "timing", "limiter_ns", "allowed")
    assert (allowed["count"], allowed["p50"]) == (1, 612_345)
    assert results["refunds"] == {
        "applied": 1,
        "already_applied": 0,
        "receipt_expired": 0,
        "receipt_invalid": 0,
        "unresolved": 0,
    }
    # The refund's release to final: 8,418,950,122 − 8,411,236,450 ns.
    assert section(report, "timing", "refund_ns", "applied")["p50"] == 7_713_672
    load = section(report, "load", "total")
    assert (load["initial"], load["retries_429"], load["sent"], load["unsent"]) == (3, 1, 3, 0)
    # Lags 184,000, 210,000 and 160,000 ns: p50 rank 2, p99 rank 3.
    lag = section(report, "load", "total", "sched_lag_ns")
    assert (lag["p50"], lag["p99"]) == (184_000, 210_000)
    assert section(report, "results", "transport_errors")["total"] == 1
    # End to end: attempt 1 with a response (200 and 429; the transport error has none).
    e2e = section(report, "timing", "end_to_end_ns")
    assert section(e2e, "all")["count"] == 2
    assert section(e2e, "retries")["p50"] == 3_120_000
    resources = section(report, "resources")
    assert resources["samples"] == 2
    # hitapi-1's CPU 12.5 % and 9.75 %: mean 11.125, a tie rounded half to even to 11.12; RSS
    # 31,457,280 and 31,719,424 bytes: mean 31,588,352. Postgres is unread in both samples.
    assert resources["processes"] == {
        "hitapi-1": {
            "cpu_pct_mean": 11.12,
            "cpu_pct_max": 12.5,
            "rss_bytes_mean": 31_588_352,
            "rss_bytes_max": 31_719_424,
            "cpu_readings": 2,
            "rss_readings": 2,
        }
    }
    # One /health read (the second sample's failed), in_flight 3.
    assert resources["max_in_flight"] == 3
    assert section(report, "context")["label"] == "test-build scenario, not a benchmark"
    assert section(report, "correctness")["verdict"] == "pass"
    assert section(report, "evidence_validity")["valid"] is True
    traffic = section(report, "traffic")
    assert traffic["instances_used"] == 3
    assert traffic["schedule_sha256_matches"] is True
    # Three users with one first attempt each; the tie goes to the first user ID, u000000, sent
    # to instance 2 (its retry is not a first attempt).
    assert traffic["busiest_user"] == {"user": "u000000", "first_attempts": 1, "instances": [2]}


def test_cli_writes_report(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    """python3 -m report runs/<id> writes report.json and report.md and prints where."""
    spec = RunSpec(name="cli", instances=1, steps=[(1.0, 10.0)])
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[attempt(0, 6 * SECOND), summary(1)],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    assert main([str(run_dir)]) == 0
    printed = json.loads(capsys.readouterr().out)
    assert printed == {"report": str(run_dir.joinpath("report.json")), "valid": True}
    report = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    assert list(report) == [
        "run_id",
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
    ]
    markdown = run_dir.joinpath("report.md").read_text(encoding="utf-8")
    assert "## Evidence validity (§B8)" in markdown
    assert "| field | value |" in markdown


def test_cli_marks_invalid(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    """python3 -m report --mark-invalid runs/<id> (plan H5 step 7, a report validate.py did not
    pass): report.json gets evidence_validity.valid false and evidence_validity.validate naming
    the FAIL lines' checks in runs/<id>/validate.stderr, every other member as it was; report.md
    is rendered again from it, so it says valid false too. Without validate.stderr the output is
    null and no check is named; a report.json without evidence_validity is refused (exit 1, both
    files left as they were); a wrong command line exits 2."""
    spec = RunSpec(name="mark", instances=1, steps=[(1.0, 10.0)])
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[attempt(0, 6 * SECOND), summary(1)],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    assert main([str(run_dir)]) == 0
    capsys.readouterr()
    built = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    assert "| valid | true |" in run_dir.joinpath("report.md").read_text(encoding="utf-8")
    run_dir.joinpath("validate.stderr").write_text(
        "FAIL thresholds_hash: a, b\nFAIL evidence_validity.log_drops: 1\n"
        "FAIL thresholds_hash: again\nvalidate: x: invalid (full mode)\n",
        encoding="utf-8",
    )
    assert main(["--mark-invalid", str(run_dir)]) == 0
    printed = json.loads(capsys.readouterr().out)
    assert printed == {"report": str(run_dir.joinpath("report.json")), "valid": False}
    marked = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    evidence = built["evidence_validity"]
    assert isinstance(evidence, dict)
    want = {
        **evidence,
        "valid": False,
        "validate": {
            "passed": False,
            "failed_checks": ["thresholds_hash", "evidence_validity.log_drops"],
            "output": "validate.stderr",
        },
    }
    assert marked == {**built, "evidence_validity": want}
    assert list(marked) == list(built)
    markdown = run_dir.joinpath("report.md").read_text(encoding="utf-8")
    assert "| valid | false |" in markdown and "| valid | true |" not in markdown
    assert '["thresholds_hash","evidence_validity.log_drops"]' in markdown
    assert "| validate.output | validate.stderr |" in markdown

    run_dir.joinpath("validate.stderr").unlink()
    assert main(["--mark-invalid", str(run_dir)]) == 0
    capsys.readouterr()
    marked = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    assert marked["evidence_validity"]["validate"] == {
        "passed": False,
        "failed_checks": [],
        "output": None,
    }

    del marked["evidence_validity"]
    run_dir.joinpath("report.json").write_text(json.dumps(marked), encoding="utf-8")
    before = run_dir.joinpath("report.md").read_bytes()
    assert main(["--mark-invalid", str(run_dir)]) == 1
    assert "evidence_validity" in capsys.readouterr().err
    assert json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8")) == marked
    assert run_dir.joinpath("report.md").read_bytes() == before
    for argv in (["--mark-invalid"], ["--bogus", str(run_dir)], [str(run_dir), "--mark-invalid"]):
        assert main(argv) == 2, argv


def test_cli_marks_freeze_invalid(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    """python3 -m report --mark-invalid --freeze runs/<id> (plan H5 step 7, a valid baseline whose
    threshold freeze failed): report.json gets evidence_validity.valid false and
    evidence_validity.freeze naming runs/<id>/thresholds.stderr (null without it), no validate
    record, every other member as it was; report.md is rendered again from it. A wrong command
    line exits 2."""
    spec = RunSpec(name="markfreeze", instances=1, steps=[(1.0, 10.0)])
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[attempt(0, 6 * SECOND), summary(1)],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    assert main([str(run_dir)]) == 0
    capsys.readouterr()
    built = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    run_dir.joinpath("validate.stderr").write_text("validate: x: valid (baseline mode)\n")
    run_dir.joinpath("thresholds.stderr").write_text("thresholds: refused\n")
    assert main(["--mark-invalid", "--freeze", str(run_dir)]) == 0
    printed = json.loads(capsys.readouterr().out)
    assert printed == {"report": str(run_dir.joinpath("report.json")), "valid": False}
    marked = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    evidence = built["evidence_validity"]
    assert isinstance(evidence, dict)
    want = {**evidence, "valid": False, "freeze": {"passed": False, "output": "thresholds.stderr"}}
    assert marked == {**built, "evidence_validity": want}
    markdown = run_dir.joinpath("report.md").read_text(encoding="utf-8")
    assert "| valid | false |" in markdown and "| valid | true |" not in markdown
    assert "| freeze.output | thresholds.stderr |" in markdown

    run_dir.joinpath("thresholds.stderr").unlink()
    assert main(["--mark-invalid", "--freeze", str(run_dir)]) == 0
    capsys.readouterr()
    marked = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    assert marked["evidence_validity"]["freeze"] == {"passed": False, "output": None}
    for argv in (
        ["--freeze", str(run_dir)],
        ["--mark-invalid", str(run_dir), "--freeze"],
        ["--freeze", "--mark-invalid", str(run_dir)],
        ["--mark-invalid", "--freeze"],
    ):
        assert main(argv) == 2, argv


def test_input_error_names_line_and_field(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    """A gen.jsonl line without its latency is an input error naming the line and the field."""
    spec = RunSpec(name="broken", instances=1, steps=[(1.0, 10.0)])
    line = attempt(0, 6 * SECOND)
    del line["latency"]
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[line, summary(1)],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    with pytest.raises(InputError, match=r"gen\.jsonl: line 1: field latency: missing"):
        build(run_dir)
    assert main([str(run_dir)]) == 1
    assert "gen.jsonl: line 1: field latency" in capsys.readouterr().err
    assert not run_dir.joinpath("report.json").exists()


def base_run(
    tmp_path: Path,
    spec: RunSpec,
    *,
    gen_tail: list[dict[str, object]] | None = None,
    apis: dict[int, list[dict[str, object]]] | None = None,
    samples: list[dict[str, object]] | None = None,
) -> Path:
    """A one-step, one-instance run with two answered requests (6 s and 7 s after t0)."""
    gen: list[dict[str, object]] = [attempt(0, 6 * SECOND), attempt(1, 7 * SECOND)]
    gen.extend([summary(2)] if gen_tail is None else gen_tail)
    return write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND), entry(1, 7 * SECOND)],
        gen=gen,
        apis=apis if apis is not None else {1: quiet_api()},
        samples=samples if samples is not None else [sample(T0_WALL_NS + SECOND)],
    )


def store_fault(cmd: str, at_s: int, outage_ns: int | None) -> dict[str, object]:
    return {
        "at_s": at_s,
        "cmd": cmd,
        "instance": None,
        "start_ns": T0_WALL_NS + at_s * SECOND,
        "end_ns": T0_WALL_NS + (at_s + 5) * SECOND,
        "outage_start_ns": None if outage_ns is None else T0_WALL_NS + at_s * SECOND,
        "outage_ns": outage_ns,
        "targets": None,
    }


REFUSE_BODY = '{"target":"store","mode":"refuse","match":"acquire","duration_ms":4000}'


def api_sample_of(line: dict[str, object], instance: int) -> dict[str, object]:
    """A samples.jsonl line's read of one instance (its apis entry)."""
    apis = line["apis"]
    assert isinstance(apis, list)
    for read in apis:
        assert isinstance(read, dict)
        if read["instance"] == instance:
            return read
    raise AssertionError(f"no instance {instance} in the sample")


def health_of(line: dict[str, object], instance: int) -> dict[str, object]:
    """A samples.jsonl line's /health reading of one instance."""
    health = api_sample_of(line, instance)["health"]
    assert isinstance(health, dict)
    return health


def unread_pre_kill() -> dict[str, object]:
    """A pre_kill line whose read failed (read_ok false, HAR-13)."""
    return {
        "kind": "lifecycle",
        "event": "pre_kill",
        "listener_drops": 0,
        "log_drops": 0,
        "read_ok": False,
    }


# The defects check_bud11 also judges invalid evidence (HAR-13; plan H5 step 8): drops, an
# unread pre_kill and a process segment with neither stop nor pre_kill fail correctness too.
BUD11_INVALID = "correctness"


@pytest.mark.parametrize(
    ("case", "failing"),
    [
        ("valid", ()),
        ("segment opened by a ready line", ()),
        ("listener drop", ("listener_drops", BUD11_INVALID)),
        ("pre-kill listener drop", ("listener_drops", BUD11_INVALID)),
        ("pre-kill unread", ("listener_drops", BUD11_INVALID)),
        ("process without its drop count", ("listener_drops", BUD11_INVALID)),
        ("log ends without a drop count", ("listener_drops", BUD11_INVALID)),
        ("ready line only", ("listener_drops", BUD11_INVALID)),
        ("empty instance log", ("listener_drops", BUD11_INVALID)),
        ("log drop", ("log_drops", BUD11_INVALID)),
        ("stop log drop", ("log_drops", BUD11_INVALID)),
        ("outage over 5 s", ("outage",)),
        ("outage not measured", ("outage",)),
        ("two outage segments", ("outage_segments",)),
        ("summary missing", ("gen_summary",)),
        ("summary log drop", ("gen_summary",)),
        ("no rewrite window", ("rewrite_windows",)),
        ("fixtures on in a benchmark", ("fixtures_off",)),
        ("benchmark on the controlled clock", ("fixtures_off",)),
        ("benchmark without a health read", ("fixtures_off",)),
        ("benchmark with a fault", ("fixtures_off",)),
        ("release without its final event", (BUD11_INVALID,)),
    ],
)
def test_evidence_validity(tmp_path: Path, case: str, failing: tuple[str, ...]) -> None:
    """Each §B8 evidence check fails on its own defect, and only then is the run invalid. HAR-13:
    drops count from every stop and pre_kill line, and every process segment (from a start line,
    or from the file's first line) ends with one, an empty log included; and the correctness
    check fails whenever check_bud11's verdict is invalid (incomplete refund evidence), naming
    its invalid_reasons: on those defects, and on a release with no final event in a segment
    that ended with stop, which no other check sees."""
    spec = RunSpec(name="evidence", instances=1, steps=[(1.0, 20.0)])
    api: list[dict[str, object]] = quiet_api()
    gen_tail: list[dict[str, object]] | None = None
    samples = [sample(T0_WALL_NS + SECOND)]
    if case == "listener drop":
        api = [lifecycle_start(), lifecycle("ready"), lifecycle("stop", listener_drops=1)]
    elif case == "pre-kill listener drop":
        api = [lifecycle_start(), lifecycle("pre_kill", listener_drops=1), lifecycle_start()]
        api.append(lifecycle("stop"))
    elif case == "pre-kill unread":
        api = [lifecycle_start(), unread_pre_kill(), lifecycle_start(), lifecycle("stop")]
    elif case == "process without its drop count":
        api = [lifecycle_start(), lifecycle("ready"), lifecycle_start(), lifecycle("stop")]
    elif case == "log ends without a drop count":
        api = [lifecycle_start(), lifecycle("ready")]
    elif case == "segment opened by a ready line":
        # No start line: the ready line opens the file's first segment and the pre_kill line
        # ends it; a record the killed process wrote after it still belongs to it.
        late = request(1, "r000000001", arrival_ns=T0_WALL_NS + 7 * SECOND)
        api = [lifecycle("ready"), lifecycle("pre_kill"), late]
    elif case == "ready line only":
        api = [lifecycle("ready")]
    elif case == "empty instance log":
        api = []
    elif case == "log drop":
        api = [lifecycle_start(), lifecycle("pre_kill", log_drops=2), lifecycle_start()]
        api.append(lifecycle("stop"))
    elif case == "stop log drop":
        api = [lifecycle_start(), lifecycle("stop", log_drops=1)]
    elif case == "outage over 5 s":
        spec.faults = [store_fault("faultctl store down 4s", 2, 5_000_000_001)]
    elif case == "outage not measured":
        spec.faults = [store_fault("faultctl store pause 4.5s", 2, None)]
    elif case == "two outage segments":
        spec.faults = [
            store_fault("faultctl store pause 4.5s", 2, 4_600 * MS),
            store_fault(REFUSE_BODY, 10, None),
        ]
    elif case == "summary missing":
        gen_tail = []
    elif case == "summary log drop":
        gen_tail = [summary(2, log_drops=1)]
    elif case == "no rewrite window":
        spec.forced_aof_rewrites_s = [10.0]
    elif case == "fixtures on in a benchmark":
        spec.run_kind = "bench"
        samples = [sample(T0_WALL_NS + SECOND), sample(T0_WALL_NS + 2 * SECOND)]
        health_of(samples[1], 1)["fixtures"] = True
    elif case == "benchmark on the controlled clock":
        spec.run_kind = "bench"
        samples = [sample(T0_WALL_NS + SECOND), sample(T0_WALL_NS + 2 * SECOND)]
        health_of(samples[1], 1)["clock_source"] = "controlled"
    elif case == "benchmark without a health read":
        spec.run_kind = "bench"
        samples = [sample(T0_WALL_NS + SECOND), sample(T0_WALL_NS + 2 * SECOND)]
        for line in samples:
            read = api_sample_of(line, 1)
            read["health"], read["health_error"] = None, "GET /health: context deadline exceeded"
    elif case == "benchmark with a fault":
        spec.run_kind = "bench"
        spec.faults = [store_fault("faultctl store pause 4.5s", 2, 4_600 * MS)]
    elif case == "release without its final event":
        # Deduction d0's release has no refund line, and its process ended with stop.
        api = [lifecycle_start(), lifecycle("ready"), *refund_records([("applied", "", MS)])[:2]]
        api.append(lifecycle("stop"))
    run_dir = base_run(tmp_path, spec, gen_tail=gen_tail, apis={1: api}, samples=samples)
    report = build(run_dir)
    evidence = section(report, "evidence_validity")
    checks = section(evidence, "checks")
    assert len(checks) == 8
    for name, check in checks.items():
        assert isinstance(check, dict)
        assert check["ok"] is (name not in failing), (case, name, check)
    assert evidence["valid"] is (not failing)
    correctness = section(report, "correctness")
    assert section(checks, BUD11_INVALID)["value"] == {
        "verdict": correctness["verdict"],
        "invalid_reasons": correctness["invalid_reasons"],
    }
    assert (correctness["verdict"] == "invalid") is (BUD11_INVALID in failing)
    if case == "release without its final event":
        assert correctness["invalid_reasons"] == [
            "api-1.jsonl: line 4: release without final event or segment pre_kill"
        ]


def test_benchmark_fixtures_and_outage_pass(tmp_path: Path) -> None:
    """A benchmark run whose /health reads show fixtures off on the store clock passes
    fixtures_off; one measured outage of exactly 5 s, the run's one segment, passes."""
    spec = RunSpec(name="benchok", instances=1, steps=[(1.0, 20.0)], run_kind="bench")
    run_dir = base_run(tmp_path, spec)
    assert section(build(run_dir), "evidence_validity")["valid"] is True
    spec = RunSpec(name="outage5", instances=1, steps=[(1.0, 20.0)])
    spec.faults = [store_fault("faultctl store down 4s", 2, 5 * SECOND)]
    run_dir = base_run(tmp_path, spec)
    checks = section(build(run_dir), "evidence_validity", "checks")
    assert section(checks, "outage")["ok"] is True
    assert section(checks, "outage_segments")["value"] == 1


def test_rewrite_windows(tmp_path: Path) -> None:
    """TRF-S7: rewrite windows are context.json's rewrites plus each run of samples with
    aof_rewrite_in_progress:1; the p99 is over attempt-1 responses overlapping one."""
    rewrite: dict[str, object] = {
        "at_s": 15,
        "start_ns": T0_WALL_NS + 15 * SECOND,
        "end_ns": T0_WALL_NS + 16 * SECOND,
    }
    # A forced rewrite that failed (its window would hold the 12 s response) and one that never
    # ran: neither is a rewrite window.
    failed: dict[str, object] = {
        "at_s": 11.9,
        "start_ns": T0_WALL_NS + 11_900 * MS,
        "end_ns": T0_WALL_NS + 12_100 * MS,
        "status": "error",
        "error": "ERR Background append only file rewriting already in progress",
    }
    never: dict[str, object] = {"at_s": 18, "start_ns": 0, "end_ns": 0}
    spec = RunSpec(
        name="rewrites",
        instances=1,
        steps=[(1.0, 20.0)],
        forced_aof_rewrites_s=[11.9, 15.0, 18.0],
        rewrites=[failed, rewrite, never],
    )
    # Samples at 9 s (0), 10 s (1), 11 s (1), 12 s (store unread), 13 s (1): two runs,
    # [10 s, 11 s] and [13 s, 13 s].
    samples = [
        sample(T0_WALL_NS + 9 * SECOND),
        sample(T0_WALL_NS + 10 * SECOND, rewriting=True),
        sample(T0_WALL_NS + 11 * SECOND, rewriting=True),
        sample(T0_WALL_NS + 12 * SECOND, rewriting=None),
        sample(T0_WALL_NS + 13 * SECOND, rewriting=True),
    ]
    # (planned, sent, latency, status): sent at 10.5 s for 7 ms (inside), 9.990 s for 9 ms
    # (ends at 9.999 s: outside), 12 s for 1 ms (outside), 15.9995 s for 2 ms (overlaps the
    # context window), 10.6 s with no response (not in the population), 13 s for 3 ms (inside).
    # Planned at 10.9 s, inside [10 s, 11 s], but written at 11.001 s for 1 ms: its span, from its
    # write to its response, is after the window (outside; judged from planned_ns it would be in).
    plan = [
        (9_989 * MS, 9_990 * MS, 9 * MS, 200),
        (10_499 * MS, 10_500 * MS, 7 * MS, 200),
        (10_599 * MS, 10_600 * MS, 1 * MS, 0),
        (10_900 * MS, 11_001 * MS, 1 * MS, 200),
        (11_999 * MS, 12_000 * MS, 1 * MS, 200),
        (12_999 * MS, 13_000 * MS, 3 * MS, 429),
        (15_999 * MS, 15_999_500_000, 2 * MS, 200),
    ]
    gen: list[dict[str, object]] = [
        attempt(
            i,
            planned,
            sent_ns=sent,
            latency=latency,
            status=status,
            error="transport_error" if status == 0 else "rate_limited" if status == 429 else None,
        )
        for i, (planned, sent, latency, status) in enumerate(plan)
    ]
    gen.append(summary(len(plan)))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(i, planned) for i, (planned, _, _, _) in enumerate(plan)],
        gen=gen,
        apis={1: quiet_api()},
        samples=samples,
    )
    report = build(run_dir)
    timing = section(report, "timing")
    assert timing["rewrite_windows"] == [
        {
            "start_ns": T0_WALL_NS + 15 * SECOND,
            "end_ns": T0_WALL_NS + 16 * SECOND,
            "source": "context",
        },
        {
            "start_ns": T0_WALL_NS + 10 * SECOND,
            "end_ns": T0_WALL_NS + 11 * SECOND,
            "source": "samples",
        },
        {
            "start_ns": T0_WALL_NS + 13 * SECOND,
            "end_ns": T0_WALL_NS + 13 * SECOND,
            "source": "samples",
        },
    ]
    # Inside: 7 ms, 3 ms, 2 ms → sorted [2, 3, 7] ms; p99 rank 3.
    assert section(timing, "rewrite_latency_ns") == {
        "count": 3,
        "p50": 3 * MS,
        "p95": 7 * MS,
        "p99": 7 * MS,
    }
    assert timing["rewrite_p99_ns"] == 7 * MS
    assert section(report, "evidence_validity", "checks", "rewrite_windows")["ok"] is True


def test_stopped_early_steps(tmp_path: Path) -> None:
    """stop_early: planned counts only the entries planned before the stop (the summary's
    planned); excluded_step (2 here, not stopped_after_step + 1) is partial, the steps between
    ran in full, the ones after not at all."""
    spec = RunSpec(name="stopped", instances=1, steps=[(1.0, 10.0)] * 4)
    planned = [2 * SECOND, 12 * SECOND, 22 * SECOND, 28 * SECOND, 32 * SECOND]
    schedule = [entry(i, ns, step=ns // (10 * SECOND)) for i, ns in enumerate(planned)]
    gen: list[dict[str, object]] = [attempt(i, ns) for i, ns in enumerate(planned[:3])]
    gen.append(summary(3, stopped_after_step=0, excluded_step=2))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    load = section(build(run_dir), "load")
    steps = load["steps"]
    assert isinstance(steps, list)
    got = [(s["ran"], s["scheduled"], s["planned"], s["sent"], s["unsent"]) for s in steps]
    assert got == [
        ("full", 1, 1, 1, 0),
        ("full", 1, 1, 1, 0),
        ("partial", 2, 1, 1, 0),
        ("none", 1, 0, 0, 0),
    ]
    assert (load["stopped_after_step"], load["excluded_step"]) == (0, 2)


def test_generator_limited(tmp_path: Path) -> None:
    """§B9: a step whose p99 of sent_ns − planned_ns, over every planned first attempt with an
    unwritten one above any bound, exceeds 5 ms is generator-limited; the p99 is the nearest
    rank, ⌈0.99 n⌉."""
    spec = RunSpec(name="lag", instances=1, steps=[(100.0, 10.0)] * 4 + [(5.1, 10.0)])
    schedule: list[dict[str, object]] = []
    gen: list[dict[str, object]] = []
    # 100 entries a step. Step 0: one 10 ms lag (rank 99 of 100 is still 1 ms). Step 1: two
    # (rank 99 is 10 ms). Step 2: one never written (rank 99 is the 99th written, 1 ms). Step 3:
    # two never written (rank 99 is past the 98 written). Step 4: 51 entries, one 10 ms lag:
    # rank ⌈50.49⌉ = 51 is 10 ms (rounding 50.49 half up would give rank 50, 1 ms).
    entries = {0: 100, 1: 100, 2: 100, 3: 100, 4: 51}
    slow = {0: 1, 1: 2, 2: 0, 3: 0, 4: 1}
    unwritten = {0: 0, 1: 0, 2: 1, 3: 2, 4: 0}
    i = 0
    for k in range(5):
        for j in range(entries[k]):
            ns = k * 10 * SECOND + j * 50 * MS
            schedule.append(entry(i, ns, step=k))
            if j < unwritten[k]:
                gen.append(attempt(i, ns, lag_ns=None, status=0, error="transport_error"))
            else:
                late = j >= entries[k] - slow[k]
                gen.append(attempt(i, ns, lag_ns=10 * MS if late else 1 * MS))
            i += 1
    gen.append(summary(451, unsent=3))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    steps = section(build(run_dir), "load")["steps"]
    assert isinstance(steps, list)
    assert [s["generator_limited"] for s in steps] == [False, True, False, True, True]
    assert [s["sched_lag_ns"]["p99"] for s in steps] == [1 * MS, 10 * MS, 1 * MS, 1 * MS, 10 * MS]


def test_baseline_comparison(tmp_path: Path) -> None:
    """A benchmark limiter run compares each step's window p99 and unexpected-error rate with
    the baseline run of its scenario and seed that runs/index.jsonl names."""
    schedule = [entry(0, 6 * SECOND), entry(1, 7 * SECOND)]
    base = RunSpec(name="cmp", instances=1, steps=[(1.0, 20.0)], run_kind="bench", baseline=True)
    base.run_id = "20261007-110000-cmp-1"
    base_dir = write_run(
        tmp_path,
        base,
        schedule=schedule,
        gen=[
            attempt(0, 6 * SECOND, latency=2 * MS),
            attempt(1, 7 * SECOND, latency=4 * MS),
            summary(2),
        ],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    assert main([str(base_dir)]) == 0
    limiter = RunSpec(name="cmp", instances=1, steps=[(1.0, 20.0)], run_kind="bench")
    limiter.run_id = "20261007-120000-cmp-1"
    gen: list[dict[str, object]] = [
        attempt(0, 6 * SECOND, latency=5 * MS),
        attempt(1, 7 * SECOND, latency=9 * MS, status=503, error="database_unavailable"),
        summary(2),
    ]
    run_dir = write_run(
        tmp_path,
        limiter,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    index = tmp_path.joinpath("runs", "index.jsonl")
    # The last baseline line of scenario cmp, seed 1, before this run's own line: another seed's
    # baseline after it, and a baseline after this run's line, are not it.
    lines = [
        {"scenario": "cmp", "baseline": True, "run_id": base.id()},
        {"scenario": "cmp", "baseline": True, "run_id": "20261007-113000-cmp-2"},
        {"scenario": "other", "baseline": True, "run_id": "20261007-114000-other-1"},
        {"scenario": "cmp", "baseline": False, "run_id": limiter.id()},
        {"scenario": "cmp", "baseline": True, "run_id": "20261007-130000-cmp-1"},
    ]
    index.write_text("".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")
    baseline = section(build(run_dir), "baseline")
    assert baseline["run_id"] == base.id()
    assert baseline["same_schedule"] is True
    assert baseline["problem"] is None
    # Baseline window p99 4 ms, rate 0/2; limiter 9 ms, rate 1/2.
    assert baseline["steps"] == [
        {
            "step": 0,
            "p99_ns": 9 * MS,
            "baseline_p99_ns": 4 * MS,
            "p99_delta_ns": 5 * MS,
            "unexpected_rate": 0.5,
            "baseline_unexpected_rate": 0.0,
            "unexpected_rate_delta": 0.5,
            "baseline_ran": "full",
            "baseline_step_exists": True,
            "baseline_generator_limited": False,
        }
    ]


def test_duplicates_counted_apart(tmp_path: Path) -> None:
    """TRF-6: a duplicate entry's first attempt counts as a duplicate, not as an initial, per
    step and in total; its retries count with the 429 retries."""
    spec = RunSpec(name="dups", instances=1, steps=[(1.0, 10.0), (1.0, 10.0)])
    # (planned_ns, user, kind, duplicate_of): a duplicate repeats its original's user and request.
    plan: list[tuple[int, str, str, int | None]] = [
        (2 * SECOND, "u000001", "initial", None),
        (4 * SECOND, "u000002", "initial", None),
        (6 * SECOND, "u000001", "duplicate", 0),
        (12 * SECOND, "u000003", "initial", None),
        (14 * SECOND, "u000003", "duplicate", 3),
        (16 * SECOND, "u000002", "duplicate", 1),
    ]
    schedule: list[dict[str, object]] = []
    gen: list[dict[str, object]] = []
    for i, (planned, user, kind, of) in enumerate(plan):
        rid = f"r{i if of is None else of:09d}"
        step = planned // (10 * SECOND)
        schedule.append(
            entry(i, planned, step=step, user=user, kind=kind, duplicate_of=of, request_id=rid)
        )
        limited = i == 4
        gen.append(
            attempt(
                i,
                planned,
                user=user,
                kind=kind,
                duplicate_of=of,
                request_id=rid,
                status=429 if limited else 200,
                error="rate_limited" if limited else None,
            )
        )
    # Entry 4's retry (a retry line names its entry's duplicate_of too).
    gen.append(
        attempt(
            4,
            14 * SECOND,
            user="u000003",
            duplicate_of=3,
            request_id="r000000003",
            attempt_no=2,
            sent_ns=15 * SECOND,
        )
    )
    gen.append(summary(len(plan)))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )
    report = build(run_dir)
    load = section(report, "load")
    steps = load["steps"]
    assert isinstance(steps, list)
    got = [(s["planned"], s["initial"], s["duplicates"], s["retries_429"]) for s in steps]
    assert got == [(3, 2, 1, 0), (3, 1, 2, 1)]
    total = section(load, "total")
    assert (total["initial"], total["duplicates"], total["retries_429"]) == (3, 3, 1)
    assert section(report, "results", "attempt_outcomes") == {
        "initial": {"200": 3},
        "duplicate": {"200": 2, "429 rate_limited": 1},
        "retry": {"200": 1},
    }


def test_capacity_step_fields(tmp_path: Path) -> None:
    """The per-step fields the capacity verdict reads (plan H5 steps 2 and 4): generator_limited
    when the step's p99 lag is above 5 ms (5 ms is not), unsent and the achieved sent rate,
    got_conn_wait_ns p99, rows created by step (ERR-1, by arrival), and hitgen's retry and
    generator drops."""
    spec = RunSpec(name="capfields", instances=1, steps=[(10.0, 10.0)] * 3)
    schedule: list[dict[str, object]] = []
    gen: list[dict[str, object]] = []
    # 100 entries a step, 100 ms apart. Lags are 1 ms but for the two last entries of steps 0
    # and 1: the 99th smallest (rank ⌈99⌉) is 5 ms in step 0 and 5 ms + 1 ns in step 1, the
    # largest 10 ms. Step 2's first entry is never written. got_conn waits: step 0 1 µs … 100 µs
    # (rank 99: 99 µs), step 1 200 µs … 2 µs (rank 99: 198 µs), step 2 40 µs.
    rank99 = {0: 5 * MS, 1: 5 * MS + 1}
    for k in range(3):
        for j in range(100):
            i = 100 * k + j
            ns = k * 10 * SECOND + j * 100 * MS
            schedule.append(entry(i, ns, step=k))
            if k == 2 and j == 0:
                gen.append(attempt(i, ns, lag_ns=None, status=0, error="transport_error"))
                continue
            lag = 1 * MS
            if k < 2 and j == 98:
                lag = rank99[k]
            elif k < 2 and j == 99:
                lag = 10 * MS
            got_conn = {0: (j + 1) * 1_000, 1: (100 - j) * 2_000, 2: 40_000}[k]
            gen.append(attempt(i, ns, lag_ns=lag, got_conn_ns=got_conn))
    gen.append(summary(300, unsent=1))
    # Rows created (ERR-1) arriving in steps 0 and 1; an existing row (ERR-2) is not one.
    api: list[dict[str, object]] = [lifecycle_start(), lifecycle("ready")]
    for n, (at_s, existed) in enumerate([(3, False), (4, False), (12, True), (13, False)]):
        api.append(request(1, f"q{n}", arrival_ns=T0_WALL_NS + at_s * SECOND, existed=existed))
    api.append(lifecycle("stop"))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: api},
        samples=[sample(T0_WALL_NS + SECOND)],
        stderr=hitgen_stderr(300, retry_drops=3, generator_drops=5),
    )
    report = build(run_dir)
    load = section(report, "load")
    steps = load["steps"]
    assert isinstance(steps, list)
    assert [s["sched_lag_ns"]["p99"] for s in steps] == [5 * MS, 5 * MS + 1, 1 * MS]
    assert [s["generator_limited"] for s in steps] == [False, True, False]
    assert [s["unsent"] for s in steps] == [0, 0, 1]
    assert [s["sent"] for s in steps] == [100, 100, 99]
    assert [s["offered_per_s"] for s in steps] == [10.0, 10.0, 10.0]
    assert [s["achieved_sent_per_s"] for s in steps] == [10.0, 10.0, 9.9]
    assert [s["got_conn_wait_ns_p99"] for s in steps] == [99_000, 198_000, 40_000]
    assert (load["retry_drops"], load["generator_drops"]) == (3, 5)
    results_steps = section(report, "results")["steps"]
    assert results_steps == [
        {"step": 0, "requests": 2, "new_rows": 2, "allowed": 2, "limited": 0},
        {"step": 1, "requests": 2, "new_rows": 1, "allowed": 2, "limited": 0},
        {"step": 2, "requests": 0, "new_rows": 0, "allowed": 0, "limited": 0},
    ]


# HARNESS §3, the ERR catalog: (row, HTTP status, error, acquire result, existed).
ERR_CATALOG: list[tuple[str, int, str, str, bool]] = [
    ("ERR-1", 200, "", "allowed", False),
    ("ERR-2", 200, "", "allowed", True),
    ("ERR-3", 200, "", "degraded", False),
    ("ERR-4", 429, "rate_limited", "limited", False),
    ("ERR-5", 400, "invalid_request", "", False),
    ("ERR-6", 404, "not_found", "", False),
    ("ERR-6", 405, "method_not_allowed", "", False),
    ("ERR-7", 503, "service_overloaded", "busy", False),
    ("ERR-8", 503, "limiter_unavailable", "unavailable", False),
    ("ERR-9", 503, "limiter_outcome_unknown", "outcome_unknown", False),
    ("ERR-9a", 503, "limiter_deadline_exceeded", "cancelled", False),
    ("ERR-10", 503, "limiter_capacity_exceeded", "capacity_error", False),
    ("ERR-11", 503, "limiter_state_integrity_error", "state_integrity_error", False),
    ("ERR-12", 503, "limiter_config_error", "config_error", False),
    ("ERR-12a", 503, "limiter_not_ready", "not_ready", False),
    ("ERR-12b", 503, "limiter_clock_error", "clock_error", False),
    ("ERR-12c", 503, "limiter_receipt_error", "receipt_error", False),
    ("ERR-12d", 400, "invalid_request", "validation_error", False),
    ("ERR-13", 500, "internal_error", "allowed", False),
    ("ERR-14", 503, "database_unavailable", "allowed", False),
    ("ERR-15", 503, "service_aborted_after_acquire", "allowed", False),
    ("ERR-16", 503, "application_outcome_unknown", "allowed", False),
]
# Records per row, all different so a swap of two rows shows: ERR-6's 6 are 3 × 404, 3 × 405.
ROW_COUNTS = {
    "ERR-1": 1,
    "ERR-2": 2,
    "ERR-3": 3,
    "ERR-4": 4,
    "ERR-5": 5,
    "ERR-6": 6,
    "ERR-7": 7,
    "ERR-8": 8,
    "ERR-9": 9,
    "ERR-9a": 10,
    "ERR-10": 11,
    "ERR-11": 12,
    "ERR-12": 13,
    "ERR-12a": 14,
    "ERR-12b": 15,
    "ERR-12c": 16,
    "ERR-12d": 17,
    "ERR-13": 18,
    "ERR-14": 19,
    "ERR-15": 20,
    "ERR-16": 21,
}
# limiter_ns by the decision-time class of the acquire result (PRF-3), and db_ns by row where
# the database work ran.
CLASS_NS = {"allowed": 100_000, "limited": 200_000, "degraded": 300_000}
ERROR_CLASS_NS = 400_000
DB_NS = {
    "ERR-1": 1 * MS,
    "ERR-2": 2 * MS,
    "ERR-3": 3 * MS,
    "ERR-13": 4 * MS,
    "ERR-14": 5 * MS,
    "ERR-16": 6 * MS,
}


def refund_records(results: list[tuple[str, str, int]]) -> list[dict[str, object]]:
    """A decision, a release and a refund per (result, reason, release-to-final ns), each a
    deduction of user u000009 1 ms after the last, credited 1 ms after its decision when known."""
    lines: list[dict[str, object]] = []
    for n, (result, reason, final_ns) in enumerate(results):
        deduction = f"d{n}"
        decided = T0_WALL_NS + (n + 1) * MS
        released = 9 * SECOND + n * SECOND
        known = result in ("applied", "already_applied")
        lines.append(
            {
                "kind": "decision",
                "deduction_id": deduction,
                "user_id": "u000009",
                "decision_ns": decided,
                "result": "allowed",
            }
        )
        lines.append({"kind": "release", "deduction_id": deduction, "release_mono_ns": released})
        lines.append(
            {
                "kind": "refund",
                "deduction_id": deduction,
                "result": result,
                "reason": reason,
                "attempts": 1,
                "credit_decision_ns": decided + 1 * MS if known else 0,
                "release_mono_ns": released,
                "final_mono_ns": released + final_ns,
            }
        )
    return lines


def test_results_by_row_and_class(tmp_path: Path) -> None:
    """PRF-2 and PRF-3 by row and class: a request record per HARNESS §3 ERR row (by_err and the
    PRF-2 names), one refund per result kind, the final db_rows, limiter_ns per decision-time
    class, db_ns, and gen.jsonl's attempt outcomes, transport errors and end-to-end times per
    class."""
    spec = RunSpec(name="rows", instances=1, steps=[(1.0, 20.0)])
    api: list[dict[str, object]] = [lifecycle_start(), lifecycle("ready")]
    n = 0
    for row, status, error, result, existed in ERR_CATALOG:
        copies = ROW_COUNTS[row] // 2 if row == "ERR-6" else ROW_COUNTS[row]
        for _ in range(copies):
            limiter_ns = CLASS_NS.get(result, ERROR_CLASS_NS if result else 0)
            api.append(
                request(
                    1,
                    f"q{n}",
                    arrival_ns=T0_WALL_NS + 6 * SECOND + n * MS,
                    status=status,
                    error=error,
                    result=result,
                    existed=existed,
                    limiter_ns=limiter_ns,
                    db_ns=DB_NS.get(row, 0),
                )
            )
            n += 1
    api.extend(
        refund_records(
            [
                ("applied", "", 7 * MS),
                ("already_applied", "", 8 * MS),
                ("receipt_expired", "", 9 * MS),
                ("receipt_invalid", "", 10 * MS),
                ("unresolved", "closed", 11 * MS),
            ]
        )
    )
    api.append(lifecycle("stop"))
    # gen.jsonl, attempt 1: successes in 1 ms and 2 ms, 429s in 3 ms and 8 ms (each retried),
    # errors (503, 500) in 5 ms and 6 ms, a timeout after 50 ms; the first retry answered in
    # 4 ms, the second failing before its write.
    plan: list[tuple[int, int, str | None, int]] = [
        (6 * SECOND, 200, None, 1 * MS),
        (7 * SECOND, 200, None, 2 * MS),
        (8 * SECOND, 429, "rate_limited", 3 * MS),
        (10 * SECOND, 503, "database_unavailable", 5 * MS),
        (11 * SECOND, 500, "internal_error", 6 * MS),
        (12 * SECOND, 0, "timeout", 50 * MS),
        (13 * SECOND, 429, "rate_limited", 8 * MS),
    ]
    gen: list[dict[str, object]] = [
        attempt(i, planned, status=status, error=error, latency=latency)
        for i, (planned, status, error, latency) in enumerate(plan)
    ]
    gen.append(attempt(2, 8 * SECOND, attempt_no=2, sent_ns=9 * SECOND, latency=4 * MS))
    gen.append(
        attempt(6, 13 * SECOND, attempt_no=2, lag_ns=None, status=0, error="transport_error")
    )
    gen.append(summary(len(plan)))
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(i, planned) for i, (planned, _, _, _) in enumerate(plan)],
        gen=gen,
        apis={1: api},
        samples=[sample(T0_WALL_NS + SECOND)],
        db_rows=7,
    )
    report = build(run_dir)
    results = section(report, "results")
    assert results["requests"] == 231
    assert results["by_err"] == ROW_COUNTS
    assert results["unmatched"] == {}
    # PRF-2's names by the rows HARNESS §3 gives them: new rows are ERR-1 and ERR-3 (its row "as
    # ERR-1/2", existed false here), 1 + 3; capacity refusals are ERR-7 (busy, the in-flight cap)
    # and ERR-10 (capacity_error), 7 + 11; server failures ERR-13, -14 and -15, 18 + 19 + 20.
    assert results["prf2"] == {
        "new_rows": 4,
        "duplicate_rows_avoided": 2,
        "degraded_admissions": 3,
        "quota_rejections": 4,
        "capacity_refusals": 18,
        "integrity_failures": 12,
        "unavailable": 8,
        "unknown_acquires": 9,
        "unknown_application_outcomes": 21,
        "server_failures": 57,
        "transport_errors": 2,
        "expired_refunds": 1,
        "unresolved_refunds": 1,
        "db_rows": 7,
    }
    assert results["db_rows"] == 7
    assert results["refunds"] == {
        "applied": 1,
        "already_applied": 1,
        "receipt_expired": 1,
        "receipt_invalid": 1,
        "unresolved": 1,
    }
    assert results["unresolved_reasons"] == {"closed": 1}
    assert results["decisions"] == {"allowed": 5, "degraded": 0}
    assert results["releases"] == 5
    assert results["attempt_outcomes"] == {
        "initial": {
            "0 timeout": 1,
            "200": 2,
            "429 rate_limited": 2,
            "500 internal_error": 1,
            "503 database_unavailable": 1,
        },
        "duplicate": {},
        "retry": {"0 transport_error": 1, "200": 1},
    }
    assert results["transport_errors"] == {
        "total": 2,
        "by_kind": {"initial": {"timeout": 1}, "retry": {"transport_error": 1}},
    }
    timing = section(report, "timing")
    # allowed: ERR-1, -2, -13, -14, -15, -16 (1 + 2 + 18 + 19 + 20 + 21 = 81); limited: ERR-4;
    # degraded: ERR-3; error: ERR-7 … ERR-12d (7 + 8 + … + 17 = 132); ERR-5 and ERR-6 never
    # reached the limiter (no result).
    assert timing["limiter_ns"] == {
        "allowed": {"count": 81, "p50": 100_000, "p95": 100_000, "p99": 100_000},
        "limited": {"count": 4, "p50": 200_000, "p95": 200_000, "p99": 200_000},
        "degraded": {"count": 3, "p50": 300_000, "p95": 300_000, "p99": 300_000},
        "error": {"count": 132, "p50": 400_000, "p95": 400_000, "p99": 400_000},
    }
    # db_ns: 1 × 1 ms, 2 × 2 ms, 3 × 3 ms, 18 × 4 ms, 19 × 5 ms, 21 × 6 ms (64; cumulative 1, 3,
    # 6, 24, 43, 64): ranks 32 (5 ms), 61 and 64 (6 ms).
    assert timing["db_ns"] == {"count": 64, "p50": 5 * MS, "p95": 6 * MS, "p99": 6 * MS}
    # Refund release to final: 7 … 11 ms by kind; all five: ranks 3, 5, 5.
    refund_ns = section(timing, "refund_ns")
    assert refund_ns["all"] == {"count": 5, "p50": 9 * MS, "p95": 11 * MS, "p99": 11 * MS}
    for kind, ms in (
        ("applied", 7),
        ("already_applied", 8),
        ("receipt_expired", 9),
        ("receipt_invalid", 10),
        ("unresolved", 11),
    ):
        assert refund_ns[kind] == {"count": 1, "p50": ms * MS, "p95": ms * MS, "p99": ms * MS}
    # End to end, attempt 1 with a response: [1, 2, 3, 5, 6, 8] ms (ranks 3, 6, 6); success
    # [1, 2]; limited [3, 8]; error [5, 6]; the answered retry 4 ms.
    assert timing["end_to_end_ns"] == {
        "all": {"count": 6, "p50": 3 * MS, "p95": 8 * MS, "p99": 8 * MS},
        "success": {"count": 2, "p50": 1 * MS, "p95": 2 * MS, "p99": 2 * MS},
        "limited": {"count": 2, "p50": 3 * MS, "p95": 8 * MS, "p99": 8 * MS},
        "error": {"count": 2, "p50": 5 * MS, "p95": 6 * MS, "p99": 6 * MS},
        "retries": {"count": 1, "p50": 4 * MS, "p95": 4 * MS, "p99": 4 * MS},
    }


def test_resources_and_traffic(tmp_path: Path) -> None:
    """PRF-4: a process's CPU and RSS mean and max over its readings, and max_in_flight the
    largest /health in_flight of any instance at any reading; TRF-4: every instance the busiest
    user's first attempts went to."""
    spec = RunSpec(name="resources", instances=2, steps=[(1.0, 20.0)])
    # u000001 sends to instances 1, 2, then 1 again; u000002 to instance 2.
    plan = [(6, "u000001", 1), (7, "u000001", 2), (8, "u000002", 2), (9, "u000001", 1)]
    schedule = [
        entry(i, at_s * SECOND, user=user, instance=instance)
        for i, (at_s, user, instance) in enumerate(plan)
    ]
    gen: list[dict[str, object]] = [
        attempt(i, at_s * SECOND, user=user, instance=instance)
        for i, (at_s, user, instance) in enumerate(plan)
    ]
    gen.append(summary(len(plan)))
    # valkey-server CPU 5.5, 6.25, 7.0 % and RSS 16, 32, 48 MiB; in_flight 3, 7, 5 on
    # instance 1 and 1, 4, 2 on instance 2.
    readings = [(5.5, 16, 3, 1), (6.25, 32, 7, 4), (7.0, 48, 5, 2)]
    samples: list[dict[str, object]] = []
    for t, (cpu, rss_mib, first, second) in enumerate(readings, 1):
        line = sample(T0_WALL_NS + t * SECOND, instances=2)
        procs = line["procs"]
        assert isinstance(procs, list) and isinstance(procs[0], dict)
        procs[0]["cpu"], procs[0]["rss"] = cpu, rss_mib << 20
        health_of(line, 1)["in_flight"] = first
        health_of(line, 2)["in_flight"] = second
        samples.append(line)
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api(), 2: quiet_api()},
        samples=samples,
    )
    report = build(run_dir)
    resources = section(report, "resources")
    assert resources["max_in_flight"] == 7
    rows = resources["instance_series"]
    assert isinstance(rows, list)
    in_flight = [r for r in rows if isinstance(r, dict) and r["series"] == "in_flight"]
    assert in_flight == [
        {"instance": 1, "series": "in_flight", "max": 7, "last": 5, "readings": 3},
        {"instance": 2, "series": "in_flight", "max": 4, "last": 2, "readings": 3},
    ]
    # CPU mean (5.5 + 6.25 + 7.0) / 3 = 6.25; RSS mean 32 MiB.
    assert section(resources, "processes", "valkey-server") == {
        "cpu_pct_mean": 6.25,
        "cpu_pct_max": 7.0,
        "rss_bytes_mean": 32 << 20,
        "rss_bytes_max": 48 << 20,
        "cpu_readings": 3,
        "rss_readings": 3,
    }
    traffic = section(report, "traffic")
    assert traffic["busiest_user"] == {"user": "u000001", "first_attempts": 3, "instances": [1, 2]}
    assert traffic["requests_per_instance"] == {"1": 2, "2": 2}


@pytest.mark.parametrize(
    ("time_text", "offset_ns"),
    [
        # 05:00 at UTC−7 is 12:00 UTC; 9 fractional digits.
        ("2026-10-07T05:00:00.123456789-07:00", 123_456_789),
        # One fractional digit is 5 tenths of a second.
        ("2026-10-07T12:00:00.5Z", 500_000_000),
        # 17:30 at UTC+5:30 is 12:00 UTC; 2 fractional digits.
        ("2026-10-07T17:30:00.25+05:30", 250_000_000),
    ],
)
def test_hitgen_start_time(tmp_path: Path, time_text: str, offset_ns: int) -> None:
    """hitgen's t0 on the wall clock is its sending line's RFC 3339 time (slog writes the local
    zone's offset and up to 9 fractional digits), exactly."""
    spec = RunSpec(name="t0", instances=1, steps=[(1.0, 10.0)])
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[attempt(0, 6 * SECOND), summary(1)],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
        stderr=hitgen_stderr(1, time_text=time_text),
    )
    assert section(build(run_dir), "clocks")["t0_wall_ns"] == T0_WALL_NS + offset_ns


def test_rewrite_p99_null_unforced(tmp_path: Path) -> None:
    """TRF-S7: without forced_aof_rewrites_s the rewrite p99 is null, though rewrite windows are
    still listed and a response overlaps one."""
    rewrite: dict[str, object] = {
        "at_s": 10,
        "start_ns": T0_WALL_NS + 10 * SECOND,
        "end_ns": T0_WALL_NS + 11 * SECOND,
    }
    spec = RunSpec(name="unforced", instances=1, steps=[(1.0, 20.0)], rewrites=[rewrite])
    run_dir = write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 10_500 * MS)],
        gen=[attempt(0, 10_500 * MS), summary(1)],
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + 12 * SECOND, rewriting=True)],
    )
    report = build(run_dir)
    timing = section(report, "timing")
    assert timing["rewrite_windows"] == [
        {
            "start_ns": T0_WALL_NS + 10 * SECOND,
            "end_ns": T0_WALL_NS + 11 * SECOND,
            "source": "context",
        },
        {
            "start_ns": T0_WALL_NS + 12 * SECOND,
            "end_ns": T0_WALL_NS + 12 * SECOND,
            "source": "samples",
        },
    ]
    assert timing["rewrite_p99_ns"] is None
    assert timing["rewrite_latency_ns"] is None
    check = section(report, "evidence_validity", "checks", "rewrite_windows")
    assert check["ok"] is True
    assert check["value"] == {"applies": False, "windows": 2}


def bench_run(
    root: Path,
    run_id: str,
    steps: list[tuple[float, float]],
    schedule: list[dict[str, object]],
    gen: list[dict[str, object]],
    *,
    baseline: bool = False,
) -> Path:
    """A one-instance benchmark run of scenario cmp, seed 1 (a baseline with the limiter off)."""
    spec = RunSpec(name="cmp", instances=1, steps=steps, run_kind="bench", baseline=baseline)
    spec.run_id = run_id
    return write_run(
        root,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: quiet_api()},
        samples=[sample(T0_WALL_NS + SECOND)],
    )


def write_index(root: Path, lines: list[dict[str, object]]) -> None:
    index = root.joinpath("runs", "index.jsonl")
    index.write_text("".join(json.dumps(line) + "\n" for line in lines), encoding="utf-8")


def test_baseline_step_exists(tmp_path: Path) -> None:
    """A baseline step exists for the comparison only when it ran in full with planned entries:
    a stop_early baseline's excluded step ran in part (some of its entries planned), the steps
    after it not at all, and a step with no entry planned has nothing to compare (§B9 "Missing
    baseline step")."""
    steps = [(1.0, 10.0), (0.0, 10.0), (1.0, 10.0), (1.0, 10.0)]
    # Step 1 plans nothing. The baseline stopped after step 0 (its lags 10 ms) once its slowest
    # attempt ended, after 20.001 s: step 2's first entry was planned, its second not, so step 2
    # is the excluded step (not stopped_after_step + 1), and step 3 never ran.
    planned = [6 * SECOND, 7 * SECOND, 20_001 * MS, 25 * SECOND, 36 * SECOND]
    schedule = [entry(i, ns, step=ns // (10 * SECOND)) for i, ns in enumerate(planned)]
    base_gen: list[dict[str, object]] = [
        attempt(0, 6 * SECOND, lag_ns=10 * MS),
        attempt(1, 7 * SECOND, lag_ns=10 * MS, latency=13_500 * MS),
        attempt(2, 20_001 * MS),
        summary(3, stopped_after_step=0, excluded_step=2),
    ]
    base_dir = bench_run(
        tmp_path, "20261007-110000-cmp-1", steps, schedule, base_gen, baseline=True
    )
    assert main([str(base_dir)]) == 0
    gen: list[dict[str, object]] = [attempt(i, ns) for i, ns in enumerate(planned)]
    gen.append(summary(len(planned)))
    run_dir = bench_run(tmp_path, "20261007-120000-cmp-1", steps, schedule, gen)
    write_index(
        tmp_path, [{"scenario": "cmp", "baseline": True, "run_id": "20261007-110000-cmp-1"}]
    )
    baseline = section(build(run_dir), "baseline")
    assert baseline["problem"] is None
    assert baseline["same_schedule"] is True
    got = baseline["steps"]
    assert isinstance(got, list)
    # (baseline_ran, baseline_step_exists, baseline_generator_limited): step 0 ran in full with
    # 2 entries (lag p99 10 ms); step 1 in full with none; step 2 in part with 1; step 3 not.
    assert [
        (s["baseline_ran"], s["baseline_step_exists"], s["baseline_generator_limited"]) for s in got
    ] == [
        ("full", True, True),
        ("full", False, False),
        ("partial", False, False),
        ("none", False, False),
    ]


def test_baseline_other_schedule(tmp_path: Path) -> None:
    """A baseline whose schedule_sha256 differs from the run's is still found, and same_schedule
    is false (a comparison is valid only on the same schedule)."""
    steps = [(1.0, 20.0)]
    base_schedule = [entry(0, 6 * SECOND), entry(1, 7 * SECOND)]
    base_gen: list[dict[str, object]] = [
        attempt(0, 6 * SECOND),
        attempt(1, 7 * SECOND),
        summary(2),
    ]
    base_dir = bench_run(
        tmp_path, "20261007-110000-cmp-1", steps, base_schedule, base_gen, baseline=True
    )
    assert main([str(base_dir)]) == 0
    schedule = [entry(0, 6 * SECOND), entry(1, 8 * SECOND)]
    gen: list[dict[str, object]] = [attempt(0, 6 * SECOND), attempt(1, 8 * SECOND), summary(2)]
    run_dir = bench_run(tmp_path, "20261007-120000-cmp-1", steps, schedule, gen)
    write_index(
        tmp_path, [{"scenario": "cmp", "baseline": True, "run_id": "20261007-110000-cmp-1"}]
    )
    baseline = section(build(run_dir), "baseline")
    assert baseline["run_id"] == "20261007-110000-cmp-1"
    assert baseline["problem"] is None
    base_ctx = json.loads(base_dir.joinpath("context.json").read_text(encoding="utf-8"))
    assert baseline["schedule_sha256"] == base_ctx["schedule_sha256"]
    assert baseline["same_schedule"] is False


# A baseline line of runs/index.jsonl (the cmp scenario, seed 1) and a line cut short.
INDEX_BASELINE = {"scenario": "cmp", "baseline": True, "run_id": "20261007-110000-cmp-1"}
INDEX_BAD_LINE = '{"scenario":"cmp","baseline":tr\n'


def index_run(root: Path) -> Path:
    """A limiter run of the cmp scenario, seed 1, after INDEX_BASELINE's, in root/runs."""
    return bench_run(
        root,
        "20261007-120000-cmp-1",
        [(1.0, 20.0)],
        [entry(0, 6 * SECOND)],
        [attempt(0, 6 * SECOND), summary(1)],
    )


def test_index_read_lazily(tmp_path: Path) -> None:
    """runs/index.jsonl grows with every run and is never held whole (plan H5 Watch out): the
    baseline lookup reads it one line at a time and keeps only the baseline it selects. Over an
    entirely valid 20,000-line index its peak allocation stays below 1 MB, while the lines held
    as a list take about 4 MB, so a list built in read_index or in the lookup fails this
    assertion (the bad-line cases are test_index_bad_line_reached_lazily's)."""
    run_dir = index_run(tmp_path)
    own = {**INDEX_BASELINE, "baseline": False, "run_id": run_dir.name}
    with run_dir.parent.joinpath("index.jsonl").open("w", encoding="utf-8") as stream:
        stream.write(json.dumps(INDEX_BASELINE) + "\n")
        for i in range(20_000):  # other scenarios' runs, baselines and not
            other = {"scenario": "other", "baseline": i % 2 == 0, "run_id": f"20261007-{i:06d}-o-1"}
            stream.write(json.dumps(other) + "\n")
        stream.write(json.dumps(own) + "\n")
    ctx = rf.read_context(run_dir)
    tracemalloc.start()
    try:
        found = _baseline_run(ctx, run_dir.parent)
        _, peak = tracemalloc.get_traced_memory()
    finally:
        tracemalloc.stop()
    assert found == ("20261007-110000-cmp-1", None)
    assert peak < 1 << 20, peak


def test_index_bad_line_reached_lazily(tmp_path: Path) -> None:
    """A bad runs/index.jsonl line is reached only when it is read: read_index yields each line
    as it reads it (its first line comes before a bad second line raises InputError), and the
    baseline lookup reads no line after this run's own, so a bad one there is never reached."""
    index = tmp_path.joinpath("index.jsonl")
    index.write_text(json.dumps(INDEX_BASELINE) + "\n" + INDEX_BAD_LINE, encoding="utf-8")
    lines = rf.read_index(index)
    assert next(lines) == rf.IndexLine("cmp", True, "20261007-110000-cmp-1")
    with pytest.raises(InputError, match="line 2"):
        next(lines)

    run_dir = index_run(tmp_path)
    own = {**INDEX_BASELINE, "baseline": False, "run_id": run_dir.name}
    run_dir.parent.joinpath("index.jsonl").write_text(
        json.dumps(INDEX_BASELINE) + "\n" + json.dumps(own) + "\n" + INDEX_BAD_LINE,
        encoding="utf-8",
    )
    found = _baseline_run(rf.read_context(run_dir), run_dir.parent)
    assert found == ("20261007-110000-cmp-1", None)


def test_baseline_bad_index_line(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    """A bad runs/index.jsonl line (a short write for another scenario) is the baseline
    comparison's problem, not a failed report."""
    steps = [(1.0, 20.0)]
    schedule = [entry(0, 6 * SECOND)]
    run_dir = bench_run(
        tmp_path, "20261007-120000-cmp-1", steps, schedule, [attempt(0, 6 * SECOND), summary(1)]
    )
    good = {"scenario": "cmp", "baseline": True, "run_id": "20261007-110000-cmp-1"}
    tmp_path.joinpath("runs", "index.jsonl").write_text(
        '{"scenario":"other","baseline":tr\n' + json.dumps(good) + "\n", encoding="utf-8"
    )
    assert main([str(run_dir)]) == 0
    capsys.readouterr()
    report = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    baseline = report["baseline"]
    assert baseline["run_id"] is None
    assert baseline["steps"] == []
    assert baseline["problem"].startswith("the run index: index.jsonl: line 1: field json: ")
