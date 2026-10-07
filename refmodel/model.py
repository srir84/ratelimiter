"""Token bucket and receipt refunds, derived from BUD-13 and CLK-4."""

from dataclasses import dataclass

from refmodel import RecordError

TBOUND = (2**53 - 2**43) * 1000


@dataclass(frozen=True)
class Policy:
    rate_tokens: int = 10
    rate_period_ns: int = 1_000_000_000
    capacity: int = 20
    max_tracked_buckets: int = 1000
    receipt_lifetime_ns: int = 60_000_000_000

    def __post_init__(self) -> None:
        for field in self.__dataclass_fields__:
            value = getattr(self, field)
            if type(value) is not int or value <= 0:
                raise RecordError("policy", field, "expected positive integer")
        for field in ("rate_period_ns", "receipt_lifetime_ns"):
            if getattr(self, field) % 1000:
                raise RecordError("policy", field, "expected microsecond granularity")
        if self.rate_tokens >= 2**31:
            raise RecordError("policy", "rate_tokens", "exceeds DESIGN bound")
        if self.capacity * self.rate_period_ns >= 2**52:
            raise RecordError("policy", "capacity", "cap_units exceeds DESIGN bound")
        if self.receipt_lifetime_ns // 1000 >= 2**40:
            raise RecordError("policy", "receipt_lifetime_ns", "exceeds DESIGN bound")


@dataclass(frozen=True)
class Decision:
    result: str
    decision_ns: int = 0
    retry_after_ns: int = 0
    u: int = 0
    g: int = 0
    s: int = 0
    expiry_us: int = 0


@dataclass(frozen=True)
class Bucket:
    units: int
    time_ns: int
    generation: int
    sequence: int


@dataclass(frozen=True)
class RefundDecision:
    result: str
    decision_ns: int = 0
    credited: int = 0


class Model:
    def __init__(self, policy: Policy, origin_ns: int = 0) -> None:
        if type(origin_ns) is not int or not 0 <= origin_ns < TBOUND or origin_ns % 1000:
            raise RecordError("epoch", "origin_ns", "expected bounded microsecond time")
        self.policy = policy
        self.hwm = origin_ns
        self.buckets: dict[bytes, Bucket] = {}
        self.generation = 0
        self.markers: dict[tuple[bytes, int, int], tuple[int, int]] = {}
        self.mutant = ""
        self.quarantined: set[bytes] = set()

    def acquire(self, key_bytes: bytes, clock_ns: int) -> Decision:
        if not isinstance(key_bytes, bytes):
            raise RecordError("acquire", "key_bytes", "expected bytes")
        if not 1 <= len(key_bytes) <= 256:
            return Decision("validation_error")
        try:
            key_bytes.decode("utf-8")
        except UnicodeDecodeError:
            return Decision("validation_error")
        if type(clock_ns) is not int:
            raise RecordError("acquire", "clock_ns", "expected integer")
        if not 0 <= clock_ns < TBOUND:
            return Decision("clock_error")
        if clock_ns % 1000:
            raise RecordError("acquire", "clock_ns", "expected microsecond granularity")
        time_ns = max(clock_ns, self.hwm)
        p = self.policy
        cap = p.capacity * p.rate_period_ns
        if key_bytes in self.quarantined:
            return Decision("state_integrity_error", time_ns)
        bucket = self.buckets.get(key_bytes)
        if bucket is None:
            if len(self.buckets) >= p.max_tracked_buckets:
                return Decision("capacity_error", time_ns)
            units, generation, sequence = (
                (cap - p.rate_period_ns if self.mutant == "new_bucket_short" else cap),
                self.generation + 1,
                0,
            )
        else:
            # Clamp elapsed time to ceil(time to full) before multiplying.
            elapsed = min(
                time_ns - bucket.time_ns,
                (cap - bucket.units + p.rate_tokens - 1) // p.rate_tokens,
            )
            units = (
                bucket.units + (time_ns - bucket.time_ns) * p.rate_tokens
                if self.mutant == "refill_uncapped"
                else min(cap, bucket.units + elapsed * p.rate_tokens)
            )
            generation, sequence = bucket.generation, bucket.sequence
        if units < p.rate_period_ns or (
            self.mutant == "allow_strict" and units == p.rate_period_ns
        ):
            retry = (p.rate_period_ns - units + p.rate_tokens - 1) // p.rate_tokens
            if self.mutant == "retry_floor":
                retry = (p.rate_period_ns - units) // p.rate_tokens
            if self.mutant == "hwm_on_limited":
                self.hwm = time_ns
            return Decision("limited", time_ns, retry, units)
        units -= p.rate_period_ns
        sequence += 1
        self.buckets[key_bytes] = Bucket(units, time_ns, generation, sequence)
        self.generation = max(self.generation, generation)
        self.hwm = time_ns
        return Decision(
            "allowed",
            time_ns,
            0,
            units,
            generation,
            sequence,
            (time_ns + p.receipt_lifetime_ns) // 1000,
        )

    def refund(
        self,
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
        if not isinstance(key_bytes, bytes):
            raise RecordError("refund", "key_bytes", "expected bytes")
        for field, value in (("g", g), ("s", s), ("expiry_us", expiry_us), ("clock_ns", clock_ns)):
            if type(value) is not int:
                raise RecordError("refund", field, "expected integer")
        for field, limit in (
            ("max_live_markers", max_live_markers),
            ("max_markers_per_key", max_markers_per_key),
        ):
            if limit is not None and (type(limit) is not int or limit < 1):
                raise RecordError("refund", field, "expected positive integer")
        if not 0 <= clock_ns < TBOUND:
            return RefundDecision("clock_error")
        if clock_ns % 1000:
            raise RecordError("refund", "clock_ns", "expected microsecond granularity")
        t = max(clock_ns, self.hwm)
        self.hwm = t
        if not epoch_matches:
            return RefundDecision("receipt_invalid", t)
        expired = t > expiry_us * 1000 if self.mutant == "expiry_strict" else t >= expiry_us * 1000
        if expired:
            return RefundDecision("receipt_expired", t)
        bucket = self.buckets.get(key_bytes)
        if bucket is None or key_bytes in self.quarantined:
            self.quarantined.add(key_bytes)
            return RefundDecision("state_integrity_error", t)
        if g != bucket.generation or not 1 <= s <= bucket.sequence:
            return RefundDecision("receipt_invalid", t)
        identity = (key_bytes, g, s)
        if identity in self.markers:
            return RefundDecision("already_applied", self.markers[identity][0])
        if (max_live_markers is not None and len(self.markers) >= max_live_markers) or (
            max_markers_per_key is not None
            and sum(k[0] == key_bytes for k in self.markers) >= max_markers_per_key
        ):
            return RefundDecision("capacity_error", t)
        p = self.policy
        cap = p.capacity * p.rate_period_ns
        units = (
            bucket.units + (t - bucket.time_ns) * p.rate_tokens
            if self.mutant == "refill_uncapped"
            else min(cap, bucket.units + (t - bucket.time_ns) * p.rate_tokens)
        )
        credited = min(p.rate_period_ns, max(0, cap - units))
        if self.mutant == "credit_before_refill":
            credited = min(p.rate_period_ns, cap - bucket.units)
        if self.mutant == "credit_uncapped":
            credited = p.rate_period_ns
        self.buckets[key_bytes] = Bucket(units + credited, t, g, bucket.sequence)
        self.markers[identity] = (t, expiry_us)
        return RefundDecision("applied", t, credited)
