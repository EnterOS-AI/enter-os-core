package handlers

// container_files_delete_test.go — CWE-22/CWE-78 regression suite for
// deleteViaEphemeral (F1085).
//
// Vulnerability (F1085): deleteViaEphemeral used the 2-arg exec form
//   []string{"rm", "-rf", "/configs", filePath}
// which passes "/configs" as an rm target, causing rm to delete the
// entire volume mount regardless of what filePath resolves to after mount.
// Fix: use filepath.Join + filepath.Clean + HasPrefix to scope rm to
// /configs/<filePath> — filePath is validated by validateRelPath (CWE-22).
//
// This test suite validates that deleteViaEphemeral rejects all forms of
// path traversal before any Docker call is made (docker: nil).

import (
	"context"
	"testing"
)

func TestDeleteViaEphemeral_F1085_RejectsTraversal(t *testing.T) {
	// TemplatesHandler with nil docker — validation runs before any Docker call.
	h := &TemplatesHandler{docker: nil}
	ctx := context.Background()

	tests := []struct {
		label      string
		volumeName string
		filePath   string
		wantErr    bool
		errSubstr  string // substring that must appear in error message
	}{
		// ── Legitimate relative paths ─────────────────────────────────────────
		{
			label:      "simple_file_ok",
			volumeName: "ws-configs:/configs",
			filePath:   "config.yaml",
			wantErr:    false,
		},
		{
			label:      "nested_file_ok",
			volumeName: "ws-configs:/configs",
			filePath:   "subdir/script.sh",
			wantErr:    false,
		},
		{
			label:      "dot_in_path_ok",
			volumeName: "ws-configs:/configs",
			filePath:   "app.venv/config",
			wantErr:    false,
		},
		// ── CWE-22: absolute paths ──────────────────────────────────────────────
		{
			label:      "absolute_path_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "/etc/passwd",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		// ── CWE-22: leading ".." traversal ───────────────────────────────────────
		{
			label:      "leading_dotdot_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "../etc/passwd",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		{
			label:      "double_leading_dotdot_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "../../root/.ssh/authorized_keys",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		// ── CWE-22: mid-path traversal (F1085 regression case) ──────────────────
		// "foo/../../../etc" does NOT start with ".." — OLD code (the buggy
		// 2-arg form) passes this because rm sees "/configs" as the target and
		// "foo/../../../etc" as a path INSIDE /configs, deleting the whole mount.
		// With the fixed scoped form + validateRelPath, the traversal is caught.
		{
			label:      "mid_path_traversal_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "foo/../../../etc/cron.d",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		{
			label:      "deep_mid_path_traversal_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "x/y/../../../../../../../etc/shadow",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		// ── CWE-22: percent-encoded traversal ──────────────────────────────────
		{
			label:      "url_encoded_dotdot_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "..%2F..%2F..%2Fsecrets",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		// ── CWE-22: null-byte injection ─────────────────────────────────────────
		{
			label:      "null_byte_injection_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "../../../etc/passwd\x00.txt",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		// ── F1085-specific: the volume itself cannot be targeted ──────────────
		{
			label:      "dotdot_targets_parent_of_volume_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "..",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
		{
			label:      "dotdotdot_targets_root_of_volume_rejected",
			volumeName: "ws-configs:/configs",
			filePath:   "../..",
			wantErr:    true,
			errSubstr:  "not allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.label, func(t *testing.T) {
			err := h.deleteViaEphemeral(ctx, tc.volumeName, tc.filePath)
			if tc.wantErr {
				if err == nil {
					t.Errorf("want non-nil error, got nil")
					return
				}
				if tc.errSubstr != "" && !containsSubstr(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
			} else {
				if err != nil && containsSubstr(err.Error(), "not allowed") {
					t.Errorf("safe path rejected: %v", err)
				}
			}
		})
	}
}

// containsSubstr is a simple substring check (no external imports needed).
func containsSubstr(s, substr string) bool {
	if substr == "" {
		return true
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
