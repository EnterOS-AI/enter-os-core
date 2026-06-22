package handlers

// a2a_corpus_test.go — backward-compat replay gate for the A2A
// JSON-RPC protocol surface. Every PR that touches
// normalizeA2APayload OR bumps the a-2-a-sdk version pin runs
// every shape in testdata/a2a_corpus/ through the current code
// and asserts:
//
//   valid/   — every shape MUST parse without error and produce a
//              canonical v0.3 payload (params.message.parts list).
//
//   invalid/ — every shape MUST be rejected with the documented
//              status code and error substring. Pins the
//              rejection contract so a future PR doesn't silently
//              start accepting malformed payloads.
//
// Closes the gap that allowed the 2026-04-29 v0.2 → v0.3 silent-
// drop bug (PR #2349). That bug shipped because the SDK bump PR
// didn't replay v0.2-shaped inputs against the new code; the
// shape-mismatch surfaced only in production when the receiver's
// Pydantic validator silently rejected inbound messages.
//
// Adding to the corpus: see testdata/a2a_corpus/README.md.
// Removing from valid/: breaking change, requires explicit
// approval per the README.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	corpusValidDir   = "testdata/a2a_corpus/valid"
	corpusInvalidDir = "testdata/a2a_corpus/invalid"
)

// metadataFields are the documentation-only keys the corpus loader
// strips before passing the payload to normalizeA2APayload. They
// are required for every corpus entry per the README policy.
var metadataFields = []string{
	"_comment",
	"_added",
	"_source",
	"_expect_error",
	"_expect_status",
}

// loadCorpusEntry reads one JSON file, parses it as a generic map,
// extracts the metadata fields (including expected error/status for
// invalid entries), strips them from the payload, and returns the
// stripped JSON bytes ready for normalizeA2APayload.
func loadCorpusEntry(t *testing.T, path string) (payload []byte, expectErr string, expectStatus int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s as JSON: %v", path, err)
	}
	// Pull metadata before strip.
	if v, ok := doc["_expect_error"].(string); ok {
		expectErr = v
	}
	if v, ok := doc["_expect_status"].(float64); ok {
		expectStatus = int(v)
	}
	for _, f := range metadataFields {
		delete(doc, f)
	}
	payload, err = json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal %s after strip: %v", path, err)
	}
	return payload, expectErr, expectStatus
}

// listCorpus enumerates every .json file under dir and returns
// (filename → full path). Sorted for stable test ordering.
func listCorpus(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out[e.Name()] = filepath.Join(dir, e.Name())
	}
	if len(out) == 0 {
		t.Fatalf("corpus dir %s is empty — at least one entry is required", dir)
	}
	return out
}

// TestA2ACorpus_ValidShapesParse replays every entry in valid/
// through normalizeA2APayload and asserts:
//  1. No error returned.
//  2. The output's params.message.parts is a non-empty list
//     (v0.3 canonical shape — the compat shim must have converted
//     any v0.2 content field into parts).
//  3. The output's params.message.messageId is non-empty (the
//     normalizer auto-fills if the sender omitted it).
//  4. The output's method matches the input's method (the
//     normalizer is method-agnostic).
//
// One subtest per corpus entry — failures point directly at the
// offending shape file.
func TestA2ACorpus_ValidShapesParse(t *testing.T) {
	t.Parallel()
	for name, path := range listCorpus(t, corpusValidDir) {
		t.Run(name, func(t *testing.T) {
			payload, _, _ := loadCorpusEntry(t, path)

			normalized, method, perr := normalizeA2APayload(payload)
			if perr != nil {
				t.Fatalf("valid/%s: normalizeA2APayload returned error %d: %v",
					name, perr.Status, perr.Response)
			}

			// Read back the normalized payload to verify shape invariants.
			var parsed map[string]interface{}
			if err := json.Unmarshal(normalized, &parsed); err != nil {
				t.Fatalf("valid/%s: normalized output not valid JSON: %v", name, err)
			}

			// Method-agnostic check — input method survives normalization.
			if input := mustGetString(t, parsed, "method"); input != method {
				t.Errorf("valid/%s: method mismatch — got %q, want %q",
					name, method, input)
			}

			// Canonical v0.3 shape invariants: params.message.parts is a
			// non-empty list, messageId is non-empty.
			params := mustGetMap(t, parsed, "params")
			msg := mustGetMap(t, params, "message")

			parts, ok := msg["parts"].([]interface{})
			if !ok {
				t.Errorf("valid/%s: params.message.parts is not a list (got %T)",
					name, msg["parts"])
				return
			}
			if len(parts) == 0 {
				t.Errorf("valid/%s: params.message.parts is empty — compat shim should have converted content", name)
			}

			if id := mustGetString(t, msg, "messageId"); id == "" {
				t.Errorf("valid/%s: params.message.messageId is empty after normalization", name)
			}

			// content must NOT survive into the output — the shim
			// deletes it after converting to parts. If the shim left
			// content in place, downstream pydantic v0.3 would still
			// reject because it doesn't know that field.
			if _, hasContent := msg["content"]; hasContent {
				t.Errorf("valid/%s: params.message.content survived normalization (compat shim should delete it)", name)
			}
		})
	}
}

// TestA2ACorpus_InvalidShapesRejected replays every entry in
// invalid/ through normalizeA2APayload and asserts the rejection
// matches the documented contract — same status code AND error
// substring as recorded in the corpus entry's metadata.
//
// Catches the regression class "future PR adds permissive defaults
// that silently accept what we used to reject loud."
func TestA2ACorpus_InvalidShapesRejected(t *testing.T) {
	t.Parallel()
	for name, path := range listCorpus(t, corpusInvalidDir) {
		t.Run(name, func(t *testing.T) {
			payload, expectErr, expectStatus := loadCorpusEntry(t, path)

			if expectErr == "" {
				t.Fatalf("invalid/%s: missing _expect_error metadata", name)
			}
			if expectStatus == 0 {
				t.Fatalf("invalid/%s: missing _expect_status metadata", name)
			}

			_, _, perr := normalizeA2APayload(payload)
			if perr == nil {
				t.Fatalf("invalid/%s: normalizeA2APayload returned no error — should have rejected", name)
			}
			if perr.Status != expectStatus {
				t.Errorf("invalid/%s: status = %d, want %d", name, perr.Status, expectStatus)
			}

			body, _ := json.Marshal(perr.Response)
			if !strings.Contains(string(body), expectErr) {
				t.Errorf("invalid/%s: error response %q does not contain expected substring %q",
					name, string(body), expectErr)
			}
		})
	}
}

// TestA2ACorpus_MalformedJSONRejected covers the case where the
// body isn't valid JSON at all. The corpus is JSON-only so this
// can't be expressed as a corpus entry; pin the contract inline.
func TestA2ACorpus_MalformedJSONRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload []byte
	}{
		{"truncated_object", []byte(`{"jsonrpc":"2.0","method":"message/send"`)},
		{"not_json_at_all", []byte(`this is not json`)},
		{"empty_body", []byte(``)},
		{"only_whitespace", []byte(`   `)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, perr := normalizeA2APayload(tc.payload)
			if perr == nil {
				t.Fatalf("expected error for %s, got none", tc.name)
			}
			if perr.Status != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", perr.Status, http.StatusBadRequest)
			}
			body, _ := json.Marshal(perr.Response)
			if !strings.Contains(string(body), "invalid JSON") {
				t.Errorf("expected 'invalid JSON' in response, got %q", string(body))
			}
		})
	}
}

// TestA2ACorpus_HasMinimumCoverage pins the corpus's
// representativeness. The corpus must have at least one v0.2
// entry (string content) and at least one v0.3 entry (parts list)
// — losing either side of the schema bridge would silently drop
// the most important coverage.
func TestA2ACorpus_HasMinimumCoverage(t *testing.T) {
	t.Parallel()
	files := listCorpus(t, corpusValidDir)
	hasV02 := false
	hasV03 := false
	for name := range files {
		if strings.Contains(name, "v0_2_") {
			hasV02 = true
		}
		if strings.Contains(name, "v0_3_") {
			hasV03 = true
		}
	}
	if !hasV02 {
		t.Error("corpus has no v0_2_*.json entries — backward-compat coverage missing")
	}
	if !hasV03 {
		t.Error("corpus has no v0_3_*.json entries — forward (canonical) coverage missing")
	}
}

// TestA2ACorpus_EveryEntryHasMetadata pins the README policy:
// every corpus entry MUST have _comment, _added, _source. Catches
// the bad commit shape "added entry without explanation" before
// review.
func TestA2ACorpus_EveryEntryHasMetadata(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{corpusValidDir, corpusInvalidDir} {
		for name, path := range listCorpus(t, dir) {
			t.Run(filepath.Base(dir)+"/"+name, func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				var doc map[string]interface{}
				if err := json.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("parse %s: %v", path, err)
				}
				required := []string{"_comment", "_added", "_source"}
				if dir == corpusInvalidDir {
					required = append(required, "_expect_error", "_expect_status")
				}
				for _, key := range required {
					if _, ok := doc[key]; !ok {
						t.Errorf("missing required metadata field %q", key)
					}
				}
			})
		}
	}
}

func mustGetMap(t *testing.T, m map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	v, ok := m[key].(map[string]interface{})
	if !ok {
		t.Fatalf("expected %q to be a map, got %T", key, m[key])
	}
	return v
}

func mustGetString(t *testing.T, m map[string]interface{}, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("expected %q to be a string, got %T", key, m[key])
	}
	return v
}

// _ silences the unused-import linter for fmt in case future
// helpers don't use it. Currently used by the t.Helper-style
// formatters above (kept inline for clarity).
var _ = fmt.Sprintf
