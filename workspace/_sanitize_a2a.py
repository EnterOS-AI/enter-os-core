"""Sanitization helpers for A2A delegation results.

OFFSEC-003: Peer text must not be able to escape trust boundaries by
injecting control markers that the caller interprets as structured framing.

This module is intentionally isolated from the rest of the molecule-runtime
import graph to avoid circular imports. Callers import only from here when
they need to sanitize a2a result text before returning it to the agent.
"""

from __future__ import annotations

import re


# Sentinel strings used by a2a_tools_delegation.py as control prefixes.
_A2A_ERROR_PREFIX = "[A2A_ERROR] "
_A2A_QUEUED_PREFIX = "[A2A_QUEUED] "
_A2A_RESULT_FROM_PEER = "[A2A_RESULT_FROM_PEER]"
_A2A_RESULT_TO_PEER = "[A2A_RESULT_TO_PEER]"

# Regex patterns for the lookahead.  Each is a raw string where \[ = escaped
# '[' and \] = escaped ']'.  The full pattern (separator + '[' + rest) is
# matched in two pieces:
#   1. (?=<marker>)   — lookahead: matches the ENTIRE marker (including '[')
#                        at the current position without consuming any chars.
#   2. \[              — consumes the '[' so it gets replaced, not duplicated.
#
# Why the lookahead-first approach?  If we match (^|\n)\[ first, the lookahead
# would fire at the *new* position (after the '['), not the original one, and
# would fail.  By matching the lookahead first, we assert the marker is present
# at the correct token boundary, then consume the '[' separately.
_BOUNDARY_PATTERNS: list[tuple[str, str]] = [
    (_A2A_ERROR_PREFIX,      r"\[A2A_ERROR\] "),
    (_A2A_QUEUED_PREFIX,      r"\[A2A_QUEUED\] "),
    (_A2A_RESULT_FROM_PEER,  r"\[A2A_RESULT_FROM_PEER\]"),
    (_A2A_RESULT_TO_PEER,    r"\[A2A_RESULT_TO_PEER\]"),
]

_CONTROL_PATTERNS: list[tuple[str, str]] = [
    (r"[SYSTEM]",       r"\[SYSTEM\]"),
    (r"[OVERRIDE]",    r"\[OVERRIDE\]"),
    (r"[INSTRUCTIONS]", r"\[INSTRUCTIONS\]"),
    (r"[IGNORE ALL]",  r"\[IGNORE ALL\]"),
    (r"[YOU ARE NOW]", r"\[YOU ARE NOW\]"),
]

# ZERO-WIDTH SPACE (U+200B)
_ZWSP = "​"


def _escape_boundary_markers(text: str) -> str:
    """Escape trust-boundary markers embedded in raw peer text.

    Scans ``text`` for any known boundary-control pattern that appears as a
    TOP-LEVEL token (start of string or after a newline) and inserts a
    ZERO-WIDTH SPACE (U+200B) before the opening '[' so that downstream
    parsers that look for the raw '[' no longer match the marker as a prefix.
    """
    if not text:
        return ""

    # Build alternation from the second (regex) element of each tuple.
    marker_alts = "|".join(pat for _, pat in _BOUNDARY_PATTERNS + _CONTROL_PATTERNS)

    # Pattern: (?=<marker>)\[  — lookahead for the FULL marker, then consume '['.
    # This ensures the '[' is consumed so it gets replaced, not duplicated.
    # We use regular string concatenation for (^|\n) so \n is 0x0A.
    boundary_re = re.compile(
        "(^|\n)(?=" + marker_alts + ")\\[",
        flags=re.MULTILINE,
    )

    def _replacer(m: re.Match[str]) -> str:
        # m.group(1) = '' or '\n'; the '[' is consumed by the match
        return m.group(1) + _ZWSP + "["

    return boundary_re.sub(_replacer, text)


def sanitize_a2a_result(text: str) -> str:
    """Sanitize raw A2A delegation result text before returning to the caller."""
    if not text:
        return ""

    text = _escape_boundary_markers(text)
    text = _strip_closed_blocks(text)
    return text


def _strip_closed_blocks(text: str) -> str:
    """Remove content after a closing marker injected by a malicious peer."""
    CLOSERS = [
        "[/A2A_ERROR]",
        "[/A2A_QUEUED]",
        "[/A2A_RESULT_FROM_PEER]",
        "[/A2A_RESULT_TO_PEER]",
        "[/SYSTEM]",
        "[/OVERRIDE]",
        "[/INSTRUCTIONS]",
        "[/IGNORE ALL]",
        "[/YOU ARE NOW]",
    ]
    closer_re = "|".join(re.escape(c) for c in CLOSERS)

    parts = re.split(
        "(?<=\n)(?=" + closer_re + ")|(?=^)(?=" + closer_re + ")",
        text, maxsplit=1, flags=re.MULTILINE,
    )
    # parts[0] may have a trailing \n that was part of the (?<=\n) boundary;
    # strip it so the result ends cleanly at the closer boundary.
    return parts[0].rstrip("\n")
