"""report/thresholds.py (IMPLEMENTATION_PLAN H5 step 5; DESIGN §B9; HARNESS PRF-6, PRF-7). Expected
values are hand-computed from each synthetic baseline's lines and the §B9 table."""

import errno
import io
import json
import os
import stat
import time
from collections.abc import Callable
from pathlib import Path

import pytest

from report.__main__ import main as build_main
from report.synthetic import SECOND, T0_WALL_NS, Row, rows_run, summary, update_context
from report.thresholds import ThresholdsError, freeze
from report.thresholds import main as thresholds_main

MS = 1_000_000
CREATED_NS = T0_WALL_NS - 3600 * SECOND


def baseline_run(
    root: Path,
    steps: list[tuple[float, float]],
    plan: list[tuple[int, int, int, int, str | None]],
    *,
    name: str = "frz",
    verdict: str = "capacity",
    spike_step: int | None = None,
    planned: int | None = None,
    tail: dict[str, object] | None = None,
    baseline: bool = True,
) -> Path:
    """A one-instance benchmark baseline whose report.json is built. plan rows are (planned_ns,
    lag_ns, latency_ns, status, error); the first `planned` of them (all by default) were planned
    and have a gen.jsonl line; tail replaces the summary line."""
    count = len(plan) if planned is None else planned
    rows: list[Row] = [(*row, i < count) for i, row in enumerate(plan)]
    run_dir = rows_run(
        root,
        "",
        steps,
        rows,
        name=name,
        baseline=baseline,
        verdict=verdict,
        spike_step=spike_step,
        tail=tail if tail is not None else summary(count),
    )
    assert build_main([str(run_dir)]) == 0
    return run_dir


def read_doc(path: Path) -> dict[str, object]:
    doc = json.loads(path.read_text(encoding="utf-8"))
    assert isinstance(doc, dict)
    return doc


def test_definitions(tmp_path: Path) -> None:
    """§B9 from a synthetic baseline: per step, latency threshold = window p99 + 50 ms and error
    threshold = max(0.1 %, 2 × window unexpected-error rate); a step with p99 scheduling lag above
    5 ms (5 ms is not) is generator-limited and gets no thresholds; the first 5 s of each step
    are outside the window (a 900 ms response and a 500 there change nothing)."""
    steps = [(1.0, 10.0)] * 5
    plan: list[tuple[int, int, int, int, str | None]] = [
        # Step 0: window latencies 10, 30, 20 ms → p99 (rank ⌈2.97⌉ = 3) 30 ms; no error.
        (1 * SECOND, 100_000, 900 * MS, 200, None),
        (6 * SECOND, 100_000, 10 * MS, 200, None),
        (7 * SECOND, 100_000, 30 * MS, 200, None),
        (8 * SECOND, 100_000, 20 * MS, 200, None),
        # Step 1: window latencies 5, 6, 7, 40 ms (the 503 answered too) → p99 40 ms; 1 of 4
        # unexpected (rate 0.25 → threshold 0.5); the 500 at 11 s is before the window.
        (11 * SECOND, 100_000, 3 * MS, 500, "internal_error"),
        (15 * SECOND, 100_000, 5 * MS, 200, None),
        (16 * SECOND, 100_000, 6 * MS, 200, None),
        (17 * SECOND, 100_000, 7 * MS, 200, None),
        (18 * SECOND, 100_000, 40 * MS, 503, "database_unavailable"),
        # Step 2: lags 6 ms and 0.1 ms → p99 6 ms: generator-limited.
        (26 * SECOND, 6 * MS, 2 * MS, 200, None),
        (27 * SECOND, 100_000, 2 * MS, 200, None),
        # Step 3: lags 5 ms and 0.1 ms → p99 5 ms, not above 5 ms; window p99 4 ms.
        (36 * SECOND, 5 * MS, 4 * MS, 200, None),
        (37 * SECOND, 100_000, 1 * MS, 200, None),
        # Step 4: 2 of 3 unexpected: 2 × 2/3 is above 1, and a rate is at most 1 (hitgen takes
        # 0 to 1); window p99 5 ms.
        (45 * SECOND, 100_000, 3 * MS, 503, "database_unavailable"),
        (46 * SECOND, 100_000, 4 * MS, 503, "database_unavailable"),
        (47 * SECOND, 100_000, 5 * MS, 200, None),
    ]
    base_dir = baseline_run(tmp_path, steps, plan)
    out = tmp_path.joinpath("runs", "thresholds", "frz-1.json")
    before = time.time_ns()
    stderr = io.StringIO()
    assert thresholds_main([str(base_dir), "--out", str(out)], io.StringIO(), stderr) == 0, (
        stderr.getvalue()
    )
    after = time.time_ns()
    doc = read_doc(out)
    assert doc["steps"] == [
        {
            "step": 0,
            "generator_limited": False,
            "latency_threshold_ns": 80 * MS,
            "error_rate_threshold": 0.001,
            "baseline_p99_ns": 30 * MS,
            "baseline_latency_count": 3,
            "baseline_attempts": 3,
            "baseline_unexpected": 0,
        },
        {
            "step": 1,
            "generator_limited": False,
            "latency_threshold_ns": 90 * MS,
            "error_rate_threshold": 0.5,
            "baseline_p99_ns": 40 * MS,
            "baseline_latency_count": 4,
            "baseline_attempts": 4,
            "baseline_unexpected": 1,
        },
        {"step": 2, "generator_limited": True},
        {
            "step": 3,
            "generator_limited": False,
            "latency_threshold_ns": 54 * MS,
            "error_rate_threshold": 0.001,
            "baseline_p99_ns": 4 * MS,
            "baseline_latency_count": 2,
            "baseline_attempts": 2,
            "baseline_unexpected": 0,
        },
        {
            "step": 4,
            "generator_limited": False,
            "latency_threshold_ns": 55 * MS,
            "error_rate_threshold": 1.0,
            "baseline_p99_ns": 5 * MS,
            "baseline_latency_count": 3,
            "baseline_attempts": 3,
            "baseline_unexpected": 2,
        },
    ]
    assert doc["missing_steps"] == []
    assert doc["spike"] is None
    created = doc["created_ns"]
    assert isinstance(created, int) and before <= created <= after
    base_ctx = read_doc(base_dir.joinpath("context.json"))
    assert (doc["scenario"], doc["seed"], doc["baseline_run_id"]) == ("frz", 1, base_ctx["run_id"])
    assert doc["schedule_sha256"] == base_ctx["schedule_sha256"]
    definitions = doc["definitions"]
    assert isinstance(definitions, dict)
    assert definitions["allowed_lag_ns"] == 5 * MS
    assert definitions["latency_allowance_ns"] == 50 * MS
    assert definitions["error_rate_floor"] == 0.001
    assert definitions["observation_skip_ns"] == 5 * SECOND
    assert (definitions["spike_recovery_ns"], definitions["spike_window_ns"]) == (
        30 * SECOND,
        60 * SECOND,
    )


def test_missing_steps_and_spike(tmp_path: Path) -> None:
    """A step that did not run in full with planned entries, or has no latency population,
    gets no thresholds (§B9 "Missing baseline step"): one planning nothing, one answered only in
    its first 5 s or, in its window, only by a timeout and a transport error after the write (no
    HTTP response: neither is in the latency population), a stop_early baseline's partly run
    step and the step after it, which never ran.
    A spike scenario records its deadline (spike end + 30 s) and window ([deadline, deadline +
    60 s]) with them (PRF-6)."""
    steps = [(0.0, 10.0), (1.0, 10.0), (1.0, 10.0), (1.0, 100.0), (1.0, 10.0)]
    plan: list[tuple[int, int, int, int, str | None]] = [
        (11 * SECOND, 100_000, 2 * MS, 200, None),  # step 1: before its window
        (16 * SECOND, 100_000, 30 * SECOND, 0, "timeout"),  # step 1: a timeout in its window
        (17 * SECOND, 100_000, 5 * MS, 0, "transport_error"),  # step 1: a transport error too
        (26 * SECOND, 6 * MS, 2 * MS, 200, None),  # step 2: generator-limited, the stop
        (31 * SECOND, 100_000, 2 * MS, 200, None),  # step 3: planned before the stop
        (70 * SECOND, 100_000, 2 * MS, 200, None),  # step 3: after the stop, never planned
        (135 * SECOND, 100_000, 2 * MS, 200, None),  # step 4: never ran
    ]
    base_dir = baseline_run(
        tmp_path,
        steps,
        plan,
        name="spk",
        verdict="spike",
        spike_step=2,
        planned=5,
        tail=summary(5, stopped_after_step=2, excluded_step=3),
    )
    out = tmp_path.joinpath("runs", "thresholds", "spk-1.json")
    doc = freeze(base_dir, out, created_ns=CREATED_NS)
    assert doc["steps"] == [{"step": 2, "generator_limited": True}]
    assert doc["missing_steps"] == [
        {"step": 0, "reason": "did not run in full with planned entries (ran full, planned 0)"},
        {"step": 1, "reason": "no latency population in the observation window"},
        {"step": 3, "reason": "did not run in full with planned entries (ran partial, planned 1)"},
        {"step": 4, "reason": "did not run in full with planned entries (ran none, planned 0)"},
    ]
    assert doc["spike"] == {
        "spike_step": 2,
        "post_spike_step": 3,
        "spike_end_ns": 30 * SECOND,
        "deadline_ns": 60 * SECOND,
        "window_start_ns": 60 * SECOND,
        "window_end_ns": 120 * SECOND,
    }


def test_never_overwritten(tmp_path: Path) -> None:
    """The file is created once, mode 0444: a second freeze on it, or a freeze onto a name that
    exists already, fails and leaves its bytes unchanged, with no temp file left; an invalid
    baseline (a gen.jsonl summary with a log drop, a run the orchestrator marked failed, one
    missing a PRF-5 item, a report.json of another run), a run with the limiter on, or a
    baseline without its report writes nothing (PRF-7)."""
    plan: list[tuple[int, int, int, int, str | None]] = [
        (6 * SECOND, 100_000, 2 * MS, 200, None),
        (7 * SECOND, 100_000, 3 * MS, 200, None),
    ]
    base_dir = baseline_run(tmp_path, [(1.0, 20.0)], plan)
    out = tmp_path.joinpath("runs", "thresholds", "frz-1.json")
    stdout, stderr = io.StringIO(), io.StringIO()
    assert thresholds_main([str(base_dir), "--out", str(out)], stdout, stderr) == 0
    assert json.loads(stdout.getvalue())["thresholds"] == str(out)
    assert stat.S_IMODE(out.stat().st_mode) == 0o444
    first = out.read_bytes()
    assert read_doc(out)["steps"] == [
        {
            "step": 0,
            "generator_limited": False,
            "latency_threshold_ns": 53 * MS,
            "error_rate_threshold": 0.001,
            "baseline_p99_ns": 3 * MS,
            "baseline_latency_count": 2,
            "baseline_attempts": 2,
            "baseline_unexpected": 0,
        }
    ]
    stdout, stderr = io.StringIO(), io.StringIO()
    assert thresholds_main([str(base_dir), "--out", str(out)], stdout, stderr) == 1
    assert "exists" in stderr.getvalue()
    assert stdout.getvalue() == ""
    with pytest.raises(FileExistsError):
        freeze(base_dir, out, created_ns=CREATED_NS + 1)
    assert out.read_bytes() == first
    assert stat.S_IMODE(out.stat().st_mode) == 0o444
    # A name that exists already, not written by a freeze, is never touched either.
    other = out.parent.joinpath("other-1.json")
    other.write_bytes(b"made by hand\n")
    with pytest.raises(FileExistsError):
        freeze(base_dir, other, created_ns=CREATED_NS)
    assert other.read_bytes() == b"made by hand\n"
    assert sorted(p.name for p in out.parent.iterdir()) == ["frz-1.json", "other-1.json"]

    def nothing_written(run_dir: Path, name: str) -> None:
        target = tmp_path.joinpath("runs", "thresholds", name)
        err = io.StringIO()
        assert thresholds_main([str(run_dir), "--out", str(target)], io.StringIO(), err) == 1
        assert "nothing written" in err.getvalue()
        assert not target.exists()

    invalid = baseline_run(
        tmp_path.joinpath("invalid"), [(1.0, 20.0)], plan, tail=summary(2, log_drops=1)
    )
    nothing_written(invalid, "invalid-1.json")
    with pytest.raises(ThresholdsError, match=r"evidence_validity\.gen_summary"):
        freeze(invalid, tmp_path.joinpath("x.json"), created_ns=CREATED_NS)
    limiter = baseline_run(tmp_path.joinpath("limiter"), [(1.0, 20.0)], plan, baseline=False)
    nothing_written(limiter, "limiter-1.json")
    unbuilt = baseline_run(tmp_path.joinpath("unbuilt"), [(1.0, 20.0)], plan)
    unbuilt.joinpath("report.json").unlink()
    nothing_written(unbuilt, "unbuilt-1.json")
    # A baseline in context.json whose config.toml has the limiter on is no baseline.
    limiter_on = baseline_run(tmp_path.joinpath("limiter-on"), [(1.0, 20.0)], plan)
    config = limiter_on.joinpath("config.toml")
    text = config.read_text(encoding="utf-8")
    config.write_text(text.replace("limiter_enabled = false", "limiter_enabled = true"))
    nothing_written(limiter_on, "limiter-on-1.json")
    # A baseline answered only in its first 5 s gives no step thresholds (hitgen refuses none).
    early: list[tuple[int, int, int, int, str | None]] = [(SECOND, 100_000, 2 * MS, 200, None)]
    unusable = baseline_run(tmp_path.joinpath("unusable"), [(1.0, 20.0)], early)
    nothing_written(unusable, "unusable-1.json")
    # A baseline the orchestrator marked failed, or one missing a PRF-5 item, is not valid.
    failed = baseline_run(tmp_path.joinpath("failed"), [(1.0, 20.0)], plan)
    update_context(failed, outcome="failed")
    nothing_written(failed, "failed-1.json")
    no_pg = baseline_run(tmp_path.joinpath("no-pg"), [(1.0, 20.0)], plan)
    update_context(no_pg, postgres_settings=None)
    nothing_written(no_pg, "no-pg-1.json")
    # report.json of another run (scenario oth, 900 ms answers) copied into this baseline.
    slow: list[tuple[int, int, int, int, str | None]] = [(6 * SECOND, 100_000, 900 * MS, 200, None)]
    oth = baseline_run(tmp_path.joinpath("oth"), [(1.0, 20.0)], slow, name="oth")
    copied = baseline_run(tmp_path.joinpath("copied"), [(1.0, 20.0)], plan)
    copied.joinpath("report.json").write_bytes(oth.joinpath("report.json").read_bytes())
    nothing_written(copied, "copied-1.json")


class Interrupted(BaseException):
    """An interruption that is no OSError, as a KeyboardInterrupt is."""


def test_interrupted_freeze_leaves_nothing(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """A freeze interrupted while writing leaves neither the final name (which would block every
    later freeze) nor its temp file, and the next freeze writes the whole file."""
    plan: list[tuple[int, int, int, int, str | None]] = [(6 * SECOND, 100_000, 2 * MS, 200, None)]
    base_dir = baseline_run(tmp_path, [(1.0, 20.0)], plan)
    out = tmp_path.joinpath("runs", "thresholds", "frz-1.json")
    real_write = os.write

    def write_10_bytes(fd: int, data: bytes) -> int:
        real_write(fd, bytes(data[:10]))
        raise Interrupted

    with monkeypatch.context() as patch:
        patch.setattr(os, "write", write_10_bytes)
        with pytest.raises(Interrupted):
            freeze(base_dir, out, created_ns=CREATED_NS)
    assert list(out.parent.iterdir()) == []
    assert thresholds_main([str(base_dir), "--out", str(out)], io.StringIO(), io.StringIO()) == 0
    assert read_doc(out)["baseline_run_id"] == base_dir.name
    assert [p.name for p in out.parent.iterdir()] == ["frz-1.json"]


def _fsync_fails_on_directories(real_fsync: Callable[[int], None]) -> Callable[[int], None]:
    """os.fsync that raises EIO for a directory's descriptor and fsyncs anything else."""

    def fsync(fd: int) -> None:
        if stat.S_ISDIR(os.fstat(fd).st_mode):
            raise OSError(errno.EIO, "Input/output error")
        real_fsync(fd)

    return fsync


def _unlink_fails_on(
    real_unlink: Callable[..., None], match: Callable[[str], bool]
) -> Callable[..., None]:
    """os.unlink that raises EACCES for a name match accepts and unlinks anything else."""

    def unlink(path: str | os.PathLike[str], *args: object, **kwargs: object) -> None:
        if match(Path(path).name):
            raise PermissionError(errno.EACCES, "Permission denied", str(path))
        real_unlink(path, *args, **kwargs)

    return unlink


def _is_temp(name: str) -> bool:
    return name.startswith(".frz-1.json.") and name.endswith(".tmp")


@pytest.mark.parametrize("failing", ["directory fsync", "temp unlink"])
def test_failure_after_link_writes_nothing(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, failing: str
) -> None:
    """A failure after the final name was linked (the directory's fsync, which makes the name
    durable, or unlinking the temp file) removes the final name again, so main's "nothing
    written" (exit 1) is true; the next freeze writes the whole file. A temp file that could
    not be unlinked stays under its own name only."""
    plan: list[tuple[int, int, int, int, str | None]] = [(6 * SECOND, 100_000, 2 * MS, 200, None)]
    base_dir = baseline_run(tmp_path, [(1.0, 20.0)], plan)
    out = tmp_path.joinpath("runs", "thresholds", "frz-1.json")
    out.parent.mkdir(parents=True)
    err = io.StringIO()
    with monkeypatch.context() as patch:
        if failing == "directory fsync":
            patch.setattr(os, "fsync", _fsync_fails_on_directories(os.fsync))
        else:
            patch.setattr(os, "unlink", _unlink_fails_on(os.unlink, _is_temp))
        code = thresholds_main([str(base_dir), "--out", str(out)], io.StringIO(), err)
    assert code == 1
    assert err.getvalue().endswith("; nothing written\n"), err.getvalue()
    assert not out.exists()
    left = [p.name for p in out.parent.iterdir()]
    if failing == "directory fsync":
        assert left == []
    else:
        assert len(left) == 1 and _is_temp(left[0]), left
        out.parent.joinpath(left[0]).unlink()
    assert thresholds_main([str(base_dir), "--out", str(out)], io.StringIO(), io.StringIO()) == 0
    assert read_doc(out)["baseline_run_id"] == base_dir.name
    assert stat.S_IMODE(out.stat().st_mode) == 0o444


def test_failure_after_link_not_removed(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """When the directory's fsync fails after the link and removing the final name fails too,
    main says the file was written but not confirmed durable, with exit 3 (never "nothing
    written"), and the complete 0444 file is there."""
    plan: list[tuple[int, int, int, int, str | None]] = [(6 * SECOND, 100_000, 2 * MS, 200, None)]
    base_dir = baseline_run(tmp_path, [(1.0, 20.0)], plan)
    out = tmp_path.joinpath("runs", "thresholds", "frz-1.json")
    out.parent.mkdir(parents=True)
    err = io.StringIO()
    with monkeypatch.context() as patch:
        patch.setattr(os, "fsync", _fsync_fails_on_directories(os.fsync))
        patch.setattr(os, "unlink", _unlink_fails_on(os.unlink, lambda name: name == out.name))
        code = thresholds_main([str(base_dir), "--out", str(out)], io.StringIO(), err)
    assert code == 3
    message = err.getvalue()
    assert (
        message.startswith(f"thresholds: {out} was written, but ")
        and "remove it by hand" in message
    )
    assert "nothing written" not in message
    assert [p.name for p in out.parent.iterdir()] == ["frz-1.json"]
    assert stat.S_IMODE(out.stat().st_mode) == 0o444
    assert read_doc(out)["baseline_run_id"] == base_dir.name
