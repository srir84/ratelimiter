import copy
import json
from pathlib import Path
from types import CodeType, FunctionType

import pytest

from refmodel import replay as replay_module
from refmodel.model import Model, Policy, RefundDecision
from refmodel.replay import main


def acquire(u: int, s: int, time: int = 0, key: str = "YQ", g: int = 1) -> dict[str, object]:
    # Policy: 3 units/ns, 1000 units/token, capacity 2.
    # At t=0 deductions leave 1000 then 0. At t=1000 refill caps at 2000.
    allowed = s > 0
    return {
        "type": "acquire",
        "instance": "i",
        "key_b64url": key,
        "result": "allowed" if allowed else "limited",
        "decision_ns": time,
        "retry_after_ns": 0 if allowed else 334,
        "u": u,
        "g": g if allowed else 0,
        "s": s,
        "expiry_us": time // 1000 + 10 if allowed else 0,
        "deduction_id": f"{key}:{g}:{s}" if allowed else "",
        "epoch_id": "e" if allowed else "",
    }


def log() -> list[dict[str, object]]:
    return [
        {
            "type": "policy",
            "rate_tokens": 3,
            "rate_period_ns": 1000,
            "capacity": 2,
            "max_tracked_buckets": 3,
            "receipt_lifetime_ns": 10000,
        },
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        {"type": "clock", "value_ns": 0},
        acquire(1000, 1),
        acquire(0, 2),
        acquire(0, 0),
        {"type": "clock", "value_ns": 1000},
        acquire(1000, 3, 1000),
    ]


def run(path: Path, records: list[dict[str, object]]) -> int:
    path.write_text("\n".join(json.dumps(r) for r in records) + "\n")
    return main([str(path)])


def test_detects_each_field(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("log.jsonl")
    original = [*log(), log()[0], {"type": "epoch", "epoch_id": "f", "origin_ns": 0}]
    assert run(path, original) == 0
    # Every numeric comparison, in both allowed and limited replies, ±1.
    for index in (3, 5, 7):
        for field in ("u", "decision_ns", "retry_after_ns", "g", "s", "expiry_us"):
            for delta in (-1, 1):
                records = copy.deepcopy(original)
                value = records[index][field]
                assert isinstance(value, int)
                records[index][field] = value + delta
                assert run(path, records) == 1, (index, field, delta)
                output = capsys.readouterr().out
                assert f"line {index + 1}:" in output
                assert f"expected {value!r}" in output
                assert f"actual {value + delta!r}" in output
        for field, value in (
            ("result", "limited" if index != 5 else "allowed"),
            ("epoch_id", "f"),
            ("deduction_id", "" if index != 5 else "extra"),
        ):
            records = copy.deepcopy(original)
            records[index][field] = value
            assert run(path, records) == 1
            assert f"line {index + 1}:" in capsys.readouterr().out
    # Input-only fields have no independently known expected value; mutations
    # are detected through subsequent replies, rather than invented metadata.
    for index, field, value in (
        (0, "rate_tokens", 2),
        (0, "rate_tokens", 4),
        (0, "rate_period_ns", 2000),
        (0, "capacity", 1),
        (0, "capacity", 3),
        (0, "receipt_lifetime_ns", 9000),
        (0, "receipt_lifetime_ns", 11000),
        (1, "origin_ns", 1000),
        (1, "epoch_id", "f"),
        (2, "value_ns", 1000),
        (6, "value_ns", 0),
        (6, "value_ns", 2000),
        (4, "key_b64url", "Yg"),
    ):
        records = copy.deepcopy(original)
        records[index][field] = value
        assert run(path, records) == (2 if field == "epoch_id" else 1), (index, field)
        assert "line " in capsys.readouterr().out
    for index in (3, 6):
        records = copy.deepcopy(original)
        del records[index]
        assert run(path, records) == 1
        assert "line " in capsys.readouterr().out
    records = copy.deepcopy(original)
    records[4], records[5] = records[5], records[4]
    assert run(path, records) == 1
    assert "line 5:" in capsys.readouterr().out
    check_refund_fields(tmp_path, capsys)
    check_cleanup_fields(tmp_path, capsys)
    check_admin_fields(tmp_path, capsys)
    test_capacity_policy_sensitivity(tmp_path, capsys)
    test_exit_zero_equivalences(tmp_path)
    # Error replies compare zero-valued numeric fields as well.
    for result, key, clock in (
        ("validation_error", "", 0),
        ("clock_error", "YQ", (2**53 - 2**43) * 1000),
    ):
        error = dict(acquire(0, 0), result=result, key_b64url=key, retry_after_ns=0)
        baseline_error = [*log()[:2], {"type": "clock", "value_ns": clock}, error]
        assert run(path, baseline_error) == 0
        for field in ("u", "decision_ns", "retry_after_ns", "g", "s", "expiry_us"):
            for delta in (-1, 1):
                changed = copy.deepcopy(baseline_error)
                changed[-1][field] = delta
                assert run(path, changed) == 1, (result, field, delta)
                output = capsys.readouterr().out
                assert "line 4:" in output
                assert "expected 0" in output and f"actual {delta}" in output

    # Interior baselines: one microsecond in either direction is well-formed.
    for field in ("rate_period_ns", "origin_ns"):
        baseline = log()[:2]
        baseline[0]["rate_period_ns"] = 2000
        baseline[1]["origin_ns"] = 1000
        baseline.append(dict(acquire(2000, 1, 1000), expiry_us=11))
        assert run(path, baseline) == 0
        index = 0 if field == "rate_period_ns" else 1
        for delta in (-1000, 1000):
            changed = copy.deepcopy(baseline)
            value = changed[index][field]
            assert isinstance(value, int)
            changed[index][field] = value + delta
            assert run(path, changed) == 1, (field, delta)
            assert "line " in capsys.readouterr().out


def test_exit_zero_equivalences(tmp_path: Path) -> None:
    path = tmp_path.joinpath("log.jsonl")
    records = log()
    del records[5]  # limited writes neither bucket nor hwm
    assert run(path, records) == 0
    records = [
        *log()[:4],
        acquire(1000, 1, key="Yg", g=2),
        acquire(0, 2),
        acquire(0, 2, key="Yg", g=2),
    ]
    assert run(path, records) == 0
    records[-1], records[-2] = records[-2], records[-1]
    assert run(path, records) == 0


@pytest.mark.parametrize(
    "result",
    [
        "not_ready",
        "config_error",
        "busy",
        "cancelled",
        "unavailable",
        "degraded",
        "outcome_unknown",
        "state_integrity_error",
    ],
)
def test_neutral(tmp_path: Path, result: str) -> None:
    records = log()
    neutral = acquire(0, 0)
    neutral["result"] = result
    records.insert(3, neutral)
    assert run(tmp_path.joinpath("log.jsonl"), records) == 0


def test_state_integrity_error_anywhere(tmp_path: Path) -> None:
    # Neutral reply numbers are opaque, including before there is a model.
    neutral = dict(acquire(37, 0, 9000), result="state_integrity_error")
    records = [neutral, log()[0], neutral, *log()[1:], neutral]
    assert run(tmp_path.joinpath("log.jsonl"), records) == 0


@pytest.mark.parametrize("before_epoch", [False, True])
@pytest.mark.parametrize("field", ["deduction_id", "epoch_id"])
def test_state_integrity_error_empty_ids(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], before_epoch: bool, field: str
) -> None:
    neutral = dict(acquire(0, 0), result="state_integrity_error")
    records = [neutral, *log()] if before_epoch else [*log(), neutral]
    path = tmp_path.joinpath("log.jsonl")
    assert run(path, records) == 0
    neutral[field] = "unexpected"
    assert run(path, records) == 1
    assert f"{field}: expected '', actual 'unexpected'" in capsys.readouterr().out


def test_state_integrity_error_preserves_budget_and_hwm(tmp_path: Path) -> None:
    # A's first allow leaves 1000 at hwm=0. The refusal at 5 us must neither
    # spend/refill that budget nor store time: the regressed allow uses 0,
    # spends the remaining 1000 and retains sequence 2, then limits.
    records = [
        *log()[:4],
        {"type": "clock", "value_ns": 5000},
        dict(acquire(0, 0), result="state_integrity_error"),
        {"type": "clock", "value_ns": 0},
        acquire(0, 2),
        acquire(0, 0),
    ]
    path = tmp_path.joinpath("log.jsonl")
    assert run(path, records[:5] + records[6:]) == 0
    assert run(path, records) == 0


@pytest.mark.parametrize(
    "change", ["missing", "extra", "bool", "key", "unsupported", "result", "order"]
)
def test_exit_two(tmp_path: Path, capsys: pytest.CaptureFixture[str], change: str) -> None:
    records = log()
    if change == "missing":
        del records[3]["u"]
    elif change == "extra":
        records[3]["extra"] = 1
    elif change == "bool":
        records[3]["u"] = True
    elif change == "key":
        records[3]["key_b64url"] = "!"
    elif change == "unsupported":
        records[3]["type"] = "refund"
    elif change == "result":
        records[3]["result"] = "surprise"
    else:
        records = [records[3]]
    assert run(tmp_path.joinpath("log.jsonl"), records) == 2
    assert "line " in capsys.readouterr().out


def test_bad_json_and_arguments(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("bad.jsonl")
    path.write_text("{\n")
    assert main([str(path)]) == 2
    assert "line 1" in capsys.readouterr().out
    assert main([]) == 2
    assert main([str(tmp_path.joinpath("missing"))]) == 2


def test_capacity_policy_sensitivity(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("log.jsonl")
    records = log()[:3]
    records[0]["max_tracked_buckets"] = 2
    records.extend([acquire(1000, 1), acquire(1000, 1, key="Yg", g=2)])
    refused = acquire(0, 0, key="Yw")
    refused["result"] = "capacity_error"
    refused["retry_after_ns"] = 0
    records.append(refused)
    assert run(path, records) == 0
    for limit in (1, 3):
        changed = copy.deepcopy(records)
        changed[0]["max_tracked_buckets"] = limit
        assert run(path, changed) == 1
        assert "line " in capsys.readouterr().out
    for field in ("u", "decision_ns", "retry_after_ns", "g", "s", "expiry_us"):
        for delta in (-1, 1):
            changed = copy.deepcopy(records)
            changed[-1][field] = delta
            assert run(path, changed) == 1
            assert "line 6:" in capsys.readouterr().out


def test_epoch_reset_and_duplicate_deduction(tmp_path: Path) -> None:
    path = tmp_path.joinpath("log.jsonl")
    records = log()
    replacement = copy.deepcopy(log()[:4])
    replacement[1]["epoch_id"] = "next"
    replacement[3]["epoch_id"] = "next"
    replacement[3]["deduction_id"] = "next:a:1:1"
    assert run(path, records + replacement) == 0
    records[4]["deduction_id"] = records[3]["deduction_id"]
    assert run(path, records) == 1


def refund_log() -> list[dict[str, object]]:
    # cap=2000; deductions at zero leave 0. Refund at 0 adds 1000.
    # Subsequent acquire spends it, giving sequence 3 and balance 0.
    return [
        *log()[:5],
        {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
        {
            "type": "refund",
            "deduction_id": "YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 10,
            "attempts": [{"code": "applied", "t_us": 0, "credited": 1000}],
            "result": "applied",
        },
        acquire(0, 3),
    ]


def check_refund_fields(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("refund.jsonl")
    # cap=4000, two deductions leave 0; 3 us refill leaves 3000.
    # Refund adds the remaining 1000; next deduction leaves 2000.
    # s + 1 names the second deduction, so only the s comparison rejects it.
    original = refund_log()
    original[0].update(rate_tokens=1, rate_period_ns=2000)
    original[3]["u"] = 2000
    original[6]["attempts"] = [{"code": "applied", "t_us": 3, "credited": 1000}]
    original[7] = acquire(2000, 3, 3000)
    original.insert(6, {"type": "clock", "value_ns": 3000})
    assert run(path, original) == 0
    for field in ("g", "s", "expiry_us"):
        for delta in (-1, 1):
            records = copy.deepcopy(original)
            value = records[7][field]
            assert isinstance(value, int)
            records[7][field] = value + delta
            assert run(path, records) == 1, (field, delta)
            assert "line 8:" in capsys.readouterr().out
    for field in ("t_us", "credited"):
        for delta in (-1, 1):
            records = copy.deepcopy(original)
            attempts = records[7]["attempts"]
            assert isinstance(attempts, list)
            value = attempts[0][field]
            attempts[0][field] = value + delta
            assert run(path, records) == 1
            assert "line 8:" in capsys.readouterr().out
    for field, value in (("key_b64url", "Yg"), ("result", "already_applied")):
        records = copy.deepcopy(original)
        records[7][field] = value
        assert run(path, records) == (2 if field == "result" else 1)
        assert "line 8:" in capsys.readouterr().out
    records = copy.deepcopy(original)
    attempts = records[7]["attempts"]
    assert isinstance(attempts, list)
    attempts[0]["code"] = "already_applied"
    assert run(path, records) == 2
    assert "line 8:" in capsys.readouterr().out
    records = copy.deepcopy(original)
    del records[7]
    assert run(path, records) == 1
    assert "line 8:" in capsys.readouterr().out


def test_receipt_error_and_repeat(tmp_path: Path) -> None:
    path = tmp_path.joinpath("log")
    records = log()
    records[3].update(result="receipt_error", deduction_id="", epoch_id="")
    assert run(path, records) == 0
    records = refund_log()
    # B7/API-1d: one final record, with a lost reply and its confirming retry.
    records[6]["result"] = "already_applied"
    records[6]["attempts"] = [
        {"code": "outcome_unknown", "t_us": 0, "credited": 0},
        {"code": "already_applied", "t_us": 0, "credited": 0},
    ]
    assert run(path, records) == 0
    assert main(["--mutant", "unknown", str(path)]) == 2
    records = refund_log()
    records[6]["attempts"] = [{"code": 3, "t_us": 0, "credited": 1000}]
    assert run(path, records) == 0


@pytest.mark.parametrize(
    "name",
    [
        "allow_strict",
        "new_bucket_short",
        "credit_uncapped",
        "credit_before_refill",
        "refill_uncapped",
        "retry_floor",
        "hwm_on_limited",
        "expiry_strict",
    ],
)
def test_mutants(tmp_path: Path, name: str) -> None:
    path = tmp_path.joinpath("log")
    records = log()
    if name in {"credit_uncapped", "credit_before_refill"}:
        records = [
            *log()[:4],
            {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
            {"type": "clock", "value_ns": 1000},
            dict(refund_log()[6], attempts=[{"code": "applied", "t_us": 1, "credited": 0}]),
        ]
    elif name == "refill_uncapped":
        records = [*log()[:4], {"type": "clock", "value_ns": 10000}, acquire(1000, 2, 10000)]
    elif name == "expiry_strict":
        records = [
            *log()[:4],
            {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
            {"type": "clock", "value_ns": 10000},
            dict(
                refund_log()[6],
                result="receipt_expired",
                attempts=[{"code": "receipt_expired", "t_us": 10, "credited": 0}],
            ),
        ]
    elif name == "hwm_on_limited":
        # Larger token period: at 1000 ns refill gives 3000 < 10000.
        # Limited must not store 1000; at regressed zero the units are still zero.
        records = [*log()[:2]]
        records[0]["rate_period_ns"] = 10000
        records.extend(
            [
                acquire(10000, 1),
                acquire(0, 2),
                {"type": "clock", "value_ns": 1000},
                dict(acquire(3000, 0, 1000), retry_after_ns=2334),
                {"type": "clock", "value_ns": 0},
                dict(acquire(0, 0), retry_after_ns=3334),
            ]
        )
    assert run(path, records) == 0
    assert main(["--mutant", name, str(path)]) == 1


def moved_cleanup_log() -> list[dict[str, object]]:
    # At 0, two deductions leave 0 and the first receipt's refund restores 1000.
    # The marker expires at 10 us. An acquire at 9 us leaves 1000,
    # extends x to 19 us; cleanup at 10 removes only the old marker.
    # A clock regression to 9 us still uses cleanup's hwm of 10 us.
    return [
        *refund_log()[:7],
        {"type": "clock", "value_ns": 9000},
        acquire(1000, 3, 9000),
        {"type": "clock", "value_ns": 10000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 10, "code": 31},
        {"type": "clock", "value_ns": 9000},
        acquire(1000, 4, 10000),
    ]


def check_cleanup_fields(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("cleanup.jsonl")
    original = moved_cleanup_log()
    assert run(path, original) == 0
    for field in ("code", "t_us"):
        for delta in (-1, 1):
            records = copy.deepcopy(original)
            value = records[10][field]
            assert isinstance(value, int)
            records[10][field] = value + delta
            assert run(path, records) == 1, (field, delta)
            output = capsys.readouterr().out
            assert f"line 11: {field}:" in output
            assert f"expected {value!r}, actual {value + delta!r}" in output
    # Changing the input key by one byte changes the actual cleanup outcome.
    for key in ("YA", "Yg"):
        records = copy.deepcopy(original)
        records[10]["key_b64url"] = key
        assert run(path, records) == 1
        assert "line 11:" in capsys.readouterr().out
    records = copy.deepcopy(original)
    del records[10]
    assert run(path, records) == 1
    assert "line 12:" in capsys.readouterr().out


def test_cleanup_removed_and_new_generation(tmp_path: Path) -> None:
    # At x=10 us the balance has refilled to 2000, so removal is legal.
    # Recreating the key starts at 2000, deducts 1000 and allocates g=2.
    records = [
        *log()[:4],
        {"type": "clock", "value_ns": 10000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 10, "code": 30},
        acquire(1000, 1, 10000, g=2),
    ]
    path = tmp_path.joinpath("cleanup.jsonl")
    assert run(path, records) == 0
    del records[5]
    assert run(path, records) == 1


def test_cleanup_moved_marker_removal_and_hwm(tmp_path: Path) -> None:
    assert run(tmp_path.joinpath("cleanup.jsonl"), moved_cleanup_log()) == 0


def test_cleanup_uses_hwm_after_clock_regression(tmp_path: Path) -> None:
    # A's marker expires at 10 us, its latest receipt at 19 us. B's allow
    # stores hwm=10 us, so a raw clock of 0 still removes A's expired marker,
    # keeps A at 2000 units, and the next deduction leaves 1000 units.
    records = [
        *refund_log()[:7],
        {"type": "clock", "value_ns": 9000},
        acquire(1000, 3, 9000),
        {"type": "clock", "value_ns": 10000},
        acquire(1000, 1, 10000, key="Yg", g=2),
        {"type": "clock", "value_ns": 0},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 10, "code": 31},
        acquire(1000, 4, 10000),
    ]
    assert run(tmp_path.joinpath("cleanup.jsonl"), records) == 0


@pytest.mark.parametrize("gone", [False, True])
def test_cleanup_skipped(tmp_path: Path, capsys: pytest.CaptureFixture[str], gone: bool) -> None:
    # At 9 us a present bucket is not due (x=10). Both skipped replies carry
    # budget time 9 us, but store nothing: the later regressed allow uses 0.
    records: list[dict[str, object]] = [
        *log()[:4],
        {"type": "clock", "value_ns": 9000},
        {
            "type": "cleanup",
            "key_b64url": "Yg" if gone else "YQ",
            "t_us": 9,
            "code": 33,
        },
        {"type": "clock", "value_ns": 0},
        acquire(0, 2),
    ]
    path = tmp_path.joinpath("cleanup.jsonl")
    assert run(path, records) == 0
    for wrong_time in (7, 8, 10):
        changed = copy.deepcopy(records)
        changed[5]["t_us"] = wrong_time
        assert run(path, changed) == 1
        assert f"line 6: t_us: expected 9, actual {wrong_time}" in capsys.readouterr().out


def test_cleanup_clock_bound_mismatch(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    bound = (2**53 - 2**43) * 1000  # DESIGN A3.1, hand-derived.
    path = tmp_path.joinpath("cleanup.jsonl")
    records = [
        *log()[:2],
        {"type": "clock", "value_ns": bound - 1000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": bound // 1000 - 1, "code": 33},
    ]
    assert run(path, records) == 0
    for clock in (bound, bound + 1000):
        records[2]["value_ns"] = clock
        records[3]["t_us"] = 0
        assert run(path, records) == 1
        assert "line 4: code: expected 'clock_error', actual 33" in capsys.readouterr().out


def test_cleanup_between_policy_and_epoch(tmp_path: Path) -> None:
    records = [
        *log()[:2],
        log()[0],
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 33},
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
    ]
    assert run(tmp_path.joinpath("cleanup.jsonl"), records) == 0


def test_cleanup_scan_stores_hwm_and_checks_time(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    # The pass at 5 us has nothing due (A expires at 10). Its page read still
    # stores hwm=5000. Refill caps A at 2000; the regressed allow leaves 1000,
    # uses sequence 2 and expires at 15 us. Without the scan, it leaves 0.
    records = [
        *log()[:4],
        {"type": "clock", "value_ns": 5000},
        {"type": "cleanup_scan", "t_us": 5},
        {"type": "clock", "value_ns": 0},
        acquire(1000, 2, 5000),
    ]
    path = tmp_path.joinpath("cleanup_scan.jsonl")
    assert run(path, records) == 0
    for t_us in (4, 6):
        changed = copy.deepcopy(records)
        changed[5]["t_us"] = t_us
        assert run(path, changed) == 1
        assert f"line 6: t_us: expected 5, actual {t_us}" in capsys.readouterr().out
    # A second page read after regression must retain the same cutoff.
    records.insert(7, {"type": "cleanup_scan", "t_us": 5})
    assert run(path, records) == 0
    records[7]["t_us"] = 0
    assert run(path, records) == 1
    assert "line 8: t_us: expected 5, actual 0" in capsys.readouterr().out
    without_scan = [*records[:5], records[6], records[-1]]
    assert run(path, without_scan) == 1
    assert "line 7: u: expected 0, actual 1000" in capsys.readouterr().out
    without_scan[-1] = acquire(0, 2)
    assert run(path, without_scan) == 0


def test_cleanup_scan_validation(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("cleanup_scan.jsonl")
    bound_us = 2**53 - 2**43
    for t_us in (0, bound_us - 1):
        records = [
            *log()[:2],
            {"type": "clock", "value_ns": t_us * 1000},
            {"type": "cleanup_scan", "t_us": t_us},
        ]
        assert run(path, records) == 0
    malformed_scans: tuple[dict[str, object], ...] = (
        {"type": "cleanup_scan"},
        {"type": "cleanup_scan", "t_us": 0, "extra": 0},
        {"type": "cleanup_scan", "t_us": True},
        {"type": "cleanup_scan", "t_us": "0"},
        {"type": "cleanup_scan", "t_us": -1},
        {"type": "cleanup_scan", "t_us": bound_us},
    )
    for scan in malformed_scans:
        assert run(path, [*log()[:2], scan]) == 2
        assert "line 3: field " in capsys.readouterr().out
    assert run(path, [{"type": "cleanup_scan", "t_us": 0}]) == 2
    assert "line 1: field type: cleanup_scan before epoch" in capsys.readouterr().out


def test_cleanup_quarantined(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    # Integrity faults have no op-log record; seed the model's quarantine
    # explicitly to exercise the cleanup reply without inventing a log type.
    def quarantined_model(policy: Policy, origin_ns: int, name: str = "") -> Model:
        model = Model(policy, origin_ns)
        model.acquire(b"a", origin_ns)
        model.quarantined.add(b"a")
        model.mutant = name
        return model

    monkeypatch.setattr(replay_module, "make_model", quarantined_model)
    records: list[dict[str, object]] = [
        *log()[:2],
        {"type": "clock", "value_ns": 10000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 10, "code": 31},
    ]
    assert run(tmp_path.joinpath("cleanup.jsonl"), records) == 0


def test_cleanup_after_refund_quarantine(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    # The missing bucket is an integrity fault, not an op-log record. Inject
    # its loss at refund so replay itself transitions the key to quarantine.
    class MissingBucketModel(Model):
        def refund(
            self,
            key_bytes: bytes,
            g: int,
            s: int,
            expiry_us: int,
            clock_ns: int,
            *,
            epoch_matches: bool = True,
            max_live_markers: int | None = None,
            max_markers_per_key: int | None = None,
        ) -> RefundDecision:
            self.buckets.pop(key_bytes, None)
            return super().refund(
                key_bytes,
                g,
                s,
                expiry_us,
                clock_ns,
                epoch_matches=epoch_matches,
                max_live_markers=max_live_markers,
                max_markers_per_key=max_markers_per_key,
            )

    def missing_bucket_model(policy: Policy, origin_ns: int, name: str = "") -> Model:
        model = MissingBucketModel(policy, origin_ns)
        model.mutant = name
        return model

    monkeypatch.setattr(replay_module, "make_model", missing_bucket_model)
    records: list[dict[str, object]] = [
        *log()[:4],
        {"type": "release", "instance": "i", "deduction_id": "YQ:1:1"},
        {"type": "clock", "value_ns": 1000},
        dict(
            refund_log()[6],
            attempts=[{"code": 23, "t_us": 1, "credited": 0}],
            result="unresolved",
        ),
        {"type": "clock", "value_ns": 10000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 10, "code": 31},
        {"type": "clock", "value_ns": 0},
        acquire(1000, 1, 10000, key="Yg", g=2),
    ]
    path = tmp_path.joinpath("cleanup.jsonl")
    assert run(path, records) == 0
    changed = copy.deepcopy(records)
    changed[8]["code"] = 32
    assert run(path, changed) == 1
    assert "line 9: code: expected 31, actual 32" in capsys.readouterr().out


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("code", True),
        ("code", "30"),
        ("code", 29),
        ("code", 34),
        ("t_us", True),
        ("t_us", "10"),
        ("t_us", -1),
        ("t_us", 2**53 - 2**43),
        ("key_b64url", 0),
        ("key_b64url", "!"),
        ("key_b64url", "YQ="),
        ("key_b64url", ""),
        ("key_b64url", "_w"),
        ("key_b64url", "YWFh" * 86),
    ],
)
def test_cleanup_malformed_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object
) -> None:
    records = [
        *log()[:2],
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 33},
    ]
    records[-1][field] = value
    assert run(tmp_path.joinpath("cleanup.jsonl"), records) == 2
    output = capsys.readouterr().out
    assert "line 3:" in output and f"field {field}:" in output


@pytest.mark.parametrize("change", ["missing", "extra", "epoch"])
def test_cleanup_malformed_shape_and_epoch(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], change: str
) -> None:
    records = [
        *log()[:2],
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 0, "code": 33},
    ]
    if change == "missing":
        del records[-1]["code"]
    elif change == "extra":
        records[-1]["instance"] = "i"
    else:
        records = records[-1:]
    assert run(tmp_path.joinpath("cleanup.jsonl"), records) == 2
    output = capsys.readouterr().out
    assert "line 1:" in output if change == "epoch" else "line 3:" in output
    assert "field type:" in output if change == "epoch" else "field fields:" in output


def test_pb17_units_comparison(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    source_path = Path(replay_module.__file__ or "")
    source = source_path.read_text()
    mutation = '            checks.append(("u", expected.u, actual["u"]))\n'
    assert source.count(mutation) == 1
    compiled = compile(source.replace(mutation, ""), str(source_path), "exec")
    replay_code = next(
        code
        for code in compiled.co_consts
        if isinstance(code, CodeType) and code.co_name == "replay"
    )
    records = log()
    records[3]["u"] = 1001
    path = tmp_path.joinpath("pb17.jsonl")
    assert run(path, records) == 1
    monkeypatch.setattr(replay_module, "replay", FunctionType(replay_code, vars(replay_module)))
    with pytest.raises(AssertionError):
        assert run(path, records) == 1


def default_policy() -> dict[str, object]:
    return {
        "type": "policy",
        "rate_tokens": 10,
        "rate_period_ns": 1_000_000_000,
        "capacity": 20,
        "max_tracked_buckets": 1000,
        "receipt_lifetime_ns": 60_000_000_000,
    }


def default_acquire(
    u: int, s: int, time: int = 0, *, g: int = 1, epoch: str = "e", key: str = "YQ"
) -> dict[str, object]:
    return {
        "type": "acquire",
        "instance": "i",
        "key_b64url": key,
        "result": "allowed",
        "decision_ns": time,
        "retry_after_ns": 0,
        "u": u,
        "g": g,
        "s": s,
        "expiry_us": time // 1000 + 60_000_000,
        "deduction_id": f"{epoch}:{key}:{g}:{s}",
        "epoch_id": epoch,
    }


def integrity_signal(key: str = "YQ") -> dict[str, object]:
    return dict(
        default_acquire(0, 0, key=key),
        result="state_integrity_error",
        g=0,
        expiry_us=0,
        deduction_id="",
        epoch_id="",
    )


def inspect(
    u: int | str,
    t_us: int | str,
    g: int | str,
    s: int | str,
    x_us: int | str,
    mc: int | str,
    *,
    key: str = "YQ",
    present: int = 1,
) -> dict[str, object]:
    return {
        "type": "inspect",
        "key_b64url": key,
        "present": present,
        "u": u,
        "t_us": t_us,
        "g": g,
        "s": s,
        "x_us": x_us,
        "mc": mc,
    }


def admin_log() -> list[dict[str, object]]:
    # Default policy: units/token = 10**9, refill = 10 units/ns, cap = 20*10**9.
    # At 1 s restore 3*10**9, g=2, s=0, x=61 s, keeping quarantine.
    # Clear changes no balance; 0.5 s refill adds 5*10**9, deduct 10**9
    # at 1.5 s leaves 7*10**9. Refund at that same instant leaves 8*10**9
    # and one marker. Stored x remains the acquire's 61.5 s expiry.
    return [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        integrity_signal(),
        {"type": "clock", "value_ns": 1_000_000_000},
        {
            "type": "restore",
            "key_b64url": "YQ",
            "balance_units": 3_000_000_000,
            "t_us": 1_000_000,
            "g": 2,
            "epoch_id": "e",
        },
        inspect(3_000_000_000, 1_000_000, 2, 0, 61_000_000, 0),
        {"type": "clear", "key_b64url": "YQ", "t_us": 1_000_000},
        {"type": "clock", "value_ns": 1_500_000_000},
        default_acquire(7_000_000_000, 1, 1_500_000_000, g=2),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:2:1"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:2:1",
            "key_b64url": "YQ",
            "g": 2,
            "s": 1,
            "expiry_us": 61_500_000,
            "attempts": [{"code": "applied", "t_us": 1_500_000, "credited": 1_000_000_000}],
            "result": "applied",
        },
        inspect(8_000_000_000, 1_500_000, 2, 1, 61_500_000, 1),
    ]


def check_admin_fields(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    path = tmp_path.joinpath("admin-fields.jsonl")
    original = admin_log()
    assert run(path, original) == 0
    # Interior baselines let each numeric output comparison move by ±1 while
    # remaining well-formed; generation belongs to the never-reused source.
    for index, fields in (
        (5, ("t_us", "g")),
        (7, ("t_us",)),
        (12, ("present", "u", "t_us", "g", "s", "x_us", "mc")),
    ):
        for field in fields:
            for delta in (-1, 1):
                records = copy.deepcopy(original)
                value = records[index][field]
                assert isinstance(value, int)
                records[index][field] = value + delta
                assert run(path, records) == 1, (index, field, delta)
                output = capsys.readouterr().out
                assert f"line {index + 1}: {field}:" in output
                assert f"expected {value!r}, actual {value + delta!r}" in output
    # Restore balance and key are inputs. Stored snapshots make their changed
    # effects observable without inventing an independently known input value.
    for balance in (2_999_999_999, 3_000_000_001):
        records = copy.deepcopy(original)
        records[5]["balance_units"] = balance
        assert run(path, records) == 1
        assert "line 7: u:" in capsys.readouterr().out
    for key in ("YA", "Yg"):
        records = copy.deepcopy(original)
        records[5]["key_b64url"] = key
        assert run(path, records) == 1
        assert "line 6: code: expected 41, actual 40" in capsys.readouterr().out
    for index in (5, 7, 12):
        records = copy.deepcopy(original)
        del records[index]
        if index == 12:
            # Inspect is state neutral, so removing it is an equivalence.
            assert run(path, records) == 0
        else:
            assert run(path, records) == 1
