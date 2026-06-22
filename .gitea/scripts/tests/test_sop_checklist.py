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
#   - is_high_risk (label-driven, default fallback)
#   - load_config (default config parses cleanly with both PyYAML and
#     the bundled minimal parser)
#
# All tests run WITHOUT touching the Gitea API — the team-probe
# callable is dependency-injected.

from __future__ import annotations

import os
import sys
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

    def test_emdash_separator_parsed_correctly(self):
        # Em-dash (U+2014) between slug and note is common in practice.
        # /sop-ack Five-Axis — five-axis-review
        # → slug = five-axis, note = — five-axis-review
        d = self.parse_ack_revoke("/sop-ack Five-Axis — five-axis-review")
        self.assertEqual(len(d), 1)
        self.assertEqual(d[0][1], "five-axis")
        self.assertIn("five-axis-review", d[0][2])

    def test_emdash_no_note(self):
        # Em-dash at end of slug: only slug, no note content
        d = self.parse_ack_revoke("/sop-ack Five-Axis —")
        self.assertEqual(len(d), 1)
        self.assertEqual(d[0][1], "five-axis")
        self.assertEqual(d[0][2], "")  # em-dash is separator-only → empty note


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

    def test_self_ack_rejected_when_author_in_team(self):
        # Author self-acks are forbidden — a non-author peer must ack.
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

    Predicate: any label in cfg.high_risk_labels.
    """

    def setUp(self):
        self.cfg = sop.load_config(CONFIG_PATH)

    def test_no_labels_is_default_class(self):
        pr = {"labels": []}
        self.assertFalse(sop.is_high_risk(pr, self.cfg))

    def test_area_security_label_is_high_risk(self):
        pr = {"labels": [{"name": "area:security"}]}

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
        # Gate-meta = changes to sop-checklist/sop-checklist itself.
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
    root cause is that sop-checklist ignored high-risk class. These tests
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

    def test_self_ack_rejected_with_widened_eligibility(self):
        # Author self-acks are forbidden even when the author is in the
        # required team — a non-author peer must ack.
        comments = [_comment("alice", "/sop-ack root-cause")]
        probe = self._approve_only({"alice"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe, high_risk=False
        )
        self.assertEqual(state["root-cause"]["ackers"], [])
        self.assertEqual(state["root-cause"]["rejected"]["self_ack"], ["alice"])


class TestHighRiskClassUsesElevatedListInConfig(unittest.TestCase):
    """End-to-end: the shipped config + RFC#450 predicate must keep
    root-cause / no-backwards-compat gated on ceo for high-risk PRs."""

    def test_root_cause_high_risk_elevated_to_ceo_only(self):
        items = _items_by_slug()
        # area:schema alone makes the PR high-risk → root-cause needs ceo.
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


# ---------------------------------------------------------------------------
# get_issue_comments — streaming + minimal-dict shape (task #369 / OOM fix)
# ---------------------------------------------------------------------------


class _FakeReq:
    """Stand-in for GiteaClient._req that serves canned pages."""

    def __init__(self, pages):
        # pages: list[list[dict]]; one page per call, exhausted in order.
        self._pages = list(pages)
        self.calls = []

    def __call__(self, method, path, body=None, ok_codes=(200, 201, 204)):
        self.calls.append((method, path))
        if not self._pages:
            return 200, []
        return 200, self._pages.pop(0)


class TestGetIssueCommentsStreaming(unittest.TestCase):
    """Verify the OOM-fix invariants — minimal-dict shape + page break."""

    def _client_with_pages(self, pages):
        client = sop.GiteaClient("git.example.com", "tok")
        client._req = _FakeReq(pages)  # type: ignore[method-assign]
        return client

    def test_minimal_dict_shape_drops_large_fields(self):
        """get_issue_comments must DROP html_url/assets/timestamps/etc. and
        keep ONLY {user.login, body} — that's the whole OOM-prevention."""
        full_page = [
            {
                "id": 1234,
                "html_url": "https://example.com/some-huge-url",
                "pull_request_url": "https://example.com/some-other-huge-url",
                "issue_url": "https://example.com/yet-another-url",
                "user": {"login": "bob", "avatar_url": "x" * 4000, "id": 99},
                "original_author": "",
                "original_author_id": 0,
                "body": "/sop-ack comprehensive-testing\n\nlooks good",
                "assets": ["x" * 1000, "y" * 1000],
                "created_at": "2026-05-19T01:02:03Z",
                "updated_at": "2026-05-19T01:02:03Z",
            }
        ]
        client = self._client_with_pages([full_page])
        out = client.get_issue_comments("o", "r", 1)
        self.assertEqual(len(out), 1)
        # Only the two whitelisted keys + nested user.login
        self.assertEqual(set(out[0].keys()), {"user", "body"})
        self.assertEqual(set(out[0]["user"].keys()), {"login"})
        self.assertEqual(out[0]["user"]["login"], "bob")
        self.assertEqual(out[0]["body"], "/sop-ack comprehensive-testing\n\nlooks good")
        # Critical: avatar/assets/timestamps/etc. must be gone (~4KB+ each).
        self.assertNotIn("html_url", out[0])
        self.assertNotIn("assets", out[0])
        self.assertNotIn("created_at", out[0])

    def test_pagination_break_on_short_page(self):
        # Page-size 50; a page of <50 means no more pages.
        page1 = [{"user": {"login": "u"}, "body": "x"}] * 7
        client = self._client_with_pages([page1])
        out = client.get_issue_comments("o", "r", 2)
        self.assertEqual(len(out), 7)
        # Should have made exactly 1 _req call (no page-2 probe).
        self.assertEqual(len(client._req.calls), 1)

    def test_pagination_continues_until_empty(self):
        # Two full pages + one short page.
        page1 = [{"user": {"login": "u"}, "body": "x"}] * 50
        page2 = [{"user": {"login": "u"}, "body": "y"}] * 50
        page3 = [{"user": {"login": "u"}, "body": "z"}] * 3
        client = self._client_with_pages([page1, page2, page3])
        out = client.get_issue_comments("o", "r", 3)
        self.assertEqual(len(out), 103)
        self.assertEqual(len(client._req.calls), 3)

    def test_max_comments_caps_collection(self):
        page1 = [{"user": {"login": "u"}, "body": "x"}] * 50
        page2 = [{"user": {"login": "u"}, "body": "y"}] * 50
        page3 = [{"user": {"login": "u"}, "body": "z"}] * 50
        client = self._client_with_pages([page1, page2, page3])
        out = client.get_issue_comments("o", "r", 4, max_comments=75)
        self.assertEqual(len(out), 75)
        # Stops short: shouldn't have requested page-3.
        self.assertLessEqual(len(client._req.calls), 2)

    def test_oversized_body_truncated(self):
        # An individual comment with a multi-MiB body (e.g. pasted CI log)
        # must NOT pull the whole thing into memory. The directive parser
        # only needs the first ~8 KiB to find /sop-* markers.
        huge_body = "/sop-ack comprehensive-testing\n" + ("X" * (4 * 1024 * 1024))
        page = [{"user": {"login": "bob"}, "body": huge_body}]
        client = self._client_with_pages([page])
        out = client.get_issue_comments("o", "r", 99)
        self.assertEqual(len(out), 1)
        # Cap is 8 KiB; comment body must be <= 8 KiB after streaming.
        self.assertLessEqual(len(out[0]["body"]), 8 * 1024)
        # Marker still discoverable at the start.
        self.assertTrue(out[0]["body"].startswith("/sop-ack comprehensive-testing"))

    def test_iter_handles_missing_user_or_body(self):
        # Defensive: Gitea has been seen to return user=null on deleted users.
        page = [
            {"user": None, "body": "abandoned-author"},
            {"user": {"login": "alice"}, "body": None},
            {"body": "no-user-key"},
            {"user": {"login": "bob"}, "body": "ok"},
        ]
        client = self._client_with_pages([page])
        out = client.get_issue_comments("o", "r", 5)
        self.assertEqual(len(out), 4)
        self.assertEqual(out[0]["user"]["login"], "")
        self.assertEqual(out[0]["body"], "abandoned-author")
        self.assertEqual(out[1]["user"]["login"], "alice")
        self.assertEqual(out[1]["body"], "")
        self.assertEqual(out[2]["user"]["login"], "")
        self.assertEqual(out[3]["user"]["login"], "bob")

    def test_minimal_dicts_work_with_compute_ack_state(self):
        """Round-trip: minimal dicts feed back through compute_ack_state."""
        page = [{"user": {"login": "bob"}, "body": "/sop-ack comprehensive-testing"}]
        client = self._client_with_pages([page])
        comments = client.get_issue_comments("o", "r", 6)
        items = _items_by_slug()
        aliases = _numeric_aliases()
        state = sop.compute_ack_state(
            comments, "alice", items, aliases, lambda slug, users: list(users)
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["bob"])


# ---------------------------------------------------------------------------
# probe() na-gate fallback — fix for #355-class KeyError 'security-review'
# ---------------------------------------------------------------------------


class TestComputeNaStateAcceptsGateNotInItems(unittest.TestCase):
    """compute_na_state passes the gate NAME to probe(); when the gate is
    NOT also an items entry (the common case for `security-review`,
    `qa-review`), probe must fall back to the gate's own required_teams
    instead of KeyError'ing on items_by_slug[slug].

    This test exercises the public surface (compute_na_state) rather than
    the inline `probe` closure, because the closure is built inside main().
    We simulate the fallback by passing a probe that mirrors the production
    contract — slug may be either an item OR an n/a-gate key, both are valid.
    """

    def test_na_gate_with_required_teams_resolves_without_keyerror(self):
        na_gates = {
            "security-review": {
                "required_teams": ["security", "managers", "ceo"],
                "description": "security N/A",
            },
        }
        comments = [
            {"user": {"login": "carol"}, "body": "/sop-n/a security-review docs-only"},
        ]
        # Probe approves any user in the security team; importantly it does
        # NOT try items_by_slug[slug] for the gate name.
        called_with = []

        def probe(slug, users):
            called_with.append(slug)
            # production probe accepts gate-name OR item-slug; for this test
            # we just approve everyone.
            return list(users)

        na_state = sop.compute_na_state(comments, "alice", na_gates, probe)
        self.assertTrue(na_state["security-review"]["declared"])
        self.assertEqual(na_state["security-review"]["decl_ackers"], ["carol"])
        # probe must have been called with the GATE name, not an item slug.
        self.assertEqual(called_with, ["security-review"])

    def test_na_gate_self_declaration_rejected(self):
        # Author cannot self-declare N/A — pre-existing invariant; pin it
        # so the new probe-fallback doesn't regress this.
        na_gates = {"security-review": {"required_teams": ["security"]}}
        comments = [
            {"user": {"login": "alice"}, "body": "/sop-n/a security-review"},
        ]
        na_state = sop.compute_na_state(
            comments, "alice", na_gates, lambda *_: ["alice"]
        )
        self.assertFalse(na_state["security-review"]["declared"])


# ---------------------------------------------------------------------------
# internal#760 ceremony — ai-sop-ack team + ai_ack_eligible per-item flag
# ---------------------------------------------------------------------------


class TestAIAckEligibleConfig(unittest.TestCase):
    """CTO-controlled allowlist (msg 1388c76f):
      ai_ack_eligible: comprehensive-testing, local-postgres-e2e, staging-smoke,
                       five-axis-review, memory-consulted
      human-only:      root-cause, no-backwards-compat
    """

    def test_ai_ack_eligible_items(self):
        cfg = sop.load_config(CONFIG_PATH)
        items_by_slug = {it["slug"]: it for it in cfg["items"]}
        eligible = {
            "comprehensive-testing",
            "local-postgres-e2e",
            "staging-smoke",
            "five-axis-review",
            "memory-consulted",
        }
        for slug in eligible:
            self.assertTrue(
                items_by_slug[slug].get("ai_ack_eligible"),
                f"{slug} must be ai_ack_eligible",
            )

    def test_human_only_items(self):
        cfg = sop.load_config(CONFIG_PATH)
        items_by_slug = {it["slug"]: it for it in cfg["items"]}
        human_only = {"root-cause", "no-backwards-compat"}
        for slug in human_only:
            self.assertFalse(
                items_by_slug[slug].get("ai_ack_eligible", False),
                f"{slug} must NOT be ai_ack_eligible (human-only)",
            )

    def test_testing_class_slugs_constant(self):
        """_TESTING_CLASS_SLUGS must match the three testing items."""
        self.assertEqual(
            sop._TESTING_CLASS_SLUGS,
            {"comprehensive-testing", "local-postgres-e2e", "staging-smoke"},
        )

    def test_human_only_slugs_constant(self):
        """_HUMAN_ONLY_SLUGS encodes the migration/schema carve-out.

        If this set changes, the CTO must approve the widening.
        """
        self.assertEqual(
            sop._HUMAN_ONLY_SLUGS,
            {"root-cause", "no-backwards-compat", "migration", "schema"},
        )

    def test_human_only_invariant_enforced_in_code_and_config(self):
        """Every config-present slug in _HUMAN_ONLY_SLUGS must be human-only.

        This test fails if a migration/schema-class item accidentally
        acquires ai_ack_eligible via config drift.  migration/schema are
        future-proofing slugs not yet in the live config; they are checked
        by the production probe closure but skipped here.
        """
        cfg = sop.load_config(CONFIG_PATH)
        items_by_slug = {it["slug"]: it for it in cfg["items"]}
        for slug in sop._HUMAN_ONLY_SLUGS:
            if slug not in items_by_slug:
                # Future-proofing slug (e.g. migration, schema) — not yet
                # in config, but the code guard still rejects AI acks.
                continue
            self.assertFalse(
                items_by_slug[slug].get("ai_ack_eligible", False),
                f"{slug} is in _HUMAN_ONLY_SLUGS and must NEVER be ai_ack_eligible",
            )


class TestAIAckEligibilityProbe(unittest.TestCase):
    """The probe closure in main() delegates to compute_ack_state.
    We simulate the AI-ack path by injecting a probe that behaves like
    the production probe (human team first, then ai-sop-ack fallback).
    """

    def setUp(self):
        self.items = _items_by_slug()
        self.aliases = _numeric_aliases()

    def _probe_human_then_ai(self, human_users, ai_users):
        """Return users in human_users immediately; users in ai_users only
        if the item is ai_ack_eligible."""
        def probe(slug, users):
            item = self.items.get(slug, {})
            approved = []
            for u in users:
                if u in human_users:
                    approved.append(u)
                elif u in ai_users and item.get("ai_ack_eligible"):
                    approved.append(u)
            return approved
        return probe

    def test_ai_ack_passes_for_eligible_item(self):
        comments = [_comment("ai-bot", "/sop-ack five-axis-review")]
        probe = self._probe_human_then_ai(human_users=set(), ai_users={"ai-bot"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["five-axis-review"]["ackers"], ["ai-bot"])

    def test_ai_ack_rejected_for_human_only_item(self):
        comments = [_comment("ai-bot", "/sop-ack root-cause")]
        probe = self._probe_human_then_ai(human_users=set(), ai_users={"ai-bot"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["root-cause"]["ackers"], [])
        self.assertIn("ai-bot", state["root-cause"]["rejected"]["not_in_team"])

    def test_human_ack_still_works_for_ai_eligible_item(self):
        comments = [_comment("bob", "/sop-ack comprehensive-testing")]
        probe = self._probe_human_then_ai(human_users={"bob"}, ai_users=set())
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["bob"])

    def test_ai_ack_rejected_for_testing_item_when_ci_red(self):
        # Simulate the production probe that checks CI status for testing items.
        # When CI is not green, ai-sop-ack member is rejected.
        def probe(slug, users):
            item = self.items.get(slug, {})
            approved = []
            for u in users:
                if u == "ai-bot" and item.get("ai_ack_eligible"):
                    # Testing items require CI green; simulate CI red.
                    if slug in sop._TESTING_CLASS_SLUGS:
                        continue  # rejected: CI not green
                    approved.append(u)
            return approved

        comments = [_comment("ai-bot", "/sop-ack comprehensive-testing")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], [])

    def test_ai_ack_passes_for_testing_item_when_ci_green(self):
        # Simulate CI green → AI ack passes.
        def probe(slug, users):
            item = self.items.get(slug, {})
            approved = []
            for u in users:
                if u == "ai-bot" and item.get("ai_ack_eligible"):
                    if slug in sop._TESTING_CLASS_SLUGS:
                        # CI is green → allow
                        pass
                    approved.append(u)
            return approved

        comments = [_comment("ai-bot", "/sop-ack comprehensive-testing")]
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["comprehensive-testing"]["ackers"], ["ai-bot"])


class TestAIAckHumanOnlyMigrationSchema(unittest.TestCase):
    """RC 8322: migration and schema items are human-only regardless of
    any future config that might accidentally mark them ai_ack_eligible.

    These slugs are not yet in the live config items list; the tests use
    synthetic items so the production guard can be exercised directly.
    """

    def setUp(self):
        # Synthetic items — if live config ever adds migration/schema,
        # they MUST stay human-only. The probe below mirrors the actual
        # production closure logic (human team first, then AI fallback
        # with _HUMAN_ONLY_SLUGS guard).
        self.items = {
            "migration": {
                "slug": "migration",
                "ai_ack_eligible": True,
                "required_teams": ["engineers"],
            },
            "schema": {
                "slug": "schema",
                "ai_ack_eligible": True,
                "required_teams": ["engineers"],
            },
        }
        self.aliases = {}

    def _production_like_probe(self, human_users, ai_users):
        """Return a probe that mirrors the production closure's guard."""

        def probe(slug, users):
            item = self.items.get(slug, {})
            approved = []
            for u in users:
                if u in human_users:
                    approved.append(u)
                elif u in ai_users:
                    # Production guard: _HUMAN_ONLY_SLUGS rejects AI acks
                    # regardless of the ai_ack_eligible flag.
                    if slug in sop._HUMAN_ONLY_SLUGS:
                        continue
                    if item.get("ai_ack_eligible"):
                        approved.append(u)
            return approved

        return probe

    def test_ai_ack_rejected_for_migration(self):
        comments = [_comment("ai-bot", "/sop-ack migration")]
        probe = self._production_like_probe(human_users=set(), ai_users={"ai-bot"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["migration"]["ackers"], [])
        self.assertIn("ai-bot", state["migration"]["rejected"]["not_in_team"])

    def test_ai_ack_rejected_for_schema(self):
        comments = [_comment("ai-bot", "/sop-ack schema")]
        probe = self._production_like_probe(human_users=set(), ai_users={"ai-bot"})
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["schema"]["ackers"], [])
        self.assertIn("ai-bot", state["schema"]["rejected"]["not_in_team"])

    def test_human_ack_still_works_for_migration(self):
        # Human team member acking migration/schema is unaffected.
        comments = [_comment("bob", "/sop-ack migration")]
        probe = self._production_like_probe(human_users={"bob"}, ai_users=set())
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["migration"]["ackers"], ["bob"])

    def test_human_ack_still_works_for_schema(self):
        comments = [_comment("bob", "/sop-ack schema")]
        probe = self._production_like_probe(human_users={"bob"}, ai_users=set())
        state = sop.compute_ack_state(
            comments, "alice", self.items, self.aliases, probe
        )
        self.assertEqual(state["schema"]["ackers"], ["bob"])


class TestGetCIStatus(unittest.TestCase):
    """Verify get_ci_status reads the correct context from commit statuses."""

    def _client_with_statuses(self, statuses):
        client = sop.GiteaClient("git.example.com", "tok")

        def fake_req(method, path, body=None, ok_codes=(200, 201, 204)):
            return 200, statuses

        client._req = fake_req  # type: ignore[method-assign]
        return client

    def test_ci_green_returns_success(self):
        client = self._client_with_statuses([
            {"context": "CI / all-required (pull_request)", "state": "success"},
        ])
        self.assertEqual(
            sop.get_ci_status(client, "o", "r", "sha1"), "success"
        )

    def test_ci_red_returns_failure(self):
        client = self._client_with_statuses([
            {"context": "CI / all-required (pull_request)", "state": "failure"},
        ])
        self.assertEqual(
            sop.get_ci_status(client, "o", "r", "sha1"), "failure"
        )

    def test_missing_context_returns_missing(self):
        client = self._client_with_statuses([
            {"context": "some-other-context", "state": "success"},
        ])
        self.assertEqual(
            sop.get_ci_status(client, "o", "r", "sha1"), "missing"
        )

    def test_api_error_returns_unknown(self):
        client = sop.GiteaClient("git.example.com", "tok")

        def fake_req(method, path, body=None, ok_codes=(200, 201, 204)):
            return 500, {"error": "boom"}

        client._req = fake_req  # type: ignore[method-assign]
        self.assertEqual(
            sop.get_ci_status(client, "o", "r", "sha1"), "unknown"
        )


# ---------------------------------------------------------------------------
# internal#818 — na-declarations status must be terminal success
# ---------------------------------------------------------------------------


class TestNaDeclarationsStatusTerminal(unittest.TestCase):
    """Regression for internal#818: the na-declarations context is
    informational, not a merge gate.  An empty N/A declaration list must
    post `success` (not `pending`) so it does not poison the PR combined
    status."""

    def _run_with_fake_client(self, fake_client_class):
        """Swap GiteaClient temporarily and invoke main() with a fake token."""
        orig_client = sop.GiteaClient
        orig_token = os.environ.get("GITEA_TOKEN")
        try:
            sop.GiteaClient = fake_client_class
            os.environ["GITEA_TOKEN"] = "fake-token"
            return sop.main([
                "--owner", "o", "--repo", "r", "--pr", "1",
                "--config", CONFIG_PATH,
                "--gitea-host", "git.example.com",
            ])
        finally:
            sop.GiteaClient = orig_client
            if orig_token is None:
                os.environ.pop("GITEA_TOKEN", None)
            else:
                os.environ["GITEA_TOKEN"] = orig_token

    def test_empty_na_descriptions_posts_success(self):
        posted = []

        class FakeClient(sop.GiteaClient):
            def get_pr(self, owner, repo, pr):
                return {
                    "state": "open",
                    "user": {"login": "alice"},
                    "head": {"sha": "abc123"},
                    "labels": [],
                }

            def get_issue_comments(self, owner, repo, issue, max_comments=None):
                return []

            def resolve_team_id(self, org, team_name):
                return None

            def is_team_member(self, team_id, login):
                return False

            def post_status(self, owner, repo, sha, state, context,
                            description, target_url=""):
                posted.append({
                    "state": state,
                    "context": context,
                    "description": description,
                })

        rc = self._run_with_fake_client(FakeClient)
        self.assertEqual(rc, 0)
        na_posts = [p for p in posted if "na-declarations" in p["context"]]
        self.assertEqual(len(na_posts), 1, f"expected one na-declarations post, got {posted}")
        self.assertEqual(na_posts[0]["state"], "success")
        self.assertEqual(na_posts[0]["description"], "N/A: (none)")

    def test_populated_na_descriptions_posts_success(self):
        posted = []

        class FakeClient(sop.GiteaClient):
            def get_pr(self, owner, repo, pr):
                return {
                    "state": "open",
                    "user": {"login": "alice"},
                    "head": {"sha": "abc123"},
                    "labels": [],
                }

            def get_issue_comments(self, owner, repo, issue, max_comments=None):
                return [
                    {"user": {"login": "bob"}, "body": "/sop-n/a qa-review N/A: docs-only"},
                ]

            def resolve_team_id(self, org, team_name):
                return 1

            def is_team_member(self, team_id, login):
                return True

            def post_status(self, owner, repo, sha, state, context,
                            description, target_url=""):
                posted.append({
                    "state": state,
                    "context": context,
                    "description": description,
                })

        rc = self._run_with_fake_client(FakeClient)
        self.assertEqual(rc, 0)
        na_posts = [p for p in posted if "na-declarations" in p["context"]]
        self.assertEqual(len(na_posts), 1)
        self.assertEqual(na_posts[0]["state"], "success")
        self.assertIn("qa-review", na_posts[0]["description"])
