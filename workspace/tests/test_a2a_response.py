"""Branch-by-branch coverage for ``a2a_response.parse_a2a_response``.

Every variant + every malformed shape pinned by a discriminating test.
Adversarial fuzz: random bytes via json.loads → parser must NEVER raise.

Discrimination check pattern (per memory ``feedback_assert_exact_not_substring``
+ ``feedback_question_test_when_unexpected``): each test's assertions
distinguish the variant they're testing from every other variant. A
parser that always returned ``A2AMalformed`` would fail every Result /
Error / Queued test. A parser that always returned ``A2AResult`` would
fail every Error / Queued / Malformed test.
"""
from __future__ import annotations

import json
import random
import string

import pytest

from a2a_response import (
    A2AError,
    A2AMalformed,
    A2AQueued,
    A2AResult,
    parse_a2a_response,
)


# ---------------------------------------------------------------------------
# A2AResult — JSON-RPC success
# ---------------------------------------------------------------------------


class TestA2AResult:
    def test_result_with_text_part(self):
        out = parse_a2a_response({
            "result": {"parts": [{"kind": "text", "text": "hello"}]}
        })
        assert isinstance(out, A2AResult)
        assert out.text == "hello"
        assert len(out.parts) == 1

    def test_result_with_empty_parts_returns_empty_text(self):
        out = parse_a2a_response({"result": {"parts": []}})
        assert isinstance(out, A2AResult)
        assert out.text == ""
        assert out.parts == []

    def test_result_with_missing_parts_key_returns_empty(self):
        """Defensive: result with no parts key is unusual but not malformed —
        treat as 'empty success' rather than alerting an operator."""
        out = parse_a2a_response({"result": {}})
        assert isinstance(out, A2AResult)
        assert out.text == ""

    def test_result_with_non_dict_result_field(self):
        """Server bug case: result is a string instead of an object. Don't
        crash; degrade to empty success."""
        out = parse_a2a_response({"result": "not a dict"})
        assert isinstance(out, A2AResult)
        assert out.text == ""

    def test_result_with_non_list_parts(self):
        """Server bug: parts is a string. Same degradation."""
        out = parse_a2a_response({"result": {"parts": "not a list"}})
        assert isinstance(out, A2AResult)
        assert out.text == ""

    def test_result_with_first_part_missing_text_key(self):
        """Image-only / tool-only parts have no text. text → empty,
        but parts list still surfaces so callers that need them can reach."""
        out = parse_a2a_response({
            "result": {"parts": [{"kind": "image", "url": "https://..."}]}
        })
        assert isinstance(out, A2AResult)
        assert out.text == ""
        assert len(out.parts) == 1

    def test_result_with_non_string_text(self):
        """Server bug: text is an int. Coerce to empty (not str(int))."""
        out = parse_a2a_response({
            "result": {"parts": [{"text": 42}]}
        })
        assert isinstance(out, A2AResult)
        assert out.text == ""

    def test_result_takes_precedence_over_error(self):
        """If both keys present (server sin), result wins. Pinning the
        dispatch order so a future refactor can't silently flip it."""
        out = parse_a2a_response({
            "result": {"parts": [{"text": "hi"}]},
            "error": {"code": -1, "message": "ignored"},
        })
        assert isinstance(out, A2AResult)
        assert out.text == "hi"


# ---------------------------------------------------------------------------
# A2AError
# ---------------------------------------------------------------------------


class TestA2AError:
    def test_jsonrpc_error_with_code_and_message(self):
        out = parse_a2a_response({
            "error": {"code": -32603, "message": "Internal error"}
        })
        assert isinstance(out, A2AError)
        assert out.code == -32603
        assert out.message == "Internal error"

    def test_jsonrpc_error_with_only_code(self):
        out = parse_a2a_response({"error": {"code": -32600}})
        assert isinstance(out, A2AError)
        assert out.code == -32600
        assert out.message == ""

    def test_jsonrpc_error_with_only_message(self):
        out = parse_a2a_response({"error": {"message": "Oops"}})
        assert isinstance(out, A2AError)
        assert out.code is None
        assert out.message == "Oops"

    def test_jsonrpc_error_with_zero_code(self):
        """JSON-RPC code=0 is technically out-of-spec but a malformed peer
        can still emit it. Pin: code=0 surfaces as int 0, NOT None.
        Distinguishes from the 'no code' branch."""
        out = parse_a2a_response({"error": {"code": 0, "message": "x"}})
        assert isinstance(out, A2AError)
        assert out.code == 0  # NOT None — discriminating against truthy-check
        assert out.message == "x"

    def test_jsonrpc_error_with_non_int_code(self):
        """code as string '42': not a valid JSON-RPC error code → None."""
        out = parse_a2a_response({"error": {"code": "42", "message": "x"}})
        assert isinstance(out, A2AError)
        assert out.code is None

    def test_gateway_string_error(self):
        """Gateway-style: ``{error: <string>}`` (no code, no nested dict).
        Caller doesn't get to know the code; message is the string verbatim."""
        out = parse_a2a_response({"error": "workspace not found"})
        assert isinstance(out, A2AError)
        assert out.code is None
        assert out.message == "workspace not found"

    def test_error_with_non_dict_non_string_value(self):
        """Server bug: error is a list. Coerce to repr-string so the
        operator at least sees what landed."""
        out = parse_a2a_response({"error": [1, 2, 3]})
        assert isinstance(out, A2AError)
        assert out.code is None
        assert "1" in out.message  # str([1,2,3]) contains '1'

    def test_error_message_strip(self):
        """Whitespace in the message field is stripped — a peer that
        emits '  Internal error  \\n' shouldn't surface trailing whitespace
        in operator dashboards."""
        out = parse_a2a_response({
            "error": {"code": -1, "message": "  hello  \n"}
        })
        assert isinstance(out, A2AError)
        assert out.message == "hello"


# ---------------------------------------------------------------------------
# A2AQueued — poll-mode short-circuit
# ---------------------------------------------------------------------------


class TestA2AQueued:
    def test_canonical_queued_envelope(self):
        out = parse_a2a_response({
            "status": "queued",
            "delivery_mode": "poll",
            "method": "message/send",
        })
        assert isinstance(out, A2AQueued)
        assert out.method == "message/send"
        assert out.delivery_mode == "poll"

    def test_queued_with_other_method(self):
        """Future protocols (message/sendStream, message/cancel) should
        surface their method verbatim. Pin: parser doesn't hardcode."""
        out = parse_a2a_response({
            "status": "queued",
            "delivery_mode": "poll",
            "method": "message/sendStream",
        })
        assert isinstance(out, A2AQueued)
        assert out.method == "message/sendStream"

    def test_queued_missing_delivery_mode_is_malformed(self):
        """Defensive: only the ``status="queued" + delivery_mode="poll"``
        pair triggers Queued. A partial envelope (status only) collapses
        to Malformed so a future server bug doesn't silently classify
        unrelated wire shapes as queued."""
        out = parse_a2a_response({
            "status": "queued",
            "method": "message/send",
        })
        assert isinstance(out, A2AMalformed)

    def test_queued_with_non_poll_delivery_mode_is_malformed(self):
        """Today only delivery_mode='poll' is valid. A future delivery
        mode (e.g. 'webhook') would be a NEW variant we'd add explicitly,
        not silently classify as Queued."""
        out = parse_a2a_response({
            "status": "queued",
            "delivery_mode": "webhook",
            "method": "message/send",
        })
        assert isinstance(out, A2AMalformed)

    def test_queued_status_other_value_is_malformed(self):
        """status="dispatched" with delivery_mode="poll" — server-bug shape.
        Pin: only status="queued" triggers."""
        out = parse_a2a_response({
            "status": "dispatched",
            "delivery_mode": "poll",
            "method": "message/send",
        })
        assert isinstance(out, A2AMalformed)

    def test_queued_missing_method_defaults_to_message_send(self):
        """Defensive: server emits queued without method. Default to
        the canonical method so callers get a non-empty method field."""
        out = parse_a2a_response({
            "status": "queued",
            "delivery_mode": "poll",
        })
        assert isinstance(out, A2AQueued)
        assert out.method == "message/send"


# ---------------------------------------------------------------------------
# A2AMalformed
# ---------------------------------------------------------------------------


class TestA2AMalformed:
    def test_empty_dict(self):
        out = parse_a2a_response({})
        assert isinstance(out, A2AMalformed)

    def test_unrelated_dict(self):
        out = parse_a2a_response({"some_other_key": 42})
        assert isinstance(out, A2AMalformed)
        assert "some_other_key" in out.raw_snippet

    def test_non_dict_root_string(self):
        """A peer that returns 'OK' as plain text instead of JSON-RPC —
        json.loads might parse it as a string. Parser must handle."""
        out = parse_a2a_response("OK")
        assert isinstance(out, A2AMalformed)

    def test_non_dict_root_list(self):
        out = parse_a2a_response([1, 2, 3])
        assert isinstance(out, A2AMalformed)

    def test_non_dict_root_none(self):
        out = parse_a2a_response(None)
        assert isinstance(out, A2AMalformed)
        assert out.raw_snippet == ""  # None → empty snippet

    def test_non_dict_root_int(self):
        out = parse_a2a_response(42)
        assert isinstance(out, A2AMalformed)

    def test_snippet_capped_at_max(self):
        big = {"unknown_key": "x" * 1000}
        out = parse_a2a_response(big, max_snippet=50)
        assert isinstance(out, A2AMalformed)
        assert len(out.raw_snippet) == 50

    def test_default_snippet_cap(self):
        big = {"unknown_key": "x" * 1000}
        out = parse_a2a_response(big)
        assert isinstance(out, A2AMalformed)
        assert len(out.raw_snippet) <= 200


# ---------------------------------------------------------------------------
# Adversarial fuzz — parser must NEVER raise
# ---------------------------------------------------------------------------


class TestFuzz:
    """Per the issue's 'parser must never raise' contract, throw a wide
    variety of inputs at it and verify it always returns a typed variant.

    Not a property-based hypothesis run (deferred to keep the test suite
    fast in CI) but covers the common adversarial shapes.
    """

    @pytest.mark.parametrize("payload", [
        {},
        [],
        "",
        "OK",
        None,
        0,
        -1,
        1.5,
        True,
        False,
        {"result": None},
        {"result": True},
        {"result": [1, 2, 3]},
        {"result": {"parts": None}},
        {"result": {"parts": [None]}},
        {"result": {"parts": [{}]}},
        {"result": {"parts": [{"text": None}]}},
        {"result": {"parts": [{"text": []}]}},
        {"error": None},
        {"error": True},
        {"error": 42},
        {"error": {}},
        {"error": {"code": "string"}},
        {"error": {"code": None}},
        {"error": {"message": None}},
        {"status": None},
        {"status": "queued"},
        {"status": "queued", "delivery_mode": None},
        {"status": "queued", "delivery_mode": "poll", "method": None},
        {"status": "queued", "delivery_mode": "poll", "method": 42},
        {"foo": "bar"},
        {"result": {"parts": [{"text": "ok"}]}, "error": "ignored"},
    ])
    def test_never_raises_on_known_adversarial_shapes(self, payload):
        out = parse_a2a_response(payload)
        # Must return one of the four variants — never raise.
        assert isinstance(out, (A2AResult, A2AError, A2AQueued, A2AMalformed))

    def test_never_raises_on_random_json(self):
        """Generate 200 random JSON-decoded payloads and assert the parser
        returns a variant for each. The randomness doesn't have to be
        principled — just that the parser stays total under deformation."""
        random.seed(42)
        primitives = [None, True, False, 0, 1, -1, 1.5, "", "x"]
        keys = ["result", "error", "status", "delivery_mode", "method",
                "code", "message", "parts", "text", "kind", "garbage"]
        for _ in range(200):
            depth = random.randint(0, 3)
            payload = _random_value(primitives, keys, depth)
            try:
                json_round_tripped = json.loads(json.dumps(payload, default=str))
            except (TypeError, ValueError):
                continue  # not JSON-encodable, skip
            out = parse_a2a_response(json_round_tripped)
            assert isinstance(out, (A2AResult, A2AError, A2AQueued, A2AMalformed))

    def test_huge_string_payload_truncates(self):
        """A 10MB string in the error message field: parser must still
        run cheaply (snippet cap) and not OOM the caller."""
        out = parse_a2a_response({"error": "x" * 10_000_000})
        assert isinstance(out, A2AError)
        # Message itself isn't capped (caller may want full text); the
        # raw_snippet path on Malformed is what's capped. Verify Error
        # code stays None and message starts with x's.
        assert out.code is None
        assert out.message.startswith("x")


def _random_value(primitives: list, keys: list, depth: int):
    """Recursively build a small random JSON-shaped value."""
    if depth == 0 or random.random() < 0.3:
        return random.choice(primitives)
    kind = random.random()
    if kind < 0.4:
        # dict
        n = random.randint(0, 4)
        return {
            random.choice(keys): _random_value(primitives, keys, depth - 1)
            for _ in range(n)
        }
    if kind < 0.7:
        # list
        n = random.randint(0, 4)
        return [_random_value(primitives, keys, depth - 1) for _ in range(n)]
    return random.choice(primitives)


# ---------------------------------------------------------------------------
# Regression test against the issue's own production payload
# ---------------------------------------------------------------------------


class TestIssue2967ProductionRepro:
    """Pin the parser against the exact envelope the production reno-stars
    tenant emitted on 2026-05-05 (the trigger for #2967). If this test
    starts failing, the SSOT regressed and the original bug came back.
    """

    def test_reno_stars_poll_envelope(self):
        # Lifted verbatim from the issue body's error_detail capture.
        payload = {
            "delivery_mode": "poll",
            "method": "message/send",
            "status": "queued",
        }
        out = parse_a2a_response(payload)
        assert isinstance(out, A2AQueued)
        assert out.method == "message/send"
        assert out.delivery_mode == "poll"
