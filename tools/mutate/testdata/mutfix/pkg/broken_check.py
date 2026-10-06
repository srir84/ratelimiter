"""A fixture test that fails on the unmutated code."""

from pkg import calc


def test_broken() -> None:
    assert calc.add(1, 1) == 3
