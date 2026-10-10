"""`python3 report/validate.py runs/<id> [--mode full|baseline]`: checks a finished run's report
(IMPLEMENTATION_PLAN H5 step 6; DESIGN §C5, §B8, §B9; HARNESS T-RPT-1, T-RPT-2).

It reads runs/<id>/report.json (built by `python3 -m report runs/<id>`), context.json and the
scenario file context.json pins by its SHA-256, and for the T-RPT-2 checks the run's copy of its
frozen thresholds (runs/<id>/thresholds.json), the frozen file itself
(runs/thresholds/<scenario>-<seed>.json) and the baseline run's context.json. It imports nothing
from the report package: the orchestrator runs it as a script, and its recomputation of the
capacity verdicts stays independent of the builder's code. Which checks apply comes from the
run's evidence, not from report.json's claims: the run kind and baseline flag from context.json,
the verdict and forced_aof_rewrites_s from the scenario file.

- Both modes: report.json is this run's (its run_id is context.json's); the run ended ok
  (context.json outcome); a benchmark run recorded its PRF-5 items (those the orchestrator's
  missingPRF5 requires); the §B8 evidence validity checks, each recorded value judged again by
  its rule (not only its ok), and valid = true, the BUD-11a verdict among them (HAR-13: check_bud11
  says pass or violation, not invalid, as report.json's correctness does when it has one); no step
  whose recorded p99 scheduling lag is above 5 ms says it is not generator-limited (§B9).
- Full mode: the T-RPT-1 keys and their PRF-1…PRF-5 fields, nested ones included (each timing
  class's p50, p95 and p99, each ERR row and refund result, each step's fields), correctness
  (every field of check_bud11's verdict JSON, a violation's window included) and
  evidence_validity, and for a benchmark run (run_kind = bench) also baseline and capacity (a test
  run needs neither). A benchmark limiter run needs its baseline comparison: the baseline run is a
  baseline of the same scenario and seed, and its schedule_sha256 equals this run's. A benchmark
  limiter run whose scenario has verdict capacity or spike adds capacity's fields and the T-RPT-2
  checks: the SHA-256 of runs/<id>/thresholds.json = context thresholds_sha256 = the SHA-256 of
  the frozen file; the file's created_ns < the run's start_ns; the file was frozen from this run's
  schedule (its schedule_sha256 is context.json's); the file holds the thresholds its baseline
  gives (PRF-7: never relaxed or edited): the run named by its baseline_run_id is a baseline of
  this run's scenario, seed and schedule, and each step recomputed from that run's report.json as
  report/thresholds.py freezes it (§B9 step 2: window p99 + 50 ms, max(0.1 %, 2 × the window
  unexpected-error rate), the generator_limited flags, a step that did not run in full or has no
  latency population left out) has the frozen values; no verdict on a generator-limited step; each
  step's verdict and sustainable_capacity as recomputed here; and for a spike, the deadline and
  window recorded in the thresholds file and met as recomputed here. So the baseline run that
  froze a thresholds file, runs/<baseline_run_id>/, must be kept while the file is in use: without
  it every later capacity or spike limiter run of that scenario and seed fails thresholds_file,
  naming it ("the baseline run runs/<id> that froze these thresholds is missing").
- Baseline mode (a baseline run, limiter_enabled = false): the T-RPT-1 keys except baseline.

The recomputed verdicts take each step's baseline-step conditions from the frozen thresholds, which
the baseline that set them wrote: a step with values had a baseline step that ran in full and was
not generator-limited, one marked generator_limited had a generator-limited one, one not named had
none. The spike's met applies the orchestrator's reading of plan H5 step 4 under PRF-6 (the
spike's backlog clears by the deadline): an initial attempt is outstanding when it was planned
before the deadline − the post-spike step's latency_threshold_ns from the frozen thresholds and
has no response or failure logged by the deadline, and every initial attempt planned before the
deadline that was never written (generator drops included) is. The builder counts them; here the
report's outstanding_cutoff_ns must be that deadline − threshold (the deadline when the file gives
the post-spike step none), and met needs an outstanding count of 0.

Every number is range-checked (integers 0 to 2^63 − 1, finite floats of at least 0): a value out
of range fails its check. Every count in the sections check_keys reads (COUNT_FIELDS and the
other count tables below it: PRF-1's planned, sent, retry, drop and per-step counts and the
gen.jsonl summary's; PRF-2's results, unexpected errors, decisions, releases, attempt outcomes
and transport errors by kind; each percentile summary's sample count; PRF-4's readings, the
store's tracked keys, live markers and quarantined keys, and each count series' max and last,
by the series' name (COUNT_SERIES: hitapi's in-flight requests and drop counts, the store-state
gauges, pending refunds, the listener-drop and cleanup counters); the schedule and routing
counts; check_bud11's; the capacity step and spike counts; and the §B8 evidence counts no
evidence rule reads in every run, EVIDENCE_COUNTS: the stop and pre_kill lines, fixtures_off's
readings and faults, and the rewrite windows) must be such an integer, never a bool or a float
(1.0 included), whether or not a check reads it, or null where the report has no reading
(NULLABLE_COUNT_FIELDS, NULLABLE_MEMBER_COUNT_FIELDS, a count series' max and last): anything
else fails required_keys, as do an object or a list of counts that is none and an instance
series whose name the report builder never writes (its set of names is closed). Not counts, so
not held to this: step indexes (step, stopped_after_step, excluded_step, spike_step,
post_spike_step, sustainable_step), instance numbers and PIDs (identifiers), times and durations
in ns, percentiles, rates and the other measurements (CPU %, RSS bytes, a lag series' max and
last: LAG_SERIES, durations in seconds and µs), and the settings context copies from the run
(PRF-5); these keep the checks that read them. Nor are the evidence counts a §B8 rule reads in
every run (log_drops, listener_drops' sum, pre_kill_unread and segments_without_count, outage's
unmeasured, outage_segments, gen_summary): each is judged again by its rule, which takes nothing
but an integer (0, or at most 1 outage segment), so a bool, a float, a negative number or a
string fails that rule's check. Exit 0 valid, 1 invalid (one "FAIL <check>: <detail>" line for
each failed check), 2 for a wrong command line.
"""

import argparse
import hashlib
import json
import math
import re
import sys
import tomllib
from dataclasses import dataclass
from pathlib import Path
from typing import TextIO

MODES = ("full", "baseline")
# The largest JSON or TOML document read whole, in bytes (as the report builder's limit).
MAX_DOC = 64 << 20
# §B8 and §A7 item 5: a measured store outage of at most 5 s, at most one outage segment.
MAX_OUTAGE_NS = 5_000_000_000
MAX_OUTAGE_SEGMENTS = 1
# §B9: a step whose p99 scheduling lag is above 5 ms is generator-limited.
ALLOWED_LAG_NS = 5_000_000
# §B9 step 2, as report/thresholds.py freezes them (recomputed here from the baseline's report):
# a step's latency threshold is its window p99 + 50 ms, its error threshold max(0.1 %, 2 × its
# window unexpected-error rate).
LATENCY_ALLOWANCE_NS = 50_000_000
ERROR_RATE_FLOOR = 0.001
# The largest integer read (the harness writes int64 values).
MAX_INT = (1 << 63) - 1
# A scenario name, one path component (harness/config nameRe).
SAFE_NAME = re.compile(r"[A-Za-z0-9_-]{1,64}")
# A run ID, one path component (the orchestrator's runID: YYYYMMDD-HHMMSS-<scenario>-<seed>).
RUN_ID = re.compile(r"[0-9]{8}-[0-9]{6}-[A-Za-z0-9_-]{1,64}-[0-9]{1,19}")
# A scenario's verdict (DESIGN §B4; none when the file sets none).
VERDICTS = ("capacity", "spike", "none")

# report.json's T-RPT-1 sections (without baseline, which only benchmark limiter runs have).
TRPT1_KEYS = ("context", "load", "results", "timing", "clocks", "resources", "traffic")
# A latency or lag summary's nearest-rank percentiles (PRF-1, PRF-3).
PERCENTILES = ("p50", "p95", "p99")
# The HARNESS §3 ERR rows, each counted in results.by_err (PRF-2).
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
# The five refund results, each counted in results.refunds (PRF-2).
REFUND_RESULTS = ("applied", "already_applied", "receipt_expired", "receipt_invalid", "unresolved")
# The limiter decision-time classes and the end-to-end classes, each with its percentiles (PRF-3).
LIMITER_CLASSES = ("allowed", "limited", "degraded", "error")
END_TO_END_CLASSES = ("all", "success", "limited", "error", "retries")
# The attempt kinds whose outcomes results.attempt_outcomes counts (§B4).
ATTEMPT_KINDS = ("initial", "duplicate", "retry")
# PRF-1's first-attempt counts, in load.total and in each load step: planned vs sent, duplicates
# and retries apart, the unsent.
LOAD_COUNTS = ("planned", "sent", "initial", "duplicates", "retries_429", "unsent")
# PRF-2's counts by name (results.prf2).
PRF2_COUNTS = (
    "new_rows",
    "duplicate_rows_avoided",
    "quota_rejections",
    "degraded_admissions",
    "capacity_refusals",
    "integrity_failures",
    "unavailable",
    "unknown_acquires",
    "unknown_application_outcomes",
    "server_failures",
    "transport_errors",
    "expired_refunds",
    "unresolved_refunds",
    "db_rows",
)
# check_bud11's counts in its verdict JSON: the credits it counted, the listener drops and the
# pre_kill lines and failed reads it judged.
VERDICT_COUNTS = (
    "known_credits",
    "unknown_credits",
    "listener_drops",
    "pre_kill_lines",
    "pre_kill_read_ok_false",
)
# The fields of check_bud11's run-log verdict JSON (plan H5 step 8) that correctness holds, each
# in every one of its shapes (pass, violation, invalid): the verdict, its counts, the reasons for
# an invalid verdict, and the violation (null unless the verdict is violation).
VERDICT_FIELDS = ("verdict", *VERDICT_COUNTS, "invalid_reasons", "violation")
# The fields of check_bud11's violation, in its violation shape: the window that broke BUD-11a.
VIOLATION_FIELDS = ("user_id", "start_ns", "end_ns", "excess_units")


def percentile_fields(section: str, path: str) -> tuple[tuple[str, str], ...]:
    """The p50, p95 and p99 of the summary at path in section."""
    return tuple((section, f"{path}.{p}") for p in PERCENTILES)


# The PRF-1…PRF-5 fields each section must hold (T-RPT-1), as dotted paths inside it, nested ones
# included (plan H5 step 6: full mode checks every key of step 2); capacity's (plan H5 step 2;
# PRF-10, PRF-11) apply to a capacity or spike benchmark limiter run.
FIELDS = (
    # PRF-5: run context.
    ("context", "run_id"),
    ("context", "epoch_id"),
    ("context", "run_kind"),
    ("context", "label"),
    ("context", "scenario"),
    ("context", "seed"),
    ("context", "hardware"),
    ("context", "versions"),
    ("context", "store_settings"),
    ("context", "postgres_settings"),
    ("context", "policy"),
    ("context", "instance_settings"),
    ("context", "p_ns"),
    ("context", "start_ns"),
    ("context", "end_ns"),
    ("context", "faults"),
    ("context", "schedule_sha256"),
    ("context", "thresholds_sha256"),
    ("context", "workload"),
    ("context", "conformance_floor"),
    # PRF-1: load.
    ("load", "steps"),
    *(("load", f"total.{name}") for name in LOAD_COUNTS),
    ("load", "total.sched_lag_ns"),
    *percentile_fields("load", "total.sched_lag_ns"),
    ("load", "total.got_conn_wait_ns_p99"),
    ("load", "stopped_after_step"),
    ("load", "generator_drops"),
    # PRF-2: results.
    ("results", "requests"),
    ("results", "by_err"),
    *(("results", f"by_err.{row}") for row in ERR_ROWS),
    ("results", "attempt_outcomes"),
    *(("results", f"attempt_outcomes.{kind}") for kind in ATTEMPT_KINDS),
    ("results", "decisions"),
    ("results", "refunds"),
    *(("results", f"refunds.{result}") for result in REFUND_RESULTS),
    ("results", "transport_errors"),
    ("results", "transport_errors.total"),
    ("results", "unexpected_errors"),
    ("results", "unexpected_errors.total"),
    ("results", "unexpected_errors.steps"),
    ("results", "db_rows"),
    *(("results", f"prf2.{name}") for name in PRF2_COUNTS),
    # PRF-3: timing.
    *(("timing", f"limiter_ns.{cls}") for cls in LIMITER_CLASSES),
    *(f for cls in LIMITER_CLASSES for f in percentile_fields("timing", f"limiter_ns.{cls}")),
    ("timing", "db_ns"),
    *percentile_fields("timing", "db_ns"),
    ("timing", "refund_ns"),
    *percentile_fields("timing", "refund_ns.all"),
    ("timing", "end_to_end_ns"),
    *(f for cls in END_TO_END_CLASSES for f in percentile_fields("timing", f"end_to_end_ns.{cls}")),
    ("timing", "steps"),
    ("timing", "rewrite_p99_ns"),
    # The clock behind each field: arrival, decision and monotonic times kept apart.
    ("clocks", "fields"),
    ("clocks", "t0_wall_ns"),
    # PRF-4: resources.
    ("resources", "processes"),
    ("resources", "store_state"),
    *(
        ("resources", f"store_state.{name}.{value}")
        for name in ("tracked_buckets", "live_markers")
        for value in ("max", "last")
    ),
    ("resources", "instance_series"),
    ("resources", "max_in_flight"),
    # The run-log schedule and routing (TRF-1, TRF-4).
    ("traffic", "schedule_path"),
    ("traffic", "schedule_sha256"),
    ("traffic", "planned_per_step"),
    ("traffic", "requests_per_instance"),
    ("traffic", "instances_used"),
    ("traffic", "busiest_user"),
    ("traffic", "busiest_user.user"),
    ("traffic", "busiest_user.instances"),
    # The baseline comparison.
    ("baseline", "run_id"),
    ("baseline", "schedule_sha256"),
    ("baseline", "steps"),
    # The BUD-11a check's verdict JSON.
    *(("correctness", name) for name in VERDICT_FIELDS),
    # The capacity verdicts.
    ("capacity", "steps"),
    ("capacity", "sustainable_capacity"),
)
# The fields every element of a list must hold, as dotted paths inside it: (section, the list's
# path, the fields).
LIST_FIELDS = (
    # PRF-1 per step: planned vs sent first attempts, retries apart, scheduling lag.
    (
        "load",
        "steps",
        (
            *LOAD_COUNTS,
            "sched_lag_ns",
            *(f"sched_lag_ns.{p}" for p in PERCENTILES),
            "got_conn_wait_ns_p99",
            "generator_limited",
        ),
    ),
    # Each step's latency population in its observation window (§B9).
    ("timing", "steps", tuple(f"latency_ns.{p}" for p in PERCENTILES)),
    # Each step's unexpected errors in its observation window (§B9).
    ("results", "unexpected_errors.steps", ("attempts", "unexpected", "rate")),
    # PRF-4: each instance's series, its max and last reading.
    ("resources", "instance_series", ("instance", "series", "max", "last")),
    # The baseline comparison per step: p99 and unexpected-error-rate deltas.
    (
        "baseline",
        "steps",
        (
            "p99_ns",
            "baseline_p99_ns",
            "p99_delta_ns",
            "unexpected_rate",
            "baseline_unexpected_rate",
            "unexpected_rate_delta",
        ),
    ),
    # Per step offered, achieved, admitted, 429 and unique-row throughput, p99 and unexpected-
    # error rate against their thresholds, and the verdict.
    (
        "capacity",
        "steps",
        (
            "offered_per_s",
            "achieved_sent_per_s",
            "admitted_per_s",
            "limited_429_per_s",
            "unique_rows_per_s",
            "p99_ns",
            "latency_threshold_ns",
            "unexpected_rate",
            "error_rate_threshold",
            "verdict",
        ),
    ),
)
# A load step's counts beside LOAD_COUNTS: its schedule entries, 200s and 429s, and unknown
# outcomes (ERR-9, ERR-16).
LOAD_STEP_COUNTS = (
    "scheduled",
    "status_200",
    "status_429",
    "unknown_acquires",
    "unknown_application_outcomes",
)
# The gen.jsonl summary line's counts, as load.summary copies them (§B4).
GEN_SUMMARY_COUNTS = ("planned", "launched", "unsent", "log_drops", "skipped_no_eligible")
# The §B9 unexpected-error counts, in results.unexpected_errors.total and in each step: the
# denominator, the unexpected, their kinds and the 5xx left out.
UNEXPECTED_COUNTS = (
    "attempts",
    "unexpected",
    "transport_errors",
    "server_errors",
    "excluded_service_aborted_after_acquire",
    "excluded_fault_window",
)
# PRF-3's percentile summaries, each with its sample count (count, p50, p95, p99), as paths in
# timing.
TIMING_SUMMARIES = (
    *(f"limiter_ns.{cls}" for cls in LIMITER_CLASSES),
    "db_ns",
    "api_total_ns",
    *(f"refund_ns.{name}" for name in ("all", *REFUND_RESULTS)),
    *(f"end_to_end_ns.{cls}" for cls in END_TO_END_CLASSES),
    "rewrite_latency_ns",
)
# A capacity step's counts behind its rates and verdict (PRF-10, PRF-11).
CAPACITY_STEP_COUNTS = (
    "admitted",
    "limited_429",
    "unique_rows",
    "window_attempts",
    "window_unexpected",
    "unsent",
    "unknown_outcomes",
)
# A spike's counts (PRF-6): its window's latency samples, attempts, unexpected errors, planned and
# unsent entries, and the first attempts before its deadline, finished or outstanding.
SPIKE_COUNTS = (
    "window_latency_ns.count",
    "window_attempts",
    "window_unexpected",
    "window_planned",
    "window_unsent",
    "planned_before_deadline",
    "first_attempts_before_deadline",
    "finished_by_deadline",
    "not_judged",
    "outstanding",
    "outstanding_detail.never_written",
    "outstanding_detail.ended_after_deadline",
    "outstanding_detail.no_line",
)
# The §B8 evidence values (evidence_validity.checks) that are counts no evidence rule reads in
# every run: the stop and pre_kill lines listener_drops was summed over (no rule reads them),
# fixtures_off's /health reads, fixture readings and faults (read for a benchmark run only), and
# the AOF rewrite windows (read only when the scenario sets forced_aof_rewrites_s).
EVIDENCE_COUNTS = (
    "listener_drops.value.stop_lines",
    "listener_drops.value.pre_kill_lines",
    "fixtures_off.value.health_reads",
    "fixtures_off.value.fixtures_on",
    "fixtures_off.value.clock_not_store",
    "fixtures_off.value.faults",
    "rewrite_windows.value.windows",
)
# The counts at fixed paths, each an integer from 0 to 2^63 − 1, never a bool or a float (as_int):
# PRF-1's first-attempt totals, schedule entries, lag sample count and summary line counts; PRF-2's
# requests, releases, ERR rows, refund results, transport errors, unexpected errors, final row
# count and prf2 counts; PRF-3's sample counts; PRF-4's sample and /health read counts; the
# schedule and routing counts; check_bud11's counts and a violation's excess units; a spike's;
# and EVIDENCE_COUNTS.
COUNT_FIELDS = (
    *(("load", f"total.{name}") for name in (*LOAD_COUNTS, "scheduled")),
    ("load", "total.sched_lag_ns.count"),
    *(("load", f"summary.{name}") for name in GEN_SUMMARY_COUNTS),
    ("results", "requests"),
    ("results", "releases"),
    *(("results", f"by_err.{row}") for row in ERR_ROWS),
    *(("results", f"refunds.{result}") for result in REFUND_RESULTS),
    ("results", "transport_errors.total"),
    *(("results", f"unexpected_errors.total.{name}") for name in UNEXPECTED_COUNTS),
    ("results", "db_rows"),
    *(("results", f"prf2.{name}") for name in PRF2_COUNTS),
    *(("timing", f"{path}.count") for path in TIMING_SUMMARIES),
    ("resources", "samples"),
    ("resources", "health_reads"),
    ("traffic", "schedule_entries"),
    ("traffic", "instances"),
    ("traffic", "instances_used"),
    ("traffic", "users"),
    ("traffic", "busiest_user.first_attempts"),
    *(("correctness", name) for name in VERDICT_COUNTS),
    ("correctness", "violation.excess_units"),
    *(("capacity", f"spike.{name}") for name in SPIKE_COUNTS),
    *(("evidence_validity", f"checks.{path}") for path in EVIDENCE_COUNTS),
)
# The counts that are null when the run gives no reading: hitgen's drop counts without its
# "sending done" line (PRF-8), and the in-flight maximum without a /health read.
NULLABLE_COUNT_FIELDS = (
    ("load", "retry_drops"),
    ("load", "generator_drops"),
    ("resources", "max_in_flight"),
)
# The counts in every element of a list, as COUNT_FIELDS: (section, the list's path, the counts):
# each load step's, each step's unexpected errors (§B9), each step's API counts, each step's
# latency sample count, each instance series' reading count, and each capacity step's.
LIST_COUNT_FIELDS: tuple[tuple[str, str, tuple[str, ...]], ...] = (
    ("load", "steps", (*LOAD_COUNTS, *LOAD_STEP_COUNTS, "sched_lag_ns.count")),
    ("results", "unexpected_errors.steps", UNEXPECTED_COUNTS),
    ("results", "steps", ("requests", "new_rows", "allowed", "limited")),
    ("timing", "steps", ("latency_ns.count",)),
    ("resources", "instance_series", ("readings",)),
    ("capacity", "steps", CAPACITY_STEP_COUNTS),
)
# The counts in every member of an object, as COUNT_FIELDS: each process's CPU and RSS reading
# counts, and each store-state count's reading count; and those that are null without a reading:
# each store-state count's max and last (PRF-4: tracked keys, live markers, quarantined keys).
MEMBER_COUNT_FIELDS: tuple[tuple[str, str, tuple[str, ...]], ...] = (
    ("resources", "processes", ("cpu_readings", "rss_readings")),
    ("resources", "store_state", ("readings",)),
)
NULLABLE_MEMBER_COUNT_FIELDS: tuple[tuple[str, str, tuple[str, ...]], ...] = (
    ("resources", "store_state", ("max", "last")),
)
# The objects of counts, each member a count (depth 1) or an object of counts (depth 2): decisions
# by result, request outcomes no ERR row matched, unresolved refunds by reason, attempt outcomes and
# transport errors by attempt kind, and requests sent to and served by each instance.
COUNT_OBJECTS = (
    ("results", "decisions", 1),
    ("results", "unmatched", 1),
    ("results", "unresolved_reasons", 1),
    ("results", "attempt_outcomes", 2),
    ("results", "transport_errors.by_kind", 2),
    ("traffic", "requests_per_instance", 1),
    ("traffic", "served_per_instance", 1),
)
# The lists of counts, each element a count: the schedule entries per step.
COUNT_LISTS = (("traffic", "planned_per_step"),)
# PRF-4's per-instance series (resources.instance_series) by name, a closed set: the report builder
# writes only hitapi's /health counts, the /metrics series it follows and each labelled series of
# the /metrics families it follows (report/build.py HEALTH_COUNTS, METRIC_SERIES, METRIC_FAMILIES;
# DESIGN §A5.11, §B8). The series whose max and last are counts, null without a reading, as
# COUNT_FIELDS: hitapi's in-flight requests and drop counts, the store-state gauges, pending
# refunds, the listener-drop counter, and each labelled series of the cleanup counters (its name up
# to its labels).
COUNT_SERIES = (
    "in_flight",
    "listener_drops",
    "log_drops",
    "rl_listener_drops_total",
    "rl_live_markers",
    "rl_pending_refunds",
    "rl_quarantined_keys",
    "rl_tracked_buckets",
)
COUNT_SERIES_FAMILIES = (
    "rl_cleanup_entries_total{",
    "rl_cleanup_passes_total{",
    "rl_cleanup_stops_total{",
)
# The series whose max and last are measurements, not counts: lags, durations in seconds (§A5.8: a
# fraction) and in µs (§A5.7), which keep only check_keys' presence check, as no check reads them.
LAG_SERIES = ("rl_cleanup_lag_seconds", "rl_clock_lag_microseconds")
# The fields every element of a list must hold where the list may be null (no element): PRF-5's
# faults with their actual start and end (null for a run with no fault).
NULLABLE_LIST_FIELDS = (("context", "faults", ("cmd", "start_ns", "end_ns")),)
# The fields an object must hold where it may be null: check_bud11's violation (null unless its
# verdict is violation).
NULLABLE_OBJECT_FIELDS = (("correctness", "violation", VIOLATION_FIELDS),)
# The fields every member of an object must hold: (section, the object's path, the fields).
MEMBER_FIELDS = (
    # PRF-4: per process CPU % and RSS mean and max.
    ("resources", "processes", ("cpu_pct_mean", "cpu_pct_max", "rss_bytes_mean", "rss_bytes_max")),
)
# A spike run's capacity.spike fields: its deadline, window and met.
SPIKE_FIELDS = ("deadline_ns", "window_start_ns", "window_end_ns", "met")
# The context.json items a benchmark run records (HARNESS PRF-5; cmd/orchestrator missingPRF5),
# each a non-empty string or object; a benchmark limiter run adds thresholds_sha256.
PRF5_ITEMS = (
    "hardware",
    "os",
    "versions.go",
    "versions.valkey",
    "versions.postgres",
    "store_settings",
    "postgres_settings",
    "conformance",
    "settings",
    "schedule_sha256",
)
# evidence_validity.checks (§B8), each judged again from its recorded value; correctness is
# check_bud11's verdict, not invalid (HAR-13).
EVIDENCE_CHECKS = (
    "log_drops",
    "listener_drops",
    "fixtures_off",
    "outage",
    "outage_segments",
    "gen_summary",
    "rewrite_windows",
    "correctness",
)
# check_bud11's verdicts that leave the evidence valid (plan H5 step 8: exit 0 pass, 1 violation;
# 2 invalid is not one).
EVIDENCE_VERDICTS = ("pass", "violation")
SPIKE_KEYS = (
    "spike_step",
    "post_spike_step",
    "spike_end_ns",
    "deadline_ns",
    "window_start_ns",
    "window_end_ns",
)


@dataclass(frozen=True)
class Failure:
    check: str
    detail: str

    def line(self) -> str:
        return f"FAIL {self.check}: {self.detail}"


class Checks:
    """The failed checks, in the order they were found."""

    def __init__(self) -> None:
        self.failures: list[Failure] = []

    def fail(self, check: str, detail: str) -> None:
        self.failures.append(Failure(check, detail))

    def expect(self, check: str, ok: bool, detail: str) -> None:
        if not ok:
            self.fail(check, detail)


class _Missing:
    """No value at a path."""

    def __repr__(self) -> str:
        return "missing"


MISSING = _Missing()


def at(doc: object, path: str) -> object:
    """The value at a dotted path of nested objects, MISSING when a part is absent."""
    value = doc
    for part in path.split("."):
        if not isinstance(value, dict) or part not in value:
            return MISSING
        value = value[part]
    return value


def show(value: object) -> str:
    return "missing" if value is MISSING else json.dumps(value)


def _no_repeat(pairs: list[tuple[str, object]]) -> dict[str, object]:
    out = dict(pairs)
    if len(out) != len(pairs):
        raise ValueError("a repeated key")
    return out


def _no_constant(name: str) -> object:
    raise ValueError(f"{name} is not a JSON number")


def _finite(text: str) -> float:
    value = float(text)
    if not math.isfinite(value):
        raise ValueError(f"{text} is not a finite number")
    return value


def read_doc(path: Path) -> dict[str, object]:
    """A whole JSON document holding one object, read strictly (UTF-8, no repeated key, no NaN,
    Infinity or number beyond a float's range). Raises OSError or ValueError."""
    with path.open("rb") as stream:
        data = stream.read(MAX_DOC + 1)
    if len(data) > MAX_DOC:
        raise ValueError(f"more than {MAX_DOC} bytes")
    try:
        value = json.loads(
            data.decode("utf-8"),
            object_pairs_hook=_no_repeat,
            parse_constant=_no_constant,
            parse_float=_finite,
        )
    except RecursionError as err:
        raise ValueError("nested too deeply") from err
    if not isinstance(value, dict):
        raise ValueError("not a JSON object")
    return value


def load(path: Path, check: str, checks: Checks) -> dict[str, object] | None:
    try:
        return read_doc(path)
    except (OSError, ValueError) as err:
        checks.fail(check, f"{path}: {err}")
        return None


def file_sha256(path: Path) -> str | None:
    """The hex SHA-256 of a file's bytes, None when it cannot be read."""
    digest = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            for chunk in iter(lambda: stream.read(1 << 20), b""):
                digest.update(chunk)
    except OSError:
        return None
    return digest.hexdigest()


def as_int(value: object) -> int | None:
    """value when it is an integer from 0 to 2^63 − 1 (never a bool), else None."""
    return value if type(value) is int and 0 <= value <= MAX_INT else None


def is_int(value: object) -> bool:
    return as_int(value) is not None


def as_number(value: object) -> int | float | None:
    """value when it is an integer from 0 to 2^63 − 1 or a finite float of at least 0 (never a
    bool), else None."""
    if type(value) is float:
        return value if math.isfinite(value) and value >= 0 else None
    return as_int(value)


def zero(value: object) -> bool:
    return type(value) is int and value == 0


def filled(value: object) -> bool:
    """Whether a context.json item holds something: a non-empty string or object."""
    return isinstance(value, str | dict) and len(value) > 0


# The run itself: whose report, its outcome, its PRF-5 items.


def check_run(
    report: dict[str, object],
    context: dict[str, object],
    bench: bool,
    limiter: bool,
    checks: Checks,
) -> None:
    """report.json belongs to the run in context.json (a report copied from another run fails),
    the orchestrator noted no failure (outcome ok), and a benchmark run recorded every PRF-5 item
    (as missingPRF5 requires: a benchmark run that lacks one fails at its end)."""
    mine, theirs = report.get("run_id", MISSING), context.get("run_id", MISSING)
    checks.expect(
        "run_id",
        isinstance(theirs, str) and mine == theirs,
        f"report.json {show(mine)}, context.json {show(theirs)}",
    )
    outcome = context.get("outcome", MISSING)
    checks.expect("outcome", outcome == "ok", f"context.json outcome {show(outcome)}: want ok")
    if bench:
        items = (*PRF5_ITEMS, "thresholds_sha256") if limiter else PRF5_ITEMS
        missing = [name for name in items if not filled(at(context, name))]
        checks.expect(
            "context_prf5", not missing, f"context.json lacks {', '.join(missing)} (PRF-5)"
        )


@dataclass(frozen=True)
class Pins:
    """What the run's scenario file says about which checks apply."""

    verdict: str
    forced_rewrites: bool  # forced_aof_rewrites_s is set (non-empty)


def read_pins(run_dir: Path, context: dict[str, object], checks: Checks) -> Pins | None:
    """The scenario file context.json pins (scenario.path, found as the report builder finds it:
    an absolute path as it is, a relative one from the working directory when it is there, else
    from the repository root holding runs/<id>), checked against scenario.sha256 and read with
    tomllib: its verdict (none when it sets none) and whether it sets forced_aof_rewrites_s.
    None, with a failed scenario_file check, when it is not the file the run used."""
    text, pinned = at(context, "scenario.path"), at(context, "scenario.sha256")
    if not isinstance(text, str) or not text or not isinstance(pinned, str):
        detail = f"path {show(text)}, sha256 {show(pinned)}: want both"
        checks.fail("scenario_file", f"context.json scenario: {detail}")
        return None
    path = Path(text)
    if not path.is_absolute() and not path.exists():
        path = run_dir.resolve().parent.parent.joinpath(path)
    try:
        with path.open("rb") as stream:
            data = stream.read(MAX_DOC + 1)
    except OSError as err:
        checks.fail("scenario_file", f"{path}: {err}")
        return None
    if len(data) > MAX_DOC or hashlib.sha256(data).hexdigest() != pinned:
        checks.fail("scenario_file", f"{path}: not the file the run used (SHA-256 {pinned})")
        return None
    try:
        raw = tomllib.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, tomllib.TOMLDecodeError, RecursionError) as err:
        checks.fail("scenario_file", f"{path}: not TOML: {err}")
        return None
    verdict = raw.get("verdict", "none")
    forced = raw.get("forced_aof_rewrites_s", [])
    if verdict not in VERDICTS or not isinstance(forced, list):
        detail = f"verdict {verdict!r}, forced_aof_rewrites_s {forced!r}"
        checks.fail("scenario_file", f"{path}: {detail}: want capacity, spike or none, and a list")
        return None
    return Pins(str(verdict), len(forced) > 0)


# The §B8 evidence rules on each check's recorded value (report/build.py evidence_section).


def evidence_rule(name: str, value: object, bench: bool, forced: bool) -> tuple[bool, str]:
    """Whether a check's recorded value meets its §B8 rule, and the rule."""
    if name == "log_drops":
        return zero(value), "0 log drops (every stop and pre_kill line)"
    if name == "listener_drops":
        keys = ("sum", "pre_kill_unread", "segments_without_count")
        ok = isinstance(value, dict) and all(zero(value.get(k)) for k in keys)
        return ok, "0 listener drops, every pre_kill read and every process segment counted"
    if name == "fixtures_off":
        if not bench:
            return True, "applies to benchmark runs only"
        keys = ("fixtures_on", "clock_not_store", "faults")
        reads = as_int(value.get("health_reads")) if isinstance(value, dict) else None
        ok = (
            isinstance(value, dict)
            and value.get("applies") is True
            and reads is not None
            and reads > 0
            and all(zero(value.get(k)) for k in keys)
        )
        return ok, "a benchmark: fixtures off on the store clock in every /health read, no fault"
    if name == "outage":
        longest = value.get("max_outage_ns") if isinstance(value, dict) else MISSING
        measured = as_int(longest)
        within = longest is None or (measured is not None and measured <= MAX_OUTAGE_NS)
        ok = isinstance(value, dict) and zero(value.get("unmeasured")) and within
        return ok, "every store outage measured and at most 5 s"
    if name == "outage_segments":
        segments = as_int(value)
        ok = segments is not None and 0 <= segments <= MAX_OUTAGE_SEGMENTS
        return ok, "at most 1 store outage segment"
    if name == "gen_summary":
        return zero(value), "the gen.jsonl summary present with log_drops 0"
    if name == "rewrite_windows":
        if not forced:
            return True, "applies when the scenario sets forced_aof_rewrites_s"
        windows = as_int(value.get("windows")) if isinstance(value, dict) else None
        ok = windows is not None and windows >= 1
        return ok, "forced_aof_rewrites_s set: at least 1 AOF rewrite window"
    if name == "correctness":
        verdict = value.get("verdict") if isinstance(value, dict) else MISSING
        return verdict in EVIDENCE_VERDICTS, "check_bud11's verdict pass or violation, not invalid"
    raise ValueError(f"no evidence rule {name}")


def check_evidence(report: dict[str, object], bench: bool, forced: bool, checks: Checks) -> None:
    """Each §B8 check's recorded value against its rule; forced: the scenario file sets
    forced_aof_rewrites_s."""
    evidence = report.get("evidence_validity")
    if not isinstance(evidence, dict):
        checks.fail("evidence_validity", f"{show(evidence)}: want an object")
        return
    items = evidence.get("checks")
    if not isinstance(items, dict):
        checks.fail("evidence_validity", "checks: want an object")
        return
    for name in EVIDENCE_CHECKS:
        item = items.get(name)
        check = f"evidence_validity.{name}"
        if not isinstance(item, dict):
            checks.fail(check, "missing")
            continue
        value = item.get("value", MISSING)
        ok, rule = evidence_rule(name, value, bench, forced)
        state = f"ok {show(item.get('ok', MISSING))}, value {show(value)}"
        checks.expect(check, item.get("ok") is True and ok, f"{state}: want {rule}")
    check_correctness(report, items.get("correctness"), checks)
    valid = evidence.get("valid", MISSING)
    checks.expect("evidence_validity.valid", valid is True, f"{show(valid)}: want true")


def check_correctness(report: dict[str, object], item: object, checks: Checks) -> None:
    """report.json's correctness section (check_bud11's verdict JSON), when it has one, is the
    verdict evidence_validity.correctness recorded, and not invalid (HAR-13), naming its
    invalid_reasons: the report never says valid while its BUD-11a verdict is invalid."""
    section = report.get("correctness", MISSING)
    if not isinstance(section, dict):
        return  # baseline mode needs none; full mode's required_keys names a missing one
    verdict, reasons = section.get("verdict", MISSING), section.get("invalid_reasons", MISSING)
    checks.expect(
        "evidence_validity.correctness",
        verdict in EVIDENCE_VERDICTS,
        f"correctness.verdict {show(verdict)} (invalid_reasons {show(reasons)}): want pass or "
        "violation (HAR-13: an invalid verdict means incomplete refund evidence; rerun)",
    )
    recorded = item.get("value") if isinstance(item, dict) else MISSING
    checks.expect(
        "evidence_validity.correctness",
        recorded == {"verdict": verdict, "invalid_reasons": reasons},
        f"recorded {show(recorded)}, report.json correctness verdict {show(verdict)} and "
        f"invalid_reasons {show(reasons)}: want the same",
    )


def check_lag_flags(report: dict[str, object], checks: Checks) -> None:
    """No step whose recorded p99 scheduling lag is above 5 ms says it is not generator-limited
    (§B9, PRF-8): the lag that flags a step is judged over every planned first attempt, so it is
    never below the p99 of the written ones that sched_lag_ns records."""
    steps = at(report, "load.steps")
    for k, step in enumerate(steps if isinstance(steps, list) else []):
        lag = at(step, "sched_lag_ns.p99")
        if lag is None:
            continue
        p99 = as_int(lag)
        flag = at(step, "generator_limited")
        if p99 is None:
            detail = f"{show(lag)}: want null or an integer from 0 to 2^63 − 1"
            checks.fail("generator_limited_lag", f"load.steps[{k}].sched_lag_ns.p99: {detail}")
        elif p99 > ALLOWED_LAG_NS and flag is not True:
            detail = f"sched_lag_ns.p99 {p99} above 5 ms, generator_limited {show(flag)}"
            checks.fail("generator_limited_lag", f"load.steps[{k}]: {detail} (§B9)")


# Required keys (T-RPT-1).


def check_count(shown: str, count: object, nullable: bool, checks: Checks) -> None:
    """A count present at shown is an integer from 0 to 2^63 − 1 (as_int: never a bool or a
    float), or null where nullable; else it fails required_keys."""
    if count is MISSING or as_int(count) is not None or (nullable and count is None):
        return
    want = "null or an integer" if nullable else "an integer"
    checks.fail("required_keys", f"{shown}: {show(count)}: want {want} from 0 to 2^63 − 1")


def count_members(node: object, shown: str, depth: int, checks: Checks) -> list[tuple[str, object]]:
    """The counts an object of counts holds (depth 1), or an object of such objects (depth 2),
    each with its path as shown; a level that is no object fails required_keys."""
    if not isinstance(node, dict):
        checks.fail("required_keys", f"{shown}: {show(node)}: want an object")
        return []
    out: list[tuple[str, object]] = []
    for member, item in node.items():
        if depth > 1:
            out.extend(count_members(item, f"{shown}.{member}", depth - 1, checks))
        else:
            out.append((f"{shown}.{member}", item))
    return out


def is_count_series(name: object) -> bool:
    """Whether an instance series' max and last are counts: its name is one of COUNT_SERIES or a
    labelled series of COUNT_SERIES_FAMILIES."""
    return isinstance(name, str) and (
        name in COUNT_SERIES or name.startswith(COUNT_SERIES_FAMILIES)
    )


def check_series(items: object, checks: Checks) -> None:
    """Each instance series (resources.instance_series) is one the report builder writes, by its
    name: a count series, whose max and last are each null or a count (check_count), or a lag
    (LAG_SERIES), whose are measurements; any other name fails required_keys, the builder's set of
    names being closed. A missing name, or an element that is no object, fails check_keys'
    LIST_FIELDS check instead."""
    for k, item in enumerate(items if isinstance(items, list) else []):
        shown = f"resources.instance_series[{k}]"
        name = at(item, "series")
        if is_count_series(name):
            for part in ("max", "last"):
                check_count(f"{shown}.{part}", at(item, part), True, checks)
        elif name is not MISSING and name not in LAG_SERIES:
            detail = f"{show(name)}: not a series the report builder writes"
            checks.fail("required_keys", f"{shown}.series: {detail}")


def check_counts(key: str, value: dict[str, object], checks: Checks) -> None:
    """Every count section key's object value holds (COUNT_FIELDS and the other count tables, and
    a count series' max and last: check_series), as check_count judges it; an object or list of
    counts that is none fails required_keys."""
    for section, path in (*COUNT_FIELDS, *NULLABLE_COUNT_FIELDS):
        if section == key:
            nullable = (section, path) in NULLABLE_COUNT_FIELDS
            check_count(f"{key}.{path}", at(value, path), nullable, checks)
    for section, path, names in LIST_COUNT_FIELDS:
        items = at(value, path) if section == key else MISSING
        for k, item in enumerate(items if isinstance(items, list) else []):
            for name in names:
                check_count(f"{key}.{path}[{k}].{name}", at(item, name), False, checks)
    for section, path, names in (*MEMBER_COUNT_FIELDS, *NULLABLE_MEMBER_COUNT_FIELDS):
        members = at(value, path) if section == key else MISSING
        nullable = (section, path, names) in NULLABLE_MEMBER_COUNT_FIELDS
        for member, item in (members if isinstance(members, dict) else {}).items():
            for name in names:
                check_count(f"{key}.{path}.{member}.{name}", at(item, name), nullable, checks)
    for section, path, depth in COUNT_OBJECTS:
        node = at(value, path) if section == key else MISSING
        if node is not MISSING:
            for shown, count in count_members(node, f"{key}.{path}", depth, checks):
                check_count(shown, count, False, checks)
    for section, path in COUNT_LISTS:
        items = at(value, path) if section == key else MISSING
        if items is MISSING:
            continue
        if not isinstance(items, list):
            checks.fail("required_keys", f"{key}.{path}: {show(items)}: want a list")
            continue
        for k, count in enumerate(items):
            check_count(f"{key}.{path}[{k}]", count, False, checks)
    if key == "resources":
        check_series(at(value, "instance_series"), checks)


def check_keys(
    report: dict[str, object],
    objects: list[str],
    present: list[str],
    spike: bool,
    checks: Checks,
) -> None:
    """Each key in present is in report.json; each in objects is an object holding its FIELDS,
    LIST_FIELDS, NULLABLE_LIST_FIELDS, MEMBER_FIELDS and NULLABLE_OBJECT_FIELDS, and a spike
    run's capacity its SPIKE_FIELDS, with each count it holds an integer from 0 to 2^63 − 1
    (check_counts)."""
    for key in present:
        if key not in report:
            checks.fail("required_keys", f"report.json has no {key}")
    for key in objects:
        value = report.get(key, MISSING)
        if value is MISSING:
            continue
        if not isinstance(value, dict):
            checks.fail("required_keys", f"{key}: {show(value)}: want an object")
            continue
        for section, path in FIELDS:
            if section == key and at(value, path) is MISSING:
                checks.fail("required_keys", f"{key}.{path}: missing")
        for section, path, names in (*LIST_FIELDS, *NULLABLE_LIST_FIELDS):
            items = at(value, path)
            nullable = (section, path, names) in NULLABLE_LIST_FIELDS
            if section != key or items is MISSING or (nullable and items is None):
                continue
            if not isinstance(items, list):
                checks.fail("required_keys", f"{key}.{path}: want a list")
                continue
            for k, item in enumerate(items):
                for name in names:
                    if at(item, name) is MISSING:
                        checks.fail("required_keys", f"{key}.{path}[{k}].{name}: missing")
        check_counts(key, value, checks)
        for section, path, names in MEMBER_FIELDS:
            members = at(value, path)
            if section != key or members is MISSING:
                continue
            if not isinstance(members, dict):
                checks.fail("required_keys", f"{key}.{path}: want an object")
                continue
            for member, item in members.items():
                for name in names:
                    if at(item, name) is MISSING:
                        checks.fail("required_keys", f"{key}.{path}.{member}.{name}: missing")
        for section, path, names in NULLABLE_OBJECT_FIELDS:
            item = at(value, path)
            if section != key or item is MISSING or item is None:
                continue
            for name in names:  # a value other than an object holds none of them
                if at(item, name) is MISSING:
                    checks.fail("required_keys", f"{key}.{path}.{name}: missing")
        if key == "capacity" and spike:
            for name in SPIKE_FIELDS:
                if at(value, f"spike.{name}") is MISSING:
                    checks.fail("required_keys", f"capacity.spike.{name}: missing")


# T-RPT-2 (capacity and spike benchmark limiter runs).


@dataclass(frozen=True)
class Limits:
    latency_ns: int
    error_rate: float


@dataclass(frozen=True)
class Frozen:
    """A thresholds file's steps: those with thresholds and those marked generator_limited."""

    limits: dict[int, Limits]
    limited: frozenset[int]

    def baseline_step(self, k: int) -> tuple[bool, bool]:
        """Step k's (baseline step exists, baseline step generator-limited): the baseline that
        set the thresholds gave a step with values one that ran in full and was not
        generator-limited, marked a generator-limited one so, and named no missing one."""
        return (k in self.limits or k in self.limited, k in self.limited)


def read_limits(doc: dict[str, object]) -> tuple[Frozen, str | None]:
    """The steps in a thresholds file (as hitgen reads it: a step marked generator_limited, or not
    named, has no thresholds), or the problem with the file."""
    none = Frozen({}, frozenset())
    steps = doc.get("steps")
    if not isinstance(steps, list) or not steps:
        return none, "steps: want a non-empty list"
    out: dict[int, Limits] = {}
    limited: set[int] = set()
    for i, item in enumerate(steps):
        step = as_int(item.get("step")) if isinstance(item, dict) else None
        if not isinstance(item, dict) or step is None:
            return none, f"steps[{i}]: want an object with an integer step"
        if step in out or step in limited:
            return none, f"steps[{i}].step: {step} is repeated"
        flag = item.get("generator_limited")
        if flag is True:
            limited.add(step)
            continue
        latency = as_int(item.get("latency_threshold_ns"))
        error_rate = as_number(item.get("error_rate_threshold"))
        if flag not in (None, False) or latency is None:
            return none, f"steps[{i}]: want generator_limited true or an integer latency threshold"
        if error_rate is None:
            return none, f"steps[{i}].error_rate_threshold: want a number"
        if latency <= 0 or not 0 <= error_rate <= 1:
            return none, f"steps[{i}]: thresholds out of range"
        out[step] = Limits(latency, float(error_rate))
    return Frozen(out, frozenset(limited)), None


@dataclass(frozen=True)
class StepRow:
    """One step's inputs to its capacity verdict, from report.json."""

    generator_limited: bool
    ran: object
    unsent: int
    unknown: int
    achieved: int | float | None
    p99: int | None
    attempts: int
    unexpected: int


class Reader:
    """Typed reads of report.json values; a wrong type, or a number out of range, is a problem,
    read as a neutral value."""

    def __init__(self) -> None:
        self.problems: list[str] = []

    def get_int(self, doc: object, path: str) -> int:
        value = at(doc, path)
        number = as_int(value)
        if number is not None:
            return number
        self.problems.append(f"{path}: {show(value)}: want an integer from 0 to 2^63 − 1")
        return 0

    def get_opt_int(self, doc: object, path: str) -> int | None:
        value = at(doc, path)
        return None if value is None else self.get_int(doc, path)

    def get_bool(self, doc: object, path: str) -> bool:
        value = at(doc, path)
        if isinstance(value, bool):
            return value
        self.problems.append(f"{path}: {show(value)}: want true or false")
        return False

    def rows(self, doc: object, path: str) -> list[object]:
        value = at(doc, path)
        if isinstance(value, list):
            return value
        self.problems.append(f"{path}: want a list")
        return []


def step_rows(report: dict[str, object], reader: Reader) -> list[StepRow]:
    """Every step's verdict inputs: load.steps, timing.steps and results.unexpected_errors.steps."""
    load_steps = reader.rows(report, "load.steps")
    timing_steps = reader.rows(report, "timing.steps")
    error_steps = reader.rows(report, "results.unexpected_errors.steps")
    if not len(load_steps) == len(timing_steps) == len(error_steps):
        reader.problems.append("load, timing and unexpected-error steps differ in number")
        return []
    rows = []
    for ld, tm, er in zip(load_steps, timing_steps, error_steps, strict=True):
        rows.append(
            StepRow(
                generator_limited=reader.get_bool(ld, "generator_limited"),
                ran=at(ld, "ran"),
                unsent=reader.get_int(ld, "unsent"),
                unknown=reader.get_int(ld, "unknown_acquires")
                + reader.get_int(ld, "unknown_application_outcomes"),
                achieved=as_number(at(ld, "achieved_sent_per_s")),
                p99=reader.get_opt_int(tm, "latency_ns.p99"),
                attempts=reader.get_int(er, "attempts"),
                unexpected=reader.get_int(er, "unexpected"),
            )
        )
    return rows


def expected_verdict(row: StepRow, base: tuple[bool, bool], limits: Limits | None) -> str | None:
    """A step's verdict from its inputs (plan H5 step 4): none for a generator-limited step, one
    that did not run in full, one whose baseline step (base, from the frozen thresholds) is
    missing or generator-limited, or one with no first attempt in its window; else pass iff the
    window p99 and unexpected-error rate are within the step's own thresholds, unsent = 0 and no
    ERR-9 or ERR-16."""
    exists, limited = base
    if row.generator_limited or row.ran != "full" or not exists or limited:
        return None
    if limits is None or row.attempts == 0:
        return None
    within = row.p99 is not None and row.p99 <= limits.latency_ns
    within = within and row.unexpected / row.attempts <= limits.error_rate
    return "pass" if within and row.unsent == 0 and row.unknown == 0 else "fail"


def check_verdicts(report: dict[str, object], frozen: Frozen, checks: Checks) -> None:
    """No verdict on a generator-limited step; each step's verdict and sustainable_capacity (the
    highest achieved sent rate of a passing step, PRF-11) as recomputed from the report and the
    frozen thresholds."""
    reader = Reader()
    rows = step_rows(report, reader)
    verdicts = reader.rows(report, "capacity.steps")
    if not reader.problems and len(verdicts) != len(rows):
        reader.problems.append("capacity.steps: not one per load step")
    if reader.problems:
        checks.fail("capacity_verdicts", "; ".join(reader.problems))
        return
    passing: list[int | float] = []
    for k, (row, reported) in enumerate(zip(rows, verdicts, strict=True)):
        verdict = at(reported, "verdict")
        if row.generator_limited:
            checks.expect(
                "generator_limited_no_verdict",
                verdict is None,
                f"step {k} is generator-limited but has verdict {show(verdict)}",
            )
        want = expected_verdict(row, frozen.baseline_step(k), frozen.limits.get(k))
        checks.expect(
            "capacity_verdicts",
            verdict == want,
            f"step {k}: verdict {show(verdict)}, recomputed {show(want)}",
        )
        achieved = row.achieved
        if want == "pass" and achieved is not None:
            passing.append(achieved)
    best = max(passing) if passing else None
    reported_best = at(report, "capacity.sustainable_capacity")
    same = reported_best is None if best is None else as_number(reported_best) is not None
    checks.expect(
        "sustainable_capacity",
        same and reported_best == best,
        f"{show(reported_best)}, recomputed {show(best)} (PRF-11)",
    )


def check_spike(
    report: dict[str, object],
    thresholds: dict[str, object],
    frozen: Frozen,
    checks: Checks,
) -> None:
    """The spike's deadline and window are the ones recorded in the thresholds file before the
    run (PRF-6), and met is as recomputed from the report's window measures and outstanding count
    under the orchestrator's reading of plan step 4 (module docstring): the report's
    outstanding_cutoff_ns must be the deadline − the post-spike step's latency threshold in the
    frozen file, and met needs the window within that step's thresholds and no outstanding
    initial attempt."""
    spike = at(report, "capacity.spike")
    recorded = thresholds.get("spike")
    if not isinstance(spike, dict):
        checks.fail("spike_deadline", f"capacity.spike: {show(spike)}: want an object")
        return
    same = isinstance(recorded, dict) and all(
        is_int(recorded.get(k)) and recorded.get(k) == spike.get(k) for k in SPIKE_KEYS
    )
    mine = {k: spike.get(k, MISSING) for k in SPIKE_KEYS}
    checks.expect(
        "spike_deadline",
        same,
        f"report {show({k: v for k, v in mine.items() if v is not MISSING})}, "
        f"thresholds.json {show(recorded)}",
    )
    reader = Reader()
    post = reader.get_int(spike, "post_spike_step")
    deadline = reader.get_int(spike, "deadline_ns")
    cutoff = reader.get_int(spike, "outstanding_cutoff_ns")
    p99 = reader.get_opt_int(spike, "window_latency_ns.p99")
    attempts = reader.get_int(spike, "window_attempts")
    unexpected = reader.get_int(spike, "window_unexpected")
    outstanding = reader.get_int(spike, "outstanding")
    met = spike.get("met", MISSING)
    if reader.problems or not isinstance(met, bool):
        checks.fail("spike_met", "; ".join([*reader.problems, f"met: {show(met)}"]))
        return
    step = frozen.limits.get(post)
    want_cutoff = deadline - (step.latency_ns if step is not None else 0)
    checks.expect(
        "spike_met",
        cutoff == want_cutoff,
        f"outstanding_cutoff_ns {cutoff}, recomputed {want_cutoff} (the deadline − the "
        "post-spike step's latency threshold)",
    )
    want = (
        same
        and step is not None
        and p99 is not None
        and p99 <= step.latency_ns
        and attempts > 0
        and unexpected / attempts <= step.error_rate
        and outstanding == 0
    )
    checks.expect("spike_met", met is want, f"met {show(met)}, recomputed {show(want)} (PRF-6)")


def baseline_limits(base: dict[str, object]) -> tuple[Frozen, str | None]:
    """The thresholds a baseline's report.json gives each step, as report/thresholds.py freezes
    them (§B9 step 2): none for a step that did not run in full with planned entries, or has no
    latency population (no window p99, or no first attempt in its window); generator_limited for
    a generator-limited step; else its window p99 + 50 ms and max(0.1 %, 2 × its window
    unexpected-error rate). Or the problem reading them."""
    reader = Reader()
    load_steps = reader.rows(base, "load.steps")
    timing_steps = reader.rows(base, "timing.steps")
    error_steps = reader.rows(base, "results.unexpected_errors.steps")
    if not reader.problems and not len(load_steps) == len(timing_steps) == len(error_steps):
        reader.problems.append("load, timing and unexpected-error steps differ in number")
    limits: dict[int, Limits] = {}
    limited: set[int] = set()
    if not reader.problems:
        for k, (ld, tm, er) in enumerate(zip(load_steps, timing_steps, error_steps, strict=True)):
            planned = reader.get_int(ld, "planned")
            if at(ld, "ran") != "full" or planned == 0:
                continue
            if reader.get_bool(ld, "generator_limited"):
                limited.add(k)
                continue
            p99 = reader.get_opt_int(tm, "latency_ns.p99")
            attempts = reader.get_int(er, "attempts")
            unexpected = reader.get_int(er, "unexpected")
            if p99 is None or attempts == 0:
                continue
            # 2 × the rate, exactly the double of its float, as report/thresholds.py writes it.
            error_rate = min(1.0, max(ERROR_RATE_FLOOR, 2 * (unexpected / attempts)))
            limits[k] = Limits(p99 + LATENCY_ALLOWANCE_NS, error_rate)
    if reader.problems:
        return Frozen({}, frozenset()), "; ".join(reader.problems)
    return Frozen(limits, frozenset(limited)), None


def describe_step(frozen: Frozen, k: int) -> str:
    if k in frozen.limited:
        return "generator_limited"
    limits = frozen.limits.get(k)
    if limits is None:
        return "no thresholds"
    return (
        f"latency_threshold_ns {limits.latency_ns}, "
        f"error_rate_threshold {json.dumps(limits.error_rate)}"
    )


def missing_baseline(base_dir: Path, role: str) -> str:
    """The problem of a baseline run directory that is gone: a frozen thresholds file needs the
    baseline run that froze it while the file is in use (PRF-7)."""
    return (
        f"the baseline run {base_dir.parent.name}/{base_dir.name} {role} is missing: keep it "
        "while its frozen thresholds file is in use, or restore it (the file is never rewritten: "
        "to start over, change the scenario's seed or name, PRF-7)"
    )


def check_frozen_values(
    run_dir: Path,
    thresholds: dict[str, object],
    context: dict[str, object],
    frozen: Frozen,
    checks: Checks,
) -> None:
    """The thresholds file holds the thresholds its baseline gives (PRF-7: never relaxed or
    edited; DESIGN §B9 step 2): the run named by baseline_run_id is a baseline of this run's
    scenario and seed on this run's schedule, and each step recomputed from that run's
    report.json (baseline_limits) has the frozen values. The three hashes agree whatever the
    frozen file held when the run copied it, so this catches a file edited before the run."""
    run_id = thresholds.get("baseline_run_id", MISSING)
    if not isinstance(run_id, str) or not RUN_ID.fullmatch(run_id):
        checks.fail(
            "thresholds_file",
            f"baseline_run_id {show(run_id)}: want the run ID of the baseline that set them",
        )
        return
    base_dir = run_dir.resolve().parent.joinpath(run_id)
    if not base_dir.is_dir():
        checks.fail("thresholds_file", missing_baseline(base_dir, "that froze these thresholds"))
        return
    base_context = load(base_dir.joinpath("context.json"), "thresholds_file", checks)
    base = load(base_dir.joinpath("report.json"), "thresholds_file", checks)
    if base_context is None or base is None:
        return
    flag, name = base_context.get("baseline", MISSING), at(base_context, "scenario.name")
    seed, schedule = base_context.get("seed", MISSING), base_context.get("schedule_sha256", MISSING)
    checks.expect(
        "thresholds_file",
        flag is True
        and name == at(context, "scenario.name")
        and seed == context.get("seed")
        and schedule == context.get("schedule_sha256")
        and base.get("run_id") == run_id,
        f"baseline_run_id {run_id}: baseline {show(flag)}, scenario {show(name)}, seed "
        f"{show(seed)}, schedule_sha256 {show(schedule)}, its report.json's run_id "
        f"{show(base.get('run_id', MISSING))}: want a baseline of this run's scenario, seed and "
        "schedule",
    )
    want, problem = baseline_limits(base)
    if problem is not None:
        checks.fail("thresholds_file", f"{run_id}/report.json: {problem}")
        return
    steps = sorted(set(frozen.limits) | frozen.limited | set(want.limits) | want.limited)
    diffs = [
        f"step {k}: frozen {describe_step(frozen, k)}, recomputed {describe_step(want, k)}"
        for k in steps
        if describe_step(frozen, k) != describe_step(want, k)
    ]
    checks.expect(
        "thresholds_file",
        not diffs,
        f"not the thresholds baseline {run_id} gives (§B9 step 2, PRF-7): " + "; ".join(diffs),
    )


def check_thresholds(
    run_dir: Path,
    report: dict[str, object],
    context: dict[str, object],
    spike: bool,
    checks: Checks,
) -> None:
    """T-RPT-2 for a capacity or spike benchmark limiter run (spike: the scenario's verdict is
    spike)."""
    name, seed = at(context, "scenario.name"), context.get("seed")
    if not isinstance(name, str) or not SAFE_NAME.fullmatch(name) or not is_int(seed):
        checks.fail("context_json", f"scenario.name {show(name)}, seed {show(seed)}")
        return
    copied_path = run_dir.joinpath("thresholds.json")
    frozen_path = run_dir.resolve().parent.joinpath("thresholds", f"{name}-{seed}.json")
    copied, frozen = file_sha256(copied_path), file_sha256(frozen_path)
    recorded = context.get("thresholds_sha256")
    hashes = f"{copied_path} {copied}, context {show(recorded)}, {frozen_path} {frozen}"
    checks.expect("thresholds_hash", copied is not None and copied == recorded == frozen, hashes)
    thresholds = load(copied_path, "thresholds_file", checks)
    if thresholds is None:
        return
    created, start = thresholds.get("created_ns"), context.get("start_ns")
    created_ns, start_ns = as_int(created), as_int(start)
    checks.expect(
        "thresholds_created",
        created_ns is not None and start_ns is not None and created_ns < start_ns,
        f"created_ns {show(created)}, run start_ns {show(start)}: want created_ns < start_ns",
    )
    schedule, own = thresholds.get("schedule_sha256", MISSING), context.get("schedule_sha256")
    same_schedule = isinstance(schedule, str) and schedule == own
    detail = f"thresholds.json schedule_sha256 {show(schedule)}, this run's {show(own)}"
    checks.expect("thresholds_schedule", same_schedule, detail)
    if not same_schedule:
        return  # thresholds from another schedule judge nothing (the builder's capacity.problem)
    limits, problem = read_limits(thresholds)
    if problem is not None:
        checks.fail("thresholds_file", f"{copied_path}: {problem}")
        return
    check_frozen_values(run_dir, thresholds, context, limits, checks)
    check_verdicts(report, limits, checks)
    if spike:
        check_spike(report, thresholds, limits, checks)


def check_baseline(
    run_dir: Path,
    report: dict[str, object],
    context: dict[str, object],
    checks: Checks,
) -> None:
    """A benchmark limiter run compares only with a baseline of its own scenario and seed on the
    same schedule: the run named by baseline.run_id has context.json baseline true, this run's
    scenario name and seed, and this run's schedule_sha256, as the report recorded it."""
    run_id, recorded = at(report, "baseline.run_id"), at(report, "baseline.schedule_sha256")
    mine = context.get("schedule_sha256")
    if not isinstance(run_id, str) or not RUN_ID.fullmatch(run_id):
        checks.fail("baseline_schedule", f"baseline.run_id {show(run_id)}: no baseline run")
        return
    base_dir = run_dir.resolve().parent.joinpath(run_id)
    if not base_dir.is_dir():
        checks.fail("baseline_schedule", missing_baseline(base_dir, "the report compares with"))
        return
    base = load(base_dir.joinpath("context.json"), "baseline_schedule", checks)
    if base is None:
        return
    name, seed = at(context, "scenario.name"), context.get("seed")
    base_name, base_seed, flag = at(base, "scenario.name"), base.get("seed"), base.get("baseline")
    checks.expect(
        "baseline_run",
        flag is True
        and isinstance(name, str)
        and base_name == name
        and is_int(seed)
        and is_int(base_seed)
        and base_seed == seed,
        f"{run_id}: baseline {show(flag)}, scenario {show(base_name)}, seed {show(base_seed)}; "
        f"this run: scenario {show(name)}, seed {show(seed)}",
    )
    theirs = base.get("schedule_sha256", MISSING)
    checks.expect(
        "baseline_schedule",
        isinstance(mine, str) and theirs == mine and recorded == mine,
        f"this run {show(mine)}, baseline {run_id} {show(theirs)}, report {show(recorded)}",
    )


def check(run_dir: Path, mode: str) -> list[Failure]:
    """Every check of the given mode on the run in run_dir; the failed ones, in order."""
    checks = Checks()
    report = load(run_dir.joinpath("report.json"), "report_json", checks)
    context = load(run_dir.joinpath("context.json"), "context_json", checks)
    if report is None or context is None:
        return checks.failures
    run_kind, baseline_run = context.get("run_kind"), context.get("baseline")
    if run_kind not in ("bench", "test") or not isinstance(baseline_run, bool):
        checks.fail("context_json", f"run_kind {show(run_kind)}, baseline {show(baseline_run)}")
        return checks.failures
    bench, limiter = run_kind == "bench", not baseline_run
    check_run(report, context, bench, limiter, checks)
    pins = read_pins(run_dir, context, checks)
    verdict = pins.verdict if pins is not None else None
    bench_limiter = mode == "full" and bench and limiter
    capacity_run = bench_limiter and verdict in ("capacity", "spike")
    present = [*TRPT1_KEYS, "evidence_validity"]
    objects = list(present)
    if mode == "baseline":
        checks.expect("mode", baseline_run, "baseline mode checks a baseline run (limiter off)")
    else:
        present.append("correctness")
        objects.append("correctness")
        if bench:
            present.extend(("baseline", "capacity"))
            if limiter:
                objects.append("baseline")
            if capacity_run:
                objects.append("capacity")
    check_keys(report, objects, present, verdict == "spike", checks)
    check_evidence(report, bench, pins is not None and pins.forced_rewrites, checks)
    check_lag_flags(report, checks)
    if bench_limiter:
        check_baseline(run_dir, report, context, checks)
        if capacity_run:
            check_thresholds(run_dir, report, context, verdict == "spike", checks)
    return checks.failures


def main(argv: list[str], out: TextIO | None = None) -> int:
    stream = out if out is not None else sys.stdout
    parser = argparse.ArgumentParser(
        prog="report/validate.py", description="Check a finished run's report (DESIGN §C5)."
    )
    parser.add_argument("run_dir", type=Path, help="the run directory, runs/<id>")
    parser.add_argument("--mode", choices=MODES, default="full")
    args = parser.parse_args(argv)
    failures = check(args.run_dir, args.mode)
    for failure in failures:
        print(failure.line(), file=stream)
    state = "invalid" if failures else "valid"
    print(f"validate: {args.run_dir}: {state} ({args.mode} mode)", file=stream)
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
