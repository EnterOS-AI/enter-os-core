"""Skills hot-reload watcher.

Monitors the workspace's ``skills/`` directory for file changes and reloads
affected skill modules in-place — no coordinator restart required.

Architecture
------------
``SkillsWatcher`` runs as a background asyncio task alongside the agent.  It
polls the skill directories every ``POLL_INTERVAL`` seconds (default 3 s),
computes SHA-256 hashes of every file, and fires ``_reload_skill()`` when any
file inside a skill's folder changes.

``_reload_skill()`` calls ``load_skills()`` from ``skills.loader`` for the
changed skill and passes the fresh ``LoadedSkill`` to every registered
``on_reload`` callback.  Adapters register a callback that rebuilds the
LangGraph agent with the updated tool set, so the change takes effect on
the very next incoming A2A task — zero downtime.

Audit event
-----------
Every successful reload emits::

    event_type : "skill_reload"
    action     : "reload"
    resource   : "<skill_name>"
    outcome    : "success" | "failure"
    changed_files : [list of relative paths that triggered the reload]

Usage::

    watcher = SkillsWatcher(
        config_path="/configs",
        skill_names=["web_search", "code_review"],
        on_reload=lambda skill: rebuild_agent_with_skill(skill),
    )
    asyncio.create_task(watcher.start())
"""

from __future__ import annotations

import asyncio
import hashlib
import logging
import sys
from pathlib import Path
from typing import Callable

logger = logging.getLogger(__name__)

POLL_INTERVAL   = 3.0   # seconds between filesystem polls
DEBOUNCE_SECS   = 1.5   # wait for writes to settle before reloading


class SkillsWatcher:
    """Watches skill directories and reloads changed skills without restarting.

    Args:
        config_path:  Path to the workspace config directory (contains ``skills/``).
        skill_names:  List of skill IDs to watch (subfolder names under ``skills/``).
        on_reload:    Async or sync callable invoked with a fresh ``LoadedSkill``
                      every time a skill is reloaded.  May be called concurrently
                      for multiple skills if several change at once.
    """

    def __init__(
        self,
        config_path: str,
        skill_names: list[str],
        on_reload: Callable | None = None,
        current_runtime: str | None = None,
    ) -> None:
        self.config_path = config_path
        self.skill_names = list(skill_names)
        self.on_reload   = on_reload
        self.current_runtime = current_runtime
        self._hashes: dict[str, str] = {}   # rel_path → sha256 hex
        self._running = False

    # ------------------------------------------------------------------
    # Public interface
    # ------------------------------------------------------------------

    async def start(self) -> None:
        """Start the poll loop in the current event loop.  Runs until ``stop()``."""
        self._running = True
        self._hashes  = self._scan()
        logger.info(
            "SkillsWatcher: monitoring %d skill(s) in %s",
            len(self.skill_names), self.config_path,
        )

        while self._running:
            await asyncio.sleep(POLL_INTERVAL)
            await self._tick()

    def stop(self) -> None:
        self._running = False

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _skills_root(self) -> Path:
        return Path(self.config_path) / "skills"

    def _hash_file(self, path: Path) -> str:
        try:
            # H1: SHA-256 replaces MD5 for file-integrity change detection.
            return hashlib.sha256(path.read_bytes()).hexdigest()
        except OSError:
            return ""

    def _scan(self) -> dict[str, str]:
        """Return {relative_path: sha256} for every file in watched skill dirs."""
        hashes: dict[str, str] = {}
        root = self._skills_root()
        for skill_name in self.skill_names:
            skill_dir = root / skill_name
            if not skill_dir.is_dir():
                continue
            for fpath in skill_dir.rglob("*"):
                if fpath.is_file() and not fpath.name.startswith("."):
                    rel = str(fpath.relative_to(root))
                    hashes[rel] = self._hash_file(fpath)
        return hashes

    def _changed_skills(self, new_hashes: dict[str, str]) -> dict[str, list[str]]:
        """Return {skill_name: [changed_file, …]} for skills with file changes."""
        changed: dict[str, list[str]] = {}

        all_paths = set(new_hashes) | set(self._hashes)
        for rel_path in all_paths:
            old = self._hashes.get(rel_path, "")
            new = new_hashes.get(rel_path, "")
            if old != new:
                # rel_path is like "web_search/SKILL.md" or "web_search/tools/foo.py"
                skill_name = rel_path.split("/")[0]
                if skill_name in self.skill_names:
                    changed.setdefault(skill_name, []).append(rel_path)

        return changed

    async def _tick(self) -> None:
        """One poll cycle: detect changes, debounce, reload."""
        new_hashes = self._scan()
        changed = self._changed_skills(new_hashes)

        if not changed:
            return

        logger.info("SkillsWatcher: changes detected in %s", list(changed.keys()))
        await asyncio.sleep(DEBOUNCE_SECS)

        # Re-scan after debounce to absorb any writes still in-flight
        new_hashes = self._scan()
        changed    = self._changed_skills(new_hashes)

        self._hashes = new_hashes   # commit new baseline

        for skill_name, files in changed.items():
            await self._reload_skill(skill_name, files)

    async def _reload_skill(self, skill_name: str, changed_files: list[str]) -> None:
        """Reload *skill_name*'s modules and notify the callback."""
        logger.info("SkillsWatcher: reloading skill '%s' (changed: %s)", skill_name, changed_files)

        # Evict stale module entries so importlib loads fresh copies
        stale = [k for k in sys.modules if k.startswith(f"skill_tool_")]
        for key in stale:
            del sys.modules[key]

        try:
            from skill_loader.loader import load_skills
            loaded = load_skills(self.config_path, [skill_name], current_runtime=self.current_runtime)

            if loaded:
                skill = loaded[0]
                logger.info(
                    "SkillsWatcher: skill '%s' reloaded — %d tool(s)",
                    skill_name, len(skill.tools),
                )

                # Audit event
                try:
                    from builtin_tools.audit import log_event
                    log_event(
                        event_type="skill_reload",
                        action="reload",
                        resource=skill_name,
                        outcome="success",
                        changed_files=changed_files,
                        tool_count=len(skill.tools),
                    )
                except Exception:
                    pass

                # Notify adapter callback
                if self.on_reload is not None:
                    try:
                        result = self.on_reload(skill)
                        if asyncio.iscoroutine(result):
                            await result
                    except Exception as exc:
                        logger.error(
                            "SkillsWatcher: on_reload callback failed for '%s': %s",
                            skill_name, exc,
                        )
            else:
                logger.warning("SkillsWatcher: no LoadedSkill returned for '%s'", skill_name)
                self._audit_failure(skill_name, changed_files, "no_skill_returned")

        except Exception as exc:
            logger.error("SkillsWatcher: reload failed for '%s': %s", skill_name, exc)
            self._audit_failure(skill_name, changed_files, str(exc))

    @staticmethod
    def _audit_failure(skill_name: str, changed_files: list[str], error: str) -> None:
        try:
            from builtin_tools.audit import log_event
            log_event(
                event_type="skill_reload",
                action="reload",
                resource=skill_name,
                outcome="failure",
                changed_files=changed_files,
                error=error,
            )
        except Exception:
            pass
