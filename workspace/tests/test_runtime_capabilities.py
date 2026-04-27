"""Tests for RuntimeCapabilities + BaseAdapter.capabilities() — the
foundation primitive for the native+pluggable runtime principle (task
#117). The dataclass + default method are intentionally a no-op
addition; these tests pin that contract so a future change can't
accidentally flip a default and silently move ownership.
"""
from dataclasses import is_dataclass

import pytest

from adapter_base import BaseAdapter, RuntimeCapabilities


class _MinimalAdapter(BaseAdapter):
    """Concrete subclass with only the abstract members satisfied —
    every other behavior should fall through to BaseAdapter defaults
    so we can assert what those defaults are."""

    @staticmethod
    def name() -> str:
        return "test-minimal"

    @staticmethod
    def display_name() -> str:
        return "Test Minimal"

    @staticmethod
    def description() -> str:
        return "Minimal adapter for capability default tests"

    async def setup(self, config) -> None:
        return None

    async def create_executor(self, config):  # pragma: no cover
        raise NotImplementedError


class _NativeHeartbeatAdapter(_MinimalAdapter):
    """Models a runtime that owns heartbeat natively — declares it via
    capabilities() override. Used to verify the override mechanism
    works without touching defaults."""

    def capabilities(self) -> RuntimeCapabilities:
        return RuntimeCapabilities(provides_native_heartbeat=True)


class TestRuntimeCapabilitiesDataclass:
    """The dataclass surface itself."""

    def test_is_a_dataclass(self):
        assert is_dataclass(RuntimeCapabilities)

    def test_is_frozen(self):
        # Immutability matters: capabilities are declared at class-load
        # time and read by the platform on every heartbeat. A mutable
        # value would let a runtime change capabilities mid-flight,
        # creating impossible-to-debug state where the platform's idea
        # of who-owns-heartbeat drifts from the adapter's actual code.
        c = RuntimeCapabilities()
        with pytest.raises((AttributeError, Exception)):
            c.provides_native_heartbeat = True  # type: ignore[misc]

    def test_all_defaults_false(self):
        # Every flag MUST default to False — that's what makes adding
        # the dataclass a no-op for existing adapters. If any default
        # flips to True, every adapter that didn't override capabilities
        # silently switches who-owns-that-capability and the platform
        # stops providing the fallback. Catastrophic for langgraph /
        # crewai / deepagents which have no native impl.
        c = RuntimeCapabilities()
        assert c.provides_native_heartbeat is False
        assert c.provides_native_scheduler is False
        assert c.provides_native_session is False
        assert c.provides_native_status_mgmt is False
        assert c.provides_native_retry is False
        assert c.provides_activity_decoration is False
        assert c.provides_channel_dispatch is False

    def test_to_dict_keys_are_stable_wire_names(self):
        # The Go side reads these by string key from the heartbeat
        # payload. If Python renames a field (provides_native_heartbeat
        # → has_native_heartbeat) the dict's wire name should NOT change
        # — pin the JSON keys here so a refactor on the Python side
        # doesn't silently break the Go consumer.
        c = RuntimeCapabilities()
        assert set(c.to_dict().keys()) == {
            "heartbeat",
            "scheduler",
            "session",
            "status_mgmt",
            "retry",
            "activity_decoration",
            "channel_dispatch",
        }

    def test_to_dict_values_match_flags(self):
        c = RuntimeCapabilities(
            provides_native_heartbeat=True,
            provides_native_session=True,
        )
        d = c.to_dict()
        assert d["heartbeat"] is True
        assert d["session"] is True
        # Untouched flags stay False — we don't want a "True for one
        # capability flips siblings via dataclass inheritance" surprise.
        assert d["scheduler"] is False
        assert d["status_mgmt"] is False


class TestBaseAdapterCapabilitiesDefault:
    """The BaseAdapter.capabilities() default — the contract every
    existing adapter inherits without changes."""

    def test_default_returns_all_false(self):
        # The whole point of landing this primitive as a separate PR
        # is that it's behavior-preserving for everyone. If this test
        # fails, every adapter in the project has just had its
        # capability declarations silently changed.
        a = _MinimalAdapter()
        caps = a.capabilities()
        assert caps == RuntimeCapabilities()
        assert caps.to_dict() == {
            "heartbeat": False,
            "scheduler": False,
            "session": False,
            "status_mgmt": False,
            "retry": False,
            "activity_decoration": False,
            "channel_dispatch": False,
        }

    def test_default_returns_RuntimeCapabilities_instance(self):
        a = _MinimalAdapter()
        assert isinstance(a.capabilities(), RuntimeCapabilities)

    def test_subclass_can_override_capabilities(self):
        # Without this working, the entire native+pluggable principle
        # is unimplementable. Pin it with a fixture that flips one flag.
        a = _NativeHeartbeatAdapter()
        caps = a.capabilities()
        assert caps.provides_native_heartbeat is True
        # Sibling flags untouched — overriding one doesn't accidentally
        # move ownership of the others.
        assert caps.provides_native_scheduler is False
        assert caps.provides_native_session is False

    def test_override_does_not_affect_default_for_other_subclasses(self):
        # Method-level dispatch, not class-attribute mutation. A
        # subclass declaring native_heartbeat must NOT change what
        # _MinimalAdapter (a sibling) reports.
        minimal = _MinimalAdapter().capabilities()
        native = _NativeHeartbeatAdapter().capabilities()
        assert minimal.provides_native_heartbeat is False
        assert native.provides_native_heartbeat is True


class TestIdleTimeoutOverride:
    """The idle_timeout_override() hook — the first capability primitive
    with an actual platform consumer (workspace-server's a2a_proxy.go
    consults this per-workspace before applying its idle timer).

    Default behavior MUST be no-op (return None → platform uses global
    default). Subclasses override to declare longer/shorter window."""

    def test_default_returns_none(self):
        # If this default ever flips to a positive number, every adapter
        # silently gets that idle timeout. The platform's global default
        # (env A2A_IDLE_TIMEOUT_SECONDS, default 5min) would stop being
        # the floor — instead this hook would be — and ops would lose
        # the central knob.
        assert _MinimalAdapter().idle_timeout_override() is None

    def test_subclass_can_override_to_positive_seconds(self):
        class _SlowAdapter(_MinimalAdapter):
            def idle_timeout_override(self) -> int:
                return 600  # 10 min — typical for a slow synth runtime
        assert _SlowAdapter().idle_timeout_override() == 600

    def test_subclass_can_explicitly_keep_default_via_none(self):
        # An adapter that overrode this in an old version then dropped
        # the override (back to None) should cleanly fall back to the
        # platform default. Pinning here makes the round-trip explicit.
        class _DroppedOverrideAdapter(_MinimalAdapter):
            def idle_timeout_override(self):
                return None
        assert _DroppedOverrideAdapter().idle_timeout_override() is None
