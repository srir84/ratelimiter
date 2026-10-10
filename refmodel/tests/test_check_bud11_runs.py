"""Hand-computed H5 runs, using only synthetic and mirrored contract records."""

import json
import tomllib
from pathlib import Path

import pytest

from refmodel.check_bud11 import main
from refmodel.model import TBOUND

BASE = 1_790_000_000_000_000_000  # Epoch ns, exact µs, beyond binary64's integer precision.
SECOND = 1_000_000_000
LIFETIME = 60 * SECOND
CONFIG = (
    "[policy]\ncapacity = 20\nrate_tokens = 10\nrate_period_ns = 1_000_000_000\n"
    "receipt_lifetime_ns = 60_000_000_000\n"
)
ROOT = Path(__file__).resolve().parents[2]


def decision(index: int, time: int = BASE, user: str = "alice") -> dict[str, object]:
    return {
        "kind": "decision",
        "deduction_id": f"d{index}",
        "user_id": user,
        "decision_ns": time,
        "result": "allowed",
    }


def burst(count: int = 21, time: int = BASE) -> list[dict[str, object]]:
    return [decision(i, time) for i in range(1, count + 1)]


def release(deduction: str = "d1") -> dict[str, object]:
    return {"kind": "release", "deduction_id": deduction, "release_mono_ns": 999_999}


def refund(
    result: str = "applied", attempts: int = 1, time: int = BASE, deduction: str = "d1"
) -> dict[str, object]:
    return {
        "kind": "refund",
        "deduction_id": deduction,
        "result": result,
        "reason": "retention" if result == "unresolved" else "",
        "attempts": attempts,
        "credit_decision_ns": time if result in {"applied", "already_applied"} else 0,
        "release_mono_ns": 123,
        "final_mono_ns": 456,
    }


def terminal(event: str = "stop", drops: int = 0, read_ok: bool = True) -> dict[str, object]:
    record: dict[str, object] = {
        "kind": "lifecycle",
        "event": event,
        "listener_drops": drops,
        "log_drops": 0,
    }
    if event == "pre_kill":
        record["read_ok"] = read_ok
    return record


def write_run(path: Path, files: list[list[dict[str, object]]], config: str = CONFIG) -> None:
    path.joinpath("config.toml").write_text(config, encoding="utf-8")
    for instance, records in enumerate(files, 1):
        with path.joinpath(f"api-{instance}.jsonl").open("w", encoding="utf-8") as stream:
            for record in records:
                stream.write(json.dumps(record) + "\n")


def output(capsys: pytest.CaptureFixture[str]) -> dict[str, object]:
    text = capsys.readouterr().out
    assert text.startswith("{"), text
    value = json.loads(text)
    assert isinstance(value, dict)
    return value


def expected(
    verdict: str = "pass",
    known: int = 0,
    unknown: int = 0,
    drops: int = 0,
    kills: int = 0,
    failed_reads: int = 0,
    reasons: list[str] | None = None,
    violation: dict[str, object] | None = None,
) -> dict[str, object]:
    return {
        "verdict": verdict,
        "known_credits": known,
        "unknown_credits": unknown,
        "listener_drops": drops,
        "pre_kill_lines": kills,
        "pre_kill_read_ok_false": failed_reads,
        "invalid_reasons": reasons or [],
        "violation": violation,
    }


def test_burst_plus_refill(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    records = burst(20) + [decision(i, BASE + SECOND) for i in range(21, 31)]
    write_run(tmp_path, [[*records, terminal()]])
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected()


def test_one_over_capacity(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    write_run(tmp_path, [[*burst(), terminal()]])
    assert main([str(tmp_path)]) == 1
    assert output(capsys) == expected(
        "violation",
        violation={
            "user_id": "alice",
            "start_ns": BASE,
            "end_ns": BASE,
            "excess_units": SECOND,
        },
    )


@pytest.mark.parametrize("result", ["applied", "already_applied"])
def test_known_credit_at_bound(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], result: str
) -> None:
    # The listener may write before the release record; joins use the entire file.
    write_run(tmp_path, [[refund(result), *burst(), release(), terminal()]])
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected(known=1)


@pytest.mark.parametrize("result", ["receipt_expired", "unresolved"])
@pytest.mark.parametrize(
    "offset,code",
    [(0, 0), (-SECOND, 0), (-LIFETIME + 1000, 0), (-LIFETIME, 1), (-LIFETIME - 1000, 1)],
)
def test_unknown_interval(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], result: str, offset: int, code: int
) -> None:
    # A receipt at the burst is one of the 21 allows; an earlier receipt is
    # separated by refill. At expiry exactly, the half-open interval adds zero.
    records = burst()
    if offset:
        records.insert(0, decision(100, BASE + offset))
    deduction = "d100" if offset else "d1"
    write_run(
        tmp_path, [[*records, release(deduction), refund(result, deduction=deduction), terminal()]]
    )
    assert main([str(tmp_path)]) == code
    value = output(capsys)
    assert value["unknown_credits"] == 1
    assert value["known_credits"] == 0
    assert value["verdict"] == ("pass" if code == 0 else "violation")


@pytest.mark.parametrize(
    "result,attempts", [("unresolved", 0), ("receipt_invalid", 0), ("receipt_invalid", 1)]
)
def test_no_credit(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], result: str, attempts: int
) -> None:
    write_run(tmp_path, [[*burst(), release(), refund(result, attempts), terminal()]])
    assert main([str(tmp_path)]) == 1
    value = output(capsys)
    assert value["known_credits"] == value["unknown_credits"] == 0


def test_degraded_adds_nothing(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    degraded = dict(decision(0), result="degraded", deduction_id="", decision_ns=0)
    write_run(tmp_path, [[*burst(20, time=0), degraded, release(""), terminal()]])
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected()


@pytest.mark.parametrize("credit_offset,code", [(0, 0), (1000, 1), (-1000, 1)])
def test_credit_uses_budget_time(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], credit_offset: int, code: int
) -> None:
    # The receipt was issued earlier. Only the original credit's budget time
    # in the frozen burst counts; the release's monotonic time cannot substitute.
    records = [
        decision(100, BASE - SECOND),
        *burst(),
        release("d100"),
        refund("already_applied", time=BASE + credit_offset, deduction="d100"),
        terminal(),
    ]
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == code
    assert output(capsys)["known_credits"] == 1


@pytest.mark.parametrize("later_kill", [True, False])
@pytest.mark.parametrize("restart", [True, False])
def test_open_release_requires_later_kill(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], later_kill: bool, restart: bool
) -> None:
    tail = [release(), terminal("pre_kill")] if later_kill else [terminal("pre_kill"), release()]
    records = [golden_records()[0], *burst(), *tail]
    if restart:
        records.extend([golden_records()[0], terminal()])
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected(unknown=1, kills=1)


def test_two_killed_segments(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    start = golden_records()[0]
    write_run(
        tmp_path, [[start, terminal("pre_kill"), start, *burst(), release(), terminal("pre_kill")]]
    )
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected(unknown=1, kills=2)


def test_final_after_pre_kill_and_restart(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    start = golden_records()[0]
    records: list[dict[str, object]] = [
        start,
        *burst(),
        release(),
        terminal("pre_kill"),
        refund("receipt_invalid"),
        start,
        {"kind": "lifecycle", "event": "ready"},
        terminal(),
    ]
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 1
    value = output(capsys)
    assert value["unknown_credits"] == 0  # A final event overrides the kill heuristic.
    assert value["pre_kill_lines"] == 1


def test_restart_release_needs_its_own_later_kill(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    write_run(
        tmp_path, [[terminal("pre_kill"), golden_records()[0], *burst(), release(), terminal()]]
    )
    assert main([str(tmp_path)]) == 2
    assert output(capsys)["unknown_credits"] == 0


@pytest.mark.parametrize("event", ["stop", "pre_kill"])
def test_listener_drop_invalidates(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], event: str
) -> None:
    write_run(tmp_path, [[*burst(), terminal(event, drops=1)]])
    assert main([str(tmp_path)]) == 2
    assert output(capsys) == expected(
        "invalid", drops=1, kills=int(event == "pre_kill"), reasons=["listener_drops"]
    )


def test_all_terminal_counts_summed(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    write_run(
        tmp_path,
        [
            [terminal("pre_kill", 2, False), terminal("pre_kill", 3), terminal("stop", 4)],
            [terminal("stop", 5), terminal("pre_kill", 6, False)],
        ],
    )
    assert main([str(tmp_path)]) == 2
    assert output(capsys) == expected(
        "invalid",
        drops=20,
        kills=3,
        failed_reads=2,
        reasons=["listener_drops", "pre_kill_read_ok_false"],
    )


def test_failed_pre_kill_read(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    write_run(tmp_path, [[*burst(20), terminal("pre_kill", read_ok=False)]])
    assert main([str(tmp_path)]) == 2
    assert output(capsys) == expected(
        "invalid", kills=1, failed_reads=1, reasons=["pre_kill_read_ok_false"]
    )


@pytest.mark.parametrize("event", ["stop", "pre_kill"])
def test_log_drops_invalid(tmp_path: Path, capsys: pytest.CaptureFixture[str], event: str) -> None:
    write_run(tmp_path, [[dict(terminal(event), log_drops=1)]])
    assert main([str(tmp_path)]) == 2
    assert output(capsys)["invalid_reasons"] == ["log_drops"]


@pytest.mark.parametrize("records", [[], [decision(1)], [terminal("pre_kill")]])
def test_terminal_evidence(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], records: list[dict[str, object]]
) -> None:
    write_run(tmp_path, [records])
    code = 0 if records and records[0].get("event") == "pre_kill" else 2
    assert main([str(tmp_path)]) == code
    assert output(capsys)["verdict"] == ("pass" if code == 0 else "invalid")


def test_restart_without_terminal_invalid(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    write_run(tmp_path, [[terminal("pre_kill"), golden_records()[0]]])
    assert main([str(tmp_path)]) == 2
    assert output(capsys)["invalid_reasons"] == ["api-1.jsonl: line 2: missing stop or pre_kill"]


def test_each_segment_needs_terminal(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    start = golden_records()[0]
    ready: dict[str, object] = {"kind": "lifecycle", "event": "ready"}
    records = [start, ready, *burst(5), start, ready]
    records.extend([decision(i) for i in range(6, 11)])
    records.append(terminal())
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 2
    assert output(capsys) == expected(
        "invalid", reasons=["api-1.jsonl: line 1: missing stop or pre_kill"]
    )


def test_shared_key_across_instances(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    write_run(tmp_path, [[*burst(20), terminal()], [decision(21), terminal()]])
    assert main([str(tmp_path)]) == 1
    assert output(capsys)["violation"] == {
        "user_id": "alice",
        "start_ns": BASE,
        "end_ns": BASE,
        "excess_units": SECOND,
    }


def test_independent_keys(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    records = burst(20) + [decision(i, user="bob") for i in range(21, 41)]
    write_run(tmp_path, [[*records, terminal()]])
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected()


@pytest.mark.parametrize("result", ["applied", "receipt_expired", "unresolved"])
def test_credit_stays_with_its_key(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], result: str
) -> None:
    records = [
        decision(100, user="bob"),
        *burst(),
        release("d100"),
        refund(result, deduction="d100"),
        terminal(),
    ]
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 1
    assert output(capsys)["violation"] == {
        "user_id": "alice",
        "start_ns": BASE,
        "end_ns": BASE,
        "excess_units": SECOND,
    }


def golden_records() -> list[dict[str, object]]:
    with ROOT.joinpath("harness/runlog/testdata/golden/api.jsonl").open(encoding="utf-8") as stream:
        return [json.loads(line) for line in stream]


@pytest.mark.parametrize("change,code", [("none", 0), ("read_ok", 2), ("decision_ns", 2)])
def test_reads_golden_runlog(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], change: str, code: int
) -> None:
    records = golden_records()
    decision_line = 0
    for line, record in enumerate(records, 1):
        if change == "read_ok" and record.get("event") == "pre_kill":
            record["read_ok"] = False
        if change == "decision_ns" and record["kind"] == "decision":
            del record["decision_ns"]
            decision_line = line
    write_run(tmp_path, [records], ROOT.joinpath("harness.toml").read_text(encoding="utf-8"))
    assert main([str(tmp_path)]) == code
    if change == "decision_ns":
        text = capsys.readouterr().out
        assert "api-1.jsonl" in text
        assert f"line {decision_line}: field 'decision_ns':" in text
    else:
        assert output(capsys) == expected(
            "invalid" if code else "pass",
            known=1,
            kills=1,
            failed_reads=int(code == 2),
            reasons=["pre_kill_read_ok_false"] if code else [],
        )


SHAPES = [(index, field) for index, record in enumerate(golden_records()) for field in record]


@pytest.mark.parametrize("index,field", SHAPES)
@pytest.mark.parametrize("change", ["missing", "wrong_type"])
def test_every_golden_field_validated(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], index: int, field: str, change: str
) -> None:
    records = golden_records()
    if change == "missing":
        del records[index][field]
    else:
        records[index][field] = []
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert "api-1.jsonl" in text
    assert f"line {index + 1}: field {field!r}:" in text


@pytest.mark.parametrize("index", range(8))
def test_unexpected_fields_rejected(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], index: int
) -> None:
    records = golden_records()
    records[index]["extra"] = 1
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 2
    assert f"line {index + 1}: field 'extra':" in capsys.readouterr().out


@pytest.mark.parametrize(
    "field", ["capacity", "rate_tokens", "rate_period_ns", "receipt_lifetime_ns"]
)
@pytest.mark.parametrize("change", ["missing", "boolean", "negative", "zero", "string"])
def test_required_policy_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, change: str
) -> None:
    lines = CONFIG.splitlines()
    line = next(i for i, text in enumerate(lines, 1) if text.startswith(field))
    values = {"boolean": "true", "negative": "-1", "zero": "0", "string": '"1"'}
    if change == "missing":
        del lines[line - 1]
    else:
        lines[line - 1] = f"{field} = {values[change]}"
    write_run(tmp_path, [[terminal()]], "\n".join(lines))
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert "config.toml" in text and f"field {field!r}:" in text
    assert f"line {1 if change == 'missing' else line}:" in text


@pytest.mark.parametrize(
    "raw,field",
    [
        ("", "policy"),
        ("[instance]\ndrain_timeout_ns = 1_000_000\n", "policy"),
        ("[policy", "toml"),
        ("[policy]\ncapacity = ", "toml"),
        (CONFIG + "unknown = 1\n", "unknown"),
        (CONFIG + 'failure_mode = "neither"\n', "failure_mode"),
        (CONFIG + 'clock_source = "monotonic"\n', "clock_source"),
        (CONFIG.replace("10\n", "2147483648\n"), "rate_tokens"),
        (CONFIG.replace("20\n", "4503599627370496\n"), "capacity"),
        (CONFIG.replace("1_000_000_000", "1001"), "rate_period_ns"),
        (CONFIG.replace("60_000_000_000", "1099511627776000"), "receipt_lifetime_ns"),
    ],
)
def test_invalid_config(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], raw: str, field: str
) -> None:
    write_run(tmp_path, [[terminal()]], raw)
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert "config.toml:" in text and f"field {field!r}:" in text
    if field != "toml":
        assert "config.toml: line " in text


@pytest.mark.parametrize("count", [20, 21])
@pytest.mark.parametrize(
    "extra",
    [
        "[instance]\ndrain_timeout_ns = 1_000_000\nunknown = true\n",
        "[instance]\nmargin_ns = 1\n",
        "[api]\nunknown = 'ignored'\n",
        "[api]\nlimiter_enabled = 1\n",
        "[api]\nrefuse_after_acquire_bp = 10001\n",
        "[store]\naddr = 1\n",
        "[other]\nunknown = [1, 'ignored', true]\n",
    ],
)
def test_non_policy_config_ignored(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], count: int, extra: str
) -> None:
    records = [[*burst(count), terminal()]]
    write_run(tmp_path, records)
    code = main([str(tmp_path)])
    assert code == (0 if count == 20 else 1)
    verdict = output(capsys)
    write_run(tmp_path, records, CONFIG + extra)
    assert main([str(tmp_path)]) == code
    assert output(capsys) == verdict


@pytest.mark.parametrize("value", [True, -1000, 1, TBOUND, "123", 1.0])
def test_decision_budget_time(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], value: object
) -> None:
    write_run(tmp_path, [[dict(decision(1), decision_ns=value), terminal()]])
    assert main([str(tmp_path)]) == 2
    assert "api-1.jsonl: line 1: field 'decision_ns':" in capsys.readouterr().out


@pytest.mark.parametrize(
    "field,value",
    [
        ("result", "limited"),
        ("deduction_id", ""),
        ("user_id", ""),
        ("user_id", "x" * 257),
        ("user_id", "\ud800"),
    ],
)
def test_invalid_decision(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object
) -> None:
    record = decision(1)
    record[field] = value
    write_run(tmp_path, [[record, terminal()]])
    assert main([str(tmp_path)]) == 2
    assert f"line 1: field {field!r}:" in capsys.readouterr().out


@pytest.mark.parametrize(
    "field,value",
    [
        ("credit_decision_ns", True),
        ("credit_decision_ns", 1),
        ("credit_decision_ns", BASE - 1000),
        ("credit_decision_ns", BASE + LIFETIME),
        ("credit_decision_ns", TBOUND),
        ("attempts", -1),
        ("attempts", 0),
        ("reason", "closed"),
        ("result", "other"),
        ("final_mono_ns", 122),
    ],
)
def test_invalid_refund(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object
) -> None:
    final = refund()
    final[field] = value
    write_run(tmp_path, [[decision(1), release(), final, terminal()]])
    assert main([str(tmp_path)]) == 2
    assert f"line 3: field {field!r}:" in capsys.readouterr().out


@pytest.mark.parametrize("result", ["receipt_invalid", "receipt_expired", "unresolved"])
def test_unknown_credit_placeholder(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], result: str
) -> None:
    final = dict(refund(result), credit_decision_ns=BASE)
    write_run(tmp_path, [[decision(1), release(), final, terminal()]])
    assert main([str(tmp_path)]) == 2
    assert "line 3: field 'credit_decision_ns':" in capsys.readouterr().out


@pytest.mark.parametrize(
    "tail",
    [
        [release("missing")],
        [refund()],
        [release(), release()],
        [release(), refund(), refund()],
    ],
)
def test_invalid_joins(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], tail: list[dict[str, object]]
) -> None:
    write_run(tmp_path, [[decision(1), *tail, terminal()]])
    assert main([str(tmp_path)]) == 2
    if len(tail) == 1:
        value = output(capsys)
        assert value["verdict"] == "invalid"
        text = str(value["invalid_reasons"])
    else:
        text = capsys.readouterr().out
    assert "api-1.jsonl: line " in text and "deduction_id" in text


@pytest.mark.parametrize("cross_file", [False, True])
def test_deduction_ids_unique(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], cross_file: bool
) -> None:
    files = (
        [[decision(1), terminal()], [decision(1), terminal()]]
        if cross_file
        else [[decision(1), decision(1), terminal()]]
    )
    write_run(tmp_path, files)
    assert main([str(tmp_path)]) == 2
    assert "field 'deduction_id': duplicate deduction identity" in capsys.readouterr().out


def test_refunds_cannot_join_other_instance(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    write_run(tmp_path, [[decision(1), terminal()], [release(), refund(), terminal()]])
    assert main([str(tmp_path)]) == 2
    value = output(capsys)
    assert value["verdict"] == "invalid"
    assert "api-2.jsonl: line 1: field 'deduction_id':" in str(value["invalid_reasons"])


@pytest.mark.parametrize("raw", ["\n", "{\n", "[]\n", "null\n", '{"kind":"other"}\n'])
def test_bad_json_line(tmp_path: Path, capsys: pytest.CaptureFixture[str], raw: str) -> None:
    write_run(tmp_path, [[terminal()]])
    with tmp_path.joinpath("api-1.jsonl").open("a", encoding="utf-8") as stream:
        stream.write(raw)
    assert main([str(tmp_path)]) == 2
    assert "api-1.jsonl: line 2: field " in capsys.readouterr().out


def test_request_does_not_duplicate_allow(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    request = golden_records()[4]
    request.update(decision_ns=BASE, deduction_id="d1")
    write_run(tmp_path, [[*burst(20), request, terminal()]])
    assert main([str(tmp_path)]) == 0
    assert output(capsys) == expected()


@pytest.mark.parametrize("filename", ["api-0.jsonl", "api-01.jsonl", "api-x.jsonl", "api-2.jsonl"])
def test_invalid_instance_filenames(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], filename: str
) -> None:
    write_run(tmp_path, [[terminal()]])
    tmp_path.joinpath("api-1.jsonl").rename(tmp_path.joinpath(filename))
    assert main([str(tmp_path)]) == 2
    assert "line 1: field " in capsys.readouterr().out


def test_missing_run_inputs(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    assert main([str(tmp_path)]) == 2
    assert "config.toml: line 1: field 'config':" in capsys.readouterr().out
    tmp_path.joinpath("config.toml").write_text(CONFIG, encoding="utf-8")
    assert main([str(tmp_path)]) == 2
    assert "field 'api-N.jsonl':" in capsys.readouterr().out


@pytest.mark.parametrize(
    "raw,field",
    [
        (b'{"kind":"lifecycle","event":"ready","event":"stop"}\n', "event"),
        (b'{"kind":"decision","decision_ns":NaN}\n', "json"),
        (b'{"kind":"decision","decision_ns":Infinity}\n', "json"),
        (b"\xff\n", "json"),
    ],
)
def test_json_encoding_and_duplicate_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], raw: bytes, field: str
) -> None:
    write_run(tmp_path, [[*burst(), terminal()]])
    with tmp_path.joinpath("api-1.jsonl").open("ab") as stream:
        stream.write(raw)
    assert main([str(tmp_path)]) == 2
    assert f"api-1.jsonl: line 23: field {field!r}:" in capsys.readouterr().out


@pytest.mark.parametrize(
    "event,value,field",
    [
        ("stop", True, "listener_drops"),
        ("pre_kill", 0, "read_ok"),
        ("start", 1, "queue_len"),
        ("request", 2, "instance"),
        ("request", 600, "http_status"),
        ("decision", 1000, "decision_ns"),
        ("decision", "d1", "deduction_id"),
    ],
)
def test_additional_record_constraints(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], event: str, value: object, field: str
) -> None:
    records = golden_records()
    for line, record in enumerate(records, 1):
        assert line >= 1
        if record.get("event", record["kind"]) == event:
            if event == "decision":
                record.update(result="degraded", deduction_id="", decision_ns=0)
            record[field] = value
            break
    assert line >= 1
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 2
    assert f"api-1.jsonl: line {line}: field {field!r}:" in capsys.readouterr().out


def test_missing_final_without_kill_invalid(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    write_run(tmp_path, [[*burst(), release(), terminal()]])
    assert main([str(tmp_path)]) == 2
    assert output(capsys) == expected(
        "invalid", reasons=["api-1.jsonl: line 22: release without final event or segment pre_kill"]
    )


def test_known_final_after_kill_counted_once(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    write_run(tmp_path, [[*burst(22), release(), terminal("pre_kill"), refund()]])
    assert main([str(tmp_path)]) == 1
    assert output(capsys) == expected(
        "violation",
        known=1,
        kills=1,
        violation={
            "user_id": "alice",
            "start_ns": BASE,
            "end_ns": BASE,
            "excess_units": SECOND,
        },
    )


def test_runlogs_stream_one_decode_per_line(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    from io import BytesIO
    from typing import Any

    records = [*burst(20), terminal()]
    write_run(tmp_path, [records])
    calls = 0
    original_loads = json.loads
    original_open = Path.open

    class IterationOnly(BytesIO):
        def read(self, size: int | None = -1) -> bytes:
            raise AssertionError("run log must be iterated line by line")

        def readlines(self, hint: int | None = -1) -> list[bytes]:
            raise AssertionError("run log must not be materialized")

    def tracked_loads(raw: str, **kwargs: Any) -> object:
        nonlocal calls
        calls += 1
        return original_loads(raw, **kwargs)

    def iteration_open(path: Path, *args: Any, **kwargs: Any) -> Any:
        if path.suffix == ".jsonl":
            return IterationOnly(b"".join((json.dumps(r) + "\n").encode() for r in records))
        return original_open(path, *args, **kwargs)

    with monkeypatch.context() as patch:
        patch.setattr(json, "loads", tracked_loads)
        patch.setattr(Path, "open", iteration_open)
        assert main([str(tmp_path)]) == 0
    assert calls == 21
    assert output(capsys) == expected()


@pytest.mark.parametrize("missing", ["decision", "release", "both"])
def test_dropped_join_evidence_json(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], missing: str
) -> None:
    records = [decision(1), release(), refund(), dict(terminal(), log_drops=1)]
    if missing in {"decision", "both"}:
        records = [record for record in records if record["kind"] != "decision"]
    if missing in {"release", "both"}:
        records = [record for record in records if record["kind"] != "release"]
    write_run(tmp_path, [records])
    assert main([str(tmp_path)]) == 2
    value = output(capsys)
    assert value["verdict"] == "invalid"
    reasons = value["invalid_reasons"]
    assert isinstance(reasons, list)
    assert "log_drops" in reasons
    line = 2 if missing == "release" else 1
    assert any(f"api-1.jsonl: line {line}: field 'deduction_id':" in reason for reason in reasons)
    assert any(
        ("no allowed decision" if missing != "release" else "final event without release") in reason
        for reason in reasons
    )
    assert all(reason.startswith("api-1.jsonl: ") for reason in reasons if reason != "log_drops")


@pytest.mark.parametrize("kind", ["integer", "nesting"])
@pytest.mark.parametrize("source", ["json", "toml"])
def test_oversized_or_nested_input(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], kind: str, source: str
) -> None:
    value = "9" * 5000 if kind == "integer" else "[" * 10000 + "0" + "]" * 10000
    write_run(tmp_path, [[terminal()]])
    if source == "json":
        path = tmp_path.joinpath("api-1.jsonl")
        with path.open("a", encoding="utf-8") as stream:
            stream.write('{"kind":"decision","decision_ns":' + value + "}\n")
        field = "json"
    else:
        path = tmp_path.joinpath("config.toml")
        path.write_text("[policy]\ncapacity = " + value + "\n", encoding="utf-8")
        field = "toml"
    try:
        code = main([str(tmp_path)])
    except (ValueError, RecursionError) as error:
        raise AssertionError("malformed input escaped main") from error
    assert code == 2
    text = capsys.readouterr().out
    assert f"{path.name}: line 2: field {field!r}:" in text


@pytest.mark.parametrize("duplicate", [False, True])
def test_surrogate_member_name(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], duplicate: bool
) -> None:
    write_run(tmp_path, [[terminal()]])
    field = "\ud800"
    raw = '{"kind":"lifecycle","event":"ready",' + json.dumps(field) + ":0"
    if duplicate:
        raw += "," + json.dumps(field) + ":1"
    with tmp_path.joinpath("api-1.jsonl").open("a", encoding="utf-8") as stream:
        stream.write(raw + "}\n")
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert f"api-1.jsonl: line 2: field {field!r}:" in text
    assert text.encode("utf-8")


@pytest.mark.parametrize("quote", ['"', "'"])
def test_quoted_config_key_location(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], quote: str
) -> None:
    config = CONFIG.replace("capacity = 20", f"{quote}capacity{quote} = true")
    write_run(tmp_path, [[terminal()]], config)
    assert main([str(tmp_path)]) == 2
    assert "config.toml: line 2: field 'capacity':" in capsys.readouterr().out


@pytest.mark.parametrize("field", ["listener_drops", "log_drops"])
@pytest.mark.parametrize("count", [str(2**63), "9" * 4300])
def test_bounded_drop_counts(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, count: str
) -> None:
    write_run(tmp_path, [[terminal()]])
    path = tmp_path.joinpath("api-1.jsonl")
    raw = json.dumps(dict(terminal(), **{field: "COUNT"})).replace('"COUNT"', count)
    path.write_text(raw + "\n" + raw + "\n", encoding="utf-8")
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert f"api-1.jsonl: line 1: field {field!r}:" in text
    assert "Traceback" not in text


@pytest.mark.parametrize("field", ["listener_drops", "log_drops"])
def test_maximum_drop_counts(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str
) -> None:
    record = dict(terminal(), **{field: 2**63 - 1})
    write_run(tmp_path, [[record, record]])
    assert main([str(tmp_path)]) == 2
    assert output(capsys) == expected(
        "invalid", drops=2 * (2**63 - 1) if field == "listener_drops" else 0, reasons=[field]
    )


@pytest.mark.parametrize("source", ["api-1.jsonl", "config.toml"])
def test_invalid_utf8_line_two(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], source: str
) -> None:
    write_run(tmp_path, [[terminal()]])
    path = tmp_path.joinpath(source)
    path.write_bytes(path.read_bytes().splitlines()[0] + b"\n\xff\n")
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert f"{source}: line 2: field " in text
    assert "Traceback" not in text


@pytest.mark.parametrize("quote", ['"""', "'''"])
def test_multiline_config_string(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], quote: str
) -> None:
    config = (
        "[api]\ndb_dsn = "
        + quote
        + "\n² = 0\n[policy]\ncapacity = true\n"
        + quote
        + "\n"
        + CONFIG.replace("capacity = 20", "capacity = true")
    )
    write_run(tmp_path, [[terminal()]], config)
    assert main([str(tmp_path)]) == 2
    assert "config.toml: line 8: field 'capacity':" in capsys.readouterr().out


@pytest.mark.parametrize("digits", ["999", "9" * 5000], ids=["fake-line", "huge-fake-line"])
def test_config_syntax_line_ignores_table_name(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], digits: str
) -> None:
    header = f'["at line {digits}"]\n'
    write_run(tmp_path, [[terminal()]], header + "\n" + header)
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    assert "config.toml: line 3: field 'toml':" in text
    assert "Traceback" not in text


@pytest.mark.parametrize(
    "message,parser_line,expected_line",
    [
        ("bad (at line 3, column 1)", None, 3),
        ("bad (at line 3, column 1) extra", None, None),
        ("bad (at end of document)", None, None),
        ("bad (at line 1000000001, column 1)", None, None),
        ("bad (at line " + "9" * 5000 + ", column 1)", None, None),
        ("bad (at line 999, column 1)", 3, 3),
        ("bad (at line 3, column 1)", 1000000001, None),
        ("bad (at line 3, column 1)", "unparsable", None),
    ],
    ids=[
        "suffix",
        "not-suffix",
        "eof",
        "absurd",
        "huge",
        "attribute",
        "absurd-attribute",
        "bad-attribute",
    ],
)
def test_config_syntax_parser_location(
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
    message: str,
    parser_line: int | str | None,
    expected_line: int | None,
) -> None:
    write_run(tmp_path, [[terminal()]])
    error = tomllib.TOMLDecodeError(message)
    if parser_line is not None:
        monkeypatch.setattr(error, "lineno", parser_line, raising=False)

    def fail_parse(source: str) -> dict[str, object]:
        raise error

    monkeypatch.setattr(tomllib, "loads", fail_parse)
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    location = "config.toml" if expected_line is None else f"config.toml: line {expected_line}"
    assert f"{location}: field 'toml':" in text
    assert "Traceback" not in text


@pytest.mark.parametrize("quote", ['"', "'"])
def test_config_hash_in_quoted_key(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], quote: str
) -> None:
    write_run(tmp_path, [[terminal()]], CONFIG + f"{quote}cap#x{quote} = 1 # comment\n")
    assert main([str(tmp_path)]) == 2
    assert "config.toml: line 6: field 'cap#x':" in capsys.readouterr().out


@pytest.mark.parametrize("quote", ['"', "'"])
def test_quoted_config_table_location(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], quote: str
) -> None:
    config = CONFIG.replace("[policy]", f"[{quote}policy{quote}] # comment").replace(
        "capacity = 20", "capacity = true"
    )
    write_run(tmp_path, [[terminal()]], config)
    assert main([str(tmp_path)]) == 2
    assert "config.toml: line 2: field 'capacity':" in capsys.readouterr().out


@pytest.mark.parametrize(
    "config",
    [
        "policy.capacity = true\n",
        "policy = {capacity = true}\n",
        "[policy]\ncapacity = [\n'capacity = true',\n]\n",
    ],
)
def test_config_location_omits_ambiguous_line(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], config: str
) -> None:
    write_run(tmp_path, [[terminal()]], config)
    assert main([str(tmp_path)]) == 2
    text = capsys.readouterr().out
    if config.startswith("[policy]"):
        assert "config.toml: line 2: field 'capacity':" in text
    else:
        assert "config.toml: key policy.capacity: field 'capacity':" in text
        assert ": line " not in text
