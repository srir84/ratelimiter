"""Hand-computed epoch and per-key snapshot regressions from the S8 audit."""

from pathlib import Path

import pytest

from refmodel.tests.test_replay import (
    admin_log,
    default_acquire,
    default_policy,
    inspect,
    run,
)


def applied_refund(*, epoch: str = "e", key: str = "YQ", g: int = 1) -> list[dict[str, object]]:
    # One first deduction at frozen zero spends 10**9 units from cap 20*10**9.
    # Returning that token credits 10**9 units and creates one signed marker.
    deduction = f"{epoch}:{key}:{g}:1"
    return [
        {"type": "release", "instance": "i", "deduction_id": deduction},
        {
            "type": "refund",
            "deduction_id": deduction,
            "key_b64url": key,
            "g": g,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [{"code": "applied", "t_us": 0, "credited": 1_000_000_000}],
            "result": "applied",
        },
    ]


def test_new_epoch_drops_old_marker_and_applies_new_receipt(tmp_path: Path) -> None:
    # ADM-1: epoch f begins with no markers, even though its first receipt
    # repeats key/g/s from epoch e. Its own refund must apply, not find e's marker.
    records = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        *applied_refund(),
        default_policy(),
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
        default_acquire(19_000_000_000, 1, epoch="f"),
        inspect(19_000_000_000, 0, 1, 1, 60_000_000, 0),
        *applied_refund(epoch="f"),
        inspect(20_000_000_000, 0, 1, 1, 60_000_000, 1),
    ]
    assert run(tmp_path.joinpath("epoch-marker.jsonl"), records) == 0


def test_new_epoch_has_no_old_cleanup_index(tmp_path: Path) -> None:
    # Epoch e's YQ was eligible at 60 s; epoch f has never created YQ.
    # At 61 s cleanup must report 33 (gone), not quarantine an old index orphan.
    records = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        default_policy(),
        {"type": "epoch", "epoch_id": "f", "origin_ns": 0},
        {"type": "clock", "value_ns": 61_000_000_000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 61_000_000, "code": 33},
    ]
    assert run(tmp_path.joinpath("epoch-index.jsonl"), records) == 0


def test_inspect_counts_only_markers_for_its_key(tmp_path: Path) -> None:
    # Each independent key spends and returns one token at zero. Both buckets
    # hold 20 tokens, with one marker each; YQ's mc is 1, not the total 2.
    records = [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        default_acquire(19_000_000_000, 1),
        default_acquire(19_000_000_000, 1, key="Yg", g=2),
        *applied_refund(),
        *applied_refund(key="Yg", g=2),
        inspect(20_000_000_000, 0, 1, 1, 60_000_000, 1),
    ]
    assert run(tmp_path.joinpath("per-key-markers.jsonl"), records) == 0


def test_restore_cannot_name_a_past_epoch(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    records = [
        default_policy(),
        {"type": "epoch", "epoch_id": "old", "origin_ns": 0},
        *admin_log(),
    ]
    path = tmp_path.joinpath("past-epoch-restore.jsonl")
    assert run(path, records) == 0
    restore_index = next(
        index for index, record in enumerate(records) if record["type"] == "restore"
    )
    records[restore_index]["epoch_id"] = "old"
    assert run(path, records) == 2
    assert (
        f"line {restore_index + 1}: field epoch_id: differs from current epoch"
        in capsys.readouterr().out
    )
