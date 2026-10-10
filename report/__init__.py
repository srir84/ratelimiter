"""Run report builder (DESIGN §B8, §B9, §C8; IMPLEMENTATION_PLAN H5).

`python3 -m report runs/<id>` builds runs/<id>/report.json and report.md from the run's saved
evidence only (HAR-9), streaming every *.jsonl file one line at a time; `python3 -m report
--mark-invalid [--freeze] runs/<id>` marks both invalid when report/validate.py did not pass the
report, or a valid baseline's threshold freeze failed.
`python3 -m report.thresholds runs/<id> --out runs/thresholds/<scenario>-<seed>.json` freezes a
valid baseline's thresholds once; `python3 report/validate.py runs/<id> [--mode full|baseline]`
checks a finished run's report (T-RPT-1, T-RPT-2).
"""


class InputError(ValueError):
    """Run evidence that is missing or malformed, naming the record and the field."""

    def __init__(self, record: str, field: str, reason: str) -> None:
        super().__init__(f"{record}: field {field}: {reason}")
        self.record = record
        self.field = field
        self.reason = reason
