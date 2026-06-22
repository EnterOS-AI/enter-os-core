#!/usr/bin/env python3
"""lint-pre-flip-continue-on-error — block a PR that flips a job from
``continue-on-error: true`` to ``continue-on-error: false`` (or removes
the key while the base had it ``true``) without proof that the job's
recent runs on the target branch are actually green.

Empirical class — PR #656 / mc#664:
  PR #656 (RFC internal#219 Phase 4) flipped 5 ``platform-build``-class
  jobs ``continue-on-error: true → false`` on the basis of a
  "verified green on main via combined-status check". But that "green"
  was the LIE produced by the prior ``continue-on-error: true``:
  Gitea Quirk #10 (internal#342 + dup #287) — when a step inside a
  job marked ``continue-on-error: true`` fails, the job-level status
  is still rolled up as ``success``. So the precondition the PR
  claimed to verify was structurally fooled by the bug being
  flipped.

  mc#664 then captured the surfaced defects (2 unrelated, mutually-
  masked regressions):

    Class 1: sqlmock helper drift since 2f36bb9a (24 days old)
    Class 2: OFFSEC-001 contract collision since 7d1a189f (1 day old)

  Codified 04:35Z as hongming-pc2 charter §SOP-N rule (e)
  "run-log-grep-before-flip": pull the actual run log + grep for
  ``--- FAIL`` / ``FAIL\\s`` BEFORE flipping; don't trust the masked
  combined-status.

This script structurally enforces that rule at PR time.

How it works (one PR tick):
  1. Parse the diff: compare ``.gitea/workflows/*.yml`` at PR base
     vs PR head. For each file present in both, parse the YAML AST
     and walk ``jobs.<key>.continue-on-error`` on each side. A
     "flip" is base ∈ {true} AND head ∈ {false, None/absent}. We
     coerce truthy/falsy per YAML semantics (PyYAML normalizes
     ``true``/``True``/``yes`` to ``True``).
  2. For each flipped job, derive its commit-status context name as
     ``"{workflow.name} / {job.name or job.key} (push)"`` — that's
     how Gitea Actions emits the context for runs on
     ``main``/``staging`` (push event, see also expected_context()
     in ci-required-drift.py).
  3. Pull the last N commits of the target branch (PR base), fetch
     combined commit-status per commit, scan ``statuses[]`` for
     contexts matching ANY of the flipped jobs. For each match,
     fetch the actual run log via the web-UI route
     ``{server_url}/{repo}/actions/runs/{run_id}/jobs/{job_idx}/logs``
     (per memory ``reference_gitea_actions_log_fetch`` — Gitea 1.22.6
     lacks REST ``/actions/runs/*`` endpoints; the web-UI route is the
     only working path; see ``reference_gitea_1_22_6_lacks_rest_rerun_endpoints``).
  4. Grep each log for the Go-test failure markers ``--- FAIL`` /
     ``FAIL\\s+<package>`` AND the bash-step error sentinel
     ``::error::``. If ANY recent log shows any of these AND the
     status itself reads ``success``, the job was masked. ``::error::``
     the flip with the offending test name + offending run URL +
     the regression commit (HEAD of the run).
  5. Exit 1 if any flips have at least one masked run; exit 0
     otherwise.

Halt-on-noise contract:
  - If a recent log fetch 404s (already-pruned-via-act_runner-gc,
     transient gitea-web outage): emit ``::warning::`` and treat the
     run as "log unavailable" — does NOT block the flip; logged so
     a curious reviewer can re-run.
  - If a flipped job has ZERO recent runs on the target branch (newly
     added workflow): emit ``::warning::`` "no run history to verify"
     and allow the flip. This is the only way a NEW workflow can ever
     ship with ``continue-on-error: false``; otherwise we'd have a
     chicken-and-egg.

Behavior-based AST gate per ``feedback_behavior_based_ast_gates``:
  - YAML parsed via PyYAML safe_load on BOTH sides of the diff
  - No grep-by-line — formatting changes (comment churn, key order)
    don't false-positive a flip
  - Job-key match — so a rename ``platform-build → core-be-build``
    appears as a DELETE + an ADD, not a flip (the delete side has no
    new value to compare against; the add side has no base side).

Run locally (works against this repo, requires PyYAML + Gitea token
that can read combined-commit-status):

    GITEA_TOKEN=... GITEA_HOST=git.moleculesai.app \\
      REPO=molecule-ai/molecule-core BASE_REF=main \\
      BASE_SHA=$(git rev-parse origin/main) \\
      HEAD_SHA=$(git rev-parse HEAD) \\
      python3 .gitea/scripts/lint_pre_flip_continue_on_error.py \\
        --dry-run

Cross-links: PR#656, mc#664, PR#665 (the interim re-mask),
Quirk #10 (internal#342 + dup #287), hongming-pc2 charter §SOP-N
rule (e), feedback_strict_root_only_after_class_a,
feedback_no_shared_persona_token_use.
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

import yaml  # PyYAML 6.0.2 — installed by the workflow before this runs.


# --------------------------------------------------------------------------
# Environment (read at module-import; runtime contract enforced in main())
# --------------------------------------------------------------------------
def _env(key: str, *, default: str = "") -> str:
    return os.environ.get(key, default)


GITEA_TOKEN = _env("GITEA_TOKEN")
GITEA_HOST = _env("GITEA_HOST")
REPO = _env("REPO")
BASE_REF = _env("BASE_REF", default="main")
BASE_SHA = _env("BASE_SHA")
HEAD_SHA = _env("HEAD_SHA")
# How many recent commits to scan on the target branch. 5 by default;
# enough to catch a job that only fails intermittently, not so many
# that the script paginates needlessly. Per spec.
RECENT_COMMITS_N = int(_env("RECENT_COMMITS_N", default="5"))

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""
WEB = f"https://{GITEA_HOST}" if GITEA_HOST else ""

# Failure markers we grep for in the run log.
#   --- FAIL — Go test failure marker
#   FAIL\s   — `FAIL  github.com/x/y` package-level rollup
#   ::error:: — bash-step `::error::` lines (the lint-curl-status-capture
#               pattern: a `python3 <<PY` block writing `::error::` then
#               sys.exit(1); also any shell `echo "::error::..."` from
#               jobs that wrap pytest/eslint/etc. and convert
#               non-zero exits into masked-by-CoE status)
FAIL_PATTERNS = (
    "--- FAIL",
    "FAIL\t",
    "FAIL ",
    "::error::",
)


def _require_runtime_env() -> None:
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "BASE_REF", "BASE_SHA", "HEAD_SHA"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)


# --------------------------------------------------------------------------
# Tiny HTTP helper (no requests dependency)
# Mirrors the api()/ApiError contract in ci-required-drift.py +
# main-red-watchdog.py per feedback_api_helper_must_raise_not_return_dict.
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    """Raised when a Gitea API/web call cannot be trusted to have succeeded.

    Soft-failure on non-2xx is the duplicate-write bug factory in
    find-or-create flows (PR #112 Five-Axis). Here it would mean a
    transient gitea-web 502 silently allows a flip whose recent runs
    we couldn't actually verify — exactly the regression class this
    lint exists to close.
    """


def http(
    method: str,
    url: str,
    *,
    body: dict | None = None,
    headers: dict[str, str] | None = None,
    expect_json: bool = True,
    timeout: int = 30,
) -> tuple[int, Any, bytes]:
    """Tiny HTTP helper around urllib.

    Returns (status, parsed_or_None, raw_bytes). Raises ApiError on any
    non-2xx response. ``expect_json=False`` returns raw bytes in the
    parsed slot (for log-fetch from the web-UI which returns text/plain).
    """
    final_headers = {
        "Authorization": f"token {GITEA_TOKEN}",
        "Accept": "application/json" if expect_json else "text/plain",
    }
    if headers:
        final_headers.update(headers)
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        final_headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, method=method, data=data, headers=final_headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read() or b""
        status = e.code

    if not (200 <= status < 300):
        snippet = raw[:500].decode("utf-8", errors="replace") if raw else ""
        raise ApiError(f"{method} {url} → HTTP {status}: {snippet}")

    if not expect_json:
        return status, raw, raw
    if not raw:
        return status, None, raw
    try:
        return status, json.loads(raw), raw
    except json.JSONDecodeError as e:
        raise ApiError(f"{method} {url} → HTTP {status} but body is not JSON: {e}") from e


def api(method: str, path: str, *, body: dict | None = None, query: dict[str, str] | None = None) -> tuple[int, Any]:
    """Read-shaped Gitea REST helper. Path is API-relative (``/repos/...``)."""
    url = f"{API}{path}"
    if query:
        url = f"{url}?{urllib.parse.urlencode(query)}"
    status, parsed, _ = http(method, url, body=body, expect_json=True)
    return status, parsed


# --------------------------------------------------------------------------
# YAML parsing — coerce truthy/falsy for continue-on-error
# --------------------------------------------------------------------------
def _coerce_coe(val: Any) -> bool:
    """Coerce a continue-on-error YAML value to bool.

    PyYAML safe_load normalizes ``true``/``True``/``yes``/``on`` to
    Python ``True`` and ``false``/``False``/``no``/``off`` / absence
    to ``False`` (we treat absence/None as False here too — that's the
    GitHub Actions default semantics).

    Edge cases:
      - String ``"true"`` (quoted in YAML) — kept as the string
        ``"true"``, falsy under bool() but a flip we DO care about
        catching. Normalize string forms case-insensitively to bool
        so the diff is consistent with the runtime behavior of
        Gitea Actions, which YAML-parses the same way.
    """
    if isinstance(val, bool):
        return val
    if val is None:
        return False
    if isinstance(val, str):
        return val.strip().lower() in ("true", "yes", "on", "1")
    return bool(val)


def jobs_coe_map(workflow_doc: dict) -> dict[str, bool]:
    """Return ``{job_key: continue_on_error_bool}`` for every job in
    the workflow. Job-level ``continue-on-error`` only — does NOT
    descend into per-step ``continue-on-error`` (step-level CoE
    masking is a separate class and is handled by the test suite
    + reviewer, not by this gate — see Future Work in the workflow
    YAML).
    """
    out: dict[str, bool] = {}
    jobs = workflow_doc.get("jobs")
    if not isinstance(jobs, dict):
        return out
    for key, job in jobs.items():
        if not isinstance(job, dict):
            continue
        out[key] = _coerce_coe(job.get("continue-on-error"))
    return out


def workflow_name(workflow_doc: dict, *, fallback: str = "") -> str:
    """Top-level ``name:`` of the workflow. Falls back to the filename
    (without extension) per Gitea Actions semantics."""
    n = workflow_doc.get("name")
    if isinstance(n, str) and n.strip():
        return n.strip()
    return fallback


def job_display_name(workflow_doc: dict, job_key: str) -> str:
    """``jobs.<key>.name`` if present, else the key. Mirrors
    expected_context() in ci-required-drift.py."""
    job = workflow_doc.get("jobs", {}).get(job_key)
    if isinstance(job, dict):
        n = job.get("name")
        if isinstance(n, str) and n.strip():
            return n.strip()
    return job_key


def context_name(workflow_name_str: str, job_name_str: str, event: str = "push") -> str:
    """Render the commit-status context the way Gitea Actions emits it.
    Default ``event="push"`` because recent-runs-on-main are push events;
    callers can override to ``"pull_request"`` for PR-context lookups."""
    return f"{workflow_name_str} / {job_name_str} ({event})"


# --------------------------------------------------------------------------
# Diff detection — flips, not arbitrary changes
# --------------------------------------------------------------------------
def detect_flips(
    base_workflows: dict[str, str],
    head_workflows: dict[str, str],
) -> list[dict]:
    """Compare per-file CoE maps; return a list of flip records.

    Inputs are ``{path: yaml_text}`` for both sides. Output records
    have the shape::

        {
          "workflow_path": ".gitea/workflows/ci.yml",
          "workflow_name": "CI",
          "job_key":   "platform-build",
          "job_name":  "Platform (Go)",
          "context":   "CI / Platform (Go) (push)",
        }

    A flip is base[CoE] ∈ {True} AND head[CoE] ∈ {False}. Files
    only present on one side are skipped — adding a new workflow
    with ``CoE: false`` is fine (no history to mask), and removing
    a workflow can't possibly flip anything.
    """
    flips: list[dict] = []
    for path, base_text in base_workflows.items():
        if path not in head_workflows:
            continue
        try:
            base_doc = yaml.safe_load(base_text) or {}
            head_doc = yaml.safe_load(head_workflows[path]) or {}
        except yaml.YAMLError as e:
            # Don't block on a parse error — the YAML lint workflows
            # catch invalid YAML separately. Just warn so the failing
            # file is visible.
            sys.stderr.write(f"::warning file={path}::YAML parse error: {e}\n")
            continue
        if not isinstance(base_doc, dict) or not isinstance(head_doc, dict):
            continue
        base_map = jobs_coe_map(base_doc)
        head_map = jobs_coe_map(head_doc)
        wf_name = workflow_name(head_doc, fallback=os.path.basename(path).rsplit(".", 1)[0])
        for job_key, base_val in base_map.items():
            if job_key not in head_map:
                continue  # job removed — not a flip
            if base_val is True and head_map[job_key] is False:
                flips.append({
                    "workflow_path": path,
                    "workflow_name": wf_name,
                    "job_key": job_key,
                    "job_name": job_display_name(head_doc, job_key),
                    "context": context_name(wf_name, job_display_name(head_doc, job_key), "push"),
                })
    return flips


# --------------------------------------------------------------------------
# Git: snapshot every .gitea/workflows/*.yml at a SHA (no checkout)
# --------------------------------------------------------------------------
def _git(*args: str, cwd: str | None = None) -> str:
    """Run ``git`` and return stdout (text)."""
    result = subprocess.run(
        ["git", *args],
        capture_output=True,
        text=True,
        check=False,
        cwd=cwd,
    )
    if result.returncode != 0:
        raise RuntimeError(f"git {args!r} failed: {result.stderr.strip()}")
    return result.stdout


def workflows_at_sha(sha: str, *, repo_dir: str | None = None) -> dict[str, str]:
    """Read every ``.gitea/workflows/*.yml`` blob at ``sha``.

    Uses ``git ls-tree`` + ``git show`` so we never need to check out
    the SHA (the workflow runs on the PR head; the base SHA is
    fetched, not checked out).
    """
    out: dict[str, str] = {}
    listing = _git("ls-tree", "-r", "--name-only", sha, ".gitea/workflows/", cwd=repo_dir)
    for line in listing.splitlines():
        line = line.strip()
        if not line.endswith((".yml", ".yaml")):
            continue
        try:
            blob = _git("show", f"{sha}:{line}", cwd=repo_dir)
        except RuntimeError:
            # Symlink or other non-blob; skip.
            continue
        out[line] = blob
    return out


# --------------------------------------------------------------------------
# Gitea: recent commits + per-commit combined status + log fetch
# --------------------------------------------------------------------------
def recent_commits_on_branch(branch: str, n: int) -> list[str]:
    """Last `n` commit SHAs on ``branch`` (oldest→newest is fine; we
    treat them as a set). Uses the REST ``/commits`` endpoint with
    ``sha=branch&limit=n``."""
    _, body = api(
        "GET",
        f"/repos/{OWNER}/{NAME}/commits",
        query={"sha": branch, "limit": str(n)},
    )
    if not isinstance(body, list):
        raise ApiError(f"/commits for {branch} returned non-list: {type(body).__name__}")
    out: list[str] = []
    for c in body:
        if isinstance(c, dict):
            sha = c.get("sha") or (c.get("commit", {}) or {}).get("id")
            if isinstance(sha, str) and len(sha) >= 7:
                out.append(sha)
    return out


def combined_status(sha: str) -> dict:
    """Combined commit status for a SHA. Same shape as
    ``main-red-watchdog.get_combined_status``."""
    _, body = api("GET", f"/repos/{OWNER}/{NAME}/commits/{sha}/status")
    if not isinstance(body, dict):
        raise ApiError(f"combined-status for {sha} not a dict")
    return body


def _entry_state(s: dict) -> str:
    """Per-entry state — Gitea 1.22.6 schema asymmetry: top-level
    uses ``state``, per-entry uses ``status``. Defensive fallback per
    main-red-watchdog.py line 233."""
    return s.get("status") or s.get("state") or ""


def fetch_log(target_url: str) -> str | None:
    """Fetch a job log given its web-UI ``target_url`` (e.g.
    ``/molecule-ai/molecule-core/actions/runs/13494/jobs/0``).

    Per ``reference_gitea_actions_log_fetch``: append ``/logs`` to the
    job route. Per ``reference_gitea_1_22_6_lacks_rest_rerun_endpoints``:
    Gitea 1.22.6 lacks the REST ``/api/v1/.../actions/runs/*`` path; the
    web-UI route is the only working endpoint until 1.24+.

    Returns the log text on success, ``None`` on 404 / log-pruned /
    network error (caller treats None as "log unavailable, warn-not-fail").
    """
    if not target_url:
        return None
    # Normalize: target_url may be relative ("/owner/repo/...") or
    # absolute. Both need ``/logs`` appended to the job sub-path.
    if target_url.startswith("/"):
        url = f"{WEB}{target_url}"
    else:
        url = target_url
    if not url.endswith("/logs"):
        url = f"{url}/logs"
    try:
        _, body, _ = http("GET", url, expect_json=False, timeout=60)
    except ApiError as e:
        sys.stderr.write(f"::warning::log fetch failed for {url}: {e}\n")
        return None
    if isinstance(body, bytes):
        return body.decode("utf-8", errors="replace")
    return None


def grep_fail_markers(log_text: str) -> list[str]:
    """Return up to 5 sample matching lines for any FAIL_PATTERNS hit.
    Empty list = clean log.

    Heuristic: skip lines where the marker appears inside script source
    (e.g. ``echo "::error::..."`` in a ``::group::Run`` block) rather
    than actual execution output. The Gitea Actions log prints the raw
    script before executing it; ``echo "::error::"`` lines in that
    display are false positives.
    """
    matches: list[str] = []
    in_run_group = False
    group_depth = 0
    for line in log_text.splitlines():
        stripped = line.strip()
        # Track Gitea Actions group markers so we can skip the
        # ``::group::Run`` script-source display blocks.
        if stripped.startswith("::group::Run"):
            in_run_group = True
            group_depth = 1
            continue
        if stripped == "::endgroup::":
            if in_run_group:
                in_run_group = False
                group_depth = 0
            continue
        if in_run_group:
            continue
        for pat in FAIL_PATTERNS:
            if pat in line:
                # Additional false-positive guard: ``echo "::error::"``
                # is script source, not a runtime error emission.
                if pat == "::error::":
                    prefix = line[: line.index(pat)].strip()
                    if prefix.endswith('echo') or prefix.endswith("echo '") or prefix.endswith('echo "'):
                        break
                matches.append(line.strip()[:240])
                break
        if len(matches) >= 5:
            break
    return matches


# --------------------------------------------------------------------------
# Verification: for one flip, scan recent runs on BASE_REF
# --------------------------------------------------------------------------
def verify_flip(flip: dict, branch: str, n: int) -> dict:
    """Scan the last ``n`` commits on ``branch``. For each commit whose
    combined status contains a context matching ``flip["context"]``,
    fetch the run log and grep for FAIL markers.

    Returns::

        {
          "flip": flip,
          "checked_commits": int,        # how many commits had a matching context
          "masked_runs": [               # runs where log shows FAIL despite status==success
            {"sha": "...", "status": "success", "target_url": "...", "samples": [...]},
            ...
          ],
          "fail_runs": [                 # runs where status itself is failure/error
            {"sha": "...", "status": "failure", "target_url": "...", "samples": [...]},
            ...
          ],
          "warnings": [str],             # log-unavailable warnings (not blocking)
        }

    Blocking condition: ``masked_runs`` OR ``fail_runs`` non-empty.
    A ``success`` status with a clean log is the only "OK to flip"
    outcome (per hongming-pc2 §SOP-N rule (e)).
    """
    target_context = flip["context"]
    result = {
        "flip": flip,
        "checked_commits": 0,
        "masked_runs": [],
        "fail_runs": [],
        "warnings": [],
    }

    shas = recent_commits_on_branch(branch, n)
    if not shas:
        result["masked_runs"].append({
            "sha": "",
            "status": "unverified",
            "target_url": "",
            "samples": [f"no recent commits on {branch} — cannot verify flip"],
        })
        return result

    for sha in shas:
        try:
            status_doc = combined_status(sha)
        except ApiError as e:
            result["masked_runs"].append({
                "sha": sha,
                "status": "error",
                "target_url": "",
                "samples": [f"combined-status API error: {e}"],
            })
            continue
        statuses = status_doc.get("statuses") or []
        # First entry matching the context name. Newest SHAs come
        # first; one entry per context per SHA is the usual shape.
        for s in statuses:
            if not isinstance(s, dict):
                continue
            if s.get("context") != target_context:
                continue
            result["checked_commits"] += 1
            state = _entry_state(s)
            target_url = s.get("target_url") or ""
            log_text = fetch_log(target_url)
            if log_text is None:
                result["warnings"].append(
                    f"log unavailable for {sha} {target_context}"
                )
                # Still record the status itself if it's red — that's
                # a hard signal that doesn't need log access.
                if state in ("failure", "error"):
                    result["fail_runs"].append({
                        "sha": sha,
                        "status": state,
                        "target_url": target_url,
                        "samples": ["[log unavailable; status itself is " + state + "]"],
                    })
                elif state == "success":
                    # Fail-closed: unreadable log on a success status is a
                    # potential Quirk #10 mask (continue-on-error hiding real
                    # failures). We cannot verify it's clean, so treat as
                    # masked rather than allowing the flip.
                    result["masked_runs"].append({
                        "sha": sha,
                        "status": state,
                        "target_url": target_url,
                        "samples": ["[log unavailable; cannot verify status is genuine — treat as masked]"],
                    })
                break
            samples = grep_fail_markers(log_text)
            if state in ("failure", "error"):
                result["fail_runs"].append({
                    "sha": sha,
                    "status": state,
                    "target_url": target_url,
                    "samples": samples or ["[no FAIL markers found but status is " + state + "]"],
                })
            elif samples and state == "success":
                # The bug class: status==success while log shows FAIL.
                # That's exactly Quirk #10 (continue-on-error masking).
                result["masked_runs"].append({
                    "sha": sha,
                    "status": state,
                    "target_url": target_url,
                    "samples": samples,
                })
            # Either way, we matched one context entry for this SHA;
            # don't keep looping `statuses[]`.
            break

    if result["checked_commits"] == 0:
        result["masked_runs"].append({
            "sha": "",
            "status": "unverified",
            "target_url": "",
            "samples": [f"no runs of {target_context!r} found in the last {n} commits on {branch} — cannot verify flip"],
        })
    return result


# --------------------------------------------------------------------------
# Report rendering
# --------------------------------------------------------------------------
def render_flip_report(verdict: dict) -> str:
    flip = verdict["flip"]
    lines = [
        f"job: {flip['job_key']} ({flip['context']})",
        f"  workflow:        {flip['workflow_path']}",
        f"  checked_commits: {verdict['checked_commits']}",
    ]
    for run in verdict["fail_runs"]:
        url = run["target_url"]
        # target_url may be relative; render the absolute form for
        # click-through.
        if url.startswith("/"):
            url = f"{WEB}{url}"
        lines.append(f"  fail run {run['sha'][:10]} (status={run['status']}): {url}")
        for sample in run["samples"]:
            lines.append(f"    | {sample}")
    for run in verdict["masked_runs"]:
        url = run["target_url"]
        if url.startswith("/"):
            url = f"{WEB}{url}"
        lines.append(
            f"  MASKED run {run['sha'][:10]} (status=success, log shows FAIL): {url}"
        )
        for sample in run["samples"]:
            lines.append(f"    | {sample}")
    for w in verdict["warnings"]:
        lines.append(f"  warning: {w}")
    return "\n".join(lines)


# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="lint-pre-flip-continue-on-error",
        description="Block a PR that flips continue-on-error true→false "
        "without proof recent runs are actually green.",
    )
    p.add_argument(
        "--dry-run",
        action="store_true",
        help="Detect + print findings to stdout; never exit non-zero. "
        "Useful for local testing.",
    )
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = _parse_args(argv)
    _require_runtime_env()

    base_workflows = workflows_at_sha(BASE_SHA)
    head_workflows = workflows_at_sha(HEAD_SHA)
    # Ignore workflow files that are identical on both sides — old branches
    # that haven't rebased onto main carry stale copies of workflows that
    # were updated later. Comparing those stale copies against the current
    # base produces false-positive "flips".
    base_workflows = {
        p: t for p, t in base_workflows.items()
        if p in head_workflows and head_workflows[p] != t
    }
    head_workflows = {p: t for p, t in head_workflows.items() if p in base_workflows}
    flips = detect_flips(base_workflows, head_workflows)

    if not flips:
        print("::notice::no continue-on-error true→false flips in this PR")
        return 0

    print(f"::notice::detected {len(flips)} continue-on-error true→false flip(s); verifying recent runs on {BASE_REF}")
    bad_flips: list[dict] = []
    for flip in flips:
        verdict = verify_flip(flip, BASE_REF, RECENT_COMMITS_N)
        report = render_flip_report(verdict)
        if verdict["fail_runs"] or verdict["masked_runs"]:
            print(f"::error file={flip['workflow_path']}::flip of {flip['job_key']} "
                  f"({flip['context']}) blocked — recent runs on {BASE_REF} show "
                  f"FAIL markers OR are red. Pull each run log below + grep "
                  f"`--- FAIL` / `FAIL ` / `::error::` — DON'T trust the masked "
                  f"combined-status. See hongming-pc2 charter §SOP-N rule (e). "
                  f"PR#656 / mc#664 reference class.")
            bad_flips.append(verdict)
        else:
            print(f"::notice::flip of {flip['job_key']} ({flip['context']}) is safe — "
                  f"{verdict['checked_commits']} recent run(s), no FAIL markers")
        # Always print the per-flip detail block so the human-readable
        # report is in the run log for both safe and unsafe flips.
        print(f"::group::flip detail: {flip['job_key']}")
        print(report)
        print("::endgroup::")

    if bad_flips and not args.dry_run:
        print(f"::error::{len(bad_flips)}/{len(flips)} flip(s) failed pre-flip verification")
        return 1
    if bad_flips and args.dry_run:
        print(f"::warning::[dry-run] {len(bad_flips)}/{len(flips)} flip(s) WOULD fail; exit 0 forced")
    return 0


if __name__ == "__main__":
    sys.exit(main())
