"""Hand-computed STA-3/STA-4 boundaries and CLK-4 cleanup witnesses."""

import copy
import json
from pathlib import Path
from typing import cast

import pytest

from refmodel import RecordError
from refmodel.model import TBOUND, Bucket, CleanupDecision, Decision, Model, Policy, RefundDecision
from refmodel.mutants import make_model


def test_vectors() -> None:
    vector = json.loads(Path(__file__).parents[1].joinpath("vectors", "cleanup.json").read_text())
    assert vector["arithmetic"]
    model = Model(Policy(**vector["policy"]))
    decision: Decision | RefundDecision | CleanupDecision
    for step in vector["steps"]:
        key = step.get("key", "a").encode()
        for _ in range(step.get("repeat", 1)):
            if step["op"] == "acquire":
                decision = model.acquire(key, step["clock_ns"])
            elif step["op"] == "refund":
                decision = model.refund(
                    key, step["g"], step["s"], step["expiry_us"], step["clock_ns"]
                )
            else:
                decision = model.cleanup_step(key, step["clock_ns"])
            for field, expected in step["expected"].items():
                assert getattr(decision, field) == expected
        for field, expected in step.get("bucket", {}).items():
            assert getattr(model.buckets[key], field) == expected
        if "hwm" in step:
            assert model.hwm == step["hwm"]
        if "markers" in step:
            assert len(model.markers) == step["markers"]
        if step.get("absent"):
            assert key not in model.buckets and key not in model.cleanup_due


@pytest.mark.parametrize("clock,code", [(999000, 33), (1000000, 30), (1001000, 30)])
def test_latest_expiry_edge(clock: int, code: int) -> None:
    model = Model(Policy(rate_tokens=1, rate_period_ns=1000, receipt_lifetime_ns=1000000))
    assert model.acquire(b"a", 0).expiry_us == 1000
    # The missing 1000 units refill in 1000 ns, well before x=1000000.
    assert model.cleanup_step(b"a", clock).code == code
    assert (b"a" in model.buckets) == (code != 30)


def test_one_unexpired_marker_keeps_bucket() -> None:
    model = Model(Policy(rate_tokens=1, rate_period_ns=1000, receipt_lifetime_ns=10000))
    model.acquire(b"a", 0)
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 10000, 0)
    model.refund(b"a", 1, 2, 10001, 0)
    # Seed an independent marker edge: x=10000 us, one marker ends 1 us later.
    model.buckets[b"a"] = Bucket(20000, 0, 1, 2, 10000000)
    model.cleanup_due[b"a"] = 10000000
    assert model.cleanup_step(b"a", 10000000) == CleanupDecision(31, 10000000)
    assert model.markers == {(b"a", 1, 2): (0, 10001)}
    assert model.buckets[b"a"].units == 20000
    assert model.cleanup_step(b"a", 10001000) == CleanupDecision(30, 10001000)
    assert not model.markers and not model.buckets


def test_refill_one_unit_short() -> None:
    model = Model(Policy(rate_tokens=1, rate_period_ns=10000, capacity=1, receipt_lifetime_ns=1000))
    # A balance restored to 8999 units at 0: 1000 ns adds 1000, leaving 9999.
    model.buckets[b"a"] = Bucket(8999, 0, 1, 0, 1000)
    model.cleanup_due[b"a"] = 1000
    model.generation = 1
    assert model.cleanup_step(b"a", 1000) == CleanupDecision(31, 1000)
    assert model.buckets[b"a"] == Bucket(9999, 1000, 1, 0, 1000)
    # One unit needs 1 ns, rounded up to the next store microsecond.
    assert model.cleanup_due[b"a"] == 2000
    assert model.cleanup_step(b"a", 2000) == CleanupDecision(30, 2000)


def test_expired_markers_removed_from_active_bucket() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    model.acquire(b"a", 60000000000)
    assert model.cleanup_step(b"a", 60000000000) == CleanupDecision(31, 60000000000)
    assert model.buckets[b"a"] == Bucket(19000000000, 60000000000, 1, 2, 120000000000)
    assert not model.markers
    # The cleaned marker cannot become refundable again after clock regression.
    assert model.refund(b"a", 1, 1, 60000000, 0).result == "receipt_expired"


def test_refund_keeps_newer_receipt_expiry() -> None:
    model = Model(Policy(rate_tokens=1, rate_period_ns=1000, receipt_lifetime_ns=10000))
    assert model.acquire(b"a", 0).expiry_us == 10
    assert model.acquire(b"a", 5000).expiry_us == 15
    assert model.refund(b"a", 1, 1, 10, 5000).result == "applied"
    # The first marker is due at 10 us; the latest receipt stays valid until 15 us.
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 9000) == CleanupDecision(33, 9000)
    assert vars(model) == before
    assert model.cleanup_step(b"a", 10000) == CleanupDecision(31, 10000)
    assert model.refund(b"a", 1, 2, 15, 12000).result == "applied"
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 14000) == CleanupDecision(33, 14000)
    assert vars(model) == before
    assert model.cleanup_step(b"a", 15000) == CleanupDecision(30, 15000)


def test_acquire_moves_cleanup_deadline_without_markers() -> None:
    model = Model(Policy())
    assert model.acquire(b"a", 0).expiry_us == 60000000
    assert model.acquire(b"a", 60000000000).expiry_us == 120000000
    # The second allow postpones the due entry from 60 s to its own 120 s expiry.
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 60000000000) == CleanupDecision(33, 60000000000)
    assert vars(model) == before
    assert model.cleanup_step(b"a", 119999999000) == CleanupDecision(33, 119999999000)
    assert vars(model) == before
    assert model.cleanup_step(b"a", 120000000000) == CleanupDecision(30, 120000000000)


@pytest.mark.parametrize("kind", ["gone", "bucket-not-due", "marker-not-due", "moved-not-due"])
def test_skipped_writes_nothing(kind: str) -> None:
    model = Model(Policy(rate_tokens=1, rate_period_ns=10000, capacity=1, receipt_lifetime_ns=1000))
    if kind != "gone":
        model.acquire(b"a", 0)
    if kind == "marker-not-due":
        model.refund(b"a", 1, 1, 1, 0)
    if kind == "moved-not-due":
        assert model.cleanup_step(b"a", 1000).code == 31
        # Ten thousand units need ten thousand ns; due moves from x=1000.
        assert model.cleanup_due[b"a"] == 10000
    before = copy.deepcopy(vars(model))
    clock = 2000 if kind == "moved-not-due" else 0
    assert model.cleanup_step(b"a", clock).code == 33
    assert vars(model) == before


def test_bucket_removal_frees_slot_and_generation_never_repeats() -> None:
    model = Model(Policy(max_tracked_buckets=1))
    assert model.acquire(b"a", 0).g == 1
    assert model.acquire(b"b", 60000000000).result == "capacity_error"
    assert model.cleanup_step(b"a", 60000000000).code == 30
    assert model.acquire(b"b", 0).g == 2
    assert model.cleanup_step(b"b", 120000000000).code == 30
    next_a = model.acquire(b"a", 0)
    assert (next_a.u, next_a.g, next_a.s) == (19000000000, 3, 1)


def test_cleanup_other_key_unchanged() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.acquire(b"b", 3000000000)
    model.refund(b"b", 2, 1, 63000000, 3000000000)
    bucket_b = model.buckets[b"b"]
    marker_b = model.markers[(b"b", 2, 1)]
    due_b = model.cleanup_due[b"b"]
    assert model.cleanup_step(b"a", 60000000000).code == 30
    assert model.buckets[b"b"] == bucket_b
    assert model.markers == {(b"b", 2, 1): marker_b}
    assert model.cleanup_due[b"b"] == due_b


@pytest.mark.parametrize(("missing", "code"), [("none", 31), ("bucket", 32), ("index", 33)])
def test_quarantine_retains_evidence_and_stores_hwm(missing: str, code: int) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 0)
    if missing == "bucket":
        del model.buckets[b"a"]
    elif missing == "index":
        del model.cleanup_due[b"a"]
    else:
        model.quarantined.add(b"a")
    buckets = dict(model.buckets)
    markers = dict(model.markers)
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 60000000000) == CleanupDecision(code, 60000000000)
    assert model.buckets == buckets and model.markers == markers
    if missing == "index":
        assert model.hwm == before["hwm"]
        assert vars(model) == before
    else:
        assert model.hwm == 60000000000
        assert model.quarantined == {b"a"}
        assert model.cleanup_due[b"a"] == 120000000000
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 60000001000).code == 33
    assert vars(model) == before


def test_missing_index_quarantine_mutant(monkeypatch: pytest.MonkeyPatch) -> None:
    cleanup_step = Model.cleanup_step

    def quarantine_missing_index(self: Model, key_bytes: bytes, clock_ns: int) -> CleanupDecision:
        if key_bytes not in self.cleanup_due:
            t = max(clock_ns, self.hwm)
            self.hwm = t
            self.quarantined.add(key_bytes)
            self.cleanup_due[key_bytes] = t + self.policy.receipt_lifetime_ns
            return CleanupDecision(32, t)
        return cleanup_step(self, key_bytes, clock_ns)

    monkeypatch.setattr(Model, "cleanup_step", quarantine_missing_index)
    with pytest.raises(AssertionError):
        test_quarantine_retains_evidence_and_stores_hwm("index", 33)


def test_cleanup_moves_refund_quarantine_and_stores_hwm() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.acquire(b"a", 0)
    assert model.refund(b"a", 1, 1, 60000000, 0).result == "applied"
    del model.buckets[b"a"]
    assert model.refund(b"a", 1, 2, 60000000, 1000) == RefundDecision("state_integrity_error", 1000)
    assert model.quarantined == {b"a"}
    buckets = dict(model.buckets)
    markers = dict(model.markers)
    assert model.cleanup_step(b"a", 60000000000) == CleanupDecision(31, 60000000000)
    assert model.hwm == 60000000000
    assert model.buckets == buckets and model.markers == markers
    assert model.cleanup_due[b"a"] == 120000000000
    assert model.quarantined == {b"a"}
    assert model.acquire(b"b", 0).decision_ns == 60000000000


@pytest.mark.parametrize("mutant", ["expiry_strict", "refill_uncapped", "credit_uncapped"])
def test_cleanup_preserves_mutant_rules(mutant: str) -> None:
    model = make_model(Policy(), 0, mutant)
    model.acquire(b"a", 0)
    model.refund(b"a", 1, 1, 60000000, 100000000)
    if mutant == "credit_uncapped":
        assert model.buckets[b"a"].units == 21000000000
        # Cleanup has no credit: its ordinary refill caps this stored balance.
        model.acquire(b"a", 60000000000)
        assert model.cleanup_step(b"a", 60000000000).code == 31
        assert model.buckets[b"a"].units == 19000000000
    elif mutant == "expiry_strict":
        assert model.cleanup_step(b"a", 60000000000).code == 31
        assert model.markers == {(b"a", 1, 1): (100000000, 60000000)}
        assert model.cleanup_step(b"a", 60000001000).code == 30
    else:
        # Uncapped refill gives 20b + 59.9b*10 = 619b units, not full=20b.
        assert model.cleanup_step(b"a", 60000000000).code == 31
        assert model.buckets[b"a"].units == 619000000000


def test_strict_expiry_without_marker() -> None:
    model = make_model(
        Policy(rate_tokens=1, rate_period_ns=1000, receipt_lifetime_ns=1000000),
        0,
        "expiry_strict",
    )
    assert model.acquire(b"a", 0).expiry_us == 1000
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 999000) == CleanupDecision(33, 999000)
    assert vars(model) == before
    # Strict expiry keeps x=1000 us once, then moves its deadline one microsecond.
    assert model.cleanup_step(b"a", 1000000) == CleanupDecision(31, 1000000)
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", 1000000) == CleanupDecision(33, 1000000)
    assert vars(model) == before
    assert model.cleanup_step(b"a", 1001000) == CleanupDecision(30, 1001000)


def test_clock_bound_last_valid_microsecond() -> None:
    model = Model(Policy())
    before = copy.deepcopy(vars(model))
    assert model.cleanup_step(b"a", TBOUND - 1000) == CleanupDecision(33, TBOUND - 1000)
    assert vars(model) == before


@pytest.mark.parametrize("clock", [-1000, 1500, True, TBOUND, TBOUND + 1000])
def test_invalid_clock_writes_nothing(clock: int) -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    before = copy.deepcopy(vars(model))
    if clock >= TBOUND:
        assert model.cleanup_step(b"a", clock) == CleanupDecision("clock_error")
    else:
        with pytest.raises(RecordError, match=r"cleanup.*clock_ns"):
            model.cleanup_step(b"a", clock)
    assert vars(model) == before


@pytest.mark.parametrize("key", [b"", b"a" * 257, b"\xff", "a"])
def test_cleanup_key_validation(key: bytes | str) -> None:
    model = Model(Policy())
    with pytest.raises(RecordError, match=r"cleanup.*key_bytes"):
        model.cleanup_step(cast("bytes", key), 0)
