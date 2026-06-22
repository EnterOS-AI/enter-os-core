#!/usr/bin/env python3
"""lint_no_coe_on_required — forbid continue-on-error on REQUIRED jobs.

Forbidden shape
---------------
A job in `.gitea/workflows/*.yml` that BOTH:
  - has `continue-on-error: true` (job-level), AND
  - emits a commit-status context that is in the repo's required
    branch-protection set.

`continue-on-error: true` makes a failed step roll up to a *success*
job status (Gitea Quirk #10). On a job whose context branch-protection
treats as REQUIRED, that converts a real failure into a green gate —
exactly the mc#1982 masking incident (continue-on-error on platform-build
hid regressions for ~3 weeks; SOP#765). This makes SOP#765 mechanical.

Required-context SSOT
---------------------
A checked-in allowlist (REQUIRED_CONTEXTS_FILE, default
.gitea/required-contexts.txt — one context per line, `#` comments). This
is authoritative because the CI token cannot always read
branch_protections (cp returns 403). When a token IS available
(GITEA_TOKEN + repo admin) the script ALSO live-reads branch_protections
and fails if the checked-in allowlist has drifted from live BP — but a
403/absent token degrades gracefully to allowlist-only (warn, don't fail
on the read).

Context derivation
------------------
Gitea emits the per-job status context as `"{workflow_name} / {job_name
or job_key}{suffix}"` where suffix is ` (pull_request)` / ` (push)` on
those events. The allowlist stores the bare `workflow / job` form; we
match a required context if its event-stripped form equals a job's
`workflow / job`.
"""
import os
import re
import sys

try:
    import yaml
except ImportError:
    print("FAIL: PyYAML not available", file=sys.stderr)
    sys.exit(2)

WORKFLOWS_DIR = os.environ.get("WORKFLOWS_DIR", ".gitea/workflows")
REQUIRED_FILE = os.environ.get("REQUIRED_CONTEXTS_FILE", ".gitea/required-contexts.txt")
GITEA_TOKEN = os.environ.get("GITEA_TOKEN", "")
GITEA_HOST = os.environ.get("GITEA_HOST", "git.moleculesai.app")
REPO = os.environ.get("REPO", "")

EVENT_SUFFIX = re.compile(r"\s*\((pull_request|push|pull_request_target)\)\s*$")


def strip_event(ctx):
    return EVENT_SUFFIX.sub("", ctx).strip()


def load_required_allowlist(path):
    if not os.path.isfile(path):
        return None
    out = set()
    with open(path) as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                out.add(strip_event(line))
    return out


def job_contexts(workflows_dir):
    """Return dict context -> (file, job_key, continue_on_error_bool)."""
    contexts = {}
    for fn in sorted(os.listdir(workflows_dir)):
        if not (fn.endswith(".yml") or fn.endswith(".yaml")):
            continue
        path = os.path.join(workflows_dir, fn)
        try:
            with open(path) as f:
                doc = yaml.safe_load(f)
        except yaml.YAMLError:
            continue
        if not isinstance(doc, dict):
            continue
        wf_name = doc.get("name") or os.path.splitext(fn)[0]
        jobs = doc.get("jobs") or {}
        if not isinstance(jobs, dict):
            continue
        for jkey, jval in jobs.items():
            if not isinstance(jval, dict):
                continue
            jname = jval.get("name") or jkey
            coe = jval.get("continue-on-error", False)
            # Gitea coerces string "true" truthy.
            coe_bool = coe is True or (isinstance(coe, str) and coe.strip().lower() == "true")
            ctx = f"{wf_name} / {jname}"
            contexts[strip_event(ctx)] = (path, jkey, coe_bool)
    return contexts


def live_required_contexts():
    """Best-effort live BP read. Returns set or None (degrade)."""
    if not (GITEA_TOKEN and REPO):
        return None
    try:
        import json
        import urllib.request
        url = f"https://{GITEA_HOST}/api/v1/repos/{REPO}/branch_protections"
        req = urllib.request.Request(url, headers={"Authorization": f"token {GITEA_TOKEN}"})
        with urllib.request.urlopen(req, timeout=20) as r:
            data = json.load(r)
        out = set()
        for b in data:
            if b.get("branch_name") in ("main", None):
                for c in (b.get("status_check_contexts") or []):
                    out.add(strip_event(c))
        return out
    except Exception as e:
        print(f"::warning:: live branch_protections read failed ({e}); using checked-in allowlist only")
        return None


def main():
    if not os.path.isdir(WORKFLOWS_DIR):
        print(f"OK: no {WORKFLOWS_DIR}")
        return 0
    required = load_required_allowlist(REQUIRED_FILE)
    if required is None:
        print(f"FAIL: required-contexts allowlist {REQUIRED_FILE} is missing — "
              f"this file is the SSOT for which contexts are merge-required.")
        return 1

    # Optional live-BP drift check (graceful).
    live = live_required_contexts()
    if live is not None:
        only_live = live - required
        if only_live:
            print("FAIL: branch-protection required contexts NOT in the checked-in allowlist "
                  f"({REQUIRED_FILE}) — allowlist has drifted from live BP:")
            for c in sorted(only_live):
                print(f"  - {c}")
            print("  Add them to the allowlist (or remove from BP).")
            return 1

    ctxs = job_contexts(WORKFLOWS_DIR)
    fails = []
    for ctx in sorted(required):
        info = ctxs.get(ctx)
        if info is None:
            # The context is required but no job currently emits it — that's
            # a different lint's concern (required-context-exists). Skip.
            continue
        path, jkey, coe = info
        if coe:
            fails.append(f"{path}: job `{jkey}` (context `{ctx}`) is branch-protection REQUIRED "
                         f"but has continue-on-error: true")
    if fails:
        print("FAIL: continue-on-error: true on a REQUIRED branch-protection job (mc#1982 / SOP#765):")
        for f in fails:
            print(f"  - {f}")
        print()
        print("Why: continue-on-error makes a failed step roll up to a SUCCESS")
        print("  job status (Gitea Quirk #10). On a REQUIRED context that turns")
        print("  a real failure into a green gate — the mc#1982 masking incident.")
        print("  Remove continue-on-error from required jobs (SOP#765).")
        return 1
    print(f"OK: no continue-on-error on any of the {len(required)} required contexts.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
