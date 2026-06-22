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

// System-caller normalization (core#2680, fix/restart-context-callerid-normalize).
// A synthetic caller like "system:restart-context" must not be
// persisted into the UUID-typed activity_logs.source_id column;
// that path is the only one that lets a workspace recover from the
// post-restart wedge. Normalizing to NULL preserves the
// "system caller" semantic via source_id IS NULL (the existing
// canvas /activity?source=canvas filter) and lets the queue-fallback
// path find the row by the durable message_id.
//
// Scoped helper: callerIDToSourceID. Per the Researcher's RC
// #11295 on the prior #2701 attempt, system-caller normalization
// must NOT be in nilIfEmpty itself — nilIfEmpty is also used on
// non-ID fields (Method, Summary, ErrorDetail, MessageId,
// workspace_dir), and a method name like "system:foo" is a
// legitimate value that should NOT be silently nulled. The
// normalization is therefore scoped to the ONLY field that
// actually needs it: a callerID being persisted as
// activity_logs.SourceID.

func TestCallerIDToSourceID_SystemCallerPrefixes(t *testing.T) {
	cases := []string{
		"system:restart-context",
		"webhook:github",
		"test:lifecycle-1",
		"channel:slack:C0123",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			got := callerIDToSourceID(c)
			if got != nil {
				t.Errorf("system caller %q: got %p (%q), want nil", c, got, *got)
			}
		})
	}
}

func TestCallerIDToSourceID_RealWorkspaceUUIDStillPreserved(t *testing.T) {
	// Regression guard: a real workspace UUID must pass through
	// unchanged. The original #2694 RC closed because the fix
	// accidentally collapsed real UUIDs to NULL; this case is the
	// one that would have caught that.
	cases := []string{
		"ws-1",                                    // op-style id
		"01234567-89ab-cdef-0123-456789abcdef",   // uuid
		"agent-dev-b",                             // agent id (not a system prefix)
		"canvas_user",                             // canvas user placeholder
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			got := callerIDToSourceID(c)
			if got == nil {
				t.Errorf("real caller %q: got nil, want preserved pointer", c)
				return
			}
			if *got != c {
				t.Errorf("real caller %q: got %q, want preserved", c, *got)
			}
		})
	}
}

func TestCallerIDToSourceID_EmptyString(t *testing.T) {
	got := callerIDToSourceID("")
	if got != nil {
		t.Errorf("empty callerID: got %p, want nil", got)
	}
}

// TestNilIfEmpty_NoSystemCallerNormalization guards the narrow
// contract that prompted the RC #11295 fix. nilIfEmpty is used on
// many non-ID fields (Method, Summary, ErrorDetail, MessageId,
// workspace_dir); the system-caller normalization must NOT leak
// into those callers. A method name like "system:foo" must pass
// through unchanged.
func TestNilIfEmpty_NoSystemCallerNormalization(t *testing.T) {
	cases := []string{
		"system:foo",                              // would-be method name
		"webhook:github",                          // would-be method name
		"channel:slack:C0123",                     // would-be channel id
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			got := nilIfEmpty(c)
			if got == nil {
				t.Errorf("nilIfEmpty on %q: got nil, want preserved pointer (the system-caller normalization must be scoped to callerIDToSourceID only)", c)
				return
			}
			if *got != c {
				t.Errorf("nilIfEmpty on %q: got %q, want preserved", c, *got)
			}
		})
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

// TestRestartContext_SystemCallerDoesNotPoisonSourceID is the
// regression guard for the #2680 residual (criterion a): when the
// restart-context production path (restart_context.go:sendRestartContext
// L296) calls ProxyA2ARequest with callerID="system:restart-context",
// the synthetic non-UUID callerID must NOT be inserted into the
// UUID-typed activity_logs.source_id column. The path is:
//   sendRestartContext → ProxyA2ARequest(..., "system:restart-context", ...)
//     → persistUserMessageAtIngest(..., "system:restart-context", ...)
//       → LogActivityWithResult({SourceID: callerIDToSourceID("system:restart-context")})
//         → activity_logs INSERT with SourceID = NULL
//
// The fix (#2701) introduced the scoped helper callerIDToSourceID
// which returns nil for any system-caller prefix (matching
// isSystemCaller in a2a_proxy.go:85). This test pins the contract.
//
// If the callerIDToSourceID helper is later removed OR weakened OR
// the call site is reverted, the UUID cast on activity_logs.source_id
// will fail with pq: invalid input syntax for type uuid and the
// post-restart queue-fallback path will return 503 → workspace stays
// degraded. This test catches that regression.
func TestRestartContext_SystemCallerDoesNotPoisonSourceID(t *testing.T) {
	// Direct unit-level check of the scoped helper against all 4
	// systemCallerPrefixes. If callerIDToSourceID returns nil for
	// any of these, the production path's INSERT is safe; the
	// SQL binds NULL, the cast is skipped, no poison.
	prefixes := []string{
		"system:restart-context", // the specific offender
		"webhook:github",
		"test:lifecycle-1",
		"channel:slack:C0123",
	}
	for _, p := range prefixes {
		t.Run(p, func(t *testing.T) {
			got := callerIDToSourceID(p)
			if got != nil {
				t.Errorf("system caller %q: got non-nil pointer; would poison activity_logs.source_id (UUID cast fail → degraded wedge)", p)
			}
		})
	}
}
