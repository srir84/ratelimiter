import ast
from pathlib import Path

import pytest


@pytest.mark.parametrize("module", ["model.py", "check_bud11.py"])
def test_model_uses_integers_only(module: str) -> None:
    tree = ast.parse(Path(__file__).parents[1].joinpath(module).read_text())
    operator_names = {"operator"}
    forbidden_names = {"float"}
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                assert alias.name != "math"
                if alias.name == "operator":
                    operator_names.add(alias.asname or alias.name)
        if isinstance(node, ast.ImportFrom):
            assert node.module != "math"
            if node.module == "operator":
                forbidden_names.update(
                    a.asname or a.name for a in node.names if a.name in {"truediv", "itruediv", "*"}
                )
    for node in ast.walk(tree):
        assert not (isinstance(node, ast.Name) and node.id in forbidden_names)
        assert not (
            isinstance(node, ast.Attribute)
            and isinstance(node.value, ast.Name)
            and node.value.id in operator_names
            and node.attr in {"truediv", "itruediv"}
        )
        assert not (
            isinstance(node, ast.BinOp)
            and isinstance(node.op, ast.Pow)
            and isinstance(node.right, ast.UnaryOp)
            and isinstance(node.right.op, ast.USub)
            and isinstance(node.right.operand, ast.Constant)
            and isinstance(node.right.operand.value, int)
            and node.right.operand.value > 0
        )
        assert not (isinstance(node, ast.Constant) and isinstance(node.value, float))
        assert not isinstance(node, ast.Div)
        assert not (isinstance(node, ast.Name) and node.id == "float")
        assert not (
            isinstance(node, ast.Attribute)
            and isinstance(node.value, ast.Name)
            and node.value.id == "math"
        )
