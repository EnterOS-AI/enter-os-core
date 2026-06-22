"""Tests for the sweep-cf-orphans.sh decision function (#2027).

Run locally: ``python3 -m unittest scripts/ops/test_sweep_cf_decide.py -v``

Why this exists: the inline Python heredoc in sweep-cf-orphans.sh decides
which Cloudflare DNS records to delete. A misclassification could nuke a
live workspace's DNS record. These tests cover the rule priority order +
the safety gate, plus a parity check that asserts the inline block in the
shell script matches the importable module byte-for-byte (so the two
cannot drift silently).
"""
from __future__ import annotations

import os
import unittest

import sweep_cf_decide as M


# Caller responsibility (per the new decide signature): compute the union once.
ALL_SLUGS = {"acme", "globex", "initech", "e2e-test-runner", "soak", "playground"}
LIVE_EC2 = {"ws-d3605ef2-f7d", "ws-aaaaaaaa-bbb", "ws-cafef00d-dec"}


def rec(name: str, rid: str = "rid-x", typ: str = "A") -> dict:
    return {"name": name, "id": rid, "type": typ}


def call(record: dict) -> tuple:
    return M.decide(record, ALL_SLUGS, LIVE_EC2)


class TestPlatformCore(unittest.TestCase):
    """Apex, www, api, app, _verification keys must NEVER be touched."""

    def test_apex_kept(self):
        action, reason, *_ = call(rec("moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "apex"))

    def test_underscore_records_kept(self):
        for n in ("_vercel.moleculesai.app", "_railway-verify.moleculesai.app"):
            with self.subTest(name=n):
                action, reason, *_ = call(rec(n))
                self.assertEqual((action, reason), ("keep", "verification/key"))

    def test_dkim_kept(self):
        action, reason, *_ = call(rec("send._domainkey.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "verification/key"))

    def test_platform_subdomains_kept(self):
        for n in (
            "api.moleculesai.app",
            "app.moleculesai.app",
            "doc.moleculesai.app",
            "send.moleculesai.app",
            "status.moleculesai.app",
            "www.moleculesai.app",
            "staging-api.moleculesai.app",
        ):
            with self.subTest(name=n):
                action, reason, *_ = call(rec(n))
                self.assertEqual((action, reason), ("keep", "platform-core"))


class TestWsRule(unittest.TestCase):
    """ws-* DNS records keep iff a live EC2 with the same prefix exists."""

    def test_live_ws_kept(self):
        action, reason, *_ = call(rec("ws-d3605ef2-f7d.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "live-ec2"))

    def test_live_ws_kept_on_staging(self):
        action, reason, *_ = call(rec("ws-aaaaaaaa-bbb.staging.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "live-ec2"))

    def test_dead_ws_deleted(self):
        action, reason, *_ = call(rec("ws-deadbeef-fff.moleculesai.app"))
        self.assertEqual((action, reason), ("delete", "orphan-ws"))

    def test_dead_ws_on_staging_deleted(self):
        action, reason, *_ = call(rec("ws-deadbeef-fff.staging.moleculesai.app"))
        self.assertEqual((action, reason), ("delete", "orphan-ws"))


class TestE2ERule(unittest.TestCase):
    def test_live_e2e_kept(self):
        action, reason, *_ = call(rec("e2e-test-runner.staging.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "live-e2e-tenant"))

    def test_dead_e2e_deleted(self):
        action, reason, *_ = call(rec("e2e-ghost-1234.staging.moleculesai.app"))
        self.assertEqual((action, reason), ("delete", "orphan-e2e-tenant"))

    def test_dead_e2e_on_prod_deleted(self):
        action, reason, *_ = call(rec("e2e-ghost.moleculesai.app"))
        self.assertEqual((action, reason), ("delete", "orphan-e2e-tenant"))


class TestTenantSubdomainRule(unittest.TestCase):
    def test_live_prod_tenant_kept(self):
        action, reason, *_ = call(rec("acme.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "live-tenant"))

    def test_live_staging_tenant_kept(self):
        action, reason, *_ = call(rec("soak.staging.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "live-tenant"))

    def test_unknown_subdomain_kept_for_safety(self):
        action, reason, *_ = call(rec("hermes-final-2.moleculesai.app"))
        self.assertEqual((action, reason), ("keep", "unknown-subdomain-kept-for-safety"))


class TestNotASweepPattern(unittest.TestCase):
    def test_external_domain_kept(self):
        # Domain-spoofing attempt — must NOT match any of the moleculesai.app rules.
        action, reason, *_ = call(rec("api.openai.com.evil.internal"))
        self.assertEqual((action, reason), ("keep", "not-a-pattern-we-sweep"))

    def test_unrelated_apex_kept(self):
        action, reason, *_ = call(rec("example.com"))
        self.assertEqual((action, reason), ("keep", "not-a-pattern-we-sweep"))


class TestRulePriority(unittest.TestCase):
    """Platform-core check must precede the tenant-subdomain regex —
    e.g. ``api.moleculesai.app`` matches the tenant pattern but must
    classify as platform-core."""

    def test_api_subdomain_classified_as_platform_not_tenant(self):
        action, reason, *_ = call(rec("api.moleculesai.app"))
        self.assertEqual(reason, "platform-core")

    def test_underscore_record_classified_before_tenant(self):
        action, reason, *_ = call(rec("_vercel.moleculesai.app"))
        self.assertEqual(reason, "verification/key")


class TestSafetyGate(unittest.TestCase):
    """The bash gate refuses to delete >MAX_DELETE_PCT (default 50%)."""

    def test_under_threshold_passes(self):
        self.assertTrue(M.safety_gate(total=100, delete_count=49))
        self.assertTrue(M.safety_gate(total=100, delete_count=50))

    def test_over_threshold_fails(self):
        self.assertFalse(M.safety_gate(total=100, delete_count=51))
        self.assertFalse(M.safety_gate(total=10, delete_count=10))

    def test_zero_total_passes_trivially(self):
        # No records → nothing to delete → gate trivially OK (no div-by-zero).
        self.assertTrue(M.safety_gate(total=0, delete_count=0))

    def test_custom_threshold(self):
        self.assertTrue(M.safety_gate(total=100, delete_count=70, max_delete_pct=75))
        self.assertFalse(M.safety_gate(total=100, delete_count=76, max_delete_pct=75))


class TestEmptyLiveSets(unittest.TestCase):
    """If the CP admin API returns no orgs (auth broken, network blip),
    every tenant-shaped record looks orphan. decide() alone has no
    defense — that's safety_gate's job. This test pins the contract so
    a future "make decide() defensive" change doesn't silently bypass
    the gate."""

    def test_dead_e2e_orphans_when_live_set_empty(self):
        action, reason, *_ = M.decide(
            rec("e2e-test-runner.staging.moleculesai.app"),
            set(), set(),
        )
        self.assertEqual((action, reason), ("delete", "orphan-e2e-tenant"))

    def test_live_ws_still_kept_when_ec2_set_empty(self):
        action, reason, *_ = M.decide(
            rec("ws-cafef00d-dec.moleculesai.app"),
            ALL_SLUGS, set(),
        )
        self.assertEqual((action, reason), ("delete", "orphan-ws"))


class TestParityWithBashScript(unittest.TestCase):
    """The decision logic exists in two places: the canonical block in
    sweep_cf_decide.py and the inline heredoc in sweep-cf-orphans.sh.
    This test asserts the two match between the
    ``# CANONICAL DECIDE BEGIN`` / ``# CANONICAL DECIDE END`` markers,
    so an edit to one without the other fails CI loudly. The mirror-
    reminder comment lives OUTSIDE the markers in the .sh file so we
    don't need to special-case it here."""

    @staticmethod
    def _slice_canonical(text: str) -> list[str]:
        """Return the lines between the canonical markers, exclusive.
        Markers are matched line-anchored (a stripped-line literal match)
        so the docstring's prose mention is ignored."""
        lines = text.splitlines()
        begin_idx = end_idx = None
        for i, line in enumerate(lines):
            stripped = line.strip()
            if begin_idx is None and stripped == "# CANONICAL DECIDE BEGIN":
                begin_idx = i
            elif begin_idx is not None and stripped == "# CANONICAL DECIDE END":
                end_idx = i
                break
        if begin_idx is None or end_idx is None:
            raise AssertionError(
                "missing CANONICAL DECIDE BEGIN/END markers — "
                "first 30 lines were:\n" + "\n".join(lines[:30])
            )
        return lines[begin_idx + 1:end_idx]

    def test_blocks_match(self):
        here = os.path.dirname(__file__)
        with open(os.path.join(here, "sweep_cf_decide.py"), "r", encoding="utf-8") as f:
            py_block = self._slice_canonical(f.read())
        with open(os.path.join(here, "sweep-cf-orphans.sh"), "r", encoding="utf-8") as f:
            sh_block = self._slice_canonical(f.read())
        self.assertEqual(
            py_block,
            sh_block,
            "CANONICAL DECIDE block has drifted between sweep_cf_decide.py "
            "and sweep-cf-orphans.sh — re-sync them.",
        )


if __name__ == "__main__":
    unittest.main()
