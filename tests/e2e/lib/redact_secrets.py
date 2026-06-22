#!/usr/bin/env python3
"""Redact credential-looking values from stdin to stdout.

Used by staging E2E diagnostic emitters so run logs stay secret-safe.
Preserves HTTP status codes and non-secret error context.
"""
import re
import sys


def redact(text: str) -> str:
    patterns = [
        # Authorization / Bearer header values (consume the whole value)
        (r"(?i)(Authorization\s*[:=]\s*)[^\n]*", r"\1<REDACTED>"),
        # Bare Bearer token (e.g. in bodies or query values)
        (r"(?i)(Bearer\s+)[A-Za-z0-9_\-./=]+", r"\1<REDACTED>"),
        # Known credential key names (JSON/YAML/env-style)
        (
            r"(?i)(\"?(?:ANTHROPIC_AUTH_TOKEN|ANTHROPIC_API_KEY|MINIMAX_API_KEY|OPENAI_API_KEY|CLIENT_SECRET|ACCESS_TOKEN|GITHUB_TOKEN|GITEA_TOKEN|MOLECULE_[A-Z_]*_(?:TOKEN|SECRET|KEY))\"?\s*[:=]\s*\"?)[^\"\s,}\]]+",
            r"\1<REDACTED>",
        ),
        # Generic *_API_KEY / *_TOKEN / *_SECRET / *_AUTH_TOKEN / *_PASSWORD
        (
            r"(?i)(\"?[A-Z_]*(?:API_KEY|AUTH_TOKEN|TOKEN|SECRET|PASSWORD)\"?\s*[:=]\s*\"?)[^\"\s,}\]]+",
            r"\1<REDACTED>",
        ),
        # URL query params that commonly carry credentials
        (
            r"(?i)([?&](?:token|api[_-]?key|auth|secret|client_secret|access_token|password)=)[^&\s\"\']+",
            r"\1<REDACTED>",
        ),
    ]
    for pat, repl in patterns:
        text = re.sub(pat, repl, text)
    return text


if __name__ == "__main__":
    sys.stdout.write(redact(sys.stdin.read()))
