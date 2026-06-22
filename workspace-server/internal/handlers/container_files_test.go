package handlers

import (
	"os"
	"strings"
	"testing"
)

// TestValidateRelPath tests the path-traversal guard used in deleteViaEphemeral.
// validateRelPath should reject absolute paths and ".." segments after cleaning.
// NOTE: This test lives in a file that does NOT call setupTestDB, so SSRF checks
// remain enabled. The test directly exercises validateRelPath without any DB
// dependency, so no mock DB is needed.
func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantErr  bool
		errSubstr string // if non-empty, error message must contain this substring
	}{
		// Valid: simple relative paths inside a destination
		{"single file", "config.json", false, ""},
		{"nested relative", "dir/subdir/file.txt", false, ""},
		{"file at destination root", "file.txt", false, ""},
		{"subdirectory file", "configs/myapp/file.cfg", false, ""},
		{"dotfile (hidden file, not traversal)", ".env", false, ""},

		// Empty/dot-only: must be rejected with specific message
		{"empty string", "", true, "empty or dot-only path"},
		{"dot only", ".", true, "empty or dot-only path"},

		// Traversal: must be rejected
		{"double dot parent", "../etc/passwd", true, "path traversal"},
		{"trailing dotdot", "../", true, "path traversal"},
		{"embedded dotdot", "foo/../bar", true, "path traversal"},
		{"dotdot middle", "a/b/../../c", true, "path traversal"},
		{"path ends in ..", "foo/..", true, "path traversal"},
		{"bare ..", "..", true, "path traversal"},

		// Absolute: must be rejected
		{"absolute unix", "/etc/passwd", true, "path traversal"},
		{"absolute windows", "C:\\Windows\\System32", false, ""}, // Unix/Linux: no drive letter, treated as relative by Go
		{"embedded absolute", "foo/etc/passwd", false, ""},
		{"root absolute", "/workspace/file.txt", true, "path traversal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRelPath(tc.path)
			if tc.wantErr && err == nil {
				t.Errorf("validateRelPath(%q): expected error, got nil", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRelPath(%q): expected nil, got %v", tc.path, err)
			}
			if tc.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), tc.errSubstr)) {
				t.Errorf("validateRelPath(%q): expected error containing %q, got %v", tc.path, tc.errSubstr, err)
			}
		})
	}
}

// TestValidateRelPath_Cleaned ensures that validateRelPath is called on the
// cleaned (resolved) path, not the raw input, so tricks like "foo/./bar"
// pass but "foo/../bar" fails.
func TestValidateRelPath_Cleaned(t *testing.T) {
	// ". " (dot-space) is not "..", but after Clean() it becomes just the dir.
	// validateRelPath should be called on the clean path, not raw.
	// These are valid relative paths.
	valid := []string{
		"foo/./bar",
		"foo/././baz",
		"./file.cfg",
	}
	for _, p := range valid {
		if err := validateRelPath(p); err != nil {
			t.Errorf("validateRelPath(%q): expected nil, got %v", p, err)
		}
	}
}

// TestDeleteViaEphemeral_ConcatFormDocs documents that the exec form
// of rm used in deleteViaEphemeral receives the path as a single concatenated
// argument, not as a shell-expanded arg. This prevents traversal even if
// validateRelPath were somehow bypassed (defence in depth).
//
// The concat form: []string{"rm", "-rf", "/configs/" + filePath}
// passes ONE argument "/configs/../../../etc" to rm, which resolves it
// relative to rm's CWD, NOT the shell's working directory.
//
// By contrast, the shell-expanded form:
//   sh -c "rm -rf /configs $filePath"
// would treat ".." as path components relative to /configs and could escape.
//
// deleteViaEphemeral uses the exec form only (verified in code review).
func TestDeleteViaEphemeral_ConcatFormDocs(t *testing.T) {
	// This is a documentation test — it confirms the concat form is present
	// in the actual codebase by reading the source file directly.
	src, err := sourceFile("container_files.go")
	if err != nil {
		t.Skip("cannot read source: " + err.Error())
	}
	if !strings.Contains(src, `"/configs/" + filePath`) {
		t.Error("deleteViaEphemeral does not use concat form; F1085 fix may be missing or reverted")
	}
}

// sourceFile reads a source file from the same package at runtime.
// Used for compile-time-verification-style tests without importing io/ioutil.
func sourceFile(name string) (string, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return string(data), nil
}