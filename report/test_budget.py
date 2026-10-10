"""report/budget.py (IMPLEMENTATION_PLAN H5 step 10): the budget counts a run only when its report
shows the whole builder ran, the BUD-11a check's verdict pass and valid evidence."""

from pathlib import Path

import pytest

from report import budget

STOP = '{"kind":"lifecycle","event":"stop","listener_drops":0,"log_drops":0}\n'


def test_budget_within(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """A small synthetic run (its directory under tmp_path/.out, removed afterwards) builds with
    correctness pass and valid evidence, within the budget."""
    monkeypatch.setattr(budget, "REPO", tmp_path)
    assert budget.main(["--lines", "5000"]) == 0
    out = capsys.readouterr().out
    assert "correctness pass, evidence valid True: within budget" in out
    assert list(tmp_path.joinpath(".out").iterdir()) == []


def test_budget_needs_a_passing_check(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    """A synthetic run whose evidence is invalid (a listener drop on api-1's stop line, so the
    BUD-11a check's verdict is invalid) does not measure the builder: exit 1."""
    monkeypatch.setattr(budget, "REPO", tmp_path)
    write_api_files = budget.write_api_files

    def with_listener_drop(run_dir: Path, entries: int) -> None:
        write_api_files(run_dir, entries)
        path = run_dir.joinpath("api-1.jsonl")
        text = path.read_text(encoding="utf-8")
        assert text.endswith(STOP)
        dropped = STOP.replace('"listener_drops":0', '"listener_drops":1')
        path.write_text(text.removesuffix(STOP) + dropped, encoding="utf-8")

    monkeypatch.setattr(budget, "write_api_files", with_listener_drop)
    assert budget.main(["--lines", "5000"]) == 1
    out = capsys.readouterr().out
    assert "correctness invalid, evidence valid False" in out
    assert "budget: not measured" in out
    assert list(tmp_path.joinpath(".out").iterdir()) == []
