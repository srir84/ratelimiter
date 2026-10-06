"""Shuffle the collected tests (DESIGN §C1; IMPLEMENTATION_PLAN S5 step 17).

The order comes from random.Random(seed); --shuffle-seed=N sets the seed, otherwise it is
drawn from random.SystemRandom(). The report header shows it, so --shuffle-seed=<N> repeats
a failing order.
"""

import random

import pytest

SEED = pytest.StashKey[int]()


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption(
        "--shuffle-seed",
        type=int,
        default=None,
        help="seed of the test order (default: drawn from random.SystemRandom())",
    )


def pytest_configure(config: pytest.Config) -> None:
    seed = config.getoption("--shuffle-seed")
    if seed is None:
        seed = random.SystemRandom().getrandbits(32)
    if not isinstance(seed, int):
        raise pytest.UsageError("--shuffle-seed: expected an integer")
    config.stash[SEED] = seed


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    random.Random(config.stash[SEED]).shuffle(items)


def pytest_report_header(config: pytest.Config) -> str:
    return f"shuffle-seed: {config.stash[SEED]}"
