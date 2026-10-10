"""Synthetic run directories for the report's tests and its budget (IMPLEMENTATION_PLAN H5).

The files follow the shapes the harness writes (DESIGN §B8; the golden run-log lines; H3's
context.json, config.toml, db_rows.json, pids.json and hitgen.stderr). Values are whatever a
test sets: they are inputs, never expected values.
"""

import hashlib
import json
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from pathlib import Path

# hitgen's t0 on the wall clock in every synthetic run: 2026-10-07T12:00:00Z.
T0_WALL_NS = 1_791_374_400_000_000_000
T0_TEXT = "2026-10-07T12:00:00Z"
SECOND = 1_000_000_000
P_NS = 5 * SECOND


@dataclass
class RunSpec:
    """A synthetic run's identity and scenario."""

    name: str = "synthetic"
    seed: int = 1
    run_kind: str = "test"
    instances: int = 3
    steps: list[tuple[float, float]] = field(default_factory=lambda: [(10.0, 20.0)])  # rate, s
    users: int = 10
    baseline: bool = False
    verdict: str = "none"
    spike_step: int | None = None
    forced_aof_rewrites_s: list[float] = field(default_factory=list)
    faults: list[dict[str, object]] | None = None
    rewrites: list[dict[str, object]] | None = None
    p_ns: int = P_NS
    run_id: str = ""

    def id(self) -> str:
        return self.run_id or f"20261007-120000-{self.name}-{self.seed}"


def scenario_text(spec: RunSpec) -> str:
    lines = [
        f'name = "{spec.name}"',
        f"seed = {spec.seed}",
        f'run_kind = "{spec.run_kind}"',
        f"instances = {spec.instances}",
        f'verdict = "{spec.verdict}"',
    ]
    if spec.spike_step is not None:
        lines.append(f"spike_step = {spec.spike_step}")
    if spec.forced_aof_rewrites_s:
        lines.append(f"forced_aof_rewrites_s = {json.dumps(spec.forced_aof_rewrites_s)}")
    for rate, duration in spec.steps:
        lines.extend(["", "[[steps]]", f"rate_per_s = {rate}", f"duration_s = {duration}"])
    lines.extend(["", "[users]", f"count = {spec.users}", ""])
    return "\n".join(lines)


def config_text(spec: RunSpec) -> str:
    """A run's config.toml: harness.toml's keys (DESIGN §B2), the limiter off for a baseline."""
    enabled = "false" if spec.baseline else "true"
    return (
        "[policy]\nrate_tokens = 10\nrate_period_ns = 1_000_000_000\ncapacity = 20\n"
        'failure_mode = "closed"\nmax_tracked_buckets = 100_000\nmax_live_markers = 50_000\n'
        "max_markers_per_key = 1_000\nreceipt_lifetime_ns = 60_000_000_000\n"
        "refund_deadline_ns = 30_000_000_000\noutage_tolerance_ns = 5_000_000_000\n"
        'cleanup_interval_ns = 5_000_000_000\nclock_source = "store"\n\n'
        f"[instance]\nmax_release_delay_ns = {spec.p_ns}\nmargin_ns = 1_000_000_000\n"
        "max_held_receipts = 1152\nmax_pending_refunds = 1152\nrefund_workers = 128\n\n"
        '[store]\naddr = "127.0.0.1:6390"\n\n'
        f"[api]\nmax_in_flight = 512\nlimiter_enabled = {enabled}\nrefuse_after_acquire_bp = 0\n"
        'db_dsn = "postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64"\n'
    )


def context_doc(
    spec: RunSpec, scenario: Path, scenario_sha256: str, schedule_sha256: str | None
) -> dict[str, object]:
    """context.json as the orchestrator writes it (cmd/orchestrator context.go)."""
    label = "test-build scenario, not a benchmark" if spec.run_kind == "test" else "benchmark"
    return {
        "run_id": spec.id(),
        "epoch_id": "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f",
        "run_kind": spec.run_kind,
        "run_label": label,
        "baseline": spec.baseline,
        "outcome": "ok",
        "start_ns": T0_WALL_NS - 10 * SECOND,
        "end_ns": T0_WALL_NS + 600 * SECOND,
        "scenario": {"name": spec.name, "path": str(scenario), "sha256": scenario_sha256},
        "seed": spec.seed,
        "instances": spec.instances,
        "hardware": {"model": "Mac15,9", "ncpu": 16, "memsize_bytes": 68719476736},
        "os": {"ProductName": "macOS", "ProductVersion": "26.0", "BuildVersion": "25A1"},
        "versions": {
            "go": "go1.27.1",
            "valkey": "Server v=8.1.1",
            "valkey_path": "valkey-server",
            "postgres": "postgres (PostgreSQL) 17.2",
        },
        "store_settings": {"appendfsync": "always", "maxclients": "4096"},
        "postgres_settings": {"fsync": "on", "max_connections": "300"},
        "conformance": {
            "fsync": {"durable_script_p50_us": 6460.5, "f_fullfsync_floor_p50_us": 5667.9}
        },
        "settings": {"instance": {"max_release_delay_ns": spec.p_ns}},
        "instance_configs": None,
        "controlled_clock_ns": None,
        "schedule_sha256": schedule_sha256,
        "thresholds_sha256": None,
        "faults": spec.faults,
        "rewrites": spec.rewrites,
        "children": [],
    }


def hitgen_stderr(
    planned: int, retry_drops: int = 0, generator_drops: int = 0, time_text: str = T0_TEXT
) -> str:
    """hitgen.stderr: its slog JSON lines, with the sending line at time_text."""
    lines = [
        {"time": time_text, "level": "INFO", "msg": "sending", "entries": planned},
        {
            "time": time_text,
            "level": "INFO",
            "msg": "sending done",
            "planned": planned,
            "launched": planned,
            "unsent": 0,
            "generator_drops": generator_drops,
            "retry_drops": retry_drops,
        },
    ]
    return "".join(json.dumps(line) + "\n" for line in lines)


def write_lines(path: Path, lines: Iterable[dict[str, object] | str]) -> int:
    """Writes one JSON line per item (a str is written as it is) and returns their count."""
    count = 0
    with path.open("w", encoding="utf-8", buffering=1 << 20) as stream:
        for line in lines:
            stream.write(line if isinstance(line, str) else json.dumps(line) + "\n")
            count += 1
    return count


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_run(
    root: Path,
    spec: RunSpec,
    *,
    schedule: Iterable[dict[str, object] | str],
    gen: Iterable[dict[str, object] | str],
    apis: Mapping[int, Iterable[dict[str, object] | str]],
    samples: Iterable[dict[str, object] | str],
    db_rows: int = 0,
    stderr: str | None = None,
) -> Path:
    """root/runs/<id>/ with every file the report builder reads, and root/<name>.toml, the
    scenario. Returns the run directory."""
    run_dir = root.joinpath("runs", spec.id())
    run_dir.mkdir(parents=True)
    scenario = root.joinpath(f"{spec.name}.toml")
    scenario.write_text(scenario_text(spec), encoding="utf-8")
    planned = write_lines(run_dir.joinpath("schedule.jsonl"), schedule)
    write_lines(run_dir.joinpath("gen.jsonl"), gen)
    for instance, lines in apis.items():
        write_lines(run_dir.joinpath(f"api-{instance}.jsonl"), lines)
    write_lines(run_dir.joinpath("samples.jsonl"), samples)
    run_dir.joinpath("config.toml").write_text(config_text(spec), encoding="utf-8")
    run_dir.joinpath("db_rows.json").write_text(json.dumps({"db_rows": db_rows}) + "\n")
    procs: list[dict[str, object]] = [{"role": "valkey-server", "pid": 4001}]
    procs.extend({"role": "hitapi", "instance": n, "pid": 4100 + n} for n in apis)
    run_dir.joinpath("pids.json").write_text(json.dumps({"procs": procs}) + "\n")
    text = stderr if stderr is not None else hitgen_stderr(planned)
    run_dir.joinpath("hitgen.stderr").write_text(text, encoding="utf-8")
    ctx = context_doc(
        spec, scenario, file_sha256(scenario), file_sha256(run_dir.joinpath("schedule.jsonl"))
    )
    run_dir.joinpath("context.json").write_text(json.dumps(ctx, indent=2) + "\n")
    return run_dir


# A planned entry of rows_run: (planned_ns, lag_ns (None: never written), latency_ns, status,
# error, logged), logged False for an entry with no gen.jsonl line (a generator drop).
Row = tuple[int, int | None, int, int, str | None, bool]


def ok_row(planned_ns: int, lag_ns: int = 100_000, latency_ns: int = 2_000_000) -> Row:
    """An entry answered 200, written lag_ns after planned_ns."""
    return (planned_ns, lag_ns, latency_ns, 200, None, True)


def rows_run(
    root: Path,
    run_id: str,
    steps: list[tuple[float, float]],
    rows: list[Row],
    *,
    name: str,
    run_kind: str = "bench",
    baseline: bool = False,
    verdict: str = "none",
    spike_step: int | None = None,
    tail: dict[str, object] | None = None,
    api: list[dict[str, object]] | None = None,
    retries: list[dict[str, object]] | None = None,
) -> Path:
    """A one-instance run of scenario name, seed 1, with one schedule entry per row (rows in
    planned order) and its first attempt for each logged row, then the retries lines; tail
    replaces the summary line and api the instance's run log (start, ready, stop)."""
    spec = RunSpec(
        name=name,
        instances=1,
        steps=steps,
        run_kind=run_kind,
        baseline=baseline,
        verdict=verdict,
        spike_step=spike_step,
        run_id=run_id,
    )
    bounds = [0]
    for _, seconds in steps:
        bounds.append(bounds[-1] + round(seconds * SECOND))
    schedule: list[dict[str, object]] = []
    gen: list[dict[str, object]] = []
    for i, (ns, lag, latency, status, error, logged) in enumerate(rows):
        step = max(k for k in range(len(steps)) if bounds[k] <= ns)
        schedule.append(entry(i, ns, step=step))
        if logged:
            gen.append(attempt(i, ns, lag_ns=lag, latency=latency, status=status, error=error))
    gen.extend(retries or [])
    gen.append(tail if tail is not None else summary(len(rows)))
    lines = api if api is not None else [lifecycle_start(), lifecycle("ready"), lifecycle("stop")]
    return write_run(
        root,
        spec,
        schedule=schedule,
        gen=gen,
        apis={1: lines},
        samples=[sample(T0_WALL_NS + SECOND)],
    )


def thresholds_text(
    steps: Mapping[int, tuple[int, float] | None],
    spike: Mapping[str, object] | None,
    created_ns: int,
    schedule_sha256: str | None,
) -> str:
    """A thresholds file in report/thresholds.py's shape: per step its (latency_threshold_ns,
    error_rate_threshold), or None for a generator-limited one; schedule_sha256 is the
    baseline's schedule."""
    entries: list[dict[str, object]] = []
    for k, limits in sorted(steps.items()):
        if limits is None:
            entries.append({"step": k, "generator_limited": True})
        else:
            entries.append(
                {
                    "step": k,
                    "generator_limited": False,
                    "latency_threshold_ns": limits[0],
                    "error_rate_threshold": limits[1],
                }
            )
    doc = {
        "created_ns": created_ns,
        "schedule_sha256": schedule_sha256,
        "steps": entries,
        "spike": dict(spike) if spike is not None else None,
    }
    return json.dumps(doc) + "\n"


def schedule_sha256_of(run_dir: Path) -> str:
    """The schedule_sha256 run_dir's context.json records."""
    ctx = json.loads(run_dir.joinpath("context.json").read_text(encoding="utf-8"))
    return str(ctx["schedule_sha256"])


def write_index(root: Path, lines: Iterable[dict[str, object]]) -> None:
    """root/runs/index.jsonl (H3's {"scenario","baseline","run_id"} lines)."""
    text = "".join(json.dumps(line) + "\n" for line in lines)
    root.joinpath("runs", "index.jsonl").write_text(text, encoding="utf-8")


def update_context(run_dir: Path, **fields: object) -> None:
    """Rewrites run_dir/context.json with fields set."""
    path = run_dir.joinpath("context.json")
    ctx = json.loads(path.read_text(encoding="utf-8"))
    ctx.update(fields)
    path.write_text(json.dumps(ctx, indent=2) + "\n", encoding="utf-8")


def copy_thresholds(run_dir: Path, frozen: Path) -> str:
    """Copies a frozen thresholds file into run_dir as thresholds.json and records its SHA-256 in
    context.json, as the orchestrator does (DESIGN §B5 "Thresholds"); returns the SHA-256."""
    data = frozen.read_bytes()
    run_dir.joinpath("thresholds.json").write_bytes(data)
    sha = hashlib.sha256(data).hexdigest()
    update_context(run_dir, thresholds_sha256=sha)
    return sha


# Run-log lines (golden shapes).


def entry(
    i: int,
    planned_ns: int,
    *,
    step: int = 0,
    user: str = "u000000",
    instance: int = 1,
    kind: str = "initial",
    duplicate_of: int | None = None,
    request_id: str | None = None,
) -> dict[str, object]:
    """One schedule.jsonl line (a duplicate repeats its original's user and request ID)."""
    return {
        "i": i,
        "planned_ns": planned_ns,
        "step": step,
        "user": user,
        "request_id": request_id if request_id is not None else f"r{i:09d}",
        "instance": instance,
        "kind": kind,
        "duplicate_of": duplicate_of,
    }


def attempt(
    i: int,
    planned_ns: int,
    *,
    lag_ns: int | None = 100_000,
    status: int = 200,
    error: str | None = None,
    latency: int | None = 2_000_000,
    user: str = "u000000",
    instance: int = 1,
    kind: str = "initial",
    attempt_no: int = 1,
    sent_ns: int | None = None,
    got_conn_ns: int = 40_000,
    duplicate_of: int | None = None,
    request_id: str | None = None,
) -> dict[str, object]:
    """One gen.jsonl attempt line: sent lag_ns after planned_ns (None: never written)."""
    sent = sent_ns if sent_ns is not None else None if lag_ns is None else planned_ns + lag_ns
    retry = attempt_no > 1
    return {
        "kind": "retry" if retry else kind,
        "planned_ns": planned_ns,
        "sent_ns": sent,
        "got_conn_wait_ns": None if sent is None else got_conn_ns,
        "instance": instance,
        "user": user,
        "request_id": request_id if request_id is not None else f"r{i:09d}",
        "duplicate_of": duplicate_of,
        "attempt": attempt_no,
        "hint_source": "Retry-After" if retry else None,
        "hint_ms": 1000 if retry else None,
        "jitter_ns": 12_345 if retry else None,
        "status": status,
        "error": error,
        "latency": None if sent is None else latency,
        "arrival_ns": None,
        "decision_ns": None,
    }


def summary(
    planned: int,
    *,
    unsent: int = 0,
    log_drops: int = 0,
    stopped_after_step: int | None = None,
    excluded_step: int | None = None,
) -> dict[str, object]:
    return {
        "kind": "summary",
        "planned": planned,
        "launched": planned,
        "unsent": unsent,
        "log_drops": log_drops,
        "stopped_after_step": stopped_after_step,
        "excluded_step": excluded_step,
        "skipped_no_eligible": 0,
    }


def lifecycle_start() -> dict[str, object]:
    return {
        "kind": "lifecycle",
        "event": "start",
        "max_in_flight": 512,
        "queue_len": 0,
        "max_release_delay_ns": P_NS,
        "dependency_deadline_ns": 2 * SECOND,
        "drain_timeout_ns": 35 * SECOND,
        "nofile_limit": 8192,
    }


def lifecycle(event: str, listener_drops: int = 0, log_drops: int = 0) -> dict[str, object]:
    """A ready, stop or pre_kill (read_ok true) line."""
    if event == "ready":
        return {"kind": "lifecycle", "event": "ready"}
    line: dict[str, object] = {
        "kind": "lifecycle",
        "event": event,
        "listener_drops": listener_drops,
        "log_drops": log_drops,
    }
    if event == "pre_kill":
        line["read_ok"] = True
    return line


def request(
    instance: int,
    request_id: str,
    *,
    arrival_ns: int,
    status: int = 200,
    error: str = "",
    result: str = "allowed",
    existed: bool = False,
    user: str = "u000000",
    decision_ns: int = 0,
    deduction_id: str = "",
    limiter_ns: int = 500_000,
    db_ns: int = 1_500_000,
) -> dict[str, object]:
    return {
        "kind": "request",
        "run_id": "synthetic",
        "epoch_id": "5f0c9a3e1b7d4c2a8e6f0b1d3c5a7e9f",
        "instance": instance,
        "user_id": user,
        "request_id": request_id,
        "arrival_ns": arrival_ns,
        "decision_ns": decision_ns,
        "deduction_id": deduction_id,
        "result": result,
        "existed": existed,
        "http_status": status,
        "error": error,
        "limiter_ns": limiter_ns,
        "db_ns": db_ns,
        "total_ns": limiter_ns + db_ns + 100_000,
    }


def sample(t_ns: int, *, rewriting: bool | None = False, instances: int = 1) -> dict[str, object]:
    """One samples.jsonl line (H3's shape): rewriting None leaves the store unread."""
    store: dict[str, object] | None = None
    if rewriting is not None:
        store = {
            "persistence": {"aof_rewrite_in_progress": "1" if rewriting else "0"},
            "clients": {"connected_clients": "12"},
        }
    apis = [
        {
            "instance": n,
            "addr": f"127.0.0.1:{8080 + n}",
            "health": {
                "status": "ready",
                "fixtures": False,
                "clock_source": "store",
                "listener_drops": 0,
                "log_drops": 0,
                "in_flight": 3,
            },
            "health_error": None,
            "metrics": {"rl_pending_refunds": 0, "rl_cleanup_lag_seconds": 0.25},
            "metrics_error": None,
        }
        for n in range(1, instances + 1)
    ]
    return {
        "t_ns": t_ns,
        "procs": [
            {"role": "valkey-server", "pid": 4001, "cpu": 5.5, "rss": 1 << 24, "error": None}
        ],
        "procs_error": None,
        "postgres": {"pid": 5000, "processes": 9, "cpu": 2.25, "rss": 1 << 26},
        "postgres_error": None,
        "apis": apis,
        "status": {"tracked_buckets": 10, "live_markers": 1, "quarantined_keys": 0},
        "status_error": None,
        "store": store,
        "store_error": None if store is not None else "INFO: EOF",
    }
