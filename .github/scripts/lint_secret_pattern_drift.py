#!/usr/bin/env python3
"""Lint SECRET_PATTERNS drift across known consumers of molecule-core's canonical.

The canonical SECRET_PATTERNS array in
.github/workflows/secret-scan.yml is mirrored by every other side
that scans for credentials: the workspace-runtime's bundled
pre-commit hook, the molecule-controlplane inlined copy, etc. The
mirror is enforced socially today — when someone adds a new pattern
to canonical (e.g. the sk-cp- MiniMax token after F1088), the other
sides are supposed to be updated in lockstep.

This script automates the check. Diffs the canonical's pattern set
against each known public consumer and exits non-zero on any
mismatch. Wired into a daily cron + on-push gate via
.github/workflows/secret-pattern-drift.yml.

Private-repo consumers (currently molecule-controlplane's inlined
copy) are out of scope here because the molecule-core workflow's
GITHUB_TOKEN can't read other private repos in the org. They're
expected to self-monitor via their own copy of this script — not a
hard barrier, just a future expansion.
"""

from __future__ import annotations

import re
import sys
import urllib.request
from pathlib import Path

CANONICAL_FILE = Path(".github/workflows/secret-scan.yml")

# Public consumer mirrors. Each entry is (label, raw_url) — raw_url
# points at the file's RAW content on the consumer's default branch
# (or staging where applicable). Add an entry here when a new public
# repo starts shipping its own SECRET_PATTERNS array.
CONSUMERS: list[tuple[str, str]] = [
    (
        "molecule-ai-workspace-runtime/molecule_runtime/scripts/pre-commit-checks.sh",
        "https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime/raw/branch/main/molecule_runtime/scripts/pre-commit-checks.sh",
    ),
]

# In-repo consumers — paths read locally from the workflow checkout.
# Read-from-disk avoids the staging→main lag that the URL fetcher
# would hit (a freshly-edited canonical wouldn't yet be on the
# consumer's default branch). Same drift semantics, no network.
LOCAL_CONSUMERS: list[tuple[str, Path]] = [
    (
        ".githooks/pre-commit (molecule-core local hook)",
        Path(".githooks/pre-commit"),
    ),
]

# Matches the SECRET_PATTERNS=( ... ) array in either yaml-indented
# (the canonical workflow's `run:` block) or shell-flat (runtime
# hook) format. Patterns inside are single-quoted Bash strings; we
# pull each via _PATTERN_RE.
#
# Closing `)` is anchored to the start of a line (possibly indented)
# because pattern comments like `# GitHub PAT (classic)` contain
# their own `)` mid-line — a non-anchored regex would match through
# the comment's paren and capture only the first pattern.
_ARRAY_RE = re.compile(r"SECRET_PATTERNS=\((.*?)^\s*\)", re.DOTALL | re.MULTILINE)
_PATTERN_RE = re.compile(r"'([^']+)'")


def extract_patterns(content: str, source_label: str) -> list[str]:
    """Pull the SECRET_PATTERNS list out of either format. Raises if missing."""
    m = _ARRAY_RE.search(content)
    if not m:
        raise SystemExit(f"::error::{source_label}: SECRET_PATTERNS=(...) array not found")
    return _PATTERN_RE.findall(m.group(1))


def fetch(url: str) -> str:
    req = urllib.request.Request(
        url, headers={"User-Agent": "secret-pattern-drift-lint/1"}
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        return resp.read().decode("utf-8")


def diff_patterns(canonical: list[str], consumer: list[str]) -> tuple[list[str], list[str]]:
    """Return (missing_from_consumer, extra_in_consumer) — both sorted."""
    canonical_set = set(canonical)
    consumer_set = set(consumer)
    return (
        sorted(canonical_set - consumer_set),
        sorted(consumer_set - canonical_set),
    )


def main() -> int:
    if not CANONICAL_FILE.exists():
        print(f"::error::canonical not found at {CANONICAL_FILE}")
        return 1

    canonical = extract_patterns(CANONICAL_FILE.read_text(), str(CANONICAL_FILE))
    print(f"canonical ({CANONICAL_FILE}): {len(canonical)} patterns")

    drift = False

    # In-repo consumers first — these are read from the workflow's own
    # checkout, so they never lag behind the canonical and a missing
    # file IS a real error (not a fetch warning).
    for label, path in LOCAL_CONSUMERS:
        if not path.exists():
            print(f"::error::{label}: file not found at {path}")
            drift = True
            continue
        consumer = extract_patterns(path.read_text(), label)
        missing, extra = diff_patterns(canonical, consumer)
        if not missing and not extra:
            print(f"  ✓ {label}: aligned ({len(consumer)} patterns)")
            continue
        drift = True
        print(f"::error::DRIFT in {label}:")
        for p in missing:
            print(f"  -  missing from consumer: {p!r}")
        for p in extra:
            print(f"  -  extra in consumer (not in canonical): {p!r}")

    for label, url in CONSUMERS:
        try:
            content = fetch(url)
        except Exception as e:
            # Fetch failures are warnings, not errors. A consumer
            # whose default branch was just renamed (or whose file
            # moved) shouldn't fail the lint until someone updates
            # the URL above. Real drift is the failure mode this
            # gate exists to catch — fetch reliability isn't.
            print(f"::warning::{label}: fetch failed ({e}) — skipping")
            continue

        consumer = extract_patterns(content, label)
        missing, extra = diff_patterns(canonical, consumer)
        if not missing and not extra:
            print(f"  ✓ {label}: aligned ({len(consumer)} patterns)")
            continue

        drift = True
        print(f"::error::DRIFT in {label}:")
        for p in missing:
            print(f"  -  missing from consumer: {p!r}")
        for p in extra:
            print(f"  -  extra in consumer (not in canonical): {p!r}")

    if drift:
        print()
        print("::error::SECRET_PATTERNS drift detected. Bring consumer(s) into")
        print("alignment with the canonical SECRET_PATTERNS array in")
        print(f"{CANONICAL_FILE} by adding the missing patterns and removing")
        print("any extras. The two sides must stay byte-aligned on the pattern")
        print("list — the runtime hook is the developer's local pre-commit,")
        print("the canonical is the org-wide CI gate, divergence means a token")
        print("can pass one but get rejected by the other.")
        return 1

    print()
    print("✓ All known consumers aligned with canonical SECRET_PATTERNS.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
