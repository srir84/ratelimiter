"""SPEC T-INT-7 loss, orphan repair and old-receipt recovery sequences."""

from pathlib import Path

import pytest

from refmodel import replay as replay_module
from refmodel.model import AdminDecision, Decision, Model, Policy, RefundDecision
from refmodel.tests.test_replay import (
    default_acquire,
    default_policy,
    inspect,
    integrity_signal,
    run,
)


def depleted_log() -> list[dict[str, object]]:
    # Twenty frozen charges spend 20 * 10**9 units: g1, s20, u0, x60s.
    return [
        default_policy(),
        {"type": "epoch", "epoch_id": "e", "origin_ns": 0},
        *(default_acquire((20 - s) * 1_000_000_000, s) for s in range(1, 21)),
    ]


def test_int7a_clear_without_restore_stays_refused(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    models: list[Model] = []

    class LostBucketModel(Model):
        def acquire(self, key_bytes: bytes, clock_ns: int) -> Decision:
            decision = super().acquire(key_bytes, clock_ns)
            if decision.s == 20:
                # External loss after depletion preserves the bucket's index.
                del self.buckets[key_bytes]
                self.quarantined.add(key_bytes)
            return decision

    def lost_bucket_model(policy: Policy, origin_ns: int, mutant: str = "") -> Model:
        model = LostBucketModel(policy, origin_ns)
        model.mutant = mutant
        models.append(model)
        return model

    monkeypatch.setattr(replay_module, "make_model", lost_bucket_model)
    records = [*depleted_log(), integrity_signal()]
    path = tmp_path.joinpath("clear-without-restore.jsonl")
    # Refused admin operations emit no record; the real refusal keeps loss.
    assert run(path, records) == 0
    assert models[-1].clear(b"a", 0) == AdminDecision(41, reason="bucket_missing")
    assert models[-1].quarantined == {b"a"} and not models[-1].buckets
    # A fabricated success for that refusal must disagree with the model.
    records.append({"type": "clear", "key_b64url": "YQ", "t_us": 0})
    assert run(path, records) == 1
    assert f"line {len(records)}: code: expected 41, actual 40" in capsys.readouterr().out


def test_int7e_restore_repairs_missing_marker_payload(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    class MissingMarkerModel(Model):
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
            if s == 2:
                # The compact map combines marker identities and payloads;
                # it has no separate marker index. Retain an attributable
                # identity with a missing payload as the orphan witness.
                vars(self)["markers"][(key_bytes, g, 1)] = None
                self.quarantined.add(key_bytes)
            return super().refund(
                key_bytes,
                g,
                s,
                expiry_us,
                clock_ns,
                epoch_matches=epoch_matches,
                max_live_markers=max_live_markers,
                max_markers_per_key=max_markers_per_key,
            )

    def missing_marker_model(policy: Policy, origin_ns: int, mutant: str = "") -> Model:
        model = MissingMarkerModel(policy, origin_ns)
        model.mutant = mutant
        return model

    monkeypatch.setattr(replay_module, "make_model", missing_marker_model)
    records: list[dict[str, object]] = [
        *depleted_log(),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [{"code": 3, "t_us": 0, "credited": 1_000_000_000}],
            "result": "applied",
        },
        # Spend the returned token before the fault: the bucket is depleted.
        default_acquire(0, 21),
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:2"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:2",
            "key_b64url": "YQ",
            "g": 1,
            "s": 2,
            "expiry_us": 60_000_000,
            "attempts": [{"code": 23, "t_us": 0, "credited": 0}],
            "result": "unresolved",
        },
        {"type": "clock", "value_ns": 1_000_000_000},
        {
            "type": "restore",
            "key_b64url": "YQ",
            "balance_units": 3_000_000_000,
            "t_us": 1_000_000,
            "g": 2,
            "epoch_id": "e",
        },
        inspect(3_000_000_000, 1_000_000, 2, 0, 61_000_000, 0),
        {"type": "clear", "key_b64url": "YQ", "t_us": 1_000_000},
        # No elapsed refill: restored 3 tokens minus one leaves 2 tokens.
        default_acquire(2_000_000_000, 1, 1_000_000_000, g=2),
    ]
    assert run(tmp_path.joinpath("restore-marker-orphan.jsonl"), records) == 0


def test_int7f_full_restore_old_receipt_and_eventual_cleanup(tmp_path: Path) -> None:
    records: list[dict[str, object]] = [
        *depleted_log(),
        integrity_signal(),
        {"type": "clock", "value_ns": 1_000_000_000},
        {
            "type": "restore",
            "key_b64url": "YQ",
            "balance_units": 20_000_000_000,
            "t_us": 1_000_000,
            "g": 2,
            "epoch_id": "e",
        },
        {"type": "clear", "key_b64url": "YQ", "t_us": 1_000_000},
        {"type": "release", "instance": "i", "deduction_id": "e:YQ:1:1"},
        {
            "type": "refund",
            "deduction_id": "e:YQ:1:1",
            "key_b64url": "YQ",
            "g": 1,
            "s": 1,
            "expiry_us": 60_000_000,
            "attempts": [{"code": 10, "t_us": 1_000_000, "credited": 0}],
            "result": "receipt_invalid",
        },
        inspect(20_000_000_000, 1_000_000, 2, 0, 61_000_000, 0),
        # Restore at 1s plus the 60s lifetime reaches eligibility at 61s.
        {"type": "clock", "value_ns": 61_000_000_000},
        {"type": "cleanup", "key_b64url": "YQ", "t_us": 61_000_000, "code": 30},
        inspect("", "", "", "", "", "", present=0),
        # The invalid old receipt did not quarantine the key: a fresh g3 works.
        default_acquire(19_000_000_000, 1, 61_000_000_000, g=3),
    ]
    assert run(tmp_path.joinpath("restore-full-old-receipt.jsonl"), records) == 0
