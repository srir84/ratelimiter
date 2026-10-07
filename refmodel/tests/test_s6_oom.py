"""OOM refusals remain distinct from script E_CAPACITY replies."""

from pathlib import Path

import pytest

from refmodel import replay
from refmodel.tests.test_check_bud11 import run_check
from refmodel.tests.test_replay import acquire, refund_log, run
from refmodel.tests.test_s6_repair import attempt, budget_log, budget_refund, refund


@pytest.mark.parametrize("t,expected", [(0, 0), (1, 2)])
def test_oom_then_applied(tmp_path: Path, t: int, expected: int) -> None:
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": 1000},
        refund([attempt("capacity_error", t), attempt(3, 1)], "applied"),
        acquire(1000, 3, 1000),
    ]
    for runner in (run, run_check):
        assert runner(tmp_path / "log", records) == expected


def test_oom_preserves_state_and_hwm(tmp_path: Path) -> None:
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": 1000},
        refund([attempt("capacity_error")], "unresolved"),
        {"type": "clock", "value_ns": 0},
        acquire(0, 0),
    ]
    assert run(tmp_path / "log", records) == 0


@pytest.mark.parametrize("t,expected", [(1, 0), (0, 1)])
def test_integer_capacity_keeps_budget_and_hwm(tmp_path: Path, t: int, expected: int) -> None:
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": 1000},
        refund([attempt(24, t)], "unresolved"),
        {"type": "clock", "value_ns": 0},
        acquire(1000, 3, 1000),
    ]
    assert run(tmp_path / "log", records) == expected


@pytest.mark.parametrize(
    "code,t,expected", [("capacity_error", 0, 0), ("capacity_error", 1, 2), ("unavailable", 0, 1)]
)
def test_oom_counts_as_sent(tmp_path: Path, code: str, t: int, expected: int) -> None:
    records = [*budget_log(), budget_refund([attempt(code, t)], "unresolved")]
    assert run_check(tmp_path / "log", records) == expected


def test_oom_integer_branch_mutant(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    original = replay.refund_attempts

    def integer_branch(record: dict[str, object], line: int) -> list[dict[str, object]]:
        attempts = original(record, line)
        for item in attempts:
            if item["raw_code"] == "capacity_error":
                item["raw_code"] = 24
        return attempts

    monkeypatch.setattr(replay, "refund_attempts", integer_branch)
    with pytest.raises(AssertionError):
        test_oom_then_applied(tmp_path, 0, 0)
