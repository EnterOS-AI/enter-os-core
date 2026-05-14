package handlers

import (
	"encoding/json"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// nilIfEmpty tests
// ─────────────────────────────────────────────────────────────────────────────

func TestNilIfEmpty_EmptyString(t *testing.T) {
	got := nilIfEmpty("")
	if got != nil {
		t.Errorf("empty string: got %p, want nil", got)
	}
}

func TestNilIfEmpty_NonEmptyString(t *testing.T) {
	s := "hello"
	got := nilIfEmpty(s)
	if got == nil {
		t.Fatal("non-empty string: got nil, want pointer")
	}
	if *got != "hello" {
		t.Errorf("non-empty string: got %q, want %q", *got, "hello")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// extractToolTrace tests
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractToolTrace_EmptyBody(t *testing.T) {
	got := extractToolTrace(nil)
	if got != nil {
		t.Errorf("nil body: got %v, want nil", got)
	}
	got = extractToolTrace([]byte{})
	if got != nil {
		t.Errorf("empty body: got %v, want nil", got)
	}
}

func TestExtractToolTrace_InvalidJSON(t *testing.T) {
	got := extractToolTrace([]byte("not json"))
	if got != nil {
		t.Errorf("invalid JSON: got %v, want nil", got)
	}
}

func TestExtractToolTrace_NoResultKey(t *testing.T) {
	got := extractToolTrace([]byte(`{"error": "oops"}`))
	if got != nil {
		t.Errorf("no result key: got %v, want nil", got)
	}
}

func TestExtractToolTrace_NoMetadataKey(t *testing.T) {
	got := extractToolTrace([]byte(`{"result": {"data": {}}}`))
	if got != nil {
		t.Errorf("no metadata key: got %v, want nil", got)
	}
}

func TestExtractToolTrace_NoToolTraceKey(t *testing.T) {
	got := extractToolTrace([]byte(`{"result": {"metadata": {}}}`))
	if got != nil {
		t.Errorf("no tool_trace key: got %v, want nil", got)
	}
}

// extractToolTrace calls json.Unmarshal, which sets a RawMessage to nil when
// unmarshaling a JSON null value. The fix for mc#669 changes len(trace)==0
// to string(trace)=="[]" to avoid len(nil) panicking on null.
func TestExtractToolTrace_NullValue(t *testing.T) {
	// JSON null in tool_trace → RawMessage becomes nil → len would panic.
	// The fix checks string(trace)=="[]" which is safe on nil (returns false).
	body := []byte(`{"result": {"metadata": {"tool_trace": null}}}`)
	got := extractToolTrace(body)
	if got != nil {
		t.Errorf("null tool_trace: got %v, want nil", got)
	}
}

// "[]" unmarshaled into RawMessage is []byte("[]") — not nil, len=2.
// The fix returns nil for [] so empty tool_trace arrays don't surface as traces.
func TestExtractToolTrace_EmptyArray(t *testing.T) {
	body := []byte(`{"result": {"metadata": {"tool_trace": []}}}`)
	got := extractToolTrace(body)
	if got != nil {
		t.Errorf("empty array tool_trace: got %v, want nil", got)
	}
}

func TestExtractToolTrace_ValidNonEmpty(t *testing.T) {
	trace := []byte(`[{"name":"search","result":"done"}]`)
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"metadata": map[string]interface{}{
				"tool_trace": json.RawMessage(trace),
			},
		},
	})
	got := extractToolTrace(body)
	if got == nil {
		t.Fatal("valid non-empty trace: got nil, want the trace")
	}
	if string(got) != string(trace) {
		t.Errorf("valid trace: got %s, want %s", got, trace)
	}
}

// Document that the CURRENT code (len check) panics on null tool_trace.
// This test exists to signal when PR #669's fix lands: after the fix,
// the defer-recover will NOT trigger (panic goes away) and the
// post-recover assertion runs. While unfixed: the panic fires and

// ─────────────────────────────────────────────────────────────────────────────
// readUsageMap tests
// ─────────────────────────────────────────────────────────────────────────────

func TestReadUsageMap_NoUsageKey(t *testing.T) {
	m := map[string]json.RawMessage{}
	_, _, ok := readUsageMap(m)
	if ok {
		t.Error("no usage key: ok should be false")
	}
}

func TestReadUsageMap_InvalidUsageJSON(t *testing.T) {
	m := map[string]json.RawMessage{"usage": json.RawMessage(`"not an object"`)}
	_, _, ok := readUsageMap(m)
	if ok {
		t.Error("invalid usage JSON: ok should be false")
	}
}

func TestReadUsageMap_ZeroUsage(t *testing.T) {
	m := map[string]json.RawMessage{"usage": json.RawMessage(`{"input_tokens": 0, "output_tokens": 0}`)}
	_, _, ok := readUsageMap(m)
	if ok {
		t.Error("zero usage: ok should be false")
	}
}

func TestReadUsageMap_InputOnly(t *testing.T) {
	m := map[string]json.RawMessage{"usage": json.RawMessage(`{"input_tokens": 100, "output_tokens": 0}`)}
	in, out, ok := readUsageMap(m)
	if !ok {
		t.Fatal("input-only usage: ok should be true")
	}
	if in != 100 {
		t.Errorf("input tokens: got %d, want 100", in)
	}
	if out != 0 {
		t.Errorf("output tokens: got %d, want 0", out)
	}
}

func TestReadUsageMap_BothTokens(t *testing.T) {
	m := map[string]json.RawMessage{"usage": json.RawMessage(`{"input_tokens": 500, "output_tokens": 200}`)}
	in, out, ok := readUsageMap(m)
	if !ok {
		t.Fatal("both tokens: ok should be true")
	}
	if in != 500 || out != 200 {
		t.Errorf("tokens: got (%d, %d), want (500, 200)", in, out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// parseUsageFromA2AResponse tests
// ─────────────────────────────────────────────────────────────────────────────

func TestParseUsageFromA2AResponse_Empty(t *testing.T) {
	in, out := parseUsageFromA2AResponse(nil)
	if in != 0 || out != 0 {
		t.Errorf("nil: got (%d, %d), want (0, 0)", in, out)
	}
	in, out = parseUsageFromA2AResponse([]byte{})
	if in != 0 || out != 0 {
		t.Errorf("empty: got (%d, %d), want (0, 0)", in, out)
	}
}

func TestParseUsageFromA2AResponse_InvalidJSON(t *testing.T) {
	in, out := parseUsageFromA2AResponse([]byte("not json"))
	if in != 0 || out != 0 {
		t.Errorf("invalid JSON: got (%d, %d), want (0, 0)", in, out)
	}
}

func TestParseUsageFromA2AResponse_NoResultNoUsage(t *testing.T) {
	in, out := parseUsageFromA2AResponse([]byte(`{"id": 1}`))
	if in != 0 || out != 0 {
		t.Errorf("no result/usage: got (%d, %d), want (0, 0)", in, out)
	}
}

func TestParseUsageFromA2AResponse_ResultUsage(t *testing.T) {
	body := []byte(`{"result": {"usage": {"input_tokens": 42, "output_tokens": 7}}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 42 || out != 7 {
		t.Errorf("result usage: got (%d, %d), want (42, 7)", in, out)
	}
}

func TestParseUsageFromA2AResponse_ResultUsageWinsOverTopLevel(t *testing.T) {
	// JSON-RPC result.usage takes precedence over top-level usage.
	body := []byte(`{"result": {"usage": {"input_tokens": 42, "output_tokens": 7}}, "usage": {"input_tokens": 99, "output_tokens": 99}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 42 || out != 7 {
		t.Errorf("result usage should win: got (%d, %d), want (42, 7)", in, out)
	}
}

func TestParseUsageFromA2AResponse_TopLevelFallback(t *testing.T) {
	// Direct (non-JSON-RPC) response: usage at top level.
	body := []byte(`{"usage": {"input_tokens": 11, "output_tokens": 13}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 11 || out != 13 {
		t.Errorf("top-level usage: got (%d, %d), want (11, 13)", in, out)
	}
}

func TestParseUsageFromA2AResponse_ZeroValuesInResult(t *testing.T) {
	// Zero usage in result.result.usage: returns (0, 0) — no panic.
	body := []byte(`{"result": {"usage": {"input_tokens": 0, "output_tokens": 0}}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 0 || out != 0 {
		t.Errorf("zero usage: got (%d, %d), want (0, 0)", in, out)
	}
}

func TestParseUsageFromA2AResponse_MissingTokensInUsageObject(t *testing.T) {
	// usage object exists but tokens are absent — returns (0, 0).
	body := []byte(`{"result": {"usage": {"other_field": 5}}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 0 || out != 0 {
		t.Errorf("missing tokens: got (%d, %d), want (0, 0)", in, out)
	}
}
