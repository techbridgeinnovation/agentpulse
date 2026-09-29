import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).parent))
sys.path.insert(0, str(pathlib.Path(__file__).parent.parent / "src"))

import pytest


@pytest.fixture(autouse=True)
def _forget_refusals():
    # Refusals are remembered for the whole process, so each test starts with none.
    from agentpulse import governance

    governance._refused.clear()
    yield
    governance._refused.clear()
