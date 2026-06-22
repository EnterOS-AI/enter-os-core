#!/usr/bin/env python3
"""Cross-repo SSOT drift gate: compare molecule-core canvas tokens to molecule-app.

Fetches molecule-ai/molecule-app/app/globals.css and byte-compares the shared
@theme tokens (surface/ink/line/accent/warm/good/bad, light + dark) against the
local canvas/src/app/globals.css. Real drift -> ::error:: + exit 1 with diffs.

Advisory by default: the calling CI job uses continue-on-error: true and is NOT
in all-required. The gate skips loud when APP_SSOT_READ_TOKEN is absent.

Mirrors molecule-ai/molecule-app/.gitea/scripts/check_canvas_token_drift.py.
"""

from __future__ import annotations

import base64
import json
import os
import re
import sys
import urllib.request
from typing import Dict, Tuple

SHARED_TOKEN_NAMES: Tuple[str, ...] = (
    "--color-surface",
    "--color-surface-elevated",
    "--color-surface-sunken",
    "--color-surface-card",
    "--color-line",
    "--color-line-soft",
    "--color-ink",
    "--color-ink-mid",
    "--color-ink-soft",
    "--color-accent",
    "--color-accent-strong",
    "--color-warm",
    "--color-good",
    "--color-bad",
)

APP_FILE_PATH = "app/globals.css"
APP_REPO = "molecule-ai/molecule-app"
GITEA_SERVER = os.environ.get("GITEA_SERVER_URL", "https://git.moleculesai.app")
CANVAS_FILE_PATH = "canvas/src/app/globals.css"


def fetch_app_css(token: str) -> str:
    """Fetch the molecule-app globals.css raw content."""
    url = f"{GITEA_SERVER}/api/v1/repos/{APP_REPO}/contents/{APP_FILE_PATH}"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"token {token}",
            "Accept": "application/json",
        },
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = resp.read().decode("utf-8")

    payload = json.loads(data)
    content = payload.get("content")
    if not content:
        raise RuntimeError(f"empty content field from {url}")
    return base64.b64decode(content).decode("utf-8")


def extract_theme_block(css: str) -> str:
    """Extract the body of the first @theme block."""
    pattern = re.compile(r"@theme\s*\{([^}]*)\}", re.DOTALL)
    match = pattern.search(css)
    return match.group(1) if match else ""


def extract_data_theme_dark(css: str) -> str:
    """Extract the body of the first [data-theme=\"dark\"] block."""
    pattern = re.compile(r'\[data-theme="dark"\]\s*\{([^}]*)\}', re.DOTALL)
    match = pattern.search(css)
    return match.group(1) if match else ""


def parse_tokens(block: str) -> Dict[str, str]:
    """Parse --color-* tokens from a CSS block."""
    tokens: Dict[str, str] = {}
    for line in block.splitlines():
        line = line.strip()
        if not line or line.startswith("/*") or line.startswith("*"):
            continue
        match = re.match(r"(--color-[a-z-]+):\s*([^;]+);", line)
        if match:
            tokens[match.group(1)] = match.group(2).strip()
    return tokens


def extract_shared_tokens(css: str) -> Dict[str, Dict[str, str]]:
    """Return {light: tokens, dark: tokens} for the shared SSOT surface."""
    light_block = extract_theme_block(css)
    dark_block = extract_data_theme_dark(css)
    return {
        "light": parse_tokens(light_block),
        "dark": parse_tokens(dark_block),
    }


def compare_tokens(
    canvas: Dict[str, Dict[str, str]],
    app: Dict[str, Dict[str, str]],
) -> Tuple[bool, Dict[str, Dict[str, str]]]:
    """Compare canvas vs app tokens. Returns (ok, drift_by_mode)."""
    drift: Dict[str, Dict[str, str]] = {"light": {}, "dark": {}}
    ok = True
    for mode in ("light", "dark"):
        canvas_mode = canvas[mode]
        app_mode = app[mode]
        for name in SHARED_TOKEN_NAMES:
            canvas_val = canvas_mode.get(name)
            app_val = app_mode.get(name)
            if canvas_val != app_val:
                drift[mode][name] = f"app={app_val!r} canvas={canvas_val!r}"
                ok = False
    return ok, drift


def main() -> int:
    token = os.environ.get("APP_SSOT_READ_TOKEN")
    if not token:
        print("::notice::APP_SSOT_READ_TOKEN not set; skipping app token-SSOT drift gate.")
        print("           Gate will activate once the molecule-app read PAT is provisioned.")
        return 0

    if not os.path.exists(CANVAS_FILE_PATH):
        print(f"::error::{CANVAS_FILE_PATH} not found in working tree")
        return 1

    try:
        app_css = fetch_app_css(token)
    except Exception as exc:  # noqa: BLE001
        print(f"::error::Failed to fetch app SSOT ({APP_REPO}/{APP_FILE_PATH}): {exc}")
        return 1

    with open(CANVAS_FILE_PATH, "r", encoding="utf-8") as f:
        canvas_css = f.read()

    canvas_tokens = extract_shared_tokens(canvas_css)
    app_tokens = extract_shared_tokens(app_css)

    ok, drift = compare_tokens(canvas_tokens, app_tokens)
    if ok:
        print("::notice::Canvas↔app token SSOT is aligned.")
        return 0

    print("::error::Canvas↔app token SSOT drift detected.")
    for mode in ("light", "dark"):
        if drift[mode]:
            print(f"\n[{mode} mode]")
            for name, detail in drift[mode].items():
                print(f"  {name}: {detail}")
    print(f"\n{len(drift['light']) + len(drift['dark'])} token(s) differ from app SSOT.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
