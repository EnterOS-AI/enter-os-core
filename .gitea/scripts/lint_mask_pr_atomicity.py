#!/usr/bin/env python3
"""lint_mask_pr_atomicity — Tier 2d structural enforcement per internal#350.

Rule
----
A PR whose diff touches `.gitea/workflows/ci.yml` AND modifies EITHER:

  - any `continue-on-error:` value, OR
  - the `all-required` sentinel job's `needs:` block

must EITHER:

  - Touch BOTH atomically in the same PR (preferred), OR
  - Cross-link the paired PR via a literal `Paired: #NNN` reference in
    the PR body OR in any commit message between BASE_SHA and HEAD_SHA.

The class this prevents
-----------------------
PR#665 (interim `continue-on-error: true` on `platform-build`) and
PR#668 (sentinel-`needs` demotion of the same job) were designed as a
pair but merged solo — #665 landed at 04:47Z 2026-05-12, #668 was still
open at 05:07Z when the main-red watchdog (#674) fired. Result: ~20
minutes of `main` red and a cascade of false-positives on unrelated PRs.

The lint operates on the YAML AST (PyYAML), not grep, per
`feedback_behavior_based_ast_gates`: a refactor that moves `continue-on-error`
between job keys, or renames the `all-required` job, would still be
detected because we walk the parsed structure.

Why this works on Gitea 1.22.6
------------------------------
We don't use any 1.22.6-missing endpoints (no `/actions/runs/*`, no
`branch_protections/*` — Tier 2f/g need those; Tier 2d does not). All
required inputs come from the workflow `pull_request` event payload
(BASE_SHA, HEAD_SHA, PR_BODY) and from local git via `git show`/`git log`.
The auto-injected `GITHUB_TOKEN` is enough; we don't need
DRIFT_BOT_TOKEN.

Exit codes
----------
  0 — ci.yml not in diff, OR diff is no-op for the rule predicates,
      OR atomicity satisfied (both touched), OR a valid `Paired: #NNN`
      reference is present.
  1 — exactly ONE of {coe, sentinel-needs} touched AND no valid
      `Paired: #NNN` reference. The split-pair regression class.
  2 — env contract violation (BASE_SHA / HEAD_SHA missing) or YAML
      parse error on either side.

Env
---
  BASE_SHA          — PR base (pull_request.base.sha)
  HEAD_SHA          — PR head (pull_request.head.sha)
  PR_BODY           — pull_request.body (may be empty)
  CI_WORKFLOW_PATH  — defaults to `.gitea/workflows/ci.yml`
  SENTINEL_JOB_KEY  — defaults to `all-required`

Memory cross-links
------------------
  - internal#350 (the RFC that specs this lint)
  - PR#665 / PR#668 (the empirical split-pair)
  - mc#664 (the main-red incident)
  - feedback_strict_root_only_after_class_a
  - feedback_behavior_based_ast_gates
"""
from __future__ import annotations

import os
import re
import subprocess
import sys
from typing import Any

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "::error::PyYAML is required. Install with: pip install PyYAML\n"
    )
    sys.exit(2)


# ---------------------------------------------------------------------------
# YAML quirk: bare `on:` at the top level becomes Python `True` because
# `on` is a YAML 1.1 boolean. Not used here but documented for future
# editors who copy from this module.
# ---------------------------------------------------------------------------


# `Paired: #NNN` reference. `#` is mandatory, NNN must be digits. Any
# surrounding markdown/whitespace is fine. The match is case-sensitive
# on `Paired:` because lower-case `paired:` collides with conversational
# prose ("paired: see comment above") and the convention is the exact
# capitalisation.
PAIRED_RE = re.compile(r"\bPaired:\s*#(?P<num>\d+)\b")


# ---------------------------------------------------------------------------
# Env contract
# ---------------------------------------------------------------------------
def _env(key: str, default: str | None = None) -> str:
    v = os.environ.get(key, default)
    return v if v is not None else ""


def _require_env(key: str) -> str:
    v = os.environ.get(key)
    if not v:
        sys.stderr.write(f"::error::missing required env var: {key}\n")
        sys.exit(2)
    return v


# ---------------------------------------------------------------------------
# git-show helper. Returns None when the path doesn't exist on that side
# (new file, deleted file, or rename — git returns exit 128 with "fatal:
# path not in tree"). We treat None as "no rule predicate triggered on
# that side".
# ---------------------------------------------------------------------------
def git_show(sha: str, path: str) -> str | None:
    r = subprocess.run(
        ["git", "show", f"{sha}:{path}"],
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        return None
    return r.stdout


def git_log_messages(base_sha: str, head_sha: str) -> str:
    r = subprocess.run(
        ["git", "log", "--format=%B", f"{base_sha}..{head_sha}"],
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        return ""
    return r.stdout


def git_diff_paths(base_sha: str, head_sha: str) -> list[str]:
    r = subprocess.run(
        ["git", "diff", "--name-only", f"{base_sha}..{head_sha}"],
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        return []
    return [p for p in r.stdout.splitlines() if p.strip()]


# ---------------------------------------------------------------------------
# Predicate 1 — any `continue-on-error` value changed between base and head
# ---------------------------------------------------------------------------
def _collect_coe(doc: Any) -> dict[str, Any]:
    """Walk every job in `jobs.*` and collect its continue-on-error value.

    Returns a dict {job_key: coe_value}. Missing keys are absent from
    the dict (NOT `False` — distinguishes "added the key" from
    "unchanged absent"). Job-step `continue-on-error` is NOT considered
    — only job-level, because that's the value that masks job status
    rollup, which is the class this lint targets.
    """
    out: dict[str, Any] = {}
    if not isinstance(doc, dict):
        return out
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return out
    for k, j in jobs.items():
        if not isinstance(j, dict):
            continue
        if "continue-on-error" in j:
            out[k] = j["continue-on-error"]
    return out


def coe_changed(base_doc: Any, head_doc: Any) -> tuple[bool, list[str]]:
    """Return (changed?, [reasons]) describing per-job coe diffs."""
    base = _collect_coe(base_doc)
    head = _collect_coe(head_doc)
    reasons: list[str] = []
    all_keys = set(base) | set(head)
    for k in sorted(all_keys):
        b = base.get(k, "<absent>")
        h = head.get(k, "<absent>")
        if b != h:
            reasons.append(f"job '{k}' continue-on-error: {b!r} → {h!r}")
    return (bool(reasons), reasons)


# ---------------------------------------------------------------------------
# Predicate 2 — sentinel job's `needs:` changed
# ---------------------------------------------------------------------------
def _collect_needs(doc: Any, sentinel_key: str) -> list[str] | None:
    """Return the sentinel job's needs list (sorted) or None if absent."""
    if not isinstance(doc, dict):
        return None
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return None
    j = jobs.get(sentinel_key)
    if not isinstance(j, dict):
        return None
    needs = j.get("needs")
    if needs is None:
        return []
    if isinstance(needs, str):
        return [needs]
    if isinstance(needs, list):
        # Sort because `needs:` is order-insensitive at the engine
        # level; a reorder is not a semantic change and shouldn't
        # trip the lint.
        return sorted(str(x) for x in needs)
    return None


def sentinel_needs_changed(
    base_doc: Any, head_doc: Any, sentinel_key: str
) -> tuple[bool, str]:
    """Return (changed?, reason)."""
    base = _collect_needs(base_doc, sentinel_key)
    head = _collect_needs(head_doc, sentinel_key)
    if base == head:
        return (False, "")
    return (
        True,
        f"sentinel '{sentinel_key}'.needs: {base!r} → {head!r}",
    )


# ---------------------------------------------------------------------------
# Predicate 3 — `Paired: #NNN` present in body or any commit message
# ---------------------------------------------------------------------------
def find_paired_refs(pr_body: str, commit_log: str) -> list[str]:
    """Return list of `#NNN` strings found (deduped, sorted)."""
    found: set[str] = set()
    for src in (pr_body, commit_log):
        for m in PAIRED_RE.finditer(src or ""):
            found.add(m.group("num"))
    return sorted(found)


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------
def _parse(content: str | None, label: str) -> Any:
    if content is None:
        return None
    try:
        return yaml.safe_load(content)
    except yaml.YAMLError as e:
        sys.stderr.write(f"::error::YAML parse error on {label}: {e}\n")
        sys.exit(2)


def run() -> int:
    base_sha = _require_env("BASE_SHA")
    head_sha = _require_env("HEAD_SHA")
    pr_body = _env("PR_BODY", "")
    ci_path = _env("CI_WORKFLOW_PATH", ".gitea/workflows/ci.yml")
    sentinel_key = _env("SENTINEL_JOB_KEY", "all-required")

    # Step 0 — is ci.yml even in the diff? If not, the lint doesn't apply.
    changed_paths = git_diff_paths(base_sha, head_sha)
    if ci_path not in changed_paths:
        print(
            f"::notice::{ci_path} not in PR diff; lint-mask-pr-atomicity "
            f"skipped (no atomicity risk)."
        )
        return 0

    base_yml = git_show(base_sha, ci_path)
    head_yml = git_show(head_sha, ci_path)

    base_doc = _parse(base_yml, f"{ci_path}@{base_sha}")
    head_doc = _parse(head_yml, f"{ci_path}@{head_sha}")

    # If the file is newly added (no base), no flip is possible — every
    # value is "newly introduced", not "changed". Tier 2e covers the
    # tracking-issue check for new continue-on-error: true. Exit 0.
    if base_doc is None:
        print(
            f"::notice::{ci_path} newly added in this PR; no flip to "
            f"analyse — lint-mask-pr-atomicity skipped."
        )
        return 0

    # If the file is deleted on head, ditto — no atomicity question.
    if head_doc is None:
        print(
            f"::notice::{ci_path} deleted in this PR; "
            f"lint-mask-pr-atomicity skipped."
        )
        return 0

    coe_yes, coe_reasons = coe_changed(base_doc, head_doc)
    needs_yes, needs_reason = sentinel_needs_changed(
        base_doc, head_doc, sentinel_key
    )

    if not coe_yes and not needs_yes:
        print(
            f"::notice::{ci_path} touched but neither continue-on-error "
            f"nor sentinel '{sentinel_key}'.needs changed — no atomicity "
            f"risk. OK."
        )
        return 0

    if coe_yes and needs_yes:
        print(
            f"::notice::Atomic change detected: both continue-on-error "
            f"AND sentinel '{sentinel_key}'.needs touched in same PR. OK."
        )
        for r in coe_reasons:
            print(f"  - {r}")
        print(f"  - {needs_reason}")
        return 0

    # Exactly one side touched — require Paired: #NNN reference.
    commit_log = git_log_messages(base_sha, head_sha)
    paired = find_paired_refs(pr_body, commit_log)

    one_side = "continue-on-error" if coe_yes else f"sentinel '{sentinel_key}'.needs"
    other_side = (
        f"sentinel '{sentinel_key}'.needs" if coe_yes else "continue-on-error"
    )

    if paired:
        print(
            f"::notice::Split-pair detected ({one_side} changed without "
            f"{other_side}), but Paired reference(s) present: "
            f"{', '.join('#' + n for n in paired)}. OK."
        )
        for r in coe_reasons:
            print(f"  - {r}")
        if needs_reason:
            print(f"  - {needs_reason}")
        return 0

    # The failure mode this lint exists to prevent.
    print(
        f"::error file={ci_path}::lint-mask-pr-atomicity (Tier 2d): "
        f"PR touches {one_side} in {ci_path} but NOT {other_side}, "
        f"and no `Paired: #NNN` reference was found in the PR body or "
        f"in commit messages between {base_sha[:8]}..{head_sha[:8]}. "
        f"This is the PR#665+#668 split-pair regression class "
        f"(see internal#350, mc#664). FIX: either (a) include the "
        f"matching {other_side} change in the same PR (preferred), or "
        f"(b) add `Paired: #NNN` (literal, capital P, with `#`) to the "
        f"PR body or a commit message referencing the paired PR."
    )
    for r in coe_reasons:
        print(f"  - {r}")
    if needs_reason:
        print(f"  - {needs_reason}")
    return 1


if __name__ == "__main__":
    sys.exit(run())
