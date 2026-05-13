#!/usr/bin/env python3
"""Production auto-deploy helpers for Gitea Actions.

The workflow keeps network side effects in shell/curl, but centralizes the
release decision shape here so it has unit coverage: disable flag parsing,
target tag selection, CP payload construction, and status-context selection.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import quote


TRUE_VALUES = {"1", "true", "yes", "on", "disabled", "disable"}
PROD_CP_URL = "https://api.moleculesai.app"
DEFAULT_REQUIRED_CONTEXTS = [
    "CI / Platform (Go) (push)",
    "CI / Canvas (Next.js) (push)",
    "CI / Shellcheck (E2E scripts) (push)",
    "CI / Python Lint & Test (push)",
    "CI / all-required (push)",
    "Secret scan / Scan diff for credential-shaped strings (push)",
]
TERMINAL_FAILURE_STATES = {"failure", "error", "cancelled", "canceled", "skipped"}


def truthy_flag(value: str | None) -> bool:
    if value is None:
        return False
    return value.strip().lower() in TRUE_VALUES


def _int_env(env: dict[str, str], name: str, default: int, minimum: int = 1) -> int:
    raw = env.get(name, "")
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise ValueError(f"{name} must be >= {minimum}, got {value}")
    return value


def build_plan(env: dict[str, str]) -> dict:
    sha = env.get("GITHUB_SHA", "").strip()
    if not sha:
        raise ValueError("GITHUB_SHA is required")

    disabled_value = env.get("PROD_AUTO_DEPLOY_DISABLED", "")
    if truthy_flag(disabled_value):
        return {
            "enabled": False,
            "sha": sha,
            "disabled_reason": f"PROD_AUTO_DEPLOY_DISABLED={disabled_value}",
        }

    short_sha = sha[:7]
    target_tag = env.get("PROD_AUTO_DEPLOY_TARGET_TAG", "").strip() or f"staging-{short_sha}"
    canary_slug = env.get("PROD_AUTO_DEPLOY_CANARY_SLUG", "hongming").strip()
    body = {
        "target_tag": target_tag,
        "soak_seconds": _int_env(env, "PROD_AUTO_DEPLOY_SOAK_SECONDS", 60, minimum=0),
        "batch_size": _int_env(env, "PROD_AUTO_DEPLOY_BATCH_SIZE", 3),
        "dry_run": truthy_flag(env.get("PROD_AUTO_DEPLOY_DRY_RUN", "")),
    }
    if canary_slug:
        body["canary_slug"] = canary_slug

    cp_url = env.get("CP_URL", "").strip() or PROD_CP_URL
    if cp_url != PROD_CP_URL and not truthy_flag(env.get("PROD_ALLOW_NON_PROD_CP_URL", "")):
        raise ValueError(
            f"Refusing production deploy to CP_URL={cp_url!r}; "
            f"set PROD_ALLOW_NON_PROD_CP_URL=true for an explicit non-prod drill"
        )

    return {
        "enabled": True,
        "sha": sha,
        "short_sha": short_sha,
        "target_tag": target_tag,
        "cp_url": cp_url,
        "body": body,
    }


def latest_status_for_context(statuses: list[dict], context: str) -> dict | None:
    """Return the first matching status.

    Gitea's combined-status response is newest-first in practice. The merge
    queue relies on the same contract; keeping the selector explicit makes
    stale duplicate contexts easy to test.
    """

    for status in statuses:
        if status.get("context") == context:
            return status
    return None


def ci_context_state(statuses: list[dict], context: str) -> str:
    status = latest_status_for_context(statuses, context)
    if not status:
        return "missing"
    return str(status.get("status") or status.get("state") or "missing").lower()


def context_is_satisfied(state: str) -> bool:
    return state == "success"


def context_is_terminal_failure(state: str) -> bool:
    return state in TERMINAL_FAILURE_STATES


def required_contexts(env: dict[str, str]) -> list[str]:
    raw = env.get("PROD_AUTO_DEPLOY_REQUIRED_CONTEXTS", "")
    if not raw.strip():
        return DEFAULT_REQUIRED_CONTEXTS
    return [line.strip() for line in raw.replace(",", "\n").splitlines() if line.strip()]


def _api_json(url: str, token: str) -> dict:
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:500]
        raise RuntimeError(f"GET {url} -> HTTP {exc.code}: {body}") from exc


def _api_json_optional(url: str, token: str) -> tuple[int, dict | None]:
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return exc.code, None
        body = exc.read().decode("utf-8", errors="replace")[:300]
        print(f"::warning::GET {url} -> HTTP {exc.code}: {body}", file=sys.stderr)
        return exc.code, None


def live_disable_flag(env: dict[str, str]) -> str:
    """Return a live disable value from Gitea variables when readable.

    Gitea evaluates `${{ vars.* }}` once when the job starts. This API read is
    the emergency re-check immediately before production side effects.
    """

    token = env.get("GITEA_TOKEN", "").strip()
    if not token:
        return ""
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    variable = quote("PROD_AUTO_DEPLOY_DISABLED", safe="")
    url = f"https://{host}/api/v1/repos/{repo}/actions/variables/{variable}"
    status, body = _api_json_optional(url, token)
    if status != 200 or not isinstance(body, dict):
        return ""
    return str(body.get("data") or body.get("value") or "")


def assert_not_disabled(env: dict[str, str]) -> None:
    plan = build_plan(env)
    if not plan.get("enabled"):
        raise RuntimeError(plan.get("disabled_reason", "production auto-deploy disabled"))
    live_value = live_disable_flag(env)
    if truthy_flag(live_value):
        raise RuntimeError(f"PROD_AUTO_DEPLOY_DISABLED={live_value} (live Gitea variable)")


def wait_for_ci_context(env: dict[str, str]) -> str:
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    sha = env.get("GITHUB_SHA", "").strip()
    token = env.get("GITEA_TOKEN", "").strip()
    contexts = required_contexts(env)
    interval = _int_env(env, "CI_STATUS_POLL_INTERVAL_SECONDS", 15)
    timeout = _int_env(env, "CI_STATUS_TIMEOUT_SECONDS", 1800)

    if not sha:
        raise ValueError("GITHUB_SHA is required")
    if not token:
        raise ValueError("GITEA_TOKEN is required to wait for CI status")

    url = f"https://{host}/api/v1/repos/{repo}/commits/{sha}/status"
    deadline = time.time() + timeout
    last_states: dict[str, str] = {}
    while time.time() <= deadline:
        body = _api_json(url, token)
        statuses = body.get("statuses") or []
        states = {context: ci_context_state(statuses, context) for context in contexts}
        for context, state in states.items():
            if state != last_states.get(context):
                print(f"CI context {context!r}: {state}", file=sys.stderr)
        last_states = states

        failures = [
            f"{context}={state}"
            for context, state in states.items()
            if context_is_terminal_failure(state)
        ]
        if failures:
            raise RuntimeError(
                "Required CI context failed; refusing production deploy: "
                + ", ".join(failures)
            )
        if all(context_is_satisfied(state) for state in states.values()):
            return "success"
        time.sleep(interval)
    last = ", ".join(f"{context}={state}" for context, state in last_states.items()) or "none"
    raise TimeoutError(f"Timed out waiting {timeout}s for required CI contexts; last_states={last}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("plan", help="print production deploy plan as JSON")
    sub.add_parser("assert-enabled", help="fail if production deploy is currently disabled")
    sub.add_parser("wait-ci", help="block until required CI context is green")
    args = parser.parse_args()

    try:
        if args.command == "plan":
            print(json.dumps(build_plan(dict(os.environ)), sort_keys=True))
            return 0
        if args.command == "assert-enabled":
            assert_not_disabled(dict(os.environ))
            return 0
        if args.command == "wait-ci":
            wait_for_ci_context(dict(os.environ))
            return 0
    except Exception as exc:  # noqa: BLE001 - CLI should render operator-friendly errors.
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
