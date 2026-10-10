"""report/completion.py (IMPLEMENTATION_PLAN H6 steps 3 and 4; Tests (a)): the manifest built from a
synthetic runs/index.jsonl, and each HARNESS §8 Completion check on synthetic run directories
(report/synthetic.py) that report/validate.py passes, each case changing one thing: a clean set
exits 0, and each check broken on its own exits 1 naming that check. Expected values are
literals.

Most cases use make_world's five runs (s3-busy-user's baseline, limiter run and rerun, and two
S6 runs, closed and open), which list 3 of the 17 H6 scenarios: completion fails each other one
first (check 0, listed), and failed() and rest() assert exactly those failures before returning
the others. full_world adds a run of every other H6 scenario, so a clean one exits 0."""

import io
import json
from collections.abc import Callable, Sequence
from dataclasses import dataclass, field
from pathlib import Path

import pytest

from report import completion
from report.__main__ import main as build_main
from report.synthetic import (
    P_NS,
    SECOND,
    T0_WALL_NS,
    RunSpec,
    attempt,
    copy_thresholds,
    entry,
    lifecycle,
    lifecycle_start,
    ok_row,
    request,
    rows_run,
    sample,
    summary,
    update_context,
    write_run,
)
from report.thresholds import freeze

MS = 1_000_000
US = 1_000
DEADLINE_NS = 30 * SECOND  # config.toml's refund_deadline_ns (synthetic.config_text)
LIFETIME_NS = 60 * SECOND  # config.toml's receipt_lifetime_ns
OUTAGE_NS = 4_085_364_542  # the closed S6 run's store down, as measured (context.json faults)
CREATED_NS = T0_WALL_NS - 3600 * SECOND  # thresholds frozen before the runs' start
DECIDED = T0_WALL_NS + 7 * SECOND  # the budget time of the S6 runs' allowed decisions
RELEASED = 9 * SECOND  # a refund's release_mono_ns (the limiter's monotonic clock)
RELEASE_LINE_NS = 8 * SECOND  # a release line's release_mono_ns (hitapi's monotonic clock)
TOLERANCE_NS = 5 * SECOND  # config.toml's outage_tolerance_ns (synthetic.config_text)

S3 = "s3-busy-user"
CLOSED, OPEN = "s6-down-closed", "s6-down-open"
# The H6 scenario files in plan order (plan H6 steps 1 and 2), and the ones make_world lists.
H6 = (
    "s1-ramp",
    "s1-ramp-1api",
    "s2-users-100",
    "s2-users-1000",
    "s2-users-10000",
    "s3-busy-user",
    "s4-spike",
    "s5-retry",
    "s6-down-closed",
    "s6-down-open",
    "s6-pause-closed",
    "s6-pause-open",
    "s6-state-closed",
    "s6-state-open",
    "s6-capacity-closed",
    "s6-capacity-open",
    "s7-long",
)
WORLD = (S3, CLOSED, OPEN)
# The other H6 scenarios, which full_world adds: benchmarks (a baseline and a limiter run each)
# and S6 test runs (a limiter run each, in its name's failure mode).
OTHER_BENCH = (
    "s1-ramp",
    "s1-ramp-1api",
    "s2-users-100",
    "s2-users-1000",
    "s2-users-10000",
    "s4-spike",
    "s5-retry",
    "s7-long",
)
OTHER_S6 = (
    "s6-pause-closed",
    "s6-pause-open",
    "s6-state-closed",
    "s6-state-open",
    "s6-capacity-closed",
    "s6-capacity-open",
)
IDS = {
    "s3-base": "20261007-100000-s3-busy-user-1",
    "s3": "20261007-101000-s3-busy-user-1",
    "s3-base2": "20261007-101500-s3-busy-user-1",
    "s3-rerun": "20261007-102000-s3-busy-user-1",
    "closed": "20261007-103000-s6-down-closed-1",
    "open": "20261007-104000-s6-down-open-1",
}
# The closed run's one store outage segment, measured (FLT-1a); the open run's is FLT-1b's
# wrapper refusal, which measures none.
STORE_DOWN = {
    "at_s": 10,
    "cmd": "faultctl store down 4s",
    "instance": None,
    "start_ns": T0_WALL_NS + 10 * SECOND,
    "end_ns": T0_WALL_NS + 14_200 * MS,
    "outage_start_ns": T0_WALL_NS + 10_100 * MS,
    "outage_ns": OUTAGE_NS,
    "targets": [],
}
REFUSE = {
    "at_s": 10,
    "cmd": '{"target":"store","mode":"refuse","match":"acquire","duration_ms":5000}',
    "instance": 1,
    "start_ns": T0_WALL_NS + 10 * SECOND,
    "end_ns": T0_WALL_NS + 10_002 * MS,
    "outage_start_ns": None,
    "outage_ns": None,
    "targets": [{"instance": 1, "status": 200}],
}

Line = dict[str, object]


# Run-log lines.


def decision(deduction: str, decided: int, user: str, result: str = "allowed") -> Line:
    return {
        "kind": "decision",
        "deduction_id": deduction,
        "user_id": user,
        "decision_ns": decided,
        "result": result,
    }


def final_event(
    deduction: str,
    result: str = "applied",
    *,
    reason: str = "",
    credit_ns: int = DECIDED + MS,
    took_ns: int = 7 * MS,
) -> Line:
    """A refund line (the final event): released at RELEASED, final took_ns later; the credit
    time only for a confirmed result (0 otherwise, as the limiter writes it)."""
    confirmed = result in ("applied", "already_applied")
    return {
        "kind": "refund",
        "deduction_id": deduction,
        "result": result,
        "reason": reason,
        "attempts": 1,
        "credit_decision_ns": credit_ns if confirmed else 0,
        "release_mono_ns": RELEASED,
        "final_mono_ns": RELEASED + took_ns,
    }


def released(
    deduction: str,
    final: Line | None,
    *,
    elapsed: tuple[int, int] = (2 * MS, 0),
    with_request: bool = True,
    user: str = "u000003",
    instance: int = 1,
    release_ns: int = RELEASE_LINE_NS,
) -> list[Line]:
    """A permit released for a confirmed failure, in hitapi's order: its allowed decision, its
    release (release_mono_ns release_ns), its final event (when given) and its request line (from
    instance), whose limiter_ns and db_ns are elapsed."""
    lines: list[Line] = [
        decision(deduction, DECIDED, user),
        {"kind": "release", "deduction_id": deduction, "release_mono_ns": release_ns},
    ]
    if final is not None:
        lines.append(final)
    if with_request:
        # Refused after its acquire (ERR-15), or, with database time, failed in the database
        # step (ERR-14).
        error = "database_unavailable" if elapsed[1] else "service_aborted_after_acquire"
        lines.append(
            request(
                instance,
                f"q-{deduction}",
                arrival_ns=T0_WALL_NS + 7 * SECOND,
                status=503,
                error=error,
                user=user,
                decision_ns=DECIDED,
                deduction_id=deduction,
                limiter_ns=elapsed[0],
                db_ns=elapsed[1],
            )
        )
    return lines


def violation_lines() -> list[Line]:
    """21 admissions of u000005 at one budget time (capacity 20: a BUD-11 violation)."""
    lines: list[Line] = []
    for n in range(21):
        lines.append(decision(f"d-v{n}", DECIDED + 3 * SECOND, "u000005"))
        lines.append(
            request(
                1,
                f"q-v{n}",
                arrival_ns=T0_WALL_NS + 9 * SECOND,
                user="u000005",
                decision_ns=DECIDED + 3 * SECOND,
                deduction_id=f"d-v{n}",
            )
        )
    return lines


def s6_lines(
    mode: str,
    extra: Sequence[Line] = (),
    *,
    killed: Sequence[Line] = (),
    err16: bool = True,
    violation: bool = False,
) -> list[Line]:
    """One instance's run log of an S6 run: an allowed request (ERR-1), an ERR-9 request, an
    ERR-16 one (its permit done, no release), an ERR-8 (closed) or an ERR-3 and a degraded permit
    refused after its acquire (open: a release with no deduction_id and no final event), a permit
    refused after its acquire whose refund was applied 7 ms after its release, then extra. killed
    puts a first process segment, holding those lines and ending with a pre_kill line (FLT-6),
    before it; violation adds violation_lines()."""
    lines: list[Line] = []
    if killed:
        lines.extend([lifecycle_start(), lifecycle("ready"), *killed, lifecycle("pre_kill")])
    lines.extend([lifecycle_start(), lifecycle("ready")])
    arrival = T0_WALL_NS + 6 * SECOND
    lines.append(decision("d-err1", DECIDED, "u000001"))
    lines.append(
        request(
            1,
            "q-err1",
            arrival_ns=arrival,
            user="u000001",
            decision_ns=DECIDED,
            deduction_id="d-err1",
        )
    )
    lines.append(
        request(
            1,
            "q-err9",
            arrival_ns=arrival,
            status=503,
            error="limiter_outcome_unknown",
            result="outcome_unknown",
            db_ns=0,
        )
    )
    if err16:
        lines.append(decision("d-err16", DECIDED, "u000002"))
        lines.append(
            request(
                1,
                "q-err16",
                arrival_ns=arrival,
                status=503,
                error="application_outcome_unknown",
                user="u000002",
                decision_ns=DECIDED,
                deduction_id="d-err16",
            )
        )
    if mode == "closed":
        lines.append(
            request(
                1,
                "q-err8",
                arrival_ns=arrival,
                status=503,
                error="limiter_unavailable",
                result="unavailable",
                db_ns=0,
            )
        )
    else:
        lines.append(decision("", 0, "u000004", "degraded"))
        lines.append(request(1, "q-err3", arrival_ns=arrival, result="degraded", user="u000004"))
        lines.append(decision("", 0, "u000004", "degraded"))
        lines.append({"kind": "release", "deduction_id": "", "release_mono_ns": 8 * SECOND})
        lines.append(
            request(
                1,
                "q-degraded-refused",
                arrival_ns=arrival,
                status=503,
                error="service_aborted_after_acquire",
                result="degraded",
                user="u000004",
                db_ns=0,
            )
        )
    lines.extend(released("d-ok", final_event("d-ok")))
    if violation:
        lines.extend(violation_lines())
    lines.extend(extra)
    lines.append(lifecycle("stop"))
    return lines


# Run directories.


@dataclass
class World:
    """root/runs/: the synthetic runs and runs/index.jsonl, as the orchestrator appends it."""

    root: Path
    index: list[Line] = field(default_factory=list)

    def run(self, key: str) -> Path:
        return self.root.joinpath("runs", IDS[key])

    def add(self, scenario: str, baseline: bool, run_id: str) -> None:
        self.index.append({"scenario": scenario, "baseline": baseline, "run_id": run_id})
        text = "".join(json.dumps(line) + "\n" for line in self.index)
        self.root.joinpath("runs", "index.jsonl").write_text(text, encoding="utf-8")

    def manifest(self, edit: Callable[[dict[str, object]], None] | None = None) -> Path:
        """runs/h6-manifest.json as --build-manifest makes it from the index, then edited."""
        doc = completion.build_manifest(self.root.joinpath("runs", "index.jsonl")).to_json()
        if edit is not None:
            edit(doc)
        path = self.root.joinpath("runs", "h6-manifest.json")
        path.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
        return path


def bench_run(
    world: World,
    run_id: str,
    *,
    baseline: bool,
    name: str = S3,
    planned_s: Sequence[int] = (6, 10, 15),
    frozen: Path | None = None,
    extra: Sequence[Line] = (),
) -> Path:
    """A one-instance benchmark run of scenario name (verdict none, one 20 s step, one entry
    answered 200 at each planned_s), its report built and its index line appended: a baseline
    freezes the scenario's thresholds once, a limiter run copies them, as the orchestrator does
    (DESIGN §B5); frozen names another thresholds file than the scenario's, and extra adds run-log
    lines."""
    run = rows_run(
        world.root,
        run_id,
        [(1.0, 20.0)],
        [ok_row(s * SECOND) for s in planned_s],
        name=name,
        baseline=baseline,
        api=[lifecycle_start(), lifecycle("ready"), *extra, lifecycle("stop")],
    )
    if frozen is None:
        frozen = world.root.joinpath("runs", "thresholds", f"{name}-1.json")
    if baseline:
        assert build_main([str(run)]) == 0
        if not frozen.exists():
            freeze(run, frozen, created_ns=CREATED_NS)
    else:
        copy_thresholds(run, frozen)
        assert build_main([str(run)]) == 0
    world.add(name, baseline, run.name)
    return run


def s6_run(
    world: World,
    run_id: str,
    name: str,
    mode: str,
    apis: dict[int, list[Line]],
    faults: list[dict[str, object]] | None = None,
) -> Path:
    """An S6 test run of scenario name with one instance per apis entry (its run log lines), its
    config.toml in the given failure mode, its report built and its index line appended; its
    context.json faults are the mode's one outage segment (STORE_DOWN closed, REFUSE open) unless
    faults gives them."""
    spec = RunSpec(
        name=name,
        instances=len(apis),
        steps=[(1.0, 20.0)],
        faults=faults if faults is not None else [STORE_DOWN if mode == "closed" else REFUSE],
        run_id=run_id,
    )
    run = write_run(
        world.root,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=[attempt(0, 6 * SECOND), summary(1)],
        apis=apis,
        samples=[sample(T0_WALL_NS + SECOND, instances=len(apis))],
    )
    config = run.joinpath("config.toml")
    text = config.read_text(encoding="utf-8")
    config.write_text(text.replace('failure_mode = "closed"', f'failure_mode = "{mode}"'))
    assert build_main([str(run)]) == 0
    world.add(name, False, run.name)
    return run


def make_world(
    root: Path,
    *,
    closed: list[Line] | None = None,
    open_: list[Line] | None = None,
    closed_apis: dict[int, list[Line]] | None = None,
    closed_faults: list[dict[str, object]] | None = None,
    rerun_planned_s: Sequence[int] | None = None,
    rerun_extra: Sequence[Line] = (),
) -> World:
    """The clean set: s3-busy-user's baseline, limiter run and rerun (one schedule), and the two
    S6 runs, closed and open, each one instance with s6_lines unless given (closed_apis: the
    closed run's instances and their run logs; closed_faults: its context.json faults).
    rerun_planned_s gives the rerun another schedule, with a baseline of that schedule just before
    it whose thresholds it copies (so validate.py still passes it); rerun_extra adds lines to the
    rerun's run log."""
    world = World(root)
    bench_run(world, IDS["s3-base"], baseline=True)
    bench_run(world, IDS["s3"], baseline=False)
    if rerun_planned_s is not None:
        other = world.root.joinpath("runs", "thresholds", f"{S3}-1-other.json")
        bench_run(world, IDS["s3-base2"], baseline=True, planned_s=rerun_planned_s, frozen=other)
        bench_run(world, IDS["s3-rerun"], baseline=False, planned_s=rerun_planned_s, frozen=other)
    else:
        bench_run(world, IDS["s3-rerun"], baseline=False, extra=rerun_extra)
    if closed_apis is None:
        closed_apis = {1: closed if closed is not None else s6_lines("closed")}
    s6_run(world, IDS["closed"], CLOSED, "closed", closed_apis, closed_faults)
    s6_run(world, IDS["open"], OPEN, "open", {1: open_ if open_ is not None else s6_lines("open")})
    return world


def other_id(name: str, role: str) -> str:
    """The run ID of another H6 scenario's baseline (09:00:00) or limiter run (09:30:00)."""
    return f"20261007-{'090000' if role == 'baseline' else '093000'}-{name}-1"


def full_world(root: Path) -> World:
    """make_world's clean set, then a baseline and a limiter run of each other H6 benchmark
    scenario and a limiter run (s6_lines) of each other S6 one: the manifest lists every H6
    scenario."""
    world = make_world(root)
    for name in OTHER_BENCH:
        bench_run(world, other_id(name, "baseline"), baseline=True, name=name)
        bench_run(world, other_id(name, "limiter"), baseline=False, name=name)
    for name in OTHER_S6:
        mode = "closed" if name.endswith("-closed") else "open"
        s6_run(world, other_id(name, "limiter"), name, mode, {1: s6_lines(mode)})
    return world


def check(manifest: Path) -> tuple[int, list[str]]:
    out = io.StringIO()
    code = completion.main([str(manifest)], out)
    return code, out.getvalue().splitlines()


def key_of(line: str) -> tuple[str, str, str]:
    """A FAIL or SKIP line's (scenario, run ID, check)."""
    head = line.partition(": ")[0].split(" ")
    assert len(head) == 4, line
    return head[1], head[2], head[3]


def failures(lines: list[str]) -> list[tuple[str, str, str]]:
    """Each FAIL line's (scenario, run ID, check)."""
    return [key_of(line) for line in lines if line.startswith("FAIL ")]


def rest(lines: list[str], listed: Sequence[str] = WORLD) -> list[str]:
    """The FAIL lines after the first ones, which must be exactly check 0's for each H6 scenario
    the world does not list (listed: those it does), in plan order."""
    fails = [line for line in lines if line.startswith("FAIL ")]
    unlisted = [(name, "-", "listed") for name in H6 if name not in listed]
    assert [key_of(line) for line in fails[: len(unlisted)]] == unlisted, lines
    return fails[len(unlisted) :]


def failed(lines: list[str], listed: Sequence[str] = WORLD) -> list[tuple[str, str, str]]:
    """Each FAIL line's (scenario, run ID, check) after check 0's for the unlisted scenarios."""
    return [key_of(line) for line in rest(lines, listed)]


def skips(lines: list[str]) -> list[str]:
    return [line for line in lines if line.startswith("SKIP ")]


def holds(manifest: Path, listed: Sequence[str] = WORLD) -> None:
    """Every check holds on the world but check 0, which fails each H6 scenario it does not list,
    so completion exits 1 with only those failures."""
    code, lines = check(manifest)
    assert code == 1 and failed(lines, listed) == [] and skips(lines) == [], lines


def edit_json(path: Path, dotted: str, value: object) -> None:
    """Sets the value at a dotted path of objects in a JSON file."""
    doc = json.loads(path.read_text(encoding="utf-8"))
    *parents, last = dotted.split(".")
    node = doc
    for part in parents:
        node = node[part]
    node[last] = value
    path.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")


def rewrite_log(run: Path, change: Callable[[list[Line]], list[Line]]) -> None:
    """Rewrites run/api-1.jsonl through change, after the report was built from it."""
    path = run.joinpath("api-1.jsonl")
    lines = [json.loads(text) for text in path.read_text(encoding="utf-8").splitlines()]
    path.write_text("".join(json.dumps(line) + "\n" for line in change(lines)), encoding="utf-8")


# --build-manifest.


def test_build_manifest_latest_runs(tmp_path: Path) -> None:
    """--build-manifest on a synthetic runs/index.jsonl: per H6 scenario its latest baseline run
    and latest limiter run; for s3-busy-user the earlier of its last two limiter runs is the
    limiter run and the later one s3-busy-user-rerun; other scenarios' entries (with no run
    directories, one not even a run ID) are passed over, and a scenario with no limiter run is as
    shown; the JSON on stdout is the manifest."""
    index = tmp_path.joinpath("index.jsonl")
    lines = [
        ("s1-ramp", True, "20261007-100000-s1-ramp-1"),
        ("s1-ramp", False, "20261007-100500-s1-ramp-1"),
        ("smoke-ramp", True, "20261007-101000-smoke-ramp-1"),
        ("s1-ramp", True, "20261007-102000-s1-ramp-1"),
        ("s1-ramp", False, "20261007-102500-s1-ramp-1"),
        ("s3-busy-user", True, "20261007-110000-s3-busy-user-1"),
        ("s3-busy-user", False, "20261007-110500-s3-busy-user-1"),
        ("hflt4-faults", False, "20261007-111000-hflt4-faults-9"),
        ("s3-busy-user", False, "20261007-111500-s3-busy-user-1"),
        ("s3-busy-user", False, "20261007-112000-s3-busy-user-1"),
        ("s6-down-closed", False, "20261007-113000-s6-down-closed-1"),
        ("s6-down-closed", False, "20261007-113500-s6-down-closed-1"),
        ("s7-long", True, "20261007-120000-s7-long-1"),
        ("s6-down-closedx", False, "20261007-121000-s6-down-closedx-1"),
        ("smoke-spike", False, "not a run ID"),
    ]
    index.write_text(
        "".join(
            json.dumps({"scenario": s, "baseline": b, "run_id": r}) + "\n" for s, b, r in lines
        ),
        encoding="utf-8",
    )
    want = {
        "scenarios": {
            "s1-ramp": {
                "baseline": "20261007-102000-s1-ramp-1",
                "limiter": "20261007-102500-s1-ramp-1",
            },
            "s3-busy-user": {
                "baseline": "20261007-110000-s3-busy-user-1",
                "limiter": "20261007-111500-s3-busy-user-1",
            },
            "s6-down-closed": {"baseline": None, "limiter": "20261007-113500-s6-down-closed-1"},
            "s7-long": {"baseline": "20261007-120000-s7-long-1", "limiter": None},
        },
        "s3-busy-user-rerun": "20261007-112000-s3-busy-user-1",
    }
    out, err = io.StringIO(), io.StringIO()
    assert completion.main(["--build-manifest", str(index)], out, err) == 0, err.getvalue()
    assert json.loads(out.getvalue()) == want and err.getvalue() == ""
    assert completion.build_manifest(index).to_json() == want


def test_build_manifest_one_s3_run(tmp_path: Path) -> None:
    """With one s3-busy-user limiter run, it is the limiter run and there is no rerun (null)."""
    index = tmp_path.joinpath("index.jsonl")
    index.write_text(
        json.dumps({"scenario": S3, "baseline": False, "run_id": IDS["s3"]}) + "\n",
        encoding="utf-8",
    )
    assert completion.build_manifest(index).to_json() == {
        "scenarios": {S3: {"baseline": None, "limiter": IDS["s3"]}},
        "s3-busy-user-rerun": None,
    }


@pytest.mark.parametrize(
    ("case", "text", "cause"),
    [
        ("no index", None, "cannot be read"),
        ("not JSON", "{\n", "not JSON"),
        (
            "an unknown key",
            '{"scenario":"s1-ramp","baseline":true,"run_id":"x","extra":1}\n',
            "extra",
        ),
        (
            "a run ID of another scenario",
            '{"scenario":"s1-ramp","baseline":true,"run_id":"20261007-100000-s2-users-100-1"}\n',
            "not a run ID of s1-ramp",
        ),
    ],
)
def test_build_manifest_bad_index(tmp_path: Path, case: str, text: str | None, cause: str) -> None:
    """An index that cannot be read or holds a malformed line: exit 1 with a message naming it,
    never a traceback, and nothing on stdout."""
    index = tmp_path.joinpath("index.jsonl")
    if text is not None:
        index.write_text(text, encoding="utf-8")
    out, err = io.StringIO(), io.StringIO()
    assert completion.main(["--build-manifest", str(index)], out, err) == 1, case
    assert out.getvalue() == "" and cause in err.getvalue(), (case, err.getvalue())


# The clean set, and the entries completion ignores.


def test_clean_manifest_exits_0(tmp_path: Path) -> None:
    """The clean full set holds every check: every H6 scenario listed with its limiter run, valid
    reports that validate.py passes, each benchmark limiter run's baseline on its schedule (the S6
    test runs need none), correctness pass, both failure modes with ERR-9 and ERR-16 counted apart,
    every covered release confirmed in time (an open run's degraded release has no receipt and
    needs none), and the rerun on the limiter run's schedule."""
    world = full_world(tmp_path)
    code, lines = check(world.manifest())
    assert code == 0, lines
    manifest = world.root.joinpath("runs", "h6-manifest.json")
    assert lines == [
        f"completion: {manifest}: every check holds; 27 runs; 8 releases: 8 confirmed in time, "
        "excluded killed 0, drained 0, after_p 0"
    ]


def test_non_h6_entries_ignored(tmp_path: Path) -> None:
    """Index entries of other scenarios, with no run directory or report, are left out of the
    manifest, so completion does not fail on them."""
    world = full_world(tmp_path)
    world.add("smoke-ramp", True, "20261007-090000-smoke-ramp-1")
    world.add("hflt4-faults", False, "20261007-090500-hflt4-faults-9")
    world.add("s6-down-closedx", False, "20261007-091000-s6-down-closedx-1")
    manifest = world.manifest()
    doc = json.loads(manifest.read_text(encoding="utf-8"))
    assert list(doc["scenarios"]) == list(H6)
    assert check(manifest)[0] == 0


# Check 0: every H6 scenario listed with its limiter run.


def test_unlisted_scenarios_fail(tmp_path: Path) -> None:
    """Each H6 scenario the manifest lacks fails, naming it (a run that failed is never indexed,
    so its scenario has no entry): from the full set, a benchmark scenario and an S6 one taken out;
    and make_world's set, which lists 3 of the 17, fails each of the other 14 in plan order."""
    world = full_world(tmp_path)

    def take_out(doc: dict[str, object]) -> None:
        scenarios = doc["scenarios"]
        assert isinstance(scenarios, dict)
        del scenarios["s5-retry"], scenarios["s6-state-open"]

    code, lines = check(world.manifest(take_out))
    assert code == 1
    assert [line for line in lines if line.startswith("FAIL ")] == [
        "FAIL s5-retry - listed: not in the manifest: no baseline or limiter run of it (the "
        "orchestrator indexes only a run that finished without failure)",
        "FAIL s6-state-open - listed: not in the manifest: no limiter run of it (the orchestrator "
        "indexes only a run that finished without failure)",
    ], lines

    world = make_world(tmp_path.joinpath("five"))
    code, lines = check(world.manifest())
    assert code == 1
    assert failures(lines) == [
        ("s1-ramp", "-", "listed"),
        ("s1-ramp-1api", "-", "listed"),
        ("s2-users-100", "-", "listed"),
        ("s2-users-1000", "-", "listed"),
        ("s2-users-10000", "-", "listed"),
        ("s4-spike", "-", "listed"),
        ("s5-retry", "-", "listed"),
        ("s6-pause-closed", "-", "listed"),
        ("s6-pause-open", "-", "listed"),
        ("s6-state-closed", "-", "listed"),
        ("s6-state-open", "-", "listed"),
        ("s6-capacity-closed", "-", "listed"),
        ("s6-capacity-open", "-", "listed"),
        ("s7-long", "-", "listed"),
    ], lines
    assert lines[-1].endswith(
        ": 14 failed; 5 runs; 2 releases: 2 confirmed in time, excluded killed 0, drained 0, "
        "after_p 0"
    ), lines


def test_baseline_without_limiter_run(tmp_path: Path) -> None:
    """A scenario whose index has a baseline line but no limiter line (its limiter run failed
    after a valid baseline) gets "limiter": null from --build-manifest; completion reads that
    manifest, fails the scenario as having no limiter run, and checks every other listed run: its
    baseline, and the open run whose report is invalid."""
    world = make_world(tmp_path)
    base = bench_run(world, other_id("s5-retry", "baseline"), baseline=True, name="s5-retry")
    edit_json(world.run("open").joinpath("report.json"), "evidence_validity.valid", False)
    manifest = world.manifest()
    doc = json.loads(manifest.read_text(encoding="utf-8"))
    assert doc["scenarios"]["s5-retry"] == {"baseline": base.name, "limiter": None}
    code, lines = check(manifest)
    assert code == 1
    assert failed(lines) == [(OPEN, IDS["open"], "evidence_valid"), (OPEN, IDS["open"], "validate")]
    assert (
        f"FAIL s5-retry - listed: no limiter run in the manifest, only its baseline {base.name}"
        in lines
    ), lines
    assert ": 16 failed; 6 runs;" in lines[-1], lines


def test_bench_scenario_without_baseline(tmp_path: Path) -> None:
    """A benchmark scenario with no baseline in the manifest fails as one by its name (every H6
    scenario but the S6 ones), even when its limiter run fails on input; the S6 scenarios, test
    runs, need none (the clean full set lists none)."""
    world = full_world(tmp_path)
    limiter = other_id("s1-ramp", "limiter")
    world.root.joinpath("runs", limiter, "report.json").unlink()

    def no_baseline(doc: dict[str, object]) -> None:
        edit_entry(doc, "s1-ramp", "baseline", None)

    code, lines = check(world.manifest(no_baseline))
    assert code == 1
    assert failures(lines) == [("s1-ramp", limiter, "input"), ("s1-ramp", limiter, "baseline")]
    assert (
        f"FAIL s1-ramp {limiter} baseline: no baseline run in the manifest: a benchmark limiter "
        "run needs one (DESIGN §B9)" in lines
    ), lines


# Plan step 4's checks, each failing on its own.


def test_invalid_report(tmp_path: Path) -> None:
    """A listed report with evidence_validity.valid false fails, and validate.py fails it too."""
    world = make_world(tmp_path)
    edit_json(world.run("open").joinpath("report.json"), "evidence_validity.valid", False)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(OPEN, IDS["open"], "evidence_valid"), (OPEN, IDS["open"], "validate")]
    assert "FAIL evidence_validity.valid: false: want true" in rest(lines)[1], lines


def test_validate_failing(tmp_path: Path) -> None:
    """A report validate.py fails (here it lacks timing) fails completion, though its
    evidence_validity.valid is still true; validate.py runs in baseline mode on a baseline."""
    world = make_world(tmp_path)
    report = world.run("s3").joinpath("report.json")
    doc = json.loads(report.read_text(encoding="utf-8"))
    del doc["timing"]
    report.write_text(json.dumps(doc) + "\n", encoding="utf-8")
    base = world.run("s3-base").joinpath("report.json")
    doc = json.loads(base.read_text(encoding="utf-8"))
    del doc["load"]
    base.write_text(json.dumps(doc) + "\n", encoding="utf-8")
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(S3, IDS["s3-base"], "validate"), (S3, IDS["s3"], "validate")]
    assert "baseline mode: FAIL required_keys: report.json has no load" in rest(lines)[0], lines
    assert "full mode: FAIL required_keys: report.json has no timing" in rest(lines)[1], lines


def test_missing_baseline(tmp_path: Path) -> None:
    """A benchmark limiter run with no baseline in the manifest fails."""
    world = make_world(tmp_path)

    def no_baseline(doc: dict[str, object]) -> None:
        edit_entry(doc, S3, "baseline", None)

    code, lines = check(world.manifest(no_baseline))
    assert code == 1
    assert failed(lines) == [(S3, IDS["s3"], "baseline")], lines


def test_schedule_mismatch(tmp_path: Path) -> None:
    """A benchmark limiter run whose manifest baseline has another schedule_sha256 fails."""
    world = make_world(tmp_path)
    other = bench_run(world, IDS["s3-base2"], baseline=True, planned_s=(6, 10, 16))

    def other_baseline(doc: dict[str, object]) -> None:
        edit_entry(doc, S3, "baseline", other.name)

    code, lines = check(world.manifest(other_baseline))
    assert code == 1
    assert failed(lines) == [(S3, IDS["s3"], "baseline_schedule")], lines


def test_schedule_missing(tmp_path: Path) -> None:
    """A benchmark limiter run and its baseline that both record no schedule_sha256 (null) fail
    baseline_schedule: equal nulls are no schedule match (validate.py fails both runs and the
    rerun too, and the rerun no longer matches the limiter run's schedule)."""
    world = make_world(tmp_path)
    for key in ("s3-base", "s3"):
        update_context(world.run(key), schedule_sha256=None)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [
        (S3, IDS["s3-base"], "validate"),
        (S3, IDS["s3"], "validate"),
        ("s3-busy-user-rerun", IDS["s3-rerun"], "validate"),
        (S3, IDS["s3"], "baseline_schedule"),
        ("s3-busy-user-rerun", IDS["s3-rerun"], "rerun_schedule"),
    ], lines
    assert rest(lines)[3] == (
        f"FAIL {S3} {IDS['s3']} baseline_schedule: schedule_sha256 None, baseline "
        f"{IDS['s3-base']} None"
    ), lines


def edit_entry(doc: dict[str, object], scenario: str, role: str, value: object) -> None:
    scenarios = doc["scenarios"]
    assert isinstance(scenarios, dict)
    scenarios[scenario][role] = value


def test_correctness_not_pass(tmp_path: Path) -> None:
    """A limiter run whose BUD-11 verdict is violation (21 admissions at once, capacity 20) has a
    valid report, but fails completion: an S6 run, and the rerun (a limiter run too)."""
    world = make_world(tmp_path, closed=s6_lines("closed", violation=True))
    report = json.loads(world.run("closed").joinpath("report.json").read_text(encoding="utf-8"))
    assert report["correctness"]["verdict"] == "violation"
    assert report["evidence_validity"]["valid"] is True
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "correctness")], lines

    world = make_world(tmp_path.joinpath("rerun"), rerun_extra=violation_lines())
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [("s3-busy-user-rerun", IDS["s3-rerun"], "correctness")], lines


@pytest.mark.parametrize("dropped", [CLOSED, OPEN])
def test_s6_lacks_failure_mode(tmp_path: Path, dropped: str) -> None:
    """S6 runs in one failure mode only fail (beside check 0 naming the scenario dropped); the
    missing mode's ERR-9 and ERR-16 are then not judged, and the benchmark runs (closed mode) do
    not stand in for S6 runs."""
    world = make_world(tmp_path)

    def drop(doc: dict[str, object]) -> None:
        scenarios = doc["scenarios"]
        assert isinstance(scenarios, dict)
        del scenarios[dropped]

    code, lines = check(world.manifest(drop))
    assert code == 1
    kept = [name for name in WORLD if name != dropped]
    assert failed(lines, kept) == [("s6", "-", "s6_failure_modes")], lines
    mode = "closed" if dropped == CLOSED else "open"
    assert rest(lines, kept) == [f"FAIL s6 - s6_failure_modes: no S6 limiter run in {mode} mode"]
    assert skips(lines) == [], lines


def test_mode_counts_summed(tmp_path: Path) -> None:
    """A failure mode's ERR-9 and ERR-16 are summed over its S6 runs: one open run with only
    ERR-9 and another with only ERR-16 show both."""
    no_err9 = [line for line in s6_lines("open") if line.get("request_id") != "q-err9"]
    world = make_world(tmp_path, open_=s6_lines("open", err16=False))
    s6_run(world, other_id("s6-pause-open", "limiter"), "s6-pause-open", "open", {1: no_err9})
    holds(world.manifest(), (*WORLD, "s6-pause-open"))


# report.json edits that fold an unknown outcome into ERR-8 or ERR-3, in by_err or in prf2.
FOLDS: list[tuple[str, str, dict[str, int]]] = [
    ("ERR-9 counted as ERR-8", "closed", {"by_err.ERR-9": 0, "by_err.ERR-8": 2}),
    ("ERR-16 counted as ERR-8", "closed", {"by_err.ERR-16": 0, "by_err.ERR-8": 2}),
    ("ERR-9 counted as ERR-3", "open", {"by_err.ERR-9": 0, "by_err.ERR-3": 2}),
    (
        "ERR-16 counted as ERR-3 in prf2",
        "open",
        {"prf2.unknown_application_outcomes": 0, "prf2.degraded_admissions": 2},
    ),
    (
        "ERR-9 counted as unavailable in prf2",
        "closed",
        {"prf2.unknown_acquires": 0, "prf2.unavailable": 2},
    ),
]


@pytest.mark.parametrize(("case", "key", "edits"), FOLDS, ids=[c for c, _, _ in FOLDS])
def test_unknowns_not_apart(tmp_path: Path, case: str, key: str, edits: dict[str, int]) -> None:
    """An S6 report that counts an unknown outcome (ERR-9, ERR-16) as ERR-8 or ERR-3, apart from
    what its request lines give, fails."""
    world = make_world(tmp_path)
    report = world.run(key).joinpath("report.json")
    for dotted, value in edits.items():
        edit_json(report, f"results.{dotted}", value)
    code, lines = check(world.manifest())
    assert code == 1, case
    name = CLOSED if key == "closed" else OPEN
    assert failed(lines) == [(name, IDS[key], "s6_unknowns_apart")], (case, lines)


@pytest.mark.parametrize("value", [True, 1.0])
def test_unknown_count_not_an_integer(tmp_path: Path, value: object) -> None:
    """An S6 report whose ERR-9 count equals the run logs' 1 but is not an integer (true, 1.0)
    does not count it as the request lines give it; validate.py fails such a report too, naming
    required_keys."""
    world = make_world(tmp_path)
    edit_json(world.run("closed").joinpath("report.json"), "results.by_err.ERR-9", value)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [
        (CLOSED, IDS["closed"], "validate"),
        (CLOSED, IDS["closed"], "s6_unknowns_apart"),
    ], lines
    shown = "true" if value is True else "1.0"
    found = rest(lines)
    assert f"FAIL required_keys: results.by_err.ERR-9: {shown}: want an integer" in found[0], lines
    assert found[1].endswith(f": results.by_err.ERR-9 {shown}, the run logs 1"), lines


@pytest.mark.parametrize("mode", ["closed", "open"])
@pytest.mark.parametrize("missing", ["ERR-9", "ERR-16"])
def test_mode_without_unknown(tmp_path: Path, mode: str, missing: str) -> None:
    """A failure mode whose S6 runs have no ERR-9, or no ERR-16, fails: nothing shows it counted
    apart there."""
    run_log = s6_lines(mode, err16=missing != "ERR-16")
    if missing == "ERR-9":
        run_log = [line for line in run_log if line.get("request_id") != "q-err9"]
    if mode == "closed":
        world = make_world(tmp_path, closed=run_log)
    else:
        world = make_world(tmp_path, open_=run_log)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [("s6", "-", "s6_unknowns_apart")], lines
    assert f"no {missing} in any S6 run in {mode} mode" in rest(lines)[0], lines


# Refund completion: (case, the closed S6 run's extra release lines, whether completion holds).
# Every release is within P (limiter_ns 2 ms + db_ns 0) unless the case says otherwise.
REFUNDS: list[tuple[str, list[Line], bool]] = [
    ("confirmed in time", released("d1", final_event("d1")), True),
    ("already_applied in time", released("d1", final_event("d1", "already_applied")), True),
    (
        "confirmed 1 µs before its expiry",
        released("d1", final_event("d1", credit_ns=DECIDED + LIFETIME_NS - US)),
        True,
    ),
    (
        "confirmed at refund_deadline + the measured outage",
        released("d1", final_event("d1", took_ns=DEADLINE_NS + OUTAGE_NS)),
        True,
    ),
    (
        "confirmed 1 µs later than refund_deadline + the measured outage",
        released("d1", final_event("d1", took_ns=DEADLINE_NS + OUTAGE_NS + US)),
        False,
    ),
    (
        "confirmed 1 ns later than refund_deadline + the measured outage",
        released("d1", final_event("d1", took_ns=DEADLINE_NS + OUTAGE_NS + 1)),
        False,
    ),
    (
        "unconfirmed: unresolved (retention)",
        released("d1", final_event("d1", "unresolved", reason="retention")),
        False,
    ),
    (
        "unconfirmed: unresolved (expired)",
        released("d1", final_event("d1", "unresolved", reason="expired")),
        False,
    ),
    ("unconfirmed: receipt_expired", released("d1", final_event("d1", "receipt_expired")), False),
    ("unconfirmed: receipt_invalid", released("d1", final_event("d1", "receipt_invalid")), False),
    (
        "cut by a drain timeout right after its release: unresolved (closed)",
        released("d1", final_event("d1", "unresolved", reason="closed")),
        True,
    ),
    (
        "cut by a drain timeout at refund_deadline + the measured outage",
        released(
            "d1", final_event("d1", "unresolved", reason="closed", took_ns=DEADLINE_NS + OUTAGE_NS)
        ),
        True,
    ),
    (
        "cut by a drain timeout 1 ns later than refund_deadline + the measured outage",
        released(
            "d1",
            final_event("d1", "unresolved", reason="closed", took_ns=DEADLINE_NS + OUTAGE_NS + 1),
        ),
        False,
    ),
    (
        "released 1 ns after P, cut by a drain timeout past its deadline",
        released(
            "d1",
            final_event("d1", "unresolved", reason="closed", took_ns=DEADLINE_NS + OUTAGE_NS + 1),
            elapsed=(2 * MS, P_NS - 2 * MS + 1),
        ),
        True,
    ),
    (
        "released 1 ns after P, unconfirmed",
        released(
            "d1",
            final_event("d1", "unresolved", reason="retention"),
            elapsed=(2 * MS, P_NS - 2 * MS + 1),
        ),
        True,
    ),
    (
        "released at P, unconfirmed",
        released(
            "d1",
            final_event("d1", "unresolved", reason="retention"),
            elapsed=(2 * MS, P_NS - 2 * MS),
        ),
        False,
    ),
    (
        "released 1 ns after P, confirmed late",
        released(
            "d1",
            final_event("d1", took_ns=DEADLINE_NS + OUTAGE_NS + US),
            elapsed=(P_NS - MS, MS + 1),
        ),
        True,
    ),
    (
        "released at P, confirmed late",
        released(
            "d1", final_event("d1", took_ns=DEADLINE_NS + OUTAGE_NS + US), elapsed=(P_NS - MS, MS)
        ),
        False,
    ),
    (
        "no request line, unconfirmed",
        released("d1", final_event("d1", "unresolved", reason="retention"), with_request=False),
        False,
    ),
    (
        "no request line, confirmed in time",
        released("d1", final_event("d1"), with_request=False),
        True,
    ),
]


@pytest.mark.parametrize(("case", "extra", "ok"), REFUNDS, ids=[c for c, _, _ in REFUNDS])
def test_refund_completion(tmp_path: Path, case: str, extra: list[Line], ok: bool) -> None:
    """Each release covered by REF-5/REF-9 (released within P of just before its acquire, not on
    an instance killed before its final event, not cut by a drain timeout before its deadline)
    must be confirmed (applied, already_applied) before its expiry (decision_ns + receipt_lifetime
    60 s) and at most refund_deadline (30 s) + the run's measured outage after its release; one
    released after P, or cut by a drain timeout within that bound (SPEC REF-9 (c): the default
    drain covers the rest), need not be."""
    world = make_world(tmp_path, closed=s6_lines("closed", extra))
    if ok:
        holds(world.manifest())
        return
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert "deduction d1" in rest(lines)[0], lines


def test_drain_past_deadline_detail(tmp_path: Path) -> None:
    """A refund cut by a drain timeout later than refund_deadline + the measured outage after its
    release fails as a REF-5 miss, saying how late it came."""
    late = DEADLINE_NS + OUTAGE_NS + 1
    world = make_world(
        tmp_path,
        closed=s6_lines(
            "closed", released("d1", final_event("d1", "unresolved", reason="closed", took_ns=late))
        ),
    )
    code, lines = check(world.manifest())
    assert code == 1
    assert rest(lines) == [
        f"FAIL {CLOSED} {IDS['closed']} refund_completion: api-1.jsonl: line 14: deduction d1: "
        "released 2000000 ns after just before its acquire (limiter_ns + db_ns, within P "
        f"5000000000): unresolved (closed) {late} ns after its release (final_mono_ns − "
        "release_mono_ns), above refund_deadline_ns 30000000000 + the measured outage "
        f"4085364542 ns = {late - 1}: its instance drained past its deadline, so REF-9 (c) still "
        "covered it (REF-5)"
    ], lines


def test_refunds_of_every_run(tmp_path: Path) -> None:
    """Refund completion covers every listed run, not only the S6 ones: an unconfirmed release
    in the rerun (a benchmark run) fails."""
    unconfirmed = released("d9", final_event("d9", "unresolved", reason="retention"))
    world = make_world(tmp_path, rerun_extra=unconfirmed)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [("s3-busy-user-rerun", IDS["s3-rerun"], "refund_completion")], lines
    assert "deduction d9" in rest(lines)[0], lines


def test_every_instance_scanned(tmp_path: Path) -> None:
    """Refund completion reads every instance's run log: in a three-instance S6 run, a covered
    release on instance 2 that was never confirmed fails."""
    unconfirmed = released(
        "d2", final_event("d2", "unresolved", reason="retention"), instance=2, user="u000006"
    )
    apis = {
        1: s6_lines("closed"),
        2: [lifecycle_start(), lifecycle("ready"), *unconfirmed, lifecycle("stop")],
        3: [lifecycle_start(), lifecycle("ready"), lifecycle("stop")],
    }
    world = make_world(tmp_path, closed_apis=apis)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert ": api-2.jsonl: line 4: deduction d2: " in rest(lines)[0], lines


def test_refund_bound_counts_the_run_outage(tmp_path: Path) -> None:
    """The bound is refund_deadline + this run's measured outage: 30 s + 1 µs after its release
    holds in the closed run (a 4.09 s store outage measured) and fails in the open run, whose
    outage segment (a wrapper refusal) measured none."""
    late = released("d1", final_event("d1", took_ns=DEADLINE_NS + US))
    world = make_world(tmp_path, closed=s6_lines("closed", late), open_=s6_lines("open", late))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(OPEN, IDS["open"], "refund_completion")], lines
    assert "above refund_deadline_ns 30000000000 + the measured outage 0 ns" in rest(lines)[0]


# A second store outage segment in the closed run: a store pause after its store down.
PAUSE = {
    **STORE_DOWN,
    "at_s": 15,
    "cmd": "faultctl store pause 4.5s",
    "start_ns": T0_WALL_NS + 15 * SECOND,
    "end_ns": T0_WALL_NS + 19_600 * MS,
    "outage_start_ns": T0_WALL_NS + 15 * SECOND,
    "outage_ns": 3 * SECOND,
}
# A store fault that arms no outage segment: dropped refund replies.
DROP_REPLY = {**REFUSE, "cmd": '{"target":"store","mode":"drop_reply","match":"refund","count":5}'}
# A fault that arms no store outage segment by its command but records a measured outage: an api
# kill with an outage_ns (1 s), which counts as an outage segment all the same.
API_KILL = {
    "at_s": 15,
    "cmd": "faultctl api kill 1 --restart",
    "instance": 1,
    "start_ns": T0_WALL_NS + 15 * SECOND,
    "end_ns": T0_WALL_NS + 15_140 * MS,
    "outage_start_ns": None,
    "outage_ns": SECOND,
    "targets": [],
}
TOO_LONG = "longer than [policy] outage_tolerance_ns 5000000000 (REF-5)"
# Outage evidence the REF-5 bound cannot take (orchestrator decision R3 of the H6 slice review):
# (case, the closed run's context.json faults, its input failure's detail).
OUTAGE_ERRORS: list[tuple[str, list[dict[str, object]], str]] = [
    (
        "two measured store outages",
        [STORE_DOWN, PAUSE],
        "context.json: field faults: 2 store outage segments (faults[0] faultctl store down 4s; "
        "faults[1] faultctl store pause 4.5s): REF-5 allows one, so refund_deadline_ns + the "
        "measured outage bounds no refund",
    ),
    (
        "a store down and a timed store refusal",
        [STORE_DOWN, REFUSE],
        "context.json: field faults: 2 store outage segments (faults[0] faultctl store down 4s; "
        f"faults[1] {REFUSE['cmd']}): REF-5 allows one, so refund_deadline_ns + the measured "
        "outage bounds no refund",
    ),
    (
        "a store down and an api kill with an outage_ns",
        [STORE_DOWN, API_KILL],
        "context.json: field faults: 2 store outage segments (faults[0] faultctl store down 4s; "
        "faults[1] faultctl api kill 1 --restart): REF-5 allows one, so refund_deadline_ns + the "
        "measured outage bounds no refund",
    ),
    (
        "a measured outage 1 ns longer than outage_tolerance",
        [{**STORE_DOWN, "outage_ns": TOLERANCE_NS + 1}],
        f"context.json: faults[0]: field outage_ns: 5000000001: a store outage {TOO_LONG}",
    ),
    (
        "a timed store refusal 1 ms longer than outage_tolerance",
        [{**REFUSE, "cmd": str(REFUSE["cmd"]).replace("5000", "5001")}],
        f"context.json: faults[0]: field cmd: a store refusal of duration_ms 5001, {TOO_LONG}",
    ),
]


@pytest.mark.parametrize(
    ("case", "faults", "detail"), OUTAGE_ERRORS, ids=[c for c, _, _ in OUTAGE_ERRORS]
)
def test_outage_beyond_ref5(
    tmp_path: Path, case: str, faults: list[dict[str, object]], detail: str
) -> None:
    """A run whose context.json shows more than one store outage segment (a faultctl store down
    or pause, a timed store refusal, or any fault with an outage_ns), or one longer than its
    policy's outage_tolerance, fails as input, naming the run: REF-5 allows one outage of at most
    outage_tolerance, so the bound refund_deadline + the measured outage does not hold."""
    world = make_world(tmp_path, closed_faults=faults)
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    assert rest(lines)[0] == f"FAIL {CLOSED} {IDS['closed']} input: {detail}", (case, lines)


@pytest.mark.parametrize(
    ("case", "tolerance", "detail"),
    [
        (
            "a measured outage longer than the policy's outage_tolerance (4 s)",
            "outage_tolerance_ns = 4_000_000_000\n",
            "context.json: faults[0]: field outage_ns: 4085364542: a store outage longer than "
            "[policy] outage_tolerance_ns 4000000000 (REF-5)",
        ),
        (
            "no outage_tolerance_ns",
            "",
            "config.toml: [policy]: field outage_tolerance_ns: want an integer",
        ),
    ],
)
def test_outage_tolerance_from_config(
    tmp_path: Path, case: str, tolerance: str, detail: str
) -> None:
    """The tolerance is the run's config.toml [policy] outage_tolerance_ns, not a fixed 5 s: the
    closed run's 4.09 s store outage fails as input against 4 s, and a config.toml without it
    fails as input."""
    world = make_world(tmp_path)
    config = world.run("closed").joinpath("config.toml")
    text = config.read_text(encoding="utf-8")
    edited = text.replace("outage_tolerance_ns = 5_000_000_000\n", tolerance)
    assert edited != text
    config.write_text(edited, encoding="utf-8")
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    assert rest(lines)[0] == f"FAIL {CLOSED} {IDS['closed']} input: {detail}", (case, lines)


def test_outage_at_tolerance(tmp_path: Path) -> None:
    """One measured outage of exactly outage_tolerance is what REF-5 allows: the bound adds all of
    it (a refund confirmed refund_deadline + outage_tolerance after its release holds), and a
    store fault that arms no outage segment (dropped replies) beside it is not a second one."""
    late = released("d1", final_event("d1", took_ns=DEADLINE_NS + TOLERANCE_NS))
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", late),
        closed_faults=[{**STORE_DOWN, "outage_ns": TOLERANCE_NS}, DROP_REPLY],
    )
    holds(world.manifest())


def test_refund_at_expiry(tmp_path: Path) -> None:
    """A refund confirmed exactly at its expiry (budget time = decision_ns + receipt_lifetime)
    fails: it must come before it. (The store never applies one then, so the run log is changed
    after its report was built.)"""
    world = make_world(tmp_path, closed=s6_lines("closed", released("d1", final_event("d1"))))

    def at_expiry(lines: list[Line]) -> list[Line]:
        for line in lines:
            if line.get("kind") == "refund" and line["deduction_id"] == "d1":
                line["credit_decision_ns"] = DECIDED + LIFETIME_NS
        return lines

    rewrite_log(world.run("closed"), at_expiry)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert f"not before its expiry {DECIDED + LIFETIME_NS}" in rest(lines)[0], lines


def drop_final(deduction: str) -> Callable[[list[Line]], list[Line]]:
    """A rewrite_log change taking deduction's final event (its refund line) out."""

    def change(lines: list[Line]) -> list[Line]:
        return [
            line
            for line in lines
            if line.get("deduction_id") != deduction or line["kind"] != "refund"
        ]

    return change


def test_release_on_killed_instance(tmp_path: Path) -> None:
    """A release with no final event on an instance killed before it (its process segment ends
    with a pre_kill line, FLT-6) need not be confirmed; the same release in a segment that was
    not killed fails (here its final event is taken out of the log after the report was built);
    and a release whose final event came before the kill must still be confirmed in time."""
    world = make_world(tmp_path, closed=s6_lines("closed", killed=released("d1", None)))
    holds(world.manifest())

    world = make_world(
        tmp_path.joinpath("not-killed"),
        closed=s6_lines("closed", released("d1", final_event("d1"))),
    )
    rewrite_log(world.run("closed"), drop_final("d1"))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert "no final event, and its instance was not killed" in rest(lines)[0], lines

    late = released("d1", final_event("d1", took_ns=DEADLINE_NS + OUTAGE_NS + US))
    world = make_world(tmp_path.joinpath("late"), closed=s6_lines("closed", killed=late))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines


def test_restarted_after_kill(tmp_path: Path) -> None:
    """The kill exclusion ends with the killed process: an instance killed (FLT-6) and restarted
    starts a new process segment, where a covered release with no final event fails (its final
    event is taken out after the report was built) while the killed segment's is excluded."""
    world = make_world(
        tmp_path,
        closed=s6_lines(
            "closed", released("d2", final_event("d2")), killed=released("d1", None, user="u000007")
        ),
    )
    rewrite_log(world.run("closed"), drop_final("d2"))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert "deduction d2: " in rest(lines)[0], lines
    assert "no final event, and its instance was not killed" in rest(lines)[0], lines
    assert ": 15 failed; 5 runs; 4 releases: 2 confirmed in time, excluded killed 1," in lines[-1]


@pytest.mark.parametrize(
    ("result", "reason"), [("unresolved", "retention"), ("receipt_invalid", "")]
)
def test_final_event_before_kill(tmp_path: Path, result: str, reason: str) -> None:
    """A release whose final event came before its instance was killed is judged by that event:
    unconfirmed (unresolved (retention), receipt_invalid), it fails though the segment ends with
    a pre_kill line."""
    final = final_event("d1", result, reason=reason)
    world = make_world(tmp_path, closed=s6_lines("closed", killed=released("d1", final)))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    shown = f"{result} ({reason})" if reason else result
    assert "deduction d1: " in rest(lines)[0], lines
    assert f"final event {shown}: not confirmed" in rest(lines)[0], lines


# The kill exclusion's bound (orchestrator decision D5 of the H6 slice review, SPEC REF-9 (c)): a
# killed process's release with no final event is excused only when the kill time the run log
# gives on that process's monotonic clock, the largest release_mono_ns its segment holds (in
# whatever order its lines were written), is at most refund_deadline + the run's measured outage
# after the release.
BOUND_NS = DEADLINE_NS + OUTAGE_NS


def degraded_release(release_ns: int) -> list[Line]:
    """A degraded permit refused after its acquire: its decision and its release (no
    deduction_id, no final event), the release at release_ns on hitapi's monotonic clock."""
    return [
        decision("", 0, "u000004", "degraded"),
        {"kind": "release", "deduction_id": "", "release_mono_ns": release_ns},
    ]


def later_release(release_ns: int) -> list[Line]:
    """Another permit of the same process released at release_ns, its refund applied in time."""
    return released("d3", final_event("d3"), user="u000008", release_ns=release_ns)


def killed_log(after_d1: list[Line], *, after_kill: Sequence[Line] = ()) -> list[Line]:
    """The closed run's log with a killed first segment: d1 released at RELEASE_LINE_NS with no
    final event, then after_d1, then its pre_kill line, then after_kill (lines the killed process
    wrote after it, before the restart's start line)."""
    lines = s6_lines("closed", killed=[*released("d1", None, user="u000007"), *after_d1])
    kill = lines.index(lifecycle("pre_kill"))
    return [*lines[: kill + 1], *after_kill, *lines[kill + 1 :]]


# (case, the closed run's log, whether d1 is excused).
KILL_BOUND: list[tuple[str, list[Line], bool]] = [
    (
        "killed at the bound after its release",
        killed_log(later_release(RELEASE_LINE_NS + BOUND_NS)),
        True,
    ),
    (
        "killed 1 ns past the bound",
        killed_log(later_release(RELEASE_LINE_NS + BOUND_NS + 1)),
        False,
    ),
    (
        "a degraded permit's release 1 ns past the bound",
        killed_log(degraded_release(RELEASE_LINE_NS + BOUND_NS + 1)),
        False,
    ),
    (
        "a release after the pre_kill line 1 ns past the bound",
        killed_log([], after_kill=degraded_release(RELEASE_LINE_NS + BOUND_NS + 1)),
        False,
    ),
    # Concurrent requests write their release lines out of mono order: the one written last is
    # not the latest, so only the largest release_mono_ns gives the kill time (orchestrator
    # decision W1 of the H6 round-2 review). With the last line's, 1 ns after d1's release, d1
    # would be excused.
    (
        "release lines out of mono order, a degraded permit's written last: the largest 1 ns "
        "past the bound",
        killed_log(
            [*later_release(RELEASE_LINE_NS + BOUND_NS + 1), *degraded_release(RELEASE_LINE_NS + 1)]
        ),
        False,
    ),
    (
        "release lines out of mono order, a permit's written last: the largest 1 ns past the bound",
        killed_log(
            [
                *later_release(RELEASE_LINE_NS + BOUND_NS + 1),
                *released("d4", final_event("d4"), user="u000009", release_ns=RELEASE_LINE_NS + 1),
            ]
        ),
        False,
    ),
    (
        "the restarted process's release 1 ns past the bound (another process's clock)",
        s6_lines(
            "closed",
            released("d2", final_event("d2"), release_ns=RELEASE_LINE_NS + BOUND_NS + 1),
            killed=released("d1", None, user="u000007"),
        ),
        True,
    ),
    (
        "an earlier process's release 1 ns past the bound (another process's clock)",
        [
            lifecycle_start(),
            lifecycle("ready"),
            *released("d2", final_event("d2"), release_ns=RELEASE_LINE_NS + BOUND_NS + 1),
            lifecycle("stop"),
            *s6_lines("closed", killed=released("d1", None, user="u000007")),
        ],
        True,
    ),
]


@pytest.mark.parametrize(("case", "log", "ok"), KILL_BOUND, ids=[c for c, _, _ in KILL_BOUND])
def test_kill_within_deadline(tmp_path: Path, case: str, log: list[Line], ok: bool) -> None:
    """A release with no final event on a killed instance is excused when the kill came at most
    refund_deadline + the measured outage after it: the kill time is the largest release_mono_ns
    its process segment holds, not the last one written (concurrent requests write them out of
    mono order), a degraded permit's and one written after the pre_kill line included (not
    another process's, before or after it, on another clock); 1 ns later, the instance kept
    running past the refund's deadline and REF-5 still covered it."""
    world = make_world(tmp_path, closed=log)
    code, lines = check(world.manifest())
    assert code == 1, case
    if ok:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        assert ", excluded killed 1, " in lines[-1], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert "deduction d1: " in rest(lines)[0], (case, lines)
    assert ", excluded killed 0, " in lines[-1], (case, lines)


def test_kill_past_deadline_detail(tmp_path: Path) -> None:
    """A release its killed process outlived by more than refund_deadline + the measured outage
    fails as a REF-5 miss, saying how long the process ran on after it."""
    late = RELEASE_LINE_NS + BOUND_NS + 1
    world = make_world(tmp_path, closed=killed_log(later_release(late)))
    code, lines = check(world.manifest())
    assert code == 1
    assert rest(lines) == [
        f"FAIL {CLOSED} {IDS['closed']} refund_completion: api-1.jsonl: line 4: deduction d1: "
        "released 2000000 ns after just before its acquire (limiter_ns + db_ns, within P "
        f"5000000000): no final event, and its instance ran on {BOUND_NS + 1} ns after its "
        f"release before it was killed (release_mono_ns {RELEASE_LINE_NS}, its process's last "
        f"{late}), above refund_deadline_ns 30000000000 + the measured outage 4085364542 ns = "
        f"{BOUND_NS}: it kept running past its deadline, so REF-9 (c) still covered it (REF-5)"
    ], lines


# The s3-busy-user rerun.


def test_rerun_schedule_differs(tmp_path: Path) -> None:
    """A rerun whose schedule_sha256 is not the limiter run's fails (its own baseline, on its
    schedule, keeps validate.py passing it)."""
    world = make_world(tmp_path, rerun_planned_s=(6, 10, 16))

    def keep_first_baseline(doc: dict[str, object]) -> None:
        edit_entry(doc, S3, "baseline", IDS["s3-base"])
        edit_entry(doc, S3, "limiter", IDS["s3"])

    code, lines = check(world.manifest(keep_first_baseline))
    assert code == 1
    assert failed(lines) == [("s3-busy-user-rerun", IDS["s3-rerun"], "rerun_schedule")], lines


def as_seed2(world: World, key: str) -> str:
    """Moves a run of world to a run ID with seed 2, its context.json saying seed 2 (on the same
    schedule); returns the new run ID."""
    old = world.run(key)
    new = old.with_name(old.name.removesuffix("-1") + "-2")
    old.rename(new)
    edit_json(new.joinpath("context.json"), "run_id", new.name)
    edit_json(new.joinpath("context.json"), "seed", 2)
    edit_json(new.joinpath("report.json"), "run_id", new.name)
    return new.name


@pytest.mark.parametrize("case", ["no rerun", "the rerun is the limiter run", "seed 2"])
def test_rerun_missing_or_not_one(tmp_path: Path, case: str) -> None:
    """No rerun in the manifest, a rerun that is the limiter run itself, or a rerun with a seed
    other than 1 (on the same schedule here) fails."""
    world = make_world(tmp_path)
    rerun: str | None = None
    if case == "the rerun is the limiter run":
        rerun = IDS["s3"]
    elif case == "seed 2":
        rerun = as_seed2(world, "s3-rerun")

    def set_rerun(doc: dict[str, object]) -> None:
        doc["s3-busy-user-rerun"] = rerun

    code, lines = check(world.manifest(set_rerun))
    assert code == 1, case
    checks = failed(lines)
    assert checks[-1][2] == "rerun_schedule", (case, lines)
    if case == "no rerun":
        assert checks == [("s3-busy-user-rerun", "-", "rerun_schedule")], lines
    elif case == "seed 2":
        assert "the rerun has seed 2, not 1" in rest(lines)[-1], lines


def test_rerun_limiter_run_seed(tmp_path: Path) -> None:
    """The limiter run the rerun is matched against must have seed 1 too: one with seed 2 (on the
    rerun's schedule) fails the rerun check (validate.py fails it as well, its baseline being of
    seed 1)."""
    world = make_world(tmp_path)
    seed2 = as_seed2(world, "s3")

    def limiter_seed2(doc: dict[str, object]) -> None:
        edit_entry(doc, S3, "limiter", seed2)

    code, lines = check(world.manifest(limiter_seed2))
    assert code == 1
    assert failed(lines) == [
        (S3, seed2, "validate"),
        ("s3-busy-user-rerun", IDS["s3-rerun"], "rerun_schedule"),
    ], lines
    assert rest(lines)[-1] == (
        f"FAIL s3-busy-user-rerun {IDS['s3-rerun']} rerun_schedule: the limiter run has seed 2, "
        "not 1"
    ), lines


@pytest.mark.parametrize("case", ["no s3-busy-user entry", "s3-busy-user with only its baseline"])
def test_rerun_without_limiter_run(tmp_path: Path, case: str) -> None:
    """A rerun with no s3-busy-user limiter run in the manifest to match fails the rerun check
    (and check 0 names s3-busy-user), whether the scenario has no entry or only its baseline."""
    world = make_world(tmp_path)

    def change(doc: dict[str, object]) -> None:
        if case == "no s3-busy-user entry":
            scenarios = doc["scenarios"]
            assert isinstance(scenarios, dict)
            del scenarios[S3]
        else:
            edit_entry(doc, S3, "limiter", None)

    code, lines = check(world.manifest(change))
    assert code == 1
    assert failed(lines, (CLOSED, OPEN)) == [
        ("s3-busy-user-rerun", IDS["s3-rerun"], "rerun_schedule")
    ], (case, lines)
    assert rest(lines, (CLOSED, OPEN))[0].endswith(": no s3-busy-user limiter run to match"), lines


# Input errors: failures with a message, never a traceback.


def edited(text: str, change: Callable[[dict[str, object]], None]) -> str:
    """The JSON document text, changed."""
    doc = json.loads(text)
    change(doc)
    return json.dumps(doc)


def test_manifest_input_errors(tmp_path: Path) -> None:
    """An unreadable or malformed manifest fails with a message naming what is wrong."""
    world = make_world(tmp_path)
    path = world.manifest()
    good = path.read_text(encoding="utf-8")

    def entry_key(doc: dict[str, object]) -> None:
        edit_entry(doc, CLOSED, "extra", 1)

    def rerun_of_another(doc: dict[str, object]) -> None:
        doc["s3-busy-user-rerun"] = IDS["closed"]

    def run_id_suffix(doc: dict[str, object]) -> None:
        edit_entry(doc, CLOSED, "limiter", IDS["closed"] + "/../x")

    cases: list[tuple[str, str | None, str]] = [
        ("no manifest", None, "cannot be read"),
        ("not JSON", "[", "not JSON"),
        ("an unknown key", good.replace('"scenarios"', '"extra": 1, "scenarios"', 1), "extra"),
        (
            "a scenario that is not an H6 one",
            good.replace(
                '"scenarios": {',
                '"scenarios": {"smoke-ramp": {"baseline": null, "limiter": '
                '"20261007-090000-smoke-ramp-1"}, ',
                1,
            ),
            "smoke-ramp",
        ),
        (
            "an entry with an unknown key",
            edited(good, entry_key),
            f"h6-manifest.json: scenarios.{CLOSED}: field extra: unknown field",
        ),
        (
            "a run ID of another scenario",
            good.replace(IDS["closed"], IDS["open"], 1),
            "not a run ID",
        ),
        (
            "a rerun ID of another scenario",
            edited(good, rerun_of_another),
            f"field s3-busy-user-rerun: '{IDS['closed']}': not a run ID of s3-busy-user",
        ),
        (
            "a run ID with more after it",
            edited(good, run_id_suffix),
            f"field limiter: '{IDS['closed']}/../x': not a run ID of {CLOSED}",
        ),
    ]
    for case, text, cause in cases:
        if text is None:
            path.unlink()
        else:
            path.write_text(text, encoding="utf-8")
        code, lines = check(path)
        assert code == 1, case
        assert failures(lines) == [("-", "-", "manifest")], (case, lines)
        assert cause in lines[0], (case, lines)


@pytest.mark.parametrize("field", ["run_id", "scenario.name"])
def test_run_not_the_listed_one(tmp_path: Path, field: str) -> None:
    """A listed directory holding another run (its context.json run_id another run's), or a run of
    another scenario under a run ID of the listed one, fails as input, naming the field."""
    world = make_world(tmp_path)
    copy = world.root.joinpath("runs", "20261007-105000-s6-pause-open-1")
    copy.mkdir()
    for item in world.run("open").iterdir():
        copy.joinpath(item.name).write_bytes(item.read_bytes())
    if field == "scenario.name":
        edit_json(copy.joinpath("context.json"), "run_id", copy.name)

    def listed(doc: dict[str, object]) -> None:
        scenarios = doc["scenarios"]
        assert isinstance(scenarios, dict)
        scenarios["s6-pause-open"] = {"baseline": None, "limiter": copy.name}

    code, lines = check(world.manifest(listed))
    assert code == 1
    with_copy = (*WORLD, "s6-pause-open")
    assert failed(lines, with_copy) == [("s6-pause-open", copy.name, "input")], lines
    assert f"context.json: field {field}:" in rest(lines, with_copy)[0], lines


def test_run_input_errors(tmp_path: Path) -> None:
    """A listed run whose report.json is missing, whose run log holds a malformed line, or which
    is not the run the manifest says (a baseline listed as a limiter run) fails with a message
    naming it, and takes no further check."""
    world = make_world(tmp_path)
    world.run("open").joinpath("report.json").unlink()
    path = world.run("closed").joinpath("api-1.jsonl")
    path.write_text(path.read_text(encoding="utf-8") + "{not json\n", encoding="utf-8")

    def base_as_limiter(doc: dict[str, object]) -> None:
        edit_entry(doc, S3, "limiter", IDS["s3-base"])

    code, lines = check(world.manifest(base_as_limiter))
    assert code == 1
    assert failed(lines) == [
        (S3, IDS["s3-base"], "input"),
        (CLOSED, IDS["closed"], "input"),
        (OPEN, IDS["open"], "input"),
    ], lines
    found = rest(lines)
    assert "the manifest lists" in found[0] and "api-1.jsonl: line" in found[1], lines
    assert "report.json" in found[2], lines
    assert not any("Traceback" in line for line in lines)


# Run-log lines appended to the closed run's api-1.jsonl after its report was built, and the
# input failure's detail.
LOG_ERRORS: list[tuple[str, list[Line], str]] = [
    (
        "a second release line",
        [{"kind": "release", "deduction_id": "d-ok", "release_mono_ns": 8 * SECOND}],
        "field deduction_id: d-ok: a second release line",
    ),
    (
        "a release with no allowed decision before it",
        [{"kind": "release", "deduction_id": "d-new", "release_mono_ns": 8 * SECOND}],
        "field deduction_id: d-new: no allowed decision line before it",
    ),
    (
        "a final event with no release before it",
        [final_event("d-new")],
        "field deduction_id: 'd-new': a final event with no release before it",
    ),
    (
        "a second final event",
        [final_event("d-ok", "unresolved", reason="retention"), final_event("d-ok")],
        "field deduction_id: d-ok: a second final event",
    ),
    (
        "a final event before its release",
        released("d-new", final_event("d-new", took_ns=-1)),
        "field final_mono_ns: before release_mono_ns",
    ),
]


@pytest.mark.parametrize(("case", "extra", "cause"), LOG_ERRORS, ids=[c for c, _, _ in LOG_ERRORS])
def test_run_log_input_errors(tmp_path: Path, case: str, extra: list[Line], cause: str) -> None:
    """A run log whose refund join is malformed (a release or final event repeated, one with
    nothing to join, a final event before its release) fails as input, naming the field."""
    world = make_world(tmp_path)
    rewrite_log(world.run("closed"), lambda lines: [*lines, *extra])
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    assert cause in rest(lines)[0], (case, lines)


def without(line: Line, key: str) -> Line:
    """line with key taken out."""
    return {k: v for k, v in line.items() if k != key}


A_REQUEST = request(1, "q-x", arrival_ns=T0_WALL_NS + 7 * SECOND, user="u000009")
A_RELEASE: Line = {"kind": "release", "deduction_id": "d-x", "release_mono_ns": RELEASE_LINE_NS}
# Malformed run-log records (orchestrator decision R1 of the H6 slice review: every record is
# checked before it counts), each appended to the closed S6 run's api-1.jsonl after its report was
# built: (case, the line, the field its input failure names, the reason it gives).
BAD_RECORDS: list[tuple[str, Line, str, str]] = [
    ("no kind", {"deduction_id": "d-ok"}, "kind", "missing"),
    ("a kind that is no string", {"kind": False}, "kind", "want a string"),
    (
        "an unknown kind",
        {"kind": "crash"},
        "kind",
        "'crash': not a run-log record kind (decision, request, release, refund, lifecycle)",
    ),
    ("a lifecycle line with no event", {"kind": "lifecycle"}, "event", "want a string"),
    (
        "an unknown lifecycle event",
        {"kind": "lifecycle", "event": "kill"},
        "event",
        "'kill': not a lifecycle event (start, ready, stop, pre_kill)",
    ),
    (
        "a decision without user_id",
        without(decision("d-x", DECIDED, "u9"), "user_id"),
        "user_id",
        "missing",
    ),
    (
        "a decision whose user_id is no string",
        {**decision("d-x", DECIDED, "u000009"), "user_id": 5},
        "user_id",
        "want a string",
    ),
    (
        "a decision with another result",
        decision("d-x", DECIDED, "u000009", "limited"),
        "result",
        "'limited': want allowed or degraded",
    ),
    (
        "a decision whose decision_ns is no integer",
        {**decision("d-x", DECIDED, "u000009"), "decision_ns": 1.5},
        "decision_ns",
        "want an integer",
    ),
    (
        "a degraded decision (no deduction_id) whose decision_ns is no integer",
        {**decision("", 0, "u000004", "degraded"), "decision_ns": 1.5},
        "decision_ns",
        "want an integer",
    ),
    ("a request with an unknown field", {**A_REQUEST, "extra": 1}, "extra", "unknown field"),
    ("a request without db_ns", without(A_REQUEST, "db_ns"), "db_ns", "missing"),
    (
        "a request whose limiter_ns is no integer",
        {**A_REQUEST, "limiter_ns": 1.0},
        "limiter_ns",
        "want an integer",
    ),
    ("a request whose error is no string", {**A_REQUEST, "error": 7}, "error", "want a string"),
    ("a request whose result is null", {**A_REQUEST, "result": None}, "result", "want a string"),
    (
        "a request with an HTTP status below 100",
        {**A_REQUEST, "http_status": 99},
        "http_status",
        "99: want at least 100",
    ),
    ("a release with an unknown field", {**A_RELEASE, "extra": 1}, "extra", "unknown field"),
    (
        "a release whose deduction_id is no string",
        {**A_RELEASE, "deduction_id": 7},
        "deduction_id",
        "want a string",
    ),
    (
        "a release with a negative release_mono_ns",
        {**A_RELEASE, "release_mono_ns": -1},
        "release_mono_ns",
        "-1: want at least 0",
    ),
    (
        "a final event of another result",
        final_event("d-ok", "refunded"),
        "result",
        "'refunded': not a final event (SPEC API-1d)",
    ),
    (
        "an unresolved final event with no reason",
        final_event("d-ok", "unresolved"),
        "reason",
        "'': want closed, retention or expired (unresolved)",
    ),
    (
        "an unresolved final event with another reason",
        final_event("d-ok", "unresolved", reason="timeout"),
        "reason",
        "'timeout': want closed, retention or expired (unresolved)",
    ),
    (
        "an applied final event with a reason",
        final_event("d-ok", reason="closed"),
        "reason",
        "'closed': only unresolved has a reason",
    ),
    (
        "a final event whose reason is null",
        {**final_event("d-ok"), "reason": None},
        "reason",
        "want a string",
    ),
    (
        "a final event without attempts",
        without(final_event("d-ok"), "attempts"),
        "attempts",
        "missing",
    ),
    (
        "a final event with negative attempts",
        {**final_event("d-ok"), "attempts": -1},
        "attempts",
        "-1: want at least 0",
    ),
    (
        "a final event whose credit_decision_ns is no integer",
        {**final_event("d-ok"), "credit_decision_ns": 1.0},
        "credit_decision_ns",
        "want an integer",
    ),
    (
        "a start line without nofile_limit",
        without(lifecycle_start(), "nofile_limit"),
        "nofile_limit",
        "missing",
    ),
    (
        "a start line with a negative limit",
        {**lifecycle_start(), "queue_len": -1},
        "queue_len",
        "-1: want at least 0",
    ),
    (
        "a ready line with an unknown field",
        {**lifecycle("ready"), "extra": 1},
        "extra",
        "unknown field",
    ),
    (
        "a stop line without log_drops",
        without(lifecycle("stop"), "log_drops"),
        "log_drops",
        "missing",
    ),
    (
        "a stop line whose listener_drops is no integer",
        {**lifecycle("stop"), "listener_drops": 0.0},
        "listener_drops",
        "want an integer",
    ),
    (
        "a bare pre_kill line",
        {"kind": "lifecycle", "event": "pre_kill"},
        "listener_drops",
        "missing",
    ),
    (
        "a pre_kill line without read_ok",
        without(lifecycle("pre_kill"), "read_ok"),
        "read_ok",
        "missing",
    ),
    (
        "a pre_kill line whose read_ok is no boolean",
        {**lifecycle("pre_kill"), "read_ok": 1},
        "read_ok",
        "want true or false",
    ),
    (
        "a pre_kill line with negative listener_drops",
        {**lifecycle("pre_kill"), "listener_drops": -1},
        "listener_drops",
        "-1: want at least 0",
    ),
    (
        "a pre_kill line whose log_drops is no integer",
        {**lifecycle("pre_kill"), "log_drops": 0.0},
        "log_drops",
        "want an integer",
    ),
]


@pytest.mark.parametrize(
    ("case", "line", "field", "reason"), BAD_RECORDS, ids=[c for c, _, _, _ in BAD_RECORDS]
)
def test_malformed_record(tmp_path: Path, case: str, line: Line, field: str, reason: str) -> None:
    """A run-log record whose kind is missing, not a string or not one of DESIGN §B8's, whose
    lifecycle event is not one, which lacks a field of its kind or event or holds another, or
    whose field completion reads has the wrong type or range fails the run as input, naming the
    file, the line and the field."""
    world = make_world(tmp_path)
    log = world.run("closed").joinpath("api-1.jsonl")
    count = len(log.read_text(encoding="utf-8").splitlines())
    rewrite_log(world.run("closed"), lambda lines: [*lines, line])
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    where = f"input: api-1.jsonl: line {count + 1}: field {field}: {reason}"
    assert rest(lines)[0].endswith(where), (case, lines)


INT64_MAX = 2**63 - 1
PRE_KILL: Line = {"event": "pre_kill", "read_ok": True}  # makes a stop line a pre_kill line
# The integers completion reads from a run log, each an int64 as the Go writers write it, the
# range runfiles.get_int holds it to (orchestrator decision W4 of the H6 round-2 review: both ends
# pinned, 2^63 − 1 taken and 2^63 refused, for each one): (case, the capped_tail line holding it,
# the field, edits to the tail's lines that keep the run's verdict when it is 2^63 − 1: a confirmed
# refund's credit before its expiry, its final event not before its release and within its
# deadline).
CAPPED: list[tuple[str, int, str, dict[int, Line]]] = [
    ("a start line's max_in_flight", 0, "max_in_flight", {}),
    ("a start line's queue_len", 0, "queue_len", {}),
    ("a start line's max_release_delay_ns", 0, "max_release_delay_ns", {}),
    ("a start line's dependency_deadline_ns", 0, "dependency_deadline_ns", {}),
    ("a start line's drain_timeout_ns", 0, "drain_timeout_ns", {}),
    ("a start line's nofile_limit", 0, "nofile_limit", {}),
    ("a decision's decision_ns", 2, "decision_ns", {}),
    ("a release's release_mono_ns", 3, "release_mono_ns", {}),
    ("a final event's attempts", 4, "attempts", {}),
    (
        "a final event's credit_decision_ns",
        4,
        "credit_decision_ns",
        {2: {"decision_ns": INT64_MAX}},
    ),
    (
        "a final event's release_mono_ns",
        4,
        "release_mono_ns",
        {4: {"final_mono_ns": INT64_MAX}},
    ),
    (
        "a final event's final_mono_ns",
        4,
        "final_mono_ns",
        {4: {"release_mono_ns": INT64_MAX - 7 * MS}},
    ),
    ("a request's http_status", 5, "http_status", {}),
    ("a request's limiter_ns", 5, "limiter_ns", {}),
    ("a request's db_ns", 5, "db_ns", {}),
    ("a stop line's listener_drops", 6, "listener_drops", {}),
    ("a stop line's log_drops", 6, "log_drops", {}),
    ("a pre_kill line's listener_drops", 6, "listener_drops", {6: PRE_KILL}),
    ("a pre_kill line's log_drops", 6, "log_drops", {6: PRE_KILL}),
]


def capped_tail(index: int, field: str, value: int, edits: dict[int, Line]) -> list[Line]:
    """Another hitapi process's lines, for the closed run's log: its start and ready lines, d-x
    released and its refund applied in time (its decision, release, final event and request
    lines) and its stop line; edits applied, then the line at index given field = value."""
    tail = [
        lifecycle_start(),
        lifecycle("ready"),
        *released("d-x", final_event("d-x"), user="u000009"),
        lifecycle("stop"),
    ]
    for i, change in edits.items():
        tail[i] = {**tail[i], **change}
    tail[index] = {**tail[index], field: value}
    return tail


@pytest.mark.parametrize(
    ("case", "index", "field", "edits"), CAPPED, ids=[c for c, _, _, _ in CAPPED]
)
def test_run_log_integer_at_int64_max(
    tmp_path: Path, case: str, index: int, field: str, edits: dict[int, Line]
) -> None:
    """Each integer completion reads from a run log may be 2^63 − 1, int64's largest: another
    process appended to the closed run's log with it there fails no check, and its release is
    read and confirmed in time (3 releases: the two S6 runs' d-ok and its d-x)."""
    world = make_world(tmp_path)
    tail = capped_tail(index, field, INT64_MAX, edits)
    rewrite_log(world.run("closed"), lambda lines: [*lines, *tail])
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], (case, lines)
    assert ": 14 failed; 5 runs; 3 releases: 3 confirmed in time, " in lines[-1], (case, lines)


@pytest.mark.parametrize(
    ("case", "index", "field", "edits"), CAPPED, ids=[c for c, _, _, _ in CAPPED]
)
def test_run_log_integer_beyond_int64(
    tmp_path: Path, case: str, index: int, field: str, edits: dict[int, Line]
) -> None:
    """Each integer completion reads from a run log is refused at 2^63, 1 beyond int64: the same
    appended process with it there fails the run as input, naming the file, the line and the
    field."""
    world = make_world(tmp_path)
    log = world.run("closed").joinpath("api-1.jsonl")
    count = len(log.read_text(encoding="utf-8").splitlines())
    tail = capped_tail(index, field, INT64_MAX + 1, edits)
    rewrite_log(world.run("closed"), lambda lines: [*lines, *tail])
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    where = (
        f"input: api-1.jsonl: line {count + index + 1}: field {field}: {INT64_MAX + 1}: beyond "
        "int64"
    )
    assert rest(lines)[0].endswith(where), (case, lines)


def unconfirmed_d1(tail: list[Line]) -> Callable[[list[Line]], list[Line]]:
    """A rewrite_log change: d1's final event taken out, and the closed run's last line (its stop
    line) replaced by tail."""

    def change(lines: list[Line]) -> list[Line]:
        kept = drop_final("d1")(lines)
        assert kept[-1] == lifecycle("stop")
        return [*kept[:-1], *tail]

    return change


@pytest.mark.parametrize(
    ("case", "tail", "check_name", "detail"),
    [
        (
            "a bare pre_kill line before the stop line",
            [{"kind": "lifecycle", "event": "pre_kill"}, lifecycle("stop")],
            "input",
            "field listener_drops: missing",
        ),
        (
            "a pre_kill line before the stop line",
            [lifecycle("pre_kill"), lifecycle("stop")],
            "refund_completion",
            "no final event, and its instance was not killed",
        ),
    ],
)
def test_pre_kill_is_a_kill_only_when_one(
    tmp_path: Path, case: str, tail: list[Line], check_name: str, detail: str
) -> None:
    """A covered release whose final event is missing is excused as killed only by a pre_kill
    line that is one, with no stop line after it: a bare pre_kill line fails the run as input
    before any exclusion applies, and a process that wrote its stop line after a pre_kill line
    drained and closed, so it was not killed and the release fails."""
    world = make_world(tmp_path, closed=s6_lines("closed", released("d1", final_event("d1"))))
    rewrite_log(world.run("closed"), unconfirmed_d1(tail))
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], check_name)], (case, lines)
    assert detail in rest(lines)[0], (case, lines)


def test_pre_kill_ends_the_segment(tmp_path: Path) -> None:
    """The same unconfirmed release in a segment that ends with a pre_kill line (its stop line
    replaced by one) is excused: its process was killed right after it."""
    world = make_world(tmp_path, closed=s6_lines("closed", released("d1", final_event("d1"))))
    rewrite_log(world.run("closed"), unconfirmed_d1([lifecycle("pre_kill")]))
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], lines
    assert ", excluded killed 1, " in lines[-1], lines


def after_mode(run_id: str, mode: str) -> str:
    """Check 4's cause in a SKIP line for an S6 limiter run in mode that failed on input after its
    failure mode was read."""
    return f"S6 limiter run {run_id} in {mode} mode failed on input after its failure mode was read"


def test_s6_run_logs_failing_input(tmp_path: Path) -> None:
    """An S6 limiter run whose run logs fail on input still counts in its failure mode (read from
    its config.toml before them), so no mode is missing; its counts are unknown, so its mode's
    ERR-9 and ERR-16 are not judged, and SKIP lines say so, naming the run."""
    world = make_world(tmp_path)

    def negative(lines: list[Line]) -> list[Line]:
        for line in lines:
            if line.get("kind") == "refund":
                line["release_mono_ns"] = -39725585458
        return lines

    rewrite_log(world.run("open"), negative)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(OPEN, IDS["open"], "input")], lines
    assert "field release_mono_ns: -39725585458: want at least 0" in rest(lines)[0], lines
    cause = after_mode(IDS["open"], "open")
    assert skips(lines) == [
        f"SKIP s6 - s6_unknowns_apart: ERR-9 in open mode not judged: {cause}",
        f"SKIP s6 - s6_unknowns_apart: ERR-16 in open mode not judged: {cause}",
    ], lines


def test_s6_run_failing_input_unread(tmp_path: Path) -> None:
    """An S6 limiter run that fails on input before its failure mode is read leaves a mode no
    other S6 run shows undecided: it is not judged, and a SKIP line says so (the closed mode,
    shown with its ERR-9 and ERR-16, holds)."""
    world = make_world(tmp_path)
    world.run("open").joinpath("report.json").unlink()
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(OPEN, IDS["open"], "input")], lines
    assert skips(lines) == [
        f"SKIP s6 - s6_failure_modes: open mode not judged: S6 limiter run {IDS['open']} failed "
        "on input before its failure mode was read"
    ], lines


# The closed S6 run failing on input after its failure mode was read: (case, its context.json
# faults, None for its one store down, a line taken out of its config.toml, "" for none, and the
# start of its input failure's detail).
AFTER_MODE: list[tuple[str, list[dict[str, object]] | None, str, str]] = [
    (
        "two store outage segments (decision R3)",
        [STORE_DOWN, API_KILL],
        "",
        "context.json: field faults: 2 store outage segments",
    ),
    (
        "no outage_tolerance_ns",
        None,
        "outage_tolerance_ns = 5_000_000_000\n",
        "config.toml: [policy]: field outage_tolerance_ns: want an integer",
    ),
]


@pytest.mark.parametrize(
    ("case", "faults", "dropped", "detail"), AFTER_MODE, ids=[c for c, _, _, _ in AFTER_MODE]
)
def test_s6_input_failure_after_mode_read(
    tmp_path: Path,
    case: str,
    faults: list[dict[str, object]] | None,
    dropped: str,
    detail: str,
) -> None:
    """An S6 limiter run that fails on input after its failure mode was read (its outage
    evidence, decision R3, or another [policy] setting; orchestrator decision V4) keeps its mode:
    closed mode is shown, its ERR-9 and ERR-16 are not judged, and SKIP lines say the run failed
    on input after its failure mode was read. The open mode is still judged: the open run's
    missing ERR-16 fails s6_unknowns_apart, and is not skipped."""
    world = make_world(tmp_path, closed_faults=faults, open_=s6_lines("open", err16=False))
    if dropped:
        config = world.run("closed").joinpath("config.toml")
        text = config.read_text(encoding="utf-8")
        assert dropped in text
        config.write_text(text.replace(dropped, ""), encoding="utf-8")
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [
        (CLOSED, IDS["closed"], "input"),
        ("s6", "-", "s6_unknowns_apart"),
    ], (case, lines)
    found = rest(lines)
    assert found[0].startswith(f"FAIL {CLOSED} {IDS['closed']} input: {detail}"), (case, lines)
    assert found[1] == (
        "FAIL s6 - s6_unknowns_apart: no ERR-16 in any S6 run in open mode: nothing shows it "
        "counted apart"
    ), (case, lines)
    cause = after_mode(IDS["closed"], "closed")
    assert skips(lines) == [
        f"SKIP s6 - s6_unknowns_apart: ERR-9 in closed mode not judged: {cause}",
        f"SKIP s6 - s6_unknowns_apart: ERR-16 in closed mode not judged: {cause}",
    ], (case, lines)


def test_s6_input_failures_before_and_after_mode(tmp_path: Path) -> None:
    """With the closed S6 run failing on input after its failure mode was read (two outage
    segments) and an open one before it (its report.json gone), the closed mode's ERR-9 and ERR-16
    are not judged, and each SKIP line names both runs and when each failed; the open mode, shown
    with both by the other open run, holds."""
    world = make_world(tmp_path, closed_faults=[STORE_DOWN, API_KILL])
    name = "s6-pause-open"
    other = s6_run(world, other_id(name, "limiter"), name, "open", {1: s6_lines("open")})
    other.joinpath("report.json").unlink()
    listed = (*WORLD, name)
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines, listed) == [
        (CLOSED, IDS["closed"], "input"),
        (name, other.name, "input"),
    ], lines
    cause = (
        f"{after_mode(IDS['closed'], 'closed')}; S6 limiter run {other.name} failed on input "
        "before its failure mode was read"
    )
    assert skips(lines) == [
        f"SKIP s6 - s6_unknowns_apart: ERR-9 in closed mode not judged: {cause}",
        f"SKIP s6 - s6_unknowns_apart: ERR-16 in closed mode not judged: {cause}",
    ], lines


def test_s6_after_mode_failure_named_only_in_its_mode(tmp_path: Path) -> None:
    """With the closed S6 run failing on input after its failure mode was read (two outage
    segments) and the only open one before it (its report.json gone), the open mode is not judged,
    and its SKIP line names the open run alone, failing before its failure mode was read: the
    closed run, its mode read, is named only in the closed mode's lines, where ERR-9 and ERR-16
    are not judged and each SKIP line names both runs (orchestrator decisions V4 and V6)."""
    world = make_world(tmp_path, closed_faults=[STORE_DOWN, API_KILL])
    world.run("open").joinpath("report.json").unlink()
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [
        (CLOSED, IDS["closed"], "input"),
        (OPEN, IDS["open"], "input"),
    ], lines
    before = f"S6 limiter run {IDS['open']} failed on input before its failure mode was read"
    cause = f"{after_mode(IDS['closed'], 'closed')}; {before}"
    assert skips(lines) == [
        f"SKIP s6 - s6_failure_modes: open mode not judged: {before}",
        f"SKIP s6 - s6_unknowns_apart: ERR-9 in closed mode not judged: {cause}",
        f"SKIP s6 - s6_unknowns_apart: ERR-16 in closed mode not judged: {cause}",
    ], lines


@pytest.mark.parametrize("case", ["unscanned", "unread"])
def test_bench_input_failure_not_s6(tmp_path: Path, case: str) -> None:
    """Only an S6 limiter run that failed on input leaves a failure mode undecided: with the
    closed S6 run dropped from the manifest, the s3-busy-user limiter run (a benchmark, closed
    mode) failing on input in its run log (unscanned) or before its report was read (unread)
    still leaves 'no S6 limiter run in closed mode' a failure, not a SKIP."""
    world = make_world(tmp_path)
    if case == "unscanned":
        path = world.run("s3").joinpath("api-1.jsonl")
        path.write_text(path.read_text(encoding="utf-8") + "{not json\n", encoding="utf-8")
    else:
        world.run("s3").joinpath("report.json").unlink()

    def drop(doc: dict[str, object]) -> None:
        scenarios = doc["scenarios"]
        assert isinstance(scenarios, dict)
        del scenarios[CLOSED]

    code, lines = check(world.manifest(drop))
    assert code == 1
    kept = [name for name in WORLD if name != CLOSED]
    assert failed(lines, kept) == [(S3, IDS["s3"], "input"), ("s6", "-", "s6_failure_modes")], lines
    found = rest(lines, kept)
    assert ("api-1.jsonl: line" if case == "unscanned" else "report.json") in found[0], lines
    assert found[1] == "FAIL s6 - s6_failure_modes: no S6 limiter run in closed mode", lines
    assert skips(lines) == [], lines
