"""Shared inspect-based signature-snapshot helpers (#2364 item 2).

Originally lived inline in tests/test_adapter_base_signature.py.
Extracted here so each public-surface module gets its own
test_*_signature.py + snapshot file without copy-pasting the
introspection logic.

Pattern (one snapshot file per module):

    from tests._signature_snapshot import (
        build_class_signature_record,
        build_dataclass_record,
        compare_against_snapshot,
    )

    SNAPSHOT_PATH = Path(__file__).parent / "snapshots" / "<module>_signature.json"

    def _build_full_snapshot() -> dict:
        from <module> import PublicClass, PublicDataclass
        return {
            "module": "<module>",
            "classes": [build_class_signature_record(PublicClass)],
            "dataclasses": [build_dataclass_record(PublicDataclass)],
        }

    def test_<module>_signature_matches_snapshot():
        compare_against_snapshot(_build_full_snapshot(), SNAPSHOT_PATH)

The snapshot is a stable JSON file — sort_keys + indent=2 — so
diffs are reviewable in PR. Any drift trips the test with both
expected and actual JSON in the failure message.
"""

import inspect
import json
from pathlib import Path

import pytest


def _annotation_repr(annotation: object) -> str:
    """Stable string form of a type annotation. ``inspect`` returns the
    runtime objects which don't compare cleanly — repr is the boring
    correct answer for snapshotting."""
    if annotation is inspect.Parameter.empty:
        return ""
    if isinstance(annotation, type):
        return annotation.__name__
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


def build_class_signature_record(cls: type) -> dict:
    """Snapshot a class's public method surface. Public = name doesn't
    start with underscore. Static/class/abstract methods are unwrapped
    so the underlying function signature is captured.

    Returns: ``{class: <name>, methods: [<sorted method records>]}``
    """
    methods: list[dict] = []
    for attr_name in sorted(vars(cls)):
        if attr_name.startswith("_"):
            continue
        attr = vars(cls)[attr_name]
        if isinstance(attr, staticmethod):
            fn = attr.__func__
        elif isinstance(attr, classmethod):
            fn = attr.__func__
        elif callable(attr):
            fn = attr
        else:
            continue
        methods.append(_signature_record(attr_name, fn))
    return {"class": cls.__name__, "methods": methods}


def build_dataclass_record(cls: type) -> dict:
    """Snapshot a dataclass's field shape. Captures field name + type
    annotation + has_default per field, plus the @dataclass(frozen=...)
    flag. Default values themselves are NOT recorded (would require
    brittle value-shape stringifying for non-trivial defaults).

    Returns: ``{name, frozen, fields: [<field records>]}``
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


def compare_against_snapshot(actual: dict, snapshot_path: Path) -> None:
    """Compare a built snapshot against a checked-in JSON file.

    On first run (snapshot missing): writes the file and skips. Re-run
    to verify it now passes — the snapshot file appears in the diff
    of the PR introducing it.

    On drift: fails the test with both expected and actual JSON in
    the failure message so the reviewer sees the change without
    re-running anything.
    """
    if not snapshot_path.exists():
        snapshot_path.parent.mkdir(parents=True, exist_ok=True)
        snapshot_path.write_text(json.dumps(actual, indent=2, sort_keys=True) + "\n")
        pytest.skip(
            f"snapshot did not exist; wrote {snapshot_path.name} — "
            "re-run the test to verify it now passes"
        )

    expected = json.loads(snapshot_path.read_text())
    if actual != expected:
        actual_str = json.dumps(actual, indent=2, sort_keys=True)
        expected_str = json.dumps(expected, indent=2, sort_keys=True)
        pytest.fail(
            f"Signature drifted from {snapshot_path.name}.\n\n"
            "Update intentionally by deleting the snapshot file and re-running, "
            "OR by editing it to match. The PR diff makes the change visible "
            "to reviewers and to template repos that depend on this surface.\n\n"
            f"=== EXPECTED ({snapshot_path.name}) ===\n{expected_str}\n\n"
            f"=== ACTUAL (current source) ===\n{actual_str}\n"
        )
