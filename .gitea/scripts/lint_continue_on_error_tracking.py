#!/usr/bin/env python3
"""lint_continue_on_error_tracking — Tier 2e per internal#350.

Rule
----
Every `continue-on-error: true` directive in `.gitea/workflows/*.yml`
must be accompanied by a tracker reference comment within 2 lines
(above OR below the directive's line). The reference is one of:

  * `# mc#NNNN`          — molecule-core issue
  * `# internal#NNNN`    — molecule-ai/internal issue

The referenced issue must satisfy ALL of:

  1. Exists (HTTP 200 on `/repos/{owner}/{name}/issues/{num}`)
  2. `state == "open"`
  3. `created_at` is ≤ MAX_AGE_DAYS days ago (default 14)

A passing reference establishes an audit trail and a forced renewal
cadence — after 14 days the issue must either be CLOSED (the masked
defect was fixed) or the comment must point at a NEW tracker
(deliberate decision to keep masking, requires a paper-trail).

The class this prevents
-----------------------
Phase-3-masked failures. `continue-on-error: true` on `platform-build`
had been hiding mc#664-class regressions for ~3 weeks before #656
surfaced them on 2026-05-12. A 14-day cap forces a tracker review
cycle and surfaces mask-drift within at most 14 days of the original
defect.

Behaviour-based gate
--------------------
We parse via PyYAML AST (per `feedback_behavior_based_ast_gates`) to
detect `continue-on-error: <truthy>` at job-key level, then map each
location back to its source line via PyYAML's line-tracking loader.
Comments are scanned from the raw text within a 2-line window of
that source line. Reformatting (block-scalar vs flow-style) does not
break the rule because the source-line anchor is the directive's
own line.

Exit codes
----------
  0 — every `continue-on-error: true` has a passing tracker, OR
      the issue-API endpoint returned 403/404 (token-scope; graceful
      degrade per Tier 2a contract — surface via ::error:: on stderr
      but don't red-X every PR over auth).
  1 — at least one violation (missing/closed/too-old/non-existent
      tracker).
  2 — env contract violation, YAML parse error, or workflows-dir
      missing.

Env
---
  GITEA_TOKEN     — read scope on the configured repos.
                    Auto-injected `GITHUB_TOKEN` works for same-repo
                    issue reads; for `internal#NNN` we need a token
                    with `molecule-ai/internal` read scope. Use
                    DRIFT_BOT_TOKEN (same persona as other Tier 2
                    lints).
  GITEA_HOST      — e.g. git.moleculesai.app
  REPO            — `owner/name` for `mc#NNNN` lookups
  INTERNAL_REPO   — `owner/name` for `internal#NNNN` lookups
                    (defaults to derived `molecule-ai/internal`)
  WORKFLOWS_DIR   — defaults to `.gitea/workflows`
  MAX_AGE_DAYS    — defaults to 14

Memory cross-links
------------------
  - internal#350 (the RFC that specs this lint)
  - mc#664 (the masked-3-weeks empirical case)
  - feedback_chained_defects_in_never_tested_workflows
  - feedback_behavior_based_ast_gates
  - feedback_strict_root_only_after_class_a
"""
from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

try:
    import yaml
except ImportError:
    sys.stderr.write(
        "::error::PyYAML is required. Install with: pip install PyYAML\n"
    )
    sys.exit(2)


# ---------------------------------------------------------------------------
# Tracker comment regex.
# Matches: `# mc#1234`, `# internal#42`, `# mc#1234 - description`
# Also matches trackers embedded mid-sentence: `# see mc#1234 for details`
# Does NOT match: `# mc1234` (missing inner #), `mc#1234` (no leading
# comment `#`), `# MC#1234` (case-sensitive). The search is line-wide,
# not just at the comment-marker prefix — fixes false-negative when
# the tracker appears mid-sentence (e.g. `internal#350` after prose).
TRACKER_RE = re.compile(
    r"(?P<slug>mc|internal)#(?P<num>\d+)\b"
)

# Truthy continue-on-error values we treat as "true". PyYAML decodes
# `continue-on-error: true` to Python `True`. `continue-on-error: "true"`
# decodes to the string "true" — Gitea's evaluator coerces strings,
# so we treat string-`"true"` (case-insensitive) as truthy too.
def _is_truthy_coe(v: Any) -> bool:
    if v is True:
        return True
    if isinstance(v, str) and v.strip().lower() == "true":
        return True
    return False


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
# PyYAML line-tracking loader. yaml.SafeLoader nodes carry
# `start_mark.line` (0-based); using construct_mapping with `deep=True`
# preserves that on every node. We need the line of each
# `continue-on-error` key so we can scan the source for comments
# near it.
# ---------------------------------------------------------------------------
class _LineLoader(yaml.SafeLoader):
    """SafeLoader that annotates every dict with `__line__: {key: line}`."""


def _construct_mapping(loader: yaml.SafeLoader, node: yaml.MappingNode) -> dict:
    mapping = loader.construct_mapping(node, deep=True)
    # Annotate per-key source lines so we can locate `continue-on-error`.
    lines: dict[str, int] = {}
    for k_node, _v_node in node.value:
        try:
            key = loader.construct_object(k_node, deep=True)
        except Exception:
            continue
        if isinstance(key, (str, int, bool)):
            lines[str(key)] = k_node.start_mark.line + 1  # 1-based
    if isinstance(mapping, dict):
        mapping["__lines__"] = lines
    return mapping


_LineLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _construct_mapping
)


# ---------------------------------------------------------------------------
# Issue lookup
# ---------------------------------------------------------------------------
def fetch_issue(slug_kind: str, num: int) -> tuple[str, dict | None]:
    """Return `(status, payload_or_none)`.

    status ∈ {"ok", "not_found", "forbidden", "error"}.
    """
    repo = (
        _env("REPO") if slug_kind == "mc" else _env("INTERNAL_REPO")
    )
    if not repo:
        # Fall through gracefully — caller treats as 403 (token-scope).
        return ("forbidden", None)
    host = _env("GITEA_HOST")
    token = _env("GITEA_TOKEN")
    url = f"https://{host}/api/v1/repos/{repo}/issues/{num}"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"token {token}",
            "Accept": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return ("ok", json.loads(resp.read()))
    except urllib.error.HTTPError as e:
        if e.code == 404:
            return ("not_found", None)
        if e.code in (401, 403):
            return ("forbidden", None)
        return ("error", None)
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        return ("error", None)


# ---------------------------------------------------------------------------
# Locate every continue-on-error: <truthy> in a workflow doc, with line.
# ---------------------------------------------------------------------------
def find_coe_truthies(
    doc: Any, raw_lines: list[str]
) -> list[tuple[str, int]]:
    """Return list of (job_key, source_line_1based).

    `doc` is the LineLoader-parsed mapping. We descend `jobs.<key>` and
    return only those whose value is truthy per `_is_truthy_coe`.
    Job-step continue-on-error is intentionally NOT considered: it
    suppresses step-level failure rollup only, not job-level. The
    masking class this lint targets is the job-level rollup.
    """
    out: list[tuple[str, int]] = []
    if not isinstance(doc, dict):
        return out
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return out
    for jkey, jbody in jobs.items():
        if jkey == "__lines__":
            continue
        if not isinstance(jbody, dict):
            continue
        if "continue-on-error" not in jbody:
            continue
        v = jbody["continue-on-error"]
        if not _is_truthy_coe(v):
            continue
        line = jbody.get("__lines__", {}).get("continue-on-error")
        if not line:
            # PyYAML line-tracking shouldn't miss but guard for safety.
            # Fall back to grepping the raw text.
            line = _grep_first_coe_line(raw_lines, jkey) or 1
        out.append((str(jkey), int(line)))
    return out


def _grep_first_coe_line(raw_lines: list[str], jkey: str) -> int | None:
    """Fallback: find the first `continue-on-error:` line after a `jkey:` line."""
    saw_job = False
    for i, line in enumerate(raw_lines, start=1):
        if re.match(rf"^\s*{re.escape(jkey)}\s*:", line):
            saw_job = True
            continue
        if saw_job and "continue-on-error" in line:
            return i
    return None


# ---------------------------------------------------------------------------
# Scan window for tracker comment
# ---------------------------------------------------------------------------
WINDOW = 2  # lines above OR below the directive's line (inclusive)


def find_tracker_in_window(
    raw_lines: list[str], line_1based: int
) -> tuple[str, int] | None:
    """Return (slug, num) if a `# mc#NNN`/`# internal#NNN` appears
    in raw_lines within ±WINDOW lines of `line_1based`. None otherwise.

    We scan the directive's own line (it may carry an inline comment
    like `continue-on-error: true  # mc#3`) plus ±WINDOW.
    """
    lo = max(1, line_1based - WINDOW)
    hi = min(len(raw_lines), line_1based + WINDOW)
    for i in range(lo, hi + 1):
        line = raw_lines[i - 1]
        # Only the comment portion (after `#`) is considered, so
        # trailing-inline comments on the directive line are matched.
        m = TRACKER_RE.search(line)
        if m:
            return (m.group("slug"), int(m.group("num")))
    return None


# ---------------------------------------------------------------------------
# Tracker validation
# ---------------------------------------------------------------------------
def validate_tracker(
    slug: str, num: int, max_age_days: int
) -> tuple[bool, str]:
    """Return (ok?, reason). On 403, ok=True is returned with reason
    explaining graceful-degrade — caller treats 403 as a non-fatal
    skip (same as Tier 2a contract).
    """
    status, payload = fetch_issue(slug, num)
    if status == "forbidden":
        sys.stderr.write(
            f"::error::issue {slug}#{num} unreadable (HTTP 403 — token "
            f"scope). Cannot validate; skipping this check to avoid "
            f"red-X on every PR. Fix the token, not the lint.\n"
        )
        return (True, "forbidden — skipped")
    if status == "not_found":
        return (False, f"{slug}#{num} does not exist (404)")
    if status == "error":
        sys.stderr.write(
            f"::error::issue {slug}#{num} fetch errored — treating as "
            f"unverified, skipping this check.\n"
        )
        return (True, "fetch-error — skipped")

    assert payload is not None
    state = payload.get("state", "")
    if state != "open":
        return (False, f"{slug}#{num} state={state!r} (must be open)")

    created = payload.get("created_at", "")
    try:
        # Gitea returns ISO-8601 with timezone; Python 3.11+
        # fromisoformat handles `Z` suffix natively from 3.11. Older
        # runtimes need explicit replace.
        created_dt = datetime.fromisoformat(created.replace("Z", "+00:00"))
    except ValueError:
        return (False, f"{slug}#{num} created_at unparseable: {created!r}")

    age = datetime.now(timezone.utc) - created_dt
    # Inclusive boundary at MAX_AGE_DAYS: `age.days` truncates to a
    # whole-day floor, so an issue created 14d 0h 5m ago has
    # `age.days == 14` and passes; one created 15d 0h 0m ago has
    # `age.days == 15` and fails. This is the convention specified
    # in internal#350 ("≤14 days old").
    if age.days > max_age_days:
        return (
            False,
            f"{slug}#{num} is {age.days} days old (>{max_age_days}d cap). "
            f"Close-or-renew the tracker.",
        )
    return (True, f"{slug}#{num} open, {age.days}d old, ≤{max_age_days}d")


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------
def _iter_workflow_files(wf_dir: Path) -> list[Path]:
    return sorted(list(wf_dir.glob("*.yml")) + list(wf_dir.glob("*.yaml")))


def run() -> int:
    wf_dir = Path(_env("WORKFLOWS_DIR", ".gitea/workflows"))
    max_age = int(_env("MAX_AGE_DAYS", "14"))
    # Defaults for INTERNAL_REPO when unset (best-effort guess based on
    # the convention `mc#` = same repo, `internal#` = molecule-ai/internal).
    if not os.environ.get("INTERNAL_REPO"):
        os.environ["INTERNAL_REPO"] = "molecule-ai/internal"

    if not wf_dir.is_dir():
        sys.stderr.write(
            f"::error::workflows directory not found: {wf_dir}\n"
        )
        return 2

    yml_files = _iter_workflow_files(wf_dir)
    if not yml_files:
        print(f"::notice::no workflow files under {wf_dir}; nothing to lint.")
        return 0

    violations: list[str] = []
    notices: list[str] = []
    total_coe_true = 0

    for path in yml_files:
        raw = path.read_text(encoding="utf-8")
        raw_lines = raw.splitlines()
        try:
            doc = yaml.load(raw, Loader=_LineLoader)
        except yaml.YAMLError as e:
            sys.stderr.write(
                f"::error file={path}::YAML parse error: {e}. Skipping "
                f"this file (lint-workflow-yaml will catch separately).\n"
            )
            continue

        coe_locs = find_coe_truthies(doc, raw_lines)
        for jkey, line in coe_locs:
            total_coe_true += 1
            tracker = find_tracker_in_window(raw_lines, line)
            if tracker is None:
                violations.append(
                    f"::error file={path},line={line}::lint-continue-on-error-"
                    f"tracking (Tier 2e): job '{jkey}' has "
                    f"`continue-on-error: true` at line {line} with no "
                    f"`# mc#NNNN` or `# internal#NNNN` tracker comment "
                    f"within {WINDOW} lines. Add a tracker reference so "
                    f"this mask has a forced 14-day renewal cycle. "
                    f"Memory: feedback_chained_defects_in_never_tested_workflows."
                )
                continue
            slug, num = tracker
            ok, reason = validate_tracker(slug, num, max_age)
            if ok:
                notices.append(
                    f"::notice::{path.name} job '{jkey}' (line {line}): "
                    f"{reason}"
                )
            else:
                violations.append(
                    f"::error file={path},line={line}::lint-continue-on-error-"
                    f"tracking (Tier 2e): job '{jkey}' "
                    f"`continue-on-error: true` references {slug}#{num}, "
                    f"but {reason}. FIX: close/fix the underlying defect "
                    f"and flip continue-on-error: false, OR file a fresh "
                    f"tracker and update the comment."
                )

    for n in notices:
        print(n)

    if violations:
        print(
            f"::error::lint-continue-on-error-tracking: "
            f"{len(violations)} violation(s) across {len(yml_files)} "
            f"workflow file(s) (of {total_coe_true} `continue-on-error: "
            f"true` directives in total)."
        )
        for v in violations:
            print(v)
        return 1

    print(
        f"::notice::lint-continue-on-error-tracking: "
        f"all {total_coe_true} `continue-on-error: true` directive(s) "
        f"have valid trackers (open, ≤{max_age}d old)."
    )
    return 0


if __name__ == "__main__":
    sys.exit(run())
