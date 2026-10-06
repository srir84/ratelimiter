"""Fixture tests. The file name keeps pytest from collecting them unless they are named."""

import pytest

from pkg import calc


def test_add() -> None:
    assert calc.add(2, 3) == 5


def test_add_fail() -> None:
    if calc.add(2, 3) != 5:
        pytest.fail("add(2, 3) is not 5")


def test_label() -> None:
    assert calc.label(5) == "5"
