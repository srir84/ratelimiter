"""Python suppression markers; every marked line must be reported."""

a = 1  #noqa want
b = 2  # NOQA want
# flake8: noqa want
c = 3  #type:ignore want
d = 4  # type: ignore[attr-defined] want
# ruff: noqa: E501 want
# mypy: ignore-errors want
e = 5  # pyright: ignore want
f = "say noqa"  # prose that mentions noqa passes
import os  # isort: skip want
# ISORT: skip_file want
# fmt: off want
g = [1,2]  # fmt: skip want
# yapf: disable want
