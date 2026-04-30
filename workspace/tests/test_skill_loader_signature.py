"""skill_loader public-API signature snapshot — drift gate.

Every workspace template's adapter pulls skills via
``skill_loader.load_skills``. The returned ``LoadedSkill`` objects
expose ``metadata`` (a ``SkillMetadata``) which adapters pattern-match
on for runtime-compat filtering — see ``hermes`` and ``claude-code``
adapters which inspect ``skill.metadata.runtime`` to decide whether
to load a skill or skip it.

Renaming a field on ``SkillMetadata`` (e.g. ``runtime`` → ``runtimes``)
would silently break that filtering. The skill loader call still
returns objects, but every adapter's ``if "*" in skill.metadata.runtime``
check raises AttributeError at workspace boot — too late to be caught
by the introducing PR's CI.

Same drift class as the BaseAdapter signature snapshot (#2378, #2380),
applied to a different public surface.
"""

import sys
from pathlib import Path

import pytest

WORKSPACE_DIR = Path(__file__).parent.parent
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from tests._signature_snapshot import (  # noqa: E402
    build_dataclass_record,
    compare_against_snapshot,
)

SNAPSHOT_PATH = Path(__file__).parent / "snapshots" / "skill_loader_signature.json"


def _build_full_snapshot() -> dict:
    """Snapshot the public dataclasses exported from skill_loader.

    SkillMetadata fields are consumed by:
      - adapter runtime filtering (``runtime`` field)
      - canvas UI display (``name``, ``description``, ``tags``)
      - skill discovery / search (``id``, ``examples``)

    LoadedSkill is the return shape from ``load_skills`` and is held
    in ``SetupResult.loaded_skills`` — every adapter consumes it.
    """
    from skill_loader.loader import LoadedSkill, SkillMetadata

    return {
        "module": "skill_loader.loader",
        "dataclasses": [
            build_dataclass_record(SkillMetadata),
            build_dataclass_record(LoadedSkill),
        ],
    }


def test_skill_loader_signature_matches_snapshot():
    compare_against_snapshot(_build_full_snapshot(), SNAPSHOT_PATH)


def test_snapshot_has_required_skill_metadata_fields():
    """Defense-in-depth — adapters pattern-match on these specific
    field names. Removing one without a coordinated update breaks
    every adapter's skill-filter logic.
    """
    if not SNAPSHOT_PATH.exists():
        pytest.skip(f"{SNAPSHOT_PATH.name} not generated yet")

    import json
    snapshot = json.loads(SNAPSHOT_PATH.read_text())
    dataclasses = {dc["name"]: dc for dc in snapshot.get("dataclasses", [])}

    expected = {
        "SkillMetadata": {
            "id",
            "name",
            "description",
            # `runtime` drives per-adapter skill-compat filtering. If
            # this field is renamed, every adapter's
            # `if "*" in skill.metadata.runtime` check raises
            # AttributeError at workspace boot.
            "runtime",
        },
        "LoadedSkill": {
            "metadata",
            "instructions",
            "tools",
        },
    }

    for cls_name, required_fields in expected.items():
        if cls_name not in dataclasses:
            pytest.fail(
                f"Public dataclass {cls_name} missing from snapshot — "
                "either it was removed from skill_loader.loader, OR the "
                "snapshot wasn't regenerated after a refactor."
            )
        actual_fields = {f["name"] for f in dataclasses[cls_name]["fields"]}
        missing = required_fields - actual_fields
        if missing:
            pytest.fail(
                f"{cls_name} is missing required fields: {sorted(missing)}.\n"
                "Either restore them on skill_loader/loader.py, OR coordinate "
                "adapter + template updates AND remove the entry from "
                "`expected` in this test with a justification."
            )
