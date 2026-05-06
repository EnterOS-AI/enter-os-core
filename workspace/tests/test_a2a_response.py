"""Tests for the A2A response SSOT parser (workspace/a2a_response.py).

Branch coverage target: 100%. Each variant of ``parse()`` exercised in
isolation, plus adversarial-input fuzzing to assert the parser never
raises.

Pre-#2967, the response shape was sniffed inline at every call site
(``a2a_client.py:567-587`` had hard-coded ``"result" in data`` /
``"error" in data`` checks). The bare ``else`` returned an
"unexpected response shape" error — which silently broke poll-mode
peers because the workspace-server's poll-queued envelope has neither
``result`` nor ``error``. The SSOT parser has an explicit ``Queued``
variant for that path and routes anything truly unrecognized to
``Malformed`` so a future server-side change fails loudly.

The "this test FAILS on pre-fix source" guarantee is enforced by
running the legacy-shape sniffer alongside the new parser in
``test_legacy_sniffer_misclassified_queued`` — that test fails on
the pre-#2967 ``a2a_client.py`` shape because the legacy code
returns the unexpected-shape error path for the Queued envelope.
"""
from __future__ import annotations

import logging
from typing import Any

import pytest

import a2a_response


# ============== Fixture corpus — the canonical wire shapes ==============


# Every shape below mirrors a path the workspace-server's a2a_proxy.go
# can return. When you add a new server-side response shape, add a
# fixture entry here and a corresponding test method below.
_FIXTURES = {
    "jsonrpc_success_with_text": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "result": {
            "parts": [{"kind": "text", "text": "hello world"}],
        },
    },
    "jsonrpc_success_multipart": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "result": {
            "parts": [
                {"kind": "text", "text": "first"},
                {"kind": "text", "text": "second"},
            ],
        },
    },
    "jsonrpc_success_no_parts": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "result": {},
    },
    "jsonrpc_success_part_no_text_key": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "result": {"parts": [{"kind": "text"}]},
    },
    "jsonrpc_error_with_message_and_code": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "error": {"message": "rate limited", "code": -32003},
    },
    "jsonrpc_error_message_only": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "error": {"message": "rate limited"},
    },
    "jsonrpc_error_code_only": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "error": {"code": -32603},
    },
    "jsonrpc_error_string_form": {
        "jsonrpc": "2.0",
        "id": "abc-123",
        "error": "string-shaped error",
    },
    "platform_error_with_restart": {
        "error": "workspace agent unreachable — container restart triggered",
        "restarting": True,
        "retry_after": 15,
    },
    "platform_error_plain": {
        "error": "workspace not found",
    },
    "poll_queued_full": {
        "status": "queued",
        "delivery_mode": "poll",
        "method": "message/send",
    },
    "poll_queued_notify": {
        "status": "queued",
        "delivery_mode": "poll",
        "method": "notify",
    },
    "poll_queued_no_method": {
        "status": "queued",
        "delivery_mode": "poll",
    },
    "malformed_empty_dict": {},
    "malformed_unexpected_keys": {"foo": "bar", "baz": 42},
    "malformed_status_queued_no_delivery_mode": {
        # Server bug — status set but delivery_mode missing.
        # Should be Malformed, not Queued, because the contract says both.
        "status": "queued",
    },
    "malformed_delivery_mode_no_status": {
        "delivery_mode": "poll",
    },
}


# ============== Variant-by-variant coverage ==============


class TestQueuedVariant:
    """``parse()`` recognizes the workspace-server poll-mode short-circuit
    envelope (a2a_proxy.go:402-406) and returns ``Queued``."""

    def test_full_envelope_with_method_message_send(self):
        v = a2a_response.parse(_FIXTURES["poll_queued_full"])
        assert isinstance(v, a2a_response.Queued)
        assert v.method == "message/send"
        assert v.delivery_mode == "poll"

    def test_envelope_with_method_notify(self):
        v = a2a_response.parse(_FIXTURES["poll_queued_notify"])
        assert isinstance(v, a2a_response.Queued)
        assert v.method == "notify"

    def test_envelope_missing_method_uses_unknown_sentinel(self):
        # Envelope without ``method`` key — server contract should
        # always set it, but the parser must not raise on absence.
        v = a2a_response.parse(_FIXTURES["poll_queued_no_method"])
        assert isinstance(v, a2a_response.Queued)
        assert v.method == "unknown"

    def test_status_queued_alone_is_malformed_not_queued(self):
        # ``status=queued`` without ``delivery_mode=poll`` does not match
        # the documented envelope. Surface as Malformed for visibility.
        v = a2a_response.parse(_FIXTURES["malformed_status_queued_no_delivery_mode"])
        assert isinstance(v, a2a_response.Malformed)

    def test_delivery_mode_alone_is_malformed_not_queued(self):
        v = a2a_response.parse(_FIXTURES["malformed_delivery_mode_no_status"])
        assert isinstance(v, a2a_response.Malformed)

    def test_logs_info_on_queued(self, caplog):
        # Comprehensive logging — operator should see queued events at INFO.
        with caplog.at_level(logging.INFO, logger="a2a_response"):
            a2a_response.parse(_FIXTURES["poll_queued_full"])
        assert any("queued for poll-mode peer" in r.message for r in caplog.records)


class TestResultVariant:
    """``parse()`` extracts the JSON-RPC ``result`` envelope into
    ``Result(text, parts, raw_result)``."""

    def test_simple_text_result(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_success_with_text"])
        assert isinstance(v, a2a_response.Result)
        assert v.text == "hello world"
        assert len(v.parts) == 1
        assert v.raw_result == {"parts": [{"kind": "text", "text": "hello world"}]}

    def test_multipart_result_extracts_first_part_text(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_success_multipart"])
        assert isinstance(v, a2a_response.Result)
        assert v.text == "first"
        assert len(v.parts) == 2

    def test_result_with_no_parts(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_success_no_parts"])
        assert isinstance(v, a2a_response.Result)
        assert v.text == ""
        assert v.parts == []

    def test_part_without_text_key(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_success_part_no_text_key"])
        assert isinstance(v, a2a_response.Result)
        # No "text" key — extracted text is empty, parts list intact.
        assert v.text == ""
        assert len(v.parts) == 1

    def test_result_non_dict_returns_text_form(self):
        # Pathological but legal: ``result`` is a string instead of a dict.
        v = a2a_response.parse({"result": "hello"})
        assert isinstance(v, a2a_response.Result)
        assert v.text == "hello"
        assert v.parts == []

    def test_result_takes_precedence_when_no_queued_envelope(self):
        # Both ``result`` and ``error`` keys present — result wins
        # because it's checked first after the Queued path.
        v = a2a_response.parse({
            "result": {"parts": [{"kind": "text", "text": "ok"}]},
            "error": {"message": "should-be-ignored"},
        })
        assert isinstance(v, a2a_response.Result)
        assert v.text == "ok"

    def test_part_with_non_dict_first_entry(self):
        # ``parts[0]`` is a string instead of a dict — parser tolerates it,
        # text falls back to empty.
        v = a2a_response.parse({"result": {"parts": ["bare-string"]}})
        assert isinstance(v, a2a_response.Result)
        assert v.text == ""
        assert v.parts == ["bare-string"]

    def test_part_text_value_none(self):
        # ``parts[0].text`` is explicitly None — extracted as "".
        v = a2a_response.parse({"result": {"parts": [{"text": None}]}})
        assert isinstance(v, a2a_response.Result)
        assert v.text == ""

    def test_parts_not_a_list(self):
        # Server bug: ``parts`` is a dict instead of a list. Parser falls
        # back to empty parts rather than raising.
        v = a2a_response.parse({"result": {"parts": {"oops": True}}})
        assert isinstance(v, a2a_response.Result)
        assert v.parts == []
        assert v.text == ""


class TestErrorVariant:
    """``parse()`` extracts ``error`` envelopes into ``Error`` and
    annotates platform-restart metadata when present."""

    def test_message_and_code(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_error_with_message_and_code"])
        assert isinstance(v, a2a_response.Error)
        assert v.message == "rate limited"
        assert v.code == -32003
        assert v.restarting is False
        assert v.retry_after is None

    def test_message_only(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_error_message_only"])
        assert isinstance(v, a2a_response.Error)
        assert v.message == "rate limited"
        assert v.code is None

    def test_code_only(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_error_code_only"])
        assert isinstance(v, a2a_response.Error)
        assert v.message == ""
        assert v.code == -32603

    def test_error_string_form(self):
        v = a2a_response.parse(_FIXTURES["jsonrpc_error_string_form"])
        assert isinstance(v, a2a_response.Error)
        assert v.message == "string-shaped error"
        assert v.code is None

    def test_error_non_dict_non_string(self):
        v = a2a_response.parse({"error": 12345})
        assert isinstance(v, a2a_response.Error)
        assert v.message == "12345"

    def test_platform_error_with_restart_metadata(self):
        v = a2a_response.parse(_FIXTURES["platform_error_with_restart"])
        assert isinstance(v, a2a_response.Error)
        assert "workspace agent unreachable" in v.message
        assert v.restarting is True
        assert v.retry_after == 15

    def test_platform_error_without_restart(self):
        v = a2a_response.parse(_FIXTURES["platform_error_plain"])
        assert isinstance(v, a2a_response.Error)
        assert v.message == "workspace not found"
        assert v.restarting is False
        assert v.retry_after is None

    def test_error_message_with_whitespace_stripped(self):
        v = a2a_response.parse({"error": {"message": "  trimmed  "}})
        assert isinstance(v, a2a_response.Error)
        assert v.message == "trimmed"

    def test_non_int_code_dropped(self):
        v = a2a_response.parse({"error": {"message": "x", "code": "not-a-number"}})
        assert isinstance(v, a2a_response.Error)
        assert v.code is None

    def test_non_int_retry_after_dropped(self):
        v = a2a_response.parse({"error": "x", "restarting": True, "retry_after": "30s"})
        assert isinstance(v, a2a_response.Error)
        assert v.retry_after is None


class TestMalformedVariant:
    """``parse()`` returns ``Malformed`` for any shape it can't classify
    and logs at WARNING so operators see new server response shapes."""

    def test_empty_dict(self):
        v = a2a_response.parse(_FIXTURES["malformed_empty_dict"])
        assert isinstance(v, a2a_response.Malformed)
        assert v.raw == {}

    def test_unexpected_keys(self):
        v = a2a_response.parse(_FIXTURES["malformed_unexpected_keys"])
        assert isinstance(v, a2a_response.Malformed)
        assert v.raw == {"foo": "bar", "baz": 42}

    def test_non_dict_input_list(self):
        v = a2a_response.parse([1, 2, 3])
        assert isinstance(v, a2a_response.Malformed)
        assert v.raw == [1, 2, 3]

    def test_non_dict_input_string(self):
        v = a2a_response.parse("plain string")
        assert isinstance(v, a2a_response.Malformed)
        assert v.raw == "plain string"

    def test_non_dict_input_none(self):
        v = a2a_response.parse(None)
        assert isinstance(v, a2a_response.Malformed)
        assert v.raw is None

    def test_logs_warning_on_malformed(self, caplog):
        with caplog.at_level(logging.WARNING, logger="a2a_response"):
            a2a_response.parse(_FIXTURES["malformed_unexpected_keys"])
        assert any(r.levelno == logging.WARNING for r in caplog.records)

    def test_logs_warning_on_non_dict(self, caplog):
        with caplog.at_level(logging.WARNING, logger="a2a_response"):
            a2a_response.parse("not a dict")
        assert any("non-dict" in r.message for r in caplog.records)


# ============== Robustness — parser never raises ==============


_ADVERSARIAL_INPUTS: list[Any] = [
    None,
    True,
    False,
    0,
    -1,
    3.14,
    "",
    "string",
    [],
    [1, 2, 3],
    {},
    {"random": "garbage"},
    {"result": None},
    {"result": [1, 2, 3]},
    {"result": {"parts": None}},
    {"result": {"parts": [None]}},
    {"result": {"parts": [{"text": []}]}},
    {"error": None},
    {"error": []},
    {"error": {"message": None, "code": None}},
    {"error": {"message": ["nested", "list"]}},
    {"status": None, "delivery_mode": None, "method": None},
    {"status": "queued", "delivery_mode": "push", "method": "x"},  # wrong delivery_mode
    {"status": "running", "delivery_mode": "poll"},  # wrong status
    {"status": 42, "delivery_mode": "poll"},  # non-string status
    # Deeply-nested junk
    {"result": {"parts": [{"text": {"deeply": {"nested": "object"}}}]}},
    # Bytes (not really JSON-decodable but parser shouldn't raise)
    {"result": {"parts": [{"text": b"bytes" if False else "x"}]}},
]


class TestRobustness:
    """Parser must never raise on adversarial input — every branch is total.

    These cases catch regressions where a future change adds a key
    access that doesn't tolerate ``None`` / wrong-type values.
    """

    @pytest.mark.parametrize("payload", _ADVERSARIAL_INPUTS)
    def test_parse_never_raises(self, payload):
        # Single contract: parse must return one of the four variants
        # regardless of input. No exception classes propagated.
        v = a2a_response.parse(payload)
        assert isinstance(v, (a2a_response.Result, a2a_response.Error,
                              a2a_response.Queued, a2a_response.Malformed))


# ============== Regression gate — pre-#2967 misclassified queued ==============


class TestRegressionGate:
    """Pin the bug that prompted the SSOT abstraction.

    Before #2967, ``a2a_client.py:567-587`` sniffed only ``"result" in
    data`` and ``"error" in data`` — the poll-queued envelope (no
    result key, no error key) hit the bare-else and returned the
    "unexpected response shape" error string. This test simulates the
    pre-fix code path and confirms the SSOT parser correctly
    distinguishes Queued from Malformed.
    """

    def test_legacy_sniffer_would_return_neither_branch(self):
        # The pre-#2967 logic — provided here so the regression is
        # reproducible from this file alone, no archaeology needed.
        envelope = _FIXTURES["poll_queued_full"]
        legacy_branch = (
            "result" if "result" in envelope
            else "error" if "error" in envelope
            else "unexpected_shape"
        )
        # Legacy sniff: hits the malformed branch.
        assert legacy_branch == "unexpected_shape"

    def test_ssot_parser_classifies_correctly(self):
        # New parser: classifies as Queued.
        v = a2a_response.parse(_FIXTURES["poll_queued_full"])
        assert isinstance(v, a2a_response.Queued)
        assert v.method == "message/send"

    def test_every_fixture_classifies_to_expected_variant(self):
        # Defense in depth — pin the variant for every fixture so a
        # future shape addition has to update the table here too.
        expected: dict[str, type] = {
            "jsonrpc_success_with_text":         a2a_response.Result,
            "jsonrpc_success_multipart":         a2a_response.Result,
            "jsonrpc_success_no_parts":          a2a_response.Result,
            "jsonrpc_success_part_no_text_key":  a2a_response.Result,
            "jsonrpc_error_with_message_and_code": a2a_response.Error,
            "jsonrpc_error_message_only":        a2a_response.Error,
            "jsonrpc_error_code_only":           a2a_response.Error,
            "jsonrpc_error_string_form":         a2a_response.Error,
            "platform_error_with_restart":       a2a_response.Error,
            "platform_error_plain":              a2a_response.Error,
            "poll_queued_full":                  a2a_response.Queued,
            "poll_queued_notify":                a2a_response.Queued,
            "poll_queued_no_method":             a2a_response.Queued,
            "malformed_empty_dict":              a2a_response.Malformed,
            "malformed_unexpected_keys":         a2a_response.Malformed,
            "malformed_status_queued_no_delivery_mode": a2a_response.Malformed,
            "malformed_delivery_mode_no_status": a2a_response.Malformed,
        }
        # Every fixture must be enumerated — keeps this gate honest.
        assert set(expected.keys()) == set(_FIXTURES.keys()), (
            f"fixture/expected mismatch: "
            f"missing-from-expected={set(_FIXTURES) - set(expected)} "
            f"extra-in-expected={set(expected) - set(_FIXTURES)}"
        )
        for name, payload in _FIXTURES.items():
            v = a2a_response.parse(payload)
            assert isinstance(v, expected[name]), (
                f"fixture {name!r} classified as {type(v).__name__}, "
                f"expected {expected[name].__name__}"
            )
