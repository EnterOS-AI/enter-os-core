"""Tests for the #328 fix — /transcript endpoint must fail-CLOSED when
the workspace auth token is not yet on disk.

Prior behaviour (regressed in #287): `if expected:` skipped the auth
check when `get_token()` returned None, so any container on
`molecule-core-net` could read the full session log during the
bootstrap window. The fix lifts the guard into transcript_auth.py for
testability.
"""

from transcript_auth import transcript_authorized


def test_missing_token_fails_closed():
    # #328 regression: None token MUST return False (was the fail-open bug).
    assert transcript_authorized(None, "Bearer anything") is False


def test_empty_token_fails_closed():
    # Empty string is as-bad-as None — also a fail-closed case.
    assert transcript_authorized("", "Bearer anything") is False


def test_valid_bearer_passes():
    assert transcript_authorized("tok-123", "Bearer tok-123") is True


def test_wrong_bearer_fails():
    assert transcript_authorized("tok-123", "Bearer other-token") is False


def test_missing_header_fails_even_when_expected_is_set():
    # Empty auth header (not sent at all) must fail — client forgot.
    assert transcript_authorized("tok-123", "") is False


def test_case_sensitive_bearer_prefix():
    # Strict equality matches platform wsauth.BearerTokenFromHeader
    # which is also case-sensitive on the "Bearer " prefix. Documenting
    # the behavior so a future refactor is a conscious choice.
    assert transcript_authorized("tok-123", "bearer tok-123") is False


def test_extra_whitespace_in_header_fails():
    # Strict equality — accidental double space between Bearer and token
    # must fail so an adversary can't test fuzzed variations.
    assert transcript_authorized("tok-123", "Bearer  tok-123") is False
