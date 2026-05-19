#!/usr/bin/env python3
# Unit tests for sop-checklist.py
#
# Run:  python3 .gitea/scripts/tests/test_sop_checklist.py
#   or:  pytest .gitea/scripts/tests/test_sop_checklist.py
#
# RFC#351 Step 2 of 6 — implementation MVP. Tests cover:
#   - slug normalization (the 4 example variants in the script header)
#   - parse_directives (ack, revoke, with/without note, mid-comment, etc.)
#   - section_marker_present (empty answer rejected, filled answer ok)
#   - compute_ack_state (self-ack rejected, team probe applied, revoke
#     invalidates own prior ack, peer's ack survives unrevoked)
#   - render_status (state + description format)
#   - get_tier_mode (label-driven, default fallback)
#   - load_config (default config parses cleanly with both PyYAML and
#     the bundled minimal parser)
#
# All tests run WITHOUT touching the Gitea API — the team-probe
# callable is dependency-injected.

from __future__ import annotations

import os
import sys
import tempfile
import unittest

# Resolve sibling script regardless of where pytest is invoked from.
HERE = os.path.dirname(os.path.abspath(__file__))
PARENT = os.path.dirname(HERE)  # .gitea/scripts
sys.path.insert(0, PARENT)

import importlib.util  # noqa: E402

_spec = importlib.util.spec_from_file_location(
    "sop_checklist", os.path.join(PARENT, "sop-checklist.py")
)
sop = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(sop)  # type: ignore[union-attr]


# ---------------------------------------------------------------------------
# Test fixtures
# ---------------------------------------------------------------------------

CONFIG_PATH = os.path.join(PARENT, "..", "sop-checklist-config.yaml")


def _items() -> list[dict]:
    cfg = sop.load_config(CONFIG_PATH)
    return cfg["items"]


def _items_by_slug() -> dict[str, dict]:
    return {it["slug"]: it for it in _items()}


def _numeric_aliases() -> dict[int, str]:
    return {
        int(it["numeric_alias"]): it["slug"]
        for it in _items()
        if it.get("numeric_alias")
    }


def _comment(user: str, body: str) -> dict:
    return {"user": {"login": user}, "body": body}


# ---------------------------------------------------------------------------
# normalize_slug
# ---------------------------------------------------------------------------


class TestNormalizeSlug(unittest.TestCase):
    def test_kebab_already(self):
        self.assertEqual(sop.normalize_slug("comprehensive-testing"), "comprehensive-testing")

    def test_underscore_to_dash(self):
        self.assertEqual(sop.normalize_slug("comprehensive_testing"), "comprehensive-testing")

    def test_space_to_dash(self):
        self.assertEqual(sop.normalize_slug("comprehensive testing"), "comprehensive-testing")

    def test_uppercase_to_lower(self):
        self.assertEqual(sop.normalize_slug("Comprehensive-Testing"), "comprehensive-testing")

    def test_mixed_separators(self):
        self.assertEqual(sop.normalize_slug("Comprehensive_Testing"), "comprehensive-testing")
        self.assertEqual(sop.normalize_slug("FIVE_axis review"), "five-axis-review")

    def test_collapse_repeated_dashes(self):
        self.assertEqual(sop.normalize_slug("comprehensive--testing"), "comprehensive-testing")
        self.assertEqual(sop.normalize_slug("comprehensive  testing"), "comprehensive-testing")

    def test_strip_trailing_punctuation(self):
        self.assertEqual(sop.normalize_slug("comprehensive-testing."), "comprehensive-testing")
        self.assertEqual(sop.normalize_slug("comprehensive-testing!"), "comprehensive-testing")

    def test_numeric_shorthand_known(self):
        self.assertEqual(
            sop.normalize_slug("1", _numeric_aliases()),
            "comprehensive-testing",
        )
        self.assertEqual(
            sop.normalize_slug("3", _numeric_aliases()),
            "staging-smoke",
        )
        self.assertEqual(
            sop.normalize_slug("7", _numeric_aliases()),
            "memory-consulted",
        )

    def test_numeric_shorthand_unknown_returns_empty(self):
        # "8" is out of range → empty so caller can flag as unparseable.
        self.assertEqual(sop.normalize_slug("8", _numeric_aliases()), "")

    def test_numeric_without_alias_table_keeps_digits(self):
        # No alias table → return the digits as-is.
        self.assertEqual(sop.normalize_slug("1"), "1")

    def test_empty_input(self):
        self.assertEqual(sop.normalize_slug(""), "")
        self.assertEqual(sop.normalize_slug("   "), "")
        self.assertEqual(sop.normalize_slug(None), "")


# ---------------------------------------------------------------------------
# parse_directives
# ---------------------------------------------------------------------------


class TestParseDirectives(unittest.TestCase):
    def setUp(self):
        self.aliases = _numeric_aliases()

    def parse_ack_revoke(self, body):
        directives, na_directives = sop.parse_directives(body, self.aliases)
        self.assertEqual(na_directives, [])
        return directives

    def test_simple_ack(self):
        d = self.parse_ack_revoke("/sop-ack comprehensive-testing")
        self.assertEqual(d, [("sop-ack", "comprehensive-testing", "")])

    def test_simple_revoke(self):
        d = self.parse_ack_revoke("/sop-revoke staging-smoke")
        self.assertEqual(d, [("sop-revoke", "staging-smoke", "")])

    def test_ack_with_note(self):
        d = self.parse_ack_revoke(
            "/sop-ack comprehensive-testing LGTM the test covers all edge cases"
        )
        self.assertEqual(len(d), 1)
        self.assertEqual(d[0][0], "sop-ack")
        self.assertEqual(d[0][1], "comprehensive-testing")
        self.assertIn("LGTM", d[0][2])

    def test_numeric_shorthand(self):
        d = self.parse_ack_revoke("/sop-ack 1")
        self.assertEqual(d, [("sop-ack", "comprehensive-testing", "")])

    def test_revoke_with_reason(self):
        d = self.parse_ack_revoke(
            "/sop-revoke comprehensive-testing realized the e2e was mocking the DB"
        )
        self.assertEqual(d[0][0], "sop-revoke")
        self.assertEqual(d[0][1], "comprehensive-testing")
        self.assertIn("mocking", d[0][2])

    def test_directive_in_middle_of_comment(self):
        body = (
            "Reviewed the PR, looks good overall.\n"
            "/sop-ack comprehensive-testing\n"
            "Will follow up on the doc nit separately."
        )
        d = self.parse_ack_revoke(body)
        self.assertEqual(len(d), 1)
        self.assertEqual(d[0][1], "comprehensive-testing")

    def test_multiple_directives_in_one_comment(self):
        body = (
            "/sop-ack comprehensive-testing\n"
            "/sop-ack local-postgres-e2e\n"
        )
        d = self.parse_ack_revoke(body)
        self.assertEqual(len(d), 2)
        slugs = {x[1] for x in d}
        self.assertEqual(slugs, {"comprehensive-testing", "local-postgres-e2e"})

    def test_must_be_at_line_start(self):
        # A directive embedded mid-line is not honored (prevents review
        # comments like "to /sop-ack you need..." from acting as acks).
        body = "If you want to /sop-ack comprehensive-testing reply in this thread"
        d = self.parse_ack_revoke(body)
        self.assertEqual(d, [])

    def test_leading_whitespace_allowed(self):
        body = "  /sop-ack comprehensive-testing"
        d = self.parse_ack_revoke(body)
        self.assertEqual(len(d), 1)

    def test_empty_body(self):
        self.assertEqual(sop.parse_directives("", self.aliases), ([], []))
        self.assertEqual(sop.parse_directives(None, self.aliases), ([], []))

    def test_normalization_applied(self):
        # /sop-ack Comprehensive_Testing → canonical comprehensive-testing
        d = self.parse_ack_revoke("/sop-ack Comprehensive_Testing")
        self.assertEqual(d[0][1], "comprehensive-testing")


# ---------------------------------------------------------------------------
# section_marker_present
# ---------------------------------------------------------------------------


class TestSectionMarkerPresent(unittest.TestCase):
    def test_marker_with_inline_answer(self):
        body = "- [ ] **Comprehensive testing performed**: Added 12 new tests covering null/empty/giant inputs."
        self.assertTrue(sop.section_marker_present(body, "Comprehensive testing performed"))

    def test_marker_with_empty_answer(self):
        body = "- [ ] **Comprehensive testing performed**:"
        self.assertFalse(sop.section_marker_present(body, "Comprehensive testing performed"))

    def test_marker_with_only_whitespace_answer(self):
        body = "- [ ] **Comprehensive testing performed**:    \n"
        self.assertFalse(sop.section_marker_present(body, "Comprehensive testing performed"))

    def test_marker_with_next_line_answer(self):
        body = (
            "- [ ] **Comprehensive testing performed**:\n"
            "      Yes — see attached log + 12 new unit tests in foo_test.py.\n"
        )
        self.assertTrue(sop.section_marker_present(body, "Comprehensive testing performed"))

    def test_marker_missing(self):
        body = "- [ ] **Local-postgres E2E run**: N/A — pure-frontend\n"
        self.assertFalse(sop.section_marker_present(body, "Comprehensive testing performed"))

    def test_case_insensitive_marker_match(self):
        body = "- [ ] **comprehensive TESTING performed**: yes"
        self.assertTrue(sop.section_marker_present(body, "Comprehensive testing performed"))

    def test_empty_body(self):
        self.assertFalse(sop.section_marker_present("", "X"))
        self.assertFalse(sop.section_marker_present(None, "X"))


# ---------------------------------------------------------------------------
# compute_ack_state
# ---------------------------------------------------------------------------


class TestComputeAckState(unittest.TestCase):
    def setUp(self):
        self.items = _items_by_slug()
        self.aliases = _numeric_aliases()

    @staticmethod
    def _approve_all(slug, users):
        return list(users)

    @staticmethod
    def _approve_none(slug, users):
        return []

    def _approve_only(self, allowed_users):
        return lambda slug, users: [u for u in users if u in allowed_users]

    def test_peer_ack_passes(self):
        comments = [_comment("bob", "/sop-ack comprehensive-testing")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["bob"])

    def test_self_ack_rejected(self):
        comments = [_comment("alice", "/sop-ack comprehensive-testing")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], [])
        self.assertEqual(state["comprehensive-testing"]["rejected"]["self_ack"], ["alice"])

    def test_not_in_team_rejected(self):
        comments = [_comment("eve", "/sop-ack comprehensive-testing")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_none
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], [])
        self.assertEqual(state["comprehensive-testing"]["rejected"]["not_in_team"], ["eve"])

    def test_revoke_invalidates_own_prior_ack(self):
        # Bob acks then later revokes — Bob no longer counts.
        comments = [
            _comment("bob", "/sop-ack comprehensive-testing"),
            _comment("bob", "/sop-revoke comprehensive-testing realized e2e was mocked"),
        ]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], [])

    def test_revoke_does_not_affect_others_acks(self):
        # Bob revokes his own ack; Carol's still counts.
        comments = [
            _comment("bob", "/sop-ack comprehensive-testing"),
            _comment("carol", "/sop-ack comprehensive-testing"),
            _comment("bob", "/sop-revoke comprehensive-testing"),
        ]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["carol"])

    def test_ack_after_revoke_restored(self):
        # Bob revokes then re-acks (e.g. after re-reviewing).
        comments = [
            _comment("bob", "/sop-ack comprehensive-testing"),
            _comment("bob", "/sop-revoke comprehensive-testing"),
            _comment("bob", "/sop-ack comprehensive-testing"),
        ]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["bob"])

    def test_numeric_shorthand_ack(self):
        # /sop-ack 1 → comprehensive-testing
        comments = [_comment("bob", "/sop-ack 1")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["bob"])

    def test_ack_for_unknown_slug_ignored(self):
        # Some other slug not in config — silently drop (doesn't crash).
        comments = [_comment("bob", "/sop-ack does-not-exist")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        for slug in self.items:
            self.assertEqual(state[slug]["ackers"], [])

    def test_multi_item_multi_user(self):
        comments = [
            _comment("bob", "/sop-ack comprehensive-testing\n/sop-ack staging-smoke"),
            _comment("carol", "/sop-ack five-axis-review"),
        ]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, self._approve_all
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["bob"])
        self.assertEqual(state["staging-smoke"]["ackers"], ["bob"])
        self.assertEqual(state["five-axis-review"]["ackers"], ["carol"])
        self.assertEqual(state["root-cause"]["ackers"], [])


# ---------------------------------------------------------------------------
# render_status
# ---------------------------------------------------------------------------


class TestRenderStatus(unittest.TestCase):
    def setUp(self):
        self.items = _items()
        self.items_by_slug = _items_by_slug()

    def _state_with(self, acked: list[str]) -> dict:
        return {
            it["slug"]: {
                "ackers": ["peer"] if it["slug"] in acked else [],
                "rejected": {"self_ack": [], "not_in_team": []},
            }
            for it in self.items
        }

    def test_all_acked_returns_success(self):
        all_slugs = [it["slug"] for it in self.items]
        state, desc = sop.render_status(
            self.items, self._state_with(all_slugs), {s: True for s in all_slugs}
        )
        self.assertEqual(state, "success")
        self.assertIn("7/7", desc)

    def test_partial_acked_returns_failure(self):
        state, desc = sop.render_status(
            self.items,
            self._state_with(["comprehensive-testing", "staging-smoke"]),
            {it["slug"]: True for it in self.items},
        )
        self.assertEqual(state, "failure")
        self.assertIn("2/7", desc)
        self.assertIn("missing", desc)

    def test_description_truncates_long_missing_list(self):
        # Only ack one — 6 missing should be summarized as "+N".
        state, desc = sop.render_status(
            self.items,
            self._state_with(["comprehensive-testing"]),
            {it["slug"]: True for it in self.items},
        )
        # Length budget: under 140 chars.
        self.assertLessEqual(len(desc), 140)
        self.assertIn("+", desc)  # +N elision marker

    def test_body_unfilled_surfaced(self):
        all_slugs = [it["slug"] for it in self.items]
        state, desc = sop.render_status(
            self.items,
            self._state_with(all_slugs),
            {it["slug"]: False for it in self.items},
        )
        self.assertEqual(state, "failure")
        self.assertIn("body-unfilled", desc)


# ---------------------------------------------------------------------------
# get_tier_mode
# ---------------------------------------------------------------------------


class TestGetTierMode(unittest.TestCase):
    def setUp(self):
        self.cfg = sop.load_config(CONFIG_PATH)

    def test_tier_high_is_hard(self):
        pr = {"labels": [{"name": "tier:high"}, {"name": "area:ci"}]}
        self.assertEqual(sop.get_tier_mode(pr, self.cfg), "hard")

    def test_tier_medium_is_hard(self):
        pr = {"labels": [{"name": "tier:medium"}]}
        self.assertEqual(sop.get_tier_mode(pr, self.cfg), "hard")

    def test_tier_low_is_soft(self):
        pr = {"labels": [{"name": "tier:low"}]}
        self.assertEqual(sop.get_tier_mode(pr, self.cfg), "soft")

    def test_no_tier_label_defaults_to_hard(self):
        # Per feedback_fix_root_not_symptom — never silently lower the bar.
        pr = {"labels": [{"name": "area:ci"}]}
        self.assertEqual(sop.get_tier_mode(pr, self.cfg), "hard")

    def test_no_labels_defaults_to_hard(self):
        self.assertEqual(sop.get_tier_mode({"labels": []}, self.cfg), "hard")
        self.assertEqual(sop.get_tier_mode({}, self.cfg), "hard")


# ---------------------------------------------------------------------------
# load_config
# ---------------------------------------------------------------------------


class TestLoadConfig(unittest.TestCase):
    def test_default_config_parses(self):
        cfg = sop.load_config(CONFIG_PATH)
        self.assertIn("items", cfg)
        self.assertEqual(len(cfg["items"]), 7)
        slugs = {it["slug"] for it in cfg["items"]}
        self.assertEqual(
            slugs,
            {
                "comprehensive-testing",
                "local-postgres-e2e",
                "staging-smoke",
                "root-cause",
                "five-axis-review",
                "no-backwards-compat",
                "memory-consulted",
            },
        )

    def test_default_config_tier_mode_shape(self):
        cfg = sop.load_config(CONFIG_PATH)
        self.assertEqual(cfg["tier_failure_mode"]["tier:high"], "hard")
        self.assertEqual(cfg["tier_failure_mode"]["tier:medium"], "hard")
        self.assertEqual(cfg["tier_failure_mode"]["tier:low"], "soft")
        self.assertEqual(cfg["default_mode"], "hard")

    def test_each_item_has_required_fields(self):
        cfg = sop.load_config(CONFIG_PATH)
        for it in cfg["items"]:
            self.assertIn("slug", it)
            self.assertIn("numeric_alias", it)
            self.assertIn("pr_section_marker", it)
            self.assertIn("required_teams", it)
            self.assertIsInstance(it["required_teams"], list)
            self.assertGreater(len(it["required_teams"]), 0)


# ---------------------------------------------------------------------------
# Edge case: full integration without team probe (dependency-injected)
# ---------------------------------------------------------------------------


class TestEndToEndAckFlow(unittest.TestCase):
    """All-7-items happy path with synthetic comments. Verifies the
    full pipeline minus the Gitea API."""

    def test_all_seven_acked_by_proper_teams(self):
        items = _items_by_slug()
        aliases = _numeric_aliases()
        comments = [
            _comment("qa-bot", "/sop-ack comprehensive-testing"),
            _comment("eng-bot", "/sop-ack local-postgres-e2e"),
            _comment("eng-bot", "/sop-ack staging-smoke"),
            _comment("mgr-bot", "/sop-ack root-cause"),
            _comment("eng-bot", "/sop-ack five-axis-review"),
            _comment("mgr-bot", "/sop-ack no-backwards-compat"),
            _comment("eng-bot", "/sop-ack memory-consulted"),
        ]

        def probe(slug, users):
            # Pretend every user is in every team.
            return list(users)

        state = sop.compute_ack_state(comments, "alice-author", items, aliases, probe)
        body = {it["slug"]: True for it in items.values()}
        items_list = list(items.values())
        result_state, desc = sop.render_status(items_list, state, body)
        self.assertEqual(result_state, "success")
        self.assertIn("7/7", desc)

    def test_all_acks_still_fail_when_body_section_unfilled(self):
        items = _items_by_slug()
        aliases = _numeric_aliases()
        comments = [
            _comment("qa-bot", "/sop-ack comprehensive-testing"),
            _comment("eng-bot", "/sop-ack local-postgres-e2e"),
            _comment("eng-bot", "/sop-ack staging-smoke"),
            _comment("mgr-bot", "/sop-ack root-cause"),
            _comment("eng-bot", "/sop-ack five-axis-review"),
            _comment("mgr-bot", "/sop-ack no-backwards-compat"),
            _comment("eng-bot", "/sop-ack memory-consulted"),
        ]

        def probe(slug, users):
            return list(users)

        state = sop.compute_ack_state(comments, "alice-author", items, aliases, probe)
        body = {it["slug"]: True for it in items.values()}
        body["root-cause"] = False
        items_list = list(items.values())
        result_state, desc = sop.render_status(items_list, state, body)
        self.assertEqual(result_state, "failure")
        self.assertIn("7/7", desc)
        self.assertIn("body-unfilled: root-cause", desc)


if __name__ == "__main__":
    unittest.main(verbosity=2)


# ---------------------------------------------------------------------------
# compute_na_state
# ---------------------------------------------------------------------------


class TestComputeNaState(unittest.TestCase):
    """Tests for /sop-n/a directive evaluation."""

    def test_no_na_declarations(self):
        cfg = sop.load_config(CONFIG_PATH)
        na_gates = cfg.get("n/a_gates", {})
        comments = []
        na_state = sop.compute_na_state(comments, "alice", na_gates, lambda *_: [])
        self.assertFalse(na_state["qa-review"]["declared"])
        self.assertFalse(na_state["security-review"]["declared"])

    def test_na_declared_by_authorized_user(self):
        cfg = sop.load_config(CONFIG_PATH)
        na_gates = cfg.get("n/a_gates", {})
        comments = [_comment("bob", "/sop-n/a qa-review N/A: pure tooling change")]
        na_state = sop.compute_na_state(comments, "alice", na_gates, lambda g, u: u)
        self.assertTrue(na_state["qa-review"]["declared"])
        self.assertEqual(na_state["qa-review"]["decl_ackers"], ["bob"])

    def test_na_declared_by_unauthorized_user_rejected(self):
        cfg = sop.load_config(CONFIG_PATH)
        na_gates = cfg.get("n/a_gates", {})
        comments = [_comment("mallory", "/sop-n/a qa-review N/A: not real team")]
        na_state = sop.compute_na_state(comments, "alice", na_gates, lambda g, u: [])
        self.assertFalse(na_state["qa-review"]["declared"])
        self.assertEqual(na_state["qa-review"]["rejected"]["not_in_team"], ["mallory"])

    def test_author_cannot_self_declare_na(self):
        cfg = sop.load_config(CONFIG_PATH)
        na_gates = cfg.get("n/a_gates", {})
        comments = [_comment("alice", "/sop-n/a qa-review N/A: I am the author")]
        na_state = sop.compute_na_state(comments, "alice", na_gates, lambda g, u: u)
        self.assertFalse(na_state["qa-review"]["declared"])

    def test_parse_directives_separates_na_from_ack(self):
        directives, na_directives = sop.parse_directives(
            "/sop-ack comprehensive-testing\n/sop-n/a qa-review N/A: no surface",
            {},
        )
        self.assertEqual(len(directives), 1)
        self.assertEqual(directives[0][0], "sop-ack")
        self.assertEqual(len(na_directives), 1)
        self.assertEqual(na_directives[0][0], "sop-n/a")
        self.assertEqual(na_directives[0][1], "qa-review")


# ---------------------------------------------------------------------------
# RFC#450 Option C — risk-classed two-eyes (governance fix for internal#442)
# ---------------------------------------------------------------------------


class TestIsHighRisk(unittest.TestCase):
    """The high-risk predicate decides which required_teams list applies.

    Predicate: tier:high label OR any label in cfg.high_risk_labels.
    """

    def setUp(self):
        self.cfg = sop.load_config(CONFIG_PATH)

    def test_no_labels_is_default_class(self):
        pr = {"labels": []}
        self.assertFalse(sop.is_high_risk(pr, self.cfg))

    def test_tier_high_is_high_risk(self):
        pr = {"labels": [{"name": "tier:high"}]}
        self.assertTrue(sop.is_high_risk(pr, self.cfg))

    def test_tier_low_is_default_class(self):
        pr = {"labels": [{"name": "tier:low"}]}
        self.assertFalse(sop.is_high_risk(pr, self.cfg))

    def test_tier_medium_is_default_class(self):
        # tier:medium alone is NOT high-risk (Option C — medium routes
        # to the wider engineers OR-set).
        pr = {"labels": [{"name": "tier:medium"}]}
        self.assertFalse(sop.is_high_risk(pr, self.cfg))

    def test_area_security_label_is_high_risk(self):
        pr = {"labels": [{"name": "tier:medium"}, {"name": "area:security"}]}
        self.assertTrue(sop.is_high_risk(pr, self.cfg))

    def test_area_schema_label_is_high_risk(self):
        pr = {"labels": [{"name": "area:schema"}]}
        self.assertTrue(sop.is_high_risk(pr, self.cfg))

    def test_area_identity_label_is_high_risk(self):
        pr = {"labels": [{"name": "area:identity"}]}
        self.assertTrue(sop.is_high_risk(pr, self.cfg))

    def test_area_fleet_image_label_is_high_risk(self):
        pr = {"labels": [{"name": "area:fleet-image"}]}
        self.assertTrue(sop.is_high_risk(pr, self.cfg))

    def test_area_gate_meta_label_is_high_risk(self):
        # Gate-meta = changes to sop-checklist/sop-tier-check itself.
        pr = {"labels": [{"name": "area:gate-meta"}]}
        self.assertTrue(sop.is_high_risk(pr, self.cfg))

    def test_unknown_area_label_is_default_class(self):
        pr = {"labels": [{"name": "area:docs"}]}
        self.assertFalse(sop.is_high_risk(pr, self.cfg))


class TestResolveRequiredTeams(unittest.TestCase):
    """The team resolver picks the elevated list only for high-risk PRs
    AND only when the item declares one — items without an elevated
    list always use the default required_teams."""

    def test_default_class_uses_default_teams(self):
        item = {"required_teams": ["engineers", "managers", "ceo"], "required_teams_high_risk": ["ceo"]}
        self.assertEqual(
            sop.resolve_required_teams(item, high_risk=False),
            ["engineers", "managers", "ceo"],
        )

    def test_high_risk_uses_elevated_teams(self):
        item = {"required_teams": ["engineers", "managers", "ceo"], "required_teams_high_risk": ["ceo"]}
        self.assertEqual(
            sop.resolve_required_teams(item, high_risk=True),
            ["ceo"],
        )

    def test_high_risk_without_elevated_falls_back_to_default(self):
        # Items that don't declare required_teams_high_risk (e.g.
        # comprehensive-testing, staging-smoke) are unaffected by risk-class.
        item = {"required_teams": ["engineers"]}
        self.assertEqual(
            sop.resolve_required_teams(item, high_risk=True),
            ["engineers"],
        )

    def test_empty_elevated_list_falls_back_to_default(self):
        # A defensive case: required_teams_high_risk: [] should not
        # silently lock out all approvers — fall back to the default
        # so the gate stays satisfiable. (Tightening should remove the
        # key, not set it to empty.)
        item = {"required_teams": ["engineers"], "required_teams_high_risk": []}
        self.assertEqual(
            sop.resolve_required_teams(item, high_risk=True),
            ["engineers"],
        )


class TestRootCauseAckEligibilityWidened(unittest.TestCase):
    """Closes internal#442: a non-author engineers-team ack now satisfies
    root-cause / no-backwards-compat for the default class.

    The dead-managers/ceo-persona-token gridlock is the symptom; the
    root cause is that sop-checklist ignored tier-class. These tests
    pin the new wider-default behavior so it can't regress silently.
    """

    def setUp(self):
        self.items = _items_by_slug()
        self.aliases = _numeric_aliases()

    @staticmethod
    def _approve_only(allowed):
        return lambda slug, users: [u for u in users if u in allowed]

    def test_engineers_ack_satisfies_root_cause_default_class(self):
        # Bob is in engineers only (not managers, not ceo). Default class.
        comments = [_comment("bob", "/sop-ack root-cause")]
        # Probe: bob is approved because root-cause now lists engineers.
        probe = self._approve_only({"bob"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe, high_risk=False
        )
        self.assertEqual(state["root-cause"]["ackers"], ["bob"])

    def test_engineers_ack_satisfies_no_backwards_compat_default_class(self):
        comments = [_comment("bob", "/sop-ack no-backwards-compat")]
        probe = self._approve_only({"bob"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe, high_risk=False
        )
        self.assertEqual(state["no-backwards-compat"]["ackers"], ["bob"])

    def test_engineers_ack_alone_fails_root_cause_when_high_risk(self):
        # High-risk PR: only ceo can ack. Engineers-only ack must fail.
        comments = [_comment("bob", "/sop-ack root-cause")]
        # Probe: bob is in engineers, not ceo. Under high_risk,
        # required_teams_high_risk=[ceo] → bob is NOT approved.
        # Probe receives the items + flag indirectly via main(); for
        # the unit-test path we inject a probe that rejects bob.
        probe = self._approve_only(set())  # nobody is in ceo
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe, high_risk=True
        )
        self.assertEqual(state["root-cause"]["ackers"], [])
        self.assertIn("bob", state["root-cause"]["rejected"]["not_in_team"])

    def test_ceo_ack_satisfies_root_cause_when_high_risk(self):
        # High-risk PR + ceo-team approver → passes (the senior path).
        comments = [_comment("hongming", "/sop-ack root-cause")]
        probe = self._approve_only({"hongming"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe, high_risk=True
        )
        self.assertEqual(state["root-cause"]["ackers"], ["hongming"])

    def test_self_ack_still_forbidden_even_with_widened_eligibility(self):
        # Author cannot self-ack — widening teams must NOT weaken
        # the non-author rule.
        comments = [_comment("alice", "/sop-ack root-cause")]
        probe = self._approve_only({"alice"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe, high_risk=False
        )
        self.assertEqual(state["root-cause"]["ackers"], [])
        self.assertIn("alice", state["root-cause"]["rejected"]["self_ack"])


class TestHighRiskClassUsesElevatedListInConfig(unittest.TestCase):
    """End-to-end: the shipped config + RFC#450 predicate must keep
    root-cause / no-backwards-compat gated on ceo for high-risk PRs."""

    def test_root_cause_high_risk_elevated_to_ceo_only(self):
        items = _items_by_slug()
        # tier:high alone makes the PR high-risk → root-cause needs ceo.
        self.assertEqual(
            sop.resolve_required_teams(items["root-cause"], high_risk=True),
            ["ceo"],
        )
        # Default class accepts engineers/managers/ceo.
        self.assertEqual(
            sorted(sop.resolve_required_teams(items["root-cause"], high_risk=False)),
            sorted(["engineers", "managers", "ceo"]),
        )

    def test_no_backwards_compat_high_risk_elevated_to_ceo_only(self):
        items = _items_by_slug()
        self.assertEqual(
            sop.resolve_required_teams(items["no-backwards-compat"], high_risk=True),
            ["ceo"],
        )
        self.assertEqual(
            sorted(sop.resolve_required_teams(items["no-backwards-compat"], high_risk=False)),
            sorted(["engineers", "managers", "ceo"]),
        )

    def test_other_items_unchanged_by_risk_class(self):
        # Items without required_teams_high_risk are unaffected.
        items = _items_by_slug()
        for slug in (
            "comprehensive-testing",
            "local-postgres-e2e",
            "staging-smoke",
            "five-axis-review",
            "memory-consulted",
        ):
            self.assertEqual(
                sop.resolve_required_teams(items[slug], high_risk=False),
                sop.resolve_required_teams(items[slug], high_risk=True),
                f"item {slug} should not be affected by risk-class",
            )
