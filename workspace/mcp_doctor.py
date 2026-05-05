"""molecule-mcp doctor — diagnostic subcommand for first-run install.

Run via ``molecule-mcp doctor``. Prints a checklist of common
onboarding failure modes and concrete next-step suggestions for each
failed check.

Closes Ryan's #2934 item 6 ("Add a molecule-mcp doctor subcommand —
this single command would have saved me 30 of the 45 minutes").
Pairs with #2935 (Python>=3.11 callout, PATH guidance, TOKEN_FILE
support) — those fixed the snippet, this gives the operator a way to
self-diagnose when something still goes wrong.

Six checks, in operator-encounter order:

    1. Python version    — wheel requires >=3.11 (pip says
                            "no versions found" on older).
    2. Wheel install     — molecule_runtime importable + version reported.
    3. PATH for molecule-mcp — pip user-site installs land at
                            ~/Library/Python/3.X/bin which isn't on
                            PATH on a fresh macOS shell. Most common
                            "claude mcp add can't find molecule-mcp"
                            cause.
    4. Env vars          — PLATFORM_URL set + reachable;
                            WORKSPACE_ID set; auth token resolvable
                            (env or *_FILE or .auth_token).
    5. Platform health   — GET ${PLATFORM_URL}/healthz returns 2xx.
                            Catches DNS/firewall/wrong-scheme issues
                            before the operator hits the real
                            register call.
    6. Registry register — POST ${PLATFORM_URL}/registry/register
                            with the resolved workspace_id+token
                            returns 2xx. End-to-end auth verification.

Each check prints one of:
    [OK]   <one-line status>
    [WARN] <one-line status>      next: <fix suggestion>
    [FAIL] <one-line status>      next: <fix suggestion>

Exit 0 if all pass or only WARNs; exit 1 if any FAIL — so the
subcommand is scriptable from CI / install-checks too.

Out of scope for now (deferred follow-ups):
    - Claude Code-specific checks (parse ~/.claude.json, verify each
      MCP entry is plugin-sourced + dev-channels flag is set). That's
      a separate Claude-Code-specific doctor and lives in the
      claude-code-channel plugin, not the universal-MCP doctor.
    - Automated remediation (running the suggested fix). Doctor is
      a diagnostic tool — it tells the operator what's wrong + how
      to fix it, doesn't apply changes.
"""
from __future__ import annotations

import importlib
import importlib.metadata
import os
import shutil
import sys
from typing import Optional

# urllib avoids a hard dep on `requests` for the doctor — the real
# CLI already imports requests via mcp_heartbeat, but doctor should
# keep working even on a partial install where requests is missing
# (that itself is a finding worth surfacing).
from urllib import request as urllib_request
from urllib.error import URLError


# ANSI colors are friendly on TTYs; auto-disable on pipe / NO_COLOR
# for CI logs where the escape sequences clutter the diff.
def _color(name: str) -> str:
    if not sys.stdout.isatty() or os.environ.get("NO_COLOR"):
        return ""
    return {
        "green": "\033[32m",
        "yellow": "\033[33m",
        "red": "\033[31m",
        "dim": "\033[2m",
        "reset": "\033[0m",
    }.get(name, "")


def _ok(label: str, msg: str) -> None:
    print(f"  {_color('green')}[OK]{_color('reset')}   {label}: {msg}")


def _warn(label: str, msg: str, fix: str) -> None:
    print(f"  {_color('yellow')}[WARN]{_color('reset')} {label}: {msg}")
    print(f"        {_color('dim')}next:{_color('reset')} {fix}")


def _fail(label: str, msg: str, fix: str) -> None:
    print(f"  {_color('red')}[FAIL]{_color('reset')} {label}: {msg}")
    print(f"        {_color('dim')}next:{_color('reset')} {fix}")


# Each check returns a "ok" | "warn" | "fail" verdict so the caller
# can compute an exit code without re-walking the print stream.
Verdict = str  # "ok" | "warn" | "fail"


def check_python_version() -> Verdict:
    label = "Python version"
    major, minor = sys.version_info[:2]
    if (major, minor) >= (3, 11):
        _ok(label, f"Python {major}.{minor} (wheel requires >=3.11)")
        return "ok"
    _fail(
        label,
        f"Python {major}.{minor} is below the wheel's >=3.11 floor",
        "upgrade Python (brew install python@3.12 / apt install python3.12) "
        "or run molecule-mcp via a 3.11+ venv.",
    )
    return "fail"


def check_wheel_install() -> Verdict:
    label = "Wheel install"
    try:
        version = importlib.metadata.version("molecule-ai-workspace-runtime")
    except importlib.metadata.PackageNotFoundError:
        _fail(
            label,
            "molecule-ai-workspace-runtime not found in this interpreter's site-packages",
            "pip install molecule-ai-workspace-runtime "
            "(or pipx install molecule-ai-workspace-runtime to get the "
            "binary on PATH automatically).",
        )
        return "fail"
    try:
        importlib.import_module("molecule_runtime.mcp_cli")
    except ImportError as e:
        _fail(
            label,
            f"package found ({version}) but `molecule_runtime.mcp_cli` won't import: {e}",
            "reinstall the wheel (pip install --force-reinstall "
            "molecule-ai-workspace-runtime); if it still fails, file "
            "a bug with the traceback.",
        )
        return "fail"
    _ok(label, f"molecule-ai-workspace-runtime=={version}")
    return "ok"


def check_path_for_binary() -> Verdict:
    label = "PATH for molecule-mcp"
    found = shutil.which("molecule-mcp")
    if found:
        _ok(label, f"resolves to {found}")
        return "ok"
    # Not on PATH — work out where pip put it so the suggestion is
    # actionable instead of generic.
    user_base = os.environ.get("PYTHONUSERBASE")
    if not user_base:
        try:
            import site
            user_base = site.getuserbase()
        except Exception:
            user_base = None
    hint = (
        f"add `{user_base}/bin` to PATH"
        if user_base
        else "switch to `pipx install molecule-ai-workspace-runtime` so the "
             "binary lands in pipx's managed bin/ on PATH"
    )
    _fail(
        label,
        "molecule-mcp not found on PATH",
        f"{hint}, or invoke via `python -m molecule_runtime.mcp_cli` directly.",
    )
    return "fail"


def _resolve_token_summary() -> Optional[str]:
    """Return a short non-secret description of how the token was
    sourced (e.g. "env MOLECULE_WORKSPACE_TOKEN", "file .auth_token"),
    or None if no token is reachable.
    """
    if os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip():
        return "env MOLECULE_WORKSPACE_TOKEN"
    file_var = os.environ.get("MOLECULE_WORKSPACE_TOKEN_FILE", "").strip()
    if file_var:
        if os.path.isfile(file_var):
            return f"file {file_var} (via MOLECULE_WORKSPACE_TOKEN_FILE)"
        return None
    # Per-runtime container path used by the in-platform path; rarely
    # set on external setups but check anyway so the message is
    # accurate for both shapes.
    try:
        import configs_dir
        candidate = configs_dir.resolve() / ".auth_token"
        if candidate.is_file():
            return f"file {candidate}"
    except Exception:
        pass
    return None


def check_env_vars() -> Verdict:
    label = "Env vars"
    missing: list[str] = []
    if not os.environ.get("PLATFORM_URL", "").strip():
        missing.append("PLATFORM_URL")
    if not os.environ.get("WORKSPACE_ID", "").strip() and not os.environ.get(
        "MOLECULE_WORKSPACES", "",
    ).strip():
        missing.append("WORKSPACE_ID (or MOLECULE_WORKSPACES)")
    token_summary = _resolve_token_summary()
    if not token_summary and not os.environ.get("MOLECULE_WORKSPACES", "").strip():
        # MOLECULE_WORKSPACES is a JSON-array env that bundles its
        # own per-workspace tokens — if it's set we trust the
        # resolver to validate.
        missing.append(
            "MOLECULE_WORKSPACE_TOKEN (or MOLECULE_WORKSPACE_TOKEN_FILE, or "
            "/configs/.auth_token)",
        )
    if missing:
        _fail(
            label,
            f"unset: {', '.join(missing)}",
            "see the canvas Connect-External-Agent modal — the snippet "
            "exports all three. Use MOLECULE_WORKSPACE_TOKEN_FILE for the "
            "token to keep secrets out of shell history.",
        )
        return "fail"
    _ok(
        label,
        f"PLATFORM_URL + WORKSPACE_ID set; token from {token_summary or 'MOLECULE_WORKSPACES'}",
    )
    return "ok"


def _http_get(url: str, timeout: float = 5.0) -> tuple[Optional[int], Optional[str]]:
    """Best-effort GET that swallows transport errors and returns
    (status, error_message). Status is None when the request couldn't
    complete; error_message is None when the request returned 2xx.
    """
    try:
        # Origin header — staging tenants enforce same-origin via WAF;
        # /healthz tolerates either way but matching production headers
        # surfaces auth-style 401s correctly during the doctor run.
        req = urllib_request.Request(
            url,
            headers={"Origin": os.environ.get("PLATFORM_URL", "").rstrip("/")},
        )
        with urllib_request.urlopen(req, timeout=timeout) as resp:
            return resp.status, None
    except URLError as e:
        return None, str(e.reason if hasattr(e, "reason") else e)
    except Exception as e:
        return None, str(e)


def check_platform_health() -> Verdict:
    label = "Platform reachability"
    base = os.environ.get("PLATFORM_URL", "").strip().rstrip("/")
    if not base:
        _warn(label, "skipped (PLATFORM_URL unset — see Env vars)", "set PLATFORM_URL first")
        return "warn"
    if not base.startswith(("http://", "https://")):
        _fail(
            label,
            f"PLATFORM_URL missing scheme: {base!r}",
            "set PLATFORM_URL to include https:// — e.g. "
            "PLATFORM_URL=https://your-tenant.staging.moleculesai.app",
        )
        return "fail"
    if base.endswith("/"):
        _warn(
            label,
            "PLATFORM_URL has trailing slash (will be stripped automatically)",
            "remove the trailing slash to match the snippet shape",
        )
    status, err = _http_get(f"{base}/healthz")
    if status is None:
        _fail(label, f"GET {base}/healthz failed: {err}", "check DNS + firewall + scheme")
        return "fail"
    if not (200 <= status < 300):
        _fail(label, f"GET {base}/healthz returned HTTP {status}", "verify the tenant subdomain is correct + provisioned")
        return "fail"
    _ok(label, f"GET {base}/healthz → {status}")
    return "ok"


def check_register() -> Verdict:
    """Light end-to-end auth check via POST /registry/register.

    Doesn't write a heartbeat or change platform state beyond what
    a normal molecule-mcp boot would do — the register endpoint is
    idempotent. Skipped when env vars failed earlier so the operator
    isn't shown a redundant 401.
    """
    label = "Registry register"
    base = os.environ.get("PLATFORM_URL", "").strip().rstrip("/")
    workspace_id = os.environ.get("WORKSPACE_ID", "").strip()
    token_summary = _resolve_token_summary()
    if not (base and workspace_id and token_summary):
        _warn(label, "skipped (Env vars must pass first)", "fix Env vars, re-run")
        return "warn"
    # Get the token value matching the resolver's source preference.
    token = os.environ.get("MOLECULE_WORKSPACE_TOKEN", "").strip()
    if not token:
        file_var = os.environ.get("MOLECULE_WORKSPACE_TOKEN_FILE", "").strip()
        if file_var and os.path.isfile(file_var):
            try:
                token = open(file_var).read().strip()
            except Exception as e:
                _fail(label, f"could not read token file: {e}", "fix the file permissions or path")
                return "fail"
    if not token:
        _warn(label, "skipped (no token resolvable)", "set MOLECULE_WORKSPACE_TOKEN or MOLECULE_WORKSPACE_TOKEN_FILE")
        return "warn"
    import json
    body = json.dumps({
        "id": workspace_id,
        "url": "",  # external URL is unknown to doctor; register accepts empty
        "agent_card": {"name": "doctor-probe", "version": "0.0.0"},
    }).encode()
    req = urllib_request.Request(
        f"{base}/registry/register",
        data=body,
        method="POST",
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
            "Origin": base,
        },
    )
    try:
        with urllib_request.urlopen(req, timeout=8.0) as resp:
            status = resp.status
    except URLError as e:
        # Pull HTTP code from HTTPError; transport errors don't have one.
        status = getattr(e, "code", None)
        err = str(e.reason if hasattr(e, "reason") else e)
        if status is None:
            _fail(label, f"POST {base}/registry/register failed: {err}", "check network")
            return "fail"
    except Exception as e:
        _fail(label, f"POST register failed: {e}", "check network")
        return "fail"
    if status == 401:
        _fail(
            label,
            "401 Unauthorized — token rejected",
            "tokens are shown only once at workspace-create time; "
            "re-create the workspace OR rotate via canvas Tokens tab.",
        )
        return "fail"
    if status == 404:
        _fail(
            label,
            f"404 — workspace_id {workspace_id} not found on {base}",
            "verify WORKSPACE_ID matches a real workspace + the tenant "
            "subdomain in PLATFORM_URL.",
        )
        return "fail"
    if not (200 <= status < 300):
        _fail(label, f"POST register returned HTTP {status}", "see platform logs")
        return "fail"
    _ok(label, f"POST {base}/registry/register → {status}")
    return "ok"


CHECKS = [
    check_python_version,
    check_wheel_install,
    check_path_for_binary,
    check_env_vars,
    check_platform_health,
    check_register,
]


def run() -> int:
    """Run all checks and return a process exit code (0 ok, 1 if any fail)."""
    print("molecule-mcp doctor — onboarding diagnostic")
    print()
    verdicts = []
    for chk in CHECKS:
        try:
            verdicts.append(chk())
        except Exception as e:
            # A buggy check shouldn't kill the rest of the doctor run.
            print(f"  [BUG]  {chk.__name__}: unexpected {type(e).__name__}: {e}")
            verdicts.append("fail")
    print()
    fails = sum(1 for v in verdicts if v == "fail")
    warns = sum(1 for v in verdicts if v == "warn")
    if fails:
        print(f"{fails} check(s) failed, {warns} warning(s). Fix the FAIL items above and re-run.")
        return 1
    if warns:
        print(f"All required checks passed; {warns} warning(s) — review the next-step hints.")
        return 0
    print("All checks passed.")
    return 0
