"""Malformed evidence (AGENTS.md §4; IMPLEMENTATION_PLAN H5 step 2): each defect is an InputError
naming the file, the line and the field, the build exits 1 and writes no report; and the report
files are replaced together or not at all. Expected messages are written by hand."""

import json
from pathlib import Path

import pytest

from refmodel.model import TBOUND
from report import InputError
from report.__main__ import main
from report.build import build, write_report
from report.synthetic import (
    SECOND,
    T0_WALL_NS,
    RunSpec,
    attempt,
    entry,
    file_sha256,
    lifecycle,
    lifecycle_start,
    request,
    sample,
    summary,
    write_run,
)

MS = 1_000_000
DECIDED = T0_WALL_NS + 6 * SECOND  # the deduction's budget time (µs × 1,000)


def api_lines() -> list[dict[str, object]]:
    """Lines 1-7 of api-1.jsonl: start, ready, an allowed request whose DB write failed, its
    decision, release and applied refund, stop."""
    return [
        lifecycle_start(),
        lifecycle("ready"),
        request(
            1,
            "r000000000",
            arrival_ns=T0_WALL_NS + 6 * SECOND,
            status=503,
            error="database_unavailable",
            decision_ns=DECIDED,
            deduction_id="d1",
        ),
        {
            "kind": "decision",
            "deduction_id": "d1",
            "user_id": "u000000",
            "decision_ns": DECIDED,
            "result": "allowed",
        },
        {"kind": "release", "deduction_id": "d1", "release_mono_ns": 9 * SECOND},
        {
            "kind": "refund",
            "deduction_id": "d1",
            "result": "applied",
            "reason": "",
            "attempts": 1,
            "credit_decision_ns": DECIDED + 1 * MS,
            "release_mono_ns": 9 * SECOND,
            "final_mono_ns": 9 * SECOND + 7 * MS,
        },
        lifecycle("stop"),
    ]


def make_run(
    tmp_path: Path,
    *,
    gen: list[dict[str, object] | str] | None = None,
    api: list[dict[str, object] | str] | None = None,
    samples: list[dict[str, object] | str] | None = None,
) -> Path:
    """A one-instance, one-step test run: one answered request planned at 6 s."""
    spec = RunSpec(name="malformed", instances=1, steps=[(1.0, 10.0)])
    return write_run(
        tmp_path,
        spec,
        schedule=[entry(0, 6 * SECOND)],
        gen=gen if gen is not None else [attempt(0, 6 * SECOND), summary(1)],
        apis={1: api if api is not None else api_lines()},
        samples=samples if samples is not None else [sample(T0_WALL_NS + SECOND)],
    )


def assert_refused(run_dir: Path, message: str, capsys: pytest.CaptureFixture[str]) -> None:
    """build() raises an InputError matching message (a regular expression); python3 -m report
    exits 1 printing it and leaves no report file, temporary or not."""
    with pytest.raises(InputError, match=message) as caught:
        build(run_dir)
    assert main([str(run_dir)]) == 1
    assert str(caught.value) in capsys.readouterr().err
    assert sorted(p.name for p in run_dir.glob("report.*")) == []


def text_line(value: dict[str, object], old: str, new: str) -> str:
    """value as one JSON line with old (which must occur once) replaced by new: a literal JSON
    itself cannot write, such as 1e400 or a lone surrogate escape."""
    line = json.dumps(value)
    assert line.count(old) == 1, old
    return line.replace(old, new) + "\n"


def with_field(value: dict[str, object], key: str, item: object) -> dict[str, object]:
    out = dict(value)
    out[key] = item
    return out


@pytest.mark.parametrize(
    ("file", "line", "key", "value"),
    [
        ("gen", 1, "latency", 2**63),
        ("gen", 1, "latency", 10**30),
        ("gen", 1, "sent_ns", 2**64),
        ("gen", 1, "got_conn_wait_ns", 2**63),
        ("gen", 1, "planned_ns", 2**70),
        ("api", 3, "limiter_ns", 2**63),
        ("api", 3, "db_ns", 2**63),
        ("api", 3, "total_ns", 10**25),
        ("api", 6, "final_mono_ns", 2**64),
        ("api", 5, "release_mono_ns", 2**63),
    ],
)
def test_int64_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], file: str, line: int, key: str, value: int
) -> None:
    """An integer the report keeps in an int64 sample array (or one that makes such a sample: a
    lag, a refund time) beyond 2^63 − 1 is refused, naming the field."""
    if file == "gen":
        gen: list[dict[str, object] | str] = [
            with_field(attempt(0, 6 * SECOND), key, value),
            summary(1),
        ]
        run_dir = make_run(tmp_path, gen=gen)
        where = "gen\\.jsonl"
    else:
        api: list[dict[str, object] | str] = list(api_lines())
        record = api[line - 1]
        assert isinstance(record, dict)
        api[line - 1] = with_field(record, key, value)
        run_dir = make_run(tmp_path, api=api)
        where = "api-1\\.jsonl"
    assert_refused(run_dir, f"^{where}: line {line}: field {key}: ", capsys)


def scenario_path(run_dir: Path) -> Path:
    ctx = json.loads(run_dir.joinpath("context.json").read_text(encoding="utf-8"))
    return Path(ctx["scenario"]["path"])


def rewrite_scenario(run_dir: Path, old: str, new: str) -> None:
    """Changes the run's scenario file, and context.json's hash of it to match."""
    path = scenario_path(run_dir)
    text = path.read_text(encoding="utf-8")
    assert text.count(old) == 1, old
    path.write_text(text.replace(old, new), encoding="utf-8")
    ctx_path = run_dir.joinpath("context.json")
    ctx = json.loads(ctx_path.read_text(encoding="utf-8"))
    ctx["scenario"]["sha256"] = file_sha256(path)
    ctx_path.write_text(json.dumps(ctx), encoding="utf-8")


def rewrite_text(path: Path, old: str, new: str) -> None:
    text = path.read_text(encoding="utf-8")
    assert text.count(old) == 1, old
    path.write_text(text.replace(old, new), encoding="utf-8")


@pytest.mark.parametrize(
    "case",
    [
        "cpu 1e400",
        "metric 1e400",
        "context number 1e400",
        "context integer beyond a float",
        "policy inf",
        "scenario rate inf",
        "scenario rewrite nan",
        "scenario duration integer beyond a float",
    ],
)
def test_numbers_beyond_a_float(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], case: str
) -> None:
    """A JSON number beyond a float (1e400 reads as infinity), an integer too large for one, and
    a TOML inf or nan are refused, so report.json holds only finite numbers."""
    if case == "cpu 1e400":
        line = text_line(sample(T0_WALL_NS + SECOND), '"cpu": 5.5', '"cpu": 1e400')
        run_dir = make_run(tmp_path, samples=[line])
        message = r"^samples\.jsonl: line 1: field json: 1e400 is beyond a float"
    elif case == "metric 1e400":
        old = '"rl_pending_refunds": 0'
        line = text_line(sample(T0_WALL_NS + SECOND), old, '"rl_pending_refunds": 1e400')
        run_dir = make_run(tmp_path, samples=[line])
        message = r"^samples\.jsonl: line 1: field json: 1e400 is beyond a float"
    elif case == "context number 1e400":
        run_dir = make_run(tmp_path)
        rewrite_text(run_dir.joinpath("context.json"), '"ncpu": 16', '"ncpu": 1e400')
        message = r"^context\.json: field json: 1e400 is beyond a float"
    elif case == "context integer beyond a float":
        spec_fault = {
            "at_s": 1,
            "cmd": "faultctl store pause 4.5s",
            "instance": None,
            "start_ns": 0,
            "end_ns": 0,
            "outage_start_ns": None,
            "outage_ns": None,
        }
        run_dir = make_run(tmp_path)
        ctx_path = run_dir.joinpath("context.json")
        ctx = json.loads(ctx_path.read_text(encoding="utf-8"))
        ctx["faults"] = [spec_fault]
        ctx_path.write_text(json.dumps(ctx).replace('"at_s": 1,', f'"at_s": {10**400},'))
        message = r"^context\.json: faults\[0\]: field at_s: beyond a float"
    elif case == "policy inf":
        run_dir = make_run(tmp_path)
        rewrite_text(run_dir.joinpath("config.toml"), "capacity = 20\n", "capacity = 20\nx = inf\n")
        message = r"^config\.toml: \[policy\]: field x: inf: want a finite number"
    elif case == "scenario rate inf":
        run_dir = make_run(tmp_path)
        rewrite_scenario(run_dir, "rate_per_s = 1.0", "rate_per_s = inf")
        message = r"steps\[0\]: field rate_per_s: inf: want a finite number"
    elif case == "scenario rewrite nan":
        run_dir = make_run(tmp_path)
        rewrite_scenario(
            run_dir, 'verdict = "none"', 'verdict = "none"\nforced_aof_rewrites_s = [nan]'
        )
        message = r": field forced_aof_rewrites_s: nan: want a finite number"
    else:
        run_dir = make_run(tmp_path)
        rewrite_scenario(run_dir, "duration_s = 10.0", f"duration_s = {10**400}")
        message = r"steps\[0\]: field duration_s: beyond a float"
    assert_refused(run_dir, message, capsys)


def test_cpu_mean_of_huge_readings(tmp_path: Path) -> None:
    """The CPU mean of finite readings is finite, however large they are (their float sum would
    overflow), and report.json is strict JSON."""
    lines: list[dict[str, object] | str] = []
    for t in (1, 2):
        line = sample(T0_WALL_NS + t * SECOND)
        procs = line["procs"]
        assert isinstance(procs, list) and isinstance(procs[0], dict)
        procs[0]["cpu"] = 1e308
        lines.append(line)
    run_dir = make_run(tmp_path, samples=lines)
    assert main([str(run_dir)]) == 0

    def refuse(name: str) -> object:
        raise AssertionError(f"report.json holds {name}")

    text = run_dir.joinpath("report.json").read_text(encoding="utf-8")
    report = json.loads(text, parse_constant=refuse)
    stats = report["resources"]["processes"]["valkey-server"]
    assert (stats["cpu_pct_mean"], stats["cpu_pct_max"], stats["cpu_readings"]) == (1e308, 1e308, 2)


@pytest.mark.parametrize("case", ["gen error", "api user_id", "context value", "context key"])
def test_unencodable_strings(tmp_path: Path, capsys: pytest.CaptureFixture[str], case: str) -> None:
    """A string UTF-8 cannot encode (a lone surrogate escape) is refused, naming its field,
    before any report file is written."""
    if case == "gen error":
        limited = attempt(0, 6 * SECOND, status=429, error="rate_limited")
        line = text_line(limited, '"error": "rate_limited"', '"error": "\\ud800"')
        run_dir = make_run(tmp_path, gen=[line, summary(1)])
        message = r"^gen\.jsonl: line 1: field error: a string that is not UTF-8"
    elif case == "api user_id":
        api: list[dict[str, object] | str] = list(api_lines())
        record = api[2]
        assert isinstance(record, dict)
        api[2] = text_line(record, '"user_id": "u000000"', '"user_id": "\\udfff"')
        run_dir = make_run(tmp_path, api=api)
        message = r"^api-1\.jsonl: line 3: field user_id: a string that is not UTF-8"
    elif case == "context value":
        run_dir = make_run(tmp_path)
        rewrite_text(run_dir.joinpath("context.json"), '"macOS"', '"mac\\uD800OS"')
        message = r"^context\.json: field os: a string that is not UTF-8"
    else:
        run_dir = make_run(tmp_path)
        rewrite_text(run_dir.joinpath("context.json"), '"seed"', '"\\ud801seed"')
        message = r"^context\.json: field json: a key that is not UTF-8"
    assert_refused(run_dir, message, capsys)


def test_paired_surrogate_escape_is_text(tmp_path: Path) -> None:
    """A surrogate pair escape is one character, not a lone surrogate: the run builds."""
    run_dir = make_run(tmp_path)
    rewrite_text(run_dir.joinpath("context.json"), '"macOS"', '"mac\\ud83d\\ude00OS"')
    context = build(run_dir)["context"]
    assert isinstance(context, dict)
    assert context["os"] == {
        "ProductName": "mac\U0001f600OS",
        "ProductVersion": "26.0",
        "BuildVersion": "25A1",
    }


def test_write_report_replaces_both_or_neither(tmp_path: Path) -> None:
    """report.json and report.md are encoded and both temporary files written before either is
    replaced: a report that cannot be encoded, or a temporary file that cannot be written, leaves
    the old pair and no temporary file."""
    old_json, old_md = '{"old": true}\n', "# old\n"
    tmp_path.joinpath("report.json").write_text(old_json, encoding="utf-8")
    tmp_path.joinpath("report.md").write_text(old_md, encoding="utf-8")

    def unchanged() -> None:
        assert tmp_path.joinpath("report.json").read_text(encoding="utf-8") == old_json
        assert tmp_path.joinpath("report.md").read_text(encoding="utf-8") == old_md
        assert sorted(p.name for p in tmp_path.glob("*.tmp")) == []

    with pytest.raises(UnicodeEncodeError):
        write_report(tmp_path, {"run_id": "x"}, "# report \ud800\n")
    unchanged()
    with pytest.raises(ValueError, match="JSON compliant"):
        write_report(tmp_path, {"value": float("inf")}, "# report\n")
    unchanged()
    tmp_path.joinpath("report.md.tmp").mkdir()  # the second temporary file cannot be written
    with pytest.raises(OSError, match=r"report\.md\.tmp"):
        write_report(tmp_path, {"run_id": "x"}, "# report\n")
    tmp_path.joinpath("report.md.tmp").rmdir()
    unchanged()
    write_report(tmp_path, {"run_id": "x"}, "# report\n")
    assert tmp_path.joinpath("report.json").read_text(encoding="utf-8") == '{\n  "run_id": "x"\n}\n'
    assert tmp_path.joinpath("report.md").read_text(encoding="utf-8") == "# report\n"
    assert sorted(p.name for p in tmp_path.glob("*.tmp")) == []


@pytest.mark.parametrize(
    "case", ["policy datetime", "instance date", "deep config", "deep scenario", "no policy table"]
)
def test_toml_values(tmp_path: Path, capsys: pytest.CaptureFixture[str], case: str) -> None:
    """The TOML tables the report copies hold only JSON values (no date or time), and TOML
    nested beyond the parser's depth is an input error, not a crash."""
    run_dir = make_run(tmp_path)
    config = run_dir.joinpath("config.toml")
    if case == "policy datetime":
        rewrite_text(config, "[policy]\n", "[policy]\nwhen = 1979-05-27T07:32:00Z\n")
        message = r"^config\.toml: \[policy\]: field when: a TOML datetime: want a JSON value"
    elif case == "instance date":
        rewrite_text(config, "[instance]\n", "[instance]\nday = 1979-05-27\n")
        message = r"^config\.toml: \[instance\]: field day: a TOML date: want a JSON value"
    elif case == "deep config":
        config.write_text(
            config.read_text(encoding="utf-8") + "[deep]\nx = " + "[" * 3000 + "]" * 3000 + "\n",
            encoding="utf-8",
        )
        message = r"^config\.toml: field toml: nested too deeply"
    elif case == "deep scenario":
        rewrite_scenario(run_dir, "[users]\n", "deep = " + "[" * 3000 + "]" * 3000 + "\n[users]\n")
        message = r"^scenario .*: field toml: nested too deeply"
    else:
        # The report copies an absent [policy] as null, but the BUD-11a check needs it.
        text = config.read_text(encoding="utf-8")
        config.write_text(text[text.index("[instance]") :], encoding="utf-8")
        message = (
            r"^refmodel\.check_bud11: field evidence: refused: .*'policy': missing policy table"
        )
    assert_refused(run_dir, message, capsys)


def test_deep_context_renders(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    """A context.json value nested deeper than Python's recursion limit is copied into
    report.json whole and rendered in report.md (its tables follow a bounded depth)."""
    run_dir = make_run(tmp_path)
    depth = 2_000
    rewrite_text(
        run_dir.joinpath("context.json"),
        '"ncpu": 16',
        '"ncpu": ' + '{"a":' * depth + "16" + "}" * depth,
    )
    assert main([str(run_dir)]) == 0
    capsys.readouterr()
    report = json.loads(run_dir.joinpath("report.json").read_text(encoding="utf-8"))
    value = report["context"]["hardware"]["ncpu"]
    for _ in range(depth):
        value = value["a"]
    assert value == 16
    # report.md follows the nesting a bounded number of levels, under headings, and gives the
    # rest as one cell of compact JSON.
    lines = run_dir.joinpath("report.md").read_text(encoding="utf-8").splitlines()
    headings = [line for line in lines if line.startswith("#") and ".ncpu" in line]
    assert 0 < len(headings) < 20
    cells = [line for line in lines if line.startswith('| a | {"a":')]
    assert len(cells) == 1
    value = json.loads(cells[0].removeprefix("| a | ").removesuffix(" |"))
    while isinstance(value, dict):
        value = value["a"]
    assert value == 16


@pytest.mark.parametrize(
    ("line", "key", "value"),
    [
        (4, "decision_ns", DECIDED + 1),
        (3, "decision_ns", TBOUND),
        (6, "credit_decision_ns", DECIDED + 1_001),
        (6, "credit_decision_ns", TBOUND + 1_000),
    ],
)
def test_budget_time_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], line: int, key: str, value: int
) -> None:
    """decision_ns and credit_decision_ns are budget time: µs × 1,000 below TBOUND (API-4)."""
    api: list[dict[str, object] | str] = list(api_lines())
    record = api[line - 1]
    assert isinstance(record, dict)
    api[line - 1] = with_field(record, key, value)
    run_dir = make_run(tmp_path, api=api)
    message = f"^api-1\\.jsonl: line {line}: field {key}: {value}: want budget time"
    assert_refused(run_dir, message, capsys)


@pytest.mark.parametrize("case", ["repeated deduction", "empty user"])
def test_check_bud11_refusal(tmp_path: Path, capsys: pytest.CaptureFixture[str], case: str) -> None:
    """Evidence refmodel's BUD-11a check refuses (records the builder's own checks pass) is an
    input error keeping refmodel's file, line and field, not a report."""
    api: list[dict[str, object] | str] = list(api_lines())
    decision = api[3]
    assert isinstance(decision, dict)
    if case == "repeated deduction":
        api.insert(4, dict(decision))
        message = r"line 5: field 'deduction_id': duplicate deduction identity"
    else:
        api[3] = with_field(decision, "user_id", "")
        message = r"line 4: field 'user_id': expected 1-256 UTF-8 bytes"
    run_dir = make_run(tmp_path, api=api)
    full = r"^refmodel\.check_bud11: field evidence: refused: .*api-1\.jsonl: " + message
    assert_refused(run_dir, full, capsys)


def test_postgres_rss_location(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    """A bad postgres RSS reading names the postgres reading, not the sample line's procs."""
    line = sample(T0_WALL_NS + SECOND)
    pg = line["postgres"]
    assert isinstance(pg, dict)
    pg["rss"] = "x"
    run_dir = make_run(tmp_path, samples=[line])
    assert_refused(
        run_dir, r"^samples\.jsonl: line 1: postgres: field rss: want an integer", capsys
    )
