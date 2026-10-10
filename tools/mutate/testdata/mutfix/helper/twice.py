"""pkg's cross-package tests import this sibling module, and it imports pkg back."""

from pkg import calc


def twice(n: int) -> int:
    return calc.add(n, n)
