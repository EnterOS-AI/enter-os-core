"""Env validation + workspace resolution for the standalone ``molecule-mcp``.

Extracted from ``mcp_cli.py`` (RFC #2873 iter 3). Deals with the two
shapes ``molecule-mcp`` accepts:

  * Single-workspace legacy shape: ``WORKSPACE_ID`` + token from
    ``MOLECULE_WORKSPACE_TOKEN`` or ``${CONFIGS_DIR}/.auth_token``.
  * Multi-workspace JSON shape: ``MOLECULE_WORKSPACES`` env var carries a
    JSON array of ``{"id": ..., "token": ...}`` entries.

Public surface:

* ``resolve_workspaces()`` → ``(workspaces, errors)``.
* ``read_token_file()`` → token text or ``""``.
* ``print_missing_env_help(missing, have_token_file)`` — operator-help
  printer.
"""
from __future__ import annotations

import json
import os
import sys

import configs_dir


def resolve_workspaces() -> tuple[list[tuple[str, str]], list[str]]:
    """Return the list of ``(workspace_id, token)`` pairs to register.

    Resolution order:

    1. ``MOLECULE_WORKSPACES`` env var — JSON array of
       ``{"id": "...", "token": "..."}`` objects. Activates the
       multi-workspace external-agent path (one process registered into
       N workspaces). When set, ``WORKSPACE_ID`` / ``MOLECULE_WORKSPACE_TOKEN``
       are IGNORED — the JSON is the source of truth.

    2. Single-workspace fallback — ``WORKSPACE_ID`` env var + token
       resolved in this order:
         a. ``MOLECULE_WORKSPACE_TOKEN`` (inline env — convenient but
            leaks into shell history + plaintext MCP-host config).
         b. ``MOLECULE_WORKSPACE_TOKEN_FILE`` (path to a file holding
            the token — operator can keep it 0600 in their home dir;
            survives shell-history scrubs).
         c. ``${CONFIGS_DIR}/.auth_token`` (in-container runtimes —
            the platform writes this on provision).

    Returns ``(workspaces, errors)``:
      * ``workspaces``: list of ``(workspace_id, token)`` — non-empty
        on the happy path.
      * ``errors``: human-readable strings describing what's missing /
        malformed. ``main()`` surfaces these with the same shape as
        ``print_missing_env_help`` so the operator's first run gives
        actionable output.

    Why JSON env (not file): ergonomic for Claude Code MCP config (one
    string in ``mcpServers.molecule.env`` instead of a sidecar file)
    and for CI / launchers. A separate config-file path can be added
    later without breaking this.
    """
    raw = os.environ.get("MOLECULE_WORKSPACES", "").strip()
    if raw:
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError as exc:
            return [], [
                f"MOLECULE_WORKSPACES is not valid JSON ({exc.msg} at pos "
                f"{exc.pos}). Expected: '[{{\"id\":\"<wsid>\",\"token\":"
                f"\"<tok>\"}},{{...}}]'"
            ]
        if not isinstance(parsed, list) or not parsed:
            return [], [
                "MOLECULE_WORKSPACES must be a non-empty JSON array of "
                "{\"id\":\"...\",\"token\":\"...\"} objects"
            ]
        out: list[tuple[str, str]] = []
        seen: set[str] = set()
        errors: list[str] = []
        for i, entry in enumerate(parsed):
            if not isinstance(entry, dict):
                errors.append(
                    f"MOLECULE_WORKSPACES[{i}] is not an object — got {type(entry).__name__}"
                )
                continue
            wsid = str(entry.get("id", "")).strip()
            tok = str(entry.get("token", "")).strip()
            if not wsid or not tok:
                errors.append(
                    f"MOLECULE_WORKSPACES[{i}] missing 'id' or 'token'"
                )
                continue
            if wsid in seen:
                errors.append(
                    f"MOLECULE_WORKSPACES[{i}] duplicate workspace id {wsid!r}"
                )
                continue
            seen.add(wsid)
            out.append((wsid, tok))
        if errors:
            return [], errors
        return out, []

    # Single-workspace back-compat path.
    wsid = os.environ.get("WORKSPACE_ID", "").strip()
    if not wsid:
        return [], ["WORKSPACE_ID (or MOLECULE_WORKSPACES) is required"]
    # Token resolution order (#2934): inline env → file path → CONFIGS_DIR
    # default. The file-path option exists so operators can keep the
    # bearer out of shell history and out of MCP-host config plaintext
    # (e.g. ~/.claude.json) — set MOLECULE_WORKSPACE_TOKEN_FILE to a
    # 0600 file containing the token. The CONFIGS_DIR/.auth_token
    # fallback predates this and stays for in-container runtimes.
    tok = os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip()
    if not tok:
        tok, tf_err = _read_token_from_file_env()
        if tf_err:
            # Operator explicitly pointed TOKEN_FILE somewhere — surface
            # the SPECIFIC failure (path doesn't exist, isn't readable,
            # or holds a blank file) instead of falling through to the
            # generic "set one of these three vars" message. Otherwise
            # they get exactly the silent failure mode #2934 flagged
            # ("a new user has no chance"). Skip the CONFIGS_DIR
            # fallback in this case — the operator's intent is clearly
            # to use the file path; deferring to a different source
            # would mask their config error.
            return [], [tf_err]
    if not tok:
        tok = read_token_file()
    if not tok:
        return [], [
            "MOLECULE_WORKSPACE_TOKEN, MOLECULE_WORKSPACE_TOKEN_FILE, or "
            "CONFIGS_DIR/.auth_token is required"
        ]
    return [(wsid, tok)], []


def _read_token_from_file_env() -> tuple[str, str]:
    """Read the token from the file path in MOLECULE_WORKSPACE_TOKEN_FILE.

    Returns ``(token, error)``:
      * env var unset/blank → ``("", "")`` — caller falls through silently
        to the next source; the operator didn't ask for this path.
      * file open/read fails (missing, permission denied, decode error)
        → ``("", "<specific error>")`` — caller surfaces it directly.
        The operator EXPLICITLY pointed at this path, so a generic
        fallthrough error would mask their config bug (#2934).
      * file is blank → ``("", "<blank file error>")`` — same reasoning.
      * file read returns junk with internal whitespace/newlines (e.g.
        a CSV cell, accidental multi-token paste) → ``("", "<error>")``
        rather than concatenating into a malformed bearer that 401s
        against the platform with no context.
      * happy path → ``("<token>", "")``.
    """
    path = os.environ.get("MOLECULE_WORKSPACE_TOKEN_FILE", "").strip()
    if not path:
        return "", ""
    try:
        with open(path, encoding="utf-8") as fh:
            raw = fh.read()
    except FileNotFoundError:
        return "", (
            f"MOLECULE_WORKSPACE_TOKEN_FILE points to {path!r} which "
            f"does not exist"
        )
    except PermissionError:
        return "", (
            f"MOLECULE_WORKSPACE_TOKEN_FILE={path!r} is not readable "
            f"(permission denied)"
        )
    except OSError as exc:
        return "", (
            f"MOLECULE_WORKSPACE_TOKEN_FILE={path!r} could not be read: "
            f"{exc}"
        )
    except UnicodeDecodeError:
        return "", (
            f"MOLECULE_WORKSPACE_TOKEN_FILE={path!r} is not valid UTF-8"
        )
    tok = raw.strip()
    if not tok:
        return "", (
            f"MOLECULE_WORKSPACE_TOKEN_FILE={path!r} is empty"
        )
    # Reject tokens with internal whitespace — a CSV cell or accidental
    # multi-token paste would otherwise become a malformed bearer that
    # 401s against the platform with no diagnostic.
    if any(ch.isspace() for ch in tok):
        return "", (
            f"MOLECULE_WORKSPACE_TOKEN_FILE={path!r} contains internal "
            f"whitespace — expected a single token"
        )
    return tok, ""


def print_missing_env_help(missing: list[str], have_token_file: bool) -> None:
    print("molecule-mcp: missing required environment.\n", file=sys.stderr)
    print("Set the following before running molecule-mcp:", file=sys.stderr)
    print("  WORKSPACE_ID                — your workspace UUID (from canvas)", file=sys.stderr)
    print(
        "  PLATFORM_URL                — base URL of your Molecule platform "
        "(e.g. https://your-tenant.staging.moleculesai.app)",
        file=sys.stderr,
    )
    if not have_token_file:
        print(
            "  MOLECULE_WORKSPACE_TOKEN    — bearer token for this workspace "
            "(canvas → Tokens tab)",
            file=sys.stderr,
        )
        print(
            "                              OR set MOLECULE_WORKSPACE_TOKEN_FILE"
            " to a path that holds the token",
            file=sys.stderr,
        )
        print(
            "                              (keeps the secret out of shell"
            " history and MCP-host config plaintext)",
            file=sys.stderr,
        )
    print("", file=sys.stderr)
    print(f"Currently missing: {', '.join(missing)}", file=sys.stderr)


def read_token_file() -> str:
    """Read the token from the resolved configs dir's ``.auth_token`` if
    present.

    Mirrors platform_auth._token_file's location resolution but without
    importing the heavy module here (that import triggers a2a_client's
    WORKSPACE_ID guard which is fine after env validation, but cheaper
    to inline a 4-line file read than pull in the whole stack just for
    the path).
    """
    path = configs_dir.resolve() / ".auth_token"
    if not path.is_file():
        return ""
    try:
        return path.read_text().strip()
    except OSError:
        return ""
