import base64
import io
import json
from contextlib import redirect_stdout
from pathlib import Path

import pytest

from refmodel import RecordError
from refmodel.model import Model, Policy
from refmodel.replay import NEUTRAL, main
from refmodel.tests.test_replay import acquire, log, run

# Hand-derived DESIGN A3.1 bound, independent of model constants.
TBOUND = (2**53 - 2**43) * 1000


def reply(result: str, **fields: object) -> dict[str, object]:
    record = acquire(0, 0)
    record.update(result=result, retry_after_ns=0)
    record.update(fields)
    return record


@pytest.mark.parametrize("result", sorted(NEUTRAL | {"validation_error"}))
def test_anywhere_neutral(tmp_path: Path, capsys: pytest.CaptureFixture[str], result: str) -> None:
    r = reply(result, key_b64url="" if result == "validation_error" else "YQ")
    for records in ([r], [r, *log()], [log()[0], r, *log()[1:]]):
        assert run(tmp_path.joinpath("log"), records) == 0
    for field in ("deduction_id", "epoch_id"):
        bad = dict(r, **{field: "bad"})
        assert run(tmp_path.joinpath("log"), [bad]) == 1
        output = capsys.readouterr().out
        assert "line 1:" in output and field in output
        assert "expected ''" in output and "actual 'bad'" in output


@pytest.mark.parametrize("position", [0, 1])
def test_clock_anywhere(tmp_path: Path, position: int) -> None:
    records = log()
    records.insert(position, {"type": "clock", "value_ns": 1000})
    assert run(tmp_path.joinpath("log"), records) == 0
    for value in (-1000, 1500):
        records[position]["value_ns"] = value
        assert run(tmp_path.joinpath("log"), records) == 2


@pytest.mark.parametrize("result", ["allowed", "limited", "capacity_error", "clock_error"])
def test_epoch_required(tmp_path: Path, result: str) -> None:
    assert run(tmp_path.joinpath("log"), [reply(result)]) == 2


def test_validation_without_model(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    for key, expected in (
        ("", 0),
        ("_w", 0),
        ("YQ", 1),
        ("w6k", 1),
        (base64.urlsafe_b64encode(b"a" * 256).decode().rstrip("="), 1),
        (base64.urlsafe_b64encode(b"a" * 257).decode().rstrip("="), 0),
    ):
        assert (
            run(tmp_path.joinpath("log"), [reply("validation_error", key_b64url=key)]) == expected
        )
        output = capsys.readouterr().out
        if expected == 1:
            assert "line 1:" in output and "validation_error" in output


@pytest.mark.parametrize("result", ["outcome_unknown", "validation_error"])
def test_neutral_hwm(tmp_path: Path, result: str) -> None:
    records = [
        *log()[:2],
        {"type": "clock", "value_ns": 1000},
        reply(result, key_b64url="" if result == "validation_error" else "YQ"),
        {"type": "clock", "value_ns": 0},
        acquire(1000, 1),
    ]
    assert run(tmp_path.joinpath("log"), records) == 0


@pytest.mark.parametrize("case", ["no-policy", "second", "pending", "duplicate", "empty"])
def test_epoch_grammar(tmp_path: Path, case: str) -> None:
    assert run(tmp_path.joinpath("log"), log()) == 0
    records = log()
    if case == "no-policy":
        records = [records[1]]
    elif case == "second":
        records.append({"type": "epoch", "epoch_id": "f", "origin_ns": 0})
    elif case == "pending":
        records.append(log()[0])
    else:
        records.extend(
            [
                log()[0],
                {"type": "epoch", "epoch_id": "e" if case == "duplicate" else "", "origin_ns": 0},
            ]
        )
    assert run(tmp_path.joinpath("log"), records) == 2


def test_epoch_resets_clock_and_ids(tmp_path: Path) -> None:
    records = [
        *log(),
        log()[0],
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
        dict(acquire(1000, 1), epoch_id="f", deduction_id="fresh"),
    ]
    assert run(tmp_path.joinpath("log"), records) == 0
    records[-1]["deduction_id"] = records[3]["deduction_id"]
    assert run(tmp_path.joinpath("log"), records) == 1
    # Pending policy invalidates the preceding model.
    assert run(tmp_path.joinpath("log"), [*log(), log()[0], acquire(1000, 4, 1000)]) == 2


@pytest.mark.parametrize(
    "index,field,value",
    [
        (0, "extra", 1),
        (1, "extra", 1),
        (2, "extra", 1),
        (1, "epoch_id", 5),
        (3, "instance", 5),
        (3, "deduction_id", 5),
        (3, "epoch_id", 5),
        (3, "key_b64url", "YR"),
        (3, "key_b64url", "YQ=="),
        (1, "origin_ns", -1000),
        (1, "origin_ns", 1500),
        (1, "origin_ns", TBOUND),
    ],
)
def test_record_validation(tmp_path: Path, index: int, field: str, value: object) -> None:
    records = log()
    records[index][field] = value
    assert run(tmp_path.joinpath("log"), records) == 2


def test_canonical_and_neutral_keys(tmp_path: Path) -> None:
    assert run(tmp_path.joinpath("log"), [*log()[:3], acquire(1000, 1, key="fn5-")]) == 0
    assert run(tmp_path.joinpath("log"), [reply("busy", key_b64url="!")]) == 2


@pytest.mark.parametrize(
    "raw",
    [
        "[1]",
        '{"type":"clock","value_ns":0,"value_ns":0}',
        '{"\\ud800":0,"\\ud800":0}',
        '{"x":' + "1" * 4301 + "}",
        "[" * 10000 + "0" + "]" * 10000,
    ],
)
def test_malformed_json(tmp_path: Path, capsys: pytest.CaptureFixture[str], raw: str) -> None:
    path = tmp_path.joinpath("log")
    path.write_text(raw)
    assert main([str(path)]) == 2
    output = capsys.readouterr().out
    assert "line 1" in output
    if "value_ns" in raw:
        assert "value_ns" in output
    # Exercise main's print with the strict UTF-8 encoding used by a CLI pipe.
    with (
        io.TextIOWrapper(io.BytesIO(), encoding="utf-8", errors="strict") as stdout,
        redirect_stdout(stdout),
    ):
        assert main([str(path)]) == 2


@pytest.mark.parametrize("raw", ["", "\n", "\r", "\n\n", "\n\n{}"])
def test_record_separators(tmp_path: Path, raw: str) -> None:
    path = tmp_path.joinpath("log")
    clock = json.dumps({"type": "clock", "value_ns": 0})
    if raw == "\r":
        raw = clock + raw + clock
    elif raw == "\n\n":
        raw = clock + raw
    elif raw == "\n\n{}":
        raw = clock + "\n\n" + clock
    path.write_bytes(raw.encode("utf-8"))
    assert main([str(path)]) == 2
    for ending in ("", "\n"):
        path.write_bytes((clock + "\n" + clock + ending).encode("utf-8"))
        assert main([str(path)]) == 0


def test_unicode_lines_and_cli(tmp_path: Path) -> None:
    path = tmp_path.joinpath("log")
    records = log()
    records[3]["instance"] = "i\u0085\u2028\u2029"
    path.write_text("\n".join(json.dumps(r, ensure_ascii=False) for r in records) + "\n")
    assert main([str(path)]) == 0
    assert main([str(path), str(path)]) == 2
    path.write_bytes(b"\xff")
    assert main([str(path)]) == 2


def test_first_mismatch_values(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    records = log()
    records[3]["u"] = 1001
    records[4]["u"] = 1
    assert run(tmp_path.joinpath("log"), records) == 1
    output = capsys.readouterr().out
    assert "1000" in output and "1001" in output
    assert output.count("line ") == 1


def test_regressed_allow(tmp_path: Path) -> None:
    # cap=20000, cost=10000, refill=3/ns. Hwm=4000 after first allow.
    # Regressed allows spend remaining 10000 at t=4000, expiry=4+10=14 us.
    # At 5000, a refills (5000-4000)*3=3000, not from raw 1000.
    records = log()[:2]
    records[0].update(rate_period_ns=10000, max_tracked_buckets=2)
    steps = [
        (
            4000,
            reply(
                "allowed",
                u=10000,
                g=1,
                s=1,
                decision_ns=4000,
                expiry_us=14,
                deduction_id="a1",
                epoch_id="e",
            ),
        ),
        (
            1000,
            reply(
                "allowed", g=1, s=2, decision_ns=4000, expiry_us=14, deduction_id="a2", epoch_id="e"
            ),
        ),
        (2000, reply("limited", decision_ns=4000, retry_after_ns=3334)),
        (
            1000,
            reply(
                "allowed",
                key_b64url="Yg",
                u=10000,
                g=2,
                s=1,
                decision_ns=4000,
                expiry_us=14,
                deduction_id="b1",
                epoch_id="e",
            ),
        ),
        (1000, reply("capacity_error", key_b64url="Yw", decision_ns=4000)),
        (5000, reply("limited", u=3000, decision_ns=5000, retry_after_ns=2334)),
    ]
    for clock, record in steps:
        records.extend([{"type": "clock", "value_ns": clock}, record])
    assert run(tmp_path.joinpath("log"), records) == 0


def test_clock_boundary(tmp_path: Path) -> None:
    assert Model(Policy(), TBOUND - 1000).hwm == TBOUND - 1000
    records = [
        *log()[:2],
        {"type": "clock", "value_ns": TBOUND},
        reply("clock_error"),
        {"type": "clock", "value_ns": TBOUND - 1000},
        acquire(1000, 1, TBOUND - 1000),
    ]
    assert run(tmp_path.joinpath("log"), records) == 0
    for origin in (TBOUND, 1500):
        with pytest.raises(RecordError):
            Model(Policy(), origin)


def test_policy_edges(tmp_path: Path) -> None:
    accepted = [
        {"rate_tokens": 2**31 - 1},
        {"capacity": 1, "rate_period_ns": 4503599627370000},
        {"receipt_lifetime_ns": (2**40 - 1) * 1000},
    ]
    # ceil(2^52 / 10^9)=4503600; product=4503600000000000 >= 2^52.
    rejected = [
        {"capacity": 4503600},
        {"rate_period_ns": 1500},
        {"capacity": 1, "rate_period_ns": 4503599627371000},
    ]
    for expected, cases in ((0, accepted), (2, rejected)):
        for kwargs in cases:
            records = log()[:2]
            records[0] = {
                "type": "policy",
                "rate_tokens": 10,
                "rate_period_ns": 1000000000,
                "capacity": 20,
                "max_tracked_buckets": 1000,
                "receipt_lifetime_ns": 60000000000,
                **kwargs,
            }
            assert run(tmp_path.joinpath("log"), records) == expected


def test_model_clock_granularity() -> None:
    with pytest.raises(RecordError, match="clock_ns"):
        Model(Policy()).acquire(b"a", 1500)
