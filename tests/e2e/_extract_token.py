#!/usr/bin/env python3
"""Stdin: JSON response from token-bearing workspace APIs.
Stdout: the auth_token value, or empty string.
Stderr: diagnostic when the response is unparseable or missing a token.

Exit code is always 0 — the empty string on stdout is how callers
distinguish "no token issued" (legitimate on re-registration) from
success. The warning on stderr surfaces the no-token case so it
stops masking downstream "missing workspace auth token" 401s.
"""
import json
import sys

try:
    data = json.load(sys.stdin)
except (json.JSONDecodeError, ValueError) as e:
    sys.stderr.write(f"e2e_extract_token: invalid JSON response ({e})\n")
    print("")
    raise SystemExit(0)

token = data.get("auth_token", "") or data.get("connection", {}).get("auth_token", "")
if not token:
    sys.stderr.write("e2e_extract_token: response contained no auth_token field\n")
print(token)
