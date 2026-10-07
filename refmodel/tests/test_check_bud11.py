import copy
import json
import random
from pathlib import Path
from types import CodeType, FunctionType

import pytest

from refmodel import RecordError
from refmodel import check_bud11 as check_module
from refmodel.check_bud11 import check_window, main
from refmodel.model import TBOUND, Policy
from refmodel.tests.test_replay import acquire, log


def test_bound_and_one_unit_over() -> None:
    p = Policy(rate_tokens=1, rate_period_ns=1000, capacity=2)
    assert check_window([0, 0, 1000], [], [], p) is None
    violation = check_window([0, 0, 999], [], [], p)
    assert violation is not None
    assert (violation.start_ns, violation.end_ns, violation.excess_units) == (0, 999, 1)
    assert check_window([0, 0, 0], [], [], p) is not None


def test_pb16_window_comparison(monkeypatch: pytest.MonkeyPatch) -> None:
    source_path = Path(check_module.__file__ or "")
    source = source_path.read_text()
    mutation = "        if window_units > cap:\n"
    assert source.count(mutation) == 1
    compiled = compile(
        source.replace(mutation, "        if window_units > cap + 1:\n"), str(source_path), "exec"
    )
    window_code = next(
        code
        for code in compiled.co_consts
        if isinstance(code, CodeType) and code.co_name == "check_window"
    )
    p = Policy(rate_tokens=1, rate_period_ns=1000, capacity=2)
    assert check_module.check_window([0, 0, 999], [], [], p) is not None
    monkeypatch.setattr(check_module, "check_window", FunctionType(window_code, vars(check_module)))
    with pytest.raises(AssertionError):
        assert check_module.check_window([0, 0, 999], [], [], p) is not None


def test_credit_and_unknown_boundaries() -> None:
    p = Policy(rate_tokens=1, rate_period_ns=1000, capacity=2)
    allows = [1000, 1000, 1000]
    assert check_window(allows, [1000], [], p) is None
    assert check_window(allows, [(1000, 1000)], [], p) is None
    assert check_window(allows, [(999, 1000)], [], p) is not None
    assert check_window(allows, [(1001, 1000)], [], p) is not None
    assert check_window(allows, [], [(0, 1001)], p) is None
    assert check_window(allows, [], [(0, 1000)], p) is not None
    assert check_window(allows, [], [(1000, 2000)], p) is None
    assert check_window(allows, [], [(1001, 2000)], p) is not None


def test_seeded_pairwise() -> None:
    rng = random.Random(7544068)
    p = Policy(rate_tokens=3, rate_period_ns=7000, capacity=2)
    for _ in range(1000):
        allows = [rng.randrange(12) * 1000 for _ in range(rng.randrange(12))]
        rng.shuffle(allows)
        credits = [(rng.randrange(12) * 1000, rng.randrange(7001)) for _ in range(rng.randrange(5))]
        unknown = [
            (start, start + rng.randrange(1, 12) * 1000)
            for start in (rng.randrange(12) * 1000 for _ in range(rng.randrange(5)))
        ]
        ordered = sorted(allows)
        expected: tuple[int, int] | None = None
        for j, end in enumerate(ordered):
            best = -1
            best_start = 0
            for i in range(j + 1):
                start = ordered[i]
                credit = sum(units for t, units in credits if start <= t <= end)
                possible = sum(1 for a, b in unknown if a <= end and b > start)
                units = (j - i + 1) * 7000 - (end - start) * 3 - credit - possible * 7000
                if units > best:
                    best, best_start = units, start
            if best > 14000:
                expected = best_start, end
                break
        actual = check_window(allows, credits, unknown, p)
        assert (actual is None) == (expected is None)
        if actual is not None:
            assert (actual.start_ns, actual.end_ns) == expected


def run_check(path: Path, records: list[dict[str, object]]) -> int:
    path.write_text("\n".join(json.dumps(r) for r in records) + "\n")
    return main([str(path)])


def test_integrity_error_before_and_after_epoch(tmp_path: Path) -> None:
    neutral = dict(acquire(0, 0), result="state_integrity_error", retry_after_ns=0)
    records = [neutral, *log()[:2], neutral, *log()[2:]]
    assert run_check(tmp_path.joinpath("log"), records) == 0


@pytest.mark.parametrize("field", ["deduction_id", "epoch_id"])
@pytest.mark.parametrize("before_epoch", [True, False])
def test_integrity_error_requires_empty_ids(tmp_path: Path, field: str, before_epoch: bool) -> None:
    neutral = dict(acquire(0, 0), result="state_integrity_error", retry_after_ns=0)
    neutral[field] = "nonempty"
    records = [neutral, *log()] if before_epoch else [*log()[:2], neutral, *log()[2:]]
    assert run_check(tmp_path.joinpath("log"), records) == 2


def test_integrity_error_neither_charges_nor_credits(tmp_path: Path) -> None:
    # Neutral numeric reply fields carry no budget meaning, even when they
    # resemble a deduction. Two real simultaneous allows fit capacity two.
    neutral = dict(acquire(1000, 1), result="state_integrity_error", deduction_id="", epoch_id="")
    records = [*log()[:2], acquire(1000, 1), neutral, acquire(0, 2), neutral]
    path = tmp_path.joinpath("log")
    assert run_check(path, records) == 0
    # A neutral refusal also supplies no credit for a third real deduction.
    records.append(dict(acquire(0, 3), deduction_id="extra"))
    assert run_check(path, records) == 1


def test_concurrent_epochs_and_missing_event(tmp_path: Path) -> None:
    path = tmp_path.joinpath("log")
    records: list[dict[str, object]] = [
        *log()[:2],
        acquire(0, 2),
        acquire(1000, 1),
        dict(acquire(0, 3), deduction_id="extra"),
    ]
    assert run_check(path, records) == 1
    records.append({"type": "release", "instance": "i", "deduction_id": "YQ:1:1"})
    assert run_check(path, records) == 0
    records[-1]["deduction_id"] = ""
    assert run_check(path, records) == 1
    records = [
        *log()[:2],
        acquire(1000, 1),
        acquire(0, 2),
        log()[0],
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
        dict(acquire(1000, 1), deduction_id="fresh", epoch_id="f"),
    ]
    assert run_check(path, records) == 0
    records.append(dict(acquire(0, 0), result="degraded"))
    assert run_check(path, records) == 0
    records.append(dict(acquire(0, 1), result="receipt_error", deduction_id="", epoch_id=""))
    assert run_check(path, records) == 0


def test_refund_classifications(tmp_path: Path) -> None:
    path = tmp_path.joinpath("log")
    records: list[dict[str, object]] = [
        *log()[:2],
        acquire(1000, 1),
        acquire(0, 2),
        dict(acquire(0, 3), deduction_id="extra"),
        {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
    ]
    refund: dict[str, object] = {
        "type": "refund",
        "deduction_id": "YQ:1:1",
        "key_b64url": "YQ",
        "g": 1,
        "s": 1,
        "expiry_us": 10,
        "attempts": [],
        "result": "unresolved",
    }
    assert run_check(path, [*records, refund]) == 1
    refund["attempts"] = [{"code": "outcome_unknown", "t_us": 0, "credited": 0}]
    assert run_check(path, [*records, refund]) == 0
    refund["attempts"] = [{"code": "receipt_expired", "t_us": 10, "credited": 0}]
    refund["result"] = "receipt_expired"
    assert run_check(path, [*records, refund]) == 0
    refund["attempts"] = []
    refund["result"] = "receipt_invalid"
    assert run_check(path, [*records, refund]) == 1
    refund["result"] = "applied"
    refund["attempts"] = [{"code": "applied", "t_us": 0, "credited": 1000}]
    assert run_check(path, [*records, refund]) == 0
    changed = copy.deepcopy(refund)
    changed["attempts"] = "bad"
    assert run_check(path, [*records, changed]) == 2


@pytest.mark.parametrize("raw", ["", "[]", '{"type":"clock","value_ns":true}', '{"type":"bad"}'])
def test_malformed(tmp_path: Path, raw: str) -> None:
    path = tmp_path.joinpath("log")
    path.write_text(raw)
    assert main([str(path)]) == 2


def test_invalid_window_input() -> None:
    with pytest.raises(RecordError, match="unknown"):
        check_window([], [], [(2, 2)], Policy())


@pytest.mark.parametrize("code", [30, 31, 32, 33])
@pytest.mark.parametrize("t_us", [0, TBOUND // 1000 - 1])
@pytest.mark.parametrize("released,expected", [(False, 1), (True, 0)])
def test_cleanup_changes_no_window(
    tmp_path: Path, code: int, t_us: int, released: bool, expected: int
) -> None:
    # Three simultaneous deductions exceed capacity two by one token. A
    # released receipt with no final event supplies one possible refund credit.
    records: list[dict[str, object]] = [
        *log()[:2],
        acquire(1000, 1),
        acquire(0, 2),
        dict(acquire(0, 3), deduction_id="extra"),
    ]
    if released:
        records.append({"type": "release", "instance": "i", "deduction_id": "YQ:1:1"})
    records.append({"type": "cleanup", "key_b64url": "YQ", "t_us": t_us, "code": code})
    assert run_check(tmp_path.joinpath("log"), records) == expected


def test_cleanup_preserves_generation_history(tmp_path: Path) -> None:
    # Four simultaneous allows cost 4000 units across both generations;
    # BUD-11a's 2000-unit capacity belongs to the epoch and key.
    records: list[dict[str, object]] = [
        *log()[:2],
        acquire(1000, 1),
        acquire(0, 2),
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 30},
        acquire(1000, 1, g=2),
        acquire(0, 2, g=2),
    ]
    assert run_check(tmp_path.joinpath("log"), records) == 1


def test_cleanup_preserves_possible_credit(tmp_path: Path) -> None:
    # At each instant three allows cost 3000 units. The released receipt
    # supplies 1000 possible units in [0, 10000), including after cleanup.
    # At its 10000-ns expiry only capacity 2000 remains, so excess is 1000.
    for time, expected in ((1000, 0), (9000, 0), (10000, 1)):
        records: list[dict[str, object]] = [
            *log()[:2],
            acquire(1000, 1),
            {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
            {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 30},
            acquire(1000, 1, time, g=2),
            acquire(0, 2, time, g=2),
            dict(acquire(0, 3, time, g=2), deduction_id="extra"),
        ]
        assert run_check(tmp_path.joinpath("log"), records) == expected


def test_cleanup_completion_order_time(tmp_path: Path) -> None:
    # Completion order permits a skipped reply at 0 us after an allow at
    # 1000 ns; a reply at that allow's 1-us time is valid too.
    for t_us in (0, 1):
        assert (
            run_check(
                tmp_path.joinpath("log"),
                [*log(), {"type": "cleanup", "key_b64url": "YQ", "t_us": t_us, "code": 33}],
            )
            == 0
        )


@pytest.mark.parametrize("t_us", [0, TBOUND // 1000 - 1])
@pytest.mark.parametrize("released,expected", [(False, 1), (True, 0)])
def test_cleanup_scan_changes_no_window(
    tmp_path: Path, t_us: int, released: bool, expected: int
) -> None:
    # The page read stores time, not budget units: three simultaneous allows
    # exceed capacity two unless the release supplies one possible credit.
    records: list[dict[str, object]] = [*log()[:2], acquire(1000, 1), acquire(0, 2)]
    if released:
        records.append({"type": "release", "instance": "i", "deduction_id": "YQ:1:1"})
    records.extend(
        [{"type": "cleanup_scan", "t_us": t_us}, dict(acquire(0, 3), deduction_id="extra")]
    )
    assert run_check(tmp_path.joinpath("log"), records) == expected


@pytest.mark.parametrize("value", [True, 1.0, "1", -1, TBOUND // 1000])
def test_cleanup_scan_validates_time(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], value: object
) -> None:
    assert (
        run_check(tmp_path.joinpath("log"), [*log()[:2], {"type": "cleanup_scan", "t_us": value}])
        == 2
    )
    assert "line 3: field t_us:" in capsys.readouterr().out


@pytest.mark.parametrize("missing", ["t_us", "unexpected"])
def test_cleanup_scan_validates_shape(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], missing: str
) -> None:
    scan: dict[str, object] = {"type": "cleanup_scan", "t_us": 0}
    if missing == "unexpected":
        scan[missing] = 0
    else:
        del scan[missing]
    assert run_check(tmp_path.joinpath("log"), [*log()[:2], scan]) == 2
    assert "line 3: field fields:" in capsys.readouterr().out


def test_cleanup_scan_requires_current_epoch(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    scan: dict[str, object] = {"type": "cleanup_scan", "t_us": 0}
    for records, line in ([scan], 1), ([log()[0], scan], 2):
        assert run_check(tmp_path.joinpath("log"), records) == 2
        assert f"line {line}: field epoch:" in capsys.readouterr().out
    # A pending policy keeps the previous epoch current until the next epoch.
    assert (
        run_check(
            tmp_path.joinpath("log"),
            [*log()[:2], log()[0], scan, {"type": "epoch", "epoch_id": "f", "origin_ns": 0}],
        )
        == 0
    )


@pytest.mark.parametrize(
    "field,value",
    [
        ("key_b64url", 1),
        ("key_b64url", "YQ="),
        ("key_b64url", ""),
        ("key_b64url", "_w"),
        ("key_b64url", "YWFh" * 86),
        ("t_us", True),
        ("t_us", 1.0),
        ("t_us", "1"),
        ("t_us", -1),
        ("t_us", TBOUND // 1000),
        ("code", True),
        ("code", 30.0),
        ("code", "removed"),
        ("code", 29),
        ("code", 34),
    ],
)
def test_cleanup_validates_each_field(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object
) -> None:
    cleanup: dict[str, object] = {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 33}
    cleanup[field] = value
    assert run_check(tmp_path.joinpath("log"), [*log()[:2], cleanup]) == 2
    output = capsys.readouterr().out
    assert "line 3:" in output
    assert field in output


@pytest.mark.parametrize("missing", ["key_b64url", "t_us", "code", "unexpected"])
def test_cleanup_validates_shape(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], missing: str
) -> None:
    cleanup: dict[str, object] = {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 33}
    if missing == "unexpected":
        cleanup[missing] = 0
    else:
        del cleanup[missing]
    assert run_check(tmp_path.joinpath("log"), [*log()[:2], cleanup]) == 2
    assert "line 3: field fields:" in capsys.readouterr().out


def test_cleanup_requires_current_epoch(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    cleanup: dict[str, object] = {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 33}
    for records, line in ([cleanup], 1), ([log()[0], cleanup], 2):
        assert run_check(tmp_path.joinpath("log"), records) == 2
        assert f"line {line}: field epoch:" in capsys.readouterr().out
    # A pending policy keeps the previous epoch current until the next epoch.
    assert (
        run_check(
            tmp_path.joinpath("log"),
            [*log()[:2], log()[0], cleanup, {"type": "epoch", "epoch_id": "f", "origin_ns": 0}],
        )
        == 0
    )
