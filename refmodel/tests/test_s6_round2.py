"""Round-two validation and replay boundary witnesses."""

import copy
from pathlib import Path

import pytest

from refmodel.model import TBOUND
from refmodel.tests.test_check_bud11 import run_check
from refmodel.tests.test_replay import acquire, log, refund_log, run
from refmodel.tests.test_s6_repair import attempt, refund


@pytest.mark.parametrize("case", ["expiry", "marker", "epoch", "clock"])
def test_capacity_preconditions(tmp_path: Path, case: str) -> None:
    records = refund_log()[:6]
    attempts = [attempt(24)]
    if case == "expiry":
        records.append({"type": "clock", "value_ns": 10000})
        attempts = [attempt(24, 10)]
    elif case == "marker":
        attempts = [attempt(25), attempt(24), attempt(4)]
    elif case == "epoch":
        records.extend([log()[0], {"type": "epoch", "epoch_id": "f", "origin_ns": 0}])
    else:
        records.append({"type": "clock", "value_ns": TBOUND})
        attempts = [attempt(24, TBOUND // 1000)]
    assert (
        run(
            tmp_path / "log",
            [*records, refund(attempts, "already_applied" if case == "marker" else "unresolved")],
        )
        == 1
    )


@pytest.mark.parametrize("hidden", [False, True])
def test_regressed_budget(tmp_path: Path, hidden: bool) -> None:
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": 2000},
        acquire(1000, 3, 2000),
        {"type": "clock", "value_ns": 0},
    ]
    attempts = [attempt(25), attempt(4, 2, 2)] if hidden else [attempt(24, 2)]
    result = "already_applied" if hidden else "unresolved"
    records.extend([refund(attempts, result), acquire(0, 4, 2000)])
    # Hidden refund restores 1000 units before the next deduction.
    if hidden:
        records[-1]["u"] = 1000
    assert run(tmp_path / "log", records) == 0
    bad = copy.deepcopy(records)
    bad[-2] = refund([attempt(25), attempt(4, 2, 0)] if hidden else [attempt(24)], result)
    assert run(tmp_path / "log", bad) == 1


def test_duplicate_refund(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    records = [*refund_log()[:7], refund([attempt(4)], "already_applied")]
    assert run(tmp_path / "log", records) == 2
    assert "line 8" in capsys.readouterr().out


@pytest.mark.parametrize(
    "bad",
    [
        attempt("busy"),
        attempt("degraded"),
        attempt(22, 1),
        attempt(25, 1),
        attempt(3, 0, -1),
        attempt("config_error", 5),
    ],
)
def test_attempt_layout(tmp_path: Path, bad: dict[str, object]) -> None:
    for runner in (run, run_check):
        assert (
            runner(
                tmp_path / "log",
                [*refund_log()[:6], refund([bad], "applied" if bad["code"] == 3 else "unresolved")],
            )
            == 2
        )


@pytest.mark.parametrize(
    "code,result",
    [
        (10, "unresolved"),
        (11, "unresolved"),
        (10, "receipt_expired"),
        (11, "receipt_invalid"),
        (25, "receipt_expired"),
        (25, "receipt_invalid"),
        (3, "already_applied"),
        (4, "applied"),
    ],
)
def test_final_shape(tmp_path: Path, code: int, result: str) -> None:
    for runner in (run, run_check):
        assert runner(tmp_path / "log", [*refund_log()[:6], refund([attempt(code)], result)]) == 2


@pytest.mark.parametrize("result", ["allowed", "receipt_error"])
@pytest.mark.parametrize("delta", [-1, 0, 1])
def test_expiry_identity(tmp_path: Path, result: str, delta: int) -> None:
    record = dict(acquire(1000, 1), expiry_us=10 + delta, result=result)
    if result == "receipt_error":
        record.update(epoch_id="", deduction_id="")
    assert run_check(tmp_path / "log", [*log()[:2], record]) == (0 if delta == 0 else 2)


@pytest.mark.parametrize(
    "t,decision,credit,expected",
    [(9, 0, 1000, 0), (10, 0, 1000, 2), (0, 1000, 1000, 2), (1, 1000, 1000, 0), (0, 0, -1, 2)],
)
def test_credit_boundaries(
    tmp_path: Path, t: int, decision: int, credit: int, expected: int
) -> None:
    records = refund_log()[:6]
    records[3].update(decision_ns=decision, expiry_us=10 + decision // 1000)
    final = dict(refund([attempt(3, t, credit)], "applied"), expiry_us=10 + decision // 1000)
    assert run_check(tmp_path / "log", [*records, final]) == expected


@pytest.mark.parametrize("credit,expected", [(1000, 0), (1001, 2)])
def test_later_applied_range(tmp_path: Path, credit: int, expected: int) -> None:
    for runner in (run, run_check):
        assert (
            runner(
                tmp_path / "log",
                [*refund_log()[:6], refund([attempt(25), attempt(3, 0, credit)], "applied")],
            )
            == expected
        )


@pytest.mark.parametrize("decision,t", [(0, 10), (1000, 0)])
def test_replay_credit_lifetime(tmp_path: Path, decision: int, t: int) -> None:
    records: list[dict[str, object]] = [
        *log()[:2],
        {"type": "clock", "value_ns": decision},
        acquire(1000, 1, decision),
        {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
    ]
    final = dict(refund([attempt(3, t, 1000)], "applied"), expiry_us=10 + decision // 1000)
    assert run(tmp_path / "log", [*records, final]) == 2


@pytest.mark.parametrize("field,delta", [("t_us", -1), ("credited", 1)])
def test_original_refund_validation_boundaries(tmp_path: Path, field: str, delta: int) -> None:
    records = refund_log()
    attempts = records[6]["attempts"]
    assert isinstance(attempts, list)
    attempts[0][field] += delta
    assert run(tmp_path / "log", records) == 2
