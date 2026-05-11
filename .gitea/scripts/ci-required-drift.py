#!/usr/bin/env python3
"""ci-required-drift — RFC internal#219 §4 + §6.

Detects drift between three sources of "what counts as a required check"
for this repo, files (or updates) a `[ci-drift]` Gitea issue when any
pair diverges.

Sources:
  A. `.gitea/workflows/ci.yml` jobs  (CI source — the actual job set)
  B. `status_check_contexts` in branch_protections (the merge gate)
  C. `REQUIRED_CHECKS` env in audit-force-merge.yml (the audit env)

Three failure classes:
  F1  Job in (A) is not under the sentinel's `needs:` — sentinel
      doesn't gate it, so a red job on that name can sneak through.
      Ignores jobs whose `if:` references `github.event_name` (those
      run only on specific events and may be `skipped` legitimately).
  F2  Context in (B) corresponds to no emitter — i.e. there's no job
      in ci.yml whose runtime status-name maps to that context.
      A stale required-check name is silent: protection demands a
      green it never receives, but Gitea treats absent-as-pending,
      not absent-as-red. The gate degrades to advisory.
  F3  (B) and (C) are not set-equal. Audit env wider than protection
      → audit flags non-force-merges as force; narrower → real
      force-merges are missed.

Idempotency:
  Searches OPEN issues by exact title prefix
  `[ci-drift] {repo}/{branch}: ` and either edits the existing one
  (if any) or POSTs a new one. Never spawns duplicates.

Behavior-based AST gate per `feedback_behavior_based_ast_gates`:
  - Job set comes from PyYAML parse of jobs:* keys
  - Sentinel needs from PyYAML parse of jobs[sentinel].needs (a list)
  - Audit env from PyYAML parse, NOT grep — so reformatting the YAML
    (block-scalar `|` vs flow-style list) does not break the gate
"""
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

import yaml  # PyYAML 6.0.2 — installed by the workflow before this runs.


# --------------------------------------------------------------------------
# Environment
# --------------------------------------------------------------------------
def env(key: str, *, required: bool = True, default: str | None = None) -> str:
    val = os.environ.get(key, default)
    if required and not val:
        sys.stderr.write(f"::error::missing required env var: {key}\n")
        sys.exit(2)
    return val or ""


GITEA_TOKEN = env("GITEA_TOKEN", required=False)
GITEA_HOST = env("GITEA_HOST", required=False)
REPO = env("REPO", required=False)
BRANCHES = env("BRANCHES", required=False).split()
SENTINEL_JOB = env("SENTINEL_JOB", required=False)
AUDIT_WORKFLOW_PATH = env("AUDIT_WORKFLOW_PATH", required=False)
CI_WORKFLOW_PATH = env("CI_WORKFLOW_PATH", required=False)
DRIFT_LABEL = env("DRIFT_LABEL", required=False)

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""


def _require_runtime_env() -> None:
    """Enforce env contract — called from `main()` only. Tests import
    individual functions without setting the full env contract."""
    for key in (
        "GITEA_TOKEN",
        "GITEA_HOST",
        "REPO",
        "BRANCHES",
        "SENTINEL_JOB",
        "AUDIT_WORKFLOW_PATH",
        "CI_WORKFLOW_PATH",
        "DRIFT_LABEL",
    ):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)


# --------------------------------------------------------------------------
# Tiny HTTP helper (no requests dependency)
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    """Raised when a Gitea API call cannot be trusted to have succeeded.

    Covers non-2xx HTTP status AND 2xx with an unparseable JSON body on
    endpoints that are documented to return JSON (search/read). Callers
    that swallow this and proceed would risk e.g. creating duplicate
    `[ci-drift]` issues when a transient 500 hides an existing match.
    The cron retries hourly; one fail-loud cycle is fine — silent
    duplicate creation is not (per Five-Axis review on PR #112).
    """


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
    """Tiny HTTP helper around urllib.

    Raises ApiError on any non-2xx response. Callers that want
    best-effort semantics (e.g. label-apply) must `try/except ApiError`
    explicitly — making the failure-soft path opt-in rather than the
    default closes the duplicate-issue regression class.

    For 2xx responses with a JSON body that fails to parse, raises
    ApiError when `expect_json=True` (the default for read-shaped
    paths). On endpoints that legitimately return non-JSON success
    bodies (e.g. some Gitea create echoes — see
    `feedback_gitea_create_api_unparseable_response`), callers may pass
    `expect_json=False` to accept a `_raw` fallthrough — but they MUST
    then verify success via a follow-up GET, not by trusting the body.
    """
    url = f"{API}{path}"
    if query:
        url = f"{url}?{urllib.parse.urlencode(query)}"
    data = None
    headers = {
        "Authorization": f"token {GITEA_TOKEN}",
        "Accept": "application/json",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read()
        status = e.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(
            f"{method} {path} → HTTP {status}: {snippet}"
        )

    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as e:
        if expect_json:
            raise ApiError(
                f"{method} {path} → HTTP {status} but body is not JSON: {e}"
            ) from e
        # Opt-in raw fallthrough for endpoints with known echo-quirks.
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


# --------------------------------------------------------------------------
# YAML loaders — STRICT (reject GitHub-Actions-only syntax)
# --------------------------------------------------------------------------
def load_yaml(path: str) -> dict:
    """Load + parse a workflow YAML. Hard-fail if the file is missing
    or doesn't parse — drift-detect cannot make decisions without
    knowing the actual job set."""
    if not os.path.exists(path):
        sys.stderr.write(f"::error::file not found: {path}\n")
        sys.exit(3)
    with open(path, encoding="utf-8") as f:
        try:
            doc = yaml.safe_load(f)
        except yaml.YAMLError as e:
            sys.stderr.write(f"::error::YAML parse error in {path}: {e}\n")
            sys.exit(3)
    if not isinstance(doc, dict):
        sys.stderr.write(f"::error::{path} is not a YAML mapping\n")
        sys.exit(3)
    return doc


def ci_jobs_all(ci_doc: dict) -> set[str]:
    """Every job key in ci.yml minus the sentinel itself. Used for F1b
    (sentinel.needs typo check) — needs that name a non-existent job
    is a typo regardless of event-gating."""
    jobs = ci_doc.get("jobs")
    if not isinstance(jobs, dict):
        sys.stderr.write("::error::ci.yml has no jobs: mapping\n")
        sys.exit(3)
    return {k for k in jobs if k != SENTINEL_JOB}


def ci_job_names(ci_doc: dict) -> set[str]:
    """Set of job keys in ci.yml MINUS the sentinel itself MINUS jobs
    whose `if:` gates on `github.event_name` (those are event-scoped
    and can legitimately be `skipped` for a given trigger; if we
    required them under the sentinel `needs:`, every PR-only job
    would be `skipped` on push and the sentinel would interpret
    `skipped != success` as failure). RFC §4 spec.

    Used for F1 (jobs missing from sentinel needs). NOT used for F1b
    (typos in needs) — see `ci_jobs_all` for that."""
    jobs = ci_doc.get("jobs")
    if not isinstance(jobs, dict):
        sys.stderr.write("::error::ci.yml has no jobs: mapping\n")
        sys.exit(3)
    names: set[str] = set()
    for k, v in jobs.items():
        if k == SENTINEL_JOB:
            continue
        if isinstance(v, dict):
            gate = v.get("if")
            if isinstance(gate, str) and "github.event_name" in gate:
                continue
        names.add(k)
    return names


def sentinel_needs(ci_doc: dict) -> set[str]:
    sentinel = ci_doc.get("jobs", {}).get(SENTINEL_JOB)
    if not isinstance(sentinel, dict):
        sys.stderr.write(
            f"::error::sentinel job '{SENTINEL_JOB}' not found in {CI_WORKFLOW_PATH}\n"
        )
        sys.exit(3)
    needs = sentinel.get("needs", [])
    if isinstance(needs, str):
        needs = [needs]
    if not isinstance(needs, list):
        sys.stderr.write("::error::sentinel `needs:` is neither list nor string\n")
        sys.exit(3)
    return set(needs)


def required_checks_env(audit_doc: dict) -> set[str]:
    """Pull the REQUIRED_CHECKS env value from audit-force-merge.yml.
    Walks the YAML AST per `feedback_behavior_based_ast_gates`: we do
    NOT grep for `REQUIRED_CHECKS:` — that breaks under reformatting,
    multi-job workflows, or a future move of the env to a different
    step. Instead, look inside every job's every step's `env:` map."""
    found: list[str] = []
    jobs = audit_doc.get("jobs", {})
    if not isinstance(jobs, dict):
        sys.stderr.write(f"::warning::{AUDIT_WORKFLOW_PATH} has no jobs: mapping\n")
        return set()
    for job in jobs.values():
        if not isinstance(job, dict):
            continue
        for step in job.get("steps", []) or []:
            if not isinstance(step, dict):
                continue
            step_env = step.get("env") or {}
            if isinstance(step_env, dict) and "REQUIRED_CHECKS" in step_env:
                v = step_env["REQUIRED_CHECKS"]
                if isinstance(v, str):
                    found.append(v)
    if not found:
        sys.stderr.write(
            f"::error::REQUIRED_CHECKS env not found in any step of {AUDIT_WORKFLOW_PATH}\n"
        )
        sys.exit(3)
    if len(found) > 1:
        # Defensive: refuse to guess which one is canonical.
        sys.stderr.write(
            f"::error::REQUIRED_CHECKS env present in {len(found)} steps; ambiguous\n"
        )
        sys.exit(3)
    raw = found[0]
    # YAML block-scalars (`|`) leave a trailing newline + blanks; trim
    # consistently with audit-force-merge.sh's parser so both sides
    # produce identical sets.
    return {line.strip() for line in raw.splitlines() if line.strip()}


# --------------------------------------------------------------------------
# Mapping: ci.yml job-key  →  protection context name
# --------------------------------------------------------------------------
def expected_context(job_key: str, workflow_name: str = "ci") -> str:
    """Gitea Actions reports status-check contexts as
       "{workflow.name} / {job.name or job.key} ({event})".

    For ci.yml the event is `pull_request` on PRs (that's what
    `status_check_contexts` records). Job.name defaults to job.key
    when no `name:` is set. CP's ci.yml does NOT set per-job `name:`
    so the key equals the human-name."""
    return f"{workflow_name} / {job_key} (pull_request)"


# --------------------------------------------------------------------------
# Drift detection
# --------------------------------------------------------------------------
def detect_drift(branch: str) -> tuple[list[str], dict]:
    """Returns (findings, debug). Empty findings == no drift."""
    findings: list[str] = []

    ci_doc = load_yaml(CI_WORKFLOW_PATH)
    audit_doc = load_yaml(AUDIT_WORKFLOW_PATH)

    jobs = ci_job_names(ci_doc)
    jobs_all = ci_jobs_all(ci_doc)
    needs = sentinel_needs(ci_doc)
    env_set = required_checks_env(audit_doc)

    # Protection
    # api() raises ApiError on non-2xx; let it propagate so a transient
    # 500 fails the run loudly rather than producing a "no drift" lie.
    _, protection = api("GET", f"/repos/{OWNER}/{NAME}/branch_protections/{branch}")
    if not isinstance(protection, dict):
        sys.stderr.write(
            f"::error::protection response for {branch} not a JSON object\n"
        )
        sys.exit(4)
    contexts = set(protection.get("status_check_contexts") or [])

    # ----- F1: job exists in CI but not under sentinel.needs -----
    missing_from_needs = sorted(jobs - needs)
    if missing_from_needs:
        findings.append(
            "F1 — jobs in ci.yml NOT under sentinel `needs:` (sentinel doesn't gate them):\n"
            + "\n".join(f"  - {n}" for n in missing_from_needs)
        )

    # ----- F1b: needs lists a job that doesn't exist (typo) -----
    # Compare against jobs_all (incl. event-gated jobs); a typo is a
    # typo regardless of `if:` gating.
    stale_needs = sorted(needs - jobs_all)
    if stale_needs:
        findings.append(
            "F1b — sentinel `needs:` lists jobs NOT present in ci.yml (typo or removed job):\n"
            + "\n".join(f"  - {n}" for n in stale_needs)
        )

    # ----- F2: protection context has no emitting job -----
    # Compute the contexts the CI YAML actually produces. The sentinel
    # is in (B) intentionally (`ci / all-required (pull_request)`); we
    # whitelist it explicitly.
    emitted_contexts = {expected_context(j) for j in jobs} | {expected_context(SENTINEL_JOB)}
    # Contexts NOT produced by ci.yml may still come from other
    # workflows in the repo (Secret scan etc). We can't enumerate
    # every workflow's emissions cheaply; instead, flag only contexts
    # whose prefix is `ci / ` (this workflow's emissions) and which
    # don't appear in `emitted_contexts`. This narrows F2 to the
    # failure class the RFC actually targets without producing noise
    # from cross-workflow emitters.
    stale_protection = sorted(
        c for c in contexts if c.startswith("ci / ") and c not in emitted_contexts
    )
    if stale_protection:
        findings.append(
            "F2 — protection `status_check_contexts` entries with `ci / ` prefix that NO "
            "job in ci.yml emits (stale name → silent advisory gate):\n"
            + "\n".join(f"  - {c}" for c in stale_protection)
        )

    # ----- F3: audit env vs protection contexts (set-equal) -----
    only_in_env = sorted(env_set - contexts)
    only_in_protection = sorted(contexts - env_set)
    if only_in_env:
        findings.append(
            "F3a — audit-force-merge.yml `REQUIRED_CHECKS` env has contexts NOT in "
            f"branch_protections/{branch}.status_check_contexts (audit would flag "
            "non-force-merges as force):\n"
            + "\n".join(f"  - {c}" for c in only_in_env)
        )
    if only_in_protection:
        findings.append(
            "F3b — branch_protections/{br}.status_check_contexts has contexts NOT in "
            "audit-force-merge.yml `REQUIRED_CHECKS` env (real force-merges would be "
            "missed):\n".format(br=branch)
            + "\n".join(f"  - {c}" for c in only_in_protection)
        )

    debug = {
        "branch": branch,
        "ci_jobs": sorted(jobs),
        "sentinel_needs": sorted(needs),
        "protection_contexts": sorted(contexts),
        "audit_env_checks": sorted(env_set),
        "expected_contexts": sorted(emitted_contexts),
    }
    return findings, debug


# --------------------------------------------------------------------------
# Issue file/update
# --------------------------------------------------------------------------
def title_for(branch: str) -> str:
    # Idempotency key — keep stable, never include timestamp/SHA.
    return f"[ci-drift] {REPO}/{branch}: required-checks divergence detected"


def find_open_issue(title: str) -> dict | None:
    """Return the existing open `[ci-drift]` issue for `title`, or None.

    `None` means "search succeeded, no match" — NOT "search failed".
    Per Five-Axis review on PR #112: returning None on a transient API
    error caused the caller to POST a duplicate issue. Now api() raises
    ApiError on any non-2xx; we let it propagate. The cron retries
    hourly; failing one cycle loudly is strictly better than silently
    duplicating.

    Gitea issue search returns at most page=50 per page; one page is
    enough as long as `[ci-drift]` issues are a tiny minority. (See
    follow-up issue for Link-header pagination.)
    """
    _, results = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/issues",
        query={"state": "open", "type": "issues", "limit": "50"},
    )
    if not isinstance(results, list):
        raise ApiError(
            f"issue search returned non-list body (got {type(results).__name__})"
        )
    for issue in results:
        if issue.get("title") == title:
            return issue
    return None


def render_body(branch: str, findings: list[str], debug: dict) -> str:
    body = [
        f"# Drift detected on `{REPO}/{branch}`",
        "",
        "Auto-filed by `.gitea/workflows/ci-required-drift.yml` "
        "(RFC [internal#219](https://git.moleculesai.app/molecule-ai/internal/issues/219) §4 + §6).",
        "",
        "## Findings",
        "",
    ]
    body.extend(findings)
    body.extend(
        [
            "",
            "## Resolution",
            "",
            "- **F1 / F1b**: add the missing job to `all-required.needs:` "
            "in `.gitea/workflows/ci.yml`, or remove the stale entry.",
            "- **F2**: rename the protection context to match an emitter, "
            "or remove it from `status_check_contexts` "
            "(PATCH `/api/v1/repos/{owner}/{repo}/branch_protections/{branch}`).",
            "- **F3a / F3b**: bring `REQUIRED_CHECKS` env in "
            "`.gitea/workflows/audit-force-merge.yml` into set-equality with "
            "`status_check_contexts` (single PR, both files).",
            "",
            "## Debug",
            "",
            "```json",
            json.dumps(debug, indent=2, sort_keys=True),
            "```",
            "",
            "_This issue is idempotent: drift-detect runs hourly at `:17` "
            "and edits this body in place. Close the issue once the drift "
            "is fixed; the next hourly run will reopen if drift returns._",
        ]
    )
    return "\n".join(body)


def file_or_update(
    branch: str,
    findings: list[str],
    debug: dict,
    *,
    dry_run: bool = False,
) -> None:
    """File a new `[ci-drift]` issue, or PATCH the existing one in place.

    `dry_run=True` skips every side-effecting Gitea call (issue
    search, POST, PATCH, label apply) and prints the would-be issue
    title + body to stdout. Useful for local testing and for
    debugging drift output without polluting the issue tracker.
    """
    title = title_for(branch)
    body = render_body(branch, findings, debug)

    if dry_run:
        print(f"::notice::[dry-run] would file/update drift issue for {branch}")
        print(f"::group::[dry-run] title")
        print(title)
        print(f"::endgroup::")
        print(f"::group::[dry-run] body")
        print(body)
        print(f"::endgroup::")
        return

    existing = find_open_issue(title)
    if existing:
        num = existing["number"]
        api(
            "PATCH",
            f"/repos/{OWNER}/{NAME}/issues/{num}",
            body={"body": body},
        )
        print(f"::notice::Updated existing drift issue #{num} for {branch}")
        return

    _, created = api(
        "POST",
        f"/repos/{OWNER}/{NAME}/issues",
        body={"title": title, "body": body, "labels": []},
    )
    if not isinstance(created, dict):
        sys.stderr.write("::error::POST issue response not a JSON object\n")
        sys.exit(5)
    new_num = created.get("number")
    print(f"::warning::Filed new drift issue #{new_num} for {branch}")

    # Apply label by name (Gitea's add-labels endpoint accepts label IDs;
    # look up id by name once). Best-effort: failure to label is logged
    # but does not fail the audit run — the issue itself IS the alarm.
    try:
        _, labels = api("GET", f"/repos/{OWNER}/{NAME}/labels")
    except ApiError as e:
        sys.stderr.write(f"::warning::could not list labels: {e}\n")
        return
    label_id = None
    if isinstance(labels, list):
        for lbl in labels:
            if lbl.get("name") == DRIFT_LABEL:
                label_id = lbl.get("id")
                break
    if label_id is not None and new_num:
        try:
            api(
                "POST",
                f"/repos/{OWNER}/{NAME}/issues/{new_num}/labels",
                body={"labels": [label_id]},
            )
        except ApiError as e:
            sys.stderr.write(
                f"::warning::could not apply label '{DRIFT_LABEL}' to #{new_num}: {e}\n"
            )
    else:
        sys.stderr.write(f"::warning::label '{DRIFT_LABEL}' not found on repo\n")


# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="ci-required-drift",
        description="Detect drift between ci.yml, branch_protections, "
        "and audit-force-merge.yml REQUIRED_CHECKS env.",
    )
    p.add_argument(
        "--dry-run",
        action="store_true",
        help="Detect + print findings to stdout; do NOT file or PATCH "
        "the `[ci-drift]` issue. Useful for local testing and for "
        "previewing output before turning the workflow loose.",
    )
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(argv)
    _require_runtime_env()

    for branch in BRANCHES:
        findings, debug = detect_drift(branch)
        if findings:
            print(f"::warning::Drift detected on {branch}:")
            for f in findings:
                print(f)
            file_or_update(branch, findings, debug, dry_run=args.dry_run)
        else:
            print(f"::notice::No drift on {branch}.")
            print(json.dumps(debug, indent=2, sort_keys=True))
    # Exit 0 even on drift — the issue IS the alarm, not a red workflow.
    # A red workflow here would page on a CI rename until the issue is
    # opened, doubling the noise. The issue itself is the actionable
    # surface. (`api()` raising ApiError is the only path that exits
    # non-zero, by design: a transient Gitea outage should fail loudly.)
    return 0


if __name__ == "__main__":
    sys.exit(main())
