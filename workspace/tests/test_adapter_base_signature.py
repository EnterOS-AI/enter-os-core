"""BaseAdapter public-API signature snapshot — drift gate (#2364 item 2).

Every workspace template subclasses ``BaseAdapter``. Renaming, removing,
or re-typing a method on the base class silently breaks templates that
override it — the override stops being recognized as an override, the
old method-name's caller silently invokes the default no-op, etc.
Recent #87 universal-runtime work + #1957 recordResource refactor both
renamed/added methods; without a frozen snapshot, the next rename ships
quietly and only surfaces when a template's CI catches the AttributeError
days later.

This test pins the public surface. It walks ``BaseAdapter`` with
``inspect`` and compares the result against a checked-in JSON snapshot.
Any drift = test failure.

When the failure is intentional:

  1. Make the API change in ``adapter_base.py``.
  2. Run the test once to see the diff in the failure message.
  3. Update ``tests/snapshots/adapter_base_signature.json`` to match
     the new shape — that update IS the explicit acknowledgment that
     templates need follow-up. Reviewer of the PR sees the snapshot
     diff in their review and decides whether template repos need
     coordinated updates.

Same-shape pattern as PR #2363's A2A protocol-compat replay gate
(workspace-server/internal/handlers/testdata/a2a_corpus). Both close
drift classes by snapshotting the structural surface that templates
or callers depend on.
"""

import inspect
import json
import sys
from pathlib import Path

import pytest

# Resolve workspace/ as the import root so adapter_base imports clean.
WORKSPACE_DIR = Path(__file__).parent.parent
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

SNAPSHOT_PATH = Path(__file__).parent / "snapshots" / "adapter_base_signature.json"


def _annotation_repr(annotation: object) -> str:
    """Stable string form of a type annotation. ``inspect`` returns the
    runtime objects which don't compare cleanly — repr is the boring
    correct answer for snapshotting."""
    if annotation is inspect.Parameter.empty:
        return ""
    if isinstance(annotation, type):
        return annotation.__name__
    # types.UnionType / typing.Union / forward refs — repr captures all
    return str(annotation)


def _parameter_record(p: inspect.Parameter) -> dict:
    return {
        "name": p.name,
        "kind": p.kind.name,
        "annotation": _annotation_repr(p.annotation),
        "has_default": p.default is not inspect.Parameter.empty,
    }


def _signature_record(name: str, fn: object) -> dict:
    sig = inspect.signature(fn)
    return {
        "name": name,
        "is_async": inspect.iscoroutinefunction(fn),
        "is_abstract": getattr(fn, "__isabstractmethod__", False),
        "parameters": [_parameter_record(p) for p in sig.parameters.values()],
        "return_annotation": _annotation_repr(sig.return_annotation),
    }


def _build_signature_snapshot() -> dict:
    """Walk BaseAdapter and produce a stable JSON-serializable snapshot."""
    from adapter_base import BaseAdapter  # imported lazy so test discovery is fast

    methods: list[dict] = []
    for attr_name in sorted(vars(BaseAdapter)):
        if attr_name.startswith("_"):
            continue
        attr = vars(BaseAdapter)[attr_name]
        # Only callables — skip data attributes (none today, but
        # forward-defensive). staticmethod / classmethod are unwrapped
        # via __func__; abstractmethod wraps the underlying fn.
        if isinstance(attr, staticmethod):
            fn = attr.__func__
        elif isinstance(attr, classmethod):
            fn = attr.__func__
        elif callable(attr):
            fn = attr
        else:
            continue
        methods.append(_signature_record(attr_name, fn))
    return {"class": "BaseAdapter", "methods": methods}


def _dataclass_record(cls: type) -> dict:
    """Stable JSON shape for a public dataclass exported from
    adapter_base. Captures field name + type annotation + default
    presence so renaming, retyping, or making-required-vs-optional
    drift trips the gate.

    Note on defaults: we record presence-of-default, not the default
    value. A literal default like ``False`` or ``None`` is part of the
    contract (templates inherit it), but reproducing it here would
    require value-shape stringifying that's brittle for non-trivial
    defaults (lists, dataclasses-as-defaults). Presence is enough to
    catch the dangerous transitions (required → optional and vice
    versa).
    """
    import dataclasses as _dc

    fields = []
    for f in _dc.fields(cls):
        fields.append({
            "name": f.name,
            "annotation": _annotation_repr(f.type) if not isinstance(f.type, str) else f.type,
            "has_default": f.default is not _dc.MISSING or f.default_factory is not _dc.MISSING,
        })
    return {
        "name": cls.__name__,
        "frozen": getattr(cls, "__dataclass_params__").frozen,
        "fields": fields,
    }


def _build_dataclass_snapshot() -> list[dict]:
    """Snapshot the public dataclasses exported from adapter_base.

    These types form the call-and-return shape between the platform
    and every adapter:
      - SetupResult: returned by adapter._common_setup()
      - AdapterConfig: passed into adapter setup hooks
      - RuntimeCapabilities: returned by adapter.capabilities() and
        consumed by platform-side dispatch routing (#117). A field
        rename here silently disables every native-capability flag
        every adapter currently declares.
    """
    from adapter_base import AdapterConfig, RuntimeCapabilities, SetupResult

    classes = [SetupResult, AdapterConfig, RuntimeCapabilities]
    return [_dataclass_record(cls) for cls in classes]


def _build_full_snapshot() -> dict:
    """Combined snapshot — BaseAdapter methods + public dataclasses."""
    return {
        **_build_signature_snapshot(),
        "dataclasses": _build_dataclass_snapshot(),
    }


def test_base_adapter_signature_matches_snapshot():
    """Pin BaseAdapter's public API surface against a frozen snapshot.

    Covers BOTH method signatures AND public dataclass field shapes
    (SetupResult, AdapterConfig, RuntimeCapabilities). Renaming a
    RuntimeCapabilities field would silently disable every adapter's
    capability declaration without this gate.

    On failure, the test prints both the expected and actual snapshot
    JSON so the diff is human-readable. Updating the snapshot is the
    explicit ack that a template-affecting API change is intentional.
    """
    actual = _build_full_snapshot()
    if not SNAPSHOT_PATH.exists():
        # First-run convenience: write the snapshot if missing. A reviewer
        # of the introducing PR sees the new file in the diff.
        SNAPSHOT_PATH.parent.mkdir(parents=True, exist_ok=True)
        SNAPSHOT_PATH.write_text(json.dumps(actual, indent=2, sort_keys=True) + "\n")
        pytest.skip(
            f"snapshot did not exist; wrote {SNAPSHOT_PATH.name} — "
            "re-run the test to verify it now passes"
        )

    expected = json.loads(SNAPSHOT_PATH.read_text())
    if actual != expected:
        # Pretty-print both for the failure message so reviewer sees what
        # changed without rerunning anything.
        actual_str = json.dumps(actual, indent=2, sort_keys=True)
        expected_str = json.dumps(expected, indent=2, sort_keys=True)
        pytest.fail(
            "BaseAdapter signature drifted from snapshot.\n\n"
            f"To update intentionally:\n  cp <(python -c 'from tests.test_adapter_base_signature import _build_full_snapshot; import json; print(json.dumps(_build_full_snapshot(), indent=2, sort_keys=True))') {SNAPSHOT_PATH}\n"
            "Or rerun with the snapshot deleted to regenerate.\n\n"
            f"=== EXPECTED ({SNAPSHOT_PATH.name}) ===\n{expected_str}\n\n"
            f"=== ACTUAL (current adapter_base.py) ===\n{actual_str}\n"
        )


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
