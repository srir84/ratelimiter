"""`python3 report/completion.py runs/h6-manifest.json` checks HARNESS §8 Completion over the H6
scenario runs, and `python3 report/completion.py --build-manifest runs/index.jsonl` prints the
manifest it reads (IMPLEMENTATION_PLAN H6 steps 3 and 4).

The manifest (plan choice for its JSON shape) is one object, its run directories next to it
(runs/<run ID>/):

    {"scenarios": {"<scenario>": {"baseline": "<run ID>" | null, "limiter": "<run ID>" | null}, …},
     "s3-busy-user-rerun": "<run ID>" | null}

Its scenarios are H6 scenario files (H6_SCENARIOS: the step 1 files plus s4-spike and s7-long),
each with its limiter run and its baseline run, either null when it has none. --build-manifest
makes it from runs/index.jsonl (H3: one {"scenario","baseline","run_id"} line per finished run,
in run order, read one line at a time): per H6 scenario its last baseline line and its last
limiter line, ignoring every other scenario's lines whether or not their runs still exist; for
s3-busy-user the earlier of its last two limiter runs is the limiter run and the later one, from
the rerun, is "s3-busy-user-rerun" (null without a second). The orchestrator indexes only a run
that finished without failure (H3), so a scenario whose runs failed has no entry, and one whose
limiter run failed after a valid baseline has "limiter": null.

The check adds no rule to plan step 4: its checks, in its order, on every run the manifest lists,
after the presence of every H6 scenario's runs that the plan's Pass when counts (orchestrator
decision R1 of the H6 task 1 repair).
0. listed: every H6 scenario is in the manifest with its limiter run (plan H6 Goal and Pass
   when: "The 9 benchmark limiter runs (2 S1, 3 S2, S3, S4, S5, S7) each have a valid baseline:
   18 benchmark reports, plus the s3-busy-user rerun; 8 S6 test-build reports"); a scenario with
   only a baseline fails here, and the manifest's other runs are still checked.
1. evidence_valid, validate: each run's report.json has evidence_validity.valid = true and passes
   report/validate.py, called in this process (validate.check; no subprocess, AGENTS.md §4) in
   baseline mode for a baseline run, as the orchestrator calls it, else in full mode.
2. baseline, baseline_schedule: each benchmark scenario in the manifest (every H6 scenario but
   the S6 ones: plan step 1 makes them bench, and S6 test runs need no baseline, step 2) has a
   baseline there, and its limiter run's context.json schedule_sha256 is the baseline's.
3. correctness: each limiter run, the rerun included, has correctness.verdict = "pass".
4. s6_failure_modes, s6_unknowns_apart: the S6 limiter runs (scenarios s6-*) include one in each
   failure mode (config.toml [policy] failure_mode, closed and open), with ERR-9 and ERR-16 counted
   apart from ERR-8/ERR-3: each S6 run's report.json (results.by_err and its results.prf2 names)
   counts each of the four rows as its run logs' request lines give it, by HARNESS §3's own
   status and error code (not the report builder's table), so no unknown outcome is folded into
   ERR-8 (closed) or ERR-3 (open); and the S6 runs of each mode have at least one ERR-9 and one
   ERR-16 between them, so the separation shows in both modes.
5. refund_completion (the rules below): every release covered by REF-5/REF-9 was confirmed
   (applied/already_applied) before its expiry and within refund_deadline_ns + the run's measured
   outage of its release.
6. rerun_schedule: the s3-busy-user rerun, a limiter run of s3-busy-user with seed 1 other than
   its limiter run, has the limiter run's schedule_sha256.

Refund completion, as plan step 4 words it: "list every release covered by REF-5/REF-9 — released
within max_release_delay (P) of just before its acquire, not on an instance killed before the
release's final event, and not cut by a drain timeout — and require each to be confirmed
(applied/already_applied) before its expiry and with final_mono_ns − release_mono_ns ≤
refund_deadline_ns (30 s) + the run's measured outage (context.json faults outage_ns)". The rules
it applies:
- SPEC REF-5: "When release() is called within max_release_delay (RCP-8), the refund is confirmed
  (applied or already_applied) within refund_deadline + outage time of the release() call … Always
  before expiry (guaranteed by RCP-8). A release() after max_release_delay is still attempted but
  has no deadline guarantee."
- SPEC REF-9: refunds are guaranteed only "(c) while the instance keeps running or drains for at
  least the default drain_timeout" (CFG-8: refund_deadline + outage_tolerance, 35 s; hitapi
  closes with it); SPEC API-1d: "For each released permit that has a receipt, the library emits
  one final event: applied, already_applied, receipt_invalid, receipt_expired, or unresolved
  (reason closed, retention, or expired)", unresolved (closed) at the end of a drain (DESIGN
  §A5.6: "Stops … at the end of the drain (unresolved, closed)").
- The expiry, SPEC RCP-1: "Issue time = the acquire's decision_ns; expiry time = issue time +
  receipt_lifetime (budget time)", and RCP-5: a refund is accepted "only while … budget time <
  signed expiry": a credit_decision_ns before decision_ns + receipt_lifetime_ns.
- The fields, DESIGN §B8: "api-N.jsonl decision | Acquire returned allowed or degraded (written
  before DB work) | deduction_id, user_id, decision_ns, result"; "request | completed /hit call |
  … deduction_id, result, … limiter_ns, db_ns, total_ns"; "release | permit.Release() called |
  deduction_id, release_mono_ns"; "refund | listener event | deduction_id, result, reason,
  attempts, credit_decision_ns, release_mono_ns, final_mono_ns"; "lifecycle | start, ready,
  pre-kill read, stop"; and context.json "faults with actual start/end" (outage_ns: the measured
  outage of a store fault, H4).
So, per process segment of an api-N.jsonl (from a lifecycle start line, or the file's first
line, to the next start line or the end; the run log of one hitapi process), each release line
with a deduction_id (a degraded permit's is "": it has no receipt and no final event, REF-2) joins
its process's allowed decision line before it (decision_ns), its request line (limiter_ns,
db_ns) and its refund line (the final event). hitapi measures limiter_ns from just before acquire
(its P starts there, HTTP-13) and db_ns over the database step, and calls release() at once after
the step that failed (the refusal after acquire, or the database outcome; DESIGN §B3), so a
release came limiter_ns + db_ns after just before its acquire, as T-HFLT-2 reads it: within P when
that is at most [instance] max_release_delay_ns. A segment with a pre_kill line, and no stop line
after it (a process that wrote its stop line drained and closed: it was not killed), ended in
SIGKILL (FLT-6, written just before it): a release with no final event there was on an instance
killed before its final event (a final event before the pre_kill line is judged as any other),
which REF-9 (c) ("while the instance keeps running") excuses only when the kill came no later than
refund_deadline_ns + the run's measured outage after the release (orchestrator decision D5, as D3
bounds the drain). The pre_kill line carries no time (DESIGN §B8: its drop counts, and H4's
read_ok), so the kill time the run log gives on that process's own monotonic clock is the latest
release_mono_ns the segment holds (release lines, a degraded permit's included: hitapi's clock, ns
since the process started, the clock of the release's own release_mono_ns; the process still ran
then, so it was killed no earlier); a release that process outlived by more than the bound was
covered while the instance kept running, a REF-5 miss. A final event unresolved with reason closed
was cut by a drain timeout, which REF-9 (c) excuses only when the drain ended before the refund's
deadline (orchestrator decision D3): when final_mono_ns − release_mono_ns ≤ refund_deadline_ns +
the run's measured outage, as for a release after Close began; a later one drained for longer
than its deadline allowed, so REF-5 still covered it.
Each release whose final event is applied or already_applied with credit_decision_ns <
decision_ns + receipt_lifetime_ns and final_mono_ns − release_mono_ns ≤ refund_deadline_ns + the
run's measured outage is confirmed in time; any other is a failure unless it was released after
P, on an instance killed within that bound before its final event, or cut by a drain timeout
within that bound. A release without its request line is a failure only when its refund was not
confirmed in time (P cannot be judged).
The run's measured outage is context.json's faults outage_ns (H4: faultctl's measure of a store
down or pause). REF-5 allows "at most outage_tolerance, in one contiguous segment", so a run whose
context.json shows more than one store outage segment (a faultctl store down or pause, a timed
store refusal: target store, mode refuse, duration_ms above 0, as report.build.is_outage_segment
counts them for the orchestrator's rule, DESIGN §A7 item 5; or any fault with an outage_ns), or one
longer than config.toml's [policy] outage_tolerance_ns (its outage_ns, or a refusal's
duration_ms), has no REF-5 bound to judge: an input error (orchestrator decision R3 of the H6
slice review), found after the run's failure mode was read (orchestrator decision V4 of the H6
round-1 repair), so the run still counts in check 4's failure modes.
hitapi writes a permit's decision line before the database step and its release line right after
the failed step, through one ordered queue, and the request line once the response is written, so
the join keeps only the decisions in flight and the releases: no run log is held in memory.

Every run-log record is checked before it counts, so a malformed line never switches an exclusion
on (orchestrator decision R1 of the H6 slice review; AGENTS.md §1, §4): its kind is one DESIGN §B8
gives api-N.jsonl (decision, request, release, refund, lifecycle; a lifecycle line's event start,
ready, stop or pre_kill), it has exactly that kind's or event's fields (the golden lines,
harness/runlog/testdata/golden/api.jsonl), and each field completion reads, the start line's
limits and a stop or pre_kill line's drop counts and read_ok included, has its type and range: a
decision's result is allowed or degraded; a final event's result is one of SPEC API-1d's, with a
reason (closed, retention or expired) exactly when it is unresolved.
Input errors (an unreadable or malformed manifest, report.json, context.json, config.toml or run
log, or a run that is not what the manifest says) are failures with a message naming the record
(a run-log line by its file and line number) and the field (check "manifest" for the manifest,
"input" for a run, which then takes no further checks). An S6 limiter run that failed on input
after its failure mode (config.toml [policy] failure_mode) was read, in its other refund settings,
its outage evidence or its run logs, still counts in check 4's failure modes, not in its counts;
where such a run, or one that failed before its failure mode was read, leaves a mode or its
ERR-9/ERR-16 undecided, check 4 does not judge it and prints a SKIP line naming the run and
whether it failed before or after its failure mode was read ("SKIP <scenario> <run ID> <check>:
<detail>"; never a failure: the input failure is). Each failure prints as "FAIL <scenario>
<run ID> <check>: <detail>" ("-" where none applies). Exit 0 when every check holds, 1 when any
fails or the index cannot be read (--build-manifest), 2 for a wrong command line.
"""

import argparse
import json
import re
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import TextIO

if __package__ in (None, ""):
    # Run as `python3 report/completion.py …` (the plan's command line), sys.path starts with
    # report/: the repository root goes first, so the report package and refmodel import as under
    # `python3 -m`.
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from report import InputError, validate
from report import runfiles as rf
from report.build import fault_body, is_outage_segment

# The H6 scenario files (plan H6 step 1, with s4-spike and s7-long from step 2), in plan order.
H6_SCENARIOS = (
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
RERUN_OF = "s3-busy-user"
RERUN = "s3-busy-user-rerun"  # the manifest's key for the rerun, and its scenario in a failure
RERUN_SEED = 1  # "the s3-busy-user rerun with seed 1" (plan H6 step 4)
S6_PREFIX = "s6-"
S6 = "s6"  # the scenario a failure about the S6 runs together names
FAILURE_MODES = ("closed", "open")
ROLES = ("baseline", "limiter")
MS = 1_000_000  # ns per ms: a timed refusal's duration_ms
POLICY = f"{rf.CONFIG}: [policy]"  # where an input failure names a config.toml [policy] field

# Refund final events (SPEC API-1d) that confirm a refund (REF-5), and the unresolved reason of a
# refund cut by the drain timeout (DESIGN §A5.6).
CONFIRMED = ("applied", "already_applied")
UNRESOLVED = "unresolved"
DRAIN_CUT = "closed"
# Every final event's result, and an unresolved one's reasons (SPEC API-1d: "applied,
# already_applied, receipt_invalid, receipt_expired, or unresolved (reason closed, retention, or
# expired)").
FINAL_RESULTS = (*CONFIRMED, "receipt_invalid", "receipt_expired", UNRESOLVED)
UNRESOLVED_REASONS = (DRAIN_CUT, "retention", "expired")
# A decision line's results (DESIGN §B8: "Acquire returned allowed or degraded").
DECISION_RESULTS = ("allowed", "degraded")

# api-N.jsonl's record kinds and each one's fields (DESIGN §B8; the golden lines,
# harness/runlog/testdata/golden/api.jsonl), and the lifecycle events with theirs (DESIGN §B8:
# "start, ready, pre-kill read, stop"; H4 adds read_ok to pre_kill).
LIFECYCLE = "lifecycle"
RECORD_KEYS = {
    "decision": frozenset({"kind", "deduction_id", "user_id", "decision_ns", "result"}),
    "request": frozenset(
        {
            "kind",
            "run_id",
            "epoch_id",
            "instance",
            "user_id",
            "request_id",
            "arrival_ns",
            "decision_ns",
            "deduction_id",
            "result",
            "existed",
            "http_status",
            "error",
            "limiter_ns",
            "db_ns",
            "total_ns",
        }
    ),
    "release": frozenset({"kind", "deduction_id", "release_mono_ns"}),
    "refund": frozenset(
        {
            "kind",
            "deduction_id",
            "result",
            "reason",
            "attempts",
            "credit_decision_ns",
            "release_mono_ns",
            "final_mono_ns",
        }
    ),
}
LIFECYCLE_KEYS = {
    "start": frozenset(
        {
            "kind",
            "event",
            "max_in_flight",
            "queue_len",
            "max_release_delay_ns",
            "dependency_deadline_ns",
            "drain_timeout_ns",
            "nofile_limit",
        }
    ),
    "ready": frozenset({"kind", "event"}),
    "stop": frozenset({"kind", "event", "listener_drops", "log_drops"}),
    "pre_kill": frozenset({"kind", "event", "listener_drops", "log_drops", "read_ok"}),
}
RECORD_KINDS = ", ".join((*RECORD_KEYS, LIFECYCLE))
LIFECYCLE_EVENTS = ", ".join(LIFECYCLE_KEYS)

# HARNESS §3's rows the S6 check counts apart, by their own status and error code: "ERR-3 |
# degraded, database work succeeded | 200 | — (degraded=true)", "ERR-8 | unavailable, closed mode
# | 503 | limiter_unavailable", "ERR-9 | outcome_unknown … | 503 | limiter_outcome_unknown",
# "ERR-16 | Application outcome unknown … | 503 | application_outcome_unknown".
APART_ROWS = ("ERR-3", "ERR-8", "ERR-9", "ERR-16")
APART_503 = {
    "limiter_unavailable": "ERR-8",
    "limiter_outcome_unknown": "ERR-9",
    "application_outcome_unknown": "ERR-16",
}
# Each row's results.prf2 name in report.json (PRF-2).
PRF2_NAMES = {
    "ERR-3": "degraded_admissions",
    "ERR-8": "unavailable",
    "ERR-9": "unknown_acquires",
    "ERR-16": "unknown_application_outcomes",
}

# The checks' names, as failures print them.
CHECK_MANIFEST = "manifest"
CHECK_INPUT = "input"
CHECK_LISTED = "listed"
CHECK_VALID = "evidence_valid"
CHECK_VALIDATE = "validate"
CHECK_BASELINE = "baseline"
CHECK_SCHEDULE = "baseline_schedule"
CHECK_CORRECTNESS = "correctness"
CHECK_MODES = "s6_failure_modes"
CHECK_APART = "s6_unknowns_apart"
CHECK_REFUNDS = "refund_completion"
CHECK_RERUN = "rerun_schedule"

MANIFEST_KEYS = frozenset({"scenarios", RERUN})
ENTRY_KEYS = frozenset(ROLES)


@dataclass(frozen=True)
class Finding:
    """A failed check, or one not judged (a skip)."""

    scenario: str
    run_id: str
    check: str
    detail: str

    def line(self, word: str) -> str:
        return f"{word} {self.scenario} {self.run_id} {self.check}: {self.detail}"


def is_bench(scenario: str) -> bool:
    """A benchmark scenario: every H6 scenario but the S6 ones (plan H6 step 1 makes them bench),
    whose test runs need no baseline (step 2, DESIGN §B5)."""
    return not scenario.startswith(S6_PREFIX)


# The manifest.


@dataclass(frozen=True)
class Entry:
    baseline: str | None
    limiter: str | None


@dataclass(frozen=True)
class Manifest:
    scenarios: dict[str, Entry]
    rerun: str | None

    def to_json(self) -> dict[str, object]:
        return {
            "scenarios": {
                name: {"baseline": e.baseline, "limiter": e.limiter}
                for name, e in self.scenarios.items()
            },
            RERUN: self.rerun,
        }


def run_id_of(scenario: str, value: str, where: str, field_name: str) -> str:
    """value when it is a run ID of scenario (the orchestrator's YYYYMMDD-HHMMSS-<scenario>-<seed>,
    one path component), else InputError."""
    if not re.fullmatch(rf"[0-9]{{8}}-[0-9]{{6}}-{re.escape(scenario)}-[0-9]{{1,19}}", value):
        raise InputError(where, field_name, f"{value!r}: not a run ID of {scenario}")
    return value


def build_manifest(index: Path) -> Manifest:
    """The manifest of plan step 3 from runs/index.jsonl, read one line at a time: per H6
    scenario its last baseline run and its last limiter run (s3-busy-user: the earlier of its
    last two, the later one being the rerun); other scenarios' lines are passed over unread."""
    baselines: dict[str, str] = {}
    limiters: dict[str, list[str]] = {}
    for number, line in enumerate(rf.read_index(index), 1):
        if line.scenario not in H6_SCENARIOS:
            continue
        where = f"{index.name}: line {number}"
        run_id = run_id_of(line.scenario, line.run_id, where, "run_id")
        if line.baseline:
            baselines[line.scenario] = run_id
        else:
            last = limiters.setdefault(line.scenario, [])
            last.append(run_id)
            del last[:-2]  # only the last two count
    scenarios: dict[str, Entry] = {}
    rerun: str | None = None
    for name in H6_SCENARIOS:
        last = limiters.get(name, [])
        if name not in baselines and not last:
            continue
        limiter = last[-1] if last else None
        if name == RERUN_OF and len(last) == 2:
            limiter, rerun = last[0], last[1]
        scenarios[name] = Entry(baseline=baselines.get(name), limiter=limiter)
    return Manifest(scenarios=scenarios, rerun=rerun)


def read_manifest(path: Path) -> Manifest:
    """runs/h6-manifest.json, every key and run ID checked (InputError naming the field)."""
    raw = rf.read_json(path)
    where = path.name
    rf.exact_keys(raw, MANIFEST_KEYS, where)
    entries = rf.get_obj(raw, "scenarios", where)
    scenarios: dict[str, Entry] = {}
    for name in entries:
        if name not in H6_SCENARIOS:
            raise InputError(f"{where}: scenarios", name, "not an H6 scenario")
        entry = rf.as_obj(entries[name], f"{where}: scenarios", name)
        place = f"{where}: scenarios.{name}"
        rf.exact_keys(entry, ENTRY_KEYS, place)
        ids: dict[str, str | None] = {}
        for role in ROLES:
            value = rf.get_opt_str(entry, role, place)
            ids[role] = None if value is None else run_id_of(name, value, place, role)
        scenarios[name] = Entry(baseline=ids["baseline"], limiter=ids["limiter"])
    ordered = {name: scenarios[name] for name in H6_SCENARIOS if name in scenarios}
    rerun = rf.get_opt_str(raw, RERUN, where)
    if rerun is not None:
        run_id_of(RERUN_OF, rerun, where, RERUN)
    return Manifest(scenarios=ordered, rerun=rerun)


# One listed run's evidence.


@dataclass(frozen=True)
class Final:
    """A release's final event (its refund line)."""

    result: str
    reason: str
    credit_ns: int
    release_mono_ns: int
    final_mono_ns: int


@dataclass
class Release:
    deduction_id: str
    where: str  # its release line
    decision_ns: int  # its acquire's (the issue time, RCP-1)
    release_mono_ns: int  # its release line's (hitapi's monotonic clock)
    elapsed_ns: int | None = None  # limiter_ns + db_ns from its request line; None without one
    final: Final | None = None


@dataclass
class Scan:
    """What one run's logs give: the S6 rows' counts, and the refund joins' outcomes."""

    rows: dict[str, int] = field(default_factory=lambda: dict.fromkeys(APART_ROWS, 0))
    releases: int = 0
    confirmed: int = 0  # confirmed in time
    killed: int = 0  # no final event, on an instance killed before it
    drained: int = 0  # cut by the drain timeout
    after_p: int = 0  # released after P, not confirmed in time
    failures: list[str] = field(default_factory=list)


@dataclass(frozen=True)
class Listed:
    """A run the manifest lists: its scenario, its role there (baseline, limiter or rerun) and
    its directory."""

    scenario: str
    role: str
    run_id: str
    run_dir: Path

    @property
    def name(self) -> str:
        """The scenario its failures name: the rerun's is the manifest's key."""
        return RERUN if self.role == "rerun" else self.scenario


@dataclass(frozen=True)
class Limits:
    """A run's refund settings: P, refund_deadline, receipt_lifetime and the measured outage."""

    p_ns: int
    deadline_ns: int
    lifetime_ns: int
    outage_ns: int


@dataclass
class Run:
    listed: Listed
    ctx: rf.Context
    report: dict[str, object]
    config: rf.RunConfig
    failure_mode: str  # config.toml [policy] failure_mode
    scan: Scan | None = None


def listed_runs(manifest: Manifest, runs_dir: Path) -> list[Listed]:
    """Every run the manifest lists, in its order: each scenario's baseline, then its limiter
    run, then the rerun."""
    out: list[Listed] = []
    for name, entry in manifest.scenarios.items():
        for role, run_id in (("baseline", entry.baseline), ("limiter", entry.limiter)):
            if run_id is not None:
                out.append(Listed(name, role, run_id, runs_dir.joinpath(run_id)))
    if manifest.rerun is not None:
        out.append(Listed(RERUN_OF, "rerun", manifest.rerun, runs_dir.joinpath(manifest.rerun)))
    return out


def read_run(item: Listed) -> Run:
    """A listed run's context.json, report.json and config.toml, checked against the manifest
    (its own run ID, its scenario, and a baseline exactly where the manifest says baseline), and
    its failure mode: all check 4 counts of a run that fails on input after this (run_limits,
    scan_logs)."""
    ctx = rf.read_context(item.run_dir)
    report = rf.read_json(item.run_dir.joinpath(rf.REPORT_JSON))
    config = rf.read_config(item.run_dir)
    for key, mine, want in (
        ("run_id", ctx.run_id, item.run_id),
        ("scenario.name", ctx.scenario_name, item.scenario),
        ("baseline", ctx.baseline, item.role == "baseline"),
    ):
        if mine != want:
            raise InputError(
                rf.CONTEXT,
                key,
                f"{mine!r}: the manifest lists {item.run_id} as the {item.role} run of "
                f"{item.scenario}",
            )
    policy = rf.get_obj(config.raw, "policy", rf.CONFIG)
    mode = rf.get_str(policy, "failure_mode", POLICY)
    return Run(listed=item, ctx=ctx, report=report, config=config, failure_mode=mode)


def run_limits(run: Run) -> Limits:
    """A read run's refund settings (P and config.toml [policy]) and its measured outage
    (measured_outage), read after its failure mode, so a run that fails here still counts in
    check 4's failure modes; else InputError."""
    policy = rf.get_obj(run.config.raw, "policy", rf.CONFIG)
    tolerance = rf.get_int(policy, "outage_tolerance_ns", POLICY)
    return Limits(
        p_ns=run.config.p_ns,
        deadline_ns=rf.get_int(policy, "refund_deadline_ns", POLICY),
        lifetime_ns=rf.get_int(policy, "receipt_lifetime_ns", POLICY),
        outage_ns=measured_outage(run.ctx, tolerance),
    )


def measured_outage(ctx: rf.Context, tolerance_ns: int) -> int:
    """The run's measured outage, which the REF-5 bound adds: context.json's faults outage_ns,
    from at most one store outage segment (a faultctl store down or pause, a timed store refusal,
    or any fault with an outage_ns) of at most [policy] outage_tolerance_ns (its outage_ns, or a
    refusal's duration_ms), as REF-5 allows; else InputError (decision R3)."""
    segments = [
        (i, f)
        for i, f in enumerate(ctx.faults)
        if f.outage_ns is not None or is_outage_segment(f.cmd)
    ]
    if len(segments) > 1:
        shown = "; ".join(f"faults[{i}] {f.cmd}" for i, f in segments)
        raise InputError(
            rf.CONTEXT,
            "faults",
            f"{len(segments)} store outage segments ({shown}): REF-5 allows one, so "
            "refund_deadline_ns + the measured outage bounds no refund",
        )
    for i, f in segments:
        place = f"{rf.CONTEXT}: faults[{i}]"
        longer = f"longer than [policy] outage_tolerance_ns {tolerance_ns} (REF-5)"
        if f.outage_ns is not None and f.outage_ns > tolerance_ns:
            raise InputError(place, "outage_ns", f"{f.outage_ns}: a store outage {longer}")
        body = fault_body(f.cmd)
        if body is not None and is_outage_segment(f.cmd) and body.duration_ms * MS > tolerance_ns:
            raise InputError(
                place, "cmd", f"a store refusal of duration_ms {body.duration_ms}, {longer}"
            )
    return sum(f.outage_ns for _, f in segments if f.outage_ns is not None)


# The run logs: the S6 rows and the refund joins, one pass over each api-N.jsonl.


def apart_row(status: int, error: str, result: str) -> str | None:
    """The HARNESS §3 row among APART_ROWS of a request line, None for any other."""
    if status == 200 and error == "" and result == "degraded":
        return "ERR-3"
    if status == 503:
        return APART_503.get(error)
    return None


def past_deadline(final: Final, limits: Limits) -> str | None:
    """How final came later than refund_deadline_ns + the run's measured outage after its
    release, or None when it did not."""
    took = final.final_mono_ns - final.release_mono_ns
    bound = limits.deadline_ns + limits.outage_ns
    if took <= bound:
        return None
    reason = f" ({final.reason})" if final.reason else ""
    return (
        f"{final.result}{reason} {took} ns after its release (final_mono_ns − release_mono_ns), "
        f"above refund_deadline_ns {limits.deadline_ns} + the measured outage "
        f"{limits.outage_ns} ns = {bound}"
    )


def judge(rel: Release, kill_ns: int | None, limits: Limits, scan: Scan) -> None:
    """Counts one release's outcome in scan, or adds its failure: confirmed in time; else
    excluded when on an instance killed before its final event and within its deadline (kill_ns:
    the killed process's kill time on its own monotonic clock, None when it was not killed), cut
    by a drain timeout before its deadline, or released after P; else a failure."""
    final = rel.final
    if final is not None and final.result in CONFIRMED:
        expiry = rel.decision_ns + limits.lifetime_ns
        problems: list[str] = []
        if not final.credit_ns < expiry:
            problems.append(
                f"{final.result} at credit_decision_ns {final.credit_ns}, not before its expiry "
                f"{expiry} (decision_ns {rel.decision_ns} + receipt_lifetime_ns "
                f"{limits.lifetime_ns})"
            )
        late = past_deadline(final, limits)
        if late is not None:
            problems.append(late)
        if not problems:
            scan.confirmed += 1
            return
        why = "; ".join(problems)
    elif final is None:
        if kill_ns is None:
            why = "no final event, and its instance was not killed"
        else:
            ran = kill_ns - rel.release_mono_ns
            bound = limits.deadline_ns + limits.outage_ns
            if ran <= bound:
                scan.killed += 1  # killed before its deadline (REF-9 (c), decision D5)
                return
            why = (
                f"no final event, and its instance ran on {ran} ns after its release before it "
                f"was killed (release_mono_ns {rel.release_mono_ns}, its process's last "
                f"{kill_ns}), above refund_deadline_ns {limits.deadline_ns} + the measured outage "
                f"{limits.outage_ns} ns = {bound}: it kept running past its deadline, so REF-9 "
                "(c) still covered it (REF-5)"
            )
    elif final.result == UNRESOLVED and final.reason == DRAIN_CUT:
        late = past_deadline(final, limits)
        if late is None:
            scan.drained += 1  # the drain ended before its deadline (REF-9 (c), decision D3)
            return
        why = (
            f"{late}: its instance drained past its deadline, so REF-9 (c) still covered it (REF-5)"
        )
    else:
        reason = f" ({final.reason})" if final.reason else ""
        why = f"final event {final.result}{reason}: not confirmed"
    head = f"{rel.where}: deduction {rel.deduction_id}"
    if rel.elapsed_ns is None:
        scan.failures.append(
            f"{head}: {why}; it has no request line, so whether it was released within P "
            f"({limits.p_ns} ns) is unknown"
        )
    elif rel.elapsed_ns > limits.p_ns:
        scan.after_p += 1
    else:
        scan.failures.append(
            f"{head}: released {rel.elapsed_ns} ns after just before its acquire (limiter_ns + "
            f"db_ns, within P {limits.p_ns}): {why}"
        )


def record_of(rec: dict[str, object], where: str) -> tuple[str, str]:
    """rec's kind and, for a lifecycle line, its event ("" for another kind): each one DESIGN §B8
    gives api-N.jsonl, rec holding exactly its fields; a lifecycle line's fields are checked here
    too (the start line's limits and a stop or pre_kill line's drop counts, integers of at least 0,
    and pre_kill's read_ok), so a pre_kill line counts only when it is one (decision R1). Else
    InputError naming the field."""
    if "kind" not in rec:
        raise InputError(where, "kind", "missing")
    kind = rec["kind"]
    if not isinstance(kind, str):
        raise InputError(where, "kind", "want a string")
    event = ""
    if kind == LIFECYCLE:
        event = rf.get_str(rec, "event", where)
        keys = LIFECYCLE_KEYS.get(event)
        if keys is None:
            raise InputError(
                where, "event", f"{event!r}: not a lifecycle event ({LIFECYCLE_EVENTS})"
            )
    else:
        keys = RECORD_KEYS.get(kind)
        if keys is None:
            raise InputError(where, "kind", f"{kind!r}: not a run-log record kind ({RECORD_KINDS})")
    if rec.keys() != keys:
        rf.exact_keys(rec, keys, where)
    if kind == LIFECYCLE:
        if event == "pre_kill":
            rf.get_bool(rec, "read_ok", where)
        for key in sorted(keys - {"kind", "event", "read_ok"}):
            rf.get_int(rec, key, where)
    return kind, event


def scan_file(path: Path, limits: Limits, scan: Scan) -> None:
    """One instance's run log, one line at a time (rf.iter_jsonl), each line checked
    (record_of and the fields read below) before it counts."""
    decisions: dict[str, int] = {}  # allowed decisions whose request line is still to come
    releases: dict[str, Release] = {}  # this segment's releases
    killed = False
    last_mono = 0  # the latest release_mono_ns of this segment (its process's monotonic clock)

    def end_segment() -> None:
        kill_ns = last_mono if killed else None  # the kill time the run log gives (decision D5)
        for rel in releases.values():
            judge(rel, kill_ns, limits, scan)
        decisions.clear()
        releases.clear()

    for where, rec in rf.iter_jsonl(path):
        kind, event = record_of(rec, where)
        if kind == LIFECYCLE:
            if event == "start":
                end_segment()
                killed, last_mono = False, 0
            elif event == "pre_kill":
                killed = True
            elif event == "stop":
                killed = False  # it drained and closed: whatever came before, it was not killed
        elif kind == "decision":
            deduction = rf.get_str(rec, "deduction_id", where)
            rf.get_str(rec, "user_id", where)
            issued = rf.get_int(rec, "decision_ns", where)
            result = rf.get_str(rec, "result", where)
            if result not in DECISION_RESULTS:
                raise InputError(where, "result", f"{result!r}: want allowed or degraded")
            if deduction and result == "allowed":
                decisions[deduction] = issued
        elif kind == "release":
            deduction = rf.get_str(rec, "deduction_id", where)
            released = rf.get_int(rec, "release_mono_ns", where)
            last_mono = max(last_mono, released)
            if not deduction:
                continue  # a degraded permit: no receipt, no final event (REF-2, API-1d)
            if deduction in releases:
                raise InputError(where, "deduction_id", f"{deduction}: a second release line")
            decided = decisions.pop(deduction, None)
            if decided is None:
                raise InputError(
                    where, "deduction_id", f"{deduction}: no allowed decision line before it"
                )
            releases[deduction] = Release(deduction, where, decided, released)
            scan.releases += 1
        elif kind == "request":
            status = rf.get_int(rec, "http_status", where, 100)
            error = rf.get_str(rec, "error", where)
            result = rf.get_str(rec, "result", where)
            deduction = rf.get_str(rec, "deduction_id", where)
            limiter_ns = rf.get_int(rec, "limiter_ns", where)
            db_ns = rf.get_int(rec, "db_ns", where)
            row = apart_row(status, error, result)
            if row is not None:
                scan.rows[row] += 1
            if not deduction:
                continue
            rel = releases.get(deduction)
            if rel is None:
                decisions.pop(deduction, None)
                continue
            rel.elapsed_ns = limiter_ns + db_ns
        else:  # a refund line: a final event (SPEC API-1d)
            deduction = rf.get_str(rec, "deduction_id", where)
            result = rf.get_str(rec, "result", where)
            if result not in FINAL_RESULTS:
                raise InputError(where, "result", f"{result!r}: not a final event (SPEC API-1d)")
            reason = rf.get_str(rec, "reason", where)
            if result == UNRESOLVED and reason not in UNRESOLVED_REASONS:
                raise InputError(
                    where, "reason", f"{reason!r}: want closed, retention or expired (unresolved)"
                )
            if result != UNRESOLVED and reason:
                raise InputError(where, "reason", f"{reason!r}: only unresolved has a reason")
            rf.get_int(rec, "attempts", where)
            credit = rf.get_int(rec, "credit_decision_ns", where)
            released = rf.get_int(rec, "release_mono_ns", where)
            final = rf.get_int(rec, "final_mono_ns", where)
            if final < released:
                raise InputError(where, "final_mono_ns", "before release_mono_ns")
            rel = releases.get(deduction)
            if rel is None:
                raise InputError(
                    where, "deduction_id", f"{deduction!r}: a final event with no release before it"
                )
            if rel.final is not None:
                raise InputError(where, "deduction_id", f"{deduction}: a second final event")
            rel.final = Final(
                result=result,
                reason=reason,
                credit_ns=credit,
                release_mono_ns=released,
                final_mono_ns=final,
            )
    end_segment()


def scan_logs(run: Run) -> Scan:
    """A read run's refund limits (run_limits), then each of its run logs (scan_file)."""
    limits = run_limits(run)
    scan = Scan()
    for instance in range(1, run.ctx.instances + 1):
        scan_file(run.listed.run_dir.joinpath(rf.api_file(instance)), limits, scan)
    return scan


# The checks, in plan step 4's order.


class Checks:
    def __init__(self) -> None:
        self.failures: list[Finding] = []
        self.skipped: list[Finding] = []  # checks an input failure left undecided

    def fail(self, scenario: str, run_id: str, check: str, detail: str) -> None:
        self.failures.append(Finding(scenario, run_id, check, detail))

    def skip(self, scenario: str, run_id: str, check: str, detail: str) -> None:
        self.skipped.append(Finding(scenario, run_id, check, detail))


def check_listed(manifest: Manifest, checks: Checks) -> None:
    """0. Every H6 scenario is in the manifest with its limiter run."""
    for name in H6_SCENARIOS:
        entry = manifest.scenarios.get(name)
        if entry is None:
            runs = "baseline or limiter run" if is_bench(name) else "limiter run"
            checks.fail(
                name,
                "-",
                CHECK_LISTED,
                f"not in the manifest: no {runs} of it (the orchestrator indexes only a run "
                "that finished without failure)",
            )
        elif entry.limiter is None:
            only = "" if entry.baseline is None else f", only its baseline {entry.baseline}"
            checks.fail(name, "-", CHECK_LISTED, f"no limiter run in the manifest{only}")


def check_valid(runs: list[Run], checks: Checks) -> None:
    """1. evidence_validity.valid = true, and report/validate.py passes the run."""
    for run in runs:
        item = run.listed
        valid = validate.at(run.report, "evidence_validity.valid")
        if valid is not True:
            checks.fail(
                item.name,
                item.run_id,
                CHECK_VALID,
                f"report.json evidence_validity.valid {validate.show(valid)}: want true",
            )
        mode = "baseline" if run.ctx.baseline else "full"
        failed = validate.check(item.run_dir, mode)
        if failed:
            lines = "; ".join(f.line() for f in failed)
            checks.fail(item.name, item.run_id, CHECK_VALIDATE, f"{mode} mode: {lines}")


def check_baselines(manifest: Manifest, runs: dict[Listed, Run], checks: Checks) -> None:
    """2. Each benchmark scenario has a baseline with its limiter run's schedule_sha256."""
    by_role = {(item.scenario, item.role): run for item, run in runs.items()}
    for name, entry in manifest.scenarios.items():
        if not is_bench(name):
            continue
        if entry.baseline is None:
            checks.fail(
                name,
                entry.limiter or "-",
                CHECK_BASELINE,
                "no baseline run in the manifest: a benchmark limiter run needs one (DESIGN §B9)",
            )
            continue
        run, base = by_role.get((name, "limiter")), by_role.get((name, "baseline"))
        if run is None or base is None:
            continue  # no limiter run (check 0 fails it) or an input failure, reported
        mine, theirs = run.ctx.schedule_sha256, base.ctx.schedule_sha256
        if mine is None or theirs != mine:
            checks.fail(
                name,
                run.ctx.run_id,
                CHECK_SCHEDULE,
                f"schedule_sha256 {mine}, baseline {entry.baseline} {theirs}",
            )


def check_correctness(runs: list[Run], checks: Checks) -> None:
    """3. Each limiter run, the rerun included, has correctness.verdict = "pass"."""
    for run in runs:
        if run.listed.role == "baseline":
            continue
        verdict = validate.at(run.report, "correctness.verdict")
        if verdict != "pass":
            checks.fail(
                run.listed.name,
                run.listed.run_id,
                CHECK_CORRECTNESS,
                f'report.json correctness.verdict {validate.show(verdict)}: want "pass"',
            )


def is_s6_limiter(item: Listed) -> bool:
    return item.scenario.startswith(S6_PREFIX) and item.role == "limiter"


def check_s6(runs: list[Run], unscanned: list[Run], unread: list[Listed], checks: Checks) -> None:
    """4. Both failure modes among the S6 runs, with ERR-9 and ERR-16 counted apart from ERR-8
    and ERR-3. unscanned are the runs that failed on input after their failure mode was read (in
    their refund settings, their outage evidence or their run logs: their failure modes count,
    not their logs), unread those that failed before it: a mode or its ERR-9/ERR-16 they leave
    undecided is skipped, naming the run and whether it failed before or after its failure mode
    was read."""
    s6 = [run for run in runs if is_s6_limiter(run.listed)]
    late = [run for run in unscanned if is_s6_limiter(run.listed)]
    partial = {run.failure_mode for run in late}
    blind = ", ".join(item.run_id for item in unread if is_s6_limiter(item))
    seen = {run.failure_mode for run in s6} | partial
    unsure = set(FAILURE_MODES) if blind else partial

    def cause(mode: str) -> str:
        causes = []
        after = ", ".join(run.listed.run_id for run in late if run.failure_mode == mode)
        if after:
            causes.append(
                f"S6 limiter run {after} in {mode} mode failed on input after its failure mode "
                "was read"
            )
        if blind:
            causes.append(
                f"S6 limiter run {blind} failed on input before its failure mode was read"
            )
        return "; ".join(causes)

    for mode in FAILURE_MODES:
        if mode in seen:
            continue
        if blind:
            checks.skip(S6, "-", CHECK_MODES, f"{mode} mode not judged: {cause(mode)}")
        else:
            checks.fail(S6, "-", CHECK_MODES, f"no S6 limiter run in {mode} mode")
    totals = {mode: dict.fromkeys(APART_ROWS, 0) for mode in seen}
    for run in s6:
        if run.scan is None:
            continue  # its input failure is reported
        counted = []
        for row in APART_ROWS:
            logged = run.scan.rows[row]
            totals[run.failure_mode][row] += logged
            for path in (f"results.by_err.{row}", f"results.prf2.{PRF2_NAMES[row]}"):
                value = validate.at(run.report, path)
                if type(value) is not int or value != logged:
                    counted.append(f"{path} {validate.show(value)}, the run logs {logged}")
        if counted:
            checks.fail(
                run.listed.name,
                run.listed.run_id,
                CHECK_APART,
                "report.json does not count ERR-9, ERR-16, ERR-8 and ERR-3 as the request lines "
                f"give them: {'; '.join(counted)}",
            )
    for mode in FAILURE_MODES:
        if mode not in seen:
            continue  # reported above
        for row in ("ERR-9", "ERR-16"):
            if totals[mode][row] > 0:
                continue
            if mode in unsure:
                checks.skip(S6, "-", CHECK_APART, f"{row} in {mode} mode not judged: {cause(mode)}")
            else:
                checks.fail(
                    S6,
                    "-",
                    CHECK_APART,
                    f"no {row} in any S6 run in {mode} mode: nothing shows it counted apart",
                )


def check_refunds(runs: list[Run], checks: Checks) -> None:
    """5. Every release covered by REF-5/REF-9 confirmed before its expiry and in time."""
    for run in runs:
        if run.scan is None:
            continue
        for detail in run.scan.failures:
            checks.fail(run.listed.name, run.listed.run_id, CHECK_REFUNDS, detail)


def check_rerun(manifest: Manifest, runs: dict[Listed, Run], checks: Checks) -> None:
    """6. The s3-busy-user rerun with seed 1 has the limiter run's schedule_sha256."""
    if manifest.rerun is None:
        checks.fail(RERUN, "-", CHECK_RERUN, "no s3-busy-user rerun in the manifest")
        return
    by_role = {(item.scenario, item.role): run for item, run in runs.items()}
    rerun, first = by_role.get((RERUN_OF, "rerun")), by_role.get((RERUN_OF, "limiter"))
    entry = manifest.scenarios.get(RERUN_OF)
    if entry is None or entry.limiter is None:
        checks.fail(RERUN, manifest.rerun, CHECK_RERUN, "no s3-busy-user limiter run to match")
        return
    if rerun is None or first is None:
        return  # an input failure is reported
    problems = []
    if rerun.ctx.run_id == first.ctx.run_id:
        problems.append("the rerun is the limiter run itself")
    for label, run in (("the rerun", rerun), ("the limiter run", first)):
        if run.ctx.seed != RERUN_SEED:
            problems.append(f"{label} has seed {run.ctx.seed}, not {RERUN_SEED}")
    mine, theirs = rerun.ctx.schedule_sha256, first.ctx.schedule_sha256
    if mine is None or mine != theirs:
        problems.append(f"schedule_sha256 {mine}, the limiter run {first.ctx.run_id} {theirs}")
    if problems:
        checks.fail(RERUN, rerun.ctx.run_id, CHECK_RERUN, "; ".join(problems))


@dataclass(frozen=True)
class Outcome:
    failures: list[Finding]
    skipped: list[Finding]
    runs: int
    releases: int
    confirmed: int
    excluded: dict[str, int]


def check_manifest(path: Path) -> Outcome:
    """Every check of plan step 4 on the runs the manifest at path lists."""
    checks = Checks()
    try:
        manifest = read_manifest(path)
    except InputError as err:
        checks.fail("-", "-", CHECK_MANIFEST, str(err))
        return Outcome(checks.failures, [], 0, 0, 0, {})
    check_listed(manifest, checks)
    listed = listed_runs(manifest, path.parent)
    runs: dict[Listed, Run] = {}
    unread: list[Listed] = []  # failed on input before its failure mode was read
    unscanned: list[Run] = []  # failed on input after it: in its refund limits or run logs
    for item in listed:
        try:
            run = read_run(item)
        except InputError as err:
            checks.fail(item.name, item.run_id, CHECK_INPUT, str(err))
            unread.append(item)
            continue
        try:
            run.scan = scan_logs(run)
        except InputError as err:
            checks.fail(item.name, item.run_id, CHECK_INPUT, str(err))
            unscanned.append(run)
            continue
        runs[item] = run
    ordered = list(runs.values())
    check_valid(ordered, checks)
    check_baselines(manifest, runs, checks)
    check_correctness(ordered, checks)
    check_s6(ordered, unscanned, unread, checks)
    check_refunds(ordered, checks)
    check_rerun(manifest, runs, checks)
    scans = [run.scan for run in ordered if run.scan is not None]
    return Outcome(
        failures=checks.failures,
        skipped=checks.skipped,
        runs=len(listed),
        releases=sum(s.releases for s in scans),
        confirmed=sum(s.confirmed for s in scans),
        excluded={
            "killed": sum(s.killed for s in scans),
            "drained": sum(s.drained for s in scans),
            "after_p": sum(s.after_p for s in scans),
        },
    )


def main(argv: list[str], out: TextIO | None = None, err: TextIO | None = None) -> int:
    stream = out if out is not None else sys.stdout
    errors = err if err is not None else sys.stderr
    parser = argparse.ArgumentParser(
        prog="report/completion.py",
        description="Check HARNESS §8 Completion over the H6 runs (IMPLEMENTATION_PLAN H6 step 4).",
    )
    parser.add_argument("manifest", nargs="?", type=Path, help="runs/h6-manifest.json")
    parser.add_argument(
        "--build-manifest",
        metavar="INDEX",
        type=Path,
        help="print the manifest built from runs/index.jsonl instead (plan H6 step 3)",
    )
    args = parser.parse_args(argv)
    if (args.manifest is None) == (args.build_manifest is None):
        parser.error("give either a manifest or --build-manifest INDEX")
    if args.build_manifest is not None:
        try:
            built = build_manifest(args.build_manifest)
        except InputError as error:
            print(f"completion: {args.build_manifest}: {error}", file=errors)
            return 1
        print(json.dumps(built.to_json(), indent=2), file=stream)
        return 0
    outcome = check_manifest(args.manifest)
    for failure in outcome.failures:
        print(failure.line("FAIL"), file=stream)
    for skipped in outcome.skipped:
        print(skipped.line("SKIP"), file=stream)
    excluded = ", ".join(f"{name} {n}" for name, n in outcome.excluded.items())
    summary = (
        f"{outcome.runs} runs; {outcome.releases} releases: {outcome.confirmed} confirmed in "
        f"time, excluded {excluded or 'none'}"
    )
    if outcome.failures:
        print(
            f"completion: {args.manifest}: {len(outcome.failures)} failed; {summary}", file=stream
        )
        return 1
    print(f"completion: {args.manifest}: every check holds; {summary}", file=stream)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
