"""Decision logic extracted from sweep-cf-orphans.sh for unit testing (#2027).

The bash script embeds the same logic inline as a python heredoc — this
module is a verbatim copy used by test_sweep_cf_decide.py. The parity
test (TestParityWithBashScript) reads the bash script and asserts the
canonical block in this file is present byte-for-byte, so the two
cannot drift apart silently.

If you change the rules: edit BOTH this file AND the inline block in
``scripts/ops/sweep-cf-orphans.sh`` (the canonical block runs from
``# CANONICAL DECIDE BEGIN`` to ``# CANONICAL DECIDE END`` markers in
both files; the parity check compares those slices).

Inputs to ``decide(record, all_slugs, ec2_names)``:
  record       Cloudflare DNS record dict {name, id, type}
  all_slugs    set of CP org slugs (prod ∪ staging) — caller computes the
               union once instead of per-record (decide is hot-path: 100s
               to 1000s of records per sweep)
  ec2_names    set of live EC2 Name tags (e.g. ``ws-d3605ef2-f7d``)

Returns ``(action, reason, id, name, type)`` matching the bash heredoc.
"""
from __future__ import annotations

import re


# Pre-compile per-record regexes once at module load — saves the per-call
# pattern-cache lookup across 1000s of CF records per sweep. Mirrored at
# the same scope in sweep-cf-orphans.sh's heredoc.
_PLATFORM_CORE_NAMES = {
    "api.moleculesai.app", "app.moleculesai.app", "doc.moleculesai.app",
    "send.moleculesai.app", "status.moleculesai.app", "www.moleculesai.app",
    "staging-api.moleculesai.app",
}
_WS_RE = re.compile(r"^(ws-[a-f0-9]{8}-[a-f0-9]+)(?:\.staging)?\.moleculesai\.app$")
_E2E_RE = re.compile(r"^(e2e-[^.]+)(?:\.staging)?\.moleculesai\.app$")
_TENANT_RE = re.compile(r"^([a-z0-9][a-z0-9-]*)(?:\.staging)?\.moleculesai\.app$")


# CANONICAL DECIDE BEGIN
def decide(r, all_slugs, ec2_names):
    n = r["name"]
    rid = r["id"]
    typ = r["type"]

    if n == "moleculesai.app":
        return ("keep", "apex", rid, n, typ)
    if n.startswith("_") or n.endswith("._domainkey.moleculesai.app"):
        return ("keep", "verification/key", rid, n, typ)
    if n in _PLATFORM_CORE_NAMES:
        return ("keep", "platform-core", rid, n, typ)

    m = _WS_RE.match(n)
    if m:
        prefix = m.group(1)
        # Live EC2 names share the ws-<hex8>-<rest> shape with the DNS subdomain.
        for ename in ec2_names:
            if ename.startswith(prefix):
                return ("keep", "live-ec2", rid, n, typ)
        return ("delete", "orphan-ws", rid, n, typ)

    m = _E2E_RE.match(n)
    if m:
        slug = m.group(1)
        if slug in all_slugs:
            return ("keep", "live-e2e-tenant", rid, n, typ)
        return ("delete", "orphan-e2e-tenant", rid, n, typ)

    m = _TENANT_RE.match(n)
    if m:
        slug = m.group(1)
        if slug in all_slugs:
            return ("keep", "live-tenant", rid, n, typ)
        # KEEP unknown tenant-shaped names — avoid false-positive nukes on
        # ad-hoc records (e.g. hermes-final-*) that do not match a known slug.
        return ("keep", "unknown-subdomain-kept-for-safety", rid, n, typ)

    return ("keep", "not-a-pattern-we-sweep", rid, n, typ)
# CANONICAL DECIDE END


def safety_gate(total: int, delete_count: int, max_delete_pct: int = 50) -> bool:
    """Return True iff the sweep is safe to execute.

    Mirrors the shell-side gate: if the deletion fraction exceeds
    ``max_delete_pct`` the sweep refuses to run. Same integer arithmetic
    as the bash script (``DELETE_COUNT*100/TOTAL``) so a future threshold
    tweak only needs to land in one semantic place.
    """
    if total <= 0:
        return True
    pct = delete_count * 100 // total
    return pct <= max_delete_pct
