"""Hand-computed ADM-2, ADM-3 and epoch recovery witnesses."""

import copy
import json
from dataclasses import replace
from pathlib import Path
from typing import cast

import pytest

from refmodel import RecordError
from refmodel.model import TBOUND, AdminDecision, Bucket, Decision, Model, Policy, RefundDecision
from refmodel.mutants import NAMES, make_model


def test_restore_vector() -> None:
    vector = json.loads(Path(__file__).parents[1].joinpath("vectors", "restore.json").read_text())
    assert vector["arithmetic"]
    model = Model(Policy(**vector["policy"]))
    decision: AdminDecision | Decision | RefundDecision
    for step in vector["steps"]:
        key = step.get("key", "a").encode()
        if step["op"] == "delete_bucket":
            del model.buckets[key]
            continue
        for _ in range(step.get("repeat", 1)):
            if step["op"] == "acquire":
                decision = model.acquire(key, step["clock_ns"])
            elif step["op"] == "refund":
                decision = model.refund(
                    key, step["g"], step["s"], step["expiry_us"], step["clock_ns"]
                )
            elif step["op"] == "restore":
                decision = model.restore(key, step["balance_units"], step["clock_ns"])
            else:
                decision = model.clear(key, step["clock_ns"])
            for field, expected in step["expected"].items():
                assert getattr(decision, field) == expected
        for field, expected in step.get("bucket", {}).items():
            assert getattr(model.buckets[key], field) == expected
        if "quarantined" in step:
            assert (key in model.quarantined) == step["quarantined"]
        if "due_ns" in step:
            assert model.cleanup_due[key] == step["due_ns"]
        if "hwm" in step:
            assert model.hwm == step["hwm"]


def test_restore_removes_key_orphans_and_preserves_other_keys() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    model.acquire(b"b", 0)
    model.refund(b"b", 2, 1, 60000000, 0)
    # A lost bucket and malformed attributable marker and index are all repaired.
    del model.buckets[b"a"]
    model.markers[(b"a", 1, 999)] = (-1000, -1)
    model.markers[cast("tuple[bytes, int, int]", (b"a",))] = (0, 60000000)
    model.cleanup_due[b"a"] = -1000
    model.quarantined.add(b"a")
    assert model.restore(b"a", 3000000000, 1000000000) == AdminDecision(40, 1000000000, 3)
    assert model.buckets[b"a"] == Bucket(3000000000, 1000000000, 3, 0, 61000000000)
    assert model.buckets[b"b"] == Bucket(20000000000, 0, 2, 1, 60000000000)
    assert model.markers == {(b"b", 2, 1): (0, 60000000)}
    assert model.cleanup_due == {b"a": 1000000000, b"b": 60000000000}
    assert model.quarantined == {b"a"}
    assert model.hwm == 1000000000 and model.generation == 3
    assert model.clear(b"a", 1000000000) == AdminDecision(40, 1000000000)
    assert model.acquire(b"a", 1500000000) == Decision(
        "allowed", 1500000000, 0, 7000000000, 3, 1, 61500000
    )


def test_quarantined_cleanup_keeps_restored_bucket_then_clear_resumes_it() -> None:
    model = Model(Policy())
    for _ in range(20):
        model.acquire(b"a", 0)
    del model.buckets[b"a"]
    assert model.refund(b"a", 1, 1, 60000000, 0).result == "state_integrity_error"
    assert model.restore(b"a", 3000000000, 1000000000) == AdminDecision(40, 1000000000, 2)
    assert model.acquire(b"a", 1500000000).result == "state_integrity_error"
    assert model.cleanup_step(b"a", 61000000000).code == 31
    assert model.buckets[b"a"] == Bucket(3000000000, 1000000000, 2, 0, 61000000000)
    assert model.cleanup_due[b"a"] == 121000000000
    assert model.clear(b"a", 0) == AdminDecision(40, 61000000000)
    assert model.cleanup_due[b"a"] == 61000000000
    assert model.buckets[b"a"] == Bucket(3000000000, 1000000000, 2, 0, 61000000000)
    # Refill: min(20b, 3b + (61s - 1s) * 10) = 20b; deduct 1b.
    assert model.acquire(b"a", 0) == Decision(
        "allowed", 61000000000, 0, 19000000000, 2, 1, 121000000
    )
    assert model.refund(b"a", 1, 1, 60000000, 0) == RefundDecision("receipt_expired", 61000000000)
    assert not model.quarantined


def test_restore_invalidates_old_receipt_and_keeps_new_bucket_until_restore_expiry() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.quarantined.add(b"a")
    assert model.restore(b"a", 20000000000, 1000000000) == AdminDecision(40, 1000000000, 2)
    assert model.clear(b"a", 1000000000).code == 40
    assert model.refund(b"a", 1, 1, 60000000, 1000000000) == RefundDecision(
        "receipt_invalid", 1000000000
    )
    assert not model.quarantined and not model.markers
    assert model.cleanup_step(b"a", 60000000000).code == 31
    assert b"a" in model.buckets
    assert model.refund(b"a", 1, 1, 60000000, 60000000000).result == "receipt_expired"
    assert model.cleanup_step(b"a", 61000000000).code == 30
    assert not model.buckets and not model.quarantined
    assert model.acquire(b"a", 0).g == 3


def test_clear_preserves_consistent_bucket_and_markers_without_refill() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    model.quarantined.add(b"a")
    assert model.clear(b"a", 1000000000) == AdminDecision(40, 1000000000)
    assert model.buckets[b"a"] == Bucket(19000000000, 0, 1, 2, 60000000000)
    assert model.markers == {(b"a", 1, 1): (0, 60000000)}
    assert model.cleanup_due[b"a"] == 1000000000
    assert not model.quarantined and model.hwm == 1000000000


@pytest.mark.parametrize("operation", ["restore", "clear"])
@pytest.mark.parametrize(
    "reason", ["not_quarantined", "epoch_changed", "fingerprint_mismatch", "integrity_hold"]
)
def test_admin_refused_operation_changes_nothing(operation: str, reason: str) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    if reason != "not_quarantined":
        model.quarantined.add(b"a")
    before = copy.deepcopy(vars(model))
    options = {
        "epoch_matches": reason != "epoch_changed",
        "fingerprint_matches": reason != "fingerprint_mismatch",
        "integrity_held": reason == "integrity_hold",
    }
    decision = (
        model.restore(b"a", 3000000000, 1000000000, **options)
        if operation == "restore"
        else model.clear(b"a", 1000000000, **options)
    )
    assert decision == AdminDecision(41, reason=reason)
    assert vars(model) == before


@pytest.mark.parametrize("balance", [-1, 20000000001])
def test_restore_balance_refusal_changes_nothing(balance: int) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    model.quarantined.add(b"a")
    before = copy.deepcopy(vars(model))
    assert model.restore(b"a", balance, 1000000000) == AdminDecision(
        41, reason="balance_out_of_range"
    )
    assert vars(model) == before


@pytest.mark.parametrize("balance", [0, 20000000000])
def test_restore_balance_inclusive_boundaries(balance: int) -> None:
    model = Model(Policy())
    model.quarantined.add(b"a")
    assert model.restore(b"a", balance, 0) == AdminDecision(40, 0, 1)
    assert model.buckets[b"a"] == Bucket(balance, 0, 1, 0, 60000000000)
    assert model.quarantined == {b"a"}


def test_clear_flag_only_refusal_changes_nothing() -> None:
    model = Model(Policy())
    model.quarantined.add(b"a")
    before = copy.deepcopy(vars(model))
    assert model.clear(b"a", 1000000000) == AdminDecision(41, reason="bucket_missing")
    assert vars(model) == before


@pytest.mark.parametrize(
    "corruption,reason",
    [
        ("index_missing", "index_missing"),
        ("index_negative", "index_malformed"),
        ("index_fractional_us", "index_malformed"),
        ("bucket_negative", "bucket_malformed"),
        ("bucket_over_cap", "bucket_malformed"),
        ("bucket_generation", "bucket_malformed"),
        ("bucket_sequence", "bucket_malformed"),
        ("bucket_future_time", "bucket_malformed"),
        ("bucket_expiry", "bucket_malformed"),
        ("marker_generation", "marker_malformed"),
        ("marker_sequence", "marker_malformed"),
        ("marker_time", "marker_malformed"),
        ("marker_expiry", "marker_malformed"),
        ("marker_shape", "marker_malformed"),
    ],
)
def test_clear_consistency_refusal_changes_nothing(corruption: str, reason: str) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    model.quarantined.add(b"a")
    bucket = model.buckets[b"a"]
    if corruption == "index_missing":
        del model.cleanup_due[b"a"]
    elif corruption == "index_negative":
        model.cleanup_due[b"a"] = -1000
    elif corruption == "index_fractional_us":
        model.cleanup_due[b"a"] = 1
    elif corruption == "bucket_negative":
        model.buckets[b"a"] = replace(bucket, units=-1)
    elif corruption == "bucket_over_cap":
        model.buckets[b"a"] = replace(bucket, units=20000000001)
    elif corruption == "bucket_generation":
        model.buckets[b"a"] = replace(bucket, generation=2)
    elif corruption == "bucket_sequence":
        model.buckets[b"a"] = replace(bucket, sequence=2**53)
    elif corruption == "bucket_future_time":
        model.buckets[b"a"] = replace(bucket, time_ns=1000)
    elif corruption == "bucket_expiry":
        model.buckets[b"a"] = replace(bucket, latest_expiry_ns=1)
    elif corruption == "marker_generation":
        model.markers[(b"a", 2, 1)] = (0, 60000000)
    elif corruption == "marker_sequence":
        model.markers[(b"a", 1, 2)] = (0, 60000000)
    elif corruption == "marker_time":
        model.markers[(b"a", 1, 1)] = (1000, 60000000)
    elif corruption == "marker_expiry":
        model.markers[(b"a", 1, 1)] = (0, 60000001)
    else:
        model.markers[cast("tuple[bytes, int, int]", (b"a",))] = (0, 60000000)
    before = copy.deepcopy(vars(model))
    assert model.clear(b"a", 1000000000) == AdminDecision(41, reason=reason)
    assert vars(model) == before


@pytest.mark.parametrize("operation", ["restore", "clear"])
@pytest.mark.parametrize("clock", [-1000, True, 1500, TBOUND, TBOUND + 1000])
def test_admin_bad_clock_changes_nothing(operation: str, clock: int) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    model.quarantined.add(b"a")
    before = copy.deepcopy(vars(model))
    if clock == -1000 or clock >= TBOUND:
        decision = (
            model.restore(b"a", 3000000000, clock)
            if operation == "restore"
            else model.clear(b"a", clock)
        )
        assert decision == AdminDecision("clock_error")
    else:
        with pytest.raises(RecordError, match=operation + r".*clock_ns"):
            if operation == "restore":
                model.restore(b"a", 3000000000, clock)
            else:
                model.clear(b"a", clock)
    assert vars(model) == before


@pytest.mark.parametrize("operation", ["restore", "clear"])
@pytest.mark.parametrize("hwm", [-1000, 1, TBOUND])
def test_admin_malformed_hwm_changes_nothing(operation: str, hwm: int) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.quarantined.add(b"a")
    model.hwm = hwm
    before = copy.deepcopy(vars(model))
    decision = (
        model.restore(b"a", 3000000000, 1000000000)
        if operation == "restore"
        else model.clear(b"a", 1000000000)
    )
    assert decision == AdminDecision(41, reason="hwm_malformed")
    assert vars(model) == before


def test_admin_missing_hwm_and_exhausted_generation_change_nothing() -> None:
    model = Model(Policy())
    model.quarantined.add(b"a")
    del model.hwm
    before = copy.deepcopy(vars(model))
    assert model.restore(b"a", 0, 0) == AdminDecision(41, reason="hwm_missing")
    assert model.clear(b"a", 0) == AdminDecision(41, reason="hwm_missing")
    assert vars(model) == before
    model.hwm = 0
    model.generation = 2**53 - 1
    before = copy.deepcopy(vars(model))
    assert model.restore(b"a", 0, 0) == AdminDecision(41, reason="epoch_record_malformed")
    assert vars(model) == before


@pytest.mark.parametrize("operation", ["restore", "clear"])
@pytest.mark.parametrize("key", [b"", b"a" * 257, b"\xff", "a"])
def test_admin_key_validation(operation: str, key: bytes | str) -> None:
    model = Model(Policy())
    with pytest.raises(RecordError, match=operation + r".*key_bytes"):
        if operation == "restore":
            model.restore(cast("bytes", key), 0, 0)
        else:
            model.clear(cast("bytes", key), 0)
    assert not model.buckets and model.hwm == 0


def test_restore_balance_type_validation() -> None:
    model = Model(Policy())
    model.quarantined.add(b"a")
    with pytest.raises(RecordError, match=r"restore.*balance_units"):
        model.restore(b"a", True, 0)
    assert not model.buckets and model.hwm == 0


@pytest.mark.parametrize("operation", ["restore", "clear"])
def test_admin_integrity_fence_precedes_stale_epoch_and_key_checks(operation: str) -> None:
    model = Model(Policy())
    before = copy.deepcopy(vars(model))
    decision = (
        model.restore(b"a", -1, TBOUND, integrity_held=True, epoch_matches=False)
        if operation == "restore"
        else model.clear(b"a", TBOUND, integrity_held=True, epoch_matches=False)
    )
    assert decision == AdminDecision(41, reason="integrity_hold")
    assert vars(model) == before


def test_new_epoch_uses_own_policy_and_resets_every_budget() -> None:
    epoch_a = Model(Policy(capacity=2, max_tracked_buckets=2))
    epoch_a.acquire(b"a", 0)
    epoch_a.acquire(b"a", 0)
    epoch_a.refund(b"a", 1, 1, 60000000, 0)
    epoch_a.acquire(b"b", 0)
    epoch_a.quarantined.add(b"b")
    before_a = copy.deepcopy(vars(epoch_a))
    epoch_b = Model(Policy(capacity=1, max_tracked_buckets=2, receipt_lifetime_ns=1000000000), 1000)
    assert epoch_b.hwm == 1000 and epoch_b.generation == 0
    assert not epoch_b.buckets and not epoch_b.markers and not epoch_b.quarantined
    assert not epoch_b.cleanup_due
    assert epoch_b.acquire(b"a", 1000) == Decision("allowed", 1000, 0, 0, 1, 1, 1000001)
    assert epoch_b.acquire(b"b", 1000) == Decision("allowed", 1000, 0, 0, 2, 1, 1000001)
    assert epoch_b.acquire(b"a", 1000).result == "limited"
    assert epoch_b.refund(b"a", 1, 1, 60000000, 1000, epoch_matches=False) == RefundDecision(
        "receipt_invalid", 1000
    )
    assert not epoch_b.markers and not epoch_b.quarantined
    assert vars(epoch_a) == before_a


@pytest.mark.parametrize("mutant", sorted(NAMES))
def test_admin_assignments_preserve_all_eight_mutants(mutant: str) -> None:
    model = make_model(Policy(), 0, mutant)
    model.quarantined.add(b"a")
    assert model.restore(b"a", 3000000000, 1000000000) == AdminDecision(40, 1000000000, 1)
    assert model.clear(b"a", 1000000000) == AdminDecision(40, 1000000000)
    assert model.buckets[b"a"] == Bucket(3000000000, 1000000000, 1, 0, 61000000000)
    assert model.mutant == mutant


def test_restore_and_clear_absorb_clock_regression_without_refill() -> None:
    model = Model(Policy(), 1000000000)
    model.quarantined.add(b"a")
    assert model.restore(b"a", 3000000000, 0) == AdminDecision(40, 1000000000, 1)
    assert model.clear(b"a", 0) == AdminDecision(40, 1000000000)
    assert model.buckets[b"a"] == Bucket(3000000000, 1000000000, 1, 0, 61000000000)
    assert model.hwm == 1000000000
