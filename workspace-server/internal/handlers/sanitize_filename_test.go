package handlers_test

import (
	"strings"
	"testing"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/handlers"
)

// SanitizeFilename mirrors workspace/internal_chat_uploads.py's
// sanitize_filename. Drift between the two means canvas-emitted URIs
// differ between push and poll paths for the same upload — pin every
// case the Python suite pins (workspace/tests/test_internal_chat_uploads.py
// :: test_sanitize_filename).

func TestSanitizeFilename_StripsPathTraversal(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "passwd",
		"/etc/passwd":      "passwd",
		"a/b/c.txt":        "c.txt",
		"./relative":       "relative",
	}
	for in, want := range cases {
		if got := handlers.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilename_ReplacesUnsafeChars(t *testing.T) {
	cases := map[string]string{
		"hello world.pdf":     "hello_world.pdf",
		"weird;chars!?.txt":   "weird_chars__.txt",
		"中文.docx":             "__.docx", // non-ASCII → underscore (each rune)
		"file (1).pdf":        "file__1_.pdf",
	}
	for in, want := range cases {
		if got := handlers.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeFilename_PreservesAllowedChars(t *testing.T) {
	in := "report-2026.05.04_v2.pdf"
	if got := handlers.SanitizeFilename(in); got != in {
		t.Errorf("SanitizeFilename(%q) = %q, want unchanged", in, got)
	}
}

func TestSanitizeFilename_CapsAt100Chars_PreservesShortExtension(t *testing.T) {
	// 95-char base + ".pdf" (4 chars + dot) = 100 chars total — fits.
	base := strings.Repeat("a", 95)
	in := base + ".pdf"
	got := handlers.SanitizeFilename(in)
	if got != in {
		t.Errorf("expected unchanged at 100 chars, got %q (len=%d)", got, len(got))
	}

	// 200-char base + ".pdf" → truncated to 100 with .pdf preserved.
	long := strings.Repeat("b", 200) + ".pdf"
	got = handlers.SanitizeFilename(long)
	if len(got) != 100 {
		t.Errorf("expected length 100, got %d (%q)", len(got), got)
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Errorf("expected .pdf suffix preserved, got %q", got)
	}
}

func TestSanitizeFilename_DropsLongExtension(t *testing.T) {
	// Extension > 16 chars is treated as part of the name; truncation
	// drops it without preservation. Mirrors the Python rule
	// (dot >= 0 AND len(base) - dot <= 16).
	long := strings.Repeat("c", 90) + ".thisisaverylongextensionnotpreserved"
	got := handlers.SanitizeFilename(long)
	if len(got) != 100 {
		t.Errorf("expected 100, got %d (%q)", len(got), got)
	}
	// First 100 chars of the SANITIZED input — extension not preserved.
	if strings.Contains(got, ".thisisaverylongextensionnotpreserved") {
		t.Errorf("long extension should have been truncated, got %q", got)
	}
}

func TestSanitizeFilename_FallbackForReservedNames(t *testing.T) {
	cases := []string{"", ".", ".."}
	for _, in := range cases {
		if got := handlers.SanitizeFilename(in); got != "file" {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, "file")
		}
	}
}

func TestSanitizeFilename_AllUnsafeBecomesAllUnderscores_NotReserved(t *testing.T) {
	// All-non-ASCII input becomes all-underscores — not "." or ".." or
	// empty, so the fallback path doesn't trigger and we get a real
	// (if uninformative) sanitized name.
	got := handlers.SanitizeFilename("中文中文")
	if got != "____" {
		t.Errorf("SanitizeFilename(中文中文) = %q, want %q", got, "____")
	}
}
