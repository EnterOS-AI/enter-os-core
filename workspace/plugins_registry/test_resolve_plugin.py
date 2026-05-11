"""Tests for _load_module_from_path sys.modules injection fix (issue #296).

Verifies that plugin adapters using "from plugins_registry.builtins import ..."
can be loaded via _load_module_from_path() without ModuleNotFoundError.
"""
import sys
import tempfile
import os
from pathlib import Path

# Ensure the plugins_registry package is importable
import plugins_registry

from plugins_registry import _load_module_from_path


def test_load_adapter_with_plugins_registry_import():
    """Plugin adapter using 'from plugins_registry.builtins import ...' loads cleanly."""
    # Write a temp adapter file that does the exact import from the bug report.
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".py", delete=False, dir=tempfile.gettempdir()
    ) as f:
        f.write("from plugins_registry.builtins import AgentskillsAdaptor as Adaptor\n")
        f.write("assert Adaptor is not None\n")
        adapter_path = Path(f.name)

    try:
        module = _load_module_from_path("test_adapter", adapter_path)
        assert module is not None, "module should load without error"
        assert hasattr(module, "Adaptor"), "module should expose Adaptor"
    finally:
        os.unlink(adapter_path)


def test_load_adapter_with_full_plugins_registry_import():
    """Plugin adapter using 'from plugins_registry import ...' loads cleanly."""
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".py", delete=False, dir=tempfile.gettempdir()
    ) as f:
        f.write("from plugins_registry import InstallContext, resolve\n")
        f.write("from plugins_registry.protocol import PluginAdaptor\n")
        f.write("assert InstallContext is not None\n")
        f.write("assert resolve is not None\n")
        f.write("assert PluginAdaptor is not None\n")
        adapter_path = Path(f.name)

    try:
        module = _load_module_from_path("test_adapter_full", adapter_path)
        assert module is not None, "module should load without error"
        assert hasattr(module, "InstallContext"), "module should expose InstallContext"
        assert hasattr(module, "resolve"), "module should expose resolve"
        assert hasattr(module, "PluginAdaptor"), "module should expose PluginAdaptor"
    finally:
        os.unlink(adapter_path)


if __name__ == "__main__":
    test_load_adapter_with_plugins_registry_import()
    test_load_adapter_with_full_plugins_registry_import()
    print("ALL TESTS PASS")
