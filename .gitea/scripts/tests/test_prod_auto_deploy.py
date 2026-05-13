import importlib.util
import sys
from pathlib import Path


SCRIPT = Path(__file__).resolve().parents[1] / "prod-auto-deploy.py"
spec = importlib.util.spec_from_file_location("prod_auto_deploy", SCRIPT)
prod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = prod
spec.loader.exec_module(prod)


def test_truthy_flag_accepts_operator_disable_values():
    for value in ("1", "true", "TRUE", "yes", "on", "disabled", "disable"):
        assert prod.truthy_flag(value) is True

    for value in ("", "0", "false", "no", "off", None):
        assert prod.truthy_flag(value) is False


def test_build_plan_defaults_to_staging_sha_target_and_prod_cp():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_DISABLED": "",
        }
    )

    assert plan["enabled"] is True
    assert plan["sha"] == "abcdef1234567890"
    assert plan["target_tag"] == "staging-abcdef1"
    assert plan["cp_url"] == "https://api.moleculesai.app"
    assert plan["body"] == {
        "target_tag": "staging-abcdef1",
        "canary_slug": "hongming",
        "soak_seconds": 60,
        "batch_size": 3,
        "dry_run": False,
    }


def test_build_plan_rejects_non_prod_cp_without_explicit_override():
    try:
        prod.build_plan(
            {
                "GITHUB_SHA": "abcdef1234567890",
                "CP_URL": "https://staging-api.moleculesai.app",
            }
        )
    except ValueError as exc:
        assert "PROD_ALLOW_NON_PROD_CP_URL=true" in str(exc)
    else:
        raise AssertionError("expected non-prod CP URL rejection")


def test_build_plan_allows_non_prod_cp_only_with_override():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "CP_URL": "https://staging-api.moleculesai.app",
            "PROD_ALLOW_NON_PROD_CP_URL": "true",
        }
    )

    assert plan["cp_url"] == "https://staging-api.moleculesai.app"


def test_build_plan_disable_flag_short_circuits_before_credentials():
    plan = prod.build_plan(
        {
            "GITHUB_SHA": "abcdef1234567890",
            "PROD_AUTO_DEPLOY_DISABLED": "true",
        }
    )

    assert plan["enabled"] is False
    assert plan["disabled_reason"] == "PROD_AUTO_DEPLOY_DISABLED=true"


def test_latest_status_for_context_uses_first_matching_status():
    statuses = [
        {"context": "CI / all-required (push)", "status": "pending"},
        {"context": "CI / all-required (pull_request)", "status": "success"},
        {"context": "CI / all-required (push)", "status": "success"},
    ]

    latest = prod.latest_status_for_context(statuses, "CI / all-required (push)")

    assert latest == {"context": "CI / all-required (push)", "status": "pending"}


def test_ci_context_state_handles_missing_and_gitea_status_key():
    assert prod.ci_context_state([], "CI / all-required (push)") == "missing"
    assert (
        prod.ci_context_state(
            [{"context": "CI / all-required (push)", "status": "success"}],
            "CI / all-required (push)",
        )
        == "success"
    )
    assert (
        prod.ci_context_state(
            [{"context": "CI / all-required (push)", "state": "failure"}],
            "CI / all-required (push)",
        )
        == "failure"
    )


def test_context_is_satisfied_accepts_only_success():
    assert prod.context_is_satisfied("success") is True
    for state in ("failure", "error", "cancelled", "canceled", "skipped", "pending", "missing"):
        assert prod.context_is_satisfied(state) is False


def test_context_is_terminal_failure_rejects_cancelled_and_skipped():
    for state in ("failure", "error", "cancelled", "canceled", "skipped"):
        assert prod.context_is_terminal_failure(state) is True
    for state in ("pending", "missing", "success"):
        assert prod.context_is_terminal_failure(state) is False
