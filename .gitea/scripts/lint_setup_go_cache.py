#!/usr/bin/env python3
"""lint_setup_go_cache — forbid actions/setup-go cache on self-hosted runners.

Forbidden shape
---------------
Any `uses: actions/setup-go@...` step that enables actions/cache —
either `cache: true` explicitly OR the default-true case (a `cache-key`
/ `cache-dependency-path` set with NO `cache: false`). setup-go's
`cache` input DEFAULTS to true, so omitting it is also forbidden once
any cache-* input is present, and a bare setup-go with neither is
treated as default-true and flagged too (belt-and-braces: on our
self-hosted fleet the only safe value is explicit `cache: false`).

Why
---
The molecule self-hosted runners bind-mount a persistent, host-shared
GOCACHE/GOMODCACHE (/var/cache/ci-go-{build,mod}, see
operator-config ops/runners/config.dedicated.yaml). actions/cache
(which setup-go drives when cache:true) untars its restored archive
OVER that bind mount -> "File exists" -> "Failed to restore" ->
partial cache -> downstream linker/typecheck failures on heavy jobs
(test -race link "too many errors", go-arch-lint "without types").
The runner-level GOCACHE is the SSOT for caching; setup-go must not
also cache. Fix: add `cache: false` under the setup-go `with:`.

Empirical: 2026-06-09/10 cross-repo rollout; sweep PRs
fix/setup-go-cache-vs-bind-mount (core#2524, cli#16). This guard
PREVENTS regression after those land.

Detection is line-based (not full YAML) so it can attribute a precise
file:line and survives Gitea's ${{ }} expressions that confuse some
YAML loaders. We locate each setup-go step, then read the contiguous
`with:` block that follows it (same or deeper indent, up to the next
step `- ` at the step indent).
"""
import os
import re
import sys

WORKFLOWS_DIR = os.environ.get("WORKFLOWS_DIR", ".gitea/workflows")

SETUP_GO = re.compile(r'^(\s*)(?:-\s+)?uses:\s*actions/setup-go@', re.I)
# step boundary: a list item `- ` at an indent <= the step's own indent
STEP_ITEM = re.compile(r'^(\s*)-\s+\S')
CACHE_LINE = re.compile(r'^\s*cache:\s*(\S+)')
CACHE_DEP = re.compile(r'^\s*cache-(dependency-path|key):')
WITH_LINE = re.compile(r'^\s*with:\s*$')


def step_indent(line):
    m = re.match(r'^(\s*)', line)
    return len(m.group(1))


def scan_file(path):
    """Return list of (lineno, reason) violations."""
    with open(path) as f:
        lines = f.readlines()
    viols = []
    i = 0
    n = len(lines)
    while i < n:
        m = SETUP_GO.match(lines[i])
        if not m:
            i += 1
            continue
        go_line = i + 1
        # Indent of the `uses:` key. The step's `with:` block lives at
        # the same key indent (siblings under the same `- ` list item).
        uses_indent = step_indent(lines[i])
        # Collect the block belonging to this step: subsequent lines that
        # are more-indented than the step list marker, stopping at the
        # next `- ` item whose indent <= the list-marker indent.
        # The list marker indent is uses_indent if `- uses:` inline,
        # else uses_indent-2 (key under a `- `). Normalize to the marker.
        # Simpler: gather until a `- ` item at indent < uses_indent, or
        # indent == uses_indent for the `- uses:` inline form.
        inline_dash = bool(re.match(r'^\s*-\s+uses:', lines[i]))
        marker_indent = uses_indent if inline_dash else uses_indent - 2
        cache_val = None
        has_cache_dep = False
        j = i + 1
        while j < n:
            ln = lines[j]
            if ln.strip() == "" or ln.lstrip().startswith("#"):
                j += 1
                continue
            sm = STEP_ITEM.match(ln)
            if sm and step_indent(ln) <= marker_indent:
                break  # next step
            # also stop if we dedented out of this step entirely
            if step_indent(ln) <= marker_indent and not WITH_LINE.match(ln):
                break
            cm = CACHE_LINE.match(ln)
            if cm:
                cache_val = cm.group(1).strip().strip('"\'').lower()
            if CACHE_DEP.match(ln):
                has_cache_dep = True
            j += 1
        # Decide
        if cache_val == "true":
            viols.append((go_line, "cache: true (must be `cache: false`)"))
        elif cache_val is None:
            # default-true. Flag — explicit cache:false is required on
            # the self-hosted fleet. Strongest with cache-dep present,
            # but bare setup-go is also default-true so flag both.
            if has_cache_dep:
                viols.append((go_line, "cache-dependency-path/key set with no `cache:` (defaults to true)"))
            else:
                viols.append((go_line, "no `cache:` set (defaults to true; require explicit `cache: false`)"))
        # cache_val == "false" -> OK
        i = j
    return viols


def main():
    if not os.path.isdir(WORKFLOWS_DIR):
        print(f"OK: no {WORKFLOWS_DIR} directory")
        return 0
    all_viols = []
    for fn in sorted(os.listdir(WORKFLOWS_DIR)):
        if not (fn.endswith(".yml") or fn.endswith(".yaml")):
            continue
        path = os.path.join(WORKFLOWS_DIR, fn)
        for lineno, reason in scan_file(path):
            all_viols.append(f"{path}:{lineno}: actions/setup-go with caching enabled — {reason}")
    if all_viols:
        print("FAIL: actions/setup-go must set `cache: false` on the self-hosted fleet:")
        for v in all_viols:
            print(f"  - {v}")
        print()
        print("Why: runners bind-mount a host-shared GOCACHE/GOMODCACHE")
        print("  (/var/cache/ci-go-{build,mod}, operator-config")
        print("  ops/runners/config.dedicated.yaml). actions/cache untars OVER")
        print("  the bind mount -> 'File exists' -> partial cache -> race-link")
        print("  / arch-lint failures. The runner-level GOCACHE is the cache SSOT.")
        print("  Fix: add `cache: false` under the setup-go `with:` block.")
        return 1
    print("OK: every actions/setup-go step sets cache: false.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
