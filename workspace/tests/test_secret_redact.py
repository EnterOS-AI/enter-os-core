"""Tests for builtin_tools/security.py — _redact_secrets() (#834 — C2).

Coverage targets
----------------
- Unit: each secret pattern type (OpenAI key, GitHub tokens, AWS key,
  generic contextual pattern)
- Idempotency: already-redacted strings pass through unchanged
- Non-regression: normal prose is never modified
- Integration: commit_memory call sites in builtin_tools/memory.py,
  a2a_tools.py, and executor_helpers.py each invoke _redact_secrets before
  persisting content

Spec patterns verified:
    sk-[A-Za-z0-9]{20,}          — OpenAI/Anthropic-style keys
    ghp_[A-Za-z0-9]{36}          — GitHub classic PAT
    ghs_[A-Za-z0-9]{36}          — GitHub server token
    github_pat_[A-Za-z0-9_]{82}  — GitHub fine-grained PAT
    AKIA[0-9A-Z]{16}              — AWS access key ID
    key/token/secret/password/api_key = <40+ chars>  — generic contextual
"""

from __future__ import annotations

import importlib.util
import json
import os
import sys
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

# ---------------------------------------------------------------------------
# Bootstrap: load the real builtin_tools/security.py before conftest stubs
# interfere.  conftest sets builtin_tools.__path__ = [] which prevents normal
# submodule discovery, so we load via file path (same pattern as test_memory.py).
# ---------------------------------------------------------------------------

_WT_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_SECURITY_PATH = os.path.join(_WT_ROOT, "builtin_tools", "security.py")

_spec = importlib.util.spec_from_file_location("builtin_tools.security", _SECURITY_PATH)
_security_mod = importlib.util.module_from_spec(_spec)
sys.modules["builtin_tools.security"] = _security_mod
_spec.loader.exec_module(_security_mod)

REDACTED: str = _security_mod.REDACTED
_redact_secrets = _security_mod._redact_secrets


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _gh_classic() -> str:
    """Return a syntactically valid-length GitHub classic PAT."""
    return "ghp_" + "A" * 36


def _ghs() -> str:
    return "ghs_" + "B" * 36


def _gh_fine() -> str:
    return "github_pat_" + "C" * 82


def _aws() -> str:
    return "AKIA" + "0" * 16


def _openai() -> str:
    return "sk-" + "x" * 48


def _anthropic() -> str:
    return "sk-ant-" + "y" * 40  # sk- prefix applies


# ---------------------------------------------------------------------------
# _redact_secrets — per-pattern unit tests
# ---------------------------------------------------------------------------


class TestRedactOpenAIStyleKeys:
    def test_bare_openai_key_redacted(self):
        result = _redact_secrets(f"Key is {_openai()}")
        assert _openai() not in result
        assert REDACTED in result

    def test_anthropic_key_redacted(self):
        result = _redact_secrets(f"Using {_anthropic()} for requests")
        assert _anthropic() not in result
        assert REDACTED in result

    def test_short_sk_prefix_not_redacted(self):
        """sk- with fewer than 20 chars should NOT be redacted (e.g. 'sk-test')."""
        result = _redact_secrets("sk-test")
        assert result == "sk-test"


class TestRedactGitHubTokens:
    def test_classic_pat_redacted(self):
        token = _gh_classic()
        result = _redact_secrets(f"auth: {token}")
        assert token not in result
        assert REDACTED in result

    def test_server_token_redacted(self):
        token = _ghs()
        result = _redact_secrets(f"ghs token: {token}")
        assert token not in result
        assert REDACTED in result

    def test_fine_grained_pat_redacted(self):
        token = _gh_fine()
        result = _redact_secrets(token)
        assert token not in result
        assert REDACTED in result

    def test_classic_pat_wrong_length_not_redacted(self):
        """ghp_ token with only 10 chars should NOT be redacted (wrong length)."""
        short = "ghp_" + "A" * 10
        result = _redact_secrets(short)
        assert result == short


class TestRedactAWSKey:
    def test_aws_access_key_redacted(self):
        key = _aws()
        result = _redact_secrets(f"AWS key: {key}")
        assert key not in result
        assert REDACTED in result

    def test_akia_prefix_wrong_length_not_redacted(self):
        """AKIA with only 10 trailing chars should NOT be redacted."""
        short = "AKIA" + "X" * 10
        result = _redact_secrets(short)
        assert result == short


class TestRedactGenericContextual:
    def test_api_key_equals_redacted(self):
        secret = "A" * 45
        result = _redact_secrets(f"api_key={secret}")
        assert secret not in result
        assert "api_key=" in result
        assert REDACTED in result

    def test_key_equals_redacted(self):
        secret = "B" * 42
        result = _redact_secrets(f"key={secret}")
        assert secret not in result
        assert REDACTED in result

    def test_token_equals_redacted(self):
        secret = "C" * 50
        result = _redact_secrets(f"token={secret}")
        assert secret not in result
        assert REDACTED in result

    def test_secret_equals_redacted(self):
        secret = "D" * 44
        result = _redact_secrets(f"secret={secret}")
        assert secret not in result
        assert REDACTED in result

    def test_password_equals_redacted(self):
        secret = "E" * 41
        result = _redact_secrets(f"password={secret}")
        assert secret not in result
        assert REDACTED in result

    def test_keyword_case_insensitive(self):
        secret = "F" * 40
        result = _redact_secrets(f"API_KEY={secret}")
        assert secret not in result
        assert REDACTED in result

    def test_keyword_with_spaces_around_equals(self):
        secret = "G" * 40
        result = _redact_secrets(f"token = {secret}")
        assert secret not in result
        assert REDACTED in result

    def test_short_value_not_redacted(self):
        """Values shorter than 40 chars should NOT be treated as secrets."""
        result = _redact_secrets("api_key=short")
        assert result == "api_key=short"

    def test_base64_value_with_equals_padding_redacted(self):
        """Base64-padded values (ending in ==) should be redacted."""
        secret = "A" * 44 + "=="
        result = _redact_secrets(f"key={secret}")
        assert secret not in result
        assert REDACTED in result


# ---------------------------------------------------------------------------
# _redact_secrets — idempotency
# ---------------------------------------------------------------------------


class TestIdempotency:
    def test_already_redacted_token_passes_through(self):
        content = f"The token was {REDACTED}"
        assert _redact_secrets(content) == content

    def test_double_application_unchanged(self):
        """Applying _redact_secrets twice must not alter the result."""
        content = f"key={_openai()} and github {_gh_classic()}"
        once = _redact_secrets(content)
        twice = _redact_secrets(once)
        assert once == twice

    def test_pure_redacted_string(self):
        assert _redact_secrets(REDACTED) == REDACTED


# ---------------------------------------------------------------------------
# _redact_secrets — non-regression (normal prose untouched)
# ---------------------------------------------------------------------------


class TestNormalProseUnchanged:
    def test_plain_sentence(self):
        text = "The quick brown fox jumps over the lazy dog."
        assert _redact_secrets(text) == text

    def test_numbers_and_punctuation(self):
        text = "Order #12345 shipped at 09:00 on 2026-04-17."
        assert _redact_secrets(text) == text

    def test_empty_string(self):
        assert _redact_secrets("") == ""

    def test_short_key_value(self):
        assert _redact_secrets("key=short_value") == "key=short_value"

    def test_json_with_short_values(self):
        text = json.dumps({"status": "ok", "workspace_id": "ws-abc123"})
        assert _redact_secrets(text) == text

    def test_markdown_content(self):
        text = "## Summary\n\nThe task completed successfully. No errors."
        assert _redact_secrets(text) == text


# ---------------------------------------------------------------------------
# _redact_secrets — multiple secrets in one string
# ---------------------------------------------------------------------------


class TestMultipleSecrets:
    def test_two_different_types_both_redacted(self):
        content = f"OpenAI key: {_openai()} GitHub: {_gh_classic()}"
        result = _redact_secrets(content)
        assert _openai() not in result
        assert _gh_classic() not in result
        assert result.count(REDACTED) == 2

    def test_all_pattern_types_in_one_string(self):
        parts = [
            f"openai={_openai()}",
            f"github={_gh_classic()}",
            f"aws={_aws()}",
        ]
        content = " | ".join(parts)
        result = _redact_secrets(content)
        assert _openai() not in result
        assert _gh_classic() not in result
        assert _aws() not in result


# ---------------------------------------------------------------------------
# Integration: builtin_tools/memory.py commit_memory
#
# The conftest stubs builtin_tools with __path__=[] at collection time, so
# `from builtin_tools import memory` would return the mock module.  We load
# the real memory.py directly via spec_from_file_location (same pattern as
# test_memory.py) so that the _redact_secrets import inside it binds to the
# real function.
# ---------------------------------------------------------------------------


class TestMemoryCommitRedactsSecrets:
    """Verify that builtin_tools/memory.py calls _redact_secrets before storage.

    Loading the real memory.py in tests is impractical because the conftest
    awareness_client stub does not expose build_awareness_client.  Instead we
    verify at two levels:
      1. Source-code inspection: the call site exists and is correctly placed
         (before any HTTP/awareness write).
      2. Functional: the unit-tested _redact_secrets function itself handles
         the token — the a2a_tools integration tests cover the end-to-end path.
    """

    def test_memory_py_imports_redact_secrets(self):
        """builtin_tools/memory.py must import _redact_secrets from security."""
        source = open(os.path.join(_WT_ROOT, "builtin_tools", "memory.py")).read()
        assert "from builtin_tools.security import _redact_secrets" in source, (
            "memory.py must import _redact_secrets from builtin_tools.security"
        )

    def test_memory_py_calls_redact_before_use(self):
        """_redact_secrets(content) must appear in memory.py before the HTTP call."""
        source = open(os.path.join(_WT_ROOT, "builtin_tools", "memory.py")).read()
        assert "_redact_secrets(content)" in source, (
            "memory.py must call _redact_secrets(content) before storing"
        )

    def test_redact_applied_before_store_in_function_body(self):
        """_redact_secrets(content) must appear before build_awareness_client()
        inside the commit_memory function body (i.e. before any storage call).
        """
        source = open(os.path.join(_WT_ROOT, "builtin_tools", "memory.py")).read()
        # Find the commit_memory function definition, then measure positions
        # of redact and awareness_client() calls within that scope.
        fn_start = source.find("async def commit_memory(")
        assert fn_start != -1, "commit_memory not found in memory.py"
        fn_body = source[fn_start:]  # everything from the function onward
        redact_pos = fn_body.find("_redact_secrets(content)")
        store_pos = fn_body.find("build_awareness_client()")
        assert redact_pos != -1, "_redact_secrets(content) not found in commit_memory body"
        assert store_pos != -1, "build_awareness_client() not found in commit_memory body"
        assert redact_pos < store_pos, (
            f"_redact_secrets (offset {redact_pos}) must appear BEFORE "
            f"build_awareness_client() (offset {store_pos}) in commit_memory body"
        )


# ---------------------------------------------------------------------------
# Integration: a2a_tools.tool_commit_memory
# ---------------------------------------------------------------------------


class TestA2AToolCommitMemoryRedactsSecrets:
    @pytest.mark.asyncio
    async def test_github_token_redacted(self):
        """tool_commit_memory must scrub secrets before the HTTP POST."""
        import a2a_tools

        token = _gh_classic()
        content_with_secret = f"ghp token encountered: {token}"
        captured: dict = {}

        fake_resp = MagicMock()
        fake_resp.status_code = 201
        fake_resp.json = MagicMock(return_value={"id": "mem-3"})

        fake_client = AsyncMock()
        fake_client.__aenter__ = AsyncMock(return_value=fake_client)
        fake_client.__aexit__ = AsyncMock(return_value=False)

        async def _capture(url, *, json=None, headers=None, **kwargs):
            captured.update(json or {})
            return fake_resp

        fake_client.post = _capture

        with patch("a2a_tools_memory.httpx.AsyncClient", return_value=fake_client):
            await a2a_tools.tool_commit_memory(content_with_secret)

        stored = captured.get("content", "")
        assert token not in stored
        assert REDACTED in stored

    @pytest.mark.asyncio
    async def test_openai_key_redacted(self):
        import a2a_tools

        key = _openai()
        captured: dict = {}

        fake_resp = MagicMock()
        fake_resp.status_code = 201
        fake_resp.json = MagicMock(return_value={"id": "mem-4"})

        fake_client = AsyncMock()
        fake_client.__aenter__ = AsyncMock(return_value=fake_client)
        fake_client.__aexit__ = AsyncMock(return_value=False)

        async def _capture(url, *, json=None, headers=None, **kwargs):
            captured.update(json or {})
            return fake_resp

        fake_client.post = _capture

        with patch("a2a_tools_memory.httpx.AsyncClient", return_value=fake_client):
            await a2a_tools.tool_commit_memory(f"key={key}")

        stored = captured.get("content", "")
        assert key not in stored
        assert REDACTED in stored


# ---------------------------------------------------------------------------
# Integration: executor_helpers.commit_memory
# ---------------------------------------------------------------------------


class TestExecutorHelpersCommitMemoryRedactsSecrets:
    @pytest.mark.asyncio
    async def test_aws_key_redacted_before_post(self, monkeypatch):
        """executor_helpers.commit_memory must scrub AWS keys before the POST."""
        import executor_helpers

        monkeypatch.setenv("WORKSPACE_ID", "ws-test")
        monkeypatch.setenv("PLATFORM_URL", "http://platform.test")

        aws_key = _aws()
        content_with_secret = f"Discovered AWS key: {aws_key}"
        captured: dict = {}

        # get_http_client() returns a plain AsyncClient — no context manager.
        # Patch it to return an async-capable mock with a .post() coroutine.
        fake_client = MagicMock()

        async def _capture_post(url, *, json=None, headers=None, **kwargs):
            captured.update(json or {})
            return MagicMock(status_code=200)

        fake_client.post = _capture_post

        with patch("executor_helpers.get_http_client", return_value=fake_client):
            await executor_helpers.commit_memory(content_with_secret)

        stored = captured.get("content", "")
        assert aws_key not in stored, f"AWS key found in stored content: {stored!r}"
        assert REDACTED in stored

    @pytest.mark.asyncio
    async def test_openai_key_redacted_before_post(self, monkeypatch):
        """executor_helpers.commit_memory must scrub OpenAI-style keys."""
        import executor_helpers

        monkeypatch.setenv("WORKSPACE_ID", "ws-test")
        monkeypatch.setenv("PLATFORM_URL", "http://platform.test")

        key = _openai()
        captured: dict = {}

        fake_client = MagicMock()

        async def _capture(url, *, json=None, headers=None, **kwargs):
            captured.update(json or {})
            return MagicMock(status_code=200)

        fake_client.post = _capture

        with patch("executor_helpers.get_http_client", return_value=fake_client):
            await executor_helpers.commit_memory(f"model key: {key}")

        assert key not in captured.get("content", "")
        assert REDACTED in captured.get("content", "")
