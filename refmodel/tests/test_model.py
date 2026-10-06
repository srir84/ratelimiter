import json
from pathlib import Path

import pytest

from refmodel.model import Model, Policy


@pytest.mark.parametrize("name", ["ex1", "ex2", "ex3", "boundary"])
def test_vectors(name: str) -> None:
    vector = json.loads(Path(__file__).parents[1].joinpath("vectors", name + ".json").read_text())
    assert vector["arithmetic"]
    model = Model(Policy(**vector["policy"]))
    for step in vector["steps"]:
        for _ in range(step.get("repeat", 1)):
            decision = model.acquire(step.get("key", "a").encode(), step["clock_ns"])
            for field, expected in step["expected"].items():
                assert getattr(decision, field) == expected


@pytest.mark.parametrize("key", [b"", b"a" * 257, b"\xff"])
def test_invalid_key_is_state_neutral(key: bytes) -> None:
    model = Model(Policy())
    assert model.acquire(key, -1).result == "validation_error"
    assert model.hwm == 0
    assert model.buckets == {}


def test_utf8_byte_boundary_and_independence() -> None:
    model = Model(Policy(capacity=1, max_tracked_buckets=2))
    assert model.acquire(("é" * 128).encode(), 0).result == "allowed"
    assert model.acquire(b"other", 0).result == "allowed"
    assert model.acquire(("é" * 129).encode(), 0).result == "validation_error"
    assert model.acquire(b"third", 1000000000).result == "capacity_error"
    assert model.hwm == 0


@pytest.mark.parametrize(
    "field,value",
    [
        ("rate_tokens", True),
        ("capacity", 0),
        ("rate_period_ns", 1001),
        ("receipt_lifetime_ns", 1001),
        ("rate_tokens", 2**31),
        ("capacity", 2**52),
        ("receipt_lifetime_ns", 2**40 * 1000),
        ("max_tracked_buckets", 0),
    ],
)
def test_policy_rejects_invalid(field: str, value: int) -> None:
    from refmodel import RecordError

    with pytest.raises(RecordError, match=field):
        Policy(**{field: value})
