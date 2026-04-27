"""Load skill packages from the workspace config directory."""

import importlib.util
import logging
import os
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

logger = logging.getLogger(__name__)

try:
    from builtin_tools.security_scan import SkillSecurityError, scan_skill_dependencies
    _SECURITY_SCAN_AVAILABLE = True
except ImportError:  # lightweight test environments without tools/ on sys.path
    _SECURITY_SCAN_AVAILABLE = False


@dataclass
class SkillMetadata:
    id: str
    name: str
    description: str
    tags: list[str] = field(default_factory=list)
    examples: list[str] = field(default_factory=list)
    # Runtime compatibility — list of adapter `name()` values this skill
    # supports, or ["*"] for universal. Borrowed from hermes' declarative
    # skill-compat pattern: a skill that depends on claude-code-only tools
    # should declare `runtime: [claude-code]` so hermes (or any other
    # adapter) skips it at load time instead of failing at first invocation.
    runtime: list[str] = field(default_factory=lambda: ["*"])


@dataclass
class LoadedSkill:
    metadata: SkillMetadata
    instructions: str
    tools: list[Any] = field(default_factory=list)


def parse_skill_frontmatter(skill_md_path: Path) -> tuple[dict, str]:
    """Parse YAML frontmatter from a SKILL.md file.

    Runtime-side: tolerant of malformed frontmatter (returns ``({}, body)``
    so the skill loads with empty metadata rather than crashing the
    workspace at startup). The SDK's :func:`molecule_plugin.parse_skill_md`
    is the authoring-time strict validator that surfaces the same errors.
    Keep behaviour aligned: if you change acceptance rules here, mirror
    them in the SDK's parser.
    """
    content = skill_md_path.read_text()

    if not content.startswith("---"):
        return {}, content

    parts = content.split("---", 2)
    if len(parts) < 3:
        return {}, content

    try:
        frontmatter = yaml.safe_load(parts[1]) or {}
    except yaml.YAMLError:
        logger.warning("SKILL.md at %s has malformed frontmatter; loading with empty metadata", skill_md_path)
        frontmatter = {}
    if not isinstance(frontmatter, dict):
        logger.warning("SKILL.md at %s frontmatter is not a mapping; ignoring", skill_md_path)
        frontmatter = {}

    body = parts[2].strip()
    return frontmatter, body


def load_skill_tools(scripts_dir: Path) -> list[Any]:
    """Dynamically load tool functions from a skill's scripts/ directory.

    Follows the agentskills.io spec layout: each skill's executable code
    lives under ``scripts/``. Returns an empty list if the directory
    doesn't exist.
    """
    tools = []
    if not scripts_dir.exists():
        return tools

    # Import langchain only when we actually have scripts to process.
    # Keeps test environments (and empty skills) from needing langchain.
    from langchain_core.tools import BaseTool

    # Sensitive env vars that must not be readable by skill scripts.
    # Fix C (Cycle 5): scrub before exec_module() so a malicious skill cannot
    # exfiltrate credentials even if it somehow bypasses the POST /plugins
    # auth gate (defence in depth).
    _SCRUB_KEYS = (
        "CLAUDE_CODE_OAUTH_TOKEN",
        "ANTHROPIC_API_KEY",
        "OPENAI_API_KEY",
        "WORKSPACE_AUTH_TOKEN",
        "GITHUB_TOKEN",
        "GH_TOKEN",
    )

    for py_file in sorted(scripts_dir.glob("*.py")):
        if py_file.name.startswith("_"):
            continue

        # Verify the script is actually inside the expected scripts directory
        # (path traversal guard — glob shouldn't produce outside paths, but
        # belt-and-suspenders for symlink attacks).
        try:
            py_file.resolve().relative_to(scripts_dir.resolve())
        except ValueError:
            logger.warning("skill_loader: rejecting script outside scripts_dir: %s", py_file)
            continue

        module_name = f"skill_tool_{py_file.stem}"
        spec = importlib.util.spec_from_file_location(module_name, py_file)
        if spec is None or spec.loader is None:
            continue

        module = importlib.util.module_from_spec(spec)
        sys.modules[module_name] = module

        # Temporarily remove sensitive env vars before running skill code.
        _saved_env = {k: os.environ.pop(k) for k in _SCRUB_KEYS if k in os.environ}
        try:
            spec.loader.exec_module(module)
        finally:
            # Always restore so the rest of the agent process retains them.
            os.environ.update(_saved_env)

        # Look for functions decorated with @tool (BaseTool instances)
        for attr_name in dir(module):
            attr = getattr(module, attr_name)
            if isinstance(attr, BaseTool):
                tools.append(attr)

    return tools


def _normalize_runtime_field(raw: Any, skill_name: str) -> list[str]:
    """Normalize the optional `runtime` frontmatter field to a list[str].

    Accepts: ["*"] (default), ["claude-code"], "claude-code" (string sugar),
    or absent (-> ["*"]). Anything else logs a warning and falls back to
    universal so a malformed manifest doesn't silently filter the skill.
    """
    if raw is None:
        return ["*"]
    if isinstance(raw, str):
        return [raw]
    if isinstance(raw, list) and all(isinstance(x, str) for x in raw):
        return raw or ["*"]
    logger.warning(
        "SKILL.md for '%s' has invalid `runtime` field %r; treating as universal",
        skill_name, raw,
    )
    return ["*"]


def load_skills(
    config_path: str,
    skill_names: list[str],
    current_runtime: str | None = None,
) -> list[LoadedSkill]:
    """Load all skills specified in the config.

    If ``current_runtime`` is provided, skills whose ``runtime`` frontmatter
    list does not include ``"*"`` or ``current_runtime`` are skipped (with a
    log line) instead of being loaded — matches hermes' declarative compat
    model so adapter-specific skills don't get force-loaded into runtimes
    that can't actually execute their tools.
    """
    skills_dir = Path(config_path) / "skills"
    loaded = []

    # Resolve security scan mode once before the loop
    scan_mode = "warn"
    fail_open_if_no_scanner = True  # safe default matches security_scan.py default
    if _SECURITY_SCAN_AVAILABLE:
        try:
            from config import load_config
            _cfg = load_config(config_path)
            scan_mode = _cfg.security_scan.mode
            fail_open_if_no_scanner = _cfg.security_scan.fail_open_if_no_scanner
        except Exception:
            pass  # use defaults — never block on config error

    for skill_name in skill_names:
        skill_path = skills_dir / skill_name
        skill_md = skill_path / "SKILL.md"

        if not skill_md.exists():
            logger.warning("SKILL.md not found for %s, skipping", skill_name)
            continue

        # --- Security scan before loading any code from the skill ------------
        if _SECURITY_SCAN_AVAILABLE and scan_mode != "off":
            try:
                scan_skill_dependencies(
                    skill_name, skill_path, scan_mode,
                    fail_open_if_no_scanner=fail_open_if_no_scanner,
                )
            except SkillSecurityError as exc:
                logger.warning("Skipping skill '%s': blocked by security scan — %s", skill_name, exc)
                continue

        frontmatter, instructions = parse_skill_frontmatter(skill_md)

        runtime_compat = _normalize_runtime_field(frontmatter.get("runtime"), skill_name)
        if current_runtime is not None and "*" not in runtime_compat and current_runtime not in runtime_compat:
            logger.info(
                "Skipping skill '%s': runtime=%s not compatible with current=%s",
                skill_name, runtime_compat, current_runtime,
            )
            continue

        metadata = SkillMetadata(
            id=skill_name,
            name=frontmatter.get("name", skill_name),
            description=frontmatter.get("description", ""),
            tags=frontmatter.get("tags", []),
            examples=frontmatter.get("examples", []),
            runtime=runtime_compat,
        )

        # Executables live under scripts/ per the agentskills.io spec.
        tools = load_skill_tools(skill_path / "scripts")

        loaded.append(LoadedSkill(
            metadata=metadata,
            instructions=instructions,
            tools=tools,
        ))

    return loaded
