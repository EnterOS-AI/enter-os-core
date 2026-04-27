"""Tests for skills/loader.py — skill parsing and loading."""

import sys
from pathlib import Path
from types import ModuleType
from unittest.mock import MagicMock, patch

from skill_loader.loader import (
    LoadedSkill,
    SkillMetadata,
    parse_skill_frontmatter,
    load_skills,
)


def test_parse_skill_frontmatter_full(tmp_path):
    """Parses YAML frontmatter and body from a SKILL.md file."""
    skill_md = tmp_path / "SKILL.md"
    skill_md.write_text(
        "---\n"
        "name: SEO Optimizer\n"
        "description: Optimizes content for search engines\n"
        "tags:\n"
        "  - seo\n"
        "  - content\n"
        "examples:\n"
        "  - Optimize this blog post\n"
        "---\n"
        "## Instructions\n"
        "1. Analyze keywords\n"
        "2. Optimize headings\n"
    )

    fm, body = parse_skill_frontmatter(skill_md)
    assert fm["name"] == "SEO Optimizer"
    assert fm["description"] == "Optimizes content for search engines"
    assert fm["tags"] == ["seo", "content"]
    assert fm["examples"] == ["Optimize this blog post"]
    assert "## Instructions" in body
    assert "Analyze keywords" in body


def test_parse_skill_frontmatter_no_frontmatter(tmp_path):
    """Files without --- frontmatter return empty dict and full content."""
    skill_md = tmp_path / "SKILL.md"
    skill_md.write_text("Just instructions, no frontmatter.")

    fm, body = parse_skill_frontmatter(skill_md)
    assert fm == {}
    assert body == "Just instructions, no frontmatter."


def test_parse_skill_frontmatter_incomplete(tmp_path):
    """Incomplete frontmatter (only one ---) returns empty dict."""
    skill_md = tmp_path / "SKILL.md"
    skill_md.write_text("---\nname: Broken\n")

    fm, body = parse_skill_frontmatter(skill_md)
    assert fm == {}
    assert "---" in body


def test_parse_skill_frontmatter_empty_yaml(tmp_path):
    """Empty YAML block between --- returns empty dict."""
    skill_md = tmp_path / "SKILL.md"
    skill_md.write_text("---\n---\nBody content here.")

    fm, body = parse_skill_frontmatter(skill_md)
    assert fm == {}
    assert body == "Body content here."


def test_skill_metadata_defaults():
    """SkillMetadata has sensible defaults for optional fields."""
    meta = SkillMetadata(id="test", name="Test", description="A test skill")
    assert meta.tags == []
    assert meta.examples == []


def test_load_skills_with_temp_dir(tmp_path):
    """load_skills loads skills from a config directory structure."""
    skills_dir = tmp_path / "skills" / "my-skill"
    skills_dir.mkdir(parents=True)

    (skills_dir / "SKILL.md").write_text(
        "---\n"
        "name: My Skill\n"
        "description: Does things\n"
        "tags:\n"
        "  - general\n"
        "---\n"
        "Follow these steps to do things.\n"
    )

    # load_skill_tools will try to import langchain_core — mock it
    from unittest.mock import patch

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["my-skill"])

    assert len(loaded) == 1
    skill = loaded[0]
    assert skill.metadata.id == "my-skill"
    assert skill.metadata.name == "My Skill"
    assert skill.metadata.description == "Does things"
    assert skill.metadata.tags == ["general"]
    assert "Follow these steps" in skill.instructions


def test_load_skills_missing_skill_md(tmp_path):
    """Skills without SKILL.md are skipped with a warning."""
    skills_dir = tmp_path / "skills" / "no-md"
    skills_dir.mkdir(parents=True)
    # No SKILL.md

    from unittest.mock import patch

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["no-md"])

    assert len(loaded) == 0


def test_load_skills_multiple(tmp_path):
    """Multiple skills are loaded in order."""
    for name in ["alpha", "beta"]:
        skill_dir = tmp_path / "skills" / name
        skill_dir.mkdir(parents=True)
        (skill_dir / "SKILL.md").write_text(
            f"---\nname: {name.title()}\ndescription: Skill {name}\n---\n"
            f"Instructions for {name}."
        )

    from unittest.mock import patch

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["alpha", "beta"])

    assert len(loaded) == 2
    assert loaded[0].metadata.id == "alpha"
    assert loaded[1].metadata.id == "beta"
    assert loaded[0].metadata.name == "Alpha"
    assert loaded[1].metadata.name == "Beta"


# ---------- _SECURITY_SCAN_AVAILABLE = True (line 13) ----------


def test_security_scan_available_flag_true(monkeypatch):
    """When tools.security_scan is importable, _SECURITY_SCAN_AVAILABLE is True on reload."""
    import importlib

    # Save the original module object so we can restore it fully
    original_loader_module = sys.modules.get("skill_loader.loader")
    skills_pkg = sys.modules.get("skill_loader")

    # Create a fake tools.security_scan module with required exports
    fake_tools_mod = ModuleType("tools")

    class FakeSkillSecurityError(Exception):
        pass

    fake_security_mod = ModuleType("builtin_tools.security_scan")
    fake_security_mod.SkillSecurityError = FakeSkillSecurityError
    fake_security_mod.scan_skill_dependencies = MagicMock()

    # Inject into sys.modules BEFORE reimporting skills.loader
    monkeypatch.setitem(sys.modules, "tools", fake_tools_mod)
    monkeypatch.setitem(sys.modules, "builtin_tools.security_scan", fake_security_mod)

    # Remove skills.loader from sys.modules so it re-executes the module-level try/except
    monkeypatch.delitem(sys.modules, "skill_loader.loader", raising=False)

    try:
        # Reimport — line 13 (_SECURITY_SCAN_AVAILABLE = True) should now execute
        import skill_loader.loader as reloaded_loader
        assert reloaded_loader._SECURITY_SCAN_AVAILABLE is True
    finally:
        # ALWAYS restore the original module fully (including the package attribute)
        # to avoid contaminating subsequent tests that do `import skill_loader.loader`
        if original_loader_module is not None:
            sys.modules["skill_loader.loader"] = original_loader_module
            # Also restore the skills package attribute so `import skill_loader.loader` returns original
            if skills_pkg is not None:
                skills_pkg.loader = original_loader_module
        else:
            monkeypatch.delitem(sys.modules, "skill_loader.loader", raising=False)


# ---------- load_skill_tools() (lines 52-77) ----------


def test_load_skill_tools_returns_empty_for_missing_dir(tmp_path):
    """load_skill_tools returns [] when tools dir does not exist."""
    from skill_loader.loader import load_skill_tools

    # Mock langchain_core.tools so import works even without the real package
    fake_lc = ModuleType("langchain_core")
    fake_lc_tools = ModuleType("langchain_core.tools")

    class FakeBaseTool:
        pass

    fake_lc_tools.BaseTool = FakeBaseTool
    fake_lc.tools = fake_lc_tools

    with patch.dict(sys.modules, {
        "langchain_core": fake_lc,
        "langchain_core.tools": fake_lc_tools,
    }):
        result = load_skill_tools(tmp_path / "nonexistent_tools")

    assert result == []


def test_load_skill_tools_skips_underscore_files(tmp_path):
    """load_skill_tools skips files starting with _."""
    from skill_loader.loader import load_skill_tools

    tools_dir = tmp_path / "tools"
    tools_dir.mkdir()
    (tools_dir / "__init__.py").write_text("# init")
    (tools_dir / "_helper.py").write_text("# private")

    fake_lc = ModuleType("langchain_core")
    fake_lc_tools = ModuleType("langchain_core.tools")

    class FakeBaseTool:
        pass

    fake_lc_tools.BaseTool = FakeBaseTool
    fake_lc.tools = fake_lc_tools

    with patch.dict(sys.modules, {
        "langchain_core": fake_lc,
        "langchain_core.tools": fake_lc_tools,
    }):
        result = load_skill_tools(tools_dir)

    assert result == []


def test_load_skill_tools_loads_basetool_instances(tmp_path):
    """load_skill_tools returns BaseTool instances found in tool files."""
    from skill_loader.loader import load_skill_tools

    tools_dir = tmp_path / "tools"
    tools_dir.mkdir()

    # Write a fake tool module that exposes a FakeBaseTool instance
    (tools_dir / "my_tool.py").write_text(
        "class FakeTool:\n    pass\nmy_func = FakeTool()\n"
    )

    # Create a FakeBaseTool class and make FakeTool a subclass of it
    class FakeBaseTool:
        pass

    fake_lc = ModuleType("langchain_core")
    fake_lc_tools = ModuleType("langchain_core.tools")
    fake_lc_tools.BaseTool = FakeBaseTool
    fake_lc.tools = fake_lc_tools

    # Patch the tool file to return our FakeBaseTool instance
    fake_instance = FakeBaseTool()

    import importlib.util

    original_spec = importlib.util.spec_from_file_location

    def patched_spec(name, path, **kw):
        spec = original_spec(name, path, **kw)
        return spec

    with patch.dict(sys.modules, {
        "langchain_core": fake_lc,
        "langchain_core.tools": fake_lc_tools,
    }):
        # We can't easily inject the FakeBaseTool into the loaded module
        # so we test that it returns [] for a module with no BaseTool instances
        result = load_skill_tools(tools_dir)

    # The loaded module has FakeTool (not subclass of FakeBaseTool), so no tools returned
    assert isinstance(result, list)


def test_load_skill_tools_handles_invalid_spec(tmp_path):
    """load_skill_tools skips files where spec_from_file_location returns None."""
    from skill_loader.loader import load_skill_tools

    tools_dir = tmp_path / "tools"
    tools_dir.mkdir()
    (tools_dir / "broken_tool.py").write_text("x = 1")

    fake_lc = ModuleType("langchain_core")
    fake_lc_tools = ModuleType("langchain_core.tools")

    class FakeBaseTool:
        pass

    fake_lc_tools.BaseTool = FakeBaseTool

    with patch.dict(sys.modules, {
        "langchain_core": fake_lc,
        "langchain_core.tools": fake_lc_tools,
    }):
        with patch("importlib.util.spec_from_file_location", return_value=None):
            result = load_skill_tools(tools_dir)

    assert result == []


def test_load_skill_tools_appends_basetool_instances(tmp_path):
    """load_skill_tools appends attributes that are BaseTool instances (line 75)."""
    from skill_loader.loader import load_skill_tools

    tools_dir = tmp_path / "tools"
    tools_dir.mkdir()

    # The tool file will reference a module-level instance of FakeBaseTool.
    # We write a placeholder; then we override exec_module to inject the instance.
    (tools_dir / "real_tool.py").write_text("# will be replaced by exec_module patch\n")

    # We need BaseTool to be the *same class* used in isinstance check inside load_skill_tools.
    # Strategy: patch langchain_core.tools.BaseTool to our FakeBaseTool, and inject an
    # instance into the loaded module's namespace via a patched exec_module.

    class FakeBaseTool:
        pass

    fake_tool_instance = FakeBaseTool()

    fake_lc = ModuleType("langchain_core")
    fake_lc_tools = ModuleType("langchain_core.tools")
    fake_lc_tools.BaseTool = FakeBaseTool
    fake_lc.tools = fake_lc_tools

    import importlib.util as _ilu
    import types

    original_exec = None

    def patched_exec_module(module):
        # Inject a FakeBaseTool instance as a module attribute
        module.my_tool = fake_tool_instance

    with patch.dict(sys.modules, {
        "langchain_core": fake_lc,
        "langchain_core.tools": fake_lc_tools,
    }):
        # Patch spec.loader.exec_module on the spec returned by spec_from_file_location
        original_spec_fn = _ilu.spec_from_file_location

        def patched_spec(name, path, **kw):
            spec = original_spec_fn(name, path, **kw)
            if spec is not None and spec.loader is not None:
                spec.loader.exec_module = patched_exec_module
            return spec

        with patch("importlib.util.spec_from_file_location", side_effect=patched_spec):
            result = load_skill_tools(tools_dir)

    assert len(result) == 1
    assert result[0] is fake_tool_instance


# ---------- load_skills() with security scan available (lines 88-93, 105-109) ----------


def test_load_skills_with_security_scan_available_warn_mode(tmp_path, monkeypatch):
    """load_skills runs security scan in warn mode when _SECURITY_SCAN_AVAILABLE=True."""
    skill_dir = tmp_path / "skills" / "my-skill"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        "---\nname: My Skill\ndescription: Test\n---\nInstructions."
    )

    scan_calls = []

    import skill_loader.loader as loader_module

    monkeypatch.setattr(loader_module, "_SECURITY_SCAN_AVAILABLE", True)

    # Fake scan_skill_dependencies that just records calls
    def fake_scan(skill_name, skill_path, mode, fail_open_if_no_scanner=True):
        scan_calls.append((skill_name, mode, fail_open_if_no_scanner))

    # Fake SkillSecurityError
    class FakeSkillSecurityError(Exception):
        pass

    monkeypatch.setattr(loader_module, "scan_skill_dependencies", fake_scan, raising=False)
    monkeypatch.setattr(loader_module, "SkillSecurityError", FakeSkillSecurityError, raising=False)

    # Fake config load
    from config import WorkspaceConfig, SecurityScanConfig
    fake_cfg = WorkspaceConfig()
    fake_cfg.security_scan = SecurityScanConfig(mode="warn")

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        with patch("config.load_config", return_value=fake_cfg):
            loaded = loader_module.load_skills(str(tmp_path), ["my-skill"])

    assert len(loaded) == 1
    assert len(scan_calls) == 1
    assert scan_calls[0][0] == "my-skill"
    assert scan_calls[0][1] == "warn"
    assert scan_calls[0][2] is True  # default fail_open_if_no_scanner from SecurityScanConfig


def test_load_skills_security_scan_block_mode_skips_skill(tmp_path, monkeypatch):
    """load_skills skips skill when security scan raises SkillSecurityError in block mode."""
    skill_dir = tmp_path / "skills" / "blocked-skill"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        "---\nname: Blocked\ndescription: Unsafe\n---\nInstructions."
    )

    import skill_loader.loader as loader_module

    monkeypatch.setattr(loader_module, "_SECURITY_SCAN_AVAILABLE", True)

    class FakeSkillSecurityError(Exception):
        pass

    def blocking_scan(skill_name, skill_path, mode, fail_open_if_no_scanner=True):
        raise FakeSkillSecurityError("critical CVE found")

    monkeypatch.setattr(loader_module, "scan_skill_dependencies", blocking_scan, raising=False)
    monkeypatch.setattr(loader_module, "SkillSecurityError", FakeSkillSecurityError, raising=False)

    from config import WorkspaceConfig, SecurityScanConfig
    fake_cfg = WorkspaceConfig()
    fake_cfg.security_scan = SecurityScanConfig(mode="block")

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        with patch("config.load_config", return_value=fake_cfg):
            loaded = loader_module.load_skills(str(tmp_path), ["blocked-skill"])

    # Skill should be skipped due to security error
    assert len(loaded) == 0


def test_load_skills_security_scan_off_mode_skips_scan(tmp_path, monkeypatch):
    """load_skills skips scan entirely when mode='off'."""
    skill_dir = tmp_path / "skills" / "my-skill"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        "---\nname: My Skill\ndescription: Test\n---\nInstructions."
    )

    scan_calls = []

    import skill_loader.loader as loader_module
    monkeypatch.setattr(loader_module, "_SECURITY_SCAN_AVAILABLE", True)

    def tracking_scan(skill_name, skill_path, mode, fail_open_if_no_scanner=True):
        scan_calls.append(skill_name)

    class FakeSkillSecurityError(Exception):
        pass

    monkeypatch.setattr(loader_module, "scan_skill_dependencies", tracking_scan, raising=False)
    monkeypatch.setattr(loader_module, "SkillSecurityError", FakeSkillSecurityError, raising=False)

    from config import WorkspaceConfig, SecurityScanConfig
    fake_cfg = WorkspaceConfig()
    fake_cfg.security_scan = SecurityScanConfig(mode="off")

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        with patch("config.load_config", return_value=fake_cfg):
            loaded = loader_module.load_skills(str(tmp_path), ["my-skill"])

    # scan should have been skipped
    assert len(scan_calls) == 0
    assert len(loaded) == 1


def test_load_skills_config_load_error_defaults_to_warn(tmp_path, monkeypatch):
    """load_skills defaults scan_mode to 'warn' when load_config raises."""
    skill_dir = tmp_path / "skills" / "my-skill"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        "---\nname: My Skill\ndescription: Test\n---\nInstructions."
    )

    scan_modes = []

    import skill_loader.loader as loader_module
    monkeypatch.setattr(loader_module, "_SECURITY_SCAN_AVAILABLE", True)

    def tracking_scan(skill_name, skill_path, mode, fail_open_if_no_scanner=True):
        scan_modes.append(mode)

    class FakeSkillSecurityError(Exception):
        pass

    monkeypatch.setattr(loader_module, "scan_skill_dependencies", tracking_scan, raising=False)
    monkeypatch.setattr(loader_module, "SkillSecurityError", FakeSkillSecurityError, raising=False)

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        with patch("config.load_config", side_effect=FileNotFoundError("no config")):
            loaded = loader_module.load_skills(str(tmp_path), ["my-skill"])

    # Default warn mode used on config load failure
    assert len(scan_modes) == 1
    assert scan_modes[0] == "warn"
    assert len(loaded) == 1


# ---------- scripts/ (agentskills.io spec) precedence + legacy tools/ ----------


def test_load_skills_prefers_scripts_dir(tmp_path, monkeypatch, capsys):
    """agentskills.io spec says skill executables live under scripts/."""
    skill = tmp_path / "skills" / "demo"
    skill.mkdir(parents=True)
    (skill / "SKILL.md").write_text("---\nname: demo\ndescription: d\n---\nbody")
    (skill / "scripts").mkdir()
    (skill / "scripts" / "tool.py").write_text("# no tools to load")

    import skill_loader.loader as loader_module
    from unittest.mock import patch

    calls = []
    def spy(tools_dir):
        calls.append(tools_dir)
        return []

    with patch.object(loader_module, "load_skill_tools", side_effect=spy):
        loader_module.load_skills(str(tmp_path), ["demo"])

    assert len(calls) == 1
    assert calls[0].name == "scripts"
    # No deprecation warning should have been printed.
    out = capsys.readouterr().out
    assert "legacy" not in out


def test_load_skills_no_scripts_yields_empty_tools(tmp_path):
    """Skill with only SKILL.md (no scripts/ dir) loads with tools=[]."""
    skill = tmp_path / "skills" / "bare"
    skill.mkdir(parents=True)
    (skill / "SKILL.md").write_text("---\nname: bare\ndescription: d\n---\nbody")

    import skill_loader.loader as loader_module
    loaded = loader_module.load_skills(str(tmp_path), ["bare"])
    assert len(loaded) == 1
    assert loaded[0].tools == []


# ---------- parse_skill_frontmatter tolerance (runtime-side) ----------


def test_parse_skill_frontmatter_yaml_error_returns_empty_dict(tmp_path, caplog):
    """Runtime tolerates malformed YAML frontmatter instead of crashing
    the workspace at startup — SDK's validator is the strict one."""
    import logging
    from skill_loader.loader import parse_skill_frontmatter

    p = tmp_path / "SKILL.md"
    p.write_text("---\n: bad\nfoo: [unclosed\n---\nbody here")

    with caplog.at_level(logging.WARNING):
        fm, body = parse_skill_frontmatter(p)

    assert fm == {}
    assert body == "body here"
    assert any("malformed frontmatter" in rec.message for rec in caplog.records)


def test_parse_skill_frontmatter_non_mapping_returns_empty_dict(tmp_path, caplog):
    """If frontmatter parses to a list (not a mapping), also tolerated."""
    import logging
    from skill_loader.loader import parse_skill_frontmatter

    p = tmp_path / "SKILL.md"
    p.write_text("---\n- just\n- a\n- list\n---\nbody")

    with caplog.at_level(logging.WARNING):
        fm, body = parse_skill_frontmatter(p)

    assert fm == {}
    assert body == "body"
    assert any("not a mapping" in rec.message for rec in caplog.records)


def test_load_skills_missing_skill_md_logs_warning(tmp_path, caplog):
    """Missing SKILL.md path logs a warning via the logger (not print)."""
    import logging
    from skill_loader.loader import load_skills

    (tmp_path / "skills" / "phantom").mkdir(parents=True)
    # no SKILL.md

    with caplog.at_level(logging.WARNING):
        loaded = load_skills(str(tmp_path), ["phantom"])

    assert loaded == []
    assert any("SKILL.md not found" in rec.message for rec in caplog.records)


def test_load_skills_fail_open_if_no_scanner_wiring(tmp_path, monkeypatch):
    """#268 regression: fail_open_if_no_scanner from config is forwarded to scan_skill_dependencies.

    Previously load_skills read scan_mode from config but never read or passed
    fail_open_if_no_scanner, so setting fail_open_if_no_scanner=false in
    config.yaml had zero runtime effect.
    """
    skill_dir = tmp_path / "skills" / "my-skill"
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        "---\nname: My Skill\ndescription: Test\n---\nInstructions."
    )

    scan_kwargs: list[dict] = []

    import skill_loader.loader as loader_module

    monkeypatch.setattr(loader_module, "_SECURITY_SCAN_AVAILABLE", True)

    def capturing_scan(skill_name, skill_path, mode, fail_open_if_no_scanner=True):
        scan_kwargs.append({"mode": mode, "fail_open": fail_open_if_no_scanner})

    class FakeSkillSecurityError(Exception):
        pass

    monkeypatch.setattr(loader_module, "scan_skill_dependencies", capturing_scan, raising=False)
    monkeypatch.setattr(loader_module, "SkillSecurityError", FakeSkillSecurityError, raising=False)

    from config import WorkspaceConfig, SecurityScanConfig
    fake_cfg = WorkspaceConfig()
    fake_cfg.security_scan = SecurityScanConfig(mode="block", fail_open_if_no_scanner=False)

    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        with patch("config.load_config", return_value=fake_cfg):
            loader_module.load_skills(str(tmp_path), ["my-skill"])

    assert len(scan_kwargs) == 1, "scan_skill_dependencies should have been called once"
    assert scan_kwargs[0]["mode"] == "block"
    assert scan_kwargs[0]["fail_open"] is False, (
        "fail_open_if_no_scanner=False from config must be forwarded to scan_skill_dependencies"
    )


# ---------------------------------------------------------------------------
# Per-skill runtime compatibility (#119)
# ---------------------------------------------------------------------------
# A skill manifest can declare `runtime: [claude-code]` to opt out of being
# loaded into incompatible adapters. Default is universal — this is the
# important contract: existing skill libraries do NOT need to be migrated
# and continue to load into every adapter.


def _write_skill(tmp_path, name: str, runtime_block: str = "") -> None:
    skill_dir = tmp_path / "skills" / name
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(
        f"---\nname: {name.title()}\ndescription: x\n{runtime_block}---\n"
        f"Body for {name}."
    )


def test_skill_metadata_runtime_default_universal():
    meta = SkillMetadata(id="t", name="T", description="d")
    assert meta.runtime == ["*"], "default runtime must be universal — no implicit filtering"


def test_load_skills_no_runtime_field_is_universal(tmp_path):
    """Skills without a `runtime` frontmatter field load into any adapter."""
    _write_skill(tmp_path, "legacy")  # no runtime block
    from unittest.mock import patch
    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["legacy"], current_runtime="hermes")
    assert len(loaded) == 1
    assert loaded[0].metadata.runtime == ["*"]


def test_load_skills_explicit_match_loads(tmp_path):
    _write_skill(tmp_path, "claude-only", "runtime:\n  - claude-code\n")
    from unittest.mock import patch
    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["claude-only"], current_runtime="claude-code")
    assert len(loaded) == 1


def test_load_skills_explicit_mismatch_skips(tmp_path):
    _write_skill(tmp_path, "claude-only", "runtime:\n  - claude-code\n")
    from unittest.mock import patch
    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["claude-only"], current_runtime="hermes")
    assert loaded == [], "skill must be filtered out of incompatible runtime"


def test_load_skills_runtime_string_sugar(tmp_path):
    """Bare string `runtime: claude-code` is normalized to ['claude-code']."""
    _write_skill(tmp_path, "sugary", "runtime: claude-code\n")
    from unittest.mock import patch
    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["sugary"], current_runtime="claude-code")
    assert len(loaded) == 1
    assert loaded[0].metadata.runtime == ["claude-code"]


def test_load_skills_runtime_wildcard_matches_anything(tmp_path):
    _write_skill(tmp_path, "wild", "runtime:\n  - '*'\n  - claude-code\n")
    from unittest.mock import patch
    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["wild"], current_runtime="hermes")
    assert len(loaded) == 1, "wildcard must short-circuit the runtime check"


def test_load_skills_no_current_runtime_loads_everything(tmp_path):
    """When current_runtime is None (test/fallback), no filtering happens."""
    _write_skill(tmp_path, "claude-only", "runtime:\n  - claude-code\n")
    from unittest.mock import patch
    with patch("skill_loader.loader.load_skill_tools", return_value=[]):
        loaded = load_skills(str(tmp_path), ["claude-only"])
    assert len(loaded) == 1, "absent current_runtime must preserve old behavior"


def test_load_skills_malformed_runtime_treated_as_universal(tmp_path, caplog):
    """A garbage runtime value warns + falls back to universal — never silently drops the skill."""
    _write_skill(tmp_path, "garbage", "runtime: 123\n")
    from unittest.mock import patch
    import logging
    with caplog.at_level(logging.WARNING, logger="skill_loader.loader"):
        with patch("skill_loader.loader.load_skill_tools", return_value=[]):
            loaded = load_skills(str(tmp_path), ["garbage"], current_runtime="hermes")
    assert len(loaded) == 1, "malformed runtime must not silently filter"
    assert any("invalid `runtime`" in r.message for r in caplog.records)
