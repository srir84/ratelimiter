"""Fixture tests that import a sibling package (helper), which imports pkg in turn."""

from helper import twice


def test_twice() -> None:
    assert twice.twice(2) == 4
