#!/usr/bin/env python3
"""lint-required-no-paths — structural enforcement of
`feedback_path_filtered_workflow_cant_be_required`.

For every workflow whose status-check context appears in
`branch_protections/<branch>.status_check_contexts`, assert that the
workflow's `on:` block has NO `paths:` and NO `paths-ignore:` filter.

A required-check workflow with a paths filter silently degrades the
merge gate:

  - If the PR's diff doesn't match the `paths:` glob, the workflow
    never fires.
  - Gitea (1.22.6) reports the required context as `pending` (never as
    `skipped == success`), so the PR cannot merge.
  - For a docs-only PR against `paths: ['**.go']`, the PR is
    blocked forever — no human action can produce a green.

The class was previously prevented only by reviewer vigilance + the
saved memory `feedback_path_filtered_workflow_cant_be_required`. This
script makes it a hard CI gate so a future PR adding `paths:` to a
required workflow fails fast at PR time, not after merge when the next
docs PR wedges main.

The lint runs as `.gitea/workflows/lint-required-no-paths.yml` on every
PR. The lint workflow ITSELF must not have a paths-filter (otherwise it
could be circumvented by a paths-non-matching PR) — that's enforced by
self-reference and by the workflow's own `on:` block deliberately
omitting filters.

Sources of truth:
  - `branch_protections/<branch>` `status_check_contexts` (the merge gate)
  - `.gitea/workflows/*.yml` `name:` + `on:` (the workflow set)

Context-format note (Gitea 1.22.6):
  Status-check contexts are formatted `{workflow_name} / {job_name_or_key} ({event})`.
  We parse the workflow_name prefix and walk `.gitea/workflows/*.yml` for
  a file whose `name:` attr matches. (The filename is NOT the source of
  truth; `name:` is, because Gitea formats the context from `name:`.)

Exit codes:
  0 — no required workflow has a paths/paths-ignore filter (clean) OR
      branch_protections endpoint returned 403/404 (token-scope issue;
      surfaced via ::error:: but non-fatal so a missing scope doesn't
      red-X every PR — fix the token, not the lint).
  1 — at least one required workflow has a paths/paths-ignore filter
      (the gate-degrading defect class).
  2 — env contract violation (missing GITEA_TOKEN/HOST/REPO/BRANCH).
  3 — workflows directory missing or workflow YAML unparseable.
  4 — protection response shape unexpected (non-dict body on 2xx).

Auth note: `GET /repos/.../branch_protections/{branch}` requires
repo-admin role in Gitea 1.22.6. The workflow-default `GITHUB_TOKEN`
is non-admin; we re-use `DRIFT_BOT_TOKEN` (same persona that powers
ci-required-drift.yml). If `DRIFT_BOT_TOKEN` is unavailable in a future
context, the script falls through gracefully (exit 0 + ::error::).
"""
from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

import yaml  # PyYAML 6.0.2 — installed by the workflow before this runs.


# --------------------------------------------------------------------------
# Environment
# --------------------------------------------------------------------------
def _env(key: str, *, required: bool = True, default: str | None = None) -> str:
    val = os.environ.get(key, default)
    if required and not val:
        sys.stderr.write(f"::error::missing required env var: {key}\n")
        sys.exit(2)
    return val or ""


GITEA_TOKEN = _env("GITEA_TOKEN", required=False)
GITEA_HOST = _env("GITEA_HOST", required=False)
REPO = _env("REPO", required=False)
BRANCH = _env("BRANCH", required=False, default="main")
WORKFLOWS_DIR = _env(
    "WORKFLOWS_DIR", required=False, default=".gitea/workflows"
)

OWNER, NAME = (REPO.split("/", 1) + [""])[:2] if REPO else ("", "")
API = f"https://{GITEA_HOST}/api/v1" if GITEA_HOST else ""


def _require_runtime_env() -> None:
    """Enforce env contract — called from `run()` only. Tests import
    individual functions without setting the full env contract."""
    for key in ("GITEA_TOKEN", "GITEA_HOST", "REPO", "BRANCH"):
        if not os.environ.get(key):
            sys.stderr.write(f"::error::missing required env var: {key}\n")
            sys.exit(2)


# --------------------------------------------------------------------------
# Tiny HTTP helper (mirrors ci-required-drift.py contract:
# raise on non-2xx and on JSON-decode-fail when JSON expected, per
# `feedback_api_helper_must_raise_not_return_dict`).
# --------------------------------------------------------------------------
class ApiError(RuntimeError):
    """Raised when a Gitea API call cannot be trusted to have succeeded."""


def api(
    method: str,
    path: str,
    *,
    body: dict | None = None,
    query: dict[str, str] | None = None,
    expect_json: bool = True,
) -> tuple[int, Any]:
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
        raise ApiError(f"{method} {path} → HTTP {status}: {snippet}")

    if not raw:
        return status, None
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError as e:
        if expect_json:
            raise ApiError(
                f"{method} {path} → HTTP {status} but body is not JSON: {e}"
            ) from e
        return status, {"_raw": raw.decode("utf-8", errors="replace")}


# --------------------------------------------------------------------------
# Status-check context parser
# --------------------------------------------------------------------------
# Format: "<workflow_name> / <job_name_or_key> (<event>)"
# Examples observed on molecule-core/main:
#   "Secret scan / Scan diff for credential-shaped strings (pull_request)"
#   "sop-tier-check / tier-check (pull_request)"
#
# Split strategy: peel off the trailing ` (<event>)` first, then split
# the leading `<workflow> / <rest>` on the FIRST ` / ` (workflow names
# come from `name:` attrs which conventionally don't embed ' / '; job
# names CAN, so we keep the rest of the slash-divided text as the job
# name). This matches Gitea's `name: ` semantics.
_CONTEXT_RE = re.compile(r"^(?P<workflow>.+?) / (?P<job>.+) \((?P<event>[^)]+)\)$")


def parse_context(ctx: str) -> tuple[str, str, str] | None:
    """Parse `<workflow> / <job> (<event>)` → (workflow, job, event) or None."""
    if not ctx:
        return None
    m = _CONTEXT_RE.match(ctx)
    if not m:
        return None
    return m.group("workflow"), m.group("job"), m.group("event")


# --------------------------------------------------------------------------
# workflow-name → file resolution
# --------------------------------------------------------------------------
def _iter_workflow_files() -> list[Path]:
    d = Path(WORKFLOWS_DIR)
    if not d.is_dir():
        sys.stderr.write(f"::error::workflows directory not found: {d}\n")
        sys.exit(3)
    # `.yml` and `.yaml` — Gitea accepts both (rarely used `.yaml`, but
    # don't silently miss it if a future port uses it).
    return sorted(list(d.glob("*.yml")) + list(d.glob("*.yaml")))


def resolve_workflow_file(workflow_name: str) -> Path | None:
    """Find the YAML file whose `name:` attr matches `workflow_name`.

    Returns None if no match. Filename is NOT used as a fallback —
    Gitea's context format uses `name:`, so a `name:`-less workflow
    won't even appear in the protection list. (A YAML with no `name:`
    would default the context to the file basename, but our protection
    contexts on molecule-core are all `name:`-derived; we trust the
    same.)
    """
    for f in _iter_workflow_files():
        try:
            doc = yaml.safe_load(f.read_text(encoding="utf-8"))
        except yaml.YAMLError as e:
            sys.stderr.write(f"::error::YAML parse error in {f}: {e}\n")
            sys.exit(3)
        if isinstance(doc, dict) and doc.get("name") == workflow_name:
            return f
    return None


# --------------------------------------------------------------------------
# paths-filter detection
# --------------------------------------------------------------------------
# Triggers that accept `paths:` / `paths-ignore:` (per GitHub Actions /
# Gitea Actions docs): pull_request, pull_request_target, push.
# We don't enumerate — any sub-key named `paths` or `paths-ignore`
# inside an event mapping is flagged.
_PATHS_KEYS = ("paths", "paths-ignore")


def detect_paths_filters(workflow_path: Path) -> list[str]:
    """Walk the workflow's `on:` block and return a list of findings, one
    per offending `paths`/`paths-ignore` key.

    Returns:
        Empty list if the workflow has no paths/paths-ignore filter
        anywhere in its `on:` block. Otherwise, a list of human-readable
        strings naming the event and filter key + the filter contents.
    """
    try:
        doc = yaml.safe_load(workflow_path.read_text(encoding="utf-8"))
    except yaml.YAMLError as e:
        sys.stderr.write(f"::error::YAML parse error in {workflow_path}: {e}\n")
        sys.exit(3)
    if not isinstance(doc, dict):
        return []

    on_block = doc.get("on") or doc.get(True)  # PyYAML 6 quirk: `on:`
    # under default constructor sometimes becomes the bool key `True`
    # because YAML 1.1 treats `on` as a boolean. Tolerate both.
    if on_block is None:
        return []

    findings: list[str] = []

    # Shape A: `on: pull_request` (string shorthand) — cannot carry filters.
    if isinstance(on_block, str):
        return []
    # Shape B: `on: [pull_request, push]` (list shorthand) — cannot carry filters.
    if isinstance(on_block, list):
        return []
    # Shape C: `on: { event: { ... } }` — the standard mapping case.
    if isinstance(on_block, dict):
        # Defensive: top-level malformed `on.paths` (someone wrote
        # `on: { paths: ['x'] }` thinking it's a workflow-level filter).
        # This is invalid syntax, but if present, flag it — it might
        # not block the workflow from registering (Gitea may ignore the
        # unknown key) and would create a false sense of "filter exists"
        # the lint should still surface.
        for k in _PATHS_KEYS:
            if k in on_block:
                v = on_block[k]
                findings.append(
                    f"top-level `on.{k}` filter (malformed but present): {v!r}"
                )
        for event, event_body in on_block.items():
            if event in _PATHS_KEYS:
                continue  # already handled above
            if not isinstance(event_body, dict):
                # `pull_request: null` / `pull_request: [opened]` shapes —
                # no place for a paths filter to live; skip.
                continue
            for k in _PATHS_KEYS:
                if k in event_body:
                    v = event_body[k]
                    findings.append(
                        f"`on.{event}.{k}` filter present: {v!r}"
                    )
    return findings


# --------------------------------------------------------------------------
# Driver
# --------------------------------------------------------------------------
def run() -> int:
    """Main lint entrypoint. Returns the process exit code.

    Exit semantics (see module docstring for full table):
      0 — clean (no offending paths-filter on any required workflow),
          OR protection unreadable (403/404) — surfaced as ::error::
          but treated as non-fatal so token-scope issues don't red-X
          every PR.
      1 — at least one required workflow carries a paths/paths-ignore
          filter — the regression class this lint exists to prevent.
    """
    _require_runtime_env()

    protection_path = f"/repos/{OWNER}/{NAME}/branch_protections/{BRANCH}"
    try:
        _, protection = api("GET", protection_path)
    except ApiError as e:
        msg = str(e)
        m = re.search(r"HTTP (\d{3})", msg)
        http_status = int(m.group(1)) if m else None
        if http_status in (403, 404):
            sys.stderr.write(
                f"::error::GET {protection_path} returned HTTP {http_status} — "
                f"DRIFT_BOT_TOKEN lacks repo-admin scope (Gitea 1.22.6 "
                f"requires it for this endpoint) OR branch '{BRANCH}' has "
                f"no protection configured. Cannot enumerate required "
                f"checks; skipping lint with exit 0 to avoid red-X on "
                f"every PR. Fix: grant repo-admin to mc-drift-bot.\n"
            )
            return 0
        raise

    if not isinstance(protection, dict):
        sys.stderr.write(
            f"::error::protection response for {BRANCH} not a JSON object\n"
        )
        return 4

    contexts: list[str] = list(protection.get("status_check_contexts") or [])
    if not contexts:
        print(
            f"::notice::branch_protections/{BRANCH} has 0 required "
            f"status_check_contexts; nothing to lint. (no required contexts)"
        )
        return 0

    print(f"::notice::Linting {len(contexts)} required context(s) for paths-filter regressions:")
    for c in contexts:
        print(f"  - {c}")

    offenders: list[tuple[str, Path, list[str]]] = []
    unresolved: list[str] = []

    for ctx in contexts:
        parsed = parse_context(ctx)
        if parsed is None:
            print(
                f"::warning::could not parse context '{ctx}' "
                f"(expected `<workflow> / <job> (<event>)`); skipping"
            )
            unresolved.append(ctx)
            continue
        workflow_name, _job, _event = parsed
        wf_path = resolve_workflow_file(workflow_name)
        if wf_path is None:
            print(
                f"::warning::no workflow file in {WORKFLOWS_DIR} has "
                f"`name: {workflow_name}` (required context '{ctx}'); "
                f"skipping paths-filter check. "
                f"(orphaned-context detection is ci-required-drift's job.)"
            )
            unresolved.append(ctx)
            continue
        findings = detect_paths_filters(wf_path)
        if findings:
            offenders.append((workflow_name, wf_path, findings))
        else:
            print(f"::notice::OK {wf_path.name} ({workflow_name}) — no paths filter")

    if offenders:
        print("")
        print(f"::error::Found {len(offenders)} required workflow(s) with paths/paths-ignore filters:")
        for workflow_name, wf_path, findings in offenders:
            for finding in findings:
                # ::error file=... lets Gitea Actions surface a per-file
                # annotation in the PR UI (when annotations are wired).
                print(
                    f"::error file={wf_path}::Required workflow "
                    f"'{workflow_name}' ({wf_path.name}) has a paths "
                    f"filter that would degrade the merge gate to a "
                    f"silent indefinite pending: {finding}. "
                    f"See feedback_path_filtered_workflow_cant_be_required. "
                    f"Fix: remove the filter and instead gate per-step "
                    f"inside the job with `if: contains(steps.changed.outputs.files, ...)` "
                    f"or refactor to a single-job-with-per-step-if shape."
                )
        return 1

    print("")
    print(
        f"::notice::OK — all {len(contexts) - len(unresolved)} resolvable "
        f"required workflow(s) clean (no paths/paths-ignore filters)."
    )
    if unresolved:
        print(
            f"::notice::{len(unresolved)} required context(s) were not "
            f"resolved to a workflow file (warn-not-fail); see warnings above."
        )
    return 0


if __name__ == "__main__":
    sys.exit(run())
