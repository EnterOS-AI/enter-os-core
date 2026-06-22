package uploads

import (
	"encoding/json"
	"testing"
)

// TestDefaultUploadLimits_PinsCurrentValues guards against a silent
// cap change. Any future bump MUST update this test as part of the
// same PR — that forces a reviewer to see the cap move and audit the
// matching DB migration + nginx config + python/canvas consumer updates.
//
// If you're updating this test because you bumped the cap: also update
// (1) the matching migration's size_bytes CHECK upper bound, (2)
// tests/harness/cf-proxy/nginx.conf client_max_body_size, (3) the doc
// comments in handlers/chat_files.go + pendinguploads/storage.go +
// canvas/.../uploads.ts that quote the cap in English ("100 MB").
func TestDefaultUploadLimits_PinsCurrentValues(t *testing.T) {
	got := DefaultUploadLimits()
	const oneHundredMB = int64(100 * 1024 * 1024)

	if got.PerFileBytes != oneHundredMB {
		t.Errorf("PerFileBytes: want %d (100 MB), got %d", oneHundredMB, got.PerFileBytes)
	}
	if got.PerRequestBytes != oneHundredMB {
		t.Errorf("PerRequestBytes: want %d (100 MB), got %d", oneHundredMB, got.PerRequestBytes)
	}
	if got.MaxAttachmentsPerMessage != 10 {
		t.Errorf("MaxAttachmentsPerMessage: want 10, got %d", got.MaxAttachmentsPerMessage)
	}
}

// TestUploadLimits_JSONShape pins the wire contract. Renaming any of
// these JSON keys is a breaking change for the canvas + Python
// consumers that fetch GET /uploads/limits. Adding new keys is fine;
// renaming or removing requires a new endpoint (v2) and a coordinated
// consumer rollout.
//
// We assert via Marshal+Unmarshal-through-map rather than a literal
// JSON string match because Go map ordering in error messages is
// stable but a literal would catch every whitespace tweak; this
// formulation surfaces the actual field-name regression.
func TestUploadLimits_JSONShape(t *testing.T) {
	in := UploadLimits{
		PerFileBytes:             1,
		PerRequestBytes:          2,
		MaxAttachmentsPerMessage: 3,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Field names — exact strings the canvas TS + python clients will
	// key off. Any rename here is a coordinated multi-repo rollout.
	for _, key := range []string{"per_file_bytes", "per_request_bytes", "max_attachments_per_message"} {
		if _, ok := out[key]; !ok {
			t.Errorf("missing JSON key %q in %s", key, string(raw))
		}
	}

	// Round-trip preserves values — guards against silently changing
	// the field encoding (e.g. int → string).
	var back UploadLimits
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if back != in {
		t.Errorf("round-trip mismatch: in=%+v back=%+v", in, back)
	}
}
