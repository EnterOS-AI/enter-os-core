#!/usr/bin/env python3
"""lint_required_context_exists_in_bp — Tier 2g per internal#350.

Rule
----
When a PR adds a NEW commit-status emission (a context that didn't
exist on the base side), the workflow file must carry one of three
directive comments adjacent to the new job:

  (a) `# bp-required: yes`
      The new context MUST already be in
      `branch_protections/<branch>.status_check_contexts`. Verified
      via Gitea API at PR time.

  (b) `# bp-required: pending #NNN`
      Acknowledged asymmetry; references an OPEN tracking issue that
      will follow up with the BP PATCH.

  (c) `# bp-exempt: <free-text reason>`
      Informational job, not intended to be a required gate.

No directive on a new emitter → FAIL with a 3-option fix-hint.

The class this prevents
-----------------------
PR#656 added `CI / all-required (pull_request)` as a sentinel context
that workflows emit, but BP did NOT list it. When `platform-build`
failed, `all-required` failed, but BP let the PR merge anyway →
cascade to mc#664. With this lint, PR#656 would have been blocked
until either the BP PATCH ran alongside OR the author added a
`bp-required: pending` directive.

Why directives MUST live in the workflow YAML
---------------------------------------------
The directive comment lives with the emitter so a scheduled
audit (Tier 2f, daily) can read the same source. PR-body-only
directives invisibly evaporate on merge — the asymmetry would
return to undetected. PR-body claims are advisory; workflow-file
comments are the contract.

How "new emission" is detected
------------------------------
Diff base..head over `.gitea/workflows/*.yml`. For each YAML file
that's added or modified:
  - Parse both base-side and head-side via PyYAML AST.
  - Enumerate emitted contexts on each side using the same rules as
    Tier 2f (workflow.name + job.name|key + event-mapping).
  - `new_contexts = head_contexts - base_contexts`.

If `new_contexts` is empty after de-dup, no rule applies → pass.

Per `feedback_behavior_based_ast_gates`: comment scanning uses raw
text in a small window around the job-key line, NOT regex over the
full file. This avoids matching `bp-required:` mentioned in a
comment unrelated to the new job.

Exit codes
----------
  0 — no new emissions, all new emissions have valid directives,
      or BP read errored (graceful-degrade per Tier 2a contract).
  1 — at least one new emission lacks a directive, or has
      `bp-required: yes` but the context is missing from BP.
  2 — env contract violation or YAML parse error.

Env
---
  BASE_SHA          — PR base SHA
  HEAD_SHA          — PR head SHA
  GITEA_TOKEN       — DRIFT_BOT_TOKEN (repo-admin for BP read)
  GITEA_HOST        — e.g. git.moleculesai.app
  REPO              — owner/name
  BRANCH            — defaults to `main`
  WORKFLOWS_DIR     — defaults to `.gitea/workflows`

Memory cross-links
------------------
  - internal#350 (the RFC that specs this lint)
  - PR#656 (the empirical case that prompted Tier 2g)
  - mc#664 (the surfaced cascade)
  - feedback_phantom_required_check_after_gitea_migration (Tier 2f cousin)
  - feedback_behavior_based_ast_gates
"""
from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "::error::PyYAML is required. Install with: pip install PyYAML\n"
    )
    sys.exit(2)


# Directive comment patterns. We match `# bp-required:` OR `# bp-exempt:`,
# both with optional surrounding whitespace and case-sensitive on the
# `bp-` prefix (convention).
BP_REQUIRED_YES_RE = re.compile(
    r"#\s*bp-required:\s*yes\b", re.IGNORECASE
)
BP_REQUIRED_PENDING_RE = re.compile(
    r"#\s*bp-required:\s*pending\s*#(?P<num>\d+)\b", re.IGNORECASE
)
BP_EXEMPT_RE = re.compile(
    r"#\s*bp-exempt:\s*\S", re.IGNORECASE
)


# Gitea event-mapping (same as Tier 2f).
_EVENT_MAP = {
    "pull_request": "pull_request",
    "pull_request_target": "pull_request",
    "push": "push",
}


# ---------------------------------------------------------------------------
# Env
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
# API helper (same contract as Tier 2f).
# ---------------------------------------------------------------------------
def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
) -> tuple[str, Any]:
    host = _env("GITEA_HOST")
    token = _env("GITEA_TOKEN")
    url = f"https://{host}/api/v1{path}"
    if query:
        url = f"{url}?{urllib.parse.urlencode(query)}"
    data = None
    headers = {
        "Authorization": f"token {token}",
        "Accept": "application/json",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
            if not raw:
                return ("ok", None)
            return ("ok", json.loads(raw))
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return ("not_found", None)
        if e.code in (401, 403):
            return ("forbidden", None)
        return ("error", None)
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        return ("error", None)


# ---------------------------------------------------------------------------
# git helpers
# ---------------------------------------------------------------------------
def git_show(sha: str, path: str) -> str | None:
    r = subprocess.run(
        ["git", "show", f"{sha}:{path}"], capture_output=True, text=True
    )
    if r.returncode != 0:
        return None
    return r.stdout


def git_diff_paths(base: str, head: str) -> list[str]:
    r = subprocess.run(
        ["git", "diff", "--name-only", f"{base}..{head}"],
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        return []
    return [p for p in r.stdout.splitlines() if p.strip()]


# ---------------------------------------------------------------------------
# Workflow context enumeration (mirror Tier 2f).
# ---------------------------------------------------------------------------
def _get_on(d: Any) -> Any:
    if not isinstance(d, dict):
        return None
    if "on" in d:
        return d["on"]
    if True in d:
        return d[True]
    return None


def _on_events(doc: Any) -> set[str]:
    on = _get_on(doc)
    raw: set[str] = set()
    if on is None:
        return raw
    if isinstance(on, str):
        raw.add(on)
    elif isinstance(on, list):
        for e in on:
            if isinstance(e, str):
                raw.add(e)
    elif isinstance(on, dict):
        for k in on:
            if isinstance(k, str):
                raw.add(k)
    return {_EVENT_MAP[e] for e in raw if e in _EVENT_MAP}


def _job_display(jbody: dict, jkey: str) -> str:
    n = jbody.get("name") if isinstance(jbody, dict) else None
    if isinstance(n, str) and n:
        return n
    return jkey


def workflow_contexts(doc: Any) -> set[str]:
    if not isinstance(doc, dict):
        return set()
    wf_name = doc.get("name")
    if not isinstance(wf_name, str) or not wf_name:
        return set()
    events = _on_events(doc)
    if not events:
        return set()
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return set()
    out: set[str] = set()
    for jkey, jbody in jobs.items():
        if jkey == "__lines__":
            continue
        if not isinstance(jbody, dict):
            continue
        disp = _job_display(jbody, jkey)
        for ev in events:
            out.add(f"{wf_name} / {disp} ({ev})")
    return out


# ---------------------------------------------------------------------------
# Find the source line of a job-key in a workflow YAML's raw text.
# Used to scan for nearby directive comments.
# ---------------------------------------------------------------------------
def _find_job_key_line(raw_lines: list[str], jkey: str) -> int | None:
    """Return 1-based line of `<jkey>:` under jobs:."""
    in_jobs = False
    jobs_indent = -1
    for i, line in enumerate(raw_lines, start=1):
        stripped = line.lstrip()
        if stripped.startswith("jobs:"):
            in_jobs = True
            jobs_indent = len(line) - len(stripped)
            continue
        if in_jobs:
            # Job key is the next indent level under `jobs:`.
            indent = len(line) - len(stripped)
            if stripped and indent <= jobs_indent:
                # Left the jobs: block
                in_jobs = False
                continue
            if re.match(rf"^\s*{re.escape(jkey)}\s*:", line):
                return i
    return None


_DIRECTIVE_WINDOW = 3  # lines above the job-key line (inclusive)


def find_directive_for_job(
    raw_text: str, jkey: str
) -> tuple[str, str | None] | None:
    """Return (kind, value) tuple for the first directive in a small
    window above the job-key line.

    kind ∈ {"required-yes", "required-pending", "exempt"}.
    value is the pending-issue number for required-pending, else None.
    Returns None if no directive found.

    We scan ABOVE the line only (the convention is the directive
    precedes the job — matches how `# mc#NNN` comments are placed
    above `continue-on-error: true`). We don't scan inside the job
    body because steps can produce false positives.
    """
    lines = raw_text.splitlines()
    line_no = _find_job_key_line(lines, jkey)
    if line_no is None:
        return None
    lo = max(1, line_no - _DIRECTIVE_WINDOW)
    for i in range(lo, line_no):
        line = lines[i - 1]
        m = BP_REQUIRED_PENDING_RE.search(line)
        if m:
            return ("required-pending", m.group("num"))
        if BP_REQUIRED_YES_RE.search(line):
            return ("required-yes", None)
        if BP_EXEMPT_RE.search(line):
            return ("exempt", None)
    return None


# ---------------------------------------------------------------------------
# Map a context back to its emitting (workflow_path, job_key) pair so
# we know WHERE to look for the directive comment.
# ---------------------------------------------------------------------------
def _resolve_emitter(
    ctx: str, head_workflows: dict[str, tuple[str, Any]]
) -> tuple[str, str] | None:
    """Return (file_path, job_key) emitting ctx, or None."""
    m = re.match(r"^(?P<wf>.+?) / (?P<job>.+) \((?P<event>[^)]+)\)$", ctx)
    if not m:
        return None
    target_wf = m.group("wf")
    target_job_disp = m.group("job")
    for path, (_raw, doc) in head_workflows.items():
        if not isinstance(doc, dict):
            continue
        if doc.get("name") != target_wf:
            continue
        jobs = doc.get("jobs") or {}
        if not isinstance(jobs, dict):
            continue
        for jkey, jbody in jobs.items():
            if jkey == "__lines__":
                continue
            if not isinstance(jbody, dict):
                continue
            disp = _job_display(jbody, jkey)
            if disp == target_job_disp:
                return (path, jkey)
    return None


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------
def run() -> int:
    base_sha = _require_env("BASE_SHA")
    head_sha = _require_env("HEAD_SHA")
    _require_env("GITEA_TOKEN")
    _require_env("GITEA_HOST")
    repo = _require_env("REPO")
    branch = _env("BRANCH", "main")
    wf_dir = _env("WORKFLOWS_DIR", ".gitea/workflows")

    # Step 1 — find workflow files changed in the PR.
    changed = git_diff_paths(base_sha, head_sha)
    changed_workflows = [
        p
        for p in changed
        if p.startswith(wf_dir + "/")
        and (p.endswith(".yml") or p.endswith(".yaml"))
    ]
    if not changed_workflows:
        print(
            "::notice::no workflow file changes in this PR; "
            "lint-required-context-exists-in-bp skipped."
        )
        return 0

    # Step 2 — load base+head + compute new contexts.
    head_workflows: dict[str, tuple[str, Any]] = {}
    new_contexts: set[str] = set()
    for path in changed_workflows:
        base_raw = git_show(base_sha, path)
        head_raw = git_show(head_sha, path)
        if head_raw is None:
            # File deleted on head — no new emission contribution.
            continue
        try:
            head_doc = yaml.safe_load(head_raw)
        except yaml.YAMLError as e:
            sys.stderr.write(
                f"::error file={path}::YAML parse error on head: {e}\n"
            )
            return 2
        head_workflows[path] = (head_raw, head_doc)
        head_ctx = workflow_contexts(head_doc)
        base_ctx: set[str] = set()
        if base_raw is not None:
            try:
                base_doc = yaml.safe_load(base_raw)
            except yaml.YAMLError:
                base_doc = None
            if base_doc is not None:
                base_ctx = workflow_contexts(base_doc)
        new_contexts |= (head_ctx - base_ctx)

    if not new_contexts:
        print(
            "::notice::no new context emissions detected in this PR; "
            "lint-required-context-exists-in-bp skipped."
        )
        return 0

    # Step 3 — fetch BP context list.
    status, bp = api("GET", f"/repos/{repo}/branch_protections/{branch}")
    bp_contexts: set[str] = set()
    if status == "forbidden":
        sys.stderr.write(
            f"::error::GET branch_protections/{branch} returned HTTP 403 — "
            f"DRIFT_BOT_TOKEN lacks repo-admin scope. Cannot verify "
            f"bp-required directives; skipping lint with exit 0 per "
            f"Tier 2a contract. Fix the token, not the lint.\n"
        )
        return 0
    elif status == "not_found":
        # Branch has no protection — nothing to verify against; the
        # bp-required: yes directive can't be satisfied. Treat as
        # graceful-skip rather than red-X.
        print(
            f"::notice::branch '{branch}' has no protection; cannot verify "
            f"bp-required directives. Skipping (exit 0)."
        )
        return 0
    elif status == "ok" and isinstance(bp, dict):
        bp_contexts = set(bp.get("status_check_contexts") or [])
    else:
        sys.stderr.write(
            f"::error::branch_protections/{branch} response unexpected; "
            f"status={status}. Treating as transient; exit 0.\n"
        )
        return 0

    # Step 4 — validate each new emission's directive.
    violations: list[str] = []
    for ctx in sorted(new_contexts):
        emitter = _resolve_emitter(ctx, head_workflows)
        if emitter is None:
            # Shouldn't happen — we just derived ctx from head_workflows.
            # Belt-and-suspenders fallback.
            violations.append(
                f"::error::new emission '{ctx}' (could not resolve emitter "
                f"file/job — bug in lint?)"
            )
            continue
        file_path, jkey = emitter
        raw_text, _ = head_workflows[file_path]
        directive = find_directive_for_job(raw_text, jkey)
        if directive is None:
            violations.append(
                f"::error file={file_path}::lint-required-context-exists-in-bp "
                f"(Tier 2g): NEW emission `{ctx}` (job '{jkey}') has no "
                f"directive comment. Add ONE of these comments on the line "
                f"directly above `{jkey}:` (within {_DIRECTIVE_WINDOW} lines):\n"
                f"  - `# bp-required: yes` — and ensure the context is "
                f"already in branch_protections/{branch}.status_check_contexts.\n"
                f"  - `# bp-required: pending #NNN` — acknowledged asymmetry, "
                f"references the tracking issue for the BP PATCH.\n"
                f"  - `# bp-exempt: <reason>` — informational job, not a gate.\n"
                f"Memory: internal#350 (PR#656 + mc#664 empirical case)."
            )
            continue
        kind, value = directive
        if kind == "exempt":
            print(f"::notice::{ctx}: bp-exempt directive present, OK.")
            continue
        if kind == "required-pending":
            print(
                f"::notice::{ctx}: bp-required: pending #{value} — "
                f"acknowledged asymmetry, OK."
            )
            continue
        if kind == "required-yes":
            if ctx in bp_contexts:
                print(
                    f"::notice::{ctx}: bp-required: yes, and context is in "
                    f"BP, OK."
                )
            else:
                violations.append(
                    f"::error file={file_path}::lint-required-context-exists-in-bp "
                    f"(Tier 2g): job '{jkey}' has `bp-required: yes` "
                    f"directive but its emitted context `{ctx}` is NOT in "
                    f"`branch_protections/{branch}.status_check_contexts`. "
                    f"FIX: either (a) add `{ctx}` to BP (Owners-tier PATCH), "
                    f"or (b) downgrade the directive to "
                    f"`# bp-required: pending #NNN` referencing the tracker "
                    f"for the pending BP PATCH."
                )

    if violations:
        print(
            f"::error::lint-required-context-exists-in-bp: "
            f"{len(violations)} violation(s) across "
            f"{len(changed_workflows)} changed workflow file(s)."
        )
        for v in violations:
            print(v)
        return 1

    print(
        f"::notice::lint-required-context-exists-in-bp: "
        f"{len(new_contexts)} new emission(s) all directive-validated."
    )
    return 0


if __name__ == "__main__":
    sys.exit(run())
