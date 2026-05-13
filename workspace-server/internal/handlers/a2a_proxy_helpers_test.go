package handlers

// a2a_proxy_helpers_test.go — unit tests for extractToolTrace (the only
// untested pure function in a2a_proxy_helpers.go). The function parses JSON
// so tests use real JSON without any DB or HTTP mocking.

import (
	"encoding/json"
	"testing"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
)

// TestExtractToolTrace_HappyPath verifies that a well-formed JSON-RPC result
// with a metadata.tool_trace field returns it as json.RawMessage.
func TestExtractToolTrace_HappyPath(t *testing.T) {
	trace := json.RawMessage(`[{"tool":"bash","input":"ls"}]`)
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"metadata": map[string]interface{}{
				"tool_trace": trace,
			},
		},
	}
	body, _ := json.Marshal(resp)
	got := extractToolTrace(body)
	if got == nil {
		t.Fatal("extractToolTrace returned nil, expected the trace")
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("returned value is not valid JSON: %v", err)
	}
	if len(parsed) != 1 || parsed[0]["tool"] != "bash" {
		t.Errorf("unexpected trace content: %v", parsed)
	}
}

// TestExtractToolTrace_ResultUsageShape tests a result object that has usage
// (common A2A response shape) but no tool_trace — should return nil.
func TestExtractToolTrace_ResultHasUsageNoTrace(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"metadata": map[string]interface{}{
				"usage": map[string]int64{"input_tokens": 100, "output_tokens": 200},
			},
		},
	}
	body, _ := json.Marshal(resp)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil when no tool_trace, got: %s", string(got))
	}
}

// TestExtractToolTrace_NoResultKey verifies that a response without a "result"
// key returns nil.
func TestExtractToolTrace_NoResultKey(t *testing.T) {
	resp := map[string]interface{}{
		"error": map[string]string{"code": "-32600", "message": "Invalid Request"},
	}
	body, _ := json.Marshal(resp)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for error response, got: %s", string(got))
	}
}

// TestExtractToolTrace_ResultNotAnObject verifies that a result that is not
// a JSON object (e.g., null) returns nil without panicking.
func TestExtractToolTrace_ResultNotAnObject(t *testing.T) {
	body := []byte(`{"result": null}`)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for null result, got: %s", string(got))
	}
}

// TestExtractToolTrace_NoMetadata verifies that a result object without
// metadata returns nil.
func TestExtractToolTrace_NoMetadata(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"message": "hello",
		},
	}
	body, _ := json.Marshal(resp)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for result without metadata, got: %s", string(got))
	}
}

// TestExtractToolTrace_MetadataNotAnObject verifies that a metadata field that
// is not a JSON object returns nil without panicking.
func TestExtractToolTrace_MetadataNotAnObject(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"metadata": "not an object",
		},
	}
	body, _ := json.Marshal(resp)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for non-object metadata, got: %s", string(got))
	}
}

// TestExtractToolTrace_TraceIsEmptyArray verifies that an empty tool_trace
// array ([]) returns nil (length 0).
func TestExtractToolTrace_TraceIsEmptyArray(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"metadata": map[string]interface{}{
				"tool_trace": []interface{}{},
			},
		},
	}
	body, _ := json.Marshal(resp)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for empty tool_trace, got: %s", string(got))
	}
}

// TestExtractToolTrace_NonJSONBody verifies that a completely non-JSON body
// returns nil without panicking.
func TestExtractToolTrace_NonJSONBody(t *testing.T) {
	body := []byte("this is not json at all")
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for non-JSON body, got: %s", string(got))
	}
}

// TestExtractToolTrace_EmptyBody verifies that an empty body returns nil.
func TestExtractToolTrace_EmptyBody(t *testing.T) {
	if got := extractToolTrace(nil); got != nil {
		t.Errorf("expected nil for nil body, got: %s", string(got))
	}
	if got := extractToolTrace([]byte{}); got != nil {
		t.Errorf("expected nil for empty body, got: %s", string(got))
	}
}

// TestExtractToolTrace_ResultMetadataIsNotObject verifies that when
// metadata exists but is not a JSON object (string), nil is returned.
func TestExtractToolTrace_MetadataIsString(t *testing.T) {
	body := []byte(`{"result":{"metadata":"oops"}}`)
	if got := extractToolTrace(body); got != nil {
		t.Errorf("expected nil for string metadata, got: %s", string(got))
	}
}

// TestNilIfEmpty_Contract exercises the contract of nilIfEmpty so future
// refactors can't silently break the call-sites in a2a_proxy_helpers.go.
func TestNilIfEmpty_Contract(t *testing.T) {
	if r := nilIfEmpty(""); r != nil {
		t.Errorf("nilIfEmpty(\"\") = %p, want nil", r)
	}
	if r := nilIfEmpty("hello"); r == nil {
		t.Fatal("nilIfEmpty(\"hello\") returned nil, want pointer to string")
	} else if *r != "hello" {
		t.Errorf("nilIfEmpty(\"hello\") = %q, want \"hello\"", *r)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// parseUsageFromA2AResponse
// ──────────────────────────────────────────────────────────────────────────────

func TestParseUsageFromA2AResponse_EmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"non-JSON", []byte("not json")},
		{"empty object", []byte("{}")},
		{"null result", []byte(`{"result": null}`)},
		{"string result", []byte(`{"result": "hello"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, out := parseUsageFromA2AResponse(tc.body)
			if in != 0 || out != 0 {
				t.Errorf("parseUsageFromA2AResponse = (%d, %d), want (0, 0)", in, out)
			}
		})
	}
}

func TestParseUsageFromA2AResponse_ResultUsageShape(t *testing.T) {
	body := []byte(`{
		"result": {
			"usage": {"input_tokens": 1500, "output_tokens": 320}
		}
	}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 1500 || out != 320 {
		t.Errorf("parseUsageFromA2AResponse = (%d, %d), want (1500, 320)", in, out)
	}
}

func TestParseUsageFromA2AResponse_TopLevelUsage(t *testing.T) {
	body := []byte(`{
		"usage": {"input_tokens": 100, "output_tokens": 50}
	}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 100 || out != 50 {
		t.Errorf("parseUsageFromA2AResponse = (%d, %d), want (100, 50)", in, out)
	}
}

func TestParseUsageFromA2AResponse_BothPresentPrefersResult(t *testing.T) {
	// When both result.usage and top-level usage exist, result.usage wins.
	body := []byte(`{
		"result": {"usage": {"input_tokens": 500, "output_tokens": 200}},
		"usage": {"input_tokens": 50, "output_tokens": 20}
	}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 500 || out != 200 {
		t.Errorf("parseUsageFromA2AResponse = (%d, %d), want (500, 200) from result.usage", in, out)
	}
}

func TestParseUsageFromA2AResponse_ZeroUsage(t *testing.T) {
	// Zero values are treated as absent (readUsageMap returns ok=false).
	body := []byte(`{"result": {"usage": {"input_tokens": 0, "output_tokens": 0}}}`)
	in, out := parseUsageFromA2AResponse(body)
	if in != 0 || out != 0 {
		t.Errorf("parseUsageFromA2AResponse = (%d, %d), want (0, 0)", in, out)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// readUsageMap
// ──────────────────────────────────────────────────────────────────────────────

func TestReadUsageMap_HappyPath(t *testing.T) {
	m := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"input_tokens": 100, "output_tokens": 50}`),
	}
	in, out, ok := readUsageMap(m)
	if !ok {
		t.Fatal("readUsageMap returned ok=false, want true")
	}
	if in != 100 || out != 50 {
		t.Errorf("readUsageMap = (%d, %d, %v), want (100, 50, true)", in, out, ok)
	}
}

func TestReadUsageMap_MissingUsage(t *testing.T) {
	m := map[string]json.RawMessage{
		"other": json.RawMessage(`{}`),
	}
	in, out, ok := readUsageMap(m)
	if ok {
		t.Errorf("readUsageMap returned ok=true for missing usage, want false")
	}
}

func TestReadUsageMap_ZeroValues(t *testing.T) {
	m := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"input_tokens": 0, "output_tokens": 0}`),
	}
	in, out, ok := readUsageMap(m)
	if ok {
		t.Errorf("readUsageMap returned ok=true for zero usage, want false")
	}
	if in != 0 || out != 0 {
		t.Errorf("readUsageMap = (%d, %d, %v), want (0, 0, false)", in, out, ok)
	}
}

func TestReadUsageMap_OnlyInputTokens(t *testing.T) {
	m := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"input_tokens": 200, "output_tokens": 0}`),
	}
	in, out, ok := readUsageMap(m)
	if !ok {
		t.Fatal("readUsageMap returned ok=false, want true")
	}
	if in != 200 || out != 0 {
		t.Errorf("readUsageMap = (%d, %d, %v), want (200, 0, true)", in, out, ok)
	}
}

func TestReadUsageMap_OnlyOutputTokens(t *testing.T) {
	m := map[string]json.RawMessage{
		"usage": json.RawMessage(`{"input_tokens": 0, "output_tokens": 150}`),
	}
	in, out, ok := readUsageMap(m)
	if !ok {
		t.Fatal("readUsageMap returned ok=false, want true")
	}
	if in != 0 || out != 150 {
		t.Errorf("readUsageMap = (%d, %d, %v), want (0, 150, true)", in, out, ok)
	}
}

func TestReadUsageMap_MalformedUsageJSON(t *testing.T) {
	m := map[string]json.RawMessage{
		"usage": json.RawMessage(`not valid json`),
	}
	in, out, ok := readUsageMap(m)
	if ok {
		t.Errorf("readUsageMap returned ok=true for malformed usage JSON, want false")
	}
}

// Suppress unused import warning — setupTestDB references db.DB but this file
// only tests pure functions, so db is only needed transitively through helpers.
var _ = db.DB
