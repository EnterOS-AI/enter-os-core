"""Tests for heartbeat._runtime_metadata_payload — the heartbeat-side
producer that sends adapter capability declarations + the
idle_timeout_override value to the platform every 30s. Capability
primitive #2 (task #117) wires this into the platform's a2a_proxy.

Tests use sys.modules monkey-patching to stub the `adapters` module
because workspace/heartbeat.py lazy-imports it inside the helper —
keeping heartbeat resilient to a missing/broken adapter discovery
path."""
import sys
from types import SimpleNamespace

import pytest

from adapter_base import BaseAdapter, RuntimeCapabilities
from heartbeat import _runtime_metadata_payload


class _FakeAdapter(BaseAdapter):
    """Default adapter — every capability False, no idle override.
    Matches today's behavior for any runtime that doesn't opt in."""

    @staticmethod
    def name() -> str:
        return "fake"

    @staticmethod
    def display_name() -> str:
        return "Fake"

    @staticmethod
    def description() -> str:
        return "Fake adapter for heartbeat metadata tests"

    async def setup(self, config) -> None:
        return None

    async def create_executor(self, config):  # pragma: no cover
        raise NotImplementedError


class _NativeAdapter(_FakeAdapter):
    """Adapter that declares native heartbeat + 600s idle override —
    matches what claude-code's adapter will declare once #87 lands."""

    def capabilities(self) -> RuntimeCapabilities:
        return RuntimeCapabilities(provides_native_heartbeat=True)

    def idle_timeout_override(self) -> int:
        return 600


@pytest.fixture
def stub_adapters_module(request):
    """Install a fake `adapters` module that returns the requested
    adapter class from get_adapter(). Cleans up after the test."""
    adapter_cls = getattr(request, "param", _FakeAdapter)
    fake_mod = SimpleNamespace(get_adapter=lambda runtime: adapter_cls)
    saved = sys.modules.get("adapters")
    sys.modules["adapters"] = fake_mod  # type: ignore[assignment]
    try:
        yield adapter_cls
    finally:
        if saved is None:
            sys.modules.pop("adapters", None)
        else:
            sys.modules["adapters"] = saved


@pytest.mark.parametrize("stub_adapters_module", [_FakeAdapter], indirect=True)
def test_default_adapter_emits_all_false_capabilities_no_idle_override(stub_adapters_module):
    """Default-adapter heartbeat MUST carry the runtime_metadata block
    with all-False caps and no idle_timeout_seconds. The block being
    present (even with zero info) is the wire signal that this runtime
    speaks the new protocol — older runtimes omit the field entirely."""
    payload = _runtime_metadata_payload()
    assert "runtime_metadata" in payload
    meta = payload["runtime_metadata"]
    assert meta["capabilities"] == {
        "heartbeat": False,
        "scheduler": False,
        "session": False,
        "status_mgmt": False,
        "retry": False,
        "activity_decoration": False,
        "channel_dispatch": False,
    }
    # No override key at all — pin the "absent field = use platform
    # default" wire contract Go side relies on.
    assert "idle_timeout_seconds" not in meta


@pytest.mark.parametrize("stub_adapters_module", [_NativeAdapter], indirect=True)
def test_native_adapter_emits_capability_flag_and_idle_override(stub_adapters_module):
    payload = _runtime_metadata_payload()
    meta = payload["runtime_metadata"]
    assert meta["capabilities"]["heartbeat"] is True
    # Sibling caps untouched — declaring one capability doesn't
    # accidentally claim ownership of the others.
    assert meta["capabilities"]["scheduler"] is False
    assert meta["idle_timeout_seconds"] == 600


def test_returns_empty_dict_when_adapter_module_missing(monkeypatch):
    """get_adapter() raises KeyError when ADAPTER_MODULE is unset.
    Heartbeat must NEVER fail — the metadata is optional, the
    heartbeat itself (alive signal) is load-bearing. Pin that the
    helper swallows the error and returns {}."""
    # Remove any stub from prior tests.
    monkeypatch.delitem(sys.modules, "adapters", raising=False)
    # Force get_adapter to raise by ensuring ADAPTER_MODULE is unset.
    monkeypatch.delenv("ADAPTER_MODULE", raising=False)
    payload = _runtime_metadata_payload()
    assert payload == {}


@pytest.mark.parametrize("stub_adapters_module", [_FakeAdapter], indirect=True)
def test_idle_timeout_override_zero_or_negative_omitted(stub_adapters_module, monkeypatch):
    """An adapter that returns 0 or negative from idle_timeout_override
    means 'use the platform default' — same as None. Don't ship a
    bogus value to the wire that the Go side would have to filter."""
    class _BadOverrideAdapter(_FakeAdapter):
        def idle_timeout_override(self) -> int:
            return 0

    fake_mod = SimpleNamespace(get_adapter=lambda runtime: _BadOverrideAdapter)
    monkeypatch.setitem(sys.modules, "adapters", fake_mod)

    payload = _runtime_metadata_payload()
    assert "idle_timeout_seconds" not in payload["runtime_metadata"]


@pytest.mark.parametrize("stub_adapters_module", [_FakeAdapter], indirect=True)
def test_swallows_unexpected_exception_inside_adapter(stub_adapters_module, monkeypatch):
    """Adapter capabilities() / idle_timeout_override() throwing must
    NOT crash heartbeat. Returns {} so no field is sent and the
    platform falls through to defaults."""
    class _BrokenAdapter(_FakeAdapter):
        def capabilities(self):
            raise RuntimeError("simulated broken adapter init")

    fake_mod = SimpleNamespace(get_adapter=lambda runtime: _BrokenAdapter)
    monkeypatch.setitem(sys.modules, "adapters", fake_mod)

    payload = _runtime_metadata_payload()
    assert payload == {}
