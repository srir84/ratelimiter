"""S6 repair premises and hand-computed SPEC boundary witnesses."""

import copy
from pathlib import Path

import pytest

from refmodel.model import Model, Policy
from refmodel.replay import main
from refmodel.tests.test_check_bud11 import run_check
from refmodel.tests.test_replay import acquire, log, refund_log, run


def attempt(code: str | int, t: int = 0, credit: int = 0) -> dict[str, object]:
    return {"code": code, "t_us": t, "credited": credit}


def refund(attempts: list[dict[str, object]], result: str) -> dict[str, object]:
    return dict(refund_log()[6], attempts=attempts, result=result)


def charged(t: int, s: int, epoch: str = "e") -> dict[str, object]:
    return dict(acquire(0, s, t), deduction_id=f"{epoch}:{s}", epoch_id=epoch)


def release(deduction: str = "e:1") -> dict[str, object]:
    return {"type": "release", "instance": "i", "deduction_id": deduction}


def budget_log(t: int = 0) -> list[dict[str, object]]:
    return [*log()[:2], *(charged(t, s) for s in (1, 2, 3)), release()]


def budget_refund(
    attempts: list[dict[str, object]], result: str, expiry: int = 10
) -> dict[str, object]:
    return dict(refund(attempts, result), deduction_id="e:1", expiry_us=expiry)


def test_a_policy_epoch_ownership(tmp_path: Path) -> None:
    path = tmp_path / "log"
    records = [
        *log()[:2],
        acquire(1000, 1),
        log()[0],
        acquire(0, 2),
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
    ]
    assert run(path, records) == 0
    assert run_check(path, records) == 0
    records = [
        *log()[:2],
        acquire(1000, 1),
        log()[0],
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
        acquire(0, 2),
        charged(0, 1, "f"),
    ]
    assert run_check(path, records) == 0
    records[-1]["epoch_id"] = "missing"
    assert run_check(path, records) == 2
    assert run(path, [*log()[:2], dict(acquire(1000, 1), epoch_id="missing")]) == 2
    assert run_check(path, [log()[0], acquire(1000, 1), log()[1]]) == 2


@pytest.mark.parametrize(
    "credit,t,expiry,expected",
    [
        (100, 0, 10, 2),
        (0, 0, 10, 0),
        (9, 9, 10, 1),
        (10, 10, 10, 2),
        (1, 0, 10, 2),
        (0, 1, 10, 0),
    ],
)
def test_b1_known_time(tmp_path: Path, credit: int, t: int, expiry: int, expected: int) -> None:
    records = [*budget_log(), budget_refund([attempt(4, t, credit)], "already_applied", expiry)]
    assert run_check(tmp_path / "log", records) == expected
    records[2]["decision_ns"] = 1000
    records[-1] = budget_refund([attempt(4, 1, 0)], "already_applied", expiry)
    assert run_check(tmp_path / "log", records) == 2


def test_b2_b3_retry_and_local_invalid(tmp_path: Path) -> None:
    path = tmp_path / "log"
    records = refund_log()[:6]
    assert (
        run(
            path,
            [
                *records,
                {"type": "clock", "value_ns": (2**53 - 2**43) * 1000},
                refund([attempt(21), attempt(21)], "unresolved"),
            ],
        )
        == 0
    )
    assert run(path, [*records, refund([], "receipt_invalid"), acquire(0, 0)]) == 0


@pytest.mark.parametrize(
    "codes,expected",
    [
        (["unavailable", "cancelled"], 1),
        (["not_ready"], 1),
        ([], 1),
        (["outcome_unknown"], 0),
        (["unavailable", "outcome_unknown"], 0),
    ],
)
def test_b4_sent(tmp_path: Path, codes: list[str], expected: int) -> None:
    assert (
        run_check(
            tmp_path / "log",
            [*budget_log(), budget_refund([attempt(c) for c in codes], "unresolved")],
        )
        == expected
    )


def test_b5_capacity_hwm(tmp_path: Path) -> None:
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": 1000},
        refund([attempt(24, 1)], "unresolved"),
        {"type": "clock", "value_ns": 0},
        acquire(1000, 3, 1000),
    ]
    assert run(tmp_path / "log", records) == 0
    records[7] = refund([attempt(24, 0)], "unresolved")
    assert run(tmp_path / "log", records) == 1


def test_b6_original_credit(tmp_path: Path) -> None:
    records = [
        *refund_log()[:6],
        refund([attempt(25), attempt(4)], "already_applied"),
        acquire(0, 3),
    ]
    assert run(tmp_path / "log", records) == 0
    assert run_check(tmp_path / "log", records) == 0
    records[6] = refund([attempt(25), attempt(4, 0, 1)], "already_applied")
    assert run(tmp_path / "log", records) == 1


@pytest.mark.parametrize(
    "attempts",
    [
        [attempt(3, 0, 1000), attempt(4)],
        [attempt(10), attempt(25)],
        [attempt(3, 0, 1000), attempt(24)],
    ],
)
def test_b7_final_last(tmp_path: Path, attempts: list[dict[str, object]]) -> None:
    records = [*refund_log()[:6], refund(attempts, "unresolved")]
    assert run(tmp_path / "log", records) == 2
    assert run_check(tmp_path / "log", records) == 2


@pytest.mark.parametrize(
    "bad",
    [
        attempt(24, -1, -999),
        attempt(24, 0, 1),
        attempt(3, 0, 1001),
        attempt(21, 1),
        attempt("cancelled", 1),
    ],
)
def test_b9_layouts(tmp_path: Path, bad: dict[str, object]) -> None:
    records = [*refund_log()[:6], refund([bad], "applied" if bad["code"] == 3 else "unresolved")]
    assert run(tmp_path / "log", records) == 2
    assert run_check(tmp_path / "log", records) == 2


def test_b10_lines(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    for runner in (run, run_check):
        records = log()[:2]
        records[1]["origin_ns"] = -1000
        assert runner(tmp_path / "log", records) == 2
        assert "line 2" in capsys.readouterr().out
        assert runner(tmp_path / "log", [*refund_log()[:5], refund_log()[6]]) == 2
        assert "line 6" in capsys.readouterr().out


def test_b11_tied_output(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    assert run_check(tmp_path / "log", [*log()[:2], *(charged(0, s) for s in range(1, 6))]) == 1
    output = capsys.readouterr().out
    assert "at least 1000 units" in output or "3000 units" in output


def test_c1_known_credit_cli(tmp_path: Path) -> None:
    # The original zero-time credit rescues the zero-width window of three
    # charges; the retry's reply at 1 us is outside that window.
    original = [
        *log()[:2],
        charged(0, 1),
        release(),
        budget_refund([attempt(25), attempt(4, 1, 0)], "already_applied"),
        charged(0, 2),
        charged(0, 3),
    ]
    assert run_check(tmp_path / "log", original) == 0
    records = budget_log(10000)
    records[0]["receipt_lifetime_ns"] = 30000
    for r in records[2:5]:
        r["expiry_us"] = 40
    assert (
        run_check(
            tmp_path / "log", [*records, budget_refund([attempt(3, 20, 1000)], "applied", 40)]
        )
        == 1
    )
    # A credit at zero cannot rescue the separate, three-allow window at 10 us.
    records = [
        *log()[:2],
        charged(0, 1),
        release(),
        budget_refund([attempt(25), attempt(4, 1, 0)], "already_applied"),
        *(charged(9000, s) for s in (2, 3, 4)),
    ]
    assert run_check(tmp_path / "log", records) == 1


def test_c2_receipt_error(tmp_path: Path) -> None:
    records = [
        *log()[:2],
        charged(0, 1),
        charged(0, 2),
        dict(acquire(0, 3), result="receipt_error", deduction_id="", epoch_id=""),
    ]
    assert run_check(tmp_path / "log", records) == 1


def test_c3_policies(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    records: list[dict[str, object]] = []
    for epoch, cap in (("e", 3), ("f", 2), ("g", 3)):
        records.extend(
            [
                dict(log()[0], capacity=cap),
                {"type": "epoch", "epoch_id": epoch, "origin_ns": 0},
                *(charged(0, s, epoch) for s in (1, 2, 3)),
            ]
        )
    assert run_check(tmp_path / "log", records) == 1
    assert "epoch 'f'" in capsys.readouterr().out


@pytest.mark.parametrize("d,t,expected", [(0, 10000, 1), (0, 9000, 0), (10000, 9000, 1)])
def test_c4_half_open(tmp_path: Path, d: int, t: int, expected: int) -> None:
    assert (
        run_check(
            tmp_path / "log",
            [*log()[:2], charged(d, 1), release(), *(charged(t, s) for s in (2, 3, 4))],
        )
        == expected
    )


def test_c5_done_and_malformed(tmp_path: Path) -> None:
    path = tmp_path / "log"
    records: list[dict[str, object]] = [
        *budget_log()[:-1],
        {"type": "done", "deduction_id": "e:1"},
        release(),
    ]
    assert run_check(path, records) == 1
    malformed: list[list[dict[str, object]]] = [
        [*refund_log()[:5], refund_log()[6]],
        [*refund_log(), refund_log()[6]],
        [*log()[:2], {"type": "done", "deduction_id": "unknown"}],
        [*refund_log()[:6], dict(refund_log()[6], g=2)],
    ]
    for records in malformed:
        assert run_check(path, records) == 2


def test_c6_refund_update() -> None:
    model = Model(Policy())
    for _ in range(20):
        model.acquire(b"a", 0)
    # 50ms * 10 = 500m units; +1b refund; -1b acquire = 500m.
    assert model.refund(b"a", 1, 1, 60000000, 50000000).credited == 1000000000
    assert model.acquire(b"a", 50000000).u == 500000000


def test_c7_per_key_cap() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.acquire(b"a", 0)
    model.acquire(b"b", 0)
    assert model.refund(b"a", 1, 1, 60000000, 0, max_markers_per_key=1).result == "applied"
    assert model.refund(b"a", 1, 2, 60000000, 0, max_markers_per_key=1).result == "capacity_error"
    assert model.refund(b"b", 2, 1, 60000000, 0, max_markers_per_key=1).result == "applied"
    assert model.refund(b"a", 1, 2, 60000000, 0, max_markers_per_key=2).result == "applied"


def test_c8_clock_regression() -> None:
    model = Model(Policy())
    for _ in range(20):
        model.acquire(b"a", 0)
    model.acquire(b"b", 1000000000)
    assert model.refund(b"a", 1, 1, 60000000, 0).decision_ns == 1000000000
    assert model.refund(b"a", 1, 1, 60000000, 0).decision_ns == 1000000000
    # 1s * 10 = 10b; +1b credit -1b deduction = 10b.
    assert model.acquire(b"a", 1000000000).u == 10000000000


def test_c9_multiple_model_attempts(tmp_path: Path) -> None:
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": (2**53 - 2**43) * 1000},
        refund([attempt(21), attempt(21)], "unresolved"),
    ]
    assert run(tmp_path / "log", records) == 0
    for index in (0, 1):
        changed = copy.deepcopy(records)
        changed[-1] = refund(
            [attempt(21) if i != index else attempt(23) for i in (0, 1)], "unresolved"
        )
        assert run(tmp_path / "log", changed) == 1


def test_c10_old_epoch(tmp_path: Path) -> None:
    records = [
        *refund_log()[:6],
        log()[0],
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
        dict(acquire(1000, 1), epoch_id="f", deduction_id="fresh"),
    ]
    assert run(tmp_path / "log", [*records, refund([attempt(10)], "receipt_invalid")]) == 0
    assert run(tmp_path / "log", [*records, refund([attempt(3, 0, 1000)], "applied")]) == 1


@pytest.mark.parametrize("field,value,expected", [("key_b64url", "Yg", "'YQ'"), ("g", 2, "1")])
def test_c11_field_values(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object, expected: str
) -> None:
    for attempts, result in (
        ([attempt(3, 0, 1000)], "applied"),
        ([attempt(10)], "receipt_invalid"),
    ):
        assert (
            run(
                tmp_path / "log",
                [*refund_log()[:6], dict(refund(attempts, result), **{field: value})],
            )
            == 1
        )
        output = capsys.readouterr().out
        assert (
            field in output and f"expected {expected}" in output and f"actual {value!r}" in output
        )


def test_c12_joins(tmp_path: Path) -> None:
    records = refund_log()
    records[5]["instance"] = "wrong"
    assert run(tmp_path / "log", records) == 1
    assert (
        run(
            tmp_path / "log",
            [*refund_log()[:5], {"type": "done", "deduction_id": "YQ:1:1"}, *refund_log()[5:]],
        )
        == 2
    )
    assert run(tmp_path / "log", [*refund_log()[:5], refund_log()[6]]) == 2
    assert run(tmp_path / "log", [*log()[:2], {"type": "done", "deduction_id": "unknown"}]) == 2


def test_c13_bogus_no_epoch(tmp_path: Path) -> None:
    path = tmp_path / "log"
    assert run(path, [{"type": "clock", "value_ns": 0}]) == 0
    assert main(["--mutant", "bogus", str(path)]) == 2


@pytest.mark.parametrize("g,s,epoch", [(2, 1, True), (1, 2, True), (1, 1, False)])
def test_c14_invalid_hwm(g: int, s: int, epoch: bool) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    assert (
        model.refund(b"a", g, s, 60000000, 1000000, epoch_matches=epoch).result == "receipt_invalid"
    )
    assert model.acquire(b"b", 0).decision_ns == 1000000


@pytest.mark.parametrize("t,result", [(9999, "applied"), (10000, "receipt_expired")])
def test_c15_expiry(t: int, result: str) -> None:
    model = Model(Policy(receipt_lifetime_ns=10000000))
    model.acquire(b"a", 0)
    assert model.refund(b"a", 1, 1, 10000, t * 1000).result == result


def test_c16_quarantine() -> None:
    model = Model(Policy())
    assert model.refund(b"absent", 1, 1, 10, 0).result == "state_integrity_error"
    assert model.acquire(b"absent", 0).result == "state_integrity_error"


def test_c17_c18_unsorted_window(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    records = [
        *log()[:2],
        charged(0, 1),
        charged(10000, 2),
        charged(9000, 3),
        charged(10000, 4),
        charged(10000, 5),
    ]
    assert run_check(tmp_path / "log", records) == 1
    output = capsys.readouterr().out
    assert "epoch 'e' key b'a': window [10000, 10000]" in output


def test_b8_refill_mutant_refund() -> None:
    model = Model(Policy())
    model.mutant = "refill_uncapped"
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 1000000000)
    assert model.buckets[b"a"].units > 20000000000


def test_b1_confirmation_shape(tmp_path: Path) -> None:
    for attempts, result in (
        ([attempt(3, 0, 1000)], "already_applied"),
        ([attempt(4)], "applied"),
        ([], "applied"),
        ([attempt(3, 0, 1000)], "unresolved"),
        ([attempt(25), attempt(4)], "unresolved"),
    ):
        assert run_check(tmp_path / "log", [*budget_log(), budget_refund(attempts, result)]) == 2


def test_b6_nonzero_original_time(tmp_path: Path) -> None:
    # At 1 us, refill caps the small bucket at 2000. The hidden refund credits
    # zero, but writes a marker at 1 us; the next acquire leaves 1000 units.
    records = [
        *refund_log()[:6],
        {"type": "clock", "value_ns": 1000},
        refund([attempt(25), attempt(4, 1, 1)], "already_applied"),
        acquire(1000, 3, 1000),
    ]
    assert run(tmp_path / "log", records) == 0
    records[7] = refund([attempt(25), attempt(4, 1, 0)], "already_applied")
    assert run(tmp_path / "log", records) == 1
