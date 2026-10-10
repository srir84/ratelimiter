"""report/stats.py (IMPLEMENTATION_PLAN H5 step 1). Expected values are computed by hand."""

from array import array

import pytest

from report.stats import (
    nearest_rank,
    per_second,
    percentiles,
    rate,
    seconds_to_ns,
    step_bounds,
    step_of,
)


@pytest.mark.parametrize(
    ("values", "p50", "p95", "p99"),
    [
        # 1…100: rank ⌈p⌉ of 100 is the value p itself.
        (list(range(1, 101)), 50, 95, 99),
        # 1…10: ranks ⌈5⌉ = 5, ⌈9.5⌉ = 10, ⌈9.9⌉ = 10.
        (list(range(1, 11)), 5, 10, 10),
        # 1…12: ranks ⌈6⌉ = 6, ⌈11.4⌉ = 12, ⌈11.88⌉ = 12 (rounding 11.4 half up would give 11).
        (list(range(1, 13)), 6, 12, 12),
        # 0…100 (101 values): ranks ⌈50.5⌉ = 51, ⌈95.95⌉ = 96, ⌈99.99⌉ = 100.
        (list(range(101)), 50, 95, 99),
        # 1…200: ranks 100, 190, 198.
        (list(range(1, 201)), 100, 190, 198),
        # One value is every percentile.
        ([7], 7, 7, 7),
        # Unsorted input, sorted once: 1…5, ranks ⌈2.5⌉ = 3, ⌈4.75⌉ = 5, ⌈4.95⌉ = 5.
        ([5, 1, 4, 2, 3], 3, 5, 5),
        # Repeated values: [1, 5, 5, 5], ranks 2, 4, 4.
        ([5, 5, 1, 5], 5, 5, 5),
        # Negative values (a lag can be negative): [-3, -2, -1], ranks 2, 3, 3.
        ([-1, -3, -2], -2, -1, -1),
        # Beyond 2^53, exact: ranks 2, 3, 3 of [2^62, 2^62 + 1, 2^62 + 2].
        ([2**62 + 2, 2**62, 2**62 + 1], 2**62 + 1, 2**62 + 2, 2**62 + 2),
    ],
)
def test_percentile_nearest_rank(values: list[int], p50: int, p95: int, p99: int) -> None:
    got = percentiles(array("q", values))
    assert got == {"count": len(values), "p50": p50, "p95": p95, "p99": p99}
    ordered = sorted(values)
    assert [nearest_rank(ordered, p) for p in (50, 95, 99)] == [p50, p95, p99]


def test_percentile_of_nothing() -> None:
    assert percentiles(array("q")) == {"count": 0, "p50": None, "p95": None, "p99": None}
    with pytest.raises(ValueError, match="no values"):
        nearest_rank([], 50)
    with pytest.raises(ValueError, match="percent"):
        nearest_rank([1], 0)


def test_rates() -> None:
    assert rate(1, 4) == 0.25
    assert rate(0, 0) is None
    assert per_second(30, 10_000_000_000) == 3.0
    assert per_second(5, 0) is None


def test_steps_by_planned_ns() -> None:
    # 20 s, 0.5 s, 10 s → bounds 0, 20e9, 20.5e9, 30.5e9.
    bounds = step_bounds([20, 0.5, 10.0])
    assert bounds == [0, 20_000_000_000, 20_500_000_000, 30_500_000_000]
    assert [step_of(bounds, ns) for ns in (0, 19_999_999_999, 20_000_000_000)] == [0, 0, 1]
    assert [step_of(bounds, ns) for ns in (20_499_999_999, 20_500_000_000)] == [1, 2]
    # Outside the run: the first step before it, the last one after it.
    assert step_of(bounds, -1) == 0
    assert step_of(bounds, 30_500_000_000) == 2
    # Go's math.Round on the float64 product, halves away from zero: 1.0000000005 s and
    # 2.5e-9 s multiply to exactly 1000000000.5 and 2.5 ns (round half to even would give
    # 1000000000 and 2).
    assert seconds_to_ns(1.0000000005) == 1_000_000_001
    assert seconds_to_ns(2.5e-9) == 3
    assert seconds_to_ns(0.1) == 100_000_000
