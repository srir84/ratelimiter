"""report/completion.py (IMPLEMENTATION_PLAN H6 steps 3 and 4; Tests (a)): the manifest built from a
synthetic runs/index.jsonl, and each HARNESS §8 Completion check on synthetic run directories
(report/synthetic.py) that report/validate.py passes, each case changing one thing: a clean set
exits 0, and each check broken on its own exits 1 naming that check. Expected values are
literals.

Most cases use make_world's five runs (s3-busy-user's baseline, limiter run and rerun, and two
S6 runs, closed and open), which list 3 of the 17 H6 scenarios: completion fails each other one
first (check 0, listed), and failed() and rest() assert exactly those failures before returning
the others. full_world adds a run of every other H6 scenario, so a clean one exits 0.

Refund completion's exclusions are pinned on both sides of each bound: a killed instance's
releases against the kill's upper bound (F1 item R2: context.json's kill record, the killed and
restarted processes' request arrivals and release times, written consistently), and REF-9 (d)
against context.json's tamper records (F1 item R1), less the clocks' drift (item B6), and the
final event's latest wall-clock time (item B5); a deferred loss also against the ERR-11 request
line that shows its event (judgment RP3), done before any epoch-wide loss began (judgment RB3), or
any store-wide tamper but that loss itself, while a key-scoped tamper voids none (judgment RB5).
Each fault command completion reads is checked as faultctl's grammar takes it (item B4), each
fault's times as completion reads them (judgment RP2), and each request line's total_ns against
the limiter_ns + db_ns it covers (judgment RP4)."""

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
ARRIVED = T0_WALL_NS + 7 * SECOND  # a released permit's request arrival_ns (wall clock, HTTP-5)
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
    arrival_ns: int = ARRIVED,
) -> list[Line]:
    """A permit released for a confirmed failure, in hitapi's order: its allowed decision, its
    release (release_mono_ns release_ns), its final event (when given) and its request line (from
    instance, arriving at arrival_ns), whose limiter_ns and db_ns are elapsed."""
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
                arrival_ns=arrival_ns,
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
    arrival_ns: int = T0_WALL_NS + 6 * SECOND,
) -> list[Line]:
    """One instance's run log of an S6 run: an allowed request (ERR-1), an ERR-9 request, an
    ERR-16 one (its permit done, no release), an ERR-8 (closed) or an ERR-3 and a degraded permit
    refused after its acquire (open: a release with no deduction_id and no final event), each
    arriving at arrival_ns, a permit refused after its acquire whose refund was applied 7 ms after
    its release (its request arriving 1 s later), then extra. killed puts a first process segment,
    holding those lines and ending with a pre_kill line (FLT-6), before it; violation adds
    violation_lines()."""
    lines: list[Line] = []
    if killed:
        lines.extend([lifecycle_start(), lifecycle("ready"), *killed, lifecycle("pre_kill")])
    lines.extend([lifecycle_start(), lifecycle("ready")])
    arrival = arrival_ns
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
    lines.extend(released("d-ok", final_event("d-ok"), arrival_ns=arrival + SECOND))
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
    config: Callable[[str], str] | None = None,
) -> Path:
    """An S6 test run of scenario name with one instance per apis entry (its run log lines), its
    config.toml in the given failure mode (then changed by config, when given), its report built
    and its index line appended; its context.json faults are the mode's one outage segment
    (STORE_DOWN closed, REFUSE open) unless faults gives them."""
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
    path = run.joinpath("config.toml")
    text = path.read_text(encoding="utf-8").replace(
        'failure_mode = "closed"', f'failure_mode = "{mode}"'
    )
    path.write_text(text if config is None else config(text), encoding="utf-8")
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
    closed_config: Callable[[str], str] | None = None,
    rerun_planned_s: Sequence[int] | None = None,
    rerun_extra: Sequence[Line] = (),
) -> World:
    """The clean set: s3-busy-user's baseline, limiter run and rerun (one schedule), and the two
    S6 runs, closed and open, each one instance with s6_lines unless given (closed_apis: the
    closed run's instances and their run logs; closed_faults: its context.json faults;
    closed_config: a change to its config.toml text, before its report is built).
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
    s6_run(world, IDS["closed"], CLOSED, "closed", closed_apis, closed_faults, closed_config)
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
        "excluded killed 0, drained 0, after_p 0, lost 0, quarantined 0"
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
        "after_p 0, lost 0, quarantined 0"
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


# Releases on a killed instance (SPEC REF-9 (c), orchestrator decision D5, F1 item R2). In the kill
# cases instance 1's first process is killed: it started at P0 on the wall clock, so a permit it
# released mono ns in (release_mono_ns, its monotonic clock), refused after its acquire with
# limiter_ns 2 ms, had its request arrive at P0 + mono − 2 ms (released()'s d1: ARRIVED, 8 s in);
# its kill, faultctl api kill 1 --restart, ran from KILL_START to KILL_END on the wall clock (the
# record context.json keeps); and the restarted process's requests arrive from RESTARTED on.
P0 = ARRIVED - (RELEASE_LINE_NS - 2 * MS)
KILL_START = T0_WALL_NS + 7_500 * MS
KILL_END = T0_WALL_NS + 8 * SECOND
RESTARTED = T0_WALL_NS + 10 * SECOND
BOUND_NS = DEADLINE_NS + OUTAGE_NS
# The kill's upper bound on the killed process's monotonic clock, K_hi, with d1 its one request
# joined to a release: d1's arrival at most 8 s − 2 ms into the process, then the 1 s from it to
# KILL_END on the wall clock, plus completion's 4 % slew allowance over that second (40 ms; F1 item
# B7: SLEW_PPM 40,000).
K_HI = RELEASE_LINE_NS - 2 * MS + SECOND + 40 * MS


def kill_fault(
    start_ns: int = KILL_START,
    end_ns: int = KILL_END,
    *,
    instance: int = 1,
    error: str = "",
    restart: bool = True,
) -> dict[str, object]:
    """context.json's record of faultctl api kill <instance> [--restart] (plan H4 step 7,
    runFaultctl): the instance it names, the command's own start and end on the wall clock, no
    outage and no targets; error when it failed (or did not run: start_ns and end_ns 0)."""
    rec: dict[str, object] = {
        "at_s": max(start_ns - T0_WALL_NS, 0) / SECOND,
        "cmd": f"faultctl api kill {instance}" + (" --restart" if restart else ""),
        "instance": instance,
        "start_ns": start_ns,
        "end_ns": end_ns,
        "outage_start_ns": None,
        "outage_ns": None,
        "targets": [],
    }
    if error:
        rec["error"] = error
    return rec


def not_ready(arrival_ns: int, request_id: str) -> Line:
    """A request a restarted process answered 503 limiter_not_ready (ERR-12a) before it was ready,
    arriving at arrival_ns."""
    return request(
        1,
        request_id,
        arrival_ns=arrival_ns,
        status=503,
        error="limiter_not_ready",
        result="not_ready",
        db_ns=0,
    )


def killed_log(
    killed: Sequence[Line],
    extra: Sequence[Line] = (),
    *,
    after_kill: Sequence[Line] = (),
    restart: Sequence[Line] = (),
    restarted: int = RESTARTED,
) -> list[Line]:
    """The closed run's log with a killed first process segment: killed, its pre_kill line, then
    after_kill (lines the killed process wrote after it); then the restarted process (s6_lines,
    its requests arriving at restarted, then extra), with restart's lines right after its ready
    line."""
    lines = s6_lines("closed", extra, killed=killed, arrival_ns=restarted)
    kill = lines.index(lifecycle("pre_kill"))
    return [
        *lines[: kill + 1],
        *after_kill,
        *lines[kill + 1 : kill + 3],
        *restart,
        *lines[kill + 3 :],
    ]


def kill_world(root: Path, log: list[Line], *kills: dict[str, object]) -> World:
    """make_world with the closed run's log, its context.json faults its store down and kills
    (kill_fault() when none is given)."""
    return make_world(root, closed=log, closed_faults=[STORE_DOWN, *(kills or (kill_fault(),))])


def test_release_on_killed_instance(tmp_path: Path) -> None:
    """A release with no final event on an instance killed before it (its process segment ends
    with a pre_kill line, FLT-6, matched to context.json's kill) need not be confirmed when the
    kill came within refund_deadline + the measured outage after it; the same release in a
    segment that was not killed fails (here its final event is taken out of the log after the
    report was built); and a release whose final event came before the kill must still be
    confirmed in time."""
    world = kill_world(tmp_path, killed_log(released("d1", None)))
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
    world = kill_world(tmp_path.joinpath("late"), killed_log(late))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines


def test_restarted_after_kill(tmp_path: Path) -> None:
    """The kill exclusion ends with the killed process: an instance killed (FLT-6) and restarted
    starts a new process segment, where a covered release with no final event fails (its final
    event is taken out after the report was built) while the killed segment's is excluded."""
    d2 = released("d2", final_event("d2"), arrival_ns=RESTARTED + 2 * SECOND)
    world = kill_world(tmp_path, killed_log(released("d1", None, user="u000007"), d2))
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
    world = kill_world(tmp_path, killed_log(released("d1", final)))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    shown = f"{result} ({reason})" if reason else result
    assert "deduction d1: " in rest(lines)[0], lines
    assert f"final event {shown}: not confirmed" in rest(lines)[0], lines


def degraded_release(release_ns: int) -> list[Line]:
    """A degraded permit refused after its acquire: its decision and its release (no
    deduction_id, no final event), the release at release_ns on hitapi's monotonic clock."""
    return [
        decision("", 0, "u000004", "degraded"),
        {"kind": "release", "deduction_id": "", "release_mono_ns": release_ns},
    ]


# The kill's upper bound K_hi decides (F1 item R2). In the calibrated cases the killed process
# released d3 40 s in, its request at CALIBRATED (P0 + 40 s − 2 ms) and its refund applied in time,
# and the kill ended 1 s + 1 ns after that request, so K_hi = 40 s − 2 ms + 1 s + 1 ns + 40,000,001
# ns of slew (4 % of 1 s + 1 ns, rounded up: SLEW_PPM ± 1 moves it by 1,000 ns); d1, released
# earlier with no final event, is released exactly refund_deadline + the measured outage before
# K_hi (its own request, 34 s before the kill's end, gives a looser bound), or 1 ns earlier. The
# restarted process serves from LATE_RESTART on.
CALIBRATED = P0 + 40 * SECOND - 2 * MS
CAL_KILL_START = CALIBRATED + 500 * MS
CAL_KILL_END = CALIBRATED + SECOND + 1
CAL_K_HI = 40 * SECOND - 2 * MS + SECOND + 1 + 40_000_001
LATE_RESTART = T0_WALL_NS + 50 * SECOND


def calibrated(
    d1_ns: int, *, before: Sequence[Line] = (), after: Sequence[Line] = ()
) -> list[Line]:
    """The killed process's lines: d1 released d1_ns in with no final event (its request at P0 +
    d1_ns − 2 ms), then before, then d3, the calibrating permit, then after."""
    return [
        *released("d1", None, user="u000007", release_ns=d1_ns, arrival_ns=P0 + d1_ns - 2 * MS),
        *before,
        *released(
            "d3", final_event("d3"), user="u000008", release_ns=40 * SECOND, arrival_ns=CALIBRATED
        ),
        *after,
    ]


# Two more calibrating permits, released 39 s and 38 s in: their requests, 1 s and 2 s before d3's,
# give bounds 40 ms and 80 ms looser than d3's (the slew over 1 s and 2 s more).
D4 = released(
    "d4", final_event("d4"), user="u000009", release_ns=39 * SECOND, arrival_ns=CALIBRATED - SECOND
)
D5 = released(
    "d5",
    final_event("d5"),
    user="u000010",
    release_ns=38 * SECOND,
    arrival_ns=CALIBRATED - 2 * SECOND,
)
AT_BOUND = CAL_K_HI - BOUND_NS  # d1's release_mono_ns when K_hi is exactly the bound after it
# (case, the closed run's log, its kills, whether d1 is excused).
KILL_BOUND: list[tuple[str, list[Line], list[dict[str, object]], bool]] = [
    (
        "K_hi at refund_deadline + the measured outage after the release",
        killed_log(calibrated(AT_BOUND), restarted=LATE_RESTART),
        [kill_fault(CAL_KILL_START, CAL_KILL_END)],
        True,
    ),
    (
        "K_hi 1 ns past it",
        killed_log(calibrated(AT_BOUND - 1), restarted=LATE_RESTART),
        [kill_fault(CAL_KILL_START, CAL_KILL_END)],
        False,
    ),
    (
        "a lone release whose kill ends 40 s after its request",
        killed_log(released("d1", None), restarted=LATE_RESTART),
        [kill_fault(ARRIVED + 39_860 * MS, ARRIVED + 40 * SECOND)],
        False,
    ),
    (
        "the tightest of three joined requests written between the others",
        killed_log(calibrated(AT_BOUND, before=D4, after=D5), restarted=LATE_RESTART),
        [kill_fault(CAL_KILL_START, CAL_KILL_END)],
        True,
    ),
    # The restarted process ran only after the kill, so its earliest request arrival bounds the
    # kill too: here 1 s before the kill's end, though written after a later one.
    (
        "the restarted process's earliest arrival, before the kill's end",
        killed_log(
            calibrated(AT_BOUND),
            restart=[
                not_ready(CAL_KILL_END + 500 * MS, "q-late"),
                not_ready(CAL_KILL_END, "q-first"),
            ],
            restarted=LATE_RESTART,
        ),
        [kill_fault(CAL_KILL_START, CAL_KILL_END + SECOND)],
        True,
    ),
    (
        "the restarted process's arrivals all after the kill's end",
        killed_log(
            calibrated(AT_BOUND),
            restart=[not_ready(CAL_KILL_END + 1, "q-first")],
            restarted=LATE_RESTART,
        ),
        [kill_fault(CAL_KILL_START, CAL_KILL_END)],
        True,
    ),
    (
        "the restarted process's earliest arrival at the kill's start",
        killed_log(released("d1", None), restart=[not_ready(KILL_START, "q-first")]),
        [kill_fault()],
        True,
    ),
    # A kill's end at its own start is taken (judgment RP2; KILL_ERRORS refuses it 1 ns earlier).
    (
        "a kill ending as it starts",
        killed_log(released("d1", None)),
        [kill_fault(KILL_END, KILL_END)],
        True,
    ),
]


@pytest.mark.parametrize(
    ("case", "log", "kills", "ok"), KILL_BOUND, ids=[c for c, _, _, _ in KILL_BOUND]
)
def test_kill_within_deadline(
    tmp_path: Path, case: str, log: list[Line], kills: list[dict[str, object]], ok: bool
) -> None:
    """A release with no final event on a killed instance is excused only when the kill's upper
    bound K_hi, on its process's monotonic clock, is at most refund_deadline + the measured outage
    after it: K_hi maps the kill's end on the wall clock (context.json's end_ns, or the restarted
    process's earliest request arrival if earlier) through the killed process's requests joined to
    their releases, the least bound any of them gives; 1 ns later, or when the kill ends 40 s after
    a lone release (which the latest release_mono_ns alone used to excuse), the instance may have
    kept running past the refund's deadline and REF-5 still covered it."""
    world = kill_world(tmp_path, log, *kills)
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
    """A release whose kill's upper bound came more than refund_deadline + the measured outage
    after it fails as a REF-5 miss, saying how late the kill may have come and how K_hi was found:
    the kill's end 40 s after d1's request, 4 % of that (1.6 s) for slew."""
    end = ARRIVED + 40 * SECOND
    world = kill_world(
        tmp_path,
        killed_log(released("d1", None), restarted=LATE_RESTART),
        kill_fault(end - 140 * MS, end),
    )
    k_hi = RELEASE_LINE_NS - 2 * MS + 40 * SECOND + 1600 * MS
    code, lines = check(world.manifest())
    assert code == 1
    assert rest(lines) == [
        f"FAIL {CLOSED} {IDS['closed']} refund_completion: api-1.jsonl: line 4: deduction d1: "
        "released 2000000 ns after just before its acquire (limiter_ns + db_ns, within P "
        f"5000000000): no final event, and its instance was killed up to {k_hi - RELEASE_LINE_NS} "
        f"ns after its release (release_mono_ns {RELEASE_LINE_NS}; K_hi {k_hi}, the kill's upper "
        f"bound on its process's monotonic clock: context.json faults[1] end_ns {end} on the "
        f"wall clock, mapped through the request at api-1.jsonl: line 5 (arrival_ns {ARRIVED}) "
        f"released at api-1.jsonl: line 4 (release_mono_ns {RELEASE_LINE_NS}, limiter_ns + db_ns "
        "2000000), plus 1600000000 ns of slew), above refund_deadline_ns 30000000000 + the "
        f"measured outage 4085364542 ns = {BOUND_NS}: nothing shows its instance was killed "
        "before its deadline, so REF-9 (c) does not excuse it (REF-5)"
    ], lines


def test_killed_without_a_joined_request(tmp_path: Path) -> None:
    """A killed process none of whose request lines joins one of its release lines (here d1, its
    one release, lost its request line too) gives no upper bound on its kill time: d1, with no
    final event, is not excused."""
    world = kill_world(tmp_path, killed_log(released("d1", None, with_request=False)))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert rest(lines) == [
        f"FAIL {CLOSED} {IDS['closed']} refund_completion: api-1.jsonl: line 4: deduction d1: no "
        "final event on an instance killed before it (api-1.jsonl: line 5), and no request line "
        "of that process joins one of its release lines, so nothing bounds the kill's time: REF-9 "
        "(c) does not excuse it (REF-5); it has no request line, so whether it was released "
        "within P (5000000000 ns) is unknown"
    ], lines


# The latest release_mono_ns the killed process's segment holds bounds the kill from below (the
# process still ran then): (case, the closed run's log, whether K_hi falls below it). K_hi is K_HI
# throughout (d1 is the killed process's one joined request).
K_EST: list[tuple[str, list[Line], bool]] = [
    (
        "a degraded permit's release at K_hi",
        killed_log([*released("d1", None), *degraded_release(K_HI)]),
        False,
    ),
    (
        "a degraded permit's release 1 ns after K_hi",
        killed_log([*released("d1", None), *degraded_release(K_HI + 1)]),
        True,
    ),
    (
        "a release written after the pre_kill line, 1 ns after K_hi",
        killed_log(released("d1", None), after_kill=degraded_release(K_HI + 1)),
        True,
    ),
    # Concurrent requests write their release lines out of mono order: the largest counts, not
    # the one written last (orchestrator decision W1 of the H6 round-2 review).
    (
        "release lines out of mono order, the largest written first: 1 ns after K_hi",
        killed_log(
            [
                *released("d1", None),
                *degraded_release(K_HI + 1),
                *degraded_release(RELEASE_LINE_NS + 1),
            ]
        ),
        True,
    ),
    (
        "the restarted process's release 1 ns after K_hi (another process's clock)",
        killed_log(
            released("d1", None),
            released(
                "d2", final_event("d2"), release_ns=K_HI + 1, arrival_ns=RESTARTED + 2 * SECOND
            ),
        ),
        False,
    ),
    (
        "an earlier process's release 1 ns after K_hi (another process's clock)",
        [
            lifecycle_start(),
            lifecycle("ready"),
            *released("d2", final_event("d2"), release_ns=K_HI + 1),
            lifecycle("stop"),
            *killed_log(released("d1", None, user="u000007")),
        ],
        False,
    ),
]


@pytest.mark.parametrize(("case", "log", "stepped"), K_EST, ids=[c for c, _, _ in K_EST])
def test_kill_bound_below_a_release(
    tmp_path: Path, case: str, log: list[Line], stepped: bool
) -> None:
    """K_hi below the latest release_mono_ns of the killed process's segment (its release lines',
    a degraded permit's and one written after the pre_kill line included, in whatever order they
    were written; not another process's, on another clock) means the wall clock stepped: the run
    fails as input, naming the pre_kill line. At it, or with only other processes' releases past
    it, d1 is judged, and excused."""
    world = kill_world(tmp_path, log)
    code, lines = check(world.manifest())
    assert code == 1, case
    if not stepped:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        assert ", excluded killed 1, " in lines[-1], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    assert (
        f"field event: pre_kill: K_hi {K_HI}, the kill's upper bound on its process's monotonic "
        "clock (" in rest(lines)[0]
    ), (case, lines)
    assert (
        f"is below the latest release_mono_ns {K_HI + 1} its process segment holds"
        in rest(lines)[0]
    ), (case, lines)


# Instance 1 killed twice: its first process (d1 released 8 s in, its request at ARRIVED) by the
# first kill; its second, started at T0 + 8 s, released d2 5 s in (its request at T0 + 13 s − 2
# ms) and was killed by the second; its third (s6_lines) serves from T0 + 30 s.
SECOND_KILL_END = T0_WALL_NS + 20_500 * MS
D2_ARRIVED = T0_WALL_NS + 13 * SECOND - 2 * MS


def two_kills_log() -> list[Line]:
    return [
        lifecycle_start(),
        lifecycle("ready"),
        *released("d1", None, user="u000007"),
        lifecycle("pre_kill"),
        lifecycle_start(),
        lifecycle("ready"),
        *released("d2", None, user="u000006", release_ns=5 * SECOND, arrival_ns=D2_ARRIVED),
        lifecycle("pre_kill"),
        *s6_lines("closed", arrival_ns=T0_WALL_NS + 30 * SECOND),
    ]


SECOND_KILL = kill_fault(T0_WALL_NS + 20 * SECOND, SECOND_KILL_END)
# (case, the closed run's kills after its store down), each taken by two_kills_log's two pre_kill
# lines in turn.
TWO_KILLS: list[tuple[str, list[dict[str, object]]]] = [
    ("the second kill starting long after the first", [kill_fault(), SECOND_KILL]),
    (
        "the second kill starting as the first ends",
        [kill_fault(), kill_fault(KILL_END, SECOND_KILL_END)],
    ),
    # A scenario need not list its faults by time (F1 item B8): kills are taken by start_ns.
    ("two kills listed in reverse", [SECOND_KILL, kill_fault()]),
    # Two kills starting together are taken in context.json's order (its index breaks the tie):
    # the first listed, ending as they start, takes the first pre_kill line, and the second starts
    # no earlier than it ended.
    (
        "two kills starting together, the first listed ending as they start",
        [kill_fault(KILL_START, KILL_START), kill_fault(KILL_START, SECOND_KILL_END)],
    ),
]


@pytest.mark.parametrize(("case", "kills"), TWO_KILLS, ids=[c for c, _ in TWO_KILLS])
def test_two_kills_matched_in_order(
    tmp_path: Path, case: str, kills: list[dict[str, object]]
) -> None:
    """Each pre_kill line of an instance is matched to its own kill: the k-th to the k-th
    'faultctl api kill N' fault in context.json by start_ns, context.json's order breaking a tie.
    Both releases are excused: d1 by the first kill (K_hi 1.038 s after it; 0.518 s when that kill
    ends at KILL_START), d2 by the second (7.502 s from its request to the kill's end, 300.08 ms of
    slew: 7.80208 s after it); matched the other way round, the second process's K_hi would fall
    before its own release."""
    world = kill_world(tmp_path, two_kills_log(), *kills)
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [] and skips(lines) == [], (case, lines)
    assert ", excluded killed 2, " in lines[-1], (case, lines)


def test_kill_of_instance_2(tmp_path: Path) -> None:
    """Each instance's pre_kill lines are matched to that instance's kills (F1 item B3, as both H6
    kill runs kill instance 2): in a two-instance closed run, instance 2, killed by 'faultctl api
    kill 2 --restart' (K_hi 1.038 s after it) after it released d2, which has no final event, is
    excused; instance 1 has no kill and needs none."""
    killed = [
        lifecycle_start(),
        lifecycle("ready"),
        *released("d2", None, instance=2, user="u000006"),
        lifecycle("pre_kill"),
        lifecycle_start(),
        lifecycle("ready"),
        lifecycle("stop"),
    ]
    world = make_world(
        tmp_path,
        closed_apis={1: s6_lines("closed"), 2: killed},
        closed_faults=[STORE_DOWN, kill_fault(instance=2)],
    )
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], lines
    assert ", excluded killed 1, " in lines[-1], lines


# A pre_kill line's kill that is not in context.json, failed, or overlaps the kill before it fails
# the run as input, naming the line and the field: (case, the closed run's log, or its instances'
# logs, its kills, the input failure's detail).
KILL_ERRORS: list[tuple[str, list[Line] | dict[int, list[Line]], list[dict[str, object]], str]] = [
    (
        "a pre_kill line no kill matches",
        killed_log(released("d1", None)),
        [],
        "api-1.jsonl: line 6: field event: pre_kill: no context.json fault 'faultctl api kill 1' "
        "matches it: it is pre_kill line 1 of api-1.jsonl, and context.json has 0",
    ),
    # A two-instance run, whose kill of instance 2 is valid input (F1 item B4: a kill of an
    # instance the run lacks is malformed).
    (
        "a pre_kill line with only another instance's kill",
        {
            1: killed_log(released("d1", None)),
            2: [lifecycle_start(), lifecycle("ready"), lifecycle("stop")],
        },
        [kill_fault(instance=2)],
        "api-1.jsonl: line 6: field event: pre_kill: no context.json fault 'faultctl api kill 1' "
        "matches it: it is pre_kill line 1 of api-1.jsonl, and context.json has 0",
    ),
    (
        "a pre_kill line whose kill failed",
        killed_log(released("d1", None)),
        [kill_fault(error="hitapi 1 is not running")],
        "api-1.jsonl: line 6: field event: pre_kill: its kill, context.json faults[1] (faultctl "
        "api kill 1 --restart), failed: hitapi 1 is not running",
    ),
    (
        "a pre_kill line whose kill did not run (end_ns 0)",
        killed_log(released("d1", None)),
        [kill_fault(0, 0)],
        "api-1.jsonl: line 6: field event: pre_kill: its kill, context.json faults[1] (faultctl "
        "api kill 1 --restart), did not run (end_ns 0)",
    ),
    (
        "the second kill starting 1 ns before the first ends",
        two_kills_log(),
        [kill_fault(), kill_fault(KILL_END - 1, SECOND_KILL_END)],
        "api-1.jsonl: line 12: field event: pre_kill: its kill, context.json faults[2] (faultctl "
        f"api kill 1 --restart), starts at start_ns {KILL_END - 1}, before faults[1], the kill of "
        f"pre_kill line 1 of api-1.jsonl, ended at end_ns {KILL_END}: two kills of instance 1 "
        "overlap",
    ),
    # Taken by start_ns (F1 item B8), overlapping kills listed in reverse overlap all the same.
    (
        "two overlapping kills listed in reverse",
        two_kills_log(),
        [kill_fault(KILL_END - 1, SECOND_KILL_END), kill_fault()],
        "api-1.jsonl: line 12: field event: pre_kill: its kill, context.json faults[1] (faultctl "
        f"api kill 1 --restart), starts at start_ns {KILL_END - 1}, before faults[2], the kill of "
        f"pre_kill line 1 of api-1.jsonl, ended at end_ns {KILL_END}: two kills of instance 1 "
        "overlap",
    ),
    # Two kills starting together are taken in context.json's order: the first listed, ending
    # last, takes the first pre_kill line, and the second starts before it ended.
    (
        "two kills starting together, the first listed ending last",
        two_kills_log(),
        [kill_fault(KILL_START, SECOND_KILL_END), kill_fault(KILL_START, KILL_START)],
        "api-1.jsonl: line 12: field event: pre_kill: its kill, context.json faults[2] (faultctl "
        f"api kill 1 --restart), starts at start_ns {KILL_START}, before faults[1], the kill of "
        f"pre_kill line 1 of api-1.jsonl, ended at end_ns {SECOND_KILL_END}: two kills of "
        "instance 1 overlap",
    ),
    (
        "the restarted process's earliest arrival 1 ns before the kill started",
        killed_log(released("d1", None), restart=[not_ready(KILL_START - 1, "q-first")]),
        [kill_fault()],
        f"api-1.jsonl: line 9: field arrival_ns: {KILL_START - 1}: before the kill of the process "
        f"before it started (context.json faults[1] start_ns {KILL_START}, matched to "
        "api-1.jsonl: line 6): a process started after that kill cannot have served it; the wall "
        "clock stepped, or that kill is not this instance's",
    ),
    # Judgment RP2: a kill that ran (end_ns not 0) started (start_ns not 0) no later than it ended,
    # each kill of the instance checked before they are taken by start_ns, a second one matched to
    # no pre_kill line too (start_ns 0 would sort it first). Its end at its start is taken
    # (KILL_BOUND).
    (
        "a kill that ran with start_ns 0",
        killed_log(released("d1", None)),
        [kill_fault(0, KILL_END)],
        f"context.json: faults[1]: field start_ns: 0, though it ran (end_ns {KILL_END}): want its "
        "start",
    ),
    (
        "a kill ending 1 ns before it started",
        killed_log(released("d1", None)),
        [kill_fault(KILL_END + 1, KILL_END)],
        f"context.json: faults[1]: field end_ns: {KILL_END}: before its start_ns {KILL_END + 1} "
        "(the wall clock stepped back, or the record is corrupt)",
    ),
    (
        "a second kill that ran with start_ns 0",
        killed_log(released("d1", None)),
        [kill_fault(), kill_fault(0, SECOND_KILL_END)],
        f"context.json: faults[2]: field start_ns: 0, though it ran (end_ns {SECOND_KILL_END}): "
        "want its start",
    ),
    (
        "a second kill ending 1 ns before it started, matched to no pre_kill line",
        killed_log(released("d1", None)),
        [kill_fault(), kill_fault(SECOND_KILL_END + 1, SECOND_KILL_END)],
        f"context.json: faults[2]: field end_ns: {SECOND_KILL_END}: before its start_ns "
        f"{SECOND_KILL_END + 1} (the wall clock stepped back, or the record is corrupt)",
    ),
]


@pytest.mark.parametrize(
    ("case", "log", "kills", "detail"), KILL_ERRORS, ids=[c for c, _, _, _ in KILL_ERRORS]
)
def test_kill_input_errors(
    tmp_path: Path,
    case: str,
    log: list[Line] | dict[int, list[Line]],
    kills: list[dict[str, object]],
    detail: str,
) -> None:
    """A pre_kill line with no 'faultctl api kill N' fault in context.json to match (the k-th
    pre_kill line of api-N.jsonl takes the k-th by start_ns), or whose kill failed or did not run,
    two kills of one instance that overlap (a kill must start no earlier than the one before it
    ended), and a restarted process's request arriving before the kill that ended its predecessor
    started each fail the run as input, naming the line and the field."""
    apis = log if isinstance(log, dict) else {1: log}
    world = make_world(tmp_path, closed_apis=apis, closed_faults=[STORE_DOWN, *kills])
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    assert rest(lines)[0] == f"FAIL {CLOSED} {IDS['closed']} input: {detail}", (case, lines)


# SPEC REF-9 (d) (F1 item R1): refunds are guaranteed only "while none of these occurs before the
# refund is confirmed: a new epoch (ADM-1, either form), epoch-wide state loss (STA-9, including an
# integrity hold), a clock_error that, together with store unavailability, lasts longer than
# outage_tolerance, or a quarantine of the key (STA-10)". d1 (key u000003, its request at ARRIVED)
# is excused when its final event is not a confirmation and a tamper that ended after ARRIVED and
# began no later than d1's final event may have come (F1 item B5) either lost the epoch's state
# itself, ending at most at LOSS_BOUND, or left damage whose event (the integrity hold of a
# retyped shared key, or a quarantine of d1's key) the run logs show by LOSS_BOUND (judgment RP3):
# a request on d1's user answered ERR-11 that arrived no earlier than the tamper started and whose
# acquire was done, at the latest, by LOSS_BOUND. LOSS_BOUND is the earliest wall-clock time of
# d1's REF-5 deadline: its request's arrival + refund_deadline + the closed run's measured outage,
# less 4 % of that for the clocks' drift (F1 item B6: ⌈34,085,364,542 × 40,000 / 10^6⌉ =
# 1,363,414,582 ns, rounded up from 1,363,414,581.68).
BOUND_SLEW_NS = 1_363_414_582
LOSS_BOUND = ARRIVED + DEADLINE_NS + OUTAGE_NS - BOUND_SLEW_NS
WITHIN = ARRIVED + 2 * SECOND  # a fault's end inside the bound
TBOUND = 2**53 - 2**43  # DESIGN §A4.2: an hwm at or above it is malformed
# An ERR-11 refusal (integrity_refusal) has limiter_ns 1.9 ms, so total_ns 2 ms (synthetic.request
# adds 100 µs), and its acquire was done at most REFUSAL_BY_NS after its arrival on the wall clock:
# that total_ns plus 4 % of it (80,000 ns exactly; SLEW_PPM ± 1 moves it by 2 ns).
REFUSED_LIMITER_NS = 1_900_000
REFUSAL_BY_NS = 2 * MS + 80 * US
# test_ref9d's refusal on d1's user: 3 s after its request, after each deferred loss there began
# (unless a case says otherwise), and done long before LOSS_BOUND.
REFUSED_AT = ARRIVED + 3 * SECOND


def integrity_refusal(
    arrival_ns: int,
    *,
    user: str = "u000003",
    instance: int = 1,
    status: int = 503,
    error: str = "limiter_state_integrity_error",
    result: str = "state_integrity_error",
) -> Line:
    """A request on user answered ERR-11 (HARNESS §3: 503 limiter_state_integrity_error; DESIGN
    §B8: the limiter's result, state_integrity_error; no permit and no database work), arriving
    at arrival_ns on instance: its acquire was done by arrival_ns + REFUSAL_BY_NS."""
    return request(
        instance,
        f"q-e11-{arrival_ns}",
        arrival_ns=arrival_ns,
        status=status,
        error=error,
        result=result,
        user=user,
        limiter_ns=REFUSED_LIMITER_NS,
        db_ns=0,
    )


def fault_record(
    cmd: str, end_ns: int, *, start_ns: int | None = None, error: str = ""
) -> dict[str, object]:
    """context.json's record of a faultctl command other than api kill (plan H4 step 7,
    runFaultctl): no instance and no targets, the command's own start (start_ns, or 8 ms before its
    end) and end on the wall clock, both 0 when it did not run; error when it failed."""
    if start_ns is None:
        start_ns = end_ns - 8 * MS if end_ns else 0
    rec: dict[str, object] = {
        "at_s": 9,
        "cmd": cmd,
        "instance": None,
        "start_ns": start_ns,
        "end_ns": end_ns,
        "outage_start_ns": None,
        "outage_ns": None,
        "targets": [],
    }
    if error:
        rec["error"] = error
    return rec


def tamper_fault(
    op: str, end_ns: int, *, start_ns: int | None = None, error: str = ""
) -> dict[str, object]:
    """context.json's record of faultctl tamper <op> (fault_record)."""
    return fault_record(f"faultctl tamper {op}", end_ns, start_ns=start_ns, error=error)


# d1's refund unresolved (retention) when its retention timer fired: receipt_lifetime after its
# acquire, which came at most limiter_ns (2 ms) before its release, so after every tamper below.
RETAINED_NS = LIFETIME_NS - 2 * MS
RETENTION = final_event("d1", "unresolved", reason="retention", took_ns=RETAINED_NS)
LATE_NS = DEADLINE_NS + OUTAGE_NS + 1  # a final event 1 ns past refund_deadline + the outage
DEL_HWM = tamper_fault("del-hwm", WITHIN)
DEL_K = "del-k --key u000003"
# A final event 3 s after its release: the refund's next attempt after a tamper ending at WITHIN.
AFTER_WITHIN_NS = 3 * SECOND
# F1 item B5: a REF-9 (d) event excuses only a final event that may have come after the loss began.
# d1's request took total_ns 2.1 ms (synthetic.request: limiter_ns 2 ms + db_ns 0 + 100 µs), which
# hitapi measures past the release, and EARLY_FINAL came 2 s + 7 ns after the release on the
# library's monotonic clock (final_mono_ns − the refund line's release_mono_ns), so on the wall
# clock it came at most FINAL_BY: ARRIVED + 2,002,100,007 ns + 80,084,001 ns of slew (4 % of that,
# 80,084,000.28, rounded up; SLEW_PPM ± 1 moves it by about 2,000 ns).
EARLY_FINAL = final_event("d1", "receipt_invalid", took_ns=2 * SECOND + 7)
FINAL_BY = ARRIVED + 2_002_100_007 + 80_084_001
QUICK_INVALID = final_event("d1", "receipt_invalid", took_ns=MS)  # 1 ms after its release
WELL_FORMED_ID = "0123456789abcdef" * 2  # 32 lowercase hex digits
# (case, the closed run's faults after its store down, d1's final event, how d1 is excused: lost,
# quarantined, or None when it fails).
REF9D: list[tuple[str, list[dict[str, object]], Line, str | None]] = [
    ("del-hwm ending at the bound", [tamper_fault("del-hwm", LOSS_BOUND)], RETENTION, "lost"),
    (
        "del-hwm ending 1 ns past the bound",
        [tamper_fault("del-hwm", LOSS_BOUND + 1)],
        RETENTION,
        None,
    ),
    (
        "del-hwm ending at the request's arrival",
        [tamper_fault("del-hwm", ARRIVED)],
        RETENTION,
        None,
    ),
    (
        "del-hwm ending 1 ns after the request's arrival",
        [tamper_fault("del-hwm", ARRIVED + 1)],
        RETENTION,
        "lost",
    ),
    # Each tamper that loses the epoch's state itself, at the bound and 1 ns past it.
    ("wipe ending at the bound", [tamper_fault("wipe", LOSS_BOUND)], RETENTION, "lost"),
    ("wipe ending 1 ns past the bound", [tamper_fault("wipe", LOSS_BOUND + 1)], RETENTION, None),
    (
        "corrupt-record (its schema) ending at the bound",
        [tamper_fault("corrupt-record", LOSS_BOUND)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record (its schema) ending 1 ns past the bound",
        [tamper_fault("corrupt-record", LOSS_BOUND + 1)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of gen with the tampered value",
        [tamper_fault("corrupt-record --field gen", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record setting the hold",
        [tamper_fault("corrupt-record --field hold --value x", WITHIN)],
        RETENTION,
        "lost",
    ),
    # F1 item B21: without --value, corrupt-record --field writes TamperedValue ("tampered":
    # harness/fixtures TamperCommand.check), which sets the hold (load_epoch's hold ~= '').
    (
        "corrupt-record of hold with the tampered value",
        [tamper_fault("corrupt-record --field hold", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of hwm at TBOUND",
        [tamper_fault(f"corrupt-record --field hwm --value {TBOUND}", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of hwm 1 below TBOUND, still well-formed",
        [tamper_fault(f"corrupt-record --field hwm --value {TBOUND - 1}", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record leaving a counter well-formed",
        [tamper_fault("corrupt-record --field nb --value 7", WITHIN)],
        RETENTION,
        None,
    ),
    # Each counter field against is_uint53, both ways (F1 items B1 and B2): the tampered value
    # loses the epoch, 7 does not; and hwm's own rule (is_uint53, then below TBOUND).
    (
        "corrupt-record of nb with the tampered value",
        [tamper_fault("corrupt-record --field nb", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of nm with the tampered value",
        [tamper_fault("corrupt-record --field nm", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of nm 7, still well-formed",
        [tamper_fault("corrupt-record --field nm --value 7", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of origin with the tampered value",
        [tamper_fault("corrupt-record --field origin", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of origin 7, still well-formed",
        [tamper_fault("corrupt-record --field origin --value 7", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of created with the tampered value",
        [tamper_fault("corrupt-record --field created", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of created 7, still well-formed",
        [tamper_fault("corrupt-record --field created --value 7", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of hwm with the tampered value",
        [tamper_fault("corrupt-record --field hwm", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of hwm with a leading zero",
        [tamper_fault("corrupt-record --field hwm --value 07", WITHIN)],
        RETENTION,
        "lost",
    ),
    # DESIGN §A4.2's is_uint53, which the counters and the bucket fields take: 1–16 decimal digits,
    # no leading zero but "0", below 2^53.
    (
        "corrupt-record of gen at 2^53",
        [tamper_fault(f"corrupt-record --field gen --value {2**53}", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of gen at 2^53 - 1, still well-formed",
        [tamper_fault(f"corrupt-record --field gen --value {2**53 - 1}", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of gen with a leading zero",
        [tamper_fault("corrupt-record --field gen --value 07", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of gen 0, still well-formed",
        [tamper_fault("corrupt-record --field gen --value 0", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of a field the record does not hold",
        [tamper_fault("corrupt-record --field extra", WITHIN)],
        RETENTION,
        None,
    ),
    # Each record field against its grammar (DESIGN §A4.2 record_wellformed): a value it refuses
    # loses the epoch, one it takes does not (a well-formed fp that is not the instances' is a
    # config_error, which REF-9 (d) does not list; a well-formed id that is not the epoch's moves
    # the instances to that id's empty namespace, DESIGN §A5.7, though no ADM-1 started an epoch,
    # the only kind REF-9 (d) names).
    (
        "corrupt-record of fp with the tampered value",
        [tamper_fault("corrupt-record --field fp", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of fp with another well-formed fp",
        [tamper_fault(f"corrupt-record --field fp --value {WELL_FORMED_ID}", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of id with another well-formed id",
        [tamper_fault(f"corrupt-record --field id --value {'0' * 31}1", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of id with uppercase hex",
        [tamper_fault(f"corrupt-record --field id --value {'A' * 32}", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of id with 31 hex digits",
        [tamper_fault(f"corrupt-record --field id --value {WELL_FORMED_ID[:31]}", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of id with 33 hex digits",
        [tamper_fault(f"corrupt-record --field id --value {WELL_FORMED_ID}0", WITHIN)],
        RETENTION,
        "lost",
    ),
    # F1 item B15: 32 lowercase letters past f are no hex digits.
    (
        "corrupt-record of id with 32 lowercase non-hex letters",
        [tamper_fault(f"corrupt-record --field id --value {'g' * 32}", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of schema 2",
        [tamper_fault("corrupt-record --field schema --value 2", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of schema 3",
        [tamper_fault("corrupt-record --field schema --value 3", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of schema 1, still well-formed",
        [tamper_fault("corrupt-record --field schema --value 1", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of clock with the tampered value",
        [tamper_fault("corrupt-record --field clock", WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "corrupt-record of clock controlled, still well-formed",
        [tamper_fault("corrupt-record --field clock --value controlled", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record of clock store, still well-formed",
        [tamper_fault("corrupt-record --field clock --value store", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-record clearing the hold",
        [tamper_fault("corrupt-record --field hold --value=", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "retype-shared of the epoch record ending at the bound",
        [tamper_fault("retype-shared --target epoch", LOSS_BOUND)],
        RETENTION,
        "lost",
    ),
    (
        "retype-shared of the epoch record ending 1 ns past the bound",
        [tamper_fault("retype-shared --target epoch", LOSS_BOUND + 1)],
        RETENTION,
        None,
    ),
    # A shared key of the namespace retyped: the integrity hold comes at the next store step that
    # reads it, which the run logs show by a request on d1's user answered ERR-11 (judgment RP3):
    # REFUSED_AT's refusal shows a retype that began when it arrived, not one that began 1 ns
    # later. (Each such retype ends 8 ms after it starts, 371 ms past the bound DESIGN §A7's
    # margin used to set, which no longer counts.)
    (
        "retype-shared of ix starting when d1's refusal arrived",
        [tamper_fault("retype-shared --target ix", REFUSED_AT + 8 * MS, start_ns=REFUSED_AT)],
        RETENTION,
        "lost",
    ),
    (
        "retype-shared of ix starting 1 ns after d1's refusal arrived",
        [
            tamper_fault(
                "retype-shared --target ix", REFUSED_AT + 1 + 8 * MS, start_ns=REFUSED_AT + 1
            )
        ],
        RETENTION,
        None,
    ),
    (
        "retype-shared of q starting when d1's refusal arrived",
        [tamper_fault("retype-shared --target q", REFUSED_AT + 8 * MS, start_ns=REFUSED_AT)],
        RETENTION,
        "lost",
    ),
    (
        "retype-shared of q starting 1 ns after d1's refusal arrived",
        [
            tamper_fault(
                "retype-shared --target q", REFUSED_AT + 1 + 8 * MS, start_ns=REFUSED_AT + 1
            )
        ],
        RETENTION,
        None,
    ),
    (
        "retype-shared of ix ending at the request's arrival",
        [tamper_fault("retype-shared --target ix", ARRIVED)],
        RETENTION,
        None,
    ),
    (
        "retype-shared of ix ending 1 ns after the request's arrival",
        [tamper_fault("retype-shared --target ix", ARRIVED + 1)],
        RETENTION,
        "lost",
    ),
    (
        "add-bad-ix, whose hold a later cleanup pass sets",
        [tamper_fault("add-bad-ix", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "add-bad-ix with its member and score",
        [tamper_fault("add-bad-ix --member !x --value 3", WITHIN)],
        RETENTION,
        None,
    ),
    ("del-hwm with end_ns 0 (it did not run)", [tamper_fault("del-hwm", 0)], RETENTION, None),
    (
        "del-hwm that did not run, with its error",
        [tamper_fault("del-hwm", 0, error="not run: hitgen exited before its time")],
        RETENTION,
        None,
    ),
    (
        "del-hwm that failed",
        [tamper_fault("del-hwm", WITHIN, error="fixtures: tamper DeleteHwm: HDEL removed :0")],
        RETENTION,
        None,
    ),
    # Judgment RP2: a tamper's end at its own start is taken (test_fault_times_input_errors
    # refuses it 1 ns earlier).
    (
        "del-hwm ending as it starts",
        [tamper_fault("del-hwm", WITHIN, start_ns=WITHIN)],
        RETENTION,
        "lost",
    ),
    (
        "del-k of its key ending as it starts",
        [tamper_fault(DEL_K, WITHIN, start_ns=WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "receipt_invalid",
        [DEL_HWM],
        final_event("d1", "receipt_invalid", took_ns=AFTER_WITHIN_NS),
        "lost",
    ),
    (
        "receipt_expired",
        [DEL_HWM],
        final_event("d1", "receipt_expired", took_ns=RETAINED_NS),
        "lost",
    ),
    (
        "unresolved (expired)",
        [DEL_HWM],
        final_event("d1", "unresolved", reason="expired", took_ns=RETAINED_NS),
        "lost",
    ),
    (
        "unresolved (closed) past its drain's deadline",
        [DEL_HWM],
        final_event("d1", "unresolved", reason="closed", took_ns=LATE_NS),
        "lost",
    ),
    ("applied 1 ns late", [DEL_HWM], final_event("d1", took_ns=LATE_NS), None),
    (
        "already_applied 1 ns late",
        [DEL_HWM],
        final_event("d1", "already_applied", took_ns=LATE_NS),
        None,
    ),
    # Damage to d1's key: its quarantine shows by REFUSED_AT's refusal, for a tamper that began
    # when that request arrived, not 1 ns later (judgment RP3).
    (
        "del-k of its key starting when d1's refusal arrived",
        [tamper_fault(DEL_K, REFUSED_AT + 8 * MS, start_ns=REFUSED_AT)],
        RETENTION,
        "quarantined",
    ),
    (
        "del-k of its key starting 1 ns after d1's refusal arrived",
        [tamper_fault(DEL_K, REFUSED_AT + 1 + 8 * MS, start_ns=REFUSED_AT + 1)],
        RETENTION,
        None,
    ),
    (
        "del-k of its key ending at the request's arrival",
        [tamper_fault(DEL_K, ARRIVED)],
        RETENTION,
        None,
    ),
    (
        "del-k of its key ending 1 ns after the request's arrival",
        [tamper_fault(DEL_K, ARRIVED + 1)],
        RETENTION,
        "quarantined",
    ),
    ("del-k of another key", [tamper_fault("del-k --key u000009", WITHIN)], RETENTION, None),
    (
        "del-bucket-fields of its key",
        [tamper_fault("del-bucket-fields --key u000003 --field u", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "del-bucket-fields of all six fields of its key (no --field)",
        [tamper_fault("del-bucket-fields --key u000003", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "del-marker of its key, its member at int64's largest",
        [tamper_fault(f"del-marker --key u000003 --member {2**63 - 1}:0", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    ("del-mi of its key", [tamper_fault("del-mi --key u000003", WITHIN)], RETENTION, "quarantined"),
    (
        "del-mi-member of its key",
        [tamper_fault("del-mi-member --key u000003 --member 1:1", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    ("del-ix of its key", [tamper_fault("del-ix --key u000003", WITHIN)], RETENTION, "quarantined"),
    (
        "del-marker of its key",
        [tamper_fault("del-marker --key u000003 --member 1:1", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "corrupt-field of its key with a malformed value",
        [tamper_fault("corrupt-field --key u000003 --field u --value tampered", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "corrupt-field of a field no bucket holds",
        [tamper_fault("corrupt-field --key u000003 --field extra --value 1", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "corrupt-field of its key with a well-formed value",
        [tamper_fault("corrupt-field --key u000003 --field u --value 5", WITHIN)],
        RETENTION,
        None,
    ),
    # Each bucket field takes a well-formed value (DESIGN §A3.2's six; F1 item B1).
    (
        "corrupt-field of g with a well-formed value",
        [tamper_fault("corrupt-field --key u000003 --field g --value 5", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-field of s with a well-formed value",
        [tamper_fault("corrupt-field --key u000003 --field s --value 5", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-field of x with a well-formed value",
        [tamper_fault("corrupt-field --key u000003 --field x --value 5", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-field of mc with a well-formed value",
        [tamper_fault("corrupt-field --key u000003 --field mc --value 5", WITHIN)],
        RETENTION,
        None,
    ),
    # F1 item B15: mc is a bucket field, not a marker field (m:<g>:<s>), so is_uint53 checks it.
    (
        "corrupt-field of mc with a malformed value",
        [tamper_fault("corrupt-field --key u000003 --field mc --value tampered", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "corrupt-field of its key with a leading zero",
        [tamper_fault("corrupt-field --key u000003 --field u --value 01", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "corrupt-field of its key at 2^53",
        [tamper_fault(f"corrupt-field --key u000003 --field t --value {2**53}", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "corrupt-field of its key at 2^53 - 1, still well-formed",
        [tamper_fault(f"corrupt-field --key u000003 --field t --value {2**53 - 1}", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-field of a marker field",
        [tamper_fault("corrupt-field --key u000003 --field m:1:1 --value x", WITHIN)],
        RETENTION,
        None,
    ),
    # F1 item B19: a marker field is one whose name starts with m: (check_key's string.sub(f, 1, 2)
    # == 'm:' in internal/scripts/prelude.lua), whatever follows; xm:1, which holds m: only past its
    # start, is no field a bucket holds, so check_key quarantines its key (malformed_bucket).
    (
        "corrupt-field of a marker field of generation 2",
        [tamper_fault("corrupt-field --key u000003 --field m:2:1 --value x", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "corrupt-field of a field holding m: past its start",
        [tamper_fault("corrupt-field --key u000003 --field xm:1 --value 1", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    # F1 item B21: m: alone starts with m: too, so it is a marker field, which excuses nothing.
    (
        "corrupt-field of the bare marker field m:",
        [tamper_fault("corrupt-field --key u000003 --field m: --value x", WITHIN)],
        RETENTION,
        None,
    ),
    ("del-k --key=K", [tamper_fault("del-k --key=u000003", WITHIN)], RETENTION, "quarantined"),
    ("del-k -key K", [tamper_fault("del-k -key u000003", WITHIN)], RETENTION, "quarantined"),
    # Go's flag package keeps a repeated flag's last value, as faultctl does.
    (
        "del-k --key given twice, its key last",
        [tamper_fault("del-k --key u000009 --key u000003", WITHIN)],
        RETENTION,
        "quarantined",
    ),
    (
        "del-k --key given twice, another key last",
        [tamper_fault("del-k --key u000003 --key=u000009", WITHIN)],
        RETENTION,
        None,
    ),
    (
        "a tamper of its key and an epoch-wide loss: lost",
        [tamper_fault(DEL_K, WITHIN), DEL_HWM],
        RETENTION,
        "lost",
    ),
    (
        "a tamper of its key and a shared key retyped: lost",
        [tamper_fault(DEL_K, WITHIN), tamper_fault("retype-shared --target ix", WITHIN)],
        RETENTION,
        "lost",
    ),
    # Go's flag syntax, as faultctl parses the words (F1 item B4): a lone "--" ends the flags, and
    # U+001F, which Python's str.split takes for white space, is none to strings.Fields.
    ("del-hwm with a lone --", [tamper_fault("del-hwm --", WITHIN)], RETENTION, "lost"),
    (
        "corrupt-record of gen with U+001F inside its value",
        [tamper_fault("corrupt-record --field gen --value 1\x1f2", WITHIN)],
        RETENTION,
        "lost",
    ),
    # F1 item B5: a final event that came before the loss began was not its effect. EARLY_FINAL
    # came at most at FINAL_BY on the wall clock: a tamper starting then excuses it, one starting
    # 1 ns later does not (each ending 8 ms after it starts, inside its bound).
    (
        "del-hwm starting when the final event came at the latest",
        [tamper_fault("del-hwm", FINAL_BY + 8 * MS, start_ns=FINAL_BY)],
        EARLY_FINAL,
        "lost",
    ),
    (
        "del-hwm starting 1 ns after the final event came at the latest",
        [tamper_fault("del-hwm", FINAL_BY + 1 + 8 * MS, start_ns=FINAL_BY + 1)],
        EARLY_FINAL,
        None,
    ),
    (
        "del-k of its key starting when the final event came at the latest",
        [tamper_fault(DEL_K, FINAL_BY + 8 * MS, start_ns=FINAL_BY)],
        EARLY_FINAL,
        "quarantined",
    ),
    (
        "del-k of its key starting 1 ns after the final event came at the latest",
        [tamper_fault(DEL_K, FINAL_BY + 1 + 8 * MS, start_ns=FINAL_BY + 1)],
        EARLY_FINAL,
        None,
    ),
    (
        "retype-shared of ix starting when the final event came at the latest",
        [tamper_fault("retype-shared --target ix", FINAL_BY + 8 * MS, start_ns=FINAL_BY)],
        EARLY_FINAL,
        "lost",
    ),
    (
        "retype-shared of ix starting 1 ns after the final event came at the latest",
        [tamper_fault("retype-shared --target ix", FINAL_BY + 1 + 8 * MS, start_ns=FINAL_BY + 1)],
        EARLY_FINAL,
        None,
    ),
    # An early library failure stays a failure: receipt_invalid 1 ms after the release, then a
    # tamper within the bounds (the auditors' cases).
    (
        "receipt_invalid 1 ms after its release, then del-k of its key 2.6 s after its arrival",
        [tamper_fault(DEL_K, ARRIVED + 2_600 * MS)],
        QUICK_INVALID,
        None,
    ),
    (
        "receipt_invalid 1 ms after its release, then del-hwm 20 s after its arrival",
        [tamper_fault("del-hwm", ARRIVED + 20 * SECOND)],
        QUICK_INVALID,
        None,
    ),
]


@pytest.mark.parametrize(
    ("case", "faults", "final", "excused"), REF9D, ids=[c for c, _, _, _ in REF9D]
)
def test_ref9d(
    tmp_path: Path, case: str, faults: list[dict[str, object]], final: Line, excused: str | None
) -> None:
    """A release whose final event is not a confirmation (unresolved, receipt_invalid,
    receipt_expired) is excused by REF-9 (d), and counted lost or quarantined, when a context.json
    fault that ran without error ended after its request's arrival, began no later than its final
    event may have come (F1 item B5), and: loses the epoch's state at once (del-hwm, wipe,
    retype-shared of the epoch record, a corrupt-record that leaves the record malformed or held,
    DESIGN §A4.2) no later than the earliest wall-clock time of its REF-5 deadline (its arrival +
    refund_deadline + the measured outage, less the slew allowance: F1 item B6); or leaves damage
    whose next read sets the integrity hold (retype-shared of ix or q, §A3.3) or quarantines its
    key (a deletion, or a corrupt-field of a field that is neither a bucket field nor a marker
    field, or of a bucket field with a value is_uint53 refuses, §A3.3), and began no later than
    the run log's request on d1's user answered ERR-11 arrived (REFUSED_AT, done long before the
    bound; judgment RP3). 1 ns later, a tamper that leaves the state well-formed, one whose hold
    only a cleanup pass sets (add-bad-ix), a marker field's, another key's, one that did not run
    or failed, one that began after the final event, or a confirmed refund, however late, excuses
    nothing. With both an epoch-wide event or a retyped shared key and a tamper of its key, it
    counts as lost."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", final), integrity_refusal(REFUSED_AT)]),
        closed_faults=[STORE_DOWN, *faults],
    )
    code, lines = check(world.manifest())
    assert code == 1, case
    lost, quarantined = int(excused == "lost"), int(excused == "quarantined")
    assert lines[-1].endswith(f", after_p 0, lost {lost}, quarantined {quarantined}"), (
        case,
        lines,
    )
    if excused is not None:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert "deduction d1: " in rest(lines)[0], (case, lines)


# d1's request arriving 2 ms before its acquire's decision_ns (DECIDED, the budget time), as its
# limiter_ns (2 ms) allows.
EARLY = ARRIVED - 2 * MS


@pytest.mark.parametrize("late", [0, 1], ids=["at the bound", "1 ns past it"])
@pytest.mark.parametrize(
    ("case", "op", "deferred", "excused"),
    [
        ("del-hwm", "del-hwm", False, "lost 1, quarantined 0"),
        ("del-k of its key", DEL_K, True, "lost 0, quarantined 1"),
        ("retype-shared of ix", "retype-shared --target ix", True, "lost 1, quarantined 0"),
    ],
    ids=["del-hwm", "del-k of its key", "retype-shared of ix"],
)
def test_ref9d_bounds_from_the_arrival(
    tmp_path: Path, case: str, op: str, deferred: bool, excused: str, late: int
) -> None:
    """REF-9 (d)'s bound counts from the request's arrival_ns (the wall clock the fault times
    share, before the release), not from the acquire's decision_ns, 2 ms later here: an epoch-wide
    loss ending at the bound the arrival gives, or, for a deferred loss (here ending at WITHIN),
    the ERR-11 refusal on d1's user that shows its event done by that bound, excuses d1; 1 ns later
    neither does, though within the bound decision_ns would give."""
    bound = EARLY + DEADLINE_NS + OUTAGE_NS - BOUND_SLEW_NS
    unconfirmed = released("d1", RETENTION, arrival_ns=EARLY)
    if deferred:
        log = [*unconfirmed, integrity_refusal(bound - REFUSAL_BY_NS + late)]
        fault = tamper_fault(op, WITHIN)
    else:
        log, fault = unconfirmed, tamper_fault(op, bound + late)
    world = make_world(tmp_path, closed=s6_lines("closed", log), closed_faults=[STORE_DOWN, fault])
    code, lines = check(world.manifest())
    assert code == 1, case
    if not late:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        assert lines[-1].endswith(f", after_p 0, {excused}"), (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert lines[-1].endswith(", after_p 0, lost 0, quarantined 0"), (case, lines)


def d1_log(after: Sequence[Line] = ()) -> list[Line]:
    """The closed run's log (s6_lines) with d1 released, its refund unresolved (retention), then
    after's lines."""
    return s6_lines("closed", [*released("d1", RETENTION), *after])


def next_process(*lines: Line) -> list[Line]:
    """Another hitapi process's lines, its start and ready lines, lines and its stop line."""
    return [lifecycle_start(), lifecycle("ready"), *lines, lifecycle("stop")]


DEL_K_WITHIN = tamper_fault(DEL_K, WITHIN)  # starting at WITHIN - 8 ms
IX_WITHIN = tamper_fault("retype-shared --target ix", WITHIN)
Q_WITHIN = tamper_fault("retype-shared --target q", WITHIN)
TAMPER_START = WITHIN - 8 * MS  # when each of them started
# A refusal arriving at REFUSED_AT_BOUND was done exactly at LOSS_BOUND.
REFUSED_AT_BOUND = LOSS_BOUND - REFUSAL_BY_NS
OTHER_USER = "u000009"
# A deferred loss (a retyped shared key, or damage to d1's key) excuses d1 only once the run logs
# show its event (judgment RP3): a request on d1's user answered ERR-11, arriving no earlier than
# the tamper started and done by LOSS_BOUND, in any instance's log, anywhere in it. (case, the
# closed run's faults after its store down, its instances' run logs, how d1 is excused: lost,
# quarantined, or None when it fails.)
REF9D_EVIDENCE: list[tuple[str, list[dict[str, object]], dict[int, list[Line]], str | None]] = [
    # The refusal done at the bound, and 1 ns past it, for each deferred loss.
    (
        "del-k of its key, its refusal done at the bound",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND)])},
        "quarantined",
    ),
    (
        "del-k of its key, its refusal done 1 ns past the bound",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND + 1)])},
        None,
    ),
    (
        "retype-shared of ix, its refusal done at the bound",
        [IX_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND)])},
        "lost",
    ),
    (
        "retype-shared of ix, its refusal done 1 ns past the bound",
        [IX_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND + 1)])},
        None,
    ),
    (
        "retype-shared of q, its refusal done at the bound",
        [Q_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND)])},
        "lost",
    ),
    (
        "retype-shared of q, its refusal done 1 ns past the bound",
        [Q_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND + 1)])},
        None,
    ),
    # The refusal arriving when the tamper started, and 1 ns before (its arrival_ns and the
    # fault's start_ns are both the wall clock).
    (
        "del-k of its key, its refusal arriving when it started",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START)])},
        "quarantined",
    ),
    (
        "del-k of its key, its refusal arriving 1 ns before it started",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START - 1)])},
        None,
    ),
    (
        "retype-shared of ix, its refusal arriving when it started",
        [IX_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START)])},
        "lost",
    ),
    (
        "retype-shared of ix, its refusal arriving 1 ns before it started",
        [IX_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START - 1)])},
        None,
    ),
    (
        "retype-shared of q, its refusal arriving when it started",
        [Q_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START)])},
        "lost",
    ),
    (
        "retype-shared of q, its refusal arriving 1 ns before it started",
        [Q_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START - 1)])},
        None,
    ),
    # No refusal at all: nothing shows the event, whatever DESIGN §A7 says of the refund's next
    # attempt (the property the REF-5 check tests).
    ("del-k of its key, no refusal", [DEL_K_WITHIN], {1: d1_log()}, None),
    ("retype-shared of ix, no refusal", [IX_WITHIN], {1: d1_log()}, None),
    ("retype-shared of q, no refusal", [Q_WITHIN], {1: d1_log()}, None),
    # A refusal on another user only. For a retyped shared key too: an acquire of a key already
    # quarantined is refused at its step 3 without reading ix (DESIGN §A4.3), so another user's
    # refusal may be that user's own quarantine.
    (
        "del-k of its key, a refusal on another user",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT, user=OTHER_USER)])},
        None,
    ),
    (
        "retype-shared of ix, a refusal on another user",
        [IX_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT, user=OTHER_USER)])},
        None,
    ),
    (
        "retype-shared of q, a refusal on another user",
        [Q_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT, user=OTHER_USER)])},
        None,
    ),
    # A request line on d1's user that is not ERR-11 (HARNESS §3: 503, the error
    # limiter_state_integrity_error, the limiter's result state_integrity_error), in each field.
    (
        "del-k of its key, 503 limiter_outcome_unknown (ERR-9)",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT, error="limiter_outcome_unknown")])},
        None,
    ),
    (
        "del-k of its key, limiter_state_integrity_error with HTTP status 500",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT, status=500)])},
        None,
    ),
    (
        "del-k of its key, limiter_state_integrity_error with result outcome_unknown",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT, result="outcome_unknown")])},
        None,
    ),
    # Any instance's log, any process segment: d1 waits until every run log is read.
    (
        "del-k of its key, its refusal on instance 2",
        [DEL_K_WITHIN],
        {1: d1_log(), 2: next_process(integrity_refusal(REFUSED_AT, instance=2))},
        "quarantined",
    ),
    (
        "del-k of its key, its refusal from the next process on its instance",
        [DEL_K_WITHIN],
        {1: [*d1_log(), *next_process(integrity_refusal(REFUSED_AT))]},
        "quarantined",
    ),
    (
        "del-k of its key on instance 2, its refusal on instance 1",
        [DEL_K_WITHIN],
        {
            1: s6_lines("closed", [integrity_refusal(REFUSED_AT)]),
            2: next_process(*released("d1", RETENTION, instance=2)),
        },
        "quarantined",
    ),
    # The refusal done earliest of those that arrived after the tamper began counts, whatever
    # order they were written in: not one done past the bound, nor one before the tamper began.
    (
        "del-k of its key, a refusal done past the bound written before one in time",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT_BOUND + 1), integrity_refusal(REFUSED_AT)])},
        "quarantined",
    ),
    (
        "del-k of its key, a refusal before it began written before one in time",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(TAMPER_START - 1), integrity_refusal(REFUSED_AT)])},
        "quarantined",
    ),
    # Nor the one written last (judgment RB1): a quarantined key keeps refusing for the rest of
    # the run (STA-10), and completion reads one instance's log after another, so refusals done
    # past the bound follow the one in time, in its log or in a later one.
    (
        "del-k of its key, a refusal in time written before one done past the bound",
        [DEL_K_WITHIN],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT), integrity_refusal(REFUSED_AT_BOUND + 1)])},
        "quarantined",
    ),
    (
        "del-k of its key, a refusal in time, then one done past the bound on instance 2",
        [DEL_K_WITHIN],
        {
            1: d1_log(after=[integrity_refusal(REFUSED_AT)]),
            2: next_process(integrity_refusal(REFUSED_AT_BOUND + 1, instance=2)),
        },
        "quarantined",
    ),
    # Each deferred loss against its own start: here d1's key was damaged before the refusal, ix
    # retyped 1 ns after it was done, so the refusal shows the quarantine, not the hold. (A retype
    # that began before the refusal was done voids it, judgment RB5: REF9D_BEFORE_STORE_WIDE.)
    (
        "del-k of its key before its refusal, retype-shared of ix after it",
        [
            DEL_K_WITHIN,
            tamper_fault(
                "retype-shared --target ix",
                REFUSED_AT + REFUSAL_BY_NS + 1 + 8 * MS,
                start_ns=REFUSED_AT + REFUSAL_BY_NS + 1,
            ),
        ],
        {1: d1_log(after=[integrity_refusal(REFUSED_AT)])},
        "quarantined",
    ),
    # The tamper's own end does not bound the event: here it ends past the bound, and the
    # refusal, arriving as it began, shows the quarantine before it.
    (
        "del-k of its key ending past the bound, its refusal done before it",
        [tamper_fault(DEL_K, LOSS_BOUND + SECOND, start_ns=LOSS_BOUND - SECOND)],
        {1: d1_log(after=[integrity_refusal(LOSS_BOUND - SECOND)])},
        "quarantined",
    ),
]


@pytest.mark.parametrize(
    ("case", "faults", "apis", "excused"),
    REF9D_EVIDENCE,
    ids=[c for c, _, _, _ in REF9D_EVIDENCE],
)
def test_ref9d_evidence(
    tmp_path: Path,
    case: str,
    faults: list[dict[str, object]],
    apis: dict[int, list[Line]],
    excused: str | None,
) -> None:
    """A deferred REF-9 (d) loss excuses d1, unresolved on retention, only when a request line on
    d1's user answered ERR-11 (503 limiter_state_integrity_error, result state_integrity_error)
    arrived no earlier than the tamper started and was done, arrival_ns + total_ns + 4 % of it, by
    the earliest wall-clock time of d1's REF-5 deadline, from any instance and process: it shows
    d1's key quarantined, or the epoch's state lost or held, by then (judgment RP3). Otherwise d1
    fails with its own detail, not confirmed."""
    world = make_world(tmp_path, closed_apis=apis, closed_faults=[STORE_DOWN, *faults])
    code, lines = check(world.manifest())
    assert code == 1, case
    lost, quarantined = int(excused == "lost"), int(excused == "quarantined")
    assert lines[-1].endswith(f", after_p 0, lost {lost}, quarantined {quarantined}"), (
        case,
        lines,
    )
    if excused is not None:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert rest(lines)[0].endswith(
        "deduction d1: released 2000000 ns after just before its acquire (limiter_ns + db_ns, "
        "within P 5000000000): final event unresolved (retention): not confirmed"
    ), (case, lines)


def test_ref9d_waiting_keeps_its_place(tmp_path: Path) -> None:
    """Releases waiting on a deferred loss's evidence until every run log is read keep their
    places among the run's failures when nothing shows the event: d1 and d3 (del-k of their key,
    no refusal) fail before and after d2, whose receipt_invalid nothing excuses, as their release
    lines come."""
    d2 = released("d2", final_event("d2", "receipt_invalid", took_ns=MS), user="u000006")
    d3 = released("d3", final_event("d3", "unresolved", reason="retention", took_ns=RETAINED_NS))
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", RETENTION), *d2, *d3]),
        closed_faults=[STORE_DOWN, DEL_K_WITHIN],
    )
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")] * 3, lines
    shown = [line.partition("deduction ")[2].partition(":")[0] for line in rest(lines)]
    assert shown == ["d1", "d2", "d3"], lines
    assert lines[-1].endswith(", after_p 0, lost 0, quarantined 0"), lines


# Judgment RB3: an ERR-11 refusal shows a deferred loss's event only when it was done strictly
# before every tamper that ran and lost the epoch's state at once (del-hwm, wipe, retype-shared of
# the epoch, a corrupt-record that leaves the record lost or held) began, whatever release that
# loss may excuse: such a loss gives ERR-11 too, so a refusal done once it began may be that
# loss's. Here d1's final event came early (EARLY_FINAL, by FINAL_BY on the wall clock), its key
# was damaged before that (DEL_K_WITHIN, starting at TAMPER_START) unless a case says otherwise,
# and no loss excuses d1 itself: each began after FINAL_BY (F1 item B5), ended at d1's arrival, or
# did not run. d1's refusal arrived at REFUSED_AT, so it was done by REFUSED_BY.
REFUSED_BY = REFUSED_AT + REFUSAL_BY_NS
# (case, the closed run's faults after its store down, the lines after d1's in its log, how d1 is
# excused: lost, quarantined, or None when it fails).
REF9D_BEFORE_A_LOSS: list[tuple[str, list[dict[str, object]], list[Line], str | None]] = [
    # A loss that began when the refusal was done voids it; one that began 1 ns later does not.
    (
        "del-hwm starting when d1's refusal was done",
        [DEL_K_WITHIN, tamper_fault("del-hwm", REFUSED_BY + 8 * MS, start_ns=REFUSED_BY)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "del-hwm starting 1 ns after d1's refusal was done",
        [DEL_K_WITHIN, tamper_fault("del-hwm", REFUSED_BY + 1 + 8 * MS, start_ns=REFUSED_BY + 1)],
        [integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
    (
        "retype-shared of ix, del-hwm starting when d1's refusal was done",
        [IX_WITHIN, tamper_fault("del-hwm", REFUSED_BY + 8 * MS, start_ns=REFUSED_BY)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "retype-shared of ix, del-hwm starting 1 ns after d1's refusal was done",
        [IX_WITHIN, tamper_fault("del-hwm", REFUSED_BY + 1 + 8 * MS, start_ns=REFUSED_BY + 1)],
        [integrity_refusal(REFUSED_AT)],
        "lost",
    ),
    # Verifier B's residual (round 5): d1's key damaged before its final event, del-hwm after it,
    # which the causal rule refuses as an excuse, and the only refusal after del-hwm began.
    (
        "del-hwm after the final event, before the only refusal arrived",
        [DEL_K_WITHIN, tamper_fault("del-hwm", FINAL_BY + 1 + 8 * MS, start_ns=FINAL_BY + 1)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    # Every such loss in the run counts, in any order, even one that began before the key was
    # damaged (the strict side): here that one ended at d1's arrival, so it excuses nothing itself.
    (
        "del-hwm ending at d1's arrival, before its key was damaged",
        [tamper_fault("del-hwm", ARRIVED), DEL_K_WITHIN],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "a wipe before its key was damaged, listed after a del-hwm after d1's refusal",
        [DEL_K_WITHIN, tamper_fault("del-hwm", REFUSED_BY + SECOND), tamper_fault("wipe", ARRIVED)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "a wipe before its key was damaged, listed before a del-hwm after d1's refusal",
        [tamper_fault("wipe", ARRIVED), DEL_K_WITHIN, tamper_fault("del-hwm", REFUSED_BY + SECOND)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    # Each other tamper that loses the epoch's state at once, starting when the refusal was done.
    (
        "retype-shared of the epoch record starting when d1's refusal was done",
        [
            DEL_K_WITHIN,
            tamper_fault("retype-shared --target epoch", REFUSED_BY + 8 * MS, start_ns=REFUSED_BY),
        ],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "corrupt-record (its schema) starting when d1's refusal was done",
        [DEL_K_WITHIN, tamper_fault("corrupt-record", REFUSED_BY + 8 * MS, start_ns=REFUSED_BY)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    # A refusal done before the loss began still counts, whatever comes after it, in any order:
    # if the one done earliest is void, so is every later one.
    (
        "del-hwm after a refusal done in time, a refusal after del-hwm written first",
        [DEL_K_WITHIN, tamper_fault("del-hwm", REFUSED_AT + SECOND)],
        [integrity_refusal(REFUSED_AT + 2 * SECOND), integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
    # Only a loss that ran (end_ns not 0, as fault_times reads it) voids a refusal.
    (
        "del-hwm that did not run",
        [DEL_K_WITHIN, tamper_fault("del-hwm", 0)],
        [integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
    (
        "del-hwm with a start before d1's refusal but end_ns 0 (it did not run)",
        [DEL_K_WITHIN, tamper_fault("del-hwm", 0, start_ns=ARRIVED)],
        [integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
]


@pytest.mark.parametrize(
    ("case", "faults", "after", "excused"),
    REF9D_BEFORE_A_LOSS,
    ids=[c for c, _, _, _ in REF9D_BEFORE_A_LOSS],
)
def test_ref9d_evidence_before_epoch_loss(
    tmp_path: Path,
    case: str,
    faults: list[dict[str, object]],
    after: list[Line],
    excused: str | None,
) -> None:
    """An ERR-11 refusal on d1's user shows a deferred loss's event (d1's key quarantined, or a
    retyped shared key's hold) only when it was done, arrival_ns + total_ns + 4 % of it, strictly
    before every tamper that ran and lost the epoch's state at once began, in the run (judgment
    RB3): a loss of the epoch's state gives ERR-11 too. d1's receipt_invalid, which no loss here
    excuses itself, is then quarantined or lost; otherwise it fails with its own detail, not
    confirmed."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", EARLY_FINAL), *after]),
        closed_faults=[STORE_DOWN, *faults],
    )
    code, lines = check(world.manifest())
    assert code == 1, case
    lost, quarantined = int(excused == "lost"), int(excused == "quarantined")
    assert lines[-1].endswith(f", after_p 0, lost {lost}, quarantined {quarantined}"), (
        case,
        lines,
    )
    if excused is not None:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert rest(lines)[0].endswith(
        "deduction d1: released 2000000 ns after just before its acquire (limiter_ns + db_ns, "
        "within P 5000000000): final event receipt_invalid: not confirmed"
    ), (case, lines)


def started_at(op: str, start_ns: int, *, error: str = "") -> dict[str, object]:
    """context.json's record of faultctl tamper op that started at start_ns and ended 8 ms later
    (error when it failed)."""
    return tamper_fault(op, start_ns + 8 * MS, start_ns=start_ns, error=error)


# Judgment RB5: completion classifies each tamper operation by what it reaches, as faultctl's
# parser takes it (GO_TAMPER_FLAGS, harness/fixtures tamperFlags): a key-scoped one requires --key
# and writes only that key's state; every other one is store-wide. A store-wide tamper may set the
# integrity hold, after which every acquire is refused ERR-11 before its key is read, so an ERR-11
# refusal shows a deferred loss's event only when it was done strictly before every store-wide
# tamper that ran began, other than that loss itself, whatever its value; a key-scoped tamper never
# voids one. The store-wide operations, each with how a case names it: every target of
# retype-shared, add-bad-ix with and without its flags, corrupt-record of well-formed values (hwm
# 5 and gen 1 below an instance's floor set the hold store_rollback), of a field the record does
# not hold and of its schema, wipe and del-hwm (REF9D_BEFORE_A_LOSS pins del-hwm too).
STORE_WIDE_TAMPERS: list[tuple[str, str]] = [
    ("retype-shared --target ix", "retype-shared of ix"),
    ("retype-shared --target q", "retype-shared of q"),
    ("retype-shared --target epoch", "retype-shared of the epoch record"),
    ("add-bad-ix", "add-bad-ix"),
    ("add-bad-ix --member !x --value 3", "add-bad-ix with its member and score"),
    ("corrupt-record --field hwm --value 5", "corrupt-record of hwm 5, well-formed"),
    ("corrupt-record --field gen --value 1", "corrupt-record of gen 1, well-formed"),
    ("corrupt-record --field extra", "corrupt-record of a field the record does not hold"),
    (
        f"corrupt-record --field fp --value {WELL_FORMED_ID}",
        "corrupt-record of fp with another well-formed fp",
    ),
    ("corrupt-record", "corrupt-record (its schema)"),
    ("wipe", "wipe"),
    ("del-hwm", "del-hwm"),
]
# Each key-scoped operation on another user's key, and corrupt-field and del-ix (which removes
# only the key's own member of the shared ix) of d1's own key besides its loss.
KEY_SCOPED_TAMPERS: list[tuple[str, str]] = [
    ("del-bucket-fields --key u000009", "del-bucket-fields of another user's key"),
    ("del-k --key u000009", "del-k of another user's key"),
    ("del-mi --key u000009", "del-mi of another user's key"),
    ("del-mi-member --key u000009 --member 1:1", "del-mi-member of another user's key"),
    ("del-ix --key u000009", "del-ix of another user's key"),
    ("del-marker --key u000009 --member 1:1", "del-marker of another user's key"),
    ("corrupt-field --key u000009 --field u --value 01", "corrupt-field of another user's key"),
    ("corrupt-field --key u000003 --field u --value tampered", "corrupt-field of its own key"),
    ("del-ix --key u000003", "del-ix of its own key"),
]
# The store-wide tampers verifier 6 started at FINAL_BY + 1, after d1's final event (which the
# causal rule then refuses as d1's own excuse) and before d1's only refusal arrived.
PROBED_TAMPERS: list[tuple[str, str]] = [
    ("retype-shared --target ix", "retype-shared of ix"),
    ("retype-shared --target q", "retype-shared of q"),
    ("corrupt-record --field hwm --value 5", "corrupt-record of hwm 5"),
    ("corrupt-record --field gen --value 1", "corrupt-record of gen 1"),
    ("add-bad-ix", "add-bad-ix"),
]
IX_RETYPE, Q_RETYPE = "retype-shared --target ix", "retype-shared --target q"
FAILED_ADD_BAD_IX = "fixtures: tamper AddMalformedIndexMember: TYPE replied none, want zset"
# As REF9D_BEFORE_A_LOSS: d1's receipt_invalid came early (EARLY_FINAL, by FINAL_BY), its key was
# damaged before that (DEL_K_WITHIN) unless a case says otherwise, and its refusal arrived at
# REFUSED_AT, done by REFUSED_BY. (case, the closed run's faults after its store down, the lines
# after d1's in its log, how d1 is excused: lost, quarantined, or None when it fails.)
REF9D_BEFORE_STORE_WIDE: list[tuple[str, list[dict[str, object]], list[Line], str | None]] = [
    # Each store-wide tamper starting when d1's refusal was done voids it; 1 ns later it does not
    # (and, starting after FINAL_BY, it does not excuse d1 itself).
    *(
        (
            f"{name} starting when d1's refusal was done",
            [DEL_K_WITHIN, started_at(op, REFUSED_BY)],
            [integrity_refusal(REFUSED_AT)],
            None,
        )
        for op, name in STORE_WIDE_TAMPERS
    ),
    *(
        (
            f"{name} starting 1 ns after d1's refusal was done",
            [DEL_K_WITHIN, started_at(op, REFUSED_BY + 1)],
            [integrity_refusal(REFUSED_AT)],
            "quarantined",
        )
        for op, name in STORE_WIDE_TAMPERS
    ),
    # A key-scoped tamper starting when d1's refusal was done never voids it.
    *(
        (
            f"{name} starting when d1's refusal was done",
            [DEL_K_WITHIN, started_at(op, REFUSED_BY)],
            [integrity_refusal(REFUSED_AT)],
            "quarantined",
        )
        for op, name in KEY_SCOPED_TAMPERS
    ),
    # Verifier 6's probes: each store-wide tamper after the final event, before the only refusal
    # arrived.
    *(
        (
            f"{name} after the final event, before the only refusal arrived",
            [DEL_K_WITHIN, started_at(op, FINAL_BY + 1)],
            [integrity_refusal(REFUSED_AT)],
            None,
        )
        for op, name in PROBED_TAMPERS
    ),
    # A retyped ix or q is store-wide too, but its own refusal still shows its own event: only
    # another store-wide tamper, a second retype of the same key included, voids it.
    ("retype-shared of ix, its own refusal", [IX_WITHIN], [integrity_refusal(REFUSED_AT)], "lost"),
    ("retype-shared of q, its own refusal", [Q_WITHIN], [integrity_refusal(REFUSED_AT)], "lost"),
    (
        "retype-shared of ix, retype-shared of q starting when d1's refusal was done",
        [IX_WITHIN, started_at(Q_RETYPE, REFUSED_BY)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "retype-shared of ix, retype-shared of q starting 1 ns after d1's refusal was done",
        [IX_WITHIN, started_at(Q_RETYPE, REFUSED_BY + 1)],
        [integrity_refusal(REFUSED_AT)],
        "lost",
    ),
    (
        "retype-shared of ix, a second one starting when d1's refusal was done",
        [IX_WITHIN, started_at(IX_RETYPE, REFUSED_BY)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "retype-shared of ix, a second one starting 1 ns after d1's refusal was done",
        [IX_WITHIN, started_at(IX_RETYPE, REFUSED_BY + 1)],
        [integrity_refusal(REFUSED_AT)],
        "lost",
    ),
    # Only the loss's own tamper is exempt, by its place in context.json, not any tamper that
    # started with it (judgment RB6): faults due at the same offset go out together (the
    # orchestrator's runFaults), and start_ns is a wall-clock reading (whole µs in the committed
    # runs), so another store-wide tamper may start in the same ns as the loss, here TAMPER_START,
    # before d1's refusal arrived; it voids the refusal as any other does. A retype of q starting
    # with a retype of ix voids the refusal for the ix loss, and the ix retype voids it for q's, so
    # neither loss excuses d1.
    (
        "del-k of its key, add-bad-ix starting at the same ns",
        [DEL_K_WITHIN, started_at("add-bad-ix", TAMPER_START)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "retype-shared of ix, add-bad-ix starting at the same ns",
        [IX_WITHIN, started_at("add-bad-ix", TAMPER_START)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "retype-shared of ix, retype-shared of q starting at the same ns",
        [IX_WITHIN, started_at(Q_RETYPE, TAMPER_START)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    # A retype of ix that began after the refusal arrived, before it was done (REF9D_EVIDENCE's
    # shape until judgment RB5): the refusal may show its hold, not d1's quarantine.
    (
        "retype-shared of ix starting 1 ns after d1's refusal arrived",
        [DEL_K_WITHIN, started_at(IX_RETYPE, REFUSED_AT + 1)],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    # Every store-wide tamper in the run counts, even one that began before d1's key was damaged
    # and ended at d1's arrival, excusing nothing itself (the strict side).
    (
        "add-bad-ix ending at d1's arrival, before its key was damaged",
        [tamper_fault("add-bad-ix", ARRIVED), DEL_K_WITHIN],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    (
        "corrupt-record of hwm 5 ending at d1's arrival, before its key was damaged",
        [tamper_fault("corrupt-record --field hwm --value 5", ARRIVED), DEL_K_WITHIN],
        [integrity_refusal(REFUSED_AT)],
        None,
    ),
    # A refusal done before the store-wide tamper began still counts, in any order.
    (
        "add-bad-ix after a refusal done in time, a refusal after add-bad-ix written first",
        [DEL_K_WITHIN, tamper_fault("add-bad-ix", REFUSED_AT + SECOND)],
        [integrity_refusal(REFUSED_AT + 2 * SECOND), integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
    # Only a store-wide tamper that ran (no error, end_ns not 0: as losses_of and judgment RB3
    # read it) voids a refusal.
    (
        "add-bad-ix that failed, starting when d1's refusal was done",
        [DEL_K_WITHIN, started_at("add-bad-ix", REFUSED_BY, error=FAILED_ADD_BAD_IX)],
        [integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
    (
        "add-bad-ix that did not run",
        [DEL_K_WITHIN, tamper_fault("add-bad-ix", 0)],
        [integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
    (
        "add-bad-ix with a start before d1's refusal but end_ns 0 (it did not run)",
        [DEL_K_WITHIN, tamper_fault("add-bad-ix", 0, start_ns=ARRIVED)],
        [integrity_refusal(REFUSED_AT)],
        "quarantined",
    ),
]


@pytest.mark.parametrize(
    ("case", "faults", "after", "excused"),
    REF9D_BEFORE_STORE_WIDE,
    ids=[c for c, _, _, _ in REF9D_BEFORE_STORE_WIDE],
)
def test_ref9d_evidence_before_store_wide_tamper(
    tmp_path: Path,
    case: str,
    faults: list[dict[str, object]],
    after: list[Line],
    excused: str | None,
) -> None:
    """An ERR-11 refusal on d1's user shows a deferred loss's event (d1's key quarantined, or a
    retyped shared key's hold) only when it was done, arrival_ns + total_ns + 4 % of it, strictly
    before every store-wide tamper that ran began, other than that loss itself, in the run
    (judgment RB5): a store-wide tamper may set the integrity hold, which refuses every acquire
    ERR-11 too, whatever its kind or value. A key-scoped tamper, on another user's key or on d1's
    own, never voids it. d1's receipt_invalid, which no loss here excuses itself, is then
    quarantined or lost; otherwise it fails with its own detail, not confirmed."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", EARLY_FINAL), *after]),
        closed_faults=[STORE_DOWN, *faults],
    )
    code, lines = check(world.manifest())
    assert code == 1, case
    lost, quarantined = int(excused == "lost"), int(excused == "quarantined")
    assert lines[-1].endswith(f", after_p 0, lost {lost}, quarantined {quarantined}"), (
        case,
        lines,
    )
    if excused is not None:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)
    assert rest(lines)[0].endswith(
        "deduction d1: released 2000000 ns after just before its acquire (limiter_ns + db_ns, "
        "within P 5000000000): final event receipt_invalid: not confirmed"
    ), (case, lines)


# Judgment RP2: a tamper REF-9 (d) reads that ran (end_ns not 0) started (start_ns not 0) no later
# than it ended, as the orchestrator reads both from the wall clock around it: (case, the closed
# run's second fault, the input failure's detail). Its end at its start is taken (REF9D).
FAULT_TIME_ERRORS: list[tuple[str, dict[str, object], str]] = [
    (
        "del-hwm that ran with start_ns 0",
        tamper_fault("del-hwm", WITHIN, start_ns=0),
        f"context.json: faults[1]: field start_ns: 0, though it ran (end_ns {WITHIN}): want its "
        "start",
    ),
    (
        "del-hwm ending 1 ns before it started",
        tamper_fault("del-hwm", WITHIN, start_ns=WITHIN + 1),
        f"context.json: faults[1]: field end_ns: {WITHIN}: before its start_ns {WITHIN + 1} (the "
        "wall clock stepped back, or the record is corrupt)",
    ),
    (
        "del-k of its key that ran with start_ns 0",
        tamper_fault(DEL_K, WITHIN, start_ns=0),
        f"context.json: faults[1]: field start_ns: 0, though it ran (end_ns {WITHIN}): want its "
        "start",
    ),
    (
        "retype-shared of ix ending 1 ns before it started",
        tamper_fault("retype-shared --target ix", WITHIN, start_ns=WITHIN + 1),
        f"context.json: faults[1]: field end_ns: {WITHIN}: before its start_ns {WITHIN + 1} (the "
        "wall clock stepped back, or the record is corrupt)",
    ),
]


@pytest.mark.parametrize(
    ("case", "fault", "detail"), FAULT_TIME_ERRORS, ids=[c for c, _, _ in FAULT_TIME_ERRORS]
)
def test_fault_times_input_errors(
    tmp_path: Path, case: str, fault: dict[str, object], detail: str
) -> None:
    """A tamper that ran with start_ns 0 (the causal rule would never block it), or that ended
    before it started, fails the run as input, naming the fault and the field, before any
    exclusion applies (here d1 is unresolved on retention, with its ERR-11 refusal in the log)."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", RETENTION), integrity_refusal(REFUSED_AT)]),
        closed_faults=[STORE_DOWN, fault],
    )
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    assert rest(lines)[0] == f"FAIL {CLOSED} {IDS['closed']} input: {detail}", (case, lines)


def test_ref9d_on_instance_2(tmp_path: Path) -> None:
    """REF-9 (d) excuses a release on any instance (F1 item B3): in a two-instance closed run, d2
    on instance 2, unresolved (retention), counts as lost after del-hwm ended before its
    deadline."""
    unconfirmed = released(
        "d2",
        final_event("d2", "unresolved", reason="retention", took_ns=RETAINED_NS),
        instance=2,
        user="u000006",
    )
    apis = {
        1: s6_lines("closed"),
        2: [lifecycle_start(), lifecycle("ready"), *unconfirmed, lifecycle("stop")],
    }
    world = make_world(tmp_path, closed_apis=apis, closed_faults=[STORE_DOWN, DEL_HWM])
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], lines
    assert lines[-1].endswith(", after_p 0, lost 1, quarantined 0"), lines


def test_ref9d_never_excuses_a_missing_final_event(tmp_path: Path) -> None:
    """A release with no final event on an instance that was not killed fails though an epoch-wide
    state loss came before its deadline: REF-9 (d) scopes the refund, not the final event every
    released permit with a receipt gets (SPEC API-1d). (Its final event is taken out after the
    report was built.)"""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", released("d1", final_event("d1"))),
        closed_faults=[STORE_DOWN, DEL_HWM],
    )
    rewrite_log(world.run("closed"), drop_final("d1"))
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert "no final event, and its instance was not killed" in rest(lines)[0], lines
    assert lines[-1].endswith(", after_p 0, lost 0, quarantined 0"), lines


def test_ref9d_needs_the_request_line(tmp_path: Path) -> None:
    """A release without its request line has no arrival_ns, so nothing shows a loss came before
    its deadline: unconfirmed, it fails though del-hwm ran."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", released("d1", RETENTION, with_request=False)),
        closed_faults=[STORE_DOWN, DEL_HWM],
    )
    code, lines = check(world.manifest())
    assert code == 1
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], lines
    assert (
        "deduction d1: final event unresolved (retention): not confirmed; it has no request "
        in (rest(lines)[0])
    ), lines


def test_ref9d_after_the_drain_exclusion(tmp_path: Path) -> None:
    """A refund cut by a drain timeout before its deadline counts as drained (REF-9 (c), decision
    D3) though an epoch-wide state loss came before its deadline too: the drain exclusion is
    judged first."""
    drained = released("d1", final_event("d1", "unresolved", reason="closed"))
    world = make_world(
        tmp_path, closed=s6_lines("closed", drained), closed_faults=[STORE_DOWN, DEL_HWM]
    )
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], lines
    assert lines[-1].endswith(", drained 1, after_p 0, lost 0, quarantined 0"), lines


@pytest.mark.parametrize(
    ("case", "fault", "excused"),
    [
        ("del-hwm", DEL_HWM, "lost 1, quarantined 0"),
        ("del-k of its key", tamper_fault(DEL_K, WITHIN), "lost 0, quarantined 1"),
    ],
    ids=["del-hwm", "del-k of its key"],
)
@pytest.mark.parametrize("past_p", [1, 0], ids=["1 ns after P", "at P"])
def test_ref9d_after_p(
    tmp_path: Path, case: str, fault: dict[str, object], excused: str, past_p: int
) -> None:
    """P is judged before REF-9 (d): an unconfirmed release made 1 ns after P counts as released
    after P though the loss or the tamper of its key came before its deadline, the quarantine
    shown by an ERR-11 refusal on its user (REF-5 guarantees it nothing, and its receipt may
    expire before its refund reads the key); the same release made at P counts as lost or
    quarantined."""
    unconfirmed = released("d1", RETENTION, elapsed=(2 * MS, P_NS - 2 * MS + past_p))
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*unconfirmed, integrity_refusal(REFUSED_AT)]),
        closed_faults=[STORE_DOWN, fault],
    )
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], (case, lines)
    want = "after_p 1, lost 0, quarantined 0" if past_p else f"after_p 0, {excused}"
    assert lines[-1].endswith(f", {want}"), (case, lines)


def instance_setting(old: str, new: str) -> Callable[[str], str]:
    """A config.toml change replacing old with new (both in the [instance] table)."""

    def change(text: str) -> str:
        assert old in text
        return text.replace(old, new)

    return change


# The [instance] settings DESIGN §A7's wait beyond the outage takes, RefundBackoffMax +
# ReconnectBackoffMax (1 s) + (⌈max_pending_refunds / refund_workers⌉ + 3) × D (26 s with the
# closed run's settings), each changed so that wait would move: (case, the change). A deferred
# loss's event no longer needs that margin before d1's bound, as the run logs show it (judgment
# RP3), so d1's bound stays LOSS_BOUND under each.
WORKERS = "refund_workers = 128\n"
INSTANCE_SETTINGS: list[tuple[str, Callable[[str], str]]] = [
    (
        "refund_workers 127 (the wait 28 s)",
        instance_setting(WORKERS, "refund_workers = 127\n"),
    ),
    (
        "max_pending_refunds 1153 (the wait 28 s)",
        instance_setting("max_pending_refunds = 1152\n", "max_pending_refunds = 1153\n"),
    ),
    (
        "dependency_deadline_ns 1 s (the wait 14 s)",
        instance_setting(WORKERS, WORKERS + "dependency_deadline_ns = 1_000_000_000\n"),
    ),
    (
        "refund_backoff_max_ns 3 s (the wait 28 s)",
        instance_setting(WORKERS, WORKERS + "refund_backoff_max_ns = 3_000_000_000\n"),
    ),
    (
        "no refund_workers (DefaultInstance's 128: the wait 26 s)",
        instance_setting(WORKERS, ""),
    ),
    (
        "no refund_workers, max_pending_refunds 1153 (the wait 28 s)",
        instance_setting("max_pending_refunds = 1152\n" + WORKERS, "max_pending_refunds = 1153\n"),
    ),
]


@pytest.mark.parametrize("late", [0, 1])
@pytest.mark.parametrize(
    ("case", "change"), INSTANCE_SETTINGS, ids=[c for c, _ in INSTANCE_SETTINGS]
)
def test_evidence_bound_ignores_instance_settings(
    tmp_path: Path, case: str, change: Callable[[str], str], late: int
) -> None:
    """A deferred loss's event must show by the refund's REF-5 deadline bound itself, whatever
    DESIGN §A7's wait the run's config.toml [instance] settings would give (judgment RP3 removed
    that margin): a tamper of d1's key ending at WITHIN, its ERR-11 refusal done at LOSS_BOUND,
    excuses d1 under each setting; the refusal done 1 ns later does not."""
    refusal = integrity_refusal(REFUSED_AT_BOUND + late)
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", RETENTION), refusal]),
        closed_faults=[STORE_DOWN, DEL_K_WITHIN],
        closed_config=change,
    )
    code, lines = check(world.manifest())
    assert code == 1, case
    if not late:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        assert lines[-1].endswith(", after_p 0, lost 0, quarantined 1"), (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "refund_completion")], (case, lines)


@pytest.mark.parametrize(
    ("case", "old", "new"),
    [
        ("refund_workers 0", WORKERS, "refund_workers = 0\n"),
        ("a negative refund_workers", WORKERS, "refund_workers = -1\n"),
        ("no max_pending_refunds", "max_pending_refunds = 1152\n", ""),
        ("max_pending_refunds 0", "max_pending_refunds = 1152\n", "max_pending_refunds = 0\n"),
        (
            "a dependency_deadline_ns that is no integer",
            WORKERS,
            WORKERS + 'dependency_deadline_ns = "2s"\n',
        ),
        ("dependency_deadline_ns 0", WORKERS, WORKERS + "dependency_deadline_ns = 0\n"),
        ("a negative refund_backoff_max_ns", WORKERS, WORKERS + "refund_backoff_max_ns = -1\n"),
        ("refund_backoff_max_ns 0", WORKERS, WORKERS + "refund_backoff_max_ns = 0\n"),
    ],
)
def test_instance_settings_not_read(tmp_path: Path, case: str, old: str, new: str) -> None:
    """completion reads none of the [instance] settings DESIGN §A7's wait took (judgment RP3
    removed that margin, and with it the input failures these settings gave when missing or out
    of range): so changed in the closed run's config.toml after its report was built, each fails
    nothing."""
    world = make_world(tmp_path)
    config = world.run("closed").joinpath("config.toml")
    config.write_text(instance_setting(old, new)(config.read_text(encoding="utf-8")))
    holds(world.manifest())


# context.json's fault commands, checked as completion reads them (F1 item B4; AGENTS.md §1 Input):
# a cmd is faultctl's command line ("faultctl " then its words, split as strings.Fields splits
# them) or one JSON object (harness/config validCmd), and completion checks the faultctl commands
# against ParseFaultCommand's grammar (harness/fixtures faultcmd.go, DESIGN §B6) as far as it reads
# them. (case, the cmd of the closed run's second fault, the input failure's reason after the cmd.)
NOT_A_CMD = "want a command starting with 'faultctl ' or one JSON object (a /_test/faults body)"
API_KILL_SHAPE = "want api kill <N> [--restart]"
ONE_INSTANCE = "want 1 to 1 (the run's instances)"
FAULT_CMD_ERRORS: list[tuple[str, str, str]] = [
    ("a cmd neither faultctl's nor a JSON object", "xfaultctl api kill 1 --restart", NOT_A_CMD),
    ("faultctl and a tab", "faultctl\tapi kill 1 --restart", NOT_A_CMD),
    ("faultctl alone", "faultctl", NOT_A_CMD),
    (
        "a JSON body holding NaN (Go's decoder refuses it)",
        '{"target":"store","mode":"refuse","duration_ms":NaN}',
        NOT_A_CMD,
    ),
    ("a JSON array", "[]", NOT_A_CMD),
    # F1 item B14: JSON null (Go's validCmd refuses obj == nil), and a body nested deeper than
    # Python's decoder recurses (Go's refuses it too: unclosed, and past its depth of 10,000).
    ("a JSON null", "null", NOT_A_CMD),
    ("a JSON array nested 100,000 deep", "[" * 100_000, NOT_A_CMD),
    # F1 item B19: JSON that is one value but no object, which Go's decoder refuses into validCmd's
    # map: a number, a string, a bool and a float.
    ("a JSON number", "5", NOT_A_CMD),
    ("a JSON string", '"x"', NOT_A_CMD),
    ("a JSON bool", "true", NOT_A_CMD),
    ("a JSON float", "1.5", NOT_A_CMD),
    ("faultctl and white space only", "faultctl  ", "no command: want store, api, tamper or clock"),
    (
        "an unknown command",
        "faultctl restart api 1",
        "unknown command 'restart': want store, api, tamper or clock",
    ),
    # F1 item B19: code points that are no white space to Go's unicode.IsSpace, so strings.Fields
    # keeps each inside its word and api<X>kill is no command: U+001C to U+001E, which Python's
    # str.split takes for white space (as it takes U+001F, REF9D's row), U+200B and U+180E (white
    # space in older Unicode versions), U+FEFF and U+0000.
    *(
        (
            f"U+{c:04X} between api and kill",
            f"faultctl api{chr(c)}kill 1",
            f"unknown command {'api' + chr(c) + 'kill'!r}: want store, api, tamper or clock",
        )
        for c in (0x001C, 0x001D, 0x001E, 0x200B, 0x180E, 0xFEFF, 0x0000)
    ),
    # api kill <N> [--restart] (parseAPI; N at most the run's instances, the orchestrator's
    # preflight).
    ("api kill without N", "faultctl api kill", API_KILL_SHAPE),
    ("api kill with a fourth word", "faultctl api kill 1 --restart now", API_KILL_SHAPE),
    ("api stop", "faultctl api stop 1", API_KILL_SHAPE),
    ("api kill N restart", "faultctl api kill 1 restart", API_KILL_SHAPE),
    # F1 item B13: parseAPI takes only the word --restart after N.
    ("api kill N -restart", "faultctl api kill 1 -restart", API_KILL_SHAPE),
    ("api kill N --restarts", "faultctl api kill 1 --restarts", API_KILL_SHAPE),
    # F1 item B19: parseAPI compares the third word with --restart itself (no flag package), so it
    # refuses the flag forms --restart=true and --restart=x as any other word.
    ("api kill N --restart=true", "faultctl api kill 1 --restart=true", API_KILL_SHAPE),
    ("api kill N --restart=x", "faultctl api kill 1 --restart=x", API_KILL_SHAPE),
    # F1 item B21: likewise --restart= with an empty value.
    ("api kill N --restart=", "faultctl api kill 1 --restart=", API_KILL_SHAPE),
    ("api kill of no number", "faultctl api kill one", f"instance 'one': {ONE_INSTANCE}"),
    ("api kill 0", "faultctl api kill 0 --restart", f"instance '0': {ONE_INSTANCE}"),
    (
        "api kill of an instance the run lacks",
        "faultctl api kill 2 --restart",
        f"instance '2': {ONE_INSTANCE}",
    ),
    # F1 item B21: strconv.Atoi reads a trailing zero as a digit: 10 is past the run's instances.
    ("api kill 10", "faultctl api kill 10", f"instance '10': {ONE_INSTANCE}"),
    # F1 item B13: strconv.Atoi reads -1 as below 1, and refuses two signs.
    ("api kill -1", "faultctl api kill -1", f"instance '-1': {ONE_INSTANCE}"),
    ("api kill ++1", "faultctl api kill ++1", f"instance '++1': {ONE_INSTANCE}"),
    # F1 item B19: strconv.Atoi takes ASCII digits only, while Python's int() takes any Unicode
    # decimal digit, so 0 then U+0661 (ARABIC-INDIC DIGIT ONE) is no number, though int() reads 1.
    (
        "api kill of 0 then an Arabic-Indic digit one",
        f"faultctl api kill 0{chr(0x0661)}",
        f"instance {'0' + chr(0x0661)!r}: {ONE_INSTANCE}",
    ),
    # tamper <op> [flags] (parseTamper: Go's flag package, then TamperCommand.check).
    ("tamper with no operation", "faultctl tamper", "tamper needs an operation"),
    (
        "an unknown tamper operation",
        "faultctl tamper del-all",
        "unknown tamper operation 'del-all'",
    ),
    ("a positional word", "faultctl tamper del-k u000003", "unexpected argument 'u000003'"),
    ("a word after --", "faultctl tamper del-k --key u000003 -- x", "unexpected argument 'x'"),
    ("a lone -", "faultctl tamper del-k --key u000003 -", "unexpected argument '-'"),
    ("three dashes", "faultctl tamper del-k ---key u000003", "bad flag syntax: ---key"),
    # F1 item B17: a two-character word starting with - is a flag to Go's flag package.
    (
        "a two-character flag word",
        "faultctl tamper del-k -k u000003",
        "flag provided but not defined: -k",
    ),
    (
        "a flag name starting with =",
        "faultctl tamper del-k -=u000003",
        "bad flag syntax: -=u000003",
    ),
    (
        "an unknown flag",
        "faultctl tamper del-k --keys u000003",
        "flag provided but not defined: -keys",
    ),
    ("a flag with no value", "faultctl tamper del-k --key", "flag needs an argument: -key"),
    ("del-k without --key", "faultctl tamper del-k", "tamper del-k needs --key"),
    (
        "corrupt-field without --field",
        "faultctl tamper corrupt-field --key u000003 --value 5",
        "tamper corrupt-field needs --field",
    ),
    (
        "corrupt-field without --value",
        "faultctl tamper corrupt-field --key u000003 --field u",
        "tamper corrupt-field needs --value",
    ),
    (
        "retype-shared without --target",
        "faultctl tamper retype-shared",
        "tamper retype-shared needs --target",
    ),
    (
        "add-bad-ix with --target",
        "faultctl tamper add-bad-ix --target ix",
        "tamper add-bad-ix takes no --target",
    ),
    # Was REF9D's "add-bad-ix naming its key (faultctl takes no --key there)", which excused
    # nothing: faultctl refuses the command, so it is malformed input.
    (
        "add-bad-ix with --key",
        "faultctl tamper add-bad-ix --key u000003",
        "tamper add-bad-ix takes no --key",
    ),
    (
        "del-hwm with --key",
        "faultctl tamper del-hwm --key u000003",
        "tamper del-hwm takes no --key",
    ),
    (
        "a tamper with --epoch",
        f"faultctl tamper del-hwm --epoch {WELL_FORMED_ID}",
        "--epoch: the orchestrator refuses it, as a run tampers in its own fresh epoch",
    ),
    (
        "retype-shared of another target",
        "faultctl tamper retype-shared --target k",
        "--target 'k': want ix, q or epoch",
    ),
    (
        "corrupt-record --value without --field",
        "faultctl tamper corrupt-record --value 5",
        "--value needs --field",
    ),
    (
        "corrupt-record with an empty --field",
        "faultctl tamper corrupt-record --field=",
        "--field must not be empty",
    ),
    (
        "corrupt-field with an empty --field",
        "faultctl tamper corrupt-field --key u000003 --field= --value 5",
        "--field must not be empty",
    ),
    (
        "del-bucket-fields of a field no bucket holds",
        "faultctl tamper del-bucket-fields --key u000003 --field m",
        "--field 'm': want one of u, t, g, s, x, mc",
    ),
    (
        "del-marker with a malformed --member",
        "faultctl tamper del-marker --key u000003 --member 1",
        "--member '1': want g:s, two decimal numbers",
    ),
    (
        "del-mi-member with a signed --member",
        "faultctl tamper del-mi-member --key u000003 --member 1:+2",
        "--member '1:+2': want g:s, two decimal numbers",
    ),
    (
        "del-marker with a --member beyond int64",
        f"faultctl tamper del-marker --key u000003 --member {2**63}:1",
        f"--member '{2**63}:1': want g:s, two decimal numbers",
    ),
    # More digits than Python's int() converts (4,300): refused as input, not a traceback.
    (
        "api kill of a 5,000-digit N",
        f"faultctl api kill {'9' * 5000}",
        f"instance '{'9' * 5000}': {ONE_INSTANCE}",
    ),
]


def cmd_refused(tmp_path: Path, case: str, cmd: str, reason: str) -> None:
    """The closed run with cmd as its second fault fails as input, naming that fault, the field
    cmd and reason, and nothing else fails."""
    world = make_world(tmp_path, closed_faults=[STORE_DOWN, fault_record(cmd, WITHIN)])
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    detail = f"context.json: faults[1]: field cmd: {cmd!r}: {reason}"
    assert rest(lines)[0] == f"FAIL {CLOSED} {IDS['closed']} input: {detail}", (case, lines)


@pytest.mark.parametrize(
    ("case", "cmd", "reason"), FAULT_CMD_ERRORS, ids=[c for c, _, _ in FAULT_CMD_ERRORS]
)
def test_malformed_fault_command(tmp_path: Path, case: str, cmd: str, reason: str) -> None:
    """A context.json fault whose cmd is neither faultctl's command line nor one JSON object, or a
    faultctl command ParseFaultCommand or the orchestrator's preflight refuses where completion
    reads it (its command word; api kill's words and N; a tamper's operation, flag syntax, flag
    names, the flags it needs or takes, --epoch, and the values completion reads) fails the run as
    input, naming the fault and the field cmd: completion never reads a malformed command as
    something else, or passes over it."""
    cmd_refused(tmp_path, case, cmd, reason)


# F1 item B11: every per-operation flag rule of faultctl's tamper parser. GO_TAMPER_FLAGS copies
# harness/fixtures tamperFlags literally: per operation, the flags it requires, then those it may
# take (--epoch, open to every operation there, aside: the preflight refuses it). Among the flags
# the parser defines (fs.StringVar), each one an operation requires is left out in turn ("tamper
# <op> needs --<f>"), and each one it does not take is added to the flags it requires ("tamper <op>
# takes no --<f>"). Every other flag carries FLAG_VALUES' value, one faultctl takes, so only the
# rule fails.
GO_TAMPER_FLAGS: dict[str, tuple[tuple[str, ...], tuple[str, ...]]] = {
    "del-bucket-fields": (("key",), ("field",)),
    "del-k": (("key",), ()),
    "del-mi": (("key",), ()),
    "del-ix": (("key",), ()),
    "del-mi-member": (("key", "member"), ()),
    "del-marker": (("key", "member"), ()),
    "corrupt-field": (("key", "field", "value"), ()),
    "add-bad-ix": ((), ("member", "value")),
    "retype-shared": (("target",), ()),
    "corrupt-record": ((), ("field", "value")),
    "del-hwm": ((), ()),
    "wipe": ((), ()),
}
FLAG_VALUES = {"key": "u000003", "member": "1:1", "field": "u", "value": "5", "target": "ix"}


def tamper_cmd(op: str, flags: Sequence[str]) -> str:
    """faultctl tamper op with each of flags, carrying its FLAG_VALUES value."""
    return " ".join(["faultctl tamper", op, *(f"--{name} {FLAG_VALUES[name]}" for name in flags)])


TAMPER_FLAG_RULES: list[tuple[str, str, str]] = [
    *(
        (
            f"{op} without --{name}",
            tamper_cmd(op, [other for other in required if other != name]),
            f"tamper {op} needs --{name}",
        )
        for op, (required, _) in GO_TAMPER_FLAGS.items()
        for name in required
    ),
    *(
        (f"{op} with --{name}", tamper_cmd(op, [*required, name]), f"tamper {op} takes no --{name}")
        for op, (required, optional) in GO_TAMPER_FLAGS.items()
        for name in FLAG_VALUES
        if name not in required and name not in optional
    ),
]


@pytest.mark.parametrize(
    ("case", "cmd", "reason"), TAMPER_FLAG_RULES, ids=[c for c, _, _ in TAMPER_FLAG_RULES]
)
def test_tamper_flags_per_operation(tmp_path: Path, case: str, cmd: str, reason: str) -> None:
    """A tamper command without a flag its operation requires, or with a flag the parser defines
    that its operation does not take (harness/fixtures tamperFlags, each rule; F1 item B11), fails
    the run as input, naming the fault, the field cmd and the rule: completion never reads it as
    the tamper it resembles (del-k --field u as del-k of its key, say)."""
    cmd_refused(tmp_path, case, cmd, reason)


# The code points Go's unicode.IsSpace takes, at which strings.Fields splits a faultctl cmd's words
# (F1 item B12): Unicode's White_Space, U+0009 to U+000D, U+0020, U+0085, U+00A0, U+1680, U+2000 to
# U+200A, U+2028, U+2029, U+202F, U+205F and U+3000, from Go's unicode/tables.go _White_Space
# copied literally (each Range16 as its lo, hi and stride).
GO_WHITE_SPACE = [
    *range(0x0009, 0x000D + 1, 1),
    *range(0x0020, 0x0085 + 1, 101),
    *range(0x00A0, 0x1680 + 1, 5600),
    *range(0x2000, 0x200A + 1, 1),
    *range(0x2028, 0x2029 + 1, 1),
    *range(0x202F, 0x205F + 1, 48),
    *range(0x3000, 0x3000 + 1, 1),
]


@pytest.mark.parametrize(
    "cmd",
    [
        "faultctl api kill 1",
        "faultctl api kill +1 --restart",
        "faultctl api kill 01 --restart",
        "faultctl api kill 0000000000000000000001 --restart",
        "faultctl  api\tkill  1   --restart",
        "faultctl api\u3000kill 1 --restart",
        *(
            pytest.param(
                f"faultctl api{chr(c)}kill{chr(c)}1{chr(c)}--restart", id=f"U+{c:04X} between words"
            )
            for c in GO_WHITE_SPACE
        ),
    ],
)
def test_kill_command_forms(tmp_path: Path, cmd: str) -> None:
    """api kill <N> [--restart] as ParseFaultCommand takes it, N as strconv.Atoi reads it (a sign,
    or leading zeros past 19 digits) and the words split at any Unicode white space after
    "faultctl " (strings.Fields: each code point of Go's unicode.IsSpace, F1 item B12): the
    pre_kill line takes the kill, and d1, with no final event, is excused."""
    world = kill_world(tmp_path, killed_log(released("d1", None)), {**kill_fault(), "cmd": cmd})
    code, lines = check(world.manifest())
    assert code == 1 and failed(lines) == [] and skips(lines) == [], (cmd, lines)
    assert ", excluded killed 1, " in lines[-1], (cmd, lines)


def test_fault_command_completion_does_not_read(tmp_path: Path) -> None:
    """A faultctl clock command, whose arguments completion does not read (nor a store command's:
    the closed run's store down), passes unchecked."""
    clock = fault_record("faultctl clock advance 1000000", WITHIN)
    world = make_world(tmp_path, closed_faults=[STORE_DOWN, clock])
    holds(world.manifest())


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


# d-ok's request line (s6_lines: its request arriving 1 s after the others, at T0 + 7 s), again.
D_OK_REQUEST = released("d-ok", None, arrival_ns=T0_WALL_NS + 7 * SECOND)[-1]
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
    # Judgment RP1: a request line joined to a release is read once, and its user_id and
    # decision_ns are its decision line's (hitapi writes both from one acquire): a decision_ns
    # 1 above its decision line's and 1 below it both fail (judgment RB2).
    (
        "a second request line",
        [D_OK_REQUEST],
        "field deduction_id: d-ok: a second request line",
    ),
    (
        "a request line whose user_id is not its decision line's",
        [*released("d-new", None)[:2], {**released("d-new", None)[2], "user_id": "u000009"}],
        "field user_id: 'u000009': its decision line has user_id 'u000003'",
    ),
    (
        "a request line whose decision_ns is not its decision line's",
        [*released("d-new", None)[:2], {**released("d-new", None)[2], "decision_ns": DECIDED + 1}],
        f"field decision_ns: {DECIDED + 1}: its decision line has decision_ns {DECIDED}",
    ),
    (
        "a request line whose decision_ns is 1 below its decision line's",
        [*released("d-new", None)[:2], {**released("d-new", None)[2], "decision_ns": DECIDED - 1}],
        f"field decision_ns: {DECIDED - 1}: its decision line has decision_ns {DECIDED}",
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
        "a request whose arrival_ns is no integer",
        {**A_REQUEST, "arrival_ns": 1.5},
        "arrival_ns",
        "want an integer",
    ),
    (
        "a request with a negative arrival_ns",
        {**A_REQUEST, "arrival_ns": -1},
        "arrival_ns",
        "-1: want at least 0",
    ),
    (
        "a request whose limiter_ns is no integer",
        {**A_REQUEST, "limiter_ns": 1.0},
        "limiter_ns",
        "want an integer",
    ),
    (
        "a request with a negative total_ns",
        {**A_REQUEST, "total_ns": -1},
        "total_ns",
        "-1: want at least 0",
    ),
    ("a request whose error is no string", {**A_REQUEST, "error": 7}, "error", "want a string"),
    ("a request whose result is null", {**A_REQUEST, "result": None}, "result", "want a string"),
    (
        "a request with an HTTP status below 100",
        {**A_REQUEST, "http_status": 99},
        "http_status",
        "99: want at least 100",
    ),
    # Judgment RP1 and RP3: completion reads a request's user_id (its ERR-11 evidence, and the
    # join's check) and decision_ns (the join's check) too.
    (
        "a request whose user_id is no string",
        {**A_REQUEST, "user_id": 5},
        "user_id",
        "want a string",
    ),
    (
        "a request whose decision_ns is no integer",
        {**A_REQUEST, "decision_ns": 1.5},
        "decision_ns",
        "want an integer",
    ),
    (
        "a request with a negative decision_ns",
        {**A_REQUEST, "decision_ns": -1},
        "decision_ns",
        "-1: want at least 0",
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


# Judgment RP4: hitapi measures limiter_ns and db_ns one after the other inside total_ns, which runs
# from the arrival until the response was written (harness/httpapi hit.go), so a request line's
# total_ns is at least limiter_ns + db_ns, on every request line: A_REQUEST (200, no permit,
# limiter_ns 500 µs, db_ns 1.5 ms) and an ERR-11 refusal (limiter_ns 1.9 ms, db_ns 0), each with
# total_ns at that sum and 1 ns below it, appended to the closed run's api-1.jsonl after its report
# was built: (case, the line, its input failure's reason, or None when it is taken).
TOTAL_NS_ROWS: list[tuple[str, Line, str | None]] = [
    (
        "a request whose total_ns is its limiter_ns + db_ns",
        {**A_REQUEST, "total_ns": 2_000_000},
        None,
    ),
    (
        "a request whose total_ns is 1 ns below its limiter_ns + db_ns",
        {**A_REQUEST, "total_ns": 1_999_999},
        "1999999: below its limiter_ns 500000 + db_ns 1500000 = 2000000, which hitapi measures "
        "inside it",
    ),
    (
        "an ERR-11 refusal whose total_ns is its limiter_ns + db_ns",
        {**integrity_refusal(REFUSED_AT, user=OTHER_USER), "total_ns": REFUSED_LIMITER_NS},
        None,
    ),
    (
        "an ERR-11 refusal whose total_ns is 1 ns below its limiter_ns + db_ns",
        {**integrity_refusal(REFUSED_AT, user=OTHER_USER), "total_ns": REFUSED_LIMITER_NS - 1},
        "1899999: below its limiter_ns 1900000 + db_ns 0 = 1900000, which hitapi measures inside "
        "it",
    ),
]


@pytest.mark.parametrize(
    ("case", "line", "reason"), TOTAL_NS_ROWS, ids=[c for c, _, _ in TOTAL_NS_ROWS]
)
def test_request_total_ns_covers_its_parts(
    tmp_path: Path, case: str, line: Line, reason: str | None
) -> None:
    """A request line whose total_ns is below its limiter_ns + db_ns, which hitapi measures inside
    it, fails the run as input, naming the file, the line and the field total_ns, whatever the
    line answered (judgment RP4); one whose total_ns is that sum is taken."""
    world = make_world(tmp_path)
    log = world.run("closed").joinpath("api-1.jsonl")
    count = len(log.read_text(encoding="utf-8").splitlines())
    rewrite_log(world.run("closed"), lambda lines: [*lines, line])
    code, lines = check(world.manifest())
    assert code == 1, case
    if reason is None:
        assert failed(lines) == [] and skips(lines) == [], (case, lines)
        return
    assert failed(lines) == [(CLOSED, IDS["closed"], "input")], (case, lines)
    where = f"input: api-1.jsonl: line {count + 1}: field total_ns: {reason}"
    assert rest(lines)[0].endswith(where), (case, lines)


# Verifier astra's shape (judgment RP4): d1's key damaged (DEL_K_WITHIN), and its only ERR-11
# refusal arriving 1 ms before LOSS_BOUND with limiter_ns 1.9 ms. With total_ns 0 its acquire would
# seem done by LOSS_BOUND - 1 ms and excuse d1; hitapi cannot write that total_ns, so the line is an
# input error. With total_ns 1.9 ms, the least it can be, the acquire was done by 976 µs past
# LOSS_BOUND (1.9 ms + 76,000 ns of slew - 1 ms), and d1 fails.
ASTRA_ARRIVAL = LOSS_BOUND - MS


@pytest.mark.parametrize(
    ("total_ns", "check_name", "detail"),
    [
        (
            0,
            "input",
            "field total_ns: 0: below its limiter_ns 1900000 + db_ns 0 = 1900000, which hitapi "
            "measures inside it",
        ),
        (
            REFUSED_LIMITER_NS,
            "refund_completion",
            "deduction d1: released 2000000 ns after just before its acquire (limiter_ns + db_ns, "
            "within P 5000000000): final event unresolved (retention): not confirmed",
        ),
    ],
    ids=["total_ns 0", "total_ns its limiter_ns"],
)
def test_refusal_total_ns_below_its_limiter_ns(
    tmp_path: Path, total_ns: int, check_name: str, detail: str
) -> None:
    """An ERR-11 refusal 1 ms before d1's deadline bound with limiter_ns 1.9 ms and total_ns 0 is
    an input error, not evidence that excuses d1 (judgment RP4); with total_ns 1.9 ms its acquire
    was done past the bound, so d1, unresolved on retention, fails."""
    refusal = {**integrity_refusal(ASTRA_ARRIVAL), "total_ns": total_ns}
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", [*released("d1", RETENTION), refusal]),
        closed_faults=[STORE_DOWN, DEL_K_WITHIN],
    )
    code, lines = check(world.manifest())
    assert code == 1, total_ns
    assert failed(lines) == [(CLOSED, IDS["closed"], check_name)], (total_ns, lines)
    assert rest(lines)[0].endswith(detail), (total_ns, lines)


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
    # A request's decision_ns is its decision line's (judgment RP1): each edit keeps them equal.
    ("a decision's decision_ns", 2, "decision_ns", {5: {"decision_ns": INT64_MAX}}),
    ("a release's release_mono_ns", 3, "release_mono_ns", {}),
    ("a final event's attempts", 4, "attempts", {}),
    (
        "a final event's credit_decision_ns",
        4,
        "credit_decision_ns",
        {2: {"decision_ns": INT64_MAX}, 5: {"decision_ns": INT64_MAX}},
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
    ("a request's decision_ns", 5, "decision_ns", {2: {"decision_ns": INT64_MAX}}),
    ("a request's arrival_ns", 5, "arrival_ns", {}),
    # A request's total_ns covers its limiter_ns + db_ns (judgment RP4): each edit keeps it so.
    ("a request's limiter_ns", 5, "limiter_ns", {5: {"total_ns": INT64_MAX}}),
    ("a request's db_ns", 5, "db_ns", {5: {"limiter_ns": 0, "total_ns": INT64_MAX}}),
    ("a request's total_ns", 5, "total_ns", {}),
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
    read and confirmed in time (3 releases: the two S6 runs' d-ok and its d-x). context.json
    records a kill (kill_fault: after d-x's request), which a tail ending with a pre_kill line
    takes."""
    world = make_world(tmp_path, closed_faults=[STORE_DOWN, kill_fault()])
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
    (matched to context.json's kill) drained and closed, so it was not killed and the release
    fails."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", released("d1", final_event("d1"))),
        closed_faults=[STORE_DOWN, kill_fault()],
    )
    rewrite_log(world.run("closed"), unconfirmed_d1(tail))
    code, lines = check(world.manifest())
    assert code == 1, case
    assert failed(lines) == [(CLOSED, IDS["closed"], check_name)], (case, lines)
    assert detail in rest(lines)[0], (case, lines)


@pytest.mark.parametrize("restart", [True, False])
def test_pre_kill_ends_the_segment(tmp_path: Path, restart: bool) -> None:
    """The same unconfirmed release in a segment that ends with a pre_kill line (its stop line
    replaced by one) is excused: its process was killed right after it (kill_fault, with or
    without --restart: K_hi 1.038 s after its release, the file's last segment, so the kill's end
    bounds it)."""
    world = make_world(
        tmp_path,
        closed=s6_lines("closed", released("d1", final_event("d1"))),
        closed_faults=[STORE_DOWN, kill_fault(restart=restart)],
    )
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
