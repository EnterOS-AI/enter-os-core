"""Regression test #765 — gate auto-fire on real qa/security APPROVED review.

Validates the structural configuration of qa-review.yml and security-review.yml
so that a real team-member APPROVED review fires the workflow and POSTs the
exact branch-protection-required context name. This is the test #2020's
stale-context failure would have caught.
"""

from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]


def load_workflow(name: str) -> dict:
    with (ROOT / "workflows" / name).open() as f:
        return yaml.safe_load(f)


def _job_guard_string(workflow: dict) -> str:
    """Return the raw job-level `if:` string for the single job."""
    jobs = workflow["jobs"]
    # Both qa-review and security-review have exactly one job named "approved".
    job = jobs["approved"]
    return str(job.get("if", ""))


def _post_step(workflow: dict) -> dict:
    """Return the explicit POST /statuses step from the job steps list."""
    jobs = workflow["jobs"]
    steps = jobs["approved"]["steps"]
    for step in steps:
        name = step.get("name", "")
        if "Post required status context" in name:
            return step
    raise AssertionError("No explicit POST status step found")


class TestQaReviewDirectTrigger:
    def test_trigger_is_pull_request_review_submitted(self):
        wf = load_workflow("qa-review.yml")
        # PyYAML parses bare 'on' as boolean True.
        on = wf[True]
        assert "pull_request_review" in on, (
            "qa-review must trigger on pull_request_review"
        )
        types = on["pull_request_review"].get("types", [])
        assert "submitted" in types, (
            "pull_request_review must include 'submitted' type"
        )

    def test_job_guard_has_no_review_state_check(self):
        wf = load_workflow("qa-review.yml")
        guard = _job_guard_string(wf)
        assert "github.event.review.state" not in guard, (
            "job guard must NOT check review.state (#2159: Gitea 1.22.6 payload unreliable); "
            "evaluator (review-check.sh) verifies actual APPROVE via API"
        )
        assert "github.event_name == 'pull_request_target'" in guard
        assert "github.event_name == 'pull_request_review'" in guard

    def test_post_step_uses_status_post_token(self):
        wf = load_workflow("qa-review.yml")
        post = _post_step(wf)
        env = post.get("env", {})
        assert env.get("GITEA_TOKEN") == "${{ secrets.STATUS_POST_TOKEN }}", (
            "POST step must use STATUS_POST_TOKEN for write-scoped status POST"
        )

    def test_post_step_context_name_exact(self):
        """The context POSTed must byte-match the branch-protection requirement."""
        wf = load_workflow("qa-review.yml")
        post = _post_step(wf)
        run = post.get("run", "")
        assert '"qa-review / approved (pull_request_target)"' in run, (
            "POST step must emit exact BP-required context name"
        )


class TestSecurityReviewDirectTrigger:
    def test_trigger_is_pull_request_review_submitted(self):
        wf = load_workflow("security-review.yml")
        # PyYAML parses bare 'on' as boolean True.
        on = wf[True]
        assert "pull_request_review" in on, (
            "security-review must trigger on pull_request_review"
        )
        types = on["pull_request_review"].get("types", [])
        assert "submitted" in types, (
            "pull_request_review must include 'submitted' type"
        )

    def test_job_guard_has_no_review_state_check(self):
        wf = load_workflow("security-review.yml")
        guard = _job_guard_string(wf)
        assert "github.event.review.state" not in guard, (
            "job guard must NOT check review.state (#2159: Gitea 1.22.6 payload unreliable); "
            "evaluator (review-check.sh) verifies actual APPROVE via API"
        )
        assert "github.event_name == 'pull_request_target'" in guard
        assert "github.event_name == 'pull_request_review'" in guard

    def test_post_step_uses_status_post_token(self):
        wf = load_workflow("security-review.yml")
        post = _post_step(wf)
        env = post.get("env", {})
        assert env.get("GITEA_TOKEN") == "${{ secrets.STATUS_POST_TOKEN }}", (
            "POST step must use STATUS_POST_TOKEN for write-scoped status POST"
        )

    def test_post_step_context_name_exact(self):
        """The context POSTed must byte-match the branch-protection requirement."""
        wf = load_workflow("security-review.yml")
        post = _post_step(wf)
        run = post.get("run", "")
        assert '"security-review / approved (pull_request_target)"' in run, (
            "POST step must emit exact BP-required context name"
        )


class TestRefireScriptContextName:
    """review-refire-status.sh must emit the BP-required (pull_request_target) context."""

    def test_refire_script_context_is_pull_request_target(self):
        script = ROOT / "scripts" / "review-refire-status.sh"
        content = script.read_text()
        assert 'CONTEXT="${TEAM}-review / approved (pull_request_target)"' in content, (
            "refire script CONTEXT must be the exact BP-required (pull_request_target) variant"
        )
        assert 'approved (pull_request)"' not in content, (
            "refire script must NOT post bare (pull_request) context"
        )


class TestRefireTokenSeparation:
    """The /qa-recheck + /security-recheck backstop must also use STATUS_POST_TOKEN."""

    def _refire_step(self, workflow_name: str, step_name_keyword: str) -> dict:
        wf = load_workflow(workflow_name)
        jobs = wf["jobs"]
        steps = jobs["review-refire"]["steps"]
        for step in steps:
            name = step.get("name", "")
            if step_name_keyword in name:
                return step
        raise AssertionError(f"No refire step matching {step_name_keyword!r}")

    def test_qa_refire_uses_status_post_token(self):
        step = self._refire_step("sop-checklist.yml", "Refire qa-review")
        env = step.get("env", {})
        assert env.get("STATUS_POST_TOKEN") == "${{ secrets.STATUS_POST_TOKEN }}", (
            "qa refire must receive STATUS_POST_TOKEN env var"
        )
        # Evaluator stays on read token (no GITHUB_TOKEN fallback; it lacks
        # read:org scope for team-membership checks).
        assert env.get("GITEA_TOKEN") == "${{ secrets.SOP_CHECKLIST_GATE_TOKEN }}", (
            "qa refire evaluator must use read-scoped SOP_CHECKLIST_GATE_TOKEN"
        )

    def test_security_refire_uses_status_post_token(self):
        step = self._refire_step("sop-checklist.yml", "Refire security-review")
        env = step.get("env", {})
        assert env.get("STATUS_POST_TOKEN") == "${{ secrets.STATUS_POST_TOKEN }}", (
            "security refire must receive STATUS_POST_TOKEN env var"
        )
        assert env.get("GITEA_TOKEN") == "${{ secrets.SOP_CHECKLIST_GATE_TOKEN }}", (
            "security refire evaluator must use read-scoped SOP_CHECKLIST_GATE_TOKEN"
        )
