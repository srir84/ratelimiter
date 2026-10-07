"""Eight individually selectable SPEC rule mutations."""

from refmodel import RecordError
from refmodel.model import Model, Policy

NAMES = frozenset(
    {
        "refill_uncapped",
        "allow_strict",
        "credit_uncapped",
        "hwm_on_limited",
        "expiry_strict",
        "retry_floor",
        "new_bucket_short",
        "credit_before_refill",
    }
)


def make_model(policy: Policy, origin_ns: int, name: str = "") -> Model:
    if name and name not in NAMES:
        raise RecordError("arguments", "mutant", f"unknown mutant {name!r}")
    model = Model(policy, origin_ns)
    model.mutant = name
    return model
