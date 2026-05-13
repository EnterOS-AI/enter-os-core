"""Tests for `.gitea/scripts/lint-curl-status-capture.py`.

Run:
    python3 -m pytest tests/test_lint_curl_status_capture.py -v
"""
from __future__ import annotations

import importlib.util
from pathlib import Path


SCRIPT_PATH = (
    Path(__file__).resolve().parent.parent
    / ".gitea"
    / "scripts"
    / "lint-curl-status-capture.py"
)


def _load_module():
    spec = importlib.util.spec_from_file_location("lint_curl_status_capture", SCRIPT_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_finds_quoted_echo_fallback_pollution():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body -w "%{http_code}" https://example.test || echo "000")
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
    assert "echo" in findings[0].snippet


def test_finds_unquoted_echo_fallback_pollution():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body -w '%{http_code}' https://example.test || echo 000)
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
    assert "echo" in findings[0].snippet


def test_finds_printf_fallback_pollution():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body -w '%{http_code}' https://example.test || printf '000')
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
    assert "printf" in findings[0].snippet


def test_ignores_tempfile_fallback_after_curl():
    lint = _load_module()
    content = """
    set +e
    curl -sS -o /tmp/body -w '%{http_code}' https://example.test >/tmp/code
    rc=$?
    set -e
    HTTP_CODE=$(cat /tmp/code 2>/dev/null || echo "000")
    [ -z "$HTTP_CODE" ] && HTTP_CODE="000"
    """

    assert lint.scan_content("workflow.yml", content) == []


def test_collapses_bash_line_continuations():
    lint = _load_module()
    content = """
    HTTP_CODE=$(curl -sS -o /tmp/body \\
      -w "%{http_code}" \\
      https://example.test \\
      || echo "000")
    """

    findings = lint.scan_content("workflow.yml", content)

    assert len(findings) == 1
