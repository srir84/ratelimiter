"""Readers for a run directory's evidence (DESIGN §B8; the formats H1-H4 write).

Every JSON line or document is decoded strictly (UTF-8, one JSON object, no repeated key, no
NaN, Infinity or number beyond a float, no string UTF-8 cannot encode) and every field the
report reads is type- and range-checked (an integer fits int64, as the Go writers' do); a bad
record raises InputError naming the file, the line and the field (AGENTS.md §4). Line files are
read one line at a time; whole documents are small and bounded.
"""

import json
import math
import re
import tomllib
from collections.abc import Callable, Iterator
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta, timezone
from functools import partial
from pathlib import Path

from report import InputError

# The longest line a run log may hold, in bytes (plan choice: a samples.jsonl line with three
# instances' metrics is a few tens of KiB).
MAX_LINE = 16 << 20
# The largest JSON or TOML document read whole (context.json, db_rows.json, pids.json,
# config.toml, the scenario file, a baseline's report.json), in bytes.
MAX_DOC = 64 << 20

# The run directory's files (DESIGN §B8; the orchestrator's and hitgen's names, H2-H4).
CONTEXT = "context.json"
CONFIG = "config.toml"
SCHEDULE = "schedule.jsonl"
GEN = "gen.jsonl"
SAMPLES = "samples.jsonl"
DB_ROWS = "db_rows.json"
PIDS = "pids.json"
HITGEN_STDERR = "hitgen.stderr"
THRESHOLDS = "thresholds.json"
REPORT_JSON = "report.json"
REPORT_MD = "report.md"
INDEX = "index.jsonl"  # runs/index.jsonl, next to the run directories
# A run ID, one path component (the orchestrator's runID: YYYYMMDD-HHMMSS-<scenario>-<seed>).
RUN_ID = re.compile(r"[0-9]{8}-[0-9]{6}-[A-Za-z0-9_-]{1,64}-[0-9]{1,19}")

# The integers the report keeps (array('q') samples) and the Go writers write: int64.
INT64_MAX = (1 << 63) - 1

RUN_KINDS = ("bench", "test")
VERDICTS = ("capacity", "spike", "none")
# The longest run, in s (harness/config: a run lasts at most a day).
MAX_RUN_S = 86_400


def api_file(instance: int) -> str:
    """Instance N's run log, api-N.jsonl."""
    return f"api-{instance}.jsonl"


class _DuplicateKeyError(Exception):
    def __init__(self, key: str) -> None:
        super().__init__(key)
        self.key = key


class _ConstantError(Exception):
    def __init__(self, name: str) -> None:
        super().__init__(name)
        self.name = name


def _unique(pairs: list[tuple[str, object]]) -> dict[str, object]:
    record = dict(pairs)
    if len(record) != len(pairs):
        seen: set[str] = set()
        for key, _ in pairs:
            if key in seen:
                raise _DuplicateKeyError(key)
            seen.add(key)
    return record


class _RangeError(Exception):
    def __init__(self, literal: str) -> None:
        super().__init__(literal)
        self.literal = literal


def _constant(name: str) -> object:
    raise _ConstantError(name)


def _finite(literal: str) -> float:
    """A JSON number with a fraction or exponent, as a float: one beyond a float's range (1e400,
    which float() reads as infinity) is refused."""
    value = float(literal)
    if not math.isfinite(value):
        raise _RangeError(literal)
    return value


def _encodable(text: str) -> bool:
    try:
        text.encode("utf-8")
    except UnicodeEncodeError:
        return False
    return True


def _holds_unencodable(value: object) -> bool:
    """Whether value holds, at any depth, a string (a key or a value) UTF-8 cannot encode: a
    lone surrogate, which only a \\u escape can give (the bytes were strict UTF-8)."""
    stack = [value]
    while stack:
        item = stack.pop()
        if isinstance(item, str):
            if not _encodable(item):
                return True
        elif isinstance(item, dict):
            stack.extend(item.keys())
            stack.extend(item.values())
        elif isinstance(item, list):
            stack.extend(item)
    return False


def _check_encodable(value: object, where: str) -> None:
    """Refuses a decoded value holding a string UTF-8 cannot encode, naming its top-level field."""
    if not isinstance(value, dict):
        if _holds_unencodable(value):
            raise InputError(where, "json", "a string that is not UTF-8 (a lone surrogate escape)")
        return
    for key, item in value.items():
        if not _encodable(key):
            raise InputError(where, "json", "a key that is not UTF-8 (a lone surrogate escape)")
        if _holds_unencodable(item):
            raise InputError(where, key, "a string that is not UTF-8 (a lone surrogate escape)")


def decode(text: str, where: str) -> object:
    """One JSON value, strictly: no repeated key, no NaN or Infinity, no number beyond a float,
    and no string UTF-8 cannot encode."""
    try:
        value = json.loads(
            text, object_pairs_hook=_unique, parse_constant=_constant, parse_float=_finite
        )
    except _DuplicateKeyError as err:
        raise InputError(where, err.key, "repeated JSON key") from err
    except _ConstantError as err:
        raise InputError(where, "json", f"{err.name} is not a JSON number") from err
    except _RangeError as err:
        raise InputError(where, "json", f"{err.literal[:40]} is beyond a float") from err
    except RecursionError as err:
        raise InputError(where, "json", "nested too deeply") from err
    except ValueError as err:
        raise InputError(where, "json", f"not JSON: {err}") from err
    # Only a \u escape gives a surrogate: the full check runs on the lines that have one.
    if "\\ud" in text or "\\uD" in text:
        _check_encodable(value, where)
    return value


def decode_object(raw: bytes, where: str) -> dict[str, object]:
    """One line's JSON object."""
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError as err:
        raise InputError(where, "json", "not UTF-8") from err
    value = decode(text, where)
    if not isinstance(value, dict):
        raise InputError(where, "json", "not a JSON object")
    return value


def iter_jsonl(
    path: Path, on_raw: Callable[[bytes], None] | None = None
) -> Iterator[tuple[str, dict[str, object]]]:
    """Each line of the JSON-lines file at path as (location, object), one line in memory at a
    time; on_raw, when given, sees every line's bytes first (a hash of the file)."""
    try:
        stream = path.open("rb")
    except OSError as err:
        raise InputError(path.name, "file", f"cannot be read: {err.strerror}") from err
    with stream:
        for number, raw in enumerate(iter(partial(stream.readline, MAX_LINE + 1), b""), 1):
            where = f"{path.name}: line {number}"
            if len(raw) > MAX_LINE:
                raise InputError(where, "json", f"a line longer than {MAX_LINE} bytes")
            if on_raw is not None:
                on_raw(raw)
            yield where, decode_object(raw, where)


def read_bytes(path: Path) -> bytes:
    """A whole document of at most MAX_DOC bytes."""
    try:
        with path.open("rb") as stream:
            data = stream.read(MAX_DOC + 1)
    except OSError as err:
        raise InputError(path.name, "file", f"cannot be read: {err.strerror}") from err
    if len(data) > MAX_DOC:
        raise InputError(path.name, "file", f"more than {MAX_DOC} bytes")
    return data


def read_json(path: Path) -> dict[str, object]:
    """A whole JSON document holding one object."""
    data = read_bytes(path)
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as err:
        raise InputError(path.name, "json", "not UTF-8") from err
    value = decode(text, path.name)
    if not isinstance(value, dict):
        raise InputError(path.name, "json", "not a JSON object")
    return value


def read_toml(path: Path) -> dict[str, object]:
    """A whole TOML document."""
    data = read_bytes(path)
    try:
        return tomllib.loads(data.decode("utf-8"))
    except UnicodeDecodeError as err:
        raise InputError(path.name, "toml", "not UTF-8") from err
    except tomllib.TOMLDecodeError as err:
        raise InputError(path.name, "toml", f"not TOML: {err}") from err
    except RecursionError as err:
        raise InputError(path.name, "toml", "nested too deeply") from err


def check_json_typed(value: object, where: str, field: str) -> None:
    """A TOML value the report copies holds only JSON types: tables, arrays, strings, booleans,
    integers and finite floats (no date or time, no inf or nan)."""
    stack = [value]
    while stack:
        item = stack.pop()
        if isinstance(item, dict):
            stack.extend(item.values())
        elif isinstance(item, list):
            stack.extend(item)
        elif isinstance(item, float):
            if not math.isfinite(item):
                raise InputError(where, field, f"{item}: want a finite number")
        elif not isinstance(item, str | int):  # bool is an int
            kind = type(item).__name__
            raise InputError(where, field, f"a TOML {kind}: want a JSON value (no date or time)")


# Field checks. Each returns the value with its type narrowed, or raises InputError.


def get_int(rec: dict[str, object], key: str, where: str, minimum: int | None = 0) -> int:
    """An integer (never a bool or a float) at least minimum (None: any) that fits int64."""
    value = rec.get(key)
    if type(value) is not int:
        raise InputError(where, key, "want an integer")
    if minimum is not None and value < minimum:
        raise InputError(where, key, f"{value}: want at least {minimum}")
    if not -INT64_MAX - 1 <= value <= INT64_MAX:
        raise InputError(where, key, f"{value}: beyond int64")
    return value


def get_opt_int(
    rec: dict[str, object], key: str, where: str, minimum: int | None = 0
) -> int | None:
    """An integer at least minimum, or null."""
    if rec.get(key) is None:
        if key not in rec:
            raise InputError(where, key, "missing")
        return None
    return get_int(rec, key, where, minimum)


def get_str(rec: dict[str, object], key: str, where: str) -> str:
    value = rec.get(key)
    if not isinstance(value, str):
        raise InputError(where, key, "want a string")
    return value


def get_opt_str(rec: dict[str, object], key: str, where: str) -> str | None:
    if rec.get(key) is None:
        if key not in rec:
            raise InputError(where, key, "missing")
        return None
    return get_str(rec, key, where)


def get_bool(rec: dict[str, object], key: str, where: str) -> bool:
    value = rec.get(key)
    if type(value) is not bool:
        raise InputError(where, key, "want true or false")
    return value


def get_number(rec: dict[str, object], key: str, where: str) -> float:
    """A JSON number (an integer or a float, never a bool) within a float's range."""
    value = rec.get(key)
    if type(value) is int or type(value) is float:
        return _as_float(value, where, key)
    raise InputError(where, key, "want a number")


def _as_float(value: int | float, where: str, key: str) -> float:
    """value as a finite float: an integer beyond a float's range is refused."""
    try:
        number = float(value)
    except OverflowError as err:
        raise InputError(where, key, "beyond a float") from err
    if not math.isfinite(number):
        raise InputError(where, key, f"{number}: want a finite number")
    return number


def get_opt_number(rec: dict[str, object], key: str, where: str) -> float | None:
    if rec.get(key) is None:
        if key not in rec:
            raise InputError(where, key, "missing")
        return None
    return get_number(rec, key, where)


def get_obj(rec: dict[str, object], key: str, where: str) -> dict[str, object]:
    value = rec.get(key)
    if not isinstance(value, dict):
        raise InputError(where, key, "want an object")
    return value


def get_opt_obj(rec: dict[str, object], key: str, where: str) -> dict[str, object] | None:
    if rec.get(key) is None:
        if key not in rec:
            raise InputError(where, key, "missing")
        return None
    return get_obj(rec, key, where)


def get_list(rec: dict[str, object], key: str, where: str) -> list[object]:
    value = rec.get(key)
    if not isinstance(value, list):
        raise InputError(where, key, "want a list")
    return value


def get_opt_list(rec: dict[str, object], key: str, where: str) -> list[object]:
    """A list, or null read as an empty list (Go writes a nil slice as null)."""
    if rec.get(key) is None:
        if key not in rec:
            raise InputError(where, key, "missing")
        return []
    return get_list(rec, key, where)


def as_obj(value: object, where: str, field: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise InputError(where, field, "want an object")
    return value


def exact_keys(rec: dict[str, object], keys: frozenset[str], where: str) -> None:
    """rec has exactly keys: a missing or an unknown key names itself."""
    if rec.keys() == keys:
        return
    for key in sorted(keys - rec.keys()):
        raise InputError(where, key, "missing")
    for key in sorted(rec.keys() - keys):
        raise InputError(where, key, "unknown field")


# context.json (DESIGN §B8; cmd/orchestrator context.go, schedule.go).


@dataclass(frozen=True)
class Fault:
    """One [[faults]] record with its actual times (wall-clock ns; 0 when it did not run)."""

    at_s: float
    cmd: str
    instance: int | None
    start_ns: int
    end_ns: int
    outage_start_ns: int | None
    outage_ns: int | None
    error: str


@dataclass(frozen=True)
class Rewrite:
    """One forced BGREWRITEAOF's window (wall-clock ns) as context.json records it."""

    at_s: float
    start_ns: int
    end_ns: int
    status: str
    error: str


@dataclass(frozen=True)
class Context:
    raw: dict[str, object]
    run_id: str
    epoch_id: str
    run_kind: str
    baseline: bool
    start_ns: int
    end_ns: int
    scenario_name: str
    scenario_path: str
    scenario_sha256: str
    seed: int
    instances: int
    schedule_sha256: str | None
    thresholds_sha256: str | None
    faults: tuple[Fault, ...]
    rewrites: tuple[Rewrite, ...]


def _fault(value: object, where: str) -> Fault:
    rec = as_obj(value, where, "faults")
    error = rec.get("error", "")
    if not isinstance(error, str):
        raise InputError(where, "error", "want a string")
    return Fault(
        at_s=get_number(rec, "at_s", where),
        cmd=get_str(rec, "cmd", where),
        instance=get_opt_int(rec, "instance", where, 1),
        start_ns=get_int(rec, "start_ns", where),
        end_ns=get_int(rec, "end_ns", where),
        outage_start_ns=get_opt_int(rec, "outage_start_ns", where),
        outage_ns=get_opt_int(rec, "outage_ns", where),
        error=error,
    )


def _rewrite(value: object, where: str) -> Rewrite:
    rec = as_obj(value, where, "rewrites")
    extra = {}
    for key in ("status", "error"):
        text = rec.get(key, "")
        if not isinstance(text, str):
            raise InputError(where, key, "want a string")
        extra[key] = text
    return Rewrite(
        at_s=get_number(rec, "at_s", where),
        start_ns=get_int(rec, "start_ns", where),
        end_ns=get_int(rec, "end_ns", where),
        status=extra["status"],
        error=extra["error"],
    )


def read_context(run_dir: Path) -> Context:
    raw = read_json(run_dir.joinpath(CONTEXT))
    where = CONTEXT
    run_kind = get_str(raw, "run_kind", where)
    if run_kind not in RUN_KINDS:
        raise InputError(where, "run_kind", f"{run_kind!r}: want bench or test")
    scenario = get_obj(raw, "scenario", where)
    instances = get_int(raw, "instances", where, 1)
    if instances > 3:
        raise InputError(where, "instances", f"{instances}: want 1 to 3")
    faults = tuple(
        _fault(f, f"{where}: faults[{i}]") for i, f in enumerate(get_opt_list(raw, "faults", where))
    )
    rewrites = tuple(
        _rewrite(r, f"{where}: rewrites[{i}]")
        for i, r in enumerate(get_opt_list(raw, "rewrites", where))
    )
    return Context(
        raw=raw,
        run_id=get_str(raw, "run_id", where),
        epoch_id=get_str(raw, "epoch_id", where),
        run_kind=run_kind,
        baseline=get_bool(raw, "baseline", where),
        start_ns=get_int(raw, "start_ns", where),
        end_ns=get_int(raw, "end_ns", where),
        scenario_name=get_str(scenario, "name", f"{where}: scenario"),
        scenario_path=get_str(scenario, "path", f"{where}: scenario"),
        scenario_sha256=get_str(scenario, "sha256", f"{where}: scenario"),
        seed=get_int(raw, "seed", where),
        instances=instances,
        schedule_sha256=get_opt_str(raw, "schedule_sha256", where),
        thresholds_sha256=get_opt_str(raw, "thresholds_sha256", where),
        faults=faults,
        rewrites=rewrites,
    )


# config.toml (DESIGN §B2): the run's merged settings.


@dataclass(frozen=True)
class RunConfig:
    raw: dict[str, object]
    p_ns: int  # [instance] max_release_delay_ns, P
    limiter_enabled: bool  # [api] limiter_enabled (false in a baseline run, HAR-17)


def read_config(run_dir: Path) -> RunConfig:
    """config.toml: P and limiter_enabled, and the [policy] and [instance] tables the report
    copies, which must hold only JSON values."""
    raw = read_toml(run_dir.joinpath(CONFIG))
    instance = raw.get("instance")
    api = raw.get("api")
    if not isinstance(instance, dict):
        raise InputError(CONFIG, "instance", "want the [instance] table")
    if not isinstance(api, dict):
        raise InputError(CONFIG, "api", "want the [api] table")
    for table in ("policy", "instance"):
        value = raw.get(table)
        if isinstance(value, dict):
            for key, item in value.items():
                check_json_typed(item, f"{CONFIG}: [{table}]", key)
        elif value is not None:  # an absent table is copied as null
            check_json_typed(value, CONFIG, table)
    p_ns = get_int(instance, "max_release_delay_ns", f"{CONFIG}: [instance]", 1)
    enabled = get_bool(api, "limiter_enabled", f"{CONFIG}: [api]")
    return RunConfig(raw=raw, p_ns=p_ns, limiter_enabled=enabled)


# The scenario file (DESIGN §B4 "Scenario file"), pinned by context.json's scenario.sha256.


@dataclass(frozen=True)
class Step:
    rate_per_s: float
    duration_s: float


@dataclass(frozen=True)
class Scenario:
    path: Path
    name: str
    seed: int
    run_kind: str
    instances: int
    steps: tuple[Step, ...]
    verdict: str
    spike_step: int | None
    forced_aof_rewrites_s: tuple[float, ...]
    stop_early: bool
    users: int


def _toml_number(table: dict[str, object], key: str, where: str) -> float:
    """A TOML number as a finite float (no inf or nan, no integer beyond a float)."""
    value = table.get(key)
    if type(value) is int or type(value) is float:
        return _as_float(value, where, key)
    raise InputError(where, key, "want a number")


def resolve_scenario(ctx: Context, run_dir: Path) -> Path:
    """The scenario file context.json names: an absolute path as it is; a relative one from the
    working directory (the orchestrator's, which runs the builder), else from the repository
    root that holds runs/<id>."""
    path = Path(ctx.scenario_path)
    if path.is_absolute() or path.exists():
        return path
    return run_dir.resolve().parent.parent.joinpath(path)


def read_scenario(ctx: Context, run_dir: Path, sha256_of: Callable[[bytes], str]) -> Scenario:
    path = resolve_scenario(ctx, run_dir)
    where = f"scenario {ctx.scenario_path}"
    data = read_bytes(path)
    if sha256_of(data) != ctx.scenario_sha256:
        raise InputError(
            f"{CONTEXT}: scenario", "sha256", f"{path} is not the file the run used (hash differs)"
        )
    try:
        raw = tomllib.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, tomllib.TOMLDecodeError) as err:
        raise InputError(where, "toml", f"not TOML: {err}") from err
    except RecursionError as err:
        raise InputError(where, "toml", "nested too deeply") from err
    steps_raw = raw.get("steps")
    if not isinstance(steps_raw, list) or not steps_raw:
        raise InputError(where, "steps", "want at least one [[steps]] table")
    steps = []
    for i, value in enumerate(steps_raw):
        table = as_obj(value, f"{where}: steps[{i}]", "steps")
        rate = _toml_number(table, "rate_per_s", f"{where}: steps[{i}]")
        duration = _toml_number(table, "duration_s", f"{where}: steps[{i}]")
        if not rate >= 0:
            raise InputError(f"{where}: steps[{i}]", "rate_per_s", "want at least 0")
        if not 0 < duration <= MAX_RUN_S:
            raise InputError(f"{where}: steps[{i}]", "duration_s", f"want 0 < s ≤ {MAX_RUN_S}")
        steps.append(Step(rate_per_s=rate, duration_s=duration))
    verdict = raw.get("verdict", "none")
    if verdict not in VERDICTS:
        raise InputError(where, "verdict", f"{verdict!r}: want capacity, spike or none")
    spike = raw.get("spike_step")
    if spike is not None and (type(spike) is not int or not 0 <= spike < len(steps) - 1):
        raise InputError(where, "spike_step", "want a step index before the last step")
    rewrites_raw = raw.get("forced_aof_rewrites_s", [])
    if not isinstance(rewrites_raw, list):
        raise InputError(where, "forced_aof_rewrites_s", "want a list of offsets")
    rewrites = []
    for value in rewrites_raw:
        if type(value) is not int and type(value) is not float:
            raise InputError(where, "forced_aof_rewrites_s", "want numbers")
        rewrites.append(_as_float(value, where, "forced_aof_rewrites_s"))
    stop_early = raw.get("stop_early", False)
    if type(stop_early) is not bool:
        raise InputError(where, "stop_early", "want true or false")
    users = raw.get("users", {})
    if not isinstance(users, dict):
        raise InputError(where, "users", "want a [users] table")
    count = users.get("count", 0)
    if type(count) is not int or count < 0:
        raise InputError(f"{where}: users", "count", "want an integer of at least 0")
    sc = Scenario(
        path=path,
        name=_scenario_str(raw, "name", where),
        seed=_scenario_int(raw, "seed", where),
        run_kind=_scenario_str(raw, "run_kind", where),
        instances=_scenario_int(raw, "instances", where, 3),
        steps=tuple(steps),
        verdict=str(verdict),
        spike_step=spike if type(spike) is int else None,
        forced_aof_rewrites_s=tuple(rewrites),
        stop_early=stop_early,
        users=count,
    )
    for field, mine, theirs in (
        ("name", sc.name, ctx.scenario_name),
        ("seed", sc.seed, ctx.seed),
        ("run_kind", sc.run_kind, ctx.run_kind),
        ("instances", sc.instances, ctx.instances),
    ):
        if mine != theirs:
            raise InputError(where, field, f"{mine!r}, but context.json has {theirs!r}")
    return sc


def _scenario_str(raw: dict[str, object], key: str, where: str) -> str:
    value = raw.get(key)
    if not isinstance(value, str):
        raise InputError(where, key, "want a string")
    return value


def _scenario_int(raw: dict[str, object], key: str, where: str, default: int | None = None) -> int:
    value = raw.get(key, default)
    if type(value) is not int:
        raise InputError(where, key, "want an integer")
    return value


# hitgen.stderr (H3: hitgen's own JSON log lines, saved by the orchestrator).

_RFC3339 = re.compile(
    r"(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})"
)
_EPOCH = datetime(1970, 1, 1, tzinfo=UTC)


def rfc3339_ns(text: str, where: str, field: str) -> int:
    """An RFC 3339 time with up to 9 fractional digits (Go's slog JSON "time") as wall-clock ns
    since the UTC epoch, exactly."""
    m = _RFC3339.fullmatch(text)
    if m is None:
        raise InputError(where, field, f"{text!r}: want an RFC 3339 time")
    year, month, day, hour, minute, second = (int(m.group(i)) for i in range(1, 7))
    zone = m.group(8)
    offset = timedelta(0)
    if zone != "Z":
        sign = -1 if zone[0] == "-" else 1
        offset = sign * timedelta(hours=int(zone[1:3]), minutes=int(zone[4:6]))
    try:
        moment = datetime(year, month, day, hour, minute, second, tzinfo=timezone(offset))
    except ValueError as err:
        raise InputError(where, field, f"{text!r}: {err}") from err
    seconds = (moment - _EPOCH) // timedelta(seconds=1)
    fraction = (m.group(7) or "").ljust(9, "0")
    return seconds * 1_000_000_000 + int(fraction or "0")


@dataclass(frozen=True)
class HitgenLog:
    t0_wall_ns: int  # the "sending" line's time: just before hitgen takes its t0
    retry_drops: int | None  # the "sending done" line's (PRF-8); None without that line
    generator_drops: int | None


def read_hitgen_stderr(run_dir: Path) -> HitgenLog:
    """hitgen's "sending" line (exactly one: the wall-clock anchor of every planned_ns and sent_ns
    offset) and its "sending done" line (at most one). Other lines are not read; a line that is
    not a JSON object is passed over (the Go runtime writes panics there as text)."""
    path = run_dir.joinpath(HITGEN_STDERR)
    t0: int | None = None
    done: tuple[int, int] | None = None
    try:
        stream = path.open("rb")
    except OSError as err:
        raise InputError(path.name, "file", f"cannot be read: {err.strerror}") from err
    with stream:
        for number, raw in enumerate(iter(partial(stream.readline, MAX_LINE + 1), b""), 1):
            where = f"{path.name}: line {number}"
            if not raw.startswith(b"{") or len(raw) > MAX_LINE:
                continue
            rec = decode_object(raw, where)
            msg = rec.get("msg")
            if msg == "sending":
                if t0 is not None:
                    raise InputError(where, "msg", "a second sending line")
                t0 = rfc3339_ns(get_str(rec, "time", where), where, "time")
            elif msg == "sending done":
                if done is not None:
                    raise InputError(where, "msg", "a second sending done line")
                done = (
                    get_int(rec, "retry_drops", where),
                    get_int(rec, "generator_drops", where),
                )
    if t0 is None:
        raise InputError(path.name, "msg", "no sending line: hitgen never started sending")
    return HitgenLog(
        t0_wall_ns=t0,
        retry_drops=None if done is None else done[0],
        generator_drops=None if done is None else done[1],
    )


# db_rows.json (H3: {"db_rows":N}) and pids.json (harness/runlog PIDs).


def read_db_rows(run_dir: Path) -> int:
    raw = read_json(run_dir.joinpath(DB_ROWS))
    exact_keys(raw, frozenset({"db_rows"}), DB_ROWS)
    return get_int(raw, "db_rows", DB_ROWS)


def read_pids(run_dir: Path) -> list[dict[str, object]]:
    raw = read_json(run_dir.joinpath(PIDS))
    out: list[dict[str, object]] = []
    for i, value in enumerate(get_opt_list(raw, "procs", PIDS)):
        where = f"{PIDS}: procs[{i}]"
        rec = as_obj(value, where, "procs")
        entry: dict[str, object] = {"role": get_str(rec, "role", where)}
        if "instance" in rec:
            entry["instance"] = get_int(rec, "instance", where, 1)
        entry["pid"] = get_int(rec, "pid", where, 1)
        out.append(entry)
    return out


@dataclass(frozen=True)
class IndexLine:
    scenario: str
    baseline: bool
    run_id: str


def read_index(path: Path) -> Iterator[IndexLine]:
    """runs/index.jsonl (H3: one {"scenario","baseline","run_id"} line per finished run), each
    line yielded as it is read: the index grows with every run, so it is never held whole, and a
    reader that stops early reads no further (a bad line raises InputError when reached)."""
    for where, rec in iter_jsonl(path):
        exact_keys(rec, frozenset({"scenario", "baseline", "run_id"}), where)
        yield IndexLine(
            scenario=get_str(rec, "scenario", where),
            baseline=get_bool(rec, "baseline", where),
            run_id=get_str(rec, "run_id", where),
        )
