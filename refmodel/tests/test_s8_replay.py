"""Hand-computed S8 admin snapshots and epoch transitions."""

import copy
from pathlib import Path

import pytest

from refmodel.model import CleanupDecision, Model, RefundDecision
from refmodel.tests.test_replay import (
    admin_log,
    default_acquire,
    default_policy,
    inspect,
    integrity_signal,
    run,
)


@pytest.mark.parametrize("kind", ["clear", "restore"])
def test_admin_requires_quarantine_evidence(tmp_path: Path, kind: str) -> None:
    # A healthy bucket alone cannot authorize an admin record.
    operation: dict[str, object] = {"type": kind, "key_b64url": "YQ", "t_us": 0}
    if kind == "restore":
        operation.update(balance_units=3_000_000_000, g=2, epoch_id="e")
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        operation,
    ]
    assert run(tmp_path.joinpath("clean-admin.jsonl"), records) == 1


@pytest.mark.parametrize("kind", ["clear", "restore"])
def test_clear_consumes_integrity_evidence(tmp_path: Path, kind: str) -> None:
    records: list[dict[str, object]] = admin_log()[:8]
    operation: dict[str, object] = {"type": kind, "key_b64url": "YQ", "t_us": 1_000_000}
    if kind == "restore":
        operation.update(balance_units=3_000_000_000, g=3, epoch_id="e")
    assert run(tmp_path.joinpath("repeated-admin.jsonl"), [*records, operation]) == 1


def test_clear_after_integrity_signal_without_restore(tmp_path: Path) -> None:
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        integrity_signal(),
        {"type": "clear", "key_b64url": "YQ", "t_us": 0},
        inspect(19_000_000_000, 0, 1, 1, 60_000_000, 0),
    ]
    assert run(tmp_path.joinpath("signal-clear.jsonl"), records) == 0


def test_restore_after_cleanup_integrity_signal(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    cleanup_step = Model.cleanup_step

    def lose_bucket(self: Model, key_bytes: bytes, clock_ns: int) -> CleanupDecision:
        self.buckets.pop(key_bytes, None)
        return cleanup_step(self, key_bytes, clock_ns)

    # Bucket loss is an external fault, absent from the op log. Cleanup sees
    # its retained index at 60 s and writes quarantine with code 32.
    monkeypatch.setattr(Model, "cleanup_step", lose_bucket)
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        {"type": "clock", "value_ns": 60_000_000_000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 60_000_000, "code": 32},
        {
            "type": "restore",
            "key_b64url": "YQ",
            "balance_units": 3_000_000_000,
            "t_us": 60_000_000,
            "g": 2,
            "epoch_id": "e",
        },
        inspect(3_000_000_000, 60_000_000, 2, 0, 120_000_000, 0),
    ]
    assert run(tmp_path.joinpath("cleanup-restore.jsonl"), records) == 0


def test_restore_after_refund_integrity_signal(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    refund = Model.refund

    def lose_bucket(
        self: Model,
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
        return refund(
            self,
            key_bytes,
            g,
            s,
            expiry_us,
            clock_ns,
            epoch_matches=epoch_matches,
            max_live_markers=max_live_markers,
            max_markers_per_key=max_markers_per_key,
        )

    monkeypatch.setattr(Model, "refund", lose_bucket)
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [{"code": 23, "t_us": 0, "credited": 0}],
            "result": "unresolved",
        },
        {
            "type": "restore",
            "key_b64url": "YQ",
            "balance_units": 3_000_000_000,
            "t_us": 0,
            "g": 2,
            "epoch_id": "e",
        },
        inspect(3_000_000_000, 0, 2, 0, 60_000_000, 0),
    ]
    assert run(tmp_path.joinpath("refund-restore.jsonl"), records) == 0


def test_inspect_after_moved_cleanup_compares_implied_balance(tmp_path: Path) -> None:
    # One unit/ns, 10000 units/token, cap 10000, lifetime 1 us. At 1 us,
    # cleanup moves: the old (0,0) and refilled (1000,1 us) imply 1000.
    records: list[dict[str, object]] = [
        dict(
            default_policy(),
            rate_tokens=1,
            rate_period_ns=10000,
            capacity=1,
            receipt_lifetime_ns=1000,
        ),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        dict(default_acquire(0, 1), expiry_us=1),
        {"type": "clock", "value_ns": 1000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 1, "code": 31},
        inspect(0, 0, 1, 1, 1, 0),
    ]
    path = tmp_path.joinpath("cleanup-snapshot.jsonl")
    assert run(path, records) == 0
    records[-1]["u"] = 1  # Implies 1001 units at 1 us, not 1000.
    assert run(path, records) == 1
    # The comparison also works when the snapshot has the later time: both
    # histories imply 2000 at 2 us, while 2001 is a mismatch.
    records[-1] = inspect(2000, 2, 1, 1, 1, 0)
    assert run(path, records) == 0
    records[-1]["u"] = 2001
    assert run(path, records) == 1


@pytest.mark.parametrize("write", ["allow", "refund", "restore"])
def test_next_balance_write_ends_cleanup_storage_ambiguity(tmp_path: Path, write: str) -> None:
    # Default policy, active receipt at 59.5 s; its original marker expires at
    # 60 s. Cleanup 31 at 60 s removes that marker and refills to capacity.
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [{"code": 3, "t_us": 0, "credited": 1_000_000_000}],
            "result": "applied",
        },
        {"type": "clock", "value_ns": 59_500_000_000},
        default_acquire(19_000_000_000, 2, 59_500_000_000),
        {"type": "clock", "value_ns": 60_000_000_000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 60_000_000, "code": 31},
    ]
    if write == "allow":
        records.append(default_acquire(19_000_000_000, 3, 60_000_000_000))
        exact = inspect(19_000_000_000, 60_000_000, 1, 3, 120_000_000, 0)
        equivalent = inspect(18_000_000_000, 59_900_000, 1, 3, 120_000_000, 0)
    elif write == "refund":
        records.extend(
            [
                {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:2"},
                {
                    "type": "refund",
                    "deduction_id": "e:YQ:1:2",
                    "key_b64url": "YQ",
                    "g": 1,
                    "s": 2,
                    "expiry_us": 119_500_000,
                    "attempts": [{"code": 3, "t_us": 60_000_000, "credited": 0}],
                    "result": "applied",
                },
            ]
        )
        exact = inspect(20_000_000_000, 60_000_000, 1, 2, 119_500_000, 1)
        equivalent = inspect(20_000_000_000, 59_500_000, 1, 2, 119_500_000, 1)
    else:
        records.extend(
            [
                integrity_signal(),
                {
                    "type": "restore",
                    "key_b64url": "YQ",
                    "balance_units": 3_000_000_000,
                    "t_us": 60_000_000,
                    "g": 2,
                    "epoch_id": "e",
                },
            ]
        )
        exact = inspect(3_000_000_000, 60_000_000, 2, 0, 120_000_000, 0)
        equivalent = inspect(2_000_000_000, 59_900_000, 2, 0, 120_000_000, 0)
    path = tmp_path.joinpath("cleanup-next-write.jsonl")
    assert run(path, [*records, exact]) == 0
    # These snapshots imply the right balance, but a subsequent balance write
    # gives an exact stored u/t pair: accepting their earlier pair is wrong.
    assert run(path, [*records, equivalent]) == 1


@pytest.mark.parametrize("scope", ["other_key", "before_epoch", "past_epoch"])
def test_integrity_evidence_is_limited_to_key_and_epoch(tmp_path: Path, scope: str) -> None:
    records: list[dict[str, object]] = []
    if scope == "before_epoch":
        records.append(integrity_signal())
    records.extend(
        [
            default_policy(),
            {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
            default_acquire(19_000_000_000, 1),
        ]
    )
    if scope == "other_key":
        records.append(integrity_signal("Yg"))
    elif scope == "past_epoch":
        records.extend(
            [
                integrity_signal(),
                default_policy(),
                {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
                default_acquire(19_000_000_000, 1, epoch="f"),
            ]
        )
    records.append({"type": "clear", "key_b64url": "YQ", "t_us": 0})
    assert run(tmp_path.joinpath("evidence-scope.jsonl"), records) == 1


@pytest.mark.parametrize("field", ["u", "t_us"])
def test_inspect_without_moved_cleanup_requires_exact_storage(tmp_path: Path, field: str) -> None:
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        inspect(19_000_000_000, 0, 1, 1, 60_000_000, 0),
    ]
    value = records[-1][field]
    assert isinstance(value, int)
    records[-1][field] = value + 1
    path = tmp_path.joinpath("exact-snapshot.jsonl")
    assert run(path, records) == 1
    # Even a balance-equivalent pair cannot stand in for exact storage without
    # cleanup 31: 19 tokens at 0 refills to 20 tokens at 0.1 s.
    records[-1] = inspect(20_000_000_000, 100_000, 1, 1, 60_000_000, 0)
    assert run(path, records) == 1


def test_epoch_wide_refund_integrity_attempt_is_neutral(tmp_path: Path) -> None:
    records: list[dict[str, object]] = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
        default_policy(),
        {"type": "epoch", "epoch_id": "f", "origin_ns": 1_000_000},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [
                {"code": 22, "t_us": 0, "credited": 0},
                {"code": 10, "t_us": 1000, "credited": 0},
            ],
            "result": "receipt_invalid",
        },
    ]
    path = tmp_path.joinpath("epoch-integrity-refund.jsonl")
    assert run(path, records) == 0
    # A lone epoch-wide attempt at 1 s cannot advance f's zero high-water
    # mark. After the raw clock regresses, f's first allow still uses zero.
    neutral = dict(
        records[-1],
        attempts=[{"code": 22, "t_us": 0, "credited": 0}],
        result="unresolved",
    )
    assert (
        run(
            path,
            [
                *records[:5],
                {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
                {"type": "clock", "value_ns": 1_000_000_000},
                neutral,
                {"type": "clock", "value_ns": 0},
                default_acquire(19_000_000_000, 1, epoch="f"),
            ],
        )
        == 0
    )


@pytest.mark.parametrize("kind", ["restore", "clear", "inspect"])
def test_admin_match_and_mismatch(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], kind: str
) -> None:
    path = tmp_path.joinpath("admin.jsonl")
    records = admin_log()
    assert run(path, records) == 0
    index = {"restore": 5, "clear": 7, "inspect": 12}[kind]
    field = "g" if kind == "restore" else "t_us"
    value = records[index][field]
    assert isinstance(value, int)
    records[index][field] = value + 1
    assert run(path, records) == 1
    assert f"line {index + 1}: {field}:" in capsys.readouterr().out


@pytest.mark.parametrize("kind", ["restore", "clear", "inspect"])
@pytest.mark.parametrize("change", ["missing", "extra", "no_epoch"])
def test_admin_malformed_shape_or_epoch(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], kind: str, change: str
) -> None:
    records = admin_log()
    index = {"restore": 5, "clear": 7, "inspect": 12}[kind]
    record = records[index]
    if change == "missing":
        del record["t_us"]
    elif change == "extra":
        record["instance"] = "i"
    else:
        records = [record]
        index = 0
    assert run(tmp_path.joinpath("admin.jsonl"), records) == 2
    output = capsys.readouterr().out
    assert f"line {index + 1}: field " in output
    assert "type:" in output if change == "no_epoch" else "fields:" in output


@pytest.mark.parametrize("kind", ["restore", "clear", "inspect"])
@pytest.mark.parametrize("value", [0, "!", "YQ=", "", "_w", "YWFh" * 86])
def test_admin_invalid_keys(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], kind: str, value: object
) -> None:
    records = admin_log()
    index = {"restore": 5, "clear": 7, "inspect": 12}[kind]
    records[index]["key_b64url"] = value
    assert run(tmp_path.joinpath("admin.jsonl"), records) == 2
    assert f"line {index + 1}: field key_b64url:" in capsys.readouterr().out


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("balance_units", True),
        ("balance_units", "3"),
        ("balance_units", -1),
        ("balance_units", 20_000_000_001),
        ("t_us", True),
        ("t_us", "1000000"),
        ("t_us", -1),
        ("t_us", 2**53 - 2**43),
        ("g", True),
        ("g", "2"),
        ("g", 0),
        ("g", 2**53),
        ("epoch_id", 0),
        ("epoch_id", ""),
        ("epoch_id", "other"),
    ],
)
def test_restore_malformed_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object
) -> None:
    records = admin_log()
    records[5][field] = value
    assert run(tmp_path.joinpath("restore.jsonl"), records) == 2
    assert f"line 6: field {field}:" in capsys.readouterr().out


@pytest.mark.parametrize("value", [True, "1000000", -1, 2**53 - 2**43])
def test_clear_malformed_time(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], value: object
) -> None:
    records = admin_log()
    records[7]["t_us"] = value
    assert run(tmp_path.joinpath("clear.jsonl"), records) == 2
    assert "line 8: field t_us:" in capsys.readouterr().out


@pytest.mark.parametrize("field", ["u", "t_us", "g", "s", "x_us", "mc"])
@pytest.mark.parametrize("value", [True, "1", -1, 2**53, None])
def test_inspect_malformed_fields(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], field: str, value: object
) -> None:
    records = admin_log()
    records[12][field] = value
    assert run(tmp_path.joinpath("inspect.jsonl"), records) == 2
    assert f"line 13: field {field}:" in capsys.readouterr().out


@pytest.mark.parametrize("value", [True, "1", -1, 3])
def test_inspect_malformed_present(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], value: object
) -> None:
    records = admin_log()
    records[12]["present"] = value
    assert run(tmp_path.joinpath("inspect.jsonl"), records) == 2
    assert "line 13: field present:" in capsys.readouterr().out


def test_inspect_absent_and_unreadable_raw_fields(tmp_path: Path) -> None:
    absent = inspect("", "", "", "", "", "", present=0)
    records = [default_policy(), {"type": "epoch", "epoch_id": "e", "origin_ns": 0}, absent]
    path = tmp_path.joinpath("inspect.jsonl")
    assert run(path, records) == 0
    for field in ("present", "u", "t_us", "g", "s", "x_us", "mc"):
        changed = copy.deepcopy(records)
        changed[-1][field] = 1
        assert run(path, changed) == 1
    # A4.6 raw uint53 values and unreadable code 2 are legal snapshots, even
    # when a healthy reference bucket can establish they are mismatches.
    records = admin_log()
    for field in ("u", "t_us", "g", "s", "x_us", "mc"):
        for value in ("", 2**53 - 1):
            changed = copy.deepcopy(records)
            changed[-1][field] = value
            assert run(path, changed) == 1
    records[-1]["present"] = 2
    assert run(path, records) == 1


def test_inspect_reads_stored_state_and_never_advances_time(tmp_path: Path) -> None:
    # Even after 10 s, inspect reads u=19 tokens and t=0 without lazy refill.
    # Regressing the clock to 0 and deducting gives 18, not a refilled 19.
    snapshot = inspect(19_000_000_000, 0, 1, 1, 60_000_000, 0)
    records = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        {"type": "clock", "value_ns": 10_000_000_000},
        snapshot,
        snapshot,
        {"type": "clock", "value_ns": 0},
        default_acquire(18_000_000_000, 2),
    ]
    assert run(tmp_path.joinpath("inspect-neutral.jsonl"), records) == 0


def test_inspect_keeps_stored_expired_markers(tmp_path: Path) -> None:
    # The marker's signed expiry has passed at 100 s, but no cleanup occurred.
    # Inspect reports the stored marker and does not make expiry advance hwm.
    records = admin_log()
    records.extend(
        [
            {"type": "clock", "value_ns": 100_000_000_000},
            inspect(8_000_000_000, 1_500_000, 2, 1, 61_500_000, 1),
            {"type": "clock", "value_ns": 0},
            default_acquire(7_000_000_000, 2, 1_500_000_000, g=2),
        ]
    )
    assert run(tmp_path.joinpath("inspect-expired-marker.jsonl"), records) == 0


def test_clear_requires_known_bucket_and_keeps_stored_balance(tmp_path: Path) -> None:
    path = tmp_path.joinpath("clear.jsonl")
    # Clear stores t=1 s as hwm, but the bucket stays at t=0/u=19 tokens.
    # The regressed acquire then refills to cap at 1 s and leaves 19 tokens.
    records = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        integrity_signal(),
        {"type": "clock", "value_ns": 1_000_000_000},
        {"type": "clear", "key_b64url": "YQ", "t_us": 1_000_000},
        inspect(19_000_000_000, 0, 1, 1, 60_000_000, 0),
        {"type": "clock", "value_ns": 0},
        default_acquire(19_000_000_000, 2, 1_000_000_000),
    ]
    assert run(path, records) == 0
    records[5]["key_b64url"] = "Yg"
    assert run(path, records) == 1


def test_restore_hwm_after_clock_regression(tmp_path: Path) -> None:
    # Restore at 1 s leaves 3 tokens. After clear, a raw clock of 0 is held
    # at 1 s, so the next deduction leaves 2 tokens with no refill.
    records = [
        *admin_log()[:6],
        {"type": "clear", "key_b64url": "YQ", "t_us": 1_000_000},
        {"type": "clock", "value_ns": 0},
        default_acquire(2_000_000_000, 1, 1_000_000_000, g=2),
    ]
    assert run(tmp_path.joinpath("restore-hwm.jsonl"), records) == 0


def test_restore_quarantine_survives_cleanup_until_clear(tmp_path: Path) -> None:
    # Restore at 1 s writes 3 tokens/g2/x61 s and retains quarantine. At
    # 61 s cleanup moves its index while keeping the stored u=3/t1 s.
    # Clear then resumes that same generation: 60 s refill caps at20, and
    # the next charge leaves19. The pre-restore receipt expired at60 s.
    records: list[dict[str, object]] = [
        *admin_log()[:6],
        {"type": "clock", "value_ns": 61_000_000_000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 61_000_000, "code": 31},
        inspect(3_000_000_000, 1_000_000, 2, 0, 61_000_000, 0),
        {"type": "clear", "key_b64url": "YQ", "t_us": 61_000_000},
        default_acquire(19_000_000_000, 1, 61_000_000_000, g=2),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [{"code": "receipt_expired", "t_us": 61_000_000, "credited": 0}],
            "result": "receipt_expired",
        },
    ]
    assert run(tmp_path.joinpath("restore-quarantine.jsonl"), records) == 0


def test_restore_removes_markers_and_invalidates_old_receipts(tmp_path: Path) -> None:
    records = admin_log()
    # At the same frozen 1.5 s, restore 4 tokens with new g=3, removing the
    # old g=2 marker. Restore writes s=0, x=61.5 s; old receipt is invalid.
    records.extend(
        [
            integrity_signal(),
            {
                "type": "restore",
                "key_b64url": "YQ",
                "balance_units": 4_000_000_000,
                "t_us": 1_500_000,
                "g": 3,
                "epoch_id": "e",
            },
            inspect(4_000_000_000, 1_500_000, 3, 0, 61_500_000, 0),
            {"type": "clear", "key_b64url": "YQ", "t_us": 1_500_000},
            {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
            {
                "type": "refund",
                "deduction_id": "e:YQ:1:1",
                "key_b64url": "YQ",
                "g": 1,
                "s": 1,
                "expiry_us": 60_000_000,
                "attempts": [{"code": "receipt_invalid", "t_us": 1_500_000, "credited": 0}],
                "result": "receipt_invalid",
            },
            default_acquire(3_000_000_000, 1, 1_500_000_000, g=3),
        ]
    )
    assert run(tmp_path.joinpath("restore-markers.jsonl"), records) == 0


@pytest.mark.parametrize("kind", ["restore", "clear"])
def test_successful_admin_at_clock_bound_is_mismatch(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], kind: str
) -> None:
    records = admin_log()
    index = 5 if kind == "restore" else 7
    records.insert(index, {"type": "clock", "value_ns": (2**53 - 2**43) * 1000})
    assert run(tmp_path.joinpath("clock-bound.jsonl"), records) == 1
    assert f"line {index + 2}: code: expected 'clock_error', actual 40" in capsys.readouterr().out


def test_pending_policy_keeps_admin_on_current_epoch(tmp_path: Path) -> None:
    records = admin_log()
    # The pending policy is deliberately smaller than the current restore
    # balance: it starts only at the following epoch, never when declared.
    next_policy = dict(default_policy(), capacity=1)
    records.insert(5, next_policy)
    records.extend([{"type": "epoch", "epoch_id": "next", "origin_ns": 0}])
    assert run(tmp_path.joinpath("pending-policy.jsonl"), records) == 0


def test_new_epoch_has_own_policy_and_resets_all_budgets(tmp_path: Path) -> None:
    records = admin_log()
    # Epoch A has a marker on B, g=3, hwm=1.5 s and two buckets. Restore A
    # retains quarantine and advances g=4. Epoch B starts at 1 us with a new
    # policy: 3 units/ns, 2000 units/token, capacity2 and tracked limit2.
    # Each of two new keys starts at cap4000 and its first charge leaves2000.
    records.extend(
        [
            default_acquire(19_000_000_000, 1, 1_500_000_000, g=3, key="Yg"),
            {"type": "release", "instance": "i", "deduction_id": "e:Yg:3:1"},
            {
                "type": "refund",
                "deduction_id": "e:Yg:3:1",
                "key_b64url": "Yg",
                "g": 3,
                "s": 1,
                "expiry_us": 61_500_000,
                "attempts": [{"code": "applied", "t_us": 1_500_000, "credited": 1_000_000_000}],
                "result": "applied",
            },
            integrity_signal(),
            {
                "type": "restore",
                "key_b64url": "YQ",
                "balance_units": 3_000_000_000,
                "t_us": 1_500_000,
                "g": 4,
                "epoch_id": "e",
            },
            {
                "type": "policy",
                "rate_tokens": 3,
                "rate_period_ns": 2000,
                "capacity": 2,
                "max_tracked_buckets": 2,
                "receipt_lifetime_ns": 20000,
            },
            {"type": "epoch", "epoch_id": "next", "origin_ns": 1000},
            inspect("", "", "", "", "", "", present=0),
            inspect("", "", "", "", "", "", key="Yg", present=0),
            {
                "type": "acquire",
                "instance": "i",
                "key_b64url": "YQ",
                "result": "allowed",
                "decision_ns": 1000,
                "retry_after_ns": 0,
                "u": 2000,
                "g": 1,
                "s": 1,
                "expiry_us": 21,
                "deduction_id": "next:YQ:1:1",
                "epoch_id": "next",
            },
            inspect(2000, 1, 1, 1, 21, 0),
            {
                "type": "acquire",
                "instance": "i",
                "key_b64url": "Yw",
                "result": "allowed",
                "decision_ns": 1000,
                "retry_after_ns": 0,
                "u": 2000,
                "g": 2,
                "s": 1,
                "expiry_us": 21,
                "deduction_id": "next:Yw:2:1",
                "epoch_id": "next",
            },
            {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
            {
                "type": "refund",
                "deduction_id": "e:YQ:1:1",
                "key_b64url": "YQ",
                "g": 1,
                "s": 1,
                "expiry_us": 60_000_000,
                "attempts": [{"code": "receipt_invalid", "t_us": 1, "credited": 0}],
                "result": "receipt_invalid",
            },
            inspect(2000, 1, 1, 1, 21, 0),
        ]
    )
    path = tmp_path.joinpath("epoch-reset.jsonl")
    assert run(path, records) == 0
    # Generation/sequence repeat legally in a new epoch; the opaque deduction
    # identity still must be globally unique (RCP-2 and DECISIONS G6).
    next_acquire = next(
        record for record in records if record["type"] == "acquire" and record["epoch_id"] == "next"
    )
    next_acquire["deduction_id"] = "e:YQ:1:1"
    assert run(path, records) == 1
