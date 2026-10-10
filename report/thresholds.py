"""`python3 -m report.thresholds runs/<id> --out runs/thresholds/<scenario>-<seed>.json`: freezes a
baseline run's thresholds (IMPLEMENTATION_PLAN H5 step 5; DESIGN §B9; HARNESS PRF-6, PRF-7).

Thresholds come only from a valid baseline: a run with the limiter off that passes
report/validate.py in baseline mode, whose report.json (built by `python3 -m report`) gives each
step's §B9 measures. Per step: latency threshold = the step's window p99 + 50 ms; error threshold
= max(0.1 %, 2 × the step's window unexpected-error rate); a generator-limited step (its p99
scheduling lag above 5 ms) is marked so and gets none; a step that did not run in full with
planned entries, or has no latency population, is left out (§B9 "Missing baseline step"). A
spike scenario also records its recovery deadline and window (PRF-6).

The file is created once, mode 0444, and never overwritten or edited (PRF-7): its bytes go to a
temp file made with O_CREAT|O_EXCL in the same directory, which is then hard-linked to the final
name (atomic, and refused when the name exists), so the final name never holds a partial file.
An invalid baseline, or a file that exists, writes nothing. A failure after the link (removing
the temp file, or the directory's fsync, which makes the new name durable) removes the final name
again before the error is reported, so "nothing written" stays true; only when that removal fails
too is the file left, reported with a message of its own (exit 3).

hitgen reads "steps" (cmd/hitgen loadThresholds: "step" and either "generator_limited": true or
"latency_threshold_ns" and "error_rate_threshold"; other keys ignored); the rest records how the
values were made (PRF-7), when ("created_ns", wall-clock ns) and the spike parameters ("spike").

The file names the baseline run that froze it ("baseline_run_id"), and that run directory,
runs/<baseline_run_id>/, must be kept while the file is in use: report/validate.py recomputes the
frozen values from its report.json for every capacity or spike limiter run (PRF-7: never relaxed
or edited), and the report compares against it, so without it every later limiter run of the
scenario and seed fails. As the file is never rewritten, the way out is to restore that run, or
to change the scenario's seed or name.

Exit 0 when the file was written, 1 when nothing was written, 2 for a wrong command line, 3 when
the file was written but neither confirmed durable nor removed again (remove it by hand).
"""

import argparse
import json
import os
import secrets
import sys
import time
from pathlib import Path
from typing import TextIO

from report import InputError, validate
from report import runfiles as rf
from report.build import (
    ALLOWED_LAG_NS,
    ERROR_RATE_FLOOR,
    LATENCY_ALLOWANCE_NS,
    SPIKE_RECOVERY_NS,
    SPIKE_WINDOW_NS,
    WARMUP_NS,
    parse_thresholds,
    sha256_hex,
    spike_params,
    step_measures,
)
from report.stats import step_bounds

MODE = 0o444


class ThresholdsError(Exception):
    """A freeze that wrote nothing: the run is not a valid baseline."""


class NotDurableError(Exception):
    """A freeze that linked the final name but failed after it, and could not remove it again:
    the complete file is there, but a crash may lose it."""


def definitions() -> dict[str, object]:
    """The §B9 definitions the values follow, recorded with them (PRF-7)."""
    return {
        "latency_population": "initial attempts (attempt 1) that received an HTTP response; "
        "end-to-end latency from gen.jsonl",
        "error_denominator": "initial attempts (attempt 1)",
        "unexpected_errors": "5xx other than service_aborted_after_acquire outside every fault "
        "window [start_ns, end_ns + P], plus transport errors and timeouts",
        "observation_window": "each load step minus its first 5 s, by planned_ns",
        "observation_skip_ns": WARMUP_NS,
        "allowed_lag": "step p99 of sent_ns − planned_ns ≤ 5 ms, an unwritten first attempt above "
        "any bound; otherwise the step is generator-limited and gets no thresholds",
        "allowed_lag_ns": ALLOWED_LAG_NS,
        "latency_threshold": "the baseline step's window p99 + 50 ms",
        "latency_allowance_ns": LATENCY_ALLOWANCE_NS,
        "error_threshold": "max(0.1 %, 2 × the baseline step's window unexpected-error rate)",
        "error_rate_floor": ERROR_RATE_FLOOR,
        "missing_baseline_step": "a step that did not run in full with planned entries, or has "
        "no latency population, gets no thresholds (missing_steps)",
        "spike_recovery": "deadline = the spike step's end + 30 s; window = [deadline, deadline "
        "+ 60 s], judged against the post-spike step's thresholds",
        "spike_recovery_ns": SPIKE_RECOVERY_NS,
        "spike_window_ns": SPIKE_WINDOW_NS,
    }


def step_values(
    report: dict[str, object],
) -> tuple[list[dict[str, object]], list[dict[str, object]]]:
    """The thresholds of each step of a baseline report, and the steps left out with why."""
    where = rf.REPORT_JSON
    measures = step_measures(report, where)
    lat_steps = rf.get_list(rf.get_obj(report, "timing", where), "steps", f"{where}: timing")
    unexpected = rf.get_obj(rf.get_obj(report, "results", where), "unexpected_errors", where)
    err_steps = rf.get_list(unexpected, "steps", f"{where}: results.unexpected_errors")
    entries: list[dict[str, object]] = []
    missing: list[dict[str, object]] = []
    for k, m in enumerate(measures):
        if not m.ran_in_full():
            reason = f"did not run in full with planned entries (ran {m.ran}, planned {m.planned})"
            missing.append({"step": k, "reason": reason})
            continue
        if m.generator_limited:
            entries.append({"step": k, "generator_limited": True})
            continue
        lw = f"{where}: timing.steps[{k}]"
        latency = rf.get_obj(rf.as_obj(lat_steps[k], lw, "steps"), "latency_ns", lw)
        ew = f"{where}: results.unexpected_errors.steps[{k}]"
        err = rf.as_obj(err_steps[k], ew, "steps")
        attempts = rf.get_int(err, "attempts", ew)
        errors = rf.get_int(err, "unexpected", ew)
        if m.p99_ns is None or attempts == 0:
            missing.append({"step": k, "reason": "no latency population in the observation window"})
            continue
        error_rate = 2 * (errors / attempts)  # 2 × the rate, exactly the double of its float
        entries.append(
            {
                "step": k,
                "generator_limited": False,
                "latency_threshold_ns": m.p99_ns + LATENCY_ALLOWANCE_NS,
                # A rate is at most 1: min(1, …) changes no comparison, and hitgen takes 0 to 1.
                "error_rate_threshold": min(1.0, max(ERROR_RATE_FLOOR, error_rate)),
                "baseline_p99_ns": m.p99_ns,
                "baseline_latency_count": rf.get_int(latency, "count", lw),
                "baseline_attempts": attempts,
                "baseline_unexpected": errors,
            }
        )
    return entries, missing


def thresholds_doc(run_dir: Path, created_ns: int) -> tuple[dict[str, object], int]:
    """The thresholds file's content for the baseline run in run_dir, and its step count."""
    ctx = rf.read_context(run_dir)
    cfg = rf.read_config(run_dir)
    if not ctx.baseline or cfg.limiter_enabled:
        raise ThresholdsError("not a baseline run: only a baseline (limiter off) sets thresholds")
    scenario = rf.read_scenario(ctx, run_dir, sha256_hex)
    report = rf.read_json(run_dir.joinpath(rf.REPORT_JSON))
    entries, missing = step_values(report)
    if len(entries) + len(missing) != len(scenario.steps):
        raise InputError(rf.REPORT_JSON, "steps", "not one per scenario step")
    spike = spike_params(scenario, step_bounds([s.duration_s for s in scenario.steps]))
    doc: dict[str, object] = {
        "scenario": ctx.scenario_name,
        "seed": ctx.seed,
        "baseline_run_id": ctx.run_id,
        "schedule_sha256": ctx.schedule_sha256,
        "created_ns": created_ns,
        "definitions": definitions(),
        "steps": entries,
        "missing_steps": missing,
        "spike": spike.to_json() if spike is not None else None,
    }
    return doc, len(scenario.steps)


def write_once(path: Path, data: bytes) -> None:
    """Creates path holding data, mode 0444, never over an existing file (FileExistsError) and
    never incomplete: the bytes go to a temp file in the same directory (O_CREAT|O_EXCL, mode
    0444, fsynced), which is hard-linked to path (atomic, EEXIST when path exists) and then
    unlinked, and the directory is fsynced. Before the link a failure removes the temp file (a
    crash may leave it, under its own name, but never path) and leaves an existing path alone.
    After the link an OSError (unlinking the temp file, or the directory's open or fsync) removes
    path again, then is raised: nothing was written (a temp file that cannot be unlinked stays
    under its own name). When removing path fails too, NotDurableError is raised instead."""
    temp = path.parent.joinpath(f".{path.name}.{secrets.token_hex(8)}.tmp")
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, MODE)
    try:
        try:
            os.fchmod(fd, MODE)  # whatever the umask
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view) :]
            os.fsync(fd)
        finally:
            os.close(fd)
        os.link(temp, path)
    except BaseException:
        temp.unlink(missing_ok=True)
        raise
    try:
        temp.unlink(missing_ok=True)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except OSError as err:
        try:
            path.unlink()  # this call's own link: the name holds no frozen file yet
        except OSError as undo:
            raise NotDurableError(
                f"{path} was written, but {err}; removing it failed too ({undo}): a crash may "
                "lose it, so remove it by hand before the next freeze"
            ) from err
        raise


def freeze(run_dir: Path, out: Path, created_ns: int | None = None) -> dict[str, object]:
    """Writes the thresholds of the baseline run in run_dir to out, once (created_ns defaults to
    now on the wall clock). Raises ThresholdsError or InputError for an invalid baseline,
    FileExistsError when out exists, or another OSError, each writing nothing; NotDurableError
    when out was written but neither confirmed durable nor removed again (write_once)."""
    failures = validate.check(run_dir, "baseline")
    if failures:
        raise ThresholdsError(
            "the baseline is not valid (report/validate.py --mode baseline): "
            + "; ".join(f.line() for f in failures)
        )
    doc, n_steps = thresholds_doc(run_dir, time.time_ns() if created_ns is None else created_ns)
    data = (json.dumps(doc, indent=2, allow_nan=False) + "\n").encode("utf-8")
    parse_thresholds(data, out.name, n_steps)  # the shape hitgen and the report read
    out.parent.mkdir(parents=True, exist_ok=True)
    write_once(out, data)
    return doc


def main(argv: list[str], out: TextIO | None = None, err: TextIO | None = None) -> int:
    stdout = out if out is not None else sys.stdout
    stderr = err if err is not None else sys.stderr
    parser = argparse.ArgumentParser(
        prog="python3 -m report.thresholds", description="Freeze a baseline run's thresholds."
    )
    parser.add_argument("run_dir", type=Path, help="the baseline run directory, runs/<id>")
    parser.add_argument("--out", type=Path, required=True, help="the frozen thresholds file")
    args = parser.parse_args(argv)
    try:
        doc = freeze(args.run_dir, args.out)
    except FileExistsError:
        print(f"thresholds: {args.out} exists: it is never overwritten (PRF-7)", file=stderr)
        return 1
    except (ThresholdsError, InputError) as error:
        print(f"thresholds: {args.run_dir}: {error}; nothing written", file=stderr)
        return 1
    except OSError as error:
        print(f"thresholds: writing {args.out}: {error}; nothing written", file=stderr)
        return 1
    except NotDurableError as error:
        print(f"thresholds: {error}", file=stderr)
        return 3
    print(json.dumps({"thresholds": str(args.out), "created_ns": doc["created_ns"]}), file=stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
