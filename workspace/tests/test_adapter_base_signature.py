"""BaseAdapter public-API signature snapshot — drift gate (#2364 item 2).

Every workspace template subclasses ``BaseAdapter``. Renaming, removing,
or re-typing a method on the base class — or a field on the public
dataclasses (SetupResult, AdapterConfig, RuntimeCapabilities) —
silently breaks templates that rely on the old shape. Without a
frozen snapshot, the next rename ships quietly and only surfaces when
a template's CI catches the AttributeError days later.

Helpers live in ``tests/_signature_snapshot.py`` so future surfaces
(skill_loader, etc.) reuse the same introspection logic.

When the failure is intentional:

  1. Make the API change in ``adapter_base.py``.
  2. Run the test once to see the diff in the failure message.
  3. Update ``tests/snapshots/adapter_base_signature.json`` to match
     the new shape (or delete it and re-run to regenerate). That
     update IS the explicit acknowledgment that templates need
     follow-up. Reviewer of the PR sees the snapshot diff in their
     review and decides whether template repos need coordinated
     updates.

Same-shape pattern as PR #2363's A2A protocol-compat replay gate.
Both close drift classes by snapshotting the structural surface that
templates or callers depend on.
"""

import json
import sys
from pathlib import Path

import pytest

# Resolve workspace/ as the import root so adapter_base imports clean.
WORKSPACE_DIR = Path(__file__).parent.parent
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from tests._signature_snapshot import (  # noqa: E402
    build_class_signature_record,
    build_dataclass_record,
    compare_against_snapshot,
)

SNAPSHOT_PATH = Path(__file__).parent / "snapshots" / "adapter_base_signature.json"


def _build_full_snapshot() -> dict:
    """Snapshot of BaseAdapter methods + the three public dataclasses
    that form the call/return contract between the platform and every
    adapter:

      - SetupResult: returned by adapter._common_setup()
      - AdapterConfig: passed into adapter setup hooks
      - RuntimeCapabilities: returned by adapter.capabilities();
        drives platform-side dispatch routing (#117). A field rename
        here silently disables every native-capability flag every
        adapter currently declares.
    """
    from adapter_base import AdapterConfig, BaseAdapter, RuntimeCapabilities, SetupResult

    snap = build_class_signature_record(BaseAdapter)
    snap["dataclasses"] = [
        build_dataclass_record(SetupResult),
        build_dataclass_record(AdapterConfig),
        build_dataclass_record(RuntimeCapabilities),
    ]
    return snap


def test_base_adapter_signature_matches_snapshot():
    compare_against_snapshot(_build_full_snapshot(), SNAPSHOT_PATH)


def test_snapshot_has_required_methods():
    """Defense-in-depth: the snapshot must include the methods every
    template overrides. If a future refactor accidentally drops one of
    these from BaseAdapter (e.g., moves it to a mixin), the equality
    test above passes if the snapshot file is also updated — but THIS
    test catches the structural regression.

    Add a method to ``required`` ONLY when removing it would break a
    deployed template. The list is intentionally short.
    """
    if not SNAPSHOT_PATH.exists():
        pytest.skip(f"{SNAPSHOT_PATH.name} not generated yet")

    snapshot = json.loads(SNAPSHOT_PATH.read_text())
    method_names = {m["name"] for m in snapshot["methods"]}

    required = {
        "name",  # runtime identifier — every template MUST implement
        "display_name",  # UI-facing label
        "description",  # short description
        "capabilities",  # native vs platform-fallback declaration (#117)
        "memory_filename",  # plugin-pipeline hook
    }
    missing = required - method_names
    if missing:
        pytest.fail(
            f"BaseAdapter snapshot is missing required methods: {sorted(missing)}.\n"
            "Either restore them on adapter_base.py, OR coordinate template "
            "updates AND remove the entry from `required` in this test with "
            "a justification."
        )


def test_snapshot_has_required_dataclass_fields():
    """Defense-in-depth for the dataclass shapes — same rationale as
    test_snapshot_has_required_methods but for fields that adapters
    pattern-match on.

    The most load-bearing case: RuntimeCapabilities flags drive
    platform-side dispatch routing. Renaming a flag silently turns
    every adapter's native-capability declaration into a no-op
    (the platform fallback runs), with no AttributeError to surface
    the breakage.
    """
    if not SNAPSHOT_PATH.exists():
        pytest.skip(f"{SNAPSHOT_PATH.name} not generated yet")

    snapshot = json.loads(SNAPSHOT_PATH.read_text())
    dataclasses = {dc["name"]: dc for dc in snapshot.get("dataclasses", [])}

    expected = {
        "RuntimeCapabilities": {
            # Each flag here drives a specific platform-side consumer
            # (heartbeat, cron, session, etc). Removing one without
            # coordinated platform-side migration silently drops back
            # to the platform fallback — see project memory
            # `project_runtime_native_pluggable.md`.
            "provides_native_heartbeat",
            "provides_native_scheduler",
            "provides_native_session",
        },
        "AdapterConfig": {
            "model",
            "system_prompt",
        },
        "SetupResult": {
            "system_prompt",
            "loaded_skills",
        },
    }

    for cls_name, required_fields in expected.items():
        if cls_name not in dataclasses:
            pytest.fail(
                f"Public dataclass {cls_name} missing from snapshot — "
                "either it was removed from adapter_base, OR the snapshot "
                "wasn't regenerated after a refactor."
            )
        actual_fields = {f["name"] for f in dataclasses[cls_name]["fields"]}
        missing = required_fields - actual_fields
        if missing:
            pytest.fail(
                f"{cls_name} is missing required fields: {sorted(missing)}.\n"
                "Either restore them on adapter_base.py, OR coordinate template "
                "updates AND remove the entry from `expected` in this test "
                "with a justification."
            )
