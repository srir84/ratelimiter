"""Exact statistics for the report (IMPLEMENTATION_PLAN H5 step 1).

Samples are integers (ns, counts) kept in array('q') while the logs stream, and sorted once at
the end (plan choice: exact percentiles, no sketches). A percentile is the nearest rank: the
smallest value with at least p % of the values at or below it, as hitgen's stop_early judges
its p99 (§B9). Steps are assigned by planned_ns, an offset from hitgen's t0.
"""

from array import array
from bisect import bisect_right
from collections.abc import Sequence
from fractions import Fraction

# The percentiles every timing in the report gives (PRF-3).
PERCENTS = (50, 95, 99)

NS_PER_S = 1_000_000_000


def nearest_rank(ordered: Sequence[int], percent: int) -> int:
    """The nearest-rank percent-th percentile of ordered (sorted ascending, not empty)."""
    if not ordered:
        raise ValueError("a percentile of no values")
    if not 0 < percent <= 100:
        raise ValueError(f"percent {percent}: want 1 to 100")
    rank = (percent * len(ordered) + 99) // 100  # ceil(percent × n / 100), at least 1
    return ordered[rank - 1]


def percentiles(values: array[int]) -> dict[str, int | None]:
    """count, p50, p95 and p99 of values (nearest rank; null without values), sorted once."""
    ordered = sorted(values)
    out: dict[str, int | None] = {"count": len(ordered)}
    for percent in PERCENTS:
        out[f"p{percent}"] = nearest_rank(ordered, percent) if ordered else None
    return out


def new_samples() -> array[int]:
    """An empty sample array of signed 64-bit integers."""
    return array("q")


def rate(count: int, denominator: int) -> float | None:
    """count / denominator as a fraction, null for a zero denominator."""
    if denominator == 0:
        return None
    return count / denominator


def per_second(count: int, duration_ns: int) -> float | None:
    """count per second over duration_ns, null for an empty duration."""
    if duration_ns <= 0:
        return None
    return count * NS_PER_S / duration_ns


def seconds_to_ns(seconds: float) -> int:
    """A scenario duration in s as ns, as harness/config.SecondsToNs computes it: the float64
    product s × 1e9 rounded to the nearest integer, halves away from zero (Go's math.Round)."""
    product = float(seconds) * 1e9
    magnitude = int(abs(Fraction(product)) + Fraction(1, 2))  # int() floors a value ≥ 0
    return magnitude if product >= 0 else -magnitude


def step_bounds(durations_s: Sequence[float]) -> list[int]:
    """The steps' bounds in ns from hitgen's t0: step k is [bounds[k], bounds[k+1])."""
    bounds = [0]
    for seconds in durations_s:
        bounds.append(bounds[-1] + seconds_to_ns(seconds))
    return bounds


def step_of(bounds: Sequence[int], planned_ns: int) -> int:
    """The step whose [start, end) holds planned_ns; the first step before t0 and the last one
    from the run's end on (no schedule entry is planned outside the run)."""
    k = bisect_right(bounds, planned_ns) - 1
    return min(max(k, 0), len(bounds) - 2)
