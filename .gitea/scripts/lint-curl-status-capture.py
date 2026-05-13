#!/usr/bin/env python3
"""Lint workflow bash for curl status-code capture pollution.

The bad shape is:

    HTTP_CODE=$(curl ... -w '%{http_code}' ... || echo "000")

`curl -w` writes the HTTP code to stdout before returning non-zero, so
fallback output inside the same command substitution appends another code.
"""
from __future__ import annotations

import argparse
import glob
import re
import sys
from pathlib import Path
from typing import NamedTuple


SELF = ".gitea/workflows/lint-curl-status-capture.yml"


class Finding(NamedTuple):
    path: str
    snippet: str


BAD_STATUS_CAPTURE = re.compile(
    r"""
    \$\(\s*
    curl\b
    [^)]*
    -w\s*['"]%\{http_code\}['"]
    [^)]*
    \|\|\s*
    (?:
        echo\s+['"]?000['"]?
        |
        printf\s+['"]000['"]
    )
    \s*\)
    """,
    re.DOTALL | re.VERBOSE,
)


def _logical_shell(content: str) -> str:
    """Collapse bash line continuations so one curl command is one string."""
    return re.sub(r"\\\s*\n\s*", " ", content)


def scan_content(path: str, content: str) -> list[Finding]:
    flat = _logical_shell(content)
    return [
        Finding(path=path, snippet=re.sub(r"\s+", " ", match.group(0)).strip()[:160])
        for match in BAD_STATUS_CAPTURE.finditer(flat)
    ]


def scan_paths(paths: list[str]) -> list[Finding]:
    findings: list[Finding] = []
    for path in paths:
        if path == SELF:
            continue
        content = Path(path).read_text(encoding="utf-8")
        findings.extend(scan_content(path, content))
    return findings


def default_paths() -> list[str]:
    return sorted(glob.glob(".gitea/workflows/*.yml"))


def print_report(findings: list[Finding]) -> None:
    if not findings:
        print("OK No curl-status-capture pollution patterns detected")
        return

    print(f"::error::Found {len(findings)} curl-status-capture pollution site(s):")
    for finding in findings:
        print(
            f"::error file={finding.path}::Curl status-capture pollution: "
            "'|| echo/printf 000' inside a $(curl ... -w '%{http_code}' ...) "
            "subshell. On non-2xx or connection failure, curl's -w writes a "
            "status, then exits non-zero, then the fallback appends another "
            "status. Fix: route -w into a tempfile so the exit code cannot "
            "pollute stdout."
        )
        print(f"   matched: {finding.snippet}...")

    print()
    print("Fix template:")
    print("  set +e")
    print("  curl ... -w '%{http_code}' >code.txt 2>/dev/null")
    print("  set -e")
    print('  HTTP_CODE=$(cat code.txt 2>/dev/null)')
    print('  [ -z "$HTTP_CODE" ] && HTTP_CODE="000"')


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("paths", nargs="*", help="workflow files to scan")
    args = parser.parse_args(argv)

    paths = args.paths or default_paths()
    findings = scan_paths(paths)
    print_report(findings)
    return 1 if findings else 0


if __name__ == "__main__":
    raise SystemExit(main())
