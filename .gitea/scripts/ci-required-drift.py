#!/usr/bin/env python3
"""ci-required-drift — RFC internal#219 §4 + §6.

Detects drift between three sources of "what counts as a required check"
for this repo, files (or updates) a `[ci-drift]` Gitea issue when any
pair diverges.

Sources:
  A. `.gitea/workflows/ci.yml` jobs  (CI source — the actual job set)
  B. `status_check_contexts` in branch_protections (the merge gate)
  C. `REQUIRED_CHECKS_JSON` (preferred) or `REQUIRED_CHECKS` (legacy)
     env in audit-force-merge.yml (the audit env)

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
  F4  Context in (B) is emitted by NO workflow in .gitea/workflows/ at
      all (repo-wide, case-correct generalization of F2, which only
      covers `ci / `-prefixed names). This is the inverse-of-F2 hole and
      the one that makes the `CI / all-required` aggregator's
      name-vs-coverage gap safe: `all-required` is fail-closed over CI's
      OWN jobs but CANNOT cover sibling required workflows
      (`E2E API Smoke Test`, `Handlers Postgres Integration` — Gitea has
      no cross-workflow `needs:`). F4 verifies each cross-workflow
      required context still has a live emitter, so a renamed/deleted
      sibling workflow that BP still requires is caught instead of
      degrading to a silent absent-as-pending advisory gate.

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
    except urllib.error.URLError as e:
        # Network-level failures (DNS, connection refused, socket timeout) during
        # a Gitea brown-out should fail soft via ApiError so the hourly cron can
        # retry, instead of crashing with an unhandled stack trace.
        reason = str(e.reason) if hasattr(e, "reason") else str(e)
        raise ApiError(f"{method} {path} → network error: {reason}") from e

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
    whose `if:` gates on `github.event_name` or `github.ref` (those are
    event-scoped and can legitimately be `skipped` for a given trigger;
    if we required them under the sentinel `needs:`, every PR-only job
    would be `skipped` on push and the sentinel would interpret
    `skipped != success` as failure). RFC §4 spec.

    `github.ref` is the companion gate for jobs that run only on direct
    pushes to specific branches (e.g. `github.ref == 'refs/heads/main'`).
    These never execute in a PR context, so flagging them as missing
    from `all-required.needs:` is a false positive (mc#958 / mc#959).

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
            if isinstance(gate, str) and (
                "github.event_name" in gate or "github.ref" in gate
            ):
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


def required_checks_env(audit_doc: dict, branch: str) -> set[str]:
    """Pull the required-checks env value from audit-force-merge.yml.

    Walks the YAML AST per `feedback_behavior_based_ast_gates`: we do
    NOT grep for env keys — that breaks under reformatting,
    multi-job workflows, or a future move of the env to a different
    step. Instead, look inside every job's every step's `env:` map.

    Supports two variants:
      - REQUIRED_CHECKS_JSON (preferred): JSON dict keyed by branch name.
        We extract the array for the target branch.
      - REQUIRED_CHECKS (legacy): newline-separated list of context names.
    """
    found_json: list[str] = []
    found_legacy: list[str] = []
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
            if isinstance(step_env, dict):
                if "REQUIRED_CHECKS_JSON" in step_env:
                    v = step_env["REQUIRED_CHECKS_JSON"]
                    if isinstance(v, str):
                        found_json.append(v)
                if "REQUIRED_CHECKS" in step_env:
                    v = step_env["REQUIRED_CHECKS"]
                    if isinstance(v, str):
                        found_legacy.append(v)

    # JSON variant takes precedence.
    if found_json:
        if len(found_json) > 1:
            sys.stderr.write(
                f"::error::REQUIRED_CHECKS_JSON env present in {len(found_json)} steps; ambiguous\n"
            )
            sys.exit(3)
        try:
            parsed = json.loads(found_json[0])
        except json.JSONDecodeError as e:
            sys.stderr.write(
                f"::error::REQUIRED_CHECKS_JSON is not valid JSON: {e}\n"
            )
            sys.exit(3)
        if not isinstance(parsed, dict):
            sys.stderr.write(
                f"::error::REQUIRED_CHECKS_JSON parsed to {type(parsed).__name__}, expected dict\n"
            )
            sys.exit(3)
        branch_checks = parsed.get(branch)
        if branch_checks is None:
            sys.stderr.write(
                f"::error::REQUIRED_CHECKS_JSON has no entry for branch '{branch}'\n"
            )
            sys.exit(3)
        if not isinstance(branch_checks, list):
            sys.stderr.write(
                f"::error::REQUIRED_CHECKS_JSON['{branch}'] is {type(branch_checks).__name__}, expected list\n"
            )
            sys.exit(3)
        # Fail-closed validation: every entry must be a non-empty string.
        # Reject null, int, dict, or empty/whitespace strings silently —
        # they indicate a malformed manifest that drift-detect must not
        # normalize away (that would hide config errors).
        validated: set[str] = set()
        for idx, item in enumerate(branch_checks):
            if not isinstance(item, str):
                sys.stderr.write(
                    f"::error::REQUIRED_CHECKS_JSON['{branch}'][{idx}] is "
                    f"{type(item).__name__} (value={item!r}), expected str\n"
                )
                sys.exit(3)
            stripped = item.strip()
            if not stripped:
                sys.stderr.write(
                    f"::error::REQUIRED_CHECKS_JSON['{branch}'][{idx}] is "
                    f"empty/whitespace string\n"
                )
                sys.exit(3)
            if stripped in validated:
                sys.stderr.write(
                    f"::error::REQUIRED_CHECKS_JSON['{branch}'] contains "
                    f"duplicate context '{stripped}' at index {idx}\n"
                )
                sys.exit(3)
            validated.add(stripped)
        return validated

    # Legacy variant fallback.
    if found_legacy:
        if len(found_legacy) > 1:
            # Defensive: refuse to guess which one is canonical.
            sys.stderr.write(
                f"::error::REQUIRED_CHECKS env present in {len(found_legacy)} steps; ambiguous\n"
            )
            sys.exit(3)
        raw = found_legacy[0]
        # YAML block-scalars (`|`) leave a trailing newline + blanks; trim
        # consistently with audit-force-merge.sh's parser so both sides
        # produce identical sets.
        return {line.strip() for line in raw.splitlines() if line.strip()}

    sys.stderr.write(
        f"::error::Neither REQUIRED_CHECKS_JSON nor REQUIRED_CHECKS env found in any step of "
        f"{AUDIT_WORKFLOW_PATH}\n"
    )
    sys.exit(3)


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


def workflow_emitted_contexts(wf_doc: dict) -> set[str]:
    """The set of `pull_request` status-check contexts a SINGLE workflow
    emits, computed from its real `name:` + each job's `name or key`.

    Gitea reports a context as `{workflow.name} / {job.name|job.key}
    (pull_request)`. Unlike `expected_context()` (which hard-codes the
    lowercase literal `ci` and the bare job-KEY — a shape that does NOT
    match this repo, whose workflow is `name: CI` and whose CI jobs DO
    set per-job `name:`), this reads the authoritative names straight
    from the parsed YAML, so the contexts it produces are byte-equal to
    what BP records. Used by F4 (cross-workflow emitter existence).

    Jobs whose `if:` gates on `github.event_name`/`github.ref` are still
    emitters on the events they DO run — they remain in the set; F4 only
    asserts *existence of an emitter*, never that it ran on a given
    trigger."""
    name = wf_doc.get("name")
    if not isinstance(name, str) or not name:
        return set()
    jobs = wf_doc.get("jobs")
    if not isinstance(jobs, dict):
        return set()
    out: set[str] = set()
    for key, spec in jobs.items():
        job_name = key
        if isinstance(spec, dict) and isinstance(spec.get("name"), str) and spec["name"]:
            job_name = spec["name"]
        out.add(f"{name} / {job_name} (pull_request)")
    return out


def all_emitted_contexts(workflows_dir: str = ".gitea/workflows") -> set[str]:
    """Union of `pull_request` contexts emitted by EVERY workflow in the
    repo. F4 uses this to assert that each BP-required
    `status_check_contexts` entry corresponds to a real emitting
    workflow+job — closing the inverse-of-F2 hole where BP requires a
    context that NO workflow produces (e.g. a sibling workflow like
    `E2E API Smoke Test` or `Handlers Postgres Integration` was renamed
    or deleted while still required, leaving BP demanding a green it can
    never receive; Gitea treats absent-as-pending → silent advisory
    gate). This is what makes the misleadingly-named `CI / all-required`
    aggregator safe at the repo level: it only covers CI's own jobs, but
    F4 guarantees the cross-workflow required contexts it CANNOT cover
    are real and present."""
    import glob as _glob

    emitted: set[str] = set()
    for path in sorted(_glob.glob(os.path.join(workflows_dir, "*.yml"))):
        try:
            with open(path, encoding="utf-8") as f:
                doc = yaml.safe_load(f)
        except (OSError, yaml.YAMLError):
            # A single unparseable sibling workflow must not blind F4 to
            # the rest. Skip it loudly; lint-workflow-yaml gates parse
            # validity separately.
            sys.stderr.write(f"::warning::F4: could not parse {path}, skipping\n")
            continue
        if isinstance(doc, dict):
            emitted |= workflow_emitted_contexts(doc)
    return emitted


# --------------------------------------------------------------------------
# Drift detection
# --------------------------------------------------------------------------
def detect_drift(branch: str) -> tuple[list[str], dict]:
    """Returns (findings, debug). Empty findings == no drift.

    Raises:
        ApiError: propagated (fail-closed) on a transient Gitea outage
                  (5xx) AND on a 401/403 auth failure from the protection
                  endpoint. A 401/403 means DRIFT_BOT_TOKEN cannot read
                  branch protections at all — drift is UNVERIFIABLE, so
                  this HARD gate must fail loud rather than green
                  undetected drift (the regression class it exists to
                  catch). An authenticated 404 (branch genuinely has no
                  protection, e.g. staging pre-rollout) is the one
                  tolerated skip: it returns ([], debug) with a loud
                  ::warning:: and the workflow continues to the next
                  branch.
    """
    findings: list[str] = []

    ci_doc = load_yaml(CI_WORKFLOW_PATH)
    audit_doc = load_yaml(AUDIT_WORKFLOW_PATH)

    jobs = ci_job_names(ci_doc)
    jobs_all = ci_jobs_all(ci_doc)
    needs = sentinel_needs(ci_doc)
    env_set = required_checks_env(audit_doc, branch)

    # Protection
    # api() raises ApiError on non-2xx. Transient 5xx should fail loud.
    # 403/404 means the token lacks repo-admin scope (Gitea 1.22.6's
    # branch_protections endpoint requires it — see DRIFT_BOT_TOKEN
    # provisioning trail in ci-required-drift.yml). Treat as
    # "cannot determine drift for this branch" — skip without turning
    # the workflow red. Surface a clear diagnostic so the operator
    # knows what to fix.
    contexts: set[str] = set()
    protection_path = f"/repos/{OWNER}/{NAME}/branch_protections/{branch}"
    try:
        _, protection = api("GET", protection_path)
    except ApiError as e:
        # Isolate the HTTP status from the error message.
        http_status: int | None = None
        msg = str(e)
        # ApiError message format: "{method} {path} → HTTP {status}: {body}"
        import re as _re

        m = _re.search(r"HTTP (\d{3})", msg)
        if m:
            http_status = int(m.group(1))
        # FAIL-CLOSED contract (was fail-open: 403 AND 404 both returned
        # [] with no signal — fixed). This is a HARD gate (no
        # continue-on-error → false) running hourly on a PROTECTED context
        # (schedule/dispatch on main). We split auth-failure from
        # genuinely-absent:
        #   401/403 → AUTH FAILURE: the token cannot read branch
        #     protections at all, so drift CANNOT be determined for ANY
        #     branch. Greening the hourly cron here means jobs↔protection
        #     drift goes silently undetected — exactly the regression class
        #     this sentinel exists to catch. Raise so the workflow fails
        #     loud / fails closed.
        #   404 → authenticated absent resource: this specific branch has
        #     no protection (e.g. `staging` before its protection rollout).
        #     Genuinely nothing to diff against — skip THIS branch with a
        #     loud ::warning::, continue to the next.
        if http_status in (401, 403):
            sys.stderr.write(
                f"::error::GET {protection_path} returned HTTP "
                f"{http_status} — DRIFT_BOT_TOKEN cannot read branch "
                f"protections (needs repo-admin scope). AUTH FAILURE: "
                f"drift CANNOT be determined, so this HARD gate FAILS "
                f"CLOSED rather than greening undetected drift. Fix: grant "
                f"repo-admin to mc-drift-bot (org team `drift-bot`, "
                f"perm=admin) — fix the token, not the lint.\n"
            )
            raise
        if http_status == 404:
            sys.stderr.write(
                f"::warning::GET {protection_path} returned HTTP 404 — "
                f"branch '{branch}' has no protection configured "
                f"(authenticated absent resource). Skipping drift check for "
                f"{branch}; if it SHOULD be protected, configure it.\n"
            )
            debug = {
                "branch": branch,
                "ci_jobs": sorted(jobs),
                "sentinel_needs": sorted(needs),
                "protection_contexts_skipped": True,
                "protection_http_status": http_status,
                "audit_env_checks": sorted(env_set),
            }
            return [], debug
        # 5xx / other — propagate (transient outage, fail loud per design).
        raise
    if not isinstance(protection, dict):
        sys.stderr.write(
            f"::error::protection response for {branch} not a JSON object\n"
        )
        sys.exit(4)
    contexts = set(protection.get("status_check_contexts") or [])

    # ----- F1: job exists in CI but not under sentinel.needs -----
    # Post-#1766 contract: the sentinel may deliberately have no `needs:`
    # and instead poll path-relevant statuses dynamically. In that case
    # F1 is a false positive — skip it. F1b (typos in existing needs)
    # is naturally skipped when needs is empty.
    missing_from_needs = sorted(jobs - needs)
    if missing_from_needs and needs:
        findings.append(
            "F1 — jobs in ci.yml NOT under sentinel `needs:` "
            "(sentinel doesn't gate them):\n"
            + "\n".join(f"  - {n}" for n in missing_from_needs)
        )

    # ----- F1b: needs lists a job that doesn't exist (typo) -----
    # Compare against jobs_all (incl. event-gated jobs); a typo is a
    # typo regardless of `if:` gating.
    stale_needs = sorted(needs - jobs_all)
    if stale_needs:
        findings.append(
            "F1b — sentinel `needs:` lists jobs NOT present in ci.yml "
            "(typo or removed job):\n"
            + "\n".join(f"  - {n}" for n in stale_needs)
        )

    # ----- F2: protection context has no emitting job -----
    # Compute the contexts the CI YAML actually produces. The sentinel
    # is in (B) intentionally (`ci / all-required (pull_request)`); we
    # whitelist it explicitly.
    emitted_contexts = {
        expected_context(j) for j in jobs
    } | {expected_context(SENTINEL_JOB)}
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
            "F2 — protection `status_check_contexts` entries with `ci / ` "
            "prefix that NO job in ci.yml emits "
            "(stale name → silent advisory gate):\n"
            + "\n".join(f"  - {c}" for c in stale_protection)
        )

    # ----- F4: cross-workflow required context has no emitting workflow -----
    # F2 (above) is scoped to `ci / `-prefixed contexts ONLY, and built
    # from the hard-coded lowercase literal `ci` + bare job-keys — a shape
    # that does NOT match this repo (workflow is `name: CI`, jobs set their
    # own `name:`), so F2 is effectively dormant here. F4 is the
    # case-correct, REPO-WIDE generalization: it parses every workflow's
    # real `name:` + job `name|key` and asserts that EVERY BP-required
    # context is actually emitted by some workflow.
    #
    # This is the gate that makes the `CI / all-required` aggregator's
    # name-vs-coverage gap safe. `all-required` is fail-closed over CI's
    # OWN jobs but — by Gitea's design (no cross-workflow `needs:`) — it
    # CANNOT and does not cover sibling required workflows
    # (`E2E API Smoke Test`, `Handlers Postgres Integration`). Those MUST
    # be listed in BP independently. F4 verifies each such BP context
    # still has a live emitter, so the inverse-of-F2 hole — BP requires a
    # context that no workflow produces (rename/delete a sibling workflow
    # while still required → Gitea treats absent-as-pending → silent
    # advisory gate, and a red PR can look mergeable) — is caught.
    repo_emitted = all_emitted_contexts(os.path.dirname(CI_WORKFLOW_PATH))
    unemitted = sorted(c for c in contexts if c not in repo_emitted)
    if unemitted:
        findings.append(
            "F4 — branch_protections/{br}.status_check_contexts entries that "
            "NO workflow in .gitea/workflows/ emits "
            "(stale required name → silent advisory gate; a red PR can look "
            "mergeable):\n".format(br=branch)
            + "\n".join(f"  - {c}" for c in unemitted)
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
        "repo_emitted_contexts": sorted(repo_emitted),
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

    Paginates through all open issues (limit=50 per page) until the
    title is found or the result set is exhausted. Previously only one
    page was fetched, causing duplicate [ci-drift] issues when the
    existing tracking issue fell beyond page 1.
    """
    page = 1
    while True:
        _, results = api(
            "GET",
            f"/repos/{OWNER}/{NAME}/issues",
            query={
                "state": "open",
                "type": "issues",
                "limit": "50",
                "page": str(page),
            },
        )
        if not isinstance(results, list):
            raise ApiError(
                f"issue search returned non-list body (got {type(results).__name__})"
            )
        for issue in results:
            if issue.get("title") == title:
                return issue
        # Fewer than limit results means last page reached.
        if len(results) < 50:
            return None
        page += 1


def render_body(branch: str, findings: list[str], debug: dict) -> str:
    body = [
        f"# Drift detected on `{REPO}/{branch}`",
        "",
        "Auto-filed by `.gitea/workflows/ci-required-drift.yml` "
        "(RFC [internal#219]"
        "(https://git.moleculesai.app/molecule-ai/internal/issues/219) §4 + §6).",
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
            "- **F1 / F1b**: if the sentinel job has a `needs:` block, add "
            "the missing job to it in `.gitea/workflows/ci.yml`, or remove "
            "the stale entry. If the sentinel deliberately has no `needs:` "
            "(path-aware polling sentinel per post-#1766 contract), this "
            "finding is expected and F1 is skipped.",
            "- **F2**: rename the protection context to match an emitter, "
            "or remove it from `status_check_contexts` "
            "(PATCH `/api/v1/repos/{owner}/{repo}/branch_protections/{branch}`).",
            "- **F3a / F3b**: bring `REQUIRED_CHECKS_JSON` (or `REQUIRED_CHECKS` legacy) env in "
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
        print("::group::[dry-run] title")
        print(title)
        print("::endgroup::")
        print("::group::[dry-run] body")
        print(body)
        print("::endgroup::")
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
