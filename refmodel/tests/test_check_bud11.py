import copy
import json
import random
from pathlib import Path

import pytest

from refmodel import RecordError
from refmodel.check_bud11 import check_window, main
from refmodel.model import Policy
from refmodel.tests.test_replay import acquire, log


def test_bound_and_one_unit_over() -> None:
    p = Policy(rate_tokens=1, rate_period_ns=1000, capacity=2)
    assert check_window([0, 0, 1000], [], [], p) is None
    violation = check_window([0, 0, 999], [], [], p)
    assert violation is not None
    assert (violation.start_ns, violation.end_ns, violation.excess_units) == (0, 999, 1)
    assert check_window([0, 0, 0], [], [], p) is not None


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
