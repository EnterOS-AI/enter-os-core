"""Pattern-based secret redaction for adapter exception strings.

Used by ``not_configured_handler`` (and any future code path that exposes
adapter-side error strings to the network) to scrub secret-shaped tokens
before they land in JSON-RPC ``error.data``.

Why this exists (issue molecule-core#2760): PR #2756 piped
``adapter.setup()`` exception strings verbatim into the JSON-RPC -32603
response so canvas could surface "agent not configured: <reason>". The
4 adapters in tree today (claude-code/codex/openclaw/hermes) raise with
key NAMES not values, so this is currently safe — but a future adapter
author writing ``raise RuntimeError(f"auth failed for {token}")`` would
leak that token to every JSON-RPC client. This module is the structural
floor that keeps the leak from happening.

The redactor is intentionally pattern-based (a closed list of known
prefixes), NOT entropy-based — entropy heuristics false-positive on
hex git SHAs and base64-shaped UUIDs that carry zero secret value.
A pattern miss is preferable to redacting "RuntimeError: invalid
config_path=ed8f1234abcd" out of a real diagnostic.

Pairs with ``not_configured_handler.make_not_configured_handler`` —
the redactor runs once when the handler is built, so per-request hot
path stays unchanged.
"""
from __future__ import annotations

import re

# Closed list of known secret-shaped prefixes / formats. Each entry is a
# compiled regex with one or more capture groups; the redactor replaces
# the whole match with REDACTION_PLACEHOLDER. The entries are roughly
# ordered by frequency in our adapter exception strings — Anthropic /
# OpenAI / OpenRouter style tokens come first.
#
# Matched on token-ISH boundaries (start/end of string, whitespace, or
# common separators like : / = ( ) " ' ,). Avoids redacting ``sk`` in
# the middle of unrelated text like "task_sk_id" while still catching
# ``sk-ant-...`` / ``sk-cp-...`` / ``sk-or-...``.
_TOKEN_BOUNDARY_LEFT = r"(?:^|[\s\(\)\[\]\{\}\"'=,:/])"
_TOKEN_BOUNDARY_RIGHT = r"(?=$|[\s\(\)\[\]\{\}\"'=,:/])"

REDACTION_PLACEHOLDER = "<redacted-secret>"

_PATTERNS = [
    # Anthropic / OpenAI / OpenRouter / Stripe / proprietary `sk-` family.
    # Token format: `sk-` then any non-whitespace run. Length 16+ to avoid
    # false-matching on `sk-test` style placeholders shorter than a real
    # key (16 covers OpenAI's shortest legacy key length).
    re.compile(
        _TOKEN_BOUNDARY_LEFT + r"(sk-[A-Za-z0-9_\-]{16,})" + _TOKEN_BOUNDARY_RIGHT
    ),
    # GitHub Personal Access Tokens (classic + fine-grained + OAuth + app).
    # Format: ghp_ / gho_ / ghu_ / ghs_ / ghr_ followed by ~36 chars.
    re.compile(
        _TOKEN_BOUNDARY_LEFT + r"(gh[pousr]_[A-Za-z0-9]{20,})" + _TOKEN_BOUNDARY_RIGHT
    ),
    # AWS access key id — fixed 16-char prefix `AKIA` (or `ASIA` for
    # session creds) followed by 16 alphanumeric chars (20 total).
    re.compile(
        _TOKEN_BOUNDARY_LEFT + r"((?:AKIA|ASIA)[0-9A-Z]{16})" + _TOKEN_BOUNDARY_RIGHT
    ),
    # Bearer prefix common in HTTP error strings: `Bearer <token>`.
    # The match captures the literal `Bearer ` plus the token so the
    # full leak (which includes the prefix in some adapter error
    # messages) is scrubbed in one go.
    re.compile(r"(Bearer\s+[A-Za-z0-9_\-\.=]{16,})"),
    # Slack / Hugging Face / generic `xoxb-`, `xoxp-`, `xoxa-` prefixes.
    re.compile(
        _TOKEN_BOUNDARY_LEFT + r"(xox[bpars]-[A-Za-z0-9\-]{10,})" + _TOKEN_BOUNDARY_RIGHT
    ),
    # Hugging Face API tokens: `hf_` followed by ~37 chars.
    re.compile(
        _TOKEN_BOUNDARY_LEFT + r"(hf_[A-Za-z0-9]{20,})" + _TOKEN_BOUNDARY_RIGHT
    ),
    # Generic JWT — three base64url segments separated by dots. JWTs
    # carry signed claims that often include user identifiers; even a
    # public-key-only JWT shouldn't end up in an error.data field that
    # gets logged / echoed back to clients.
    re.compile(
        _TOKEN_BOUNDARY_LEFT + r"(eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,})" + _TOKEN_BOUNDARY_RIGHT
    ),
]


def redact_secrets(text: str) -> str:
    """Return ``text`` with any secret-shaped substrings replaced by
    ``REDACTION_PLACEHOLDER``.

    Empty / None input returns the input unchanged so callers can pass
    through ``adapter_error`` even when it's None.

    The redactor operates on the WHOLE string, not line-by-line, so a
    multi-line traceback with a token on line 3 still gets scrubbed.
    Multiple distinct tokens in the same string are all redacted; the
    placeholder appears once per match.

    Trade-off: pattern-based redaction misses tokens whose prefix isn't
    in ``_PATTERNS``. The cost of a miss is a leak; the cost of going
    pattern-free (e.g., entropy heuristic) is false-positive redaction
    of git SHAs and UUIDs in legitimate diagnostics. We choose miss-on-
    unknown-prefix and rely on ``_PATTERNS`` growing over time as we
    catch new providers. Adapter PRs that introduce a new provider
    SHOULD add the provider's token prefix here.
    """
    if not text:
        return text
    out = text
    for pat in _PATTERNS:
        out = pat.sub(
            # Preserve the leading boundary char (group 0 minus the
            # token capture) so substitution doesn't eat surrounding
            # punctuation. Achieved by re-emitting the leading
            # boundary then the placeholder. Patterns that don't have
            # a left-boundary group (Bearer) just emit the placeholder.
            _make_replacer(pat),
            out,
        )
    return out


def _make_replacer(pat: re.Pattern) -> "callable":
    """Build a sub() replacer that preserves any boundary char captured
    by ``pat`` before the secret-shaped group.

    Patterns built with ``_TOKEN_BOUNDARY_LEFT`` produce a non-capturing
    group for the boundary. Match.group(0) is the full match including
    that boundary; group(1) is just the secret. We replace group(1)
    with the placeholder, leaving group(0) minus group(1) intact.
    """
    def _repl(m: re.Match) -> str:
        full = m.group(0)
        secret = m.group(1)
        # Position of the secret within the full match.
        idx = full.find(secret)
        if idx < 0:
            return REDACTION_PLACEHOLDER
        return full[:idx] + REDACTION_PLACEHOLDER + full[idx + len(secret):]
    return _repl
