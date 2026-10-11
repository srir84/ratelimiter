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

Refund completion, as plan step 4 words it: "list every release covered by REF-5/REF-9 and require
each to be confirmed (applied/already_applied) before its expiry and with final_mono_ns −
release_mono_ns ≤ refund_deadline_ns (30 s) + the run's measured outage (context.json faults
outage_ns)", excluding only a release after P, one killed or cut by a drain timeout within that
bound, and one that REF-9 (d) excuses. The rules it applies:
- SPEC REF-5: "When release() is called within max_release_delay (RCP-8), the refund is confirmed
  (applied or already_applied) within refund_deadline + outage time of the release() call … Always
  before expiry (guaranteed by RCP-8). A release() after max_release_delay is still attempted but
  has no deadline guarantee."
- SPEC REF-9: refunds are guaranteed only "(c) while the instance keeps running or drains for at
  least the default drain_timeout" (CFG-8: refund_deadline + outage_tolerance, 35 s; hitapi
  closes with it), "and (d) while none of these occurs before the refund is confirmed: a new epoch
  (ADM-1, either form), epoch-wide state loss (STA-9, including an integrity hold), a clock_error
  that, together with store unavailability, lasts longer than outage_tolerance, or a quarantine of
  the key (STA-10)"; SPEC API-1d: "For each released permit that has a receipt, the library emits
  one final event: applied, already_applied, receipt_invalid, receipt_expired, or unresolved
  (reason closed, retention, or expired)", unresolved (closed) at the end of a drain (DESIGN
  §A5.6: "Stops … at the end of the drain (unresolved, closed)").
- The expiry, SPEC RCP-1: "Issue time = the acquire's decision_ns; expiry time = issue time +
  receipt_lifetime (budget time)", and RCP-5: a refund is accepted "only while … budget time <
  signed expiry": a credit_decision_ns before decision_ns + receipt_lifetime_ns.
- The fields, DESIGN §B8: "api-N.jsonl decision | Acquire returned allowed or degraded (written
  before DB work) | deduction_id, user_id, decision_ns, result"; "request | completed /hit call |
  … user_id, … arrival_ns, decision_ns, deduction_id, result, … http_status, error, limiter_ns,
  db_ns, total_ns"; "release |
  permit.Release() called | deduction_id, release_mono_ns"; "refund | listener event |
  deduction_id, result, reason, attempts, credit_decision_ns, release_mono_ns, final_mono_ns";
  "lifecycle | start, ready, pre-kill read, stop"; and context.json "faults with actual
  start/end" (outage_ns: the measured outage of a store fault, H4). A request's arrival_ns and a
  fault's start_ns and end_ns are one clock: the machine's wall clock (HARNESS HTTP-5 "Wall-clock
  integer nanoseconds since the UTC epoch", hitapi's time.Now().UnixNano(), DESIGN §B3; the
  orchestrator's time.Now().UnixNano() around each fault, §B5).
So, per process segment of an api-N.jsonl (from a lifecycle start line, or the file's first
line, to the next start line or the end; the run log of one hitapi process), each release line
with a deduction_id (a degraded permit's is "": it has no receipt and no final event, REF-2) joins
its process's allowed decision line before it (decision_ns, user_id), its one request line
(arrival_ns, limiter_ns, db_ns, total_ns; its user_id and decision_ns the decision line's, as
hitapi writes both from one acquire) and its refund line (the final event). hitapi measures
limiter_ns from just before acquire (its P starts there, HTTP-13) and db_ns over the database step,
and calls release() at once after the step that failed (the refusal after acquire, or the
database outcome; DESIGN §B3), so a release came limiter_ns + db_ns after just before its acquire,
as T-HFLT-2 reads it: within P when that is at most [instance] max_release_delay_ns.
A segment with a pre_kill line, and no stop line after it (a process that wrote its stop line
drained and closed: it was not killed), ended in SIGKILL (FLT-6, written just before it): a release
with no final event there was on an instance killed before its final event (a final event before
the pre_kill line is judged as any other), which REF-9 (c) ("while the instance keeps running")
excuses only when the kill came no later than refund_deadline_ns + the run's measured outage after
the release (orchestrator decision D5, as D3 bounds the drain). The pre_kill line carries no time
(DESIGN §B8: its drop counts, and H4's read_ok), so the kill is bounded from above on that
process's monotonic clock (hitapi's: ns since the process started, its release lines' clock) from
what every run records (F1 item R2: the latest release_mono_ns alone, a lower bound, excused the
segment's latest release however long the process ran on):
- the k-th pre_kill line of api-N.jsonl is the k-th context.json fault "faultctl api kill N" (with
  or without --restart) by start_ns, context.json's order breaking a tie (F1 item B8: a scenario
  need not list its faults by time), which must have run without error, and start no earlier than
  the kill matched before it ended (two kills of one instance that overlap are an input error);
  its end_ns came after the SIGKILL (the record brackets the pre-kill read, the pre_kill line, the
  SIGKILL and the restart, plan H4 step 7);
- the next process segment's earliest request arrival_ns, when earlier: that process started
  after the kill (the orchestrator restarts an instance only once the killed one is reaped), so it
  bounds the kill too, and it is no earlier than the kill's start_ns;
- a request line of the killed process joined to its release line maps that wall-clock bound W
  onto the process's clock: time.Now gave the request's arrival_ns and its monotonic instant
  together, and the release came at least limiter_ns + db_ns later, so the request arrived at
  most release_mono_ns − limiter_ns − db_ns into the process. The kill came at most K_hi =
  release_mono_ns − limiter_ns − db_ns + (W − arrival_ns) + the slew allowance into it (SLEW_PPM
  says how the mapping bounds the clocks' drift), the least bound over the segment's joined pairs.
The latest release_mono_ns the segment holds (release lines, a degraded permit's and one written
after the pre_kill line included; the process still ran then) bounds the kill from below
(orchestrator decision W1): K_hi below it means the wall clock stepped. A release whose
K_hi − release_mono_ns is above refund_deadline_ns + the measured outage, or whose killed process
has no joined pair to bound its kill, may have been outlived by its process past its deadline: a
REF-5 miss.
A final event unresolved with reason closed was cut by a drain timeout, which REF-9 (c) excuses
only when the drain ended before the refund's deadline (orchestrator decision D3): when
final_mono_ns − release_mono_ns ≤ refund_deadline_ns + the run's measured outage, as for a release
after Close began; a later one drained for longer than its deadline allowed, so REF-5 still
covered it.
REF-9 (d) (F1 item R1; plan step 4 lists every release "covered by REF-5/REF-9"): context.json
records two of its events, as faultctl tamper commands (plan H4 step 3; DESIGN §B6). Epoch-wide
state loss (STA-9, the integrity hold included): del-hwm and wipe (a missing high-water mark or
epoch record), retype-shared --target epoch (an epoch record HGETALL cannot read), and a
corrupt-record whose value the field's grammar refuses (record_wellformed and load_epoch, DESIGN
§A4.2: a malformed record or hwm; or a hold) lose the epoch's state at once; retype-shared of ix or
q leaves a shared key of the wrong type, whose next read sets the hold (§A3.3); the hold add-bad-ix
causes comes only when a cleanup pass reaches its member (§A3.2), at a time nothing bounds, so it
excuses nothing. And a quarantine of the key (STA-10): a tamper that damages a key so the next
store step on it quarantines it (§A3.3 check_key): a deletion (del-bucket-fields, del-k, del-mi,
del-mi-member, del-ix, del-marker) or a corrupt-field of a field that is neither a bucket field nor
a marker field (m:…), or of a bucket field with a value DESIGN §A4.2's is_uint53 refuses. A release
whose final event is not a confirmation is excused when such a tamper, run without error (end_ns
not 0, no error), ended after its request's arrival_ns (before it, its acquire would have met the
damage), started no later than the final event may have come (F1 item B5: a final event that came
before the loss began was not its effect; it came at most arrival_ns + total_ns, which covers the
release, + final_mono_ns − release_mono_ns, plus the slew allowance, on the wall clock), and its
event came no later than the earliest its refund's REF-5 deadline came on the wall clock (F1 item
B6: arrival_ns + refund_deadline_ns + the measured outage, less the slew allowance over that span,
as the deadline is the library's monotonic time after the release, which came after the arrival).
A tamper that loses the epoch's state is that event: it ended by the bound. Retyping a shared key,
or damaging the release's key (the decision's user_id: hitapi acquires with the user_id as the key,
DESIGN §B3), leaves its event to the next store step that reads the damage, so it excuses only when
the run logs show that event by the bound (F1 judgment RP3): a request line on the release's
user_id answered ERR-11 (HARNESS §3: http_status 503, error limiter_state_integrity_error, the
limiter's result state_integrity_error), in any instance's log, that arrived no earlier than the
tamper's start_ns (two readings of the wall clock, so no drift lies between them; the arrival came
before its acquire's store step, the start before the tamper wrote) and whose acquire was done by
the bound (arrival_ns + total_ns, which hitapi measures from the arrival past the acquire's reply,
plus the slew allowance over it), and strictly before the start_ns of every store-wide tamper that
ran (no error, as losses_of reads a tamper, and end_ns not 0, as judgment RB3 does), other than the
retype the refusal shows (F1 judgments RB3 and RB5). Each tamper is key-scoped or store-wide by what
it reaches, as faultctl's parser takes it (store_wide: harness/fixtures tamperFlags, which faultctl
and the orchestrator both parse with): a key-scoped one names a key with --key and writes only that
key's state (del-bucket-fields, del-k, del-mi, del-mi-member, del-ix, del-marker, corrupt-field);
every other one is store-wide (add-bad-ix, retype-shared, del-hwm, corrupt-record, wipe). A
store-wide tamper may set the integrity hold, after which every acquire is refused ERR-11 before its
key is read (DESIGN §A4.2 load_epoch): a loss of the epoch's state at once; a retyped ix or q, at
its first read; a corrupt-record of a well-formed value below an instance's floor (store_rollback);
add-bad-ix, at a cleanup pass. The library maps the epoch-wide and the per-key reply to one result
kind, and its epoch watch's lost or held state refuses acquires so. So a refusal done once a
store-wide tamper began may show its hold, not the tamper's event, and that hold may come from a
loss the causal rule refuses as the release's excuse; the strict side splits store-wide tampers by
neither kind nor value. A key-scoped tamper voids no refusal: on another key it cannot refuse the
user's acquire, and on the release's own key the quarantine it causes is the very event the refusal
shows. Every store-wide tamper in the run counts, whatever release it may excuse, so one that began
before the key's tamper leaves no refusal to show that tamper's event (the strict side): a scenario
whose key tampers need excusing must let their refusals come before any store-wide tamper. That
refusal shows the release's key quarantined or the epoch's state lost or held, and each lasts
(STA-10; STA-9, until a new epoch), so neither came before the release's own acquire, which met
neither: either is a REF-9 (d) event. For a retyped ix or q only the release's own user_id's refusal
counts: an acquire of a key already quarantined is refused at its step 3 without reading ix (§A4.3),
so another user's refusal may be that user's own quarantine.
Nothing else in the evidence shows the event: a refund attempt that reads the damage is retried
until the library stops (SPEC API-19), with no line before its final event, and DESIGN §A7's REF-5
proof, by which the refund's next attempt reads the damage in time, is the property this check
tests, so completion does not assume it. P is judged first (REF-5 guarantees nothing to a release
after P, whose receipt may expire before its refund reads the key), then an epoch-wide loss, then a
retyped shared key, then a tamper of the key; a confirmed refund, a missing final event and a
release without its request line are never excused so.
Each release whose final event is applied or already_applied with credit_decision_ns <
decision_ns + receipt_lifetime_ns and final_mono_ns − release_mono_ns ≤ refund_deadline_ns + the
run's measured outage is confirmed in time; any other is a failure unless it was on an instance
killed within that bound before its final event, cut by a drain timeout within that bound,
released after P, or excused by REF-9 (d). A release without its request line is a failure only
when its refund was not confirmed in time (P cannot be judged).
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
the join keeps only the decisions in flight and the releases (a killed segment's until the next
segment's earliest arrival is known): no run log is held in memory. Across a run's logs only REF-9
(d)'s evidence is kept: per deferred tamper and user, the time the ERR-11 refusal done earliest of
those that arrived after the tamper began, and were done before every store-wide tamper but that
one began, was done, and the releases only such a refusal may excuse, judged once every log is
read.

Every run-log record is checked before it counts, so a malformed line never switches an exclusion
on (orchestrator decision R1 of the H6 slice review; AGENTS.md §1, §4): its kind is one DESIGN §B8
gives api-N.jsonl (decision, request, release, refund, lifecycle; a lifecycle line's event start,
ready, stop or pre_kill), it has exactly that kind's or event's fields (the golden lines,
harness/runlog/testdata/golden/api.jsonl), and each field completion reads, the start line's
limits and a stop or pre_kill line's drop counts and read_ok included, has its type and range: a
decision's result is allowed or degraded; a final event's result is one of SPEC API-1d's, with a
reason (closed, retention or expired) exactly when it is unresolved; a request line's total_ns is
at least its limiter_ns + db_ns, which hitapi measures one after the other inside it (F1 judgment
RP4; harness/httpapi hit.go), so a smaller one never moves a bound that adds total_ns earlier.
Every context.json fault's cmd is checked as completion reads it too (F1 item B4; fault_command),
so a malformed one never reads as another command or passes unread: a /_test/faults body is one
JSON object, and a faultctl command line (its words after "faultctl ", as strings.Fields splits
them) is what ParseFaultCommand (harness/fixtures, DESIGN §B6) and the orchestrator's preflight
take, in its command, api kill's words and N (at most the run's instances), and a tamper's
operation, flag syntax (Go's flag package), flags (those the operation needs and takes; no
--epoch) and the values completion reads.
Input errors (an unreadable or malformed manifest, report.json, context.json, config.toml or run
log, a malformed fault command, a kill or tamper that ran with start_ns 0 or after its end_ns, or a
run that is not what the manifest says; a second request line for a release, or one whose user_id
or decision_ns is not its decision line's; a request line whose total_ns is below its limiter_ns +
db_ns; a pre_kill line no kill matches, or whose kill failed or did not run, two kills of one
instance that overlap, a restarted process's request before the kill that ended its predecessor
started, or a K_hi below the killed segment's latest release_mono_ns: a clock step) are failures
with a message naming the record (a run-log line by its file and line number) and the field (check
"manifest" for the manifest, "input" for a run, which then takes no further checks). An S6 limiter
run that failed on input after its failure mode (config.toml [policy] failure_mode) was read, in its
other refund settings, its outage evidence or its run logs, still counts in check 4's failure modes,
not in its counts; where such a run, or one that failed before its failure mode was read, leaves a
mode or its ERR-9/ERR-16 undecided, check 4 does not judge it and prints a SKIP line naming the run
and whether it failed before or after its failure mode was read ("SKIP <scenario> <run ID> <check>:
<detail>"; never a failure: the input failure is). Each failure prints as "FAIL <scenario> <run ID>
<check>: <detail>" ("-" where none applies). Exit 0 when every check holds, 1 when any fails or the
index cannot be read (--build-manifest), 2 for a wrong command line.
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

# The clocks' drift. A request's arrival_ns and a fault's start_ns and end_ns are the machine's
# wall clock; hitapi's and the library's durations and mono times are Go's monotonic clock
# (mach_absolute_time on macOS), which nothing slews. The kernel slews the wall clock to discipline
# it, so the two drift apart at most by that slew (F1 item B7): xnu's ntp_update_second
# (bsd/kern/kern_ntptime.c) adds each second time_offset >> (SHIFT_PLL + time_constant), with
# SHIFT_PLL 4, time_constant clamped to [0, MAXTC] and |time_offset| at most MAXPHASE, 500 ms
# (sys/timex.h): up to 31,250 ppm; then time_freq, at most MAXFREQ, 500 ppm (sys/timex.h); and
# adjtime(2)'s slew, 5,000 ppm while more than 1 s remains (500 ppm below). So the wall clock runs
# at 1 + a times the monotonic clock's rate with |a| ≤ 36,750 ppm, and SLEW_PPM (4 %) bounds the
# drift over a span s by slew_ns(s) = ⌈s × SLEW_PPM / 10^6⌉ either way: a monotonic span m lasts at
# least m − slew_ns(m) and at most m + slew_ns(m) of wall-clock time, and a wall-clock span w at
# most w + slew_ns(w) of monotonic time (1 / (1 − 0.03675) < 1.04). The H6 kill runs measured 1.75
# ppm. A sleeping machine stops the monotonic clock while the wall clock runs on, and a clock step
# is no slew: where each bound uses the allowance (kill_bound, ref9d_losses, note_refusal), it
# says which way they move it.
SLEW_PPM = 40_000
PPM = 1_000_000

# HARNESS §3's row whose request lines show a deferred REF-9 (d) event (F1 judgment RP3): "ERR-11 |
# state_integrity_error | 503 | limiter_state_integrity_error"; a request line's result is the
# limiter's result kind (DESIGN §B8; hitapi writes the acquire's), state_integrity_error here.
INTEGRITY_STATUS = 503
INTEGRITY_ERROR = "limiter_state_integrity_error"
INTEGRITY_RESULT = "state_integrity_error"

# context.json's faults (plan H4 step 3: faultctl's command line, as a scenario's cmd gives it).
FAULTCTL = "faultctl"
# A cmd that runs faultctl starts so; any other is a /_test/faults JSON body (harness/config
# FaultctlPrefix and validCmd).
FAULTCTL_PREFIX = "faultctl "
# The white space strings.Fields splits a faultctl cmd's words at (Go's unicode.IsSpace: Unicode's
# White_Space; Python's str.split splits at U+001C to U+001F too).
GO_SPACE = re.compile("[\t\n\v\f\r \x85\xa0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+")
FAULT_COMMANDS = "store, api, tamper or clock"  # faultctl's commands (ParseFaultCommand)
# The flags faultctl's tamper parser defines (any other is an error) and, per operation, those it
# requires, then those it may take; --epoch is open to every operation (harness/fixtures
# tamperFlags), though the orchestrator's preflight refuses it in a scenario's cmd.
TAMPER_FLAG_NAMES = ("key", "member", "field", "value", "target", "epoch")
TAMPER_FLAGS: dict[str, tuple[tuple[str, ...], tuple[str, ...]]] = {
    "del-bucket-fields": (("key",), ("field",)),
    "del-k": (("key",), ()),
    "del-mi": (("key",), ()),
    "del-mi-member": (("key", "member"), ()),
    "del-ix": (("key",), ()),
    "del-marker": (("key", "member"), ()),
    "corrupt-field": (("key", "field", "value"), ()),
    "add-bad-ix": ((), ("member", "value")),
    "retype-shared": (("target",), ()),
    "del-hwm": ((), ()),
    "corrupt-record": ((), ("field", "value")),
    "wipe": ((), ()),
}
SHARED_TARGETS = ("ix", "q", "epoch")  # retype-shared's --target (harness/fixtures SharedKey)
# The tamper operations that delete part of one key's state (DESIGN §B6; the tamper tool fails one
# that finds nothing to delete), each leaving a row of DESIGN §A3.3's check_key that quarantines
# the key at its next store step.
KEY_DELETIONS = ("del-bucket-fields", "del-k", "del-mi", "del-mi-member", "del-ix", "del-marker")
BUCKET_FIELDS = ("u", "t", "g", "s", "x", "mc")  # a bucket hash's fields (DESIGN §A3.2)
# What corrupt-record writes when no --value is given (harness/fixtures: CorruptSchema into
# schema without --field, TamperedValue into the --field otherwise).
CORRUPT_SCHEMA = "corrupt"
TAMPERED_VALUE = "tampered"
TBOUND = 2**53 - 2**43  # DESIGN §A4.2: a high-water mark at or above it is malformed

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
    user_id: str  # its acquire's key
    release_mono_ns: int  # its release line's (hitapi's monotonic clock)
    # From its request line, None without one: limiter_ns + db_ns, arrival_ns (the wall clock),
    # total_ns (from the arrival until the response was written, the release before it), and
    # where the line is.
    elapsed_ns: int | None = None
    arrival_ns: int | None = None
    total_ns: int | None = None
    request_where: str = ""
    final: Final | None = None


@dataclass(frozen=True)
class KillFault:
    """A context.json fault faultctl api kill N [--restart] (FLT-6): its index there, and it."""

    index: int
    fault: rf.Fault


@dataclass(frozen=True)
class FaultCommand:
    """A context.json fault's faultctl command as completion reads it (fault_command): its
    command (store, api, tamper or clock), api kill's instance N, and a tamper's operation and
    flags (a later one winning, as Go's flag package keeps it)."""

    command: str
    instance: int = 0
    op: str = ""
    flags: dict[str, str] = field(default_factory=dict)


@dataclass(frozen=True)
class Loss:
    """A REF-9 (d) event a context.json fault records (a faultctl tamper that ran): the epoch's
    state lost by the tamper itself (key None, not deferred), or damage that the next store step
    reading it turns into one (deferred), which excuses only once a run log shows that event (an
    ERR-11 refusal, settle): a shared key of the wrong type, whose read sets the integrity hold
    (key None), or damage to one key, which quarantines it (that key)."""

    index: int  # context.json faults[index]
    start_ns: int  # the tamper's start (wall clock): the loss began no earlier
    end_ns: int  # the tamper's end (wall clock): the damage was done by then
    key: str | None  # the key it damages; None: the whole epoch
    deferred: bool  # the event comes only at the next store step that reads the damage


@dataclass(frozen=True)
class StoreWide:
    """A store-wide tamper that ran (store_wide_of), which may have set the integrity hold from its
    start on, so an ERR-11 refusal done since may show that hold (F1 judgment RB5)."""

    index: int  # context.json faults[index]
    start_ns: int  # the tamper's start (wall clock): it began writing no earlier


@dataclass(frozen=True)
class Ref9d:
    """The REF-9 (d) losses that may excuse a release's unconfirmed refund (ref9d_losses): one
    that lost the epoch's state itself by its deadline, which does at once (immediate), else the
    deferred ones, which do once an ERR-11 refusal shows their event by that deadline (settle)."""

    immediate: Loss | None
    deferred: tuple[Loss, ...]  # a shared key retyped before damage to its key
    deadline: int  # the earliest the refund's REF-5 deadline came on the wall clock


@dataclass(frozen=True)
class Waiting:
    """A release within P whose final event is not a confirmation, which only a deferred
    REF-9 (d) loss may excuse (judge): judged once every run log is read (settle)."""

    at: int  # its failure's place among the run's failures: how many came before it
    failure: str  # its failure, should no ERR-11 refusal show a loss's event in time
    user_id: str  # its acquire's key
    deadline: int  # the earliest its refund's REF-5 deadline came on the wall clock
    losses: tuple[Loss, ...]  # the deferred losses that may excuse it, in Ref9d's order


@dataclass
class Scan:
    """What one run's logs give: the S6 rows' counts, and the refund joins' outcomes."""

    rows: dict[str, int] = field(default_factory=lambda: dict.fromkeys(APART_ROWS, 0))
    releases: int = 0
    confirmed: int = 0  # confirmed in time
    killed: int = 0  # no final event, on an instance killed before it
    drained: int = 0  # cut by the drain timeout
    after_p: int = 0  # released after P, not confirmed in time
    lost: int = 0  # unconfirmed after an epoch-wide loss or hold before its deadline (REF-9 (d))
    quarantined: int = 0  # unconfirmed after a quarantine of its key before it (REF-9 (d))
    failures: list[str] = field(default_factory=list)
    # REF-9 (d)'s run-log evidence (F1 judgment RP3), across the run's logs: per deferred loss
    # (its context.json index) and user_id, when the ERR-11 refusal done earliest of those on that
    # user that arrived after the loss began, and were done before every store-wide tamper other
    # than that loss began (judgments RB3 and RB5), was done, at the latest, on the wall clock
    # (note_refusal); and the releases waiting on it (settle).
    refusals: dict[tuple[int, str], int] = field(default_factory=dict)
    waiting: list[Waiting] = field(default_factory=list)


@dataclass
class Killed:
    """A process segment that ended in SIGKILL, its releases waiting for the next segment's
    earliest request arrival, which may bound the kill too."""

    where: str  # its pre_kill line
    kill: KillFault
    releases: list[Release]
    last_mono: int  # the latest release_mono_ns it holds: the kill came no earlier
    last_where: str  # the release line holding it


@dataclass(frozen=True)
class KillBound:
    """A killed process's kill time bound from above on its monotonic clock (K_hi; None when no
    request of the process joins one of its releases), and how it was found."""

    k_hi: int | None
    source: str
    pre_kill: str  # its pre_kill line


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
    (measured_outage), read after its failure mode, so a run that fails here still counts in check
    4's failure modes; else InputError."""
    policy = rf.get_obj(run.config.raw, "policy", rf.CONFIG)
    tolerance = rf.get_int(policy, "outage_tolerance_ns", POLICY)
    return Limits(
        p_ns=run.config.p_ns,
        deadline_ns=rf.get_int(policy, "refund_deadline_ns", POLICY),
        lifetime_ns=rf.get_int(policy, "receipt_lifetime_ns", POLICY),
        outage_ns=measured_outage(run.ctx, tolerance),
    )


def cmd_error(where: str, cmd: str, reason: str) -> InputError:
    """The InputError for a context.json fault (at where) whose cmd completion refuses."""
    return InputError(where, "cmd", f"{cmd!r}: {reason}")


def refuse_constant(name: str) -> object:
    """json.loads's parse_constant: NaN, Infinity and -Infinity, which Go's decoder refuses, are
    not JSON (ValueError)."""
    raise ValueError(f"{name} is not JSON")


def is_json_object(text: str) -> bool:
    """Whether text is one JSON object and nothing else, as Go's decoder reads it (harness/config
    validCmd)."""
    try:
        value = json.loads(text, parse_constant=refuse_constant)
    except (ValueError, RecursionError):
        return False
    return isinstance(value, dict)


def go_int(text: str) -> int | None:
    """text as Go's strconv.ParseInt(text, 10, 64) (and Atoi) reads it, an optional sign and
    decimal digits within int64; None for anything else."""
    if re.fullmatch(r"[+-]?[0-9]+", text) is None:
        return None
    digits = text.lstrip("+-").lstrip("0") or "0"
    if len(digits) > 19:
        return None
    value = -int(digits) if text.startswith("-") else int(digits)
    return value if -(2**63) <= value < 2**63 else None


def is_marker_id(text: str) -> bool:
    """Whether text is a marker's g:s as harness/fixtures parseMarkerID takes it: two decimal
    numbers without a sign, each within int64."""
    g, _, s = text.partition(":")  # s is "" without a colon
    return all(
        re.fullmatch(r"[0-9]+", part) is not None and go_int(part) is not None for part in (g, s)
    )


def api_kill(args: list[str], cmd: str, where: str, instances: int) -> int:
    """The instance N of faultctl's api kill <N> [--restart] (args: the words after api), as
    ParseFaultCommand takes it (N as strconv.Atoi reads it) and the orchestrator's preflight (N at
    most the scenario's instances, which are the run's); else InputError."""
    if not 2 <= len(args) <= 3 or args[0] != "kill" or args[2:] not in ([], ["--restart"]):
        raise cmd_error(where, cmd, "want api kill <N> [--restart]")
    n = go_int(args[1])
    if n is None or not 1 <= n <= instances:
        raise cmd_error(
            where, cmd, f"instance {args[1]!r}: want 1 to {instances} (the run's instances)"
        )
    return n


def tamper_values(op: str, flags: dict[str, str], cmd: str, where: str) -> None:
    """A tamper command's flag values that completion reads, checked as harness/fixtures
    TamperCommand.check takes them: retype-shared's --target ix, q or epoch; del-bucket-fields'
    --field a bucket field; corrupt-record's --value only with a --field; corrupt-field's and
    corrupt-record's --field not empty; del-mi-member's and del-marker's --member g:s. It leaves
    to faultctl what completion does not read: --key's encoding (compared only with a release's
    user_id, a valid key), and add-bad-ix's --member and --value (add-bad-ix excuses nothing, and
    voids refusals whatever they are: store_wide). Else InputError."""
    name, target, member = flags.get("field"), flags.get("target", ""), flags.get("member", "")
    if op == "retype-shared" and target not in SHARED_TARGETS:
        raise cmd_error(where, cmd, f"--target {target!r}: want ix, q or epoch")
    if op == "del-bucket-fields" and name is not None and name not in BUCKET_FIELDS:
        raise cmd_error(where, cmd, f"--field {name!r}: want one of {', '.join(BUCKET_FIELDS)}")
    if op == "corrupt-record" and "value" in flags and name is None:
        raise cmd_error(where, cmd, "--value needs --field")
    if op in ("corrupt-field", "corrupt-record") and name == "":
        raise cmd_error(where, cmd, "--field must not be empty")
    if op in ("del-mi-member", "del-marker") and not is_marker_id(member):
        raise cmd_error(where, cmd, f"--member {member!r}: want g:s, two decimal numbers")


def tamper_command(args: list[str], cmd: str, where: str) -> tuple[str, dict[str, str]]:
    """A tamper command's operation and flags (args: the words after tamper), parsed as
    ParseFaultCommand parses them: a known operation, then Go's flag syntax (-name value, --name
    value, -name=value or --name=value of a flag the parser defines, a later one winning; "--" ends
    the flags; no other word), and checked: the flags the operation requires, none it does not
    take, no --epoch (the orchestrator's preflight refuses one: a run tampers in its own fresh
    epoch), and the values completion reads (tamper_values); else InputError."""
    if not args:
        raise cmd_error(where, cmd, "tamper needs an operation")
    op, words = args[0], args[1:]
    rules = TAMPER_FLAGS.get(op)
    if rules is None:
        raise cmd_error(where, cmd, f"unknown tamper operation {op!r}")
    flags: dict[str, str] = {}
    i = 0
    while i < len(words):
        word = words[i]
        i += 1
        if word == "--":  # the end of the flags: a word after it is an argument
            if i < len(words):
                raise cmd_error(where, cmd, f"unexpected argument {words[i]!r}")
            break
        if len(word) < 2 or not word.startswith("-"):
            raise cmd_error(where, cmd, f"unexpected argument {word!r}")
        name = word[2:] if word.startswith("--") else word[1:]
        if not name or name[0] in "-=":
            raise cmd_error(where, cmd, f"bad flag syntax: {word}")
        name, eq, value = name.partition("=")
        if name not in TAMPER_FLAG_NAMES:
            raise cmd_error(where, cmd, f"flag provided but not defined: -{name}")
        if not eq:
            if i == len(words):
                raise cmd_error(where, cmd, f"flag needs an argument: -{name}")
            value = words[i]
            i += 1
        flags[name] = value
    required, optional = rules
    for name in required:
        if name not in flags:
            raise cmd_error(where, cmd, f"tamper {op} needs --{name}")
    for name in flags:
        if name != "epoch" and name not in required and name not in optional:
            raise cmd_error(where, cmd, f"tamper {op} takes no --{name}")
    if "epoch" in flags:
        raise cmd_error(
            where,
            cmd,
            "--epoch: the orchestrator refuses it, as a run tampers in its own fresh epoch",
        )
    tamper_values(op, flags, cmd, where)
    return op, flags


def fault_command(cmd: str, where: str, instances: int) -> FaultCommand | None:
    """A context.json fault's cmd (at where), checked as completion reads it (F1 item B4; AGENTS.md
    §1 Input): None for a /_test/faults JSON body (one JSON object, as a scenario's cmd that does
    not start with "faultctl " must be: harness/config validCmd); else faultctl's command line, its
    words after "faultctl " split as the orchestrator splits them (strings.Fields), as
    ParseFaultCommand and the orchestrator's preflight take them where completion reads them: the
    command, api kill (api_kill) and tamper (tamper_command); a store or clock command's arguments
    are faultctl's alone. Else InputError naming the field cmd."""
    if not cmd.startswith(FAULTCTL_PREFIX):
        if not is_json_object(cmd):
            raise cmd_error(
                where,
                cmd,
                f"want a command starting with {FAULTCTL_PREFIX!r} or one JSON object (a "
                "/_test/faults body)",
            )
        return None
    words = [word for word in GO_SPACE.split(cmd[len(FAULTCTL_PREFIX) :]) if word]
    if not words:
        raise cmd_error(where, cmd, f"no command: want {FAULT_COMMANDS}")
    command, args = words[0], words[1:]
    if command == "api":
        return FaultCommand(command, instance=api_kill(args, cmd, where, instances))
    if command == "tamper":
        op, flags = tamper_command(args, cmd, where)
        return FaultCommand(command, op=op, flags=flags)
    if command in ("store", "clock"):
        return FaultCommand(command)
    raise cmd_error(where, cmd, f"unknown command {command!r}: want {FAULT_COMMANDS}")


def fault_commands(ctx: rf.Context) -> list[FaultCommand | None]:
    """Each context.json fault's command (fault_command), in its order."""
    return [
        fault_command(f.cmd, f"{rf.CONTEXT}: faults[{i}]", ctx.instances)
        for i, f in enumerate(ctx.faults)
    ]


def fault_times(f: rf.Fault, where: str) -> None:
    """A context.json fault's start_ns and end_ns (at where), as completion reads them (F1
    judgment RP2): one that ran (end_ns not 0) has its start (start_ns not 0) no later than its
    end, as the orchestrator reads both from the wall clock around it; else InputError naming the
    field (the record is corrupt, or the wall clock stepped back)."""
    if f.end_ns == 0:
        return
    if f.start_ns == 0:
        raise InputError(where, "start_ns", f"0, though it ran (end_ns {f.end_ns}): want its start")
    if f.start_ns > f.end_ns:
        raise InputError(
            where,
            "end_ns",
            f"{f.end_ns}: before its start_ns {f.start_ns} (the wall clock stepped back, or the "
            "record is corrupt)",
        )


def kills_of(
    ctx: rf.Context, commands: list[FaultCommand | None], instance: int
) -> list[KillFault]:
    """context.json's kills of instance (faultctl api kill N), by start_ns, context.json's order
    breaking a tie (F1 item B8: a scenario need not list its faults by time, and the orchestrator
    runs each at its own time), each one's times checked first (fault_times), as the order and the
    match read them: so kill_bound's W, the kill's end or the restarted process's earliest arrival
    (which kill_bound checks), never precedes the kill's start."""
    kills: list[KillFault] = []
    for i, (f, command) in enumerate(zip(ctx.faults, commands, strict=True)):
        if command is not None and command.command == "api" and command.instance == instance:
            fault_times(f, f"{rf.CONTEXT}: faults[{i}]")
            kills.append(KillFault(i, f))
    return sorted(kills, key=lambda kill: (kill.fault.start_ns, kill.index))


def is_uint53(text: str) -> bool:
    """DESIGN §A4.2's is_uint53: 1–16 decimal digits, no leading zero but "0", below 2^53."""
    return re.fullmatch(r"0|[1-9][0-9]{0,15}", text) is not None and int(text) < 2**53


def record_lost(field_name: str, value: str) -> bool:
    """Whether writing value into the epoch record's field leaves the epoch lost or held (DESIGN
    §A4.2: record_wellformed and load_epoch refuse it, or the hold is set). A field the record
    does not hold changes neither."""
    if field_name in ("id", "fp"):
        return re.fullmatch(r"[0-9a-f]{32}", value) is None
    if field_name == "schema":
        return value != "1"
    if field_name == "clock":
        return value not in ("store", "controlled")
    if field_name in ("gen", "nb", "nm", "origin", "created"):
        return not is_uint53(value)
    if field_name == "hwm":
        return not is_uint53(value) or int(value) >= TBOUND
    if field_name == "hold":
        return value != ""
    return False


def epoch_lost(op: str, flags: dict[str, str]) -> bool:
    """Whether a tamper (checked by tamper_command) loses the epoch's state at once (STA-9):
    del-hwm, wipe, retype-shared of the epoch record, or a corrupt-record that leaves the record
    lost or held (faultctl: without --field it writes CorruptSchema into schema; --field's value
    defaults to TamperedValue)."""
    if op in ("del-hwm", "wipe"):
        return True
    if op == "retype-shared":
        return flags.get("target") == "epoch"
    if op == "corrupt-record":
        if "field" not in flags:
            return record_lost("schema", CORRUPT_SCHEMA)
        return record_lost(flags["field"], flags.get("value", TAMPERED_VALUE))
    return False


def store_wide(op: str) -> bool:
    """Whether a tamper operation (checked by tamper_command) is store-wide (F1 judgment RB5): it
    names no key with --key, as faultctl's parser takes it (TAMPER_FLAGS: harness/fixtures
    tamperFlags, whose ParseFaultCommand cmd/faultctl and the orchestrator's fault runner both
    parse with), and writes the epoch record, a shared key of the epoch's namespace or every key
    (harness/fixtures tamper.go): add-bad-ix (ZADD ix), retype-shared of any --target (DEL, then
    SET of ix, q or rl:epoch), del-hwm (HDEL of rl:epoch's hwm), corrupt-record of any --field and
    --value (HSET rl:epoch) and wipe (FLUSHALL). Every other operation is key-scoped: it requires
    --key and writes only that key's state, its bucket hash k:<enc> or marker index mi:<enc>, or
    its own member of ix (del-bucket-fields, del-k, del-mi, del-mi-member, del-ix, del-marker and
    corrupt-field). A context.json cmd that is not faultctl's is a /_test/faults body
    (harness/config validCmd), which arms hitapi's wrappers and tampers with nothing."""
    return "key" not in TAMPER_FLAGS[op][0]


def shared_retyped(op: str, flags: dict[str, str]) -> bool:
    """Whether a tamper gives a shared key of the epoch's namespace the wrong type (retype-shared
    of ix or q): the next store step that reads it sets the integrity hold (DESIGN §A3.3: "ZSCORE
    ix or HEXISTS q raises an error"), and every refund attempt of an unexpired receipt of the
    epoch reads both (§A4.4: HEXISTS q, then check_key, which ends in the hold or a quarantine of
    its key once ZSCORE ix fails)."""
    return op == "retype-shared" and flags.get("target") in ("ix", "q")


def damaged_key(op: str, flags: dict[str, str]) -> str | None:
    """The key a tamper (checked by tamper_command: each per-key operation has its --key, and
    corrupt-field its --field and --value) damages so its next store step quarantines it (DESIGN
    §A3.3 check_key): a deletion of part of its state, or a corrupt-field of a field the bucket
    does not hold (malformed_bucket) or of a bucket field with a value is_uint53 refuses
    (malformed_bucket); None for any other tamper (a marker field's or a well-formed value's effect
    depends on the state it met)."""
    key = flags.get("key")
    if key is None:
        return None
    if op in KEY_DELETIONS:
        return key
    if op == "corrupt-field" and "field" in flags and "value" in flags:
        name, value = flags["field"], flags["value"]
        if name.startswith("m:"):
            return None
        if name not in BUCKET_FIELDS or not is_uint53(value):
            return key
    return None


def losses_of(ctx: rf.Context, commands: list[FaultCommand | None]) -> list[Loss]:
    """The REF-9 (d) events context.json records (commands: its faults' commands, fault_commands):
    each faultctl tamper without an error, its times checked (fault_times), that loses the epoch's
    state (epoch_lost), retypes a shared key (shared_retyped) or damages a key (damaged_key), in
    its order. One that did not run has end_ns 0, which no request's arrival_ns precedes, so it
    excuses nothing (ref9d_losses)."""
    out: list[Loss] = []
    for i, (f, command) in enumerate(zip(ctx.faults, commands, strict=True)):
        if command is None or command.command != "tamper" or f.error:
            continue
        fault_times(f, f"{rf.CONTEXT}: faults[{i}]")
        op, flags = command.op, command.flags
        if epoch_lost(op, flags):
            out.append(Loss(i, f.start_ns, f.end_ns, None, deferred=False))
        elif shared_retyped(op, flags):
            out.append(Loss(i, f.start_ns, f.end_ns, None, deferred=True))
        else:
            key = damaged_key(op, flags)
            if key is not None:
                out.append(Loss(i, f.start_ns, f.end_ns, key, deferred=True))
    return out


def store_wide_of(ctx: rf.Context, commands: list[FaultCommand | None]) -> list[StoreWide]:
    """Each store-wide tamper (store_wide) context.json records that ran, as losses_of and F1
    judgment RB3 read a tamper (completion takes an errored one as not run wherever it reads one):
    without an error and with end_ns not 0, its times checked (fault_times), in its order (F1
    judgment RB5)."""
    out: list[StoreWide] = []
    for i, (f, command) in enumerate(zip(ctx.faults, commands, strict=True)):
        if command is None or command.command != "tamper" or f.error or f.end_ns == 0:
            continue
        if store_wide(command.op):
            fault_times(f, f"{rf.CONTEXT}: faults[{i}]")
            out.append(StoreWide(i, f.start_ns))
    return out


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


def slew_ns(gap: int) -> int:
    """The slew allowance over a gap between the clocks (SLEW_PPM): ⌈gap × SLEW_PPM / 10^6⌉, 0
    for a gap of 0 or less."""
    return -(-max(gap, 0) * SLEW_PPM // PPM)


def ref9d_losses(rel: Release, final: Final, losses: list[Loss], limits: Limits) -> Ref9d | None:
    """The REF-9 (d) losses that may excuse rel's unconfirmed refund (final, its final event), or
    None when none may: of the losses that ended after its request's arrival (before it, its
    acquire would have met the damage) and began no later than final may have come (F1 item B5: a
    final event that came before the loss began was not its effect; the loss began at the tamper's
    start at the earliest, a deferred one later still), the first that lost the epoch's state
    itself by the refund's REF-5 deadline (immediate), else the deferred ones, a shared key retyped
    before damage to its key, whose event a run log must show by that deadline (F1 judgment RP3:
    an ERR-11 refusal, settle), as nothing else bounds when the next store step read the damage.
    judge uses them only for a release within P, whose receipt RCP-8 keeps unexpired until its
    deadline. Without its request line nothing shows when its deadline or final came.

    Both times are wall-clock bounds, as the faults' are, found from the request's arrival_ns and
    monotonic durations (SLEW_PPM):
    - deadline, the earliest the refund's deadline came (F1 item B6): REF-5 gives it
      refund_deadline_ns + the measured outage of monotonic time after the release, which came
      after the request's arrival, and time.Now read the arrival's wall clock before its monotonic
      one, so at least that span less its slew allowance lies between arrival_ns and the deadline
      on the wall clock. A sleep or a forward clock step only make the deadline later; a backward
      step makes it earlier by the step, unseen.
    - final_by, the latest final came (F1 item B5): the release came at most total_ns after the
      arrival (hitapi measures total_ns from the arrival's monotonic instant once the response is
      written, after release()), and final came final_mono_ns − release_mono_ns after the release
      (its refund line's times on the library's monotonic clock), so at most that span plus its
      slew allowance after arrival_ns on the wall clock. It errs late, by total_ns's time after
      the release and by the allowance beyond the true drift, which excuses more, never less, than
      causality allows. What it cannot see makes it early, failing rather than excusing: the
      instant between time.Now's wall and monotonic reads at the arrival, a sleep, or a forward
      clock step (a backward one makes it later by the step)."""
    if rel.arrival_ns is None or rel.total_ns is None:
        return None
    span = limits.deadline_ns + limits.outage_ns
    deadline = rel.arrival_ns + span - slew_ns(span)
    took = rel.total_ns + final.final_mono_ns - final.release_mono_ns
    final_by = rel.arrival_ns + took + slew_ns(took)
    after = [loss for loss in losses if loss.end_ns > rel.arrival_ns and loss.start_ns <= final_by]
    for loss in after:
        if not loss.deferred and loss.end_ns <= deadline:
            return Ref9d(loss, (), deadline)
    shared = [loss for loss in after if loss.deferred and loss.key is None]
    mine = [loss for loss in after if loss.deferred and loss.key == rel.user_id]
    if not shared and not mine:
        return None
    return Ref9d(None, (*shared, *mine), deadline)


def note_refusal(
    scan: Scan,
    losses: list[Loss],
    wide: list[StoreWide],
    user: str,
    arrival_ns: int,
    total_ns: int,
) -> None:
    """Keeps an ERR-11 refusal, a request line on user answered 503 limiter_state_integrity_error
    with the limiter's result state_integrity_error (HARNESS §3, DESIGN §B8), as evidence for each
    deferred loss that began no later than the request arrived (F1 judgment RP3; start_ns and
    arrival_ns are two readings of the wall clock, so no drift lies between them, and the arrival
    came before its acquire's store step), under user: settle counts it only for a release of
    user's own (ref9d_losses offers a release only a tamper of its own key, or a retyped shared
    key). Its acquire was done, at the latest, arrival_ns + total_ns + the slew allowance over
    total_ns on the wall clock: hitapi measures total_ns from the arrival's monotonic instant until
    the response is written, past the acquire's reply (limiter_ns starts only just before
    acquire), and scan_file has refused a total_ns below limiter_ns + db_ns (F1 judgment RP4). A
    sleep or a forward clock step make that later, failing rather than excusing; it does not see
    the instant between time.Now's wall and monotonic reads at the arrival, nanoseconds, which the
    deadline bound covers many times over by leaving out the release's own limiter_ns + db_ns
    after its arrival (ref9d_losses).
    It counts for a loss only when that bound is strictly before the start_ns of every store-wide
    tamper in wide (store_wide_of: each one that ran) other than that loss itself (F1 judgments RB3
    and RB5), as the tamper began writing after its start_ns: a store-wide tamper may set the
    integrity hold, after which every acquire is refused ERR-11 before its key is read (DESIGN
    §A4.2's load_epoch: a loss of the epoch's state at once, a retyped ix or q at its first read, a
    corrupt-record of a well-formed value below an instance's floor, store_rollback, or add-bad-ix
    at a cleanup pass; acquire.go maps the epoch-wide and the per-key reply to one result kind, and
    the epoch watch's lost or held state refuses an acquire with state_integrity_error, health.go),
    so a refusal done once one began may show that hold, not the deferred loss's event, whatever
    the tamper's kind or value (the strict side). A retyped ix or q is store-wide too, but its own
    hold is the very event its refusal shows, so it does not void its own evidence; a key-scoped
    tamper voids none: on another key it cannot refuse user's acquire, and on user's key the
    quarantine it causes is the event the refusal shows. A store-wide tamper starting at that bound
    voids the refusal, and every one in the run counts, whatever release it may excuse: one the
    causal rule refuses as a release's excuse (it began after the release's final event) voids
    every refusal done once it began, and one that began before a deferred loss leaves that loss no
    refusal to count (the strict side; so a scenario whose key tampers need excusing must let their
    refusals come before any store-wide tamper). Per loss and user only the refusal done earliest of
    those that count is kept, the one settle needs: one that does not count for a loss is never
    kept for it, and none done later counts either."""
    done_by = arrival_ns + total_ns + slew_ns(total_ns)
    begun = {tamper.index for tamper in wide if tamper.start_ns <= done_by}
    for loss in losses:
        # Only a store-wide tamper other than the loss itself voids the refusal (judgment RB5).
        if loss.deferred and loss.start_ns <= arrival_ns and not begun - {loss.index}:
            slot = (loss.index, user)
            kept = scan.refusals.get(slot)
            if kept is None or done_by < kept:
                scan.refusals[slot] = done_by


def settle(scan: Scan) -> None:
    """Judges the releases waiting on a deferred REF-9 (d) loss (judge) once every run log is
    read, so a refusal in a later process or another instance's log counts: each is excused, as
    lost after a shared key retyped or as quarantined after damage to its key (its losses in that
    order), when an ERR-11 refusal on its user_id that arrived no earlier than the loss began was
    done by its deadline, and before every store-wide tamper other than that loss began
    (note_refusal, F1 judgments RB3 and RB5): the refusal shows its key quarantined or the epoch's
    state lost or held by then, not a hold another store-wide tamper may have set, and each lasts
    (STA-10; STA-9, until a new epoch), so neither came before its own acquire, which met neither.
    For a retyped ix or q too only its own user_id's refusal counts: an acquire of a key already
    quarantined is refused at its step 3 without reading ix (DESIGN §A4.3), so another user's
    refusal may be that user's own quarantine. Else its failure goes back in its place among the
    run's failures."""
    for item in reversed(scan.waiting):
        event: Loss | None = None
        for loss in item.losses:
            done_by = scan.refusals.get((loss.index, item.user_id))
            if done_by is not None and done_by <= item.deadline:
                event = loss
                break
        if event is None:
            scan.failures.insert(item.at, item.failure)
        elif event.key is None:
            scan.lost += 1  # the integrity hold of a retyped shared key, shown by its deadline
        else:
            scan.quarantined += 1  # a quarantine of its key, shown by its deadline
    scan.waiting.clear()


def judge(
    rel: Release, kill: KillBound | None, limits: Limits, losses: list[Loss], scan: Scan
) -> None:
    """Counts one release's outcome in scan, or adds its failure: confirmed in time; else
    excluded when on an instance killed before its final event and, by the kill's upper bound,
    within its deadline (kill: None when it was not killed), cut by a drain timeout before its
    deadline, released after P, or, released within P, its final event not a confirmation after a
    REF-9 (d) event (ref9d_losses): at once after a loss of the epoch's state, else, after a
    deferred loss, once every run log is read (settle, which also adds its failure, in its place,
    when no ERR-11 refusal shows the event); else a failure."""
    final = rel.final
    found: Ref9d | None = None  # the REF-9 (d) losses an unconfirmed final event may have met
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
        if kill is None:
            why = "no final event, and its instance was not killed"
        elif kill.k_hi is None:
            why = (
                f"no final event on an instance killed before it ({kill.pre_kill}), and no "
                "request line of that process joins one of its release lines, so nothing "
                "bounds the kill's time: REF-9 (c) does not excuse it (REF-5)"
            )
        else:
            ran = kill.k_hi - rel.release_mono_ns
            bound = limits.deadline_ns + limits.outage_ns
            if ran <= bound:
                scan.killed += 1  # killed before its deadline (REF-9 (c), decision D5)
                return
            why = (
                f"no final event, and its instance was killed up to {ran} ns after its release "
                f"(release_mono_ns {rel.release_mono_ns}; K_hi {kill.k_hi}, the kill's upper "
                f"bound on its process's monotonic clock: {kill.source}), above "
                f"refund_deadline_ns {limits.deadline_ns} + the measured outage "
                f"{limits.outage_ns} ns = {bound}: nothing shows its instance was killed before "
                "its deadline, so REF-9 (c) does not excuse it (REF-5)"
            )
    else:
        if final.result == UNRESOLVED and final.reason == DRAIN_CUT:
            late = past_deadline(final, limits)
            if late is None:
                scan.drained += 1  # the drain ended before its deadline (REF-9 (c), decision D3)
                return
            why = (
                f"{late}: its instance drained past its deadline, so REF-9 (c) still covered it "
                "(REF-5)"
            )
        else:
            reason = f" ({final.reason})" if final.reason else ""
            why = f"final event {final.result}{reason}: not confirmed"
        found = ref9d_losses(rel, final, losses, limits)
    head = f"{rel.where}: deduction {rel.deduction_id}"
    if rel.elapsed_ns is None:
        scan.failures.append(
            f"{head}: {why}; it has no request line, so whether it was released within P "
            f"({limits.p_ns} ns) is unknown"
        )
        return
    if rel.elapsed_ns > limits.p_ns:
        scan.after_p += 1
        return
    failure = (
        f"{head}: released {rel.elapsed_ns} ns after just before its acquire (limiter_ns + "
        f"db_ns, within P {limits.p_ns}): {why}"
    )
    if found is None:
        scan.failures.append(failure)
    elif found.immediate is not None:
        scan.lost += 1  # an epoch-wide state loss or hold before its deadline (REF-9 (d))
    else:
        scan.waiting.append(
            Waiting(len(scan.failures), failure, rel.user_id, found.deadline, found.deferred)
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


def match_kill(
    where: str,
    path: Path,
    instance: int,
    kills: list[KillFault],
    count: int,
    previous: KillFault | None,
) -> KillFault:
    """The kill of the count-th pre_kill line of path (at where): the count-th of context.json's
    kills of instance by start_ns (kills_of), which ran without error and started no earlier than
    previous, the kill of the pre_kill line before it, ended (else the two overlap); else
    InputError naming the line."""
    if count > len(kills):
        raise InputError(
            where,
            "event",
            f"pre_kill: no context.json fault '{FAULTCTL} api kill {instance}' matches it: it is "
            f"pre_kill line {count} of {path.name}, and context.json has {len(kills)}",
        )
    kill = kills[count - 1]
    f = kill.fault
    head = f"pre_kill: its kill, context.json faults[{kill.index}] ({f.cmd}),"
    if f.error:
        raise InputError(where, "event", f"{head} failed: {f.error}")
    if f.end_ns == 0:
        raise InputError(where, "event", f"{head} did not run (end_ns 0)")
    if previous is not None and f.start_ns < previous.fault.end_ns:
        raise InputError(
            where,
            "event",
            f"{head} starts at start_ns {f.start_ns}, before faults[{previous.index}], the kill "
            f"of pre_kill line {count - 1} of {path.name}, ended at end_ns "
            f"{previous.fault.end_ns}: two kills of instance {instance} overlap",
        )
    return kill


def kill_bound(killed: Killed, earliest: tuple[int, str] | None) -> KillBound:
    """The kill's upper bound on the killed process's monotonic clock, K_hi: the kill's end on the
    wall clock (context.json's end_ns, or earliest, the next process segment's earliest request
    arrival and its line, when earlier), mapped through each request of the killed process joined
    to its release, the least bound they give. Each request's arrival_ns and its monotonic instant
    came from one time.Now (the wall clock read first, so the monotonic one is no earlier), and the
    kill came W − arrival_ns of wall-clock time after it: at most that plus its slew allowance
    (SLEW_PPM) of monotonic time, the gap running from the request nearest the kill, tens of ms
    under load (about 2.5 ms of allowance in the H6 kill runs). The bound errs late: a sleep or a
    forward clock step only make it later; a backward step makes it earlier by the step,
    InputError when that puts it below the latest release_mono_ns the killed segment holds (a
    clock step), unseen otherwise. InputError naming the line too when earliest came before the
    kill started."""
    f = killed.kill.fault
    wall, wall_from = f.end_ns, f"context.json faults[{killed.kill.index}] end_ns"
    if earliest is not None:
        arrival, where = earliest
        if arrival < f.start_ns:
            raise InputError(
                where,
                "arrival_ns",
                f"{arrival}: before the kill of the process before it started (context.json "
                f"faults[{killed.kill.index}] start_ns {f.start_ns}, matched to {killed.where}): "
                "a process started after that kill cannot have served it; the wall clock "
                "stepped, or that kill is not this instance's",
            )
        if arrival < wall:
            wall, wall_from = arrival, f"the restarted process's earliest arrival_ns ({where})"
    best: tuple[int, Release, int] | None = None
    for rel in killed.releases:
        if rel.arrival_ns is None or rel.elapsed_ns is None:
            continue
        gap = wall - rel.arrival_ns
        slew = slew_ns(gap)
        bound = rel.release_mono_ns - rel.elapsed_ns + gap + slew
        if best is None or bound < best[0]:
            best = (bound, rel, slew)
    if best is None:
        return KillBound(None, "", killed.where)
    k_hi, rel, slew = best
    source = (
        f"{wall_from} {wall} on the wall clock, mapped through the request at "
        f"{rel.request_where} (arrival_ns {rel.arrival_ns}) released at {rel.where} "
        f"(release_mono_ns {rel.release_mono_ns}, limiter_ns + db_ns {rel.elapsed_ns}), plus "
        f"{slew} ns of slew"
    )
    if k_hi < killed.last_mono:
        raise InputError(
            killed.where,
            "event",
            f"pre_kill: K_hi {k_hi}, the kill's upper bound on its process's monotonic clock "
            f"({source}), is below the latest release_mono_ns {killed.last_mono} its process "
            f"segment holds ({killed.last_where}): the wall clock stepped (a clock step), or "
            "this kill is not its process's",
        )
    return KillBound(k_hi, source, killed.where)


def scan_file(
    path: Path,
    instance: int,
    kills: list[KillFault],
    losses: list[Loss],
    wide: list[StoreWide],
    limits: Limits,
    scan: Scan,
) -> None:
    """One instance's run log, one line at a time (rf.iter_jsonl), each line checked
    (record_of and the fields read below) before it counts. kills are context.json's kills of
    the instance by start_ns (kills_of), matched to its pre_kill lines in turn (match_kill); a
    killed segment's releases are judged once the next segment's earliest request arrival is known
    (kill_bound). Every request line's total_ns is at least its limiter_ns + db_ns, which hitapi
    measures one after the other inside it (F1 judgment RP4: harness/httpapi hit.go reads the
    arrival first, then times acquire, then the database step, and total_ns once the response is
    written, all on the monotonic clock), so a smaller one never moves a bound that adds it
    earlier. Each request line joined to a release is its only one, with its decision line's
    user_id and decision_ns (F1 judgment RP1); each ERR-11 refusal is kept for REF-9 (d)
    (note_refusal, against the run's store-wide tampers in wide), whose releases waiting on one are
    judged once every run log is read (settle, scan_logs)."""
    decisions: dict[str, tuple[int, str]] = {}  # allowed decisions still to be released
    releases: dict[str, Release] = {}  # this segment's releases
    kill: KillFault | None = None  # the kill of this segment's pre_kill line, unless it stopped
    pre_kill = ""  # that pre_kill line
    matched: KillFault | None = None  # the kill of this file's latest pre_kill line
    count = 0  # this file's pre_kill lines so far
    last_mono, last_where = 0, ""  # this segment's latest release_mono_ns (its process's clock)
    earliest: tuple[int, str] | None = None  # this segment's earliest request arrival_ns
    pending: Killed | None = None  # the killed segment before this one, not yet judged

    def end_segment() -> None:
        nonlocal pending
        if pending is not None:
            bound = kill_bound(pending, earliest)
            for rel in pending.releases:
                judge(rel, bound, limits, losses, scan)
            pending = None
        if kill is not None:
            pending = Killed(pre_kill, kill, list(releases.values()), last_mono, last_where)
        else:
            for rel in releases.values():
                judge(rel, None, limits, losses, scan)
        decisions.clear()
        releases.clear()

    for where, rec in rf.iter_jsonl(path):
        kind, event = record_of(rec, where)
        if kind == LIFECYCLE:
            if event == "start":
                end_segment()
                kill, last_mono, last_where, earliest = None, 0, "", None
            elif event == "pre_kill":
                count += 1
                matched = match_kill(where, path, instance, kills, count, matched)
                kill, pre_kill = matched, where
            elif event == "stop":
                kill = None  # it drained and closed: whatever came before, it was not killed
        elif kind == "decision":
            deduction = rf.get_str(rec, "deduction_id", where)
            user = rf.get_str(rec, "user_id", where)
            issued = rf.get_int(rec, "decision_ns", where)
            result = rf.get_str(rec, "result", where)
            if result not in DECISION_RESULTS:
                raise InputError(where, "result", f"{result!r}: want allowed or degraded")
            if deduction and result == "allowed":
                decisions[deduction] = (issued, user)
        elif kind == "release":
            deduction = rf.get_str(rec, "deduction_id", where)
            released = rf.get_int(rec, "release_mono_ns", where)
            if released > last_mono or not last_where:
                last_mono, last_where = released, where
            if not deduction:
                continue  # a degraded permit: no receipt, no final event (REF-2, API-1d)
            if deduction in releases:
                raise InputError(where, "deduction_id", f"{deduction}: a second release line")
            decided = decisions.pop(deduction, None)
            if decided is None:
                raise InputError(
                    where, "deduction_id", f"{deduction}: no allowed decision line before it"
                )
            releases[deduction] = Release(deduction, where, decided[0], decided[1], released)
            scan.releases += 1
        elif kind == "request":
            arrival = rf.get_int(rec, "arrival_ns", where)
            status = rf.get_int(rec, "http_status", where, 100)
            error = rf.get_str(rec, "error", where)
            result = rf.get_str(rec, "result", where)
            deduction = rf.get_str(rec, "deduction_id", where)
            limiter_ns = rf.get_int(rec, "limiter_ns", where)
            db_ns = rf.get_int(rec, "db_ns", where)
            total_ns = rf.get_int(rec, "total_ns", where)
            user = rf.get_str(rec, "user_id", where)
            issued = rf.get_int(rec, "decision_ns", where)
            if total_ns < limiter_ns + db_ns:
                raise InputError(
                    where,
                    "total_ns",
                    f"{total_ns}: below its limiter_ns {limiter_ns} + db_ns {db_ns} = "
                    f"{limiter_ns + db_ns}, which hitapi measures inside it",
                )
            if earliest is None or arrival < earliest[0]:
                earliest = (arrival, where)
            row = apart_row(status, error, result)
            if row is not None:
                scan.rows[row] += 1
            if (status, error, result) == (INTEGRITY_STATUS, INTEGRITY_ERROR, INTEGRITY_RESULT):
                note_refusal(scan, losses, wide, user, arrival, total_ns)
            if not deduction:
                continue
            rel = releases.get(deduction)
            if rel is None:
                decisions.pop(deduction, None)
                continue
            if rel.arrival_ns is not None:
                raise InputError(where, "deduction_id", f"{deduction}: a second request line")
            if user != rel.user_id:
                raise InputError(
                    where, "user_id", f"{user!r}: its decision line has user_id {rel.user_id!r}"
                )
            if issued != rel.decision_ns:
                raise InputError(
                    where,
                    "decision_ns",
                    f"{issued}: its decision line has decision_ns {rel.decision_ns}",
                )
            rel.elapsed_ns = limiter_ns + db_ns
            rel.arrival_ns, rel.total_ns, rel.request_where = arrival, total_ns, where
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
    if pending is not None:  # the file's last segment was killed: nothing ran after it here
        bound = kill_bound(pending, None)
        for rel in pending.releases:
            judge(rel, bound, limits, losses, scan)


def scan_logs(run: Run) -> Scan:
    """A read run's refund limits (run_limits), its fault commands (fault_commands), REF-9 (d)
    events (losses_of) and store-wide tampers (store_wide_of), then each of its run logs
    (scan_file) with context.json's kills of that instance, then the releases that waited on a
    deferred REF-9 (d) loss's evidence (settle)."""
    limits = run_limits(run)
    commands = fault_commands(run.ctx)
    losses = losses_of(run.ctx, commands)
    wide = store_wide_of(run.ctx, commands)
    scan = Scan()
    for instance in range(1, run.ctx.instances + 1):
        path = run.listed.run_dir.joinpath(rf.api_file(instance))
        kills = kills_of(run.ctx, commands, instance)
        scan_file(path, instance, kills, losses, wide, limits, scan)
    settle(scan)
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
            "lost": sum(s.lost for s in scans),
            "quarantined": sum(s.quarantined for s in scans),
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
