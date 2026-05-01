"""Tests for scripts/build_runtime_package.py — the wheel-build import rewriter.

Run locally: ``python3 -m unittest scripts/test_build_runtime_package.py -v``

Why this exists: PR #2433 shipped ``import inbox as _inbox_module`` inside
the workspace runtime, and the rewriter expanded it to
``import molecule_runtime.inbox as inbox as _inbox_module`` — invalid
Python. The wheel-smoke gate caught it post-merge but couldn't block
the merge (not a required check yet — see PR #2439). PR #2436 added a
build-time gate that raises ``ValueError`` on this pattern; this file
locks the rewriter's documented contract under unit test so the gate
itself can't silently regress.

Coverage:
- ``import X``                  → ``import molecule_runtime.X as X``
- ``import X.sub``              → ``import molecule_runtime.X.sub``
- ``import X``  + trailing comment is preserved
- ``from X import Y``           → ``from molecule_runtime.X import Y``
- ``from X.sub import Y``       → ``from molecule_runtime.X.sub import Y``
- ``from X import Y, Z``        → ``from molecule_runtime.X import Y, Z``
- ``import X as Y``             → raises ValueError (the rewriter would
  produce ``import molecule_runtime.X as X as Y``, syntax error)
- non-allowlist module names    → not rewritten (regex anchors on the closed set)
- Indented imports (inside def/class) keep their indentation.
"""
from __future__ import annotations

import os
import sys
import unittest

# scripts/build_runtime_package.py lives at scripts/ — add scripts/ to sys.path
# so the import works whether unittest is invoked from repo root or scripts/.
HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import build_runtime_package as M  # noqa: E402


def rewrite(text: str) -> str:
    """Run the rewriter end-to-end so the test exercises the same path
    used by the wheel build (regex compile + substitution)."""
    regex = M.build_import_rewriter()
    return M.rewrite_imports(text, regex)


class TestBareImportRewriting(unittest.TestCase):
    def test_plain_import_aliases_to_preserve_binding(self):
        self.assertEqual(
            rewrite("import inbox\n"),
            "import molecule_runtime.inbox as inbox\n",
        )

    def test_plain_import_with_trailing_comment_is_preserved(self):
        # Real-world shape from a2a_mcp_server.py — the comment must
        # survive the rewrite without losing its leading-space buffer.
        self.assertEqual(
            rewrite("import inbox  # noqa: E402\n"),
            "import molecule_runtime.inbox as inbox  # noqa: E402\n",
        )

    def test_import_dotted_keeps_dotted_form(self):
        # `import X.sub` is rare for our modules but the rewriter must
        # not double-alias — we want `import molecule_runtime.X.sub`,
        # not `import molecule_runtime.X.sub as X.sub` (invalid).
        self.assertEqual(
            rewrite("import platform_tools.registry\n"),
            "import molecule_runtime.platform_tools.registry\n",
        )

    def test_indented_import_preserves_indentation(self):
        src = "def foo():\n    import inbox\n    return inbox.x\n"
        out = rewrite(src)
        self.assertIn("    import molecule_runtime.inbox as inbox\n", out)


class TestFromImportRewriting(unittest.TestCase):
    def test_from_module_import_simple(self):
        self.assertEqual(
            rewrite("from inbox import InboxState\n"),
            "from molecule_runtime.inbox import InboxState\n",
        )

    def test_from_dotted_import(self):
        self.assertEqual(
            rewrite("from platform_tools.registry import TOOLS\n"),
            "from molecule_runtime.platform_tools.registry import TOOLS\n",
        )

    def test_from_import_multiple_symbols(self):
        # Multi-import statement — the rewriter only touches the module
        # prefix, not the names being imported.
        self.assertEqual(
            rewrite("from a2a_tools import (foo, bar, baz)\n"),
            "from molecule_runtime.a2a_tools import (foo, bar, baz)\n",
        )

    def test_from_import_block_form(self):
        src = (
            "from a2a_tools import (\n"
            "    tool_check_task_status,\n"
            "    tool_commit_memory,\n"
            ")\n"
        )
        out = rewrite(src)
        self.assertIn("from molecule_runtime.a2a_tools import (\n", out)
        # Trailing names + closer are unchanged.
        self.assertIn("    tool_check_task_status,\n", out)
        self.assertIn(")\n", out)


class TestImportAsAliasRejection(unittest.TestCase):
    """The key regression class — the failure mode that shipped in PR #2433."""

    def test_import_as_alias_raises_value_error(self):
        with self.assertRaises(ValueError) as ctx:
            rewrite("import inbox as _inbox_module\n")
        msg = str(ctx.exception)
        # Error must name the offending module + suggest the fix.
        self.assertIn("inbox", msg)
        self.assertIn("as <alias>", msg)
        self.assertIn("from", msg)  # suggests `from X import …`

    def test_import_as_alias_indented_still_rejected(self):
        # Indented (inside def/class) — same hazard, same rejection.
        with self.assertRaises(ValueError):
            rewrite("def foo():\n    import inbox as _x\n")

    def test_import_as_alias_with_trailing_comment_still_rejected(self):
        with self.assertRaises(ValueError):
            rewrite("import inbox as _x  # comment\n")

    def test_plain_import_with_as_in_comment_does_not_trip(self):
        # The detection strips comments before pattern-matching, so a
        # comment containing "as foo" must NOT trigger the rejection.
        self.assertEqual(
            rewrite("import inbox  # rewriter produces alias as inbox\n"),
            "import molecule_runtime.inbox as inbox  # rewriter produces alias as inbox\n",
        )

    def test_import_followed_by_comma_is_not_an_alias(self):
        # `import inbox, os` — comma is not `as`, must not be rejected.
        # Our regex captures `inbox` then `,` — only `inbox` gets prefixed.
        # `os` is not in TOP_LEVEL_MODULES so it's left alone.
        out = rewrite("import inbox, os\n")
        # The first module is rewritten; the second (non-allowlist) is not.
        self.assertIn("import molecule_runtime.inbox as inbox", out)


class TestOutsideAllowlistModules(unittest.TestCase):
    def test_third_party_imports_unchanged(self):
        # `httpx`, `os`, `re` etc. are not in TOP_LEVEL_MODULES — the
        # regex must not match them. This is the closed-list invariant
        # that prevents accidental rewrites of stdlib / third-party.
        src = "import httpx\nimport os\nfrom re import match\n"
        self.assertEqual(rewrite(src), src)

    def test_short_name_collision_avoided(self):
        # `from a2a.server.X import Y` must not match the bare `a2a`
        # prefix — `a2a` isn't in our allowlist (we allow `a2a_tools`,
        # `a2a_client`, etc., but not bare `a2a`). Belt-and-suspenders.
        src = "from a2a.server.routes import create_agent_card_routes\n"
        self.assertEqual(rewrite(src), src)


class TestEndToEndShape(unittest.TestCase):
    """Reproduces the PR #2433 → #2436 incident shape."""

    def test_pr_2433_pattern_now_rejected(self):
        # The exact line PR #2433 added (inside main()), which produced
        # `import molecule_runtime.inbox as inbox as _inbox_module` —
        # invalid syntax in the published wheel.
        with self.assertRaises(ValueError) as ctx:
            rewrite(
                "    import inbox as _inbox_module\n"
                "    _inbox_module.set_notification_callback(_on_inbox_message)\n"
            )
        # Error message includes the offending line so the operator
        # knows exactly where to fix.
        self.assertIn("inbox", str(ctx.exception))

    def test_pr_2436_fix_pattern_works(self):
        # The fix-forward shape (#2436): top-level `import inbox`,
        # bridge wired in main() via `inbox.set_notification_callback`.
        src = (
            "import inbox\n"
            "\n"
            "def main():\n"
            "    inbox.set_notification_callback(cb)\n"
        )
        out = rewrite(src)
        self.assertIn("import molecule_runtime.inbox as inbox\n", out)
        # The callable reference inside main() is left alone — only
        # imports get rewritten, not arbitrary `inbox.foo` callsites
        # (those resolve via the module binding the rewrite preserves).
        self.assertIn("    inbox.set_notification_callback(cb)\n", out)


if __name__ == "__main__":
    unittest.main()
