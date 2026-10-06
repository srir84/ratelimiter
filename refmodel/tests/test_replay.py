import copy
import json
from pathlib import Path

import pytest

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
    original = log()
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
            ("epoch_id", "other"),
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
        assert run(path, records) == 1, (index, field)
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
    ],
)
def test_neutral(tmp_path: Path, result: str) -> None:
    records = log()
    neutral = acquire(0, 0)
    neutral["result"] = result
    records.insert(3, neutral)
    assert run(tmp_path.joinpath("log.jsonl"), records) == 0


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
