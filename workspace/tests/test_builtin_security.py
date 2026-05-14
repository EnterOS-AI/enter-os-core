"""Test coverage for builtin_tools.security._redact_secrets().

Issue #834 (C2): commit_memory must not persist API keys verbatim.

Pre-commit hook blocks bare secret-like strings (ghp_, sk-ant-, etc.) to prevent
accidental commits of real credentials.  These tests focus on the functional
behaviour of the redaction logic: idempotency, contextual keyword=value patterns,
boundary cases, and mixed content — without triggering the hook's length thresholds.
The pre-commit hook itself is the primary guard for bare-pattern detection.
"""
from __future__ import annotations

from builtin_tools.security import REDACTED, _redact_secrets


class TestRedactContextual:
    """Keyword=value patterns with high-entropy values (under pre-commit threshold)."""

    def test_api_key_contextual(self):
        """api_key=X where X ≥ 40 base64 chars → value replaced, keyword preserved."""
        value = "A" * 40
        assert _redact_secrets(f"api_key={value}") == f"api_key={REDACTED}"

    def test_keyword_contextual(self):
        """Generic 'key=' also matches."""
        value = "B" * 45
        assert _redact_secrets(f"key={value}") == f"key={REDACTED}"

    def test_secret_contextual(self):
        value = "C" * 50
        assert _redact_secrets(f"secret= {value}") == f"secret= {REDACTED}"

    def test_token_contextual(self):
        value = "D" * 40
        assert _redact_secrets(f"token={value}") == f"token={REDACTED}"

    def test_password_contextual(self):
        value = "E" * 50
        assert _redact_secrets(f"password={value}") == f"password={REDACTED}"

    def test_keyword_spacing_tolerated(self):
        """Spaces around = are tolerated by the pattern."""
        value = "F" * 40
        assert _redact_secrets(f"key = {value}") == f"key = {REDACTED}"

    def test_contextual_too_short_not_redacted(self):
        """Value shorter than 40 chars is not redacted."""
        short = "A" * 39
        assert _redact_secrets(f"api_key={short}") == f"api_key={short}"

    def test_case_insensitive_keyword(self):
        """Keyword matching is case-insensitive."""
        value = "G" * 40
        assert _redact_secrets(f"API_KEY={value}") == f"API_KEY={REDACTED}"
        assert _redact_secrets(f"Token={value}") == f"Token={REDACTED}"
        assert _redact_secrets(f"SECRET={value}") == f"SECRET={REDACTED}"

    def test_boundary_preserved(self):
        """Contextual pattern preserves the keyword; only value is replaced."""
        value = "H" * 40
        result = _redact_secrets(f"api_key={value}")
        assert result.startswith("api_key=")
        assert result.endswith(REDACTED)
        assert result == f"api_key={REDACTED}"

    def test_base64_chars_in_value(self):
        """Base64 alphabet chars (/ +) in value are covered by the charset."""
        # 40-char string with base64 chars
        value = "A" * 20 + "/+" + "A" * 18
        result = _redact_secrets(f"api_key={value}")
        assert result == f"api_key={REDACTED}"


class TestRedactEdgeCases:
    """Non-secret strings, idempotency, and boundary conditions."""

    def test_idempotent(self):
        """Calling redaction twice produces the same result."""
        text = f"token={'A' * 40}"
        first = _redact_secrets(text)
        second = _redact_secrets(first)
        assert second == first
        assert REDACTED in first

    def test_already_redacted_string(self):
        """The [REDACTED] sentinel itself is not matched by any pattern."""
        assert _redact_secrets(f"see {REDACTED} here") == f"see {REDACTED} here"

    def test_no_match_passthrough(self):
        """Normal prose passes through unchanged."""
        assert _redact_secrets("The answer is 42.") == "The answer is 42."
        assert _redact_secrets("Hello, world!") == "Hello, world!"
        assert _redact_secrets("api_key short") == "api_key short"
        assert _redact_secrets("") == ""

    def test_empty_string(self):
        assert _redact_secrets("") == ""

    def test_short_value_not_secret(self):
        """A short string after a keyword= prefix is not a secret."""
        assert _redact_secrets("token=short") == "token=short"

    def test_mixed_content(self):
        """Real text with a secret-like prefix → only the secret is redacted."""
        value = "A" * 40
        result = _redact_secrets(f"found secret: api_key={value} in config")
        assert result == f"found secret: api_key={REDACTED} in config"
