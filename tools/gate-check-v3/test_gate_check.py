import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("gate_check.py")


def load_gate_check():
    spec = importlib.util.spec_from_file_location("gate_check", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


def test_run_skips_pr_not_targeting_default_branch(monkeypatch):
    mod = load_gate_check()

    def fake_api_get(path):
        assert path == "/repos/molecule-ai/molecule-core/pulls/843"
        return {
            "number": 843,
            "base": {"ref": "staging"},
            "head": {"sha": "84b9ca3a129075b8d5159eda5e678f68be1af20f"},
        }

    monkeypatch.setenv("DEFAULT_BRANCH", "main")
    monkeypatch.setattr(mod, "api_get", fake_api_get)

    result = mod.run("molecule-ai/molecule-core", 843, post_comment=False)

    assert result["verdict"] == "CLEAR"
    assert result["skipped"] is True
    assert "staging" in result["reason"]


def test_signal_1_infra_sre_login_alias_resolved_to_core_devops(monkeypatch):
    """infra-sre posts [devops-agent] APPROVED → engineers gate satisfied via LOGIN_ALIASES."""
    mod = load_gate_check()

    def fake_api_get(path):
        # PR 900 has area:ci label
        if path == "/repos/molecule-ai/molecule-core/pulls/900":
            return {
                "number": 900,
                "labels": [{"name": "area:ci"}],
            }
        raise AssertionError(f"unexpected api_get: {path}")

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/issues/900/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/900/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/900/reviews":
            return [
                {
                    "id": 1,
                    "user": {"login": "infra-sre"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:00Z",
                },
                {
                    "id": 2,
                    "user": {"login": "core-lead"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:01Z",
                },
                {
                    "id": 3,
                    "user": {"login": "core-qa"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:02Z",
                },
                {
                    "id": 4,
                    "user": {"login": "core-security"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:03Z",
                },
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)
    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_1_comment_scan(900, "molecule-ai/molecule-core")

    assert result["verdict"] == "CLEAR"
    assert result["signal"] == "agent_tag_comments"
    # infra-sre (aliased to core-devops) should satisfy engineers gate
    engineers = result["results"]["core-devops"]
    assert engineers["verdict"] == "APPROVED"
    assert engineers["group"] == "engineers"


def test_signal_1_null_user_in_review_does_not_crash(monkeypatch):
    """Regression: Gitea may return reviews with user=null (deleted/bot edge case).
    signal_1_comment_scan must survive this without AttributeError."""
    mod = load_gate_check()

    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/901":
            return {
                "number": 901,
                "labels": [{"name": "area:ci"}],
            }
        raise AssertionError(f"unexpected api_get: {path}")

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/issues/901/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/901/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/901/reviews":
            return [
                {
                    "id": 1,
                    "user": None,  # <-- the regression trigger
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:00Z",
                },
                {
                    "id": 2,
                    "user": {"login": "core-devops"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:01:00Z",
                },
                {
                    "id": 3,
                    "user": {"login": "core-lead"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:01:01Z",
                },
                {
                    "id": 4,
                    "user": {"login": "core-qa"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:01:02Z",
                },
                {
                    "id": 5,
                    "user": {"login": "core-security"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:01:03Z",
                },
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)
    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_1_comment_scan(901, "molecule-ai/molecule-core")

    # Should not crash; all required gates clear
    assert result["verdict"] == "CLEAR"
    assert result["results"]["core-devops"]["verdict"] == "APPROVED"


# ── Signal 2: Draft REQUEST_CHANGES guard ───────────────────────────────────


def test_signal_2_draft_request_changes_does_not_block(monkeypatch):
    """official=False REQUEST_CHANGES is a draft/pending review and must NOT
    block the gate (matching review-check.sh post-#1818 official-filter)."""
    mod = load_gate_check()

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/902/reviews":
            return [
                {
                    "id": 1,
                    "user": {"login": "agent-reviewer"},
                    "state": "REQUEST_CHANGES",
                    "official": False,
                    "dismissed": False,
                    "submitted_at": "2026-05-13T10:00:00Z",
                }
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_2_reviews(902, "molecule-ai/molecule-core")
    assert result["verdict"] == "CLEAR"
    assert result["blocking_reviews"] == []


def test_signal_2_null_user_in_request_changes_does_not_crash(monkeypatch):
    """Regression: Gitea may return user=null on a REQUEST_CHANGES review.
    signal_2_reviews must survive this without AttributeError."""
    mod = load_gate_check()

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/903/reviews":
            return [
                {
                    "id": 1,
                    "user": None,
                    "state": "REQUEST_CHANGES",
                    "official": True,
                    "dismissed": False,
                    "submitted_at": "2026-05-13T10:00:00Z",
                }
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_2_reviews(903, "molecule-ai/molecule-core")
    assert result["verdict"] == "CLEAR"
    assert result["blocking_reviews"] == []


# ── Signal 4: Branch divergence / scope-creep guard ─────────────────────────


def test_signal_4_no_divergence_returns_clear(monkeypatch):
    """When PR.base.sha equals target branch HEAD, divergence is zero."""
    mod = load_gate_check()

    shared_sha = "abc123"

    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/100":
            return {
                "base": {"sha": shared_sha, "ref": "main"},
                "head": {"sha": "def456"},
            }
        if path == "/repos/molecule-ai/molecule-core/branches/main":
            return {"commit": {"id": shared_sha}}
        raise AssertionError(f"unexpected api_get: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)

    result = mod.signal_4_branch_divergence(100, "molecule-ai/molecule-core")

    assert result["verdict"] == "CLEAR"
    assert result["diverged"] is False
    assert result["commits_behind"] == 0
    assert result["inherited_fraction"] == 0.0


def test_signal_4_divergence_with_inherited_files_warning(monkeypatch):
    """Stale branch with overlapping files triggers WARNING and correct fractions."""
    mod = load_gate_check()

    base_sha = "base000"
    target_head = "head111"

    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/101":
            return {
                "base": {"sha": base_sha, "ref": "main"},
                "head": {"sha": "pr222"},
            }
        if path == "/repos/molecule-ai/molecule-core/branches/main":
            return {"commit": {"id": target_head}}
        if path == "/repos/molecule-ai/molecule-core/commits?sha=main&page=1&limit=50":
            return [
                {
                    "sha": target_head,
                    "files": [
                        {"filename": "ci.yml"},
                        {"filename": "README.md"},
                    ],
                },
                {"sha": base_sha, "files": []},
            ]
        raise AssertionError(f"unexpected api_get: {path}")

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/101/files":
            return [
                {"filename": "ci.yml"},
                {"filename": "README.md"},
                {"filename": "new_feature.go"},
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)
    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_4_branch_divergence(101, "molecule-ai/molecule-core")

    assert result["verdict"] == "WARNING"
    assert result["diverged"] is True
    assert result["commits_behind"] == 1
    assert result["pr_files_count"] == 3
    assert result["inherited_files"] == ["README.md", "ci.yml"]
    assert result["new_work_files"] == ["new_feature.go"]
    assert result["inherited_fraction"] == round(2 / 3, 2)


def test_signal_4_divergence_no_inherited_files_clear(monkeypatch):
    """Stale branch but zero file overlap → still CLEAR (no scope-creep risk)."""
    mod = load_gate_check()

    base_sha = "base000"
    target_head = "head111"

    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/102":
            return {
                "base": {"sha": base_sha, "ref": "main"},
                "head": {"sha": "pr222"},
            }
        if path == "/repos/molecule-ai/molecule-core/branches/main":
            return {"commit": {"id": target_head}}
        if path == "/repos/molecule-ai/molecule-core/commits?sha=main&page=1&limit=50":
            return [
                {
                    "sha": target_head,
                    "files": [{"filename": "other.go"}],
                },
                {"sha": base_sha, "files": []},
            ]
        raise AssertionError(f"unexpected api_get: {path}")

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/102/files":
            return [{"filename": "new_feature.go"}]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)
    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_4_branch_divergence(102, "molecule-ai/molecule-core")

    assert result["verdict"] == "CLEAR"
    assert result["diverged"] is True
    assert result["inherited_files"] == []
    assert result["new_work_files"] == ["new_feature.go"]
    assert result["inherited_fraction"] == 0.0


def test_signal_4_branch_api_error_returns_na(monkeypatch):
    """If the branch endpoint 404s, signal degrades to N/A rather than crashing."""
    mod = load_gate_check()

    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/103":
            return {
                "base": {"sha": "base000", "ref": "main"},
                "head": {"sha": "pr222"},
            }
        if path == "/repos/molecule-ai/molecule-core/branches/main":
            raise mod.GiteaError("GET .../branches/main → 404: not found")
        raise AssertionError(f"unexpected api_get: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)

    result = mod.signal_4_branch_divergence(103, "molecule-ai/molecule-core")

    assert result["verdict"] == "N/A"
    assert "error" in result


# ── Signal 6: CI required checks ────────────────────────────────────────────


def _signal_6_api_get(required_checks, statuses):
    """Return a fake_api_get closure for signal_6 tests."""
    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/200":
            return {"base": {"sha": "base000", "ref": "main"}, "head": {"sha": "pr222"}}
        if path == "/repos/molecule-ai/molecule-core/commits/pr222/status":
            return {"state": "failure", "statuses": statuses}
        if path == "/repos/molecule-ai/molecule-core/branches/main/protection":
            return {"required_status_checks": {"checks": [{"context": c} for c in required_checks]}}
        raise AssertionError(f"unexpected api_get: {path}")
    return fake_api_get


def test_signal_6_missing_required_context_returns_ci_pending(monkeypatch):
    """A required check that is ABSENT from the status list is treated as missing,
    which is fail-closed → CI_PENDING (never ready-by-absence)."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[
                "qa-review / approved (pull_request_target)",
                "security-review / approved (pull_request_target)",
            ],
            statuses=[
                {"context": "qa-review / approved (pull_request_target)", "status": "success"},
                # security-review is completely missing
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CI_PENDING"
    assert "security-review / approved (pull_request_target)" in result["pending_required"]


def test_signal_6_pending_required_context_returns_ci_pending(monkeypatch):
    """A required check with status 'pending' blocks the gate with CI_PENDING."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[
                "qa-review / approved (pull_request_target)",
                "security-review / approved (pull_request_target)",
                "sop-checklist / all-items-acked (pull_request_target)",
            ],
            statuses=[
                {"context": "qa-review / approved (pull_request_target)", "status": "success"},
                {"context": "security-review / approved (pull_request_target)", "status": "pending"},
                {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CI_PENDING"
    assert "security-review / approved (pull_request_target)" in result["pending_required"]


def test_signal_6_failing_required_context_returns_ci_fail(monkeypatch):
    """A required check with status 'failure' blocks the gate with CI_FAIL."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[
                "qa-review / approved (pull_request_target)",
                "security-review / approved (pull_request_target)",
                "sop-checklist / all-items-acked (pull_request_target)",
                "CI / all-required (pull_request)",
            ],
            statuses=[
                {"context": "qa-review / approved (pull_request_target)", "status": "failure"},
                {"context": "security-review / approved (pull_request_target)", "status": "success"},
                {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
                {"context": "CI / all-required (pull_request)", "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CI_FAIL"
    assert "qa-review / approved (pull_request_target)" in result["failing_required"]


def test_signal_6_all_required_green_returns_clear(monkeypatch):
    """When every required check is success/neutral, the gate is CLEAR."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[
                "qa-review / approved (pull_request_target)",
                "security-review / approved (pull_request_target)",
                "sop-checklist / all-items-acked (pull_request_target)",
                "CI / all-required (pull_request)",
            ],
            statuses=[
                {"context": "qa-review / approved (pull_request_target)", "status": "success"},
                {"context": "security-review / approved (pull_request_target)", "status": "success"},
                {"context": "sop-checklist / all-items-acked (pull_request_target)", "status": "success"},
                {"context": "CI / all-required (pull_request)", "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CLEAR"
    assert result["pending_required"] == []
    assert result["failing_required"] == []


def test_signal_6_governance_checks_always_required_even_when_bp_empty(monkeypatch):
    """Uniform gate: qa/security/sop are REQUIRED even if branch protection
    does not enumerate them. A PR with only CI/all-required green but missing
    governance contexts must be CI_PENDING (fail-closed)."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[],  # BP lists nothing
            statuses=[
                {"context": "CI / all-required (pull_request)", "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CI_PENDING"
    assert "qa-review / approved (pull_request_target)" in result["pending_required"]
    assert "security-review / approved (pull_request_target)" in result["pending_required"]
    assert "sop-checklist / all-items-acked (pull_request_target)" in result["pending_required"]


# ── Signal 6 regression tests for molecule-core#2589 ─────────────────────────

TRUSTED_QA = "qa-review / approved (pull_request_target)"
TRUSTED_SECURITY = "security-review / approved (pull_request_target)"
TRUSTED_SOP = "sop-checklist / all-items-acked (pull_request_target)"
UNTRUSTED_QA = "qa-review / approved (pull_request)"
UNTRUSTED_SECURITY = "security-review / approved (pull_request)"
UNTRUSTED_SOP = "sop-checklist / all-items-acked (pull_request)"


def test_signal_6_trusted_governance_contexts_clear(monkeypatch):
    """#2589 regression: gate is satisfied ONLY by trusted (pull_request_target)
    governance contexts."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[],
            statuses=[
                {"context": TRUSTED_QA, "status": "success"},
                {"context": TRUSTED_SECURITY, "status": "success"},
                {"context": TRUSTED_SOP, "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CLEAR"
    assert result["passing_required"] == [TRUSTED_QA, TRUSTED_SECURITY, TRUSTED_SOP]


def test_signal_6_untrusted_governance_contexts_do_not_satisfy(monkeypatch):
    """#2589 security regression: forged/untrusted (pull_request)-suffixed
    governance statuses must NOT satisfy the gate."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[],
            statuses=[
                # Attacker-controlled PR-head workflow posts the untrusted suffixes.
                {"context": UNTRUSTED_QA, "status": "success"},
                {"context": UNTRUSTED_SECURITY, "status": "success"},
                {"context": UNTRUSTED_SOP, "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] in ("CI_PENDING", "CI_FAIL")
    # Trusted contexts are still missing/unsatisfied.
    for ctx in (TRUSTED_QA, TRUSTED_SECURITY, TRUSTED_SOP):
        assert ctx in result["pending_required"]
    # Untrusted contexts are NOT counted as passing governance.
    for ctx in (UNTRUSTED_QA, UNTRUSTED_SECURITY, UNTRUSTED_SOP):
        assert ctx not in result["passing_required"]


def test_signal_6_status_collapse_uses_max_id(monkeypatch):
    """Gitea /commits/<sha>/statuses is non-monotonic by id; the gate must
    collapse duplicate contexts by max(id), not by list order."""
    mod = load_gate_check()
    monkeypatch.setattr(
        mod, "api_get",
        _signal_6_api_get(
            required_checks=[TRUSTED_QA],
            statuses=[
                # Older id claims success; newer id claims failure.
                # List order is deliberately opposite of id order.
                {"id": 3, "context": TRUSTED_QA, "status": "failure"},
                {"id": 1, "context": TRUSTED_QA, "status": "success"},
                {"id": 2, "context": TRUSTED_QA, "status": "success"},
            ],
        ),
    )
    result = mod.signal_6_ci(200, "molecule-ai/molecule-core")
    assert result["verdict"] == "CI_FAIL"
    assert TRUSTED_QA in result["failing_required"]
    assert result["all_check_statuses"][TRUSTED_QA] == "failure"



# ── core#2875 — signal_7 destructive-diff guard tests ───────────────────────


def test_signal_7_clear_when_diff_and_branch_both_small(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "CLEAR",
        "diverged": False, "commits_behind": 0, "pr_files_count": 0,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.0,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 5, "added_lines": 10, "deleted_lines": 5, "net_deleted_lines": 0,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "CLEAR"
    assert result["files_changed"] == 5
    assert result["diverged"] is False


def test_signal_7_warn_on_moderate_files_changed(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "CLEAR",
        "diverged": False, "commits_behind": 0, "pr_files_count": 0,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.0,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 60, "added_lines": 100, "deleted_lines": 50, "net_deleted_lines": 0,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "WARNING"
    assert result["files_changed"] == 60


def test_signal_7_warn_on_moderate_net_deleted(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "CLEAR",
        "diverged": False, "commits_behind": 0, "pr_files_count": 0,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.0,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 10, "added_lines": 100, "deleted_lines": 1500, "net_deleted_lines": 1400,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "WARNING"
    assert result["net_deleted_lines"] == 1400


def test_signal_7_warn_on_commits_behind_above_moderate(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 15, "pr_files_count": 5,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.0,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 5, "added_lines": 10, "deleted_lines": 5, "net_deleted_lines": 0,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "WARNING"
    assert result["commits_behind"] == 15


def test_signal_7_block_on_high_confidence_destructive_diff(monkeypatch):
    """The core#1100 / #2875 case: 481 files / -55k diff + stale branch."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": ["a.py", "b.py"], "new_work_files": ["c.py"], "inherited_fraction": 0.67,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "BLOCKED"
    assert result["files_changed"] == 481
    assert result["net_deleted_lines"] == 54800
    assert result["diverged"] is True
    assert "destructive diff" in result["reason"]


def test_signal_7_block_when_only_one_threshold_trips_but_branch_diverged(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": ["a.py"], "new_work_files": ["c.py"], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 250, "added_lines": 200, "deleted_lines": 50, "net_deleted_lines": 0,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "BLOCKED"


def test_signal_7_no_block_if_diff_destructive_but_branch_not_diverged(monkeypatch):
    """Destructive diff alone (no stale branch) does NOT BLOCK —
    prevents false-positive on large but intentional PRs."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "CLEAR",
        "diverged": False, "commits_behind": 0, "pr_files_count": 0,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.0,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "WARNING"


def test_signal_7_refactor_label_exempts_block(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"refactor": {"core-lead"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={"labels": [{"name": "refactor"}, {"name": "needs_review"}]},
    )
    assert result["verdict"] == "WARNING"
    assert result["refactor_exemption"] is True


def test_signal_7_migration_label_exempts_block(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 300, "added_lines": 100, "deleted_lines": 8000, "net_deleted_lines": 7900,
    })
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"migration": {"core-lead"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={"labels": [{"name": "migration"}]},
    )
    assert result["verdict"] == "WARNING"
    assert result["refactor_exemption"] is True


def test_signal_7_generated_label_exempts_block(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 250, "added_lines": 50, "deleted_lines": 100, "net_deleted_lines": 50,
    })
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"generated": {"core-lead"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={"labels": [{"name": "generated"}]},
    )
    assert result["verdict"] == "WARNING"


def test_signal_7_vendor_label_exempts_block(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 300, "added_lines": 10, "deleted_lines": 20000, "net_deleted_lines": 19990,
    })
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"vendor": {"core-lead"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={"labels": [{"name": "vendor"}]},
    )
    assert result["verdict"] == "WARNING"


def test_signal_7_case_insensitive_label_match(monkeypatch):
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"refactor": {"core-lead"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={"labels": [{"name": "Refactor"}]},
    )
    assert result["verdict"] == "WARNING"
    assert result["refactor_exemption"] is True


def test_signal_7_author_self_applied_refactor_label_does_not_exempt(monkeypatch):
    """core#2884: an author who can write labels must not be able to
    self-apply 'refactor' and downgrade their own destructive diff
    from BLOCK to WARN."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    # Author applied the exempt label themselves — must NOT be honored.
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"refactor": {"agent-dev-a"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={
            "user": {"login": "agent-dev-a"},
            "labels": [{"name": "refactor"}],
        },
    )
    assert result["verdict"] == "BLOCKED"
    assert result["refactor_exemption"] is False
    assert "destructive diff" in result["reason"]


def test_signal_7_non_author_label_remove_does_not_enable_author_self_exempt(monkeypatch):
    """core#2884 follow-up: a non-author who REMOVED the exempt label must
    not be counted as an applier. If the only non-author timeline event is a
    removal, the author-applied label is still treated as self-exempt and the
    destructive diff remains BLOCKED."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    # Timeline has a removal by a non-author but no non-author ADD.
    # The helper filters removals, so only the author add remains.
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"refactor": {"agent-dev-a"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={
            "user": {"login": "agent-dev-a"},
            "labels": [{"name": "refactor"}],
        },
    )
    assert result["verdict"] == "BLOCKED"
    assert result["refactor_exemption"] is False


def test_label_appliers_ignores_label_removals(monkeypatch):
    """_label_appliers must only count label ADD events (body=='1'), not
    removals (body==''), so a non-author removal cannot bypass the actor check."""
    mod = load_gate_check()

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/issues/200/timeline":
            return [
                {
                    "id": 1,
                    "type": "label",
                    "body": "1",  # ADD by author
                    "user": {"login": "agent-dev-a"},
                    "label": {"name": "refactor"},
                },
                {
                    "id": 2,
                    "type": "label",
                    "body": "",  # REMOVE by non-author — must be ignored
                    "user": {"login": "core-lead"},
                    "label": {"name": "refactor"},
                },
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_list", fake_api_list)
    appliers = mod._label_appliers(200, "molecule-ai/molecule-core")
    assert appliers == {"refactor": {"agent-dev-a"}}


def test_signal_7_refactor_exemption_rejected_when_timeline_unavailable(monkeypatch):
    """If the timeline API cannot prove a non-author applied the label,
    fail closed and do not honor the exemption."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    # Timeline API returned nothing / errored — no proof of non-author applier.
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={
            "user": {"login": "agent-dev-a"},
            "labels": [{"name": "refactor"}],
        },
    )
    assert result["verdict"] == "BLOCKED"
    assert result["refactor_exemption"] is False


def test_signal_7_files_api_error_returns_warning(monkeypatch):
    """A transient PR-files API error must surface as WARN, not BLOCK
    (transient failure shouldn't gate-block a real PR)."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "CLEAR",
        "diverged": False, "commits_behind": 0, "pr_files_count": 0,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.0,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {"error": "404 page not found"})
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    assert result["verdict"] == "WARNING"
    assert "could not fetch PR files API" in result["reason"]


def test_signal_7_net_deleted_uses_max_zero_underflow(monkeypatch):
    """A PR that ADDS more than it deletes (a large refactor rewrite) should
    have net_deleted_lines=0, NOT a negative number. The block must
    use net_deleted_lines (>= 5000), not raw deleted_lines minus added_lines."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    # deleted=100, added=200 → net should clamp to 0 (not -100)
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 5, "added_lines": 200, "deleted_lines": 100, "net_deleted_lines": 0,
    })
    result = mod.signal_7_destructive_diff_guard(200, "molecule-ai/molecule-core", pr_data={"labels": []})
    # files=5 (small) + net=0 (small) + diverged → moderate? files<50, net<1000, deleted<2000, commits_behind>10 → WARN
    assert result["net_deleted_lines"] == 0


def test_signal_7_refactor_exempt_with_still_high_diff_surfaces_numbers(monkeypatch):
    """When refactor-exemption applies, the WARN result should still
    surface the destructive numbers so an operator can review the size."""
    mod = load_gate_check()
    monkeypatch.setattr(mod, "signal_4_branch_divergence", lambda *a, **kw: {
        "signal": "branch_divergence", "verdict": "WARNING",
        "diverged": True, "commits_behind": 25, "pr_files_count": 250,
        "inherited_files": [], "new_work_files": [], "inherited_fraction": 0.5,
    })
    monkeypatch.setattr(mod, "_pr_diff_stats", lambda *a, **kw: {
        "files_changed": 481, "added_lines": 1000, "deleted_lines": 55800, "net_deleted_lines": 54800,
    })
    monkeypatch.setattr(mod, "_label_appliers", lambda *a, **kw: {"refactor": {"core-lead"}})
    result = mod.signal_7_destructive_diff_guard(
        200, "molecule-ai/molecule-core",
        pr_data={"labels": [{"name": "refactor"}]},
    )
    assert result["verdict"] == "WARNING"
    # WARN still surfaces the destructive numbers for human review
    assert result["files_changed"] == 481
    assert result["net_deleted_lines"] == 54800
    assert result["deleted_lines"] == 55800
    assert result["diverged"] is True
    assert result["refactor_exemption"] is True
