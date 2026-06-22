"""Shared pytest fixtures for the canary suite."""

from __future__ import annotations

import os
import sys
import uuid

# cp_sim.py lives one dir up — make it importable without packaging.
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import pytest  # noqa: E402

from cp_sim import CPSim, CPSimConfig  # noqa: E402


@pytest.fixture
def sim() -> CPSim:
    """Fresh CPSim per test — cheap, isolates connection state."""
    return CPSim(
        cfg=CPSimConfig(
            runtime_url=os.environ.get("RUNTIME_URL", "http://localhost:18000"),
        )
    )


@pytest.fixture
def context_id() -> str:
    """A unique canvas-thread-id per test — guarantees SessionStore isolation
    between scenarios so a failing canary doesn't poison the next one."""
    return f"canary-ctx-{uuid.uuid4().hex[:12]}"
