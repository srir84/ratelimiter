"""`python3 -m report runs/<id>`: builds runs/<id>/report.json and report.md from the run's saved
evidence (IMPLEMENTATION_PLAN H5 step 2; HAR-9).

`python3 -m report --mark-invalid runs/<id>`: marks the report of a run that report/validate.py
did not pass invalid, report.json and report.md together (plan H5 step 7; DESIGN §C5 "a failure
marks the report invalid"): it loads report.json, sets evidence_validity.valid to false, records
in evidence_validity.validate the checks validate.py's FAIL lines name and the file holding its
output (runs/<id>/validate.stderr, the orchestrator's), and writes both files again from that
report (write_report), so report.md never says valid when report.json does not.

`python3 -m report --mark-invalid --freeze runs/<id>`: the same for a baseline whose report
validate.py passed but whose threshold freeze failed, or was stopped by a signal (plan H5 step 7:
the run fails): evidence_validity.valid false, and evidence_validity.freeze names the file
holding the freeze's output (runs/<id>/thresholds.stderr, the orchestrator's).

Exit 0 when the report was written or marked (a built report whether or not its evidence is
valid: report/validate.py judges that), 1 when the evidence or report.json is missing or
malformed (the error names the file, the line and the field) or a file cannot be written, 2 for
a wrong command line.
"""

import json
import re
import sys
from functools import partial
from pathlib import Path

from report import InputError
from report import runfiles as rf
from report.build import build, write_report
from report.markdown import render

USAGE = "usage: python3 -m report [--mark-invalid [--freeze]] runs/<id>"
MARK_INVALID = "--mark-invalid"
FREEZE = "--freeze"
# report/validate.py's output in the run directory: the orchestrator saves it there (its
# nameValidate step's <stem>.stderr).
VALIDATE_OUTPUT = "validate.stderr"
# The threshold freeze's output in the run directory (the orchestrator's nameThresholds step).
FREEZE_OUTPUT = "thresholds.stderr"
# validate.py's line for a failed check: "FAIL <check>: <detail>".
FAIL_LINE = re.compile(r"FAIL ([A-Za-z0-9_.]{1,128}): ")


def failed_checks(path: Path) -> list[str] | None:
    """The checks named by the FAIL lines of validate.py's output at path, in order, each once;
    None when there is no such file. Lines are read one at a time, at most rf.MAX_LINE bytes."""
    try:
        stream = path.open("rb")
    except FileNotFoundError:
        return None
    names: list[str] = []
    with stream:
        for raw in iter(partial(stream.readline, rf.MAX_LINE + 1), b""):
            match = FAIL_LINE.match(raw.decode("utf-8", "replace"))
            if match is not None and match.group(1) not in names:
                names.append(match.group(1))
    return names


def mark_invalid(run_dir: Path, freeze: bool = False) -> dict[str, object]:
    """The run's report.json marked invalid, written again with report.md rendered from it:
    evidence_validity.valid false, and evidence_validity.validate {passed: false,
    failed_checks, output} (output null when the run directory holds no validate.stderr), or,
    for a failed freeze, evidence_validity.freeze {passed: false, output} (output null when the
    run directory holds no thresholds.stderr). Raises InputError for a report.json that cannot
    be read or holds no evidence_validity object, OSError when a file cannot be read or
    written."""
    report = rf.read_json(run_dir.joinpath(rf.REPORT_JSON))
    evidence = report.get("evidence_validity")
    if not isinstance(evidence, dict):
        raise InputError(rf.REPORT_JSON, "evidence_validity", "want an object")
    evidence["valid"] = False
    if freeze:
        output = run_dir.joinpath(FREEZE_OUTPUT)
        evidence["freeze"] = {
            "passed": False,
            "output": FREEZE_OUTPUT if output.is_file() else None,
        }
    else:
        checks = failed_checks(run_dir.joinpath(VALIDATE_OUTPUT))
        evidence["validate"] = {
            "passed": False,
            "failed_checks": checks if checks is not None else [],
            "output": VALIDATE_OUTPUT if checks is not None else None,
        }
    write_report(run_dir, report, render(report))
    return report


def main(argv: list[str]) -> int:
    freeze = len(argv) == 3 and argv[:2] == [MARK_INVALID, FREEZE]
    mark = freeze or (len(argv) == 2 and argv[0] == MARK_INVALID)
    if len(argv) != 1 and not mark:
        print(USAGE, file=sys.stderr)
        return 2
    run_dir = Path(argv[-1])
    if not run_dir.is_dir():
        print(f"report: {run_dir}: not a run directory", file=sys.stderr)
        return 2
    try:
        if mark:
            report = mark_invalid(run_dir, freeze)
        else:
            report = build(run_dir)
            write_report(run_dir, report, render(report))
    except InputError as err:
        print(f"report: {run_dir}: {err}", file=sys.stderr)
        return 1
    except OSError as err:
        doing = "marking the report invalid" if mark else "writing the report"
        print(f"report: {doing} in {run_dir}: {err}", file=sys.stderr)
        return 1
    evidence = report.get("evidence_validity")
    valid = evidence.get("valid") if isinstance(evidence, dict) else None
    print(json.dumps({"report": str(run_dir.joinpath(rf.REPORT_JSON)), "valid": valid}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
