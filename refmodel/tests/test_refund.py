import json
from pathlib import Path

import pytest

from refmodel.model import Decision, Model, Policy, RefundDecision


@pytest.mark.parametrize("name", ["ex4", "ex5"])
def test_vectors(name: str) -> None:
    vector = json.loads(Path(__file__).parents[1].joinpath("vectors", name + ".json").read_text())
    assert vector["arithmetic"]
    model = Model(Policy())
    decision: Decision | RefundDecision
    for step in vector["steps"]:
        for _ in range(step.get("repeat", 1)):
            if step["op"] == "acquire":
                decision = model.acquire(b"a", step["clock_ns"])
            else:
                decision = model.refund(b"a", 1, 1, 60000000, step["clock_ns"])
            for field, wanted in step["expected"].items():
                assert getattr(decision, field) == wanted


def test_capped_repeat_and_expiry() -> None:
    model = Model(Policy())
    model.acquire(b"a", 0)
    # 100 ms restores the spent token: credit is zero, but still marked.
    first = model.refund(b"a", 1, 1, 60000000, 100000000)
    assert (first.result, first.credited, first.decision_ns) == ("applied", 0, 100000000)
    repeat = model.refund(b"a", 1, 1, 60000000, 200000000)
    assert (repeat.result, repeat.decision_ns) == ("already_applied", 100000000)
    assert model.hwm == 200000000
    assert model.refund(b"a", 1, 1, 60000000, 60000000000).result == "receipt_expired"
    assert model.refund(b"a", 1, 1, 60000000, 0).result == "receipt_expired"


def test_order_and_capacity() -> None:
    model = Model(Policy())
    assert model.refund(b"absent", 1, 1, 10, 0).result == "state_integrity_error"
    assert not model.buckets
    model.acquire(b"a", 0)
    assert model.refund(b"a", 2, 1, 10, 0).result == "receipt_invalid"
    assert model.refund(b"a", 1, 2, 10, 0).result == "receipt_invalid"
    assert model.refund(b"a", 1, 1, 10, 10000, epoch_matches=False).result == "receipt_invalid"
    assert model.refund(b"a", 2, 2, 10, 10000).result == "receipt_expired"
    model = Model(Policy())
    model.acquire(b"a", 0)
    model.acquire(b"a", 0)
    assert model.refund(b"a", 1, 1, 10, 0, max_live_markers=1).result == "applied"
    assert model.refund(b"a", 1, 1, 10, 0, max_live_markers=1).result == "already_applied"
    assert model.refund(b"a", 1, 2, 10, 0, max_live_markers=1).result == "capacity_error"
