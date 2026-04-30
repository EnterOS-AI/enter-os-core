"""platform_auth public-API signature snapshot — drift gate.

``platform_auth`` is the workspace's auth-token store. Every outbound
HTTP from the runtime — heartbeat, registry/register, A2A delegation,
memory tool calls, chat uploads, temporal_workflow, molecule_ai_status
— pulls credentials through one of these five module-level functions.

A grep of ``from platform_auth import`` across workspace/ shows it's
imported by 14+ files in the runtime hot path:

  - main.py  (boot + token issuance)
  - heartbeat.py  (every heartbeat loop fire)
  - a2a_client.py  (every A2A peer call)
  - a2a_tools.py  (delegate_task_async)
  - consolidation.py
  - events.py  (canvas push)
  - executor_helpers.py  (3 sites)
  - molecule_ai_status.py
  - builtin_tools/memory.py  (3 sites)
  - builtin_tools/temporal_workflow.py  (2 sites)

Renaming any of the five (e.g. ``auth_headers`` → ``bearer_headers``)
would make every one of those imports raise ``ImportError`` at boot —
the workspace fails to start with a confusing trace deep in
heartbeat init, not at the rename site.

Same drift class as the BaseAdapter signature snapshot (#2378, #2380),
skill_loader gate (#2381), and runtime_wedge gate (#2383). The
shared ``_signature_snapshot.py`` helpers do the heavy lifting; this
file just declares which functions are part of the contract.
"""

import sys
from pathlib import Path

import pytest

WORKSPACE_DIR = Path(__file__).parent.parent
if str(WORKSPACE_DIR) not in sys.path:
    sys.path.insert(0, str(WORKSPACE_DIR))

from tests._signature_snapshot import (  # noqa: E402
    build_module_functions_record,
    compare_against_snapshot,
)

SNAPSHOT_PATH = Path(__file__).parent / "snapshots" / "platform_auth_signature.json"


def _build_full_snapshot() -> dict:
    """Pin only the five contract functions runtime + adapters call.
    ``clear_cache`` is intentionally NOT in the snapshot — it's a
    test-only helper. Callers in production code MUST NOT depend on it.
    """
    import platform_auth

    return build_module_functions_record(
        platform_auth,
        function_names=[
            "auth_headers",
            "self_source_headers",
            "get_token",
            "save_token",
            "refresh_cache",
        ],
    )


def test_platform_auth_signature_matches_snapshot():
    compare_against_snapshot(_build_full_snapshot(), SNAPSHOT_PATH)


def test_snapshot_has_required_functions():
    """Defense-in-depth: even if both source and snapshot are updated
    together, removing any of the five contract functions requires
    explicit edit here. The required set is the documented public
    contract — every workspace runtime import path depends on these.
    """
    if not SNAPSHOT_PATH.exists():
        pytest.skip(f"{SNAPSHOT_PATH.name} not generated yet")

    import json
    snapshot = json.loads(SNAPSHOT_PATH.read_text())
    fn_names = {f["name"] for f in snapshot["functions"]}

    required = {
        # Every outbound httpx call merges this into headers
        "auth_headers",
        # A2A peer + self-message paths add X-Workspace-ID via this
        "self_source_headers",
        # main.py reads this on boot to decide register-vs-resume
        "get_token",
        # main.py persists the platform-issued token via this
        "save_token",
        # 401-retry path drops the in-process cache via this (#1877)
        "refresh_cache",
    }
    missing = required - fn_names
    if missing:
        pytest.fail(
            f"platform_auth snapshot is missing required functions: {sorted(missing)}.\n"
            "Either restore them on platform_auth.py, OR coordinate runtime "
            "module + adapter updates AND remove the entry from `required` in "
            "this test with a justification."
        )

    for fn in snapshot["functions"]:
        if fn.get("missing"):
            pytest.fail(
                f"platform_auth.{fn['name']} resolved as a non-function — "
                "either it was replaced by a different kind of attribute "
                "(class? module-level alias?) which existing direct calls "
                "would break, OR it was removed entirely."
            )
