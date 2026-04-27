"""Tests for the skills hot-reload watcher.

Covers:
- SkillsWatcher._scan(): hashes files in watched skill dirs
- SkillsWatcher._changed_skills(): detects additions, removals, modifications
- SkillsWatcher._reload_skill(): calls load_skills and notifies callback
- SkillsWatcher.start() / stop(): polling loop lifecycle
- Audit events emitted on success and failure
- on_reload callback: sync and async variants
"""

import asyncio
import hashlib
import sys
import tempfile
from pathlib import Path
from types import ModuleType
from unittest.mock import AsyncMock, MagicMock, call, patch

import pytest

# ---------------------------------------------------------------------------
# Import the real SkillsWatcher from disk (isolated from conftest mocks)
# ---------------------------------------------------------------------------

import importlib.util as _ilu
_ROOT = Path(__file__).resolve().parents[1]
_spec = _ilu.spec_from_file_location("skill_loader.watcher", _ROOT / "skill_loader" / "watcher.py")
_watcher_mod = _ilu.module_from_spec(_spec)
_spec.loader.exec_module(_watcher_mod)
SkillsWatcher = _watcher_mod.SkillsWatcher


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def _sha256(content: str) -> str:
    return hashlib.sha256(content.encode()).hexdigest()


# ============================================================================
# _scan()
# ============================================================================

class TestScan:

    def test_empty_skills_dir_returns_empty(self, tmp_path):
        w = SkillsWatcher(str(tmp_path), ["nonexistent_skill"])
        assert w._scan() == {}

    def test_scans_files_in_skill_dir(self, tmp_path):
        _write(tmp_path / "skills" / "my_skill" / "SKILL.md", "# skill")
        _write(tmp_path / "skills" / "my_skill" / "tools" / "tool.py", "x=1")

        w = SkillsWatcher(str(tmp_path), ["my_skill"])
        hashes = w._scan()

        assert "my_skill/SKILL.md" in hashes
        assert "my_skill/tools/tool.py" in hashes
        assert len(hashes) == 2

    def test_ignores_dot_files(self, tmp_path):
        _write(tmp_path / "skills" / "sk" / "SKILL.md", "# ok")
        _write(tmp_path / "skills" / "sk" / ".hidden", "secret")

        w = SkillsWatcher(str(tmp_path), ["sk"])
        hashes = w._scan()

        assert "sk/SKILL.md" in hashes
        assert not any(".hidden" in k for k in hashes)

    def test_only_watches_declared_skills(self, tmp_path):
        _write(tmp_path / "skills" / "skill_a" / "SKILL.md", "a")
        _write(tmp_path / "skills" / "skill_b" / "SKILL.md", "b")

        w = SkillsWatcher(str(tmp_path), ["skill_a"])
        hashes = w._scan()

        assert any("skill_a" in k for k in hashes)
        assert not any("skill_b" in k for k in hashes)


# ============================================================================
# _changed_skills()
# ============================================================================

class TestChangedSkills:

    def test_detects_modification(self, tmp_path):
        _write(tmp_path / "skills" / "sk" / "SKILL.md", "v1")
        w = SkillsWatcher(str(tmp_path), ["sk"])
        w._hashes = w._scan()

        # Modify the file
        _write(tmp_path / "skills" / "sk" / "SKILL.md", "v2")
        new_hashes = w._scan()
        changed = w._changed_skills(new_hashes)

        assert "sk" in changed
        assert any("SKILL.md" in f for f in changed["sk"])

    def test_detects_new_file(self, tmp_path):
        _write(tmp_path / "skills" / "sk" / "SKILL.md", "base")
        w = SkillsWatcher(str(tmp_path), ["sk"])
        w._hashes = w._scan()

        # Add a new tool file
        _write(tmp_path / "skills" / "sk" / "tools" / "new_tool.py", "pass")
        new_hashes = w._scan()
        changed = w._changed_skills(new_hashes)

        assert "sk" in changed

    def test_detects_deleted_file(self, tmp_path):
        _write(tmp_path / "skills" / "sk" / "SKILL.md", "base")
        _write(tmp_path / "skills" / "sk" / "tools" / "old.py", "pass")
        w = SkillsWatcher(str(tmp_path), ["sk"])
        w._hashes = w._scan()

        # Delete the tool file
        (tmp_path / "skills" / "sk" / "tools" / "old.py").unlink()
        new_hashes = w._scan()
        changed = w._changed_skills(new_hashes)

        assert "sk" in changed

    def test_no_change_returns_empty(self, tmp_path):
        _write(tmp_path / "skills" / "sk" / "SKILL.md", "stable")
        w = SkillsWatcher(str(tmp_path), ["sk"])
        w._hashes = w._scan()
        new_hashes = w._scan()
        changed = w._changed_skills(new_hashes)
        assert changed == {}

    def test_ignores_changes_in_unwatched_skills(self, tmp_path):
        _write(tmp_path / "skills" / "watched" / "SKILL.md", "v1")
        _write(tmp_path / "skills" / "unwatched" / "SKILL.md", "v1")

        w = SkillsWatcher(str(tmp_path), ["watched"])
        w._hashes = w._scan()

        # Modify unwatched skill
        _write(tmp_path / "skills" / "unwatched" / "SKILL.md", "v2")
        # Also add path for unwatched to new_hashes manually (shouldn't matter)
        new_hashes = w._scan()
        new_hashes["unwatched/SKILL.md"] = _sha256("v2")

        changed = w._changed_skills(new_hashes)
        assert "unwatched" not in changed


# ============================================================================
# _reload_skill()
# ============================================================================

class TestReloadSkill:

    @pytest.mark.asyncio
    async def test_calls_callback_on_success(self, tmp_path, monkeypatch):
        _write(tmp_path / "skills" / "sk" / "SKILL.md",
               "---\nname: TestSkill\ndescription: test\n---\nInstruction")

        callback_calls = []

        async def _on_reload(skill):
            callback_calls.append(skill)

        w = SkillsWatcher(str(tmp_path), ["sk"], on_reload=_on_reload)

        # Monkey-patch load_skills to return a fake skill
        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="sk", name="TestSkill", description="test"),
            instructions="Instruction",
            tools=[],
        )

        def fake_load_skills(config_path, skill_names, **kwargs):
            return [fake_skill]

        monkeypatch.setattr(_watcher_mod, "_load_skills_impl",
                            fake_load_skills, raising=False)

        # Patch the import inside _reload_skill
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = fake_load_skills
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        await w._reload_skill("sk", ["sk/SKILL.md"])

        assert len(callback_calls) == 1
        assert callback_calls[0].metadata.id == "sk"

    @pytest.mark.asyncio
    async def test_sync_callback_also_works(self, tmp_path, monkeypatch):
        """SkillsWatcher accepts both sync and async on_reload callbacks."""
        _write(tmp_path / "skills" / "sk2" / "SKILL.md",
               "---\nname: SK2\ndescription: d\n---\n")

        callback_calls = []

        def _sync_on_reload(skill):
            callback_calls.append(skill.metadata.id)

        w = SkillsWatcher(str(tmp_path), ["sk2"], on_reload=_sync_on_reload)

        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="sk2", name="SK2", description="d"),
            instructions="",
            tools=[],
        )

        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        await w._reload_skill("sk2", ["sk2/SKILL.md"])

        assert callback_calls == ["sk2"]

    @pytest.mark.asyncio
    async def test_emits_audit_event_on_success(self, tmp_path, monkeypatch):
        _write(tmp_path / "skills" / "audited" / "SKILL.md",
               "---\nname: Audited\ndescription: a\n---\n")

        audit_events = []

        audit_mod = ModuleType("builtin_tools.audit")
        audit_mod.log_event = lambda **kwargs: audit_events.append(kwargs)
        monkeypatch.setitem(sys.modules, "builtin_tools.audit", audit_mod)

        w = SkillsWatcher(str(tmp_path), ["audited"])

        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="audited", name="Audited", description="a"),
            instructions="",
            tools=[],
        )
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        await w._reload_skill("audited", ["audited/SKILL.md"])

        assert any(
            e.get("event_type") == "skill_reload" and e.get("outcome") == "success"
            for e in audit_events
        )

    @pytest.mark.asyncio
    async def test_emits_audit_event_on_failure(self, tmp_path, monkeypatch):
        audit_events = []
        audit_mod = ModuleType("builtin_tools.audit")
        audit_mod.log_event = lambda **kwargs: audit_events.append(kwargs)
        monkeypatch.setitem(sys.modules, "builtin_tools.audit", audit_mod)

        w = SkillsWatcher(str(tmp_path), ["broken"])

        # Make load_skills blow up
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = MagicMock(side_effect=RuntimeError("bad skill"))
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        await w._reload_skill("broken", ["broken/SKILL.md"])

        assert any(
            e.get("event_type") == "skill_reload" and e.get("outcome") == "failure"
            for e in audit_events
        )


# ============================================================================
# Watcher lifecycle
# ============================================================================

class TestWatcherLifecycle:

    @pytest.mark.asyncio
    async def test_start_then_stop(self, tmp_path):
        w = SkillsWatcher(str(tmp_path), [])

        async def _stop_after():
            await asyncio.sleep(0.02)
            w.stop()

        asyncio.create_task(_stop_after())
        # Patch POLL_INTERVAL to be very short
        _watcher_mod.POLL_INTERVAL = 0.01
        await w.start()

        assert not w._running

    @pytest.mark.asyncio
    async def test_detects_change_and_calls_reload(self, tmp_path, monkeypatch):
        """Integration: change a file, expect on_reload to be called."""
        skill_dir = tmp_path / "skills" / "live"
        _write(skill_dir / "SKILL.md", "v1")

        reloads = []

        async def _on_reload(skill):
            reloads.append(skill)
            w.stop()   # stop after first reload

        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="live", name="Live", description="l"),
            instructions="",
            tools=[],
        )
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        _watcher_mod.POLL_INTERVAL  = 0.01
        _watcher_mod.DEBOUNCE_SECS  = 0.01

        w = SkillsWatcher(str(tmp_path), ["live"], on_reload=_on_reload)

        async def _modify_file():
            await asyncio.sleep(0.05)
            _write(skill_dir / "SKILL.md", "v2")

        asyncio.create_task(_modify_file())
        await asyncio.wait_for(w.start(), timeout=2.0)

        assert len(reloads) >= 1


# ============================================================================
# Additional coverage tests
# ============================================================================


class TestHashFile:
    """Tests for _hash_file — lines 107-108 (OSError path)."""

    def test_hash_file_returns_empty_on_oserror(self, tmp_path):
        """_hash_file returns '' when the file cannot be read (OSError)."""
        w = SkillsWatcher(str(tmp_path), [])
        # Provide a path that does not exist — read_bytes() raises OSError
        missing = tmp_path / "no_such_file.txt"
        result = w._hash_file(missing)
        assert result == ""

    def test_hash_file_returns_sha256_for_existing_file(self, tmp_path):
        """_hash_file returns a non-empty SHA-256 hex digest for a readable file."""
        f = tmp_path / "real_file.txt"
        f.write_text("hello")
        w = SkillsWatcher(str(tmp_path), [])
        result = w._hash_file(f)
        assert len(result) == 64  # SHA-256 hex digest length
        assert result != ""


class TestEvictStaleModules:
    """Tests for line 167: del sys.modules[key] inside _reload_skill."""

    @pytest.mark.asyncio
    async def test_stale_skill_tool_modules_are_evicted(self, tmp_path, monkeypatch):
        """_reload_skill evicts sys.modules entries starting with 'skill_tool_'."""
        # Inject fake stale module
        stale_mod = ModuleType("skill_tool_old_thing")
        monkeypatch.setitem(sys.modules, "skill_tool_old_thing", stale_mod)

        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="sk", name="SK", description="d"),
            instructions="",
            tools=[],
        )
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        w = SkillsWatcher(str(tmp_path), ["sk"])
        await w._reload_skill("sk", ["sk/SKILL.md"])

        # The stale module should be gone
        assert "skill_tool_old_thing" not in sys.modules


class TestAuditEventExceptionSuppressed:
    """Tests for lines 191-192: audit try/except in _reload_skill on success path."""

    @pytest.mark.asyncio
    async def test_audit_import_error_suppressed_on_success(self, tmp_path, monkeypatch):
        """Audit log_event exceptions are silently suppressed on skill reload success."""
        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="sk", name="SK", description="d"),
            instructions="",
            tools=[],
        )
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        # Make tools.audit.log_event raise an exception
        audit_mod = ModuleType("builtin_tools.audit")
        audit_mod.log_event = MagicMock(side_effect=RuntimeError("audit DB down"))
        monkeypatch.setitem(sys.modules, "builtin_tools.audit", audit_mod)

        w = SkillsWatcher(str(tmp_path), ["sk"])
        # Should not raise even though audit throws
        await w._reload_skill("sk", ["sk/SKILL.md"])


class TestOnReloadCallbackException:
    """Tests for lines 200-207: on_reload callback exception handling."""

    @pytest.mark.asyncio
    async def test_on_reload_sync_callback_exception_is_logged_not_raised(
        self, tmp_path, monkeypatch
    ):
        """Exceptions in sync on_reload callback are caught and logged."""
        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="sk", name="SK", description="d"),
            instructions="",
            tools=[],
        )
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        def failing_callback(skill):
            raise ValueError("callback blew up")

        w = SkillsWatcher(str(tmp_path), ["sk"], on_reload=failing_callback)
        # Should not propagate the exception
        await w._reload_skill("sk", ["sk/SKILL.md"])

    @pytest.mark.asyncio
    async def test_on_reload_async_callback_exception_is_logged_not_raised(
        self, tmp_path, monkeypatch
    ):
        """Exceptions in async on_reload callback are caught and logged."""
        from skill_loader.loader import LoadedSkill, SkillMetadata
        fake_skill = LoadedSkill(
            metadata=SkillMetadata(id="sk", name="SK", description="d"),
            instructions="",
            tools=[],
        )
        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names, **_: [fake_skill]
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        async def failing_async_callback(skill):
            raise RuntimeError("async callback blew up")

        w = SkillsWatcher(str(tmp_path), ["sk"], on_reload=failing_async_callback)
        # Should not propagate the exception
        await w._reload_skill("sk", ["sk/SKILL.md"])

    @pytest.mark.asyncio
    async def test_no_skill_returned_calls_audit_failure(self, tmp_path, monkeypatch):
        """When load_skills returns empty list, _audit_failure is called."""
        audit_events = []
        audit_mod = ModuleType("builtin_tools.audit")
        audit_mod.log_event = lambda **kwargs: audit_events.append(kwargs)
        monkeypatch.setitem(sys.modules, "builtin_tools.audit", audit_mod)

        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names: []  # empty result
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        w = SkillsWatcher(str(tmp_path), ["sk"])
        await w._reload_skill("sk", ["sk/SKILL.md"])

        assert any(
            e.get("outcome") == "failure" for e in audit_events
        )

    @pytest.mark.asyncio
    async def test_no_skill_returned_no_callback_called(self, tmp_path, monkeypatch):
        """When load_skills returns empty list, on_reload callback is NOT called."""
        callback_calls = []

        def callback(skill):
            callback_calls.append(skill)

        skills_mod = ModuleType("skill_loader.loader")
        skills_mod.load_skills = lambda cp, names: []
        monkeypatch.setitem(sys.modules, "skill_loader.loader", skills_mod)

        w = SkillsWatcher(str(tmp_path), ["sk"], on_reload=callback)
        await w._reload_skill("sk", ["sk/SKILL.md"])

        assert len(callback_calls) == 0


class TestAuditFailureExceptionSuppressed:
    """Tests for lines 225-226: _audit_failure suppresses exceptions."""

    def test_audit_failure_suppresses_import_error(self, tmp_path, monkeypatch):
        """_audit_failure silently handles ImportError when tools.audit unavailable."""
        # Remove tools.audit from sys.modules to force ImportError
        monkeypatch.delitem(sys.modules, "builtin_tools.audit", raising=False)
        monkeypatch.delitem(sys.modules, "tools", raising=False)

        # Should not raise
        SkillsWatcher._audit_failure("myskill", ["myskill/SKILL.md"], "some error")

    def test_audit_failure_suppresses_log_event_exception(self, tmp_path, monkeypatch):
        """_audit_failure suppresses exceptions raised by log_event."""
        audit_mod = ModuleType("builtin_tools.audit")
        audit_mod.log_event = MagicMock(side_effect=RuntimeError("db write failed"))
        monkeypatch.setitem(sys.modules, "builtin_tools.audit", audit_mod)

        # Should not raise
        SkillsWatcher._audit_failure("myskill", ["myskill/SKILL.md"], "error msg")
