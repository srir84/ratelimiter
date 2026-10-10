"""`python3 -m report.budget`: the report builder's budget (IMPLEMENTATION_PLAN H5 step 10, plan
choice; DESIGN §C8 "its budget on a synthetic 12-million-line log").

It writes a synthetic run directory of 12,000,000 run-log lines (schedule.jsonl, gen.jsonl,
api-1…3.jsonl and samples.jsonl together) in a fresh directory under .out/, builds its report in
this process exactly as `python3 -m report` does (the BUD-11a check included), and prints the
builder's wall time and the process's peak RSS. The builder must finish in at most 300 s with a
peak RSS of at most 2 GB (2 × 10^9 bytes), and the report must show the whole builder ran: the
BUD-11a check's verdict pass and valid evidence. Exit 0 within the budget, 1 over it, when the
report fails either condition, or on any error. The directory is removed afterwards unless
--keep is given.

The run: 5 steps × 120 s at about 4,800 req/s over 2,000 users and 3 instances, a bench limiter
run with forced AOF rewrites; every 20th entry is limited once and retried (429, then 200), every
50th fails its DB write after an allowed acquire (503 database_unavailable, a release and an
applied refund), and the rest are allowed and create their rows.
"""

import argparse
import json
import resource
import shutil
import sys
import tempfile
import time
from collections.abc import Iterator
from pathlib import Path

from report import InputError
from report.build import build, write_report
from report.markdown import render
from report.synthetic import (
    SECOND,
    T0_WALL_NS,
    RunSpec,
    config_text,
    context_doc,
    file_sha256,
    hitgen_stderr,
    sample,
    scenario_text,
    write_lines,
)

LINES = 12_000_000
MAX_SECONDS = 300
MAX_RSS_BYTES = 2_000_000_000
REPO = Path(__file__).resolve().parents[1]

USERS = 2_000
INSTANCES = 3
STEPS = 5
STEP_S = 120
RUN_NS = STEPS * STEP_S * SECOND
SAMPLES = STEPS * STEP_S  # one a second
REWRITES_S = (180, 420)  # forced rewrites, each 20 s long in the samples
LIFECYCLE_LINES = 3 * INSTANCES  # start, ready, stop per instance
EPOCH = "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f"


def retries(entries: int) -> int:
    """Entries i with i % 20 == 0: limited once, then retried."""
    return (entries + 19) // 20


def failures(entries: int) -> int:
    """Entries i with i % 50 == 1: a DB failure after an allowed acquire."""
    return (entries + 48) // 50 if entries >= 2 else 0


def total_lines(entries: int, samples: int) -> int:
    schedule = entries
    gen = entries + retries(entries) + 1
    api = entries + (entries + retries(entries)) + 2 * failures(entries) + LIFECYCLE_LINES
    return schedule + gen + api + samples


def plan(lines: int) -> tuple[int, int]:
    """The most entries whose run fits in lines with a sample a second, and the samples that
    make up the rest exactly."""
    low, high = 0, lines
    while low < high:
        mid = (low + high + 1) // 2
        if total_lines(mid, SAMPLES) <= lines:
            low = mid
        else:
            high = mid - 1
    return low, SAMPLES + lines - total_lines(low, SAMPLES)


def planned_ns(i: int, entries: int) -> int:
    return i * RUN_NS // entries


def write_synthetic(root: Path, lines: int) -> tuple[Path, int, int]:
    entries, samples = plan(lines)
    spec = RunSpec(
        name="budget",
        run_kind="bench",
        instances=INSTANCES,
        steps=[(entries * SECOND / RUN_NS, float(STEP_S))] * STEPS,
        users=USERS,
        forced_aof_rewrites_s=[float(s) for s in REWRITES_S],
        rewrites=[
            {
                "at_s": s,
                "start_ns": T0_WALL_NS + s * SECOND,
                "end_ns": T0_WALL_NS + (s + 20) * SECOND,
            }
            for s in REWRITES_S
        ],
    )
    run_dir = root.joinpath("runs", spec.id())
    run_dir.mkdir(parents=True)
    scenario = root.joinpath("budget.toml")
    scenario.write_text(scenario_text(spec), encoding="utf-8")
    step_ns = STEP_S * SECOND

    def schedule() -> Iterator[str]:
        for i in range(entries):
            p = planned_ns(i, entries)
            yield (
                f'{{"i":{i},"planned_ns":{p},"step":{p // step_ns},"user":"u{i % USERS:06d}",'
                f'"request_id":"r{i:09d}","instance":{i % INSTANCES + 1},"kind":"initial",'
                '"duplicate_of":null}\n'
            )

    def gen() -> Iterator[str]:
        for i in range(entries):
            p = planned_ns(i, entries)
            sent = p + 100_000 + (i % 997) * 1_000
            latency = 2_000_000 + (i % 1_009) * 1_000
            head = (
                f'{{"kind":"initial","planned_ns":{p},"sent_ns":{sent},'
                f'"got_conn_wait_ns":{30_000 + (i % 13) * 1_000},"instance":{i % INSTANCES + 1},'
                f'"user":"u{i % USERS:06d}","request_id":"r{i:09d}","duplicate_of":null,'
                '"attempt":1,"hint_source":null,"hint_ms":null,"jitter_ns":null,'
            )
            decision = T0_WALL_NS + (sent // 1_000) * 1_000
            if i % 20 == 0:
                status, error, arrival = 429, '"rate_limited"', "null"
            elif i % 50 == 1:
                status, error, arrival = 503, '"database_unavailable"', "null"
            else:
                status, error, arrival = 200, "null", str(T0_WALL_NS + sent + 150_000)
            yield (
                f'{head}"status":{status},"error":{error},"latency":{latency},'
                f'"arrival_ns":{arrival},"decision_ns":{decision}}}\n'
            )
            if i % 20 == 0:
                retry_sent = sent + SECOND
                yield (
                    f'{{"kind":"retry","planned_ns":{p},"sent_ns":{retry_sent},'
                    f'"got_conn_wait_ns":20000,"instance":{i % INSTANCES + 1},'
                    f'"user":"u{i % USERS:06d}","request_id":"r{i:09d}","duplicate_of":null,'
                    '"attempt":2,"hint_source":"Retry-After","hint_ms":1000,"jitter_ns":23456,'
                    f'"status":200,"error":null,"latency":{latency},'
                    f'"arrival_ns":{T0_WALL_NS + retry_sent + 150_000},'
                    f'"decision_ns":{T0_WALL_NS + (retry_sent // 1_000) * 1_000}}}\n'
                )
        yield (
            f'{{"kind":"summary","planned":{entries},"launched":{entries},"unsent":0,'
            '"log_drops":0,"stopped_after_step":null,"excluded_step":null,'
            '"skipped_no_eligible":0}\n'
        )

    write_lines(run_dir.joinpath("schedule.jsonl"), schedule())
    write_lines(run_dir.joinpath("gen.jsonl"), gen())
    write_api_files(run_dir, entries)
    rewriting = {t for s in REWRITES_S for t in range(s, s + 21)}
    write_lines(
        run_dir.joinpath("samples.jsonl"),
        (
            sample(T0_WALL_NS + t * SECOND, rewriting=t in rewriting, instances=INSTANCES)
            for t in range(1, samples + 1)
        ),
    )
    run_dir.joinpath("config.toml").write_text(config_text(spec), encoding="utf-8")
    run_dir.joinpath("db_rows.json").write_text(f'{{"db_rows":{entries - failures(entries)}}}\n')
    run_dir.joinpath("hitgen.stderr").write_text(hitgen_stderr(entries), encoding="utf-8")
    doc = context_doc(
        spec, scenario, file_sha256(scenario), file_sha256(run_dir.joinpath("schedule.jsonl"))
    )
    run_dir.joinpath("context.json").write_text(json.dumps(doc) + "\n", encoding="utf-8")
    return run_dir, entries, samples


def write_api_files(run_dir: Path, entries: int) -> None:
    """api-1…3.jsonl: each instance's decision, request, release and refund lines."""
    paths = [run_dir.joinpath(f"api-{n}.jsonl") for n in range(1, INSTANCES + 1)]
    start = (
        '{"kind":"lifecycle","event":"start","max_in_flight":512,"queue_len":0,'
        '"max_release_delay_ns":5000000000,"dependency_deadline_ns":2000000000,'
        '"drain_timeout_ns":35000000000,"nofile_limit":8192}\n'
        '{"kind":"lifecycle","event":"ready"}\n'
    )
    streams = [p.open("w", encoding="utf-8", buffering=1 << 20) for p in paths]
    try:
        for stream in streams:
            stream.write(start)
        for i in range(entries):
            instance = i % INSTANCES + 1
            out = streams[instance - 1]
            p = planned_ns(i, entries)
            sent = p + 100_000 + (i % 997) * 1_000
            arrival = T0_WALL_NS + sent + 150_000
            decision = T0_WALL_NS + (sent // 1_000) * 1_000
            user = f"u{i % USERS:06d}"
            limiter_ns = 400_000 + (i % 1_000) * 100
            db_ns = 1_500_000 + (i % 777) * 1_000
            head = (
                f'{{"kind":"request","run_id":"budget","epoch_id":"{EPOCH}","instance":{instance},'
                f'"user_id":"{user}","request_id":"r{i:09d}",'
            )
            if i % 20 == 0:
                out.write(
                    f'{head}"arrival_ns":{arrival},"decision_ns":{decision},"deduction_id":"",'
                    '"result":"limited","existed":false,"http_status":429,"error":"rate_limited",'
                    f'"limiter_ns":{limiter_ns},"db_ns":0,"total_ns":{limiter_ns + 50_000}}}\n'
                )
                arrival += SECOND
                decision += SECOND
                deduction = f"d{i:09d}r"
            else:
                deduction = f"d{i:09d}"
            out.write(
                f'{{"kind":"decision","deduction_id":"{deduction}","user_id":"{user}",'
                f'"decision_ns":{decision},"result":"allowed"}}\n'
            )
            failed = i % 50 == 1
            status, error = (503, "database_unavailable") if failed else (200, "")
            if failed:
                release = 1_000_000_000 + p // 1_000
                out.write(
                    f'{{"kind":"release","deduction_id":"{deduction}","release_mono_ns":{release}}}\n'
                )
            out.write(
                f'{head}"arrival_ns":{arrival},"decision_ns":{decision},'
                f'"deduction_id":"{deduction}","result":"allowed","existed":false,'
                f'"http_status":{status},"error":"{error}","limiter_ns":{limiter_ns},'
                f'"db_ns":{db_ns},"total_ns":{limiter_ns + db_ns + 50_000}}}\n'
            )
            if failed:
                out.write(
                    f'{{"kind":"refund","deduction_id":"{deduction}","result":"applied",'
                    f'"reason":"","attempts":1,"credit_decision_ns":{decision + 5_000_000},'
                    f'"release_mono_ns":{release},"final_mono_ns":{release + 6_000_000}}}\n'
                )
        for stream in streams:
            stream.write('{"kind":"lifecycle","event":"stop","listener_drops":0,"log_drops":0}\n')
    finally:
        for stream in streams:
            stream.close()


def peak_rss_bytes() -> int:
    """This process's peak resident set size: ru_maxrss is in bytes on macOS, KiB on Linux."""
    peak = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    return peak if sys.platform == "darwin" else peak * 1024


def count_lines(run_dir: Path) -> int:
    """The run-log lines in run_dir, counted by newline in 4 MiB chunks."""
    total = 0
    for path in sorted(run_dir.glob("*.jsonl")):
        with path.open("rb") as stream:
            while chunk := stream.read(1 << 22):
                total += chunk.count(b"\n")
    return total


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog="python3 -m report.budget", description=__doc__)
    parser.add_argument("--lines", type=int, default=LINES, help="run-log lines (default 12e6)")
    parser.add_argument("--keep", action="store_true", help="keep the synthetic run directory")
    args = parser.parse_args(argv)
    out = REPO.joinpath(".out")
    out.mkdir(exist_ok=True)
    root = Path(tempfile.mkdtemp(prefix="report-budget-", dir=out))
    try:
        began = time.monotonic()
        run_dir, entries, samples = write_synthetic(root, args.lines)
        lines = count_lines(run_dir)
        written = time.monotonic() - began
        print(
            f"budget: {lines} lines ({entries} entries, {samples} samples) in {run_dir}, "
            f"written in {written:.1f} s"
        )
        if lines != args.lines:
            print(f"budget: wrote {lines} lines, want {args.lines}")
            return 1
        began = time.monotonic()
        report = build(run_dir)
        write_report(run_dir, report, render(report))
        elapsed = time.monotonic() - began
    except InputError as err:
        print(f"budget: the builder refused the synthetic run: {err}")
        return 1
    finally:
        if not args.keep:
            shutil.rmtree(root)
    peak = peak_rss_bytes()
    correctness = report.get("correctness")
    evidence = report.get("evidence_validity")
    verdict = correctness.get("verdict") if isinstance(correctness, dict) else None
    valid = evidence.get("valid") if isinstance(evidence, dict) else None
    within = elapsed <= MAX_SECONDS and peak <= MAX_RSS_BYTES
    whole = verdict == "pass" and valid is True
    print(
        f"budget: builder {elapsed:.1f} s (limit {MAX_SECONDS} s), peak RSS "
        f"{peak / 1e6:.0f} MB (limit {MAX_RSS_BYTES / 1e6:.0f} MB); correctness {verdict}, "
        f"evidence valid {valid}: {'within' if within else 'OVER'} budget"
    )
    if not whole:
        print(
            "budget: not measured: the synthetic run must pass the BUD-11a check with valid "
            f"evidence, so the whole builder ran (correctness {verdict}, evidence valid {valid})"
        )
    if args.keep:
        print(f"budget: kept {root}")
    return 0 if within and whole else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
