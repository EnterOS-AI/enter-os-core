package textutil

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateBytes_RuneBoundary pins the byte-cap, marker-bearing
// truncation path. Every case asserts both:
//  1. the exact expected output (so a refactor that flips ellipsis or
//     drops a rune is caught), and
//  2. utf8.ValidString on the output (the invariant that the bug class
//     in #2026/#2959/#2962 violated by slicing mid-codepoint).
//
// Per memory feedback_assert_exact_not_substring.md, asserts are exact
// equality, not substring matches.
func TestTruncateBytes_RuneBoundary(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		// Under-cap: returns input verbatim.
		{"empty", "", 10, ""},
		{"under-cap ASCII", "hi", 10, "hi"},
		{"exactly-at-cap ASCII", "hello", 5, "hello"},
		{"under-cap CJK", "你好", 10, "你好"}, // 6 bytes
		{"exactly-at-cap CJK", "你好", 6, "你好"},

		// Over-cap ASCII: trims to (maxBytes - 3) bytes + "…".
		{"over-cap ASCII", "abcdefghij", 6, "abc…"},

		// Over-cap CJK where cut would land mid-codepoint. The
		// pre-fix bug shape: 7 - 3 = 4, but byte 4 is mid-"好"
		// (好 is bytes 3..5 of "你好世界"). Walking back to byte 3
		// (start of 好 — wait, that IS the start). Actually 你=0..2,
		// 好=3..5, 世=6..8, 界=9..11. Cut=4, walk back to 3 (start
		// of 好), then s[:3]="你", + "…" = "你…" (3+3=6 bytes ≤ 7).
		{"over-cap CJK lands mid-codepoint", "你好世界", 7, "你…"},

		// Over-cap CJK where cut lands exactly on rune boundary.
		// 9 - 3 = 6, byte 6 is start of 世. Walk-back is no-op.
		// s[:6]="你好" + "…" = "你好…" (9 bytes).
		{"over-cap CJK rune-aligned", "你好世界", 9, "你好…"},

		// Emoji: 😀 is 4 bytes (U+1F600). 7 - 3 = 4, byte 4 is start
		// of second 😀 — walk-back no-op. s[:4]="😀" + "…" = "😀…".
		{"over-cap emoji", "😀😀😀", 7, "😀…"},

		// Mixed ASCII + CJK. "ab你好世界": a(1) b(1) 你(3) 好(3) 世(3) 界(3) = 14 bytes.
		// maxBytes=8, 8-3=5. byte 5 is mid-好. Walk back to start of 好 = byte 5? Let me
		// recompute: a=0, b=1, 你=2..4, 好=5..7, 世=8..10. Byte 5 IS start of 好.
		// Walk-back keeps cut at 5. s[:5] = "ab你" + "…" = "ab你…" (8 bytes).
		{"mixed prefix ASCII over-cap CJK", "ab你好世界", 8, "ab你…"},

		// Pathological: maxBytes too small to even fit the marker.
		{"cap below ellipsis len", "hello", 2, ""},
		{"cap zero", "hello", 0, ""},
		{"cap negative", "hello", -1, ""},

		// Cap exactly == ellipsis len: no room for content, but
		// the marker fits. This returns "" (cut = 0, s[:0] = "").
		{"cap equals ellipsis len", "hello", 3, "…"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateBytes(c.in, c.maxBytes)
			if got != c.want {
				t.Errorf("TruncateBytes(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateBytes(%q, %d) returned invalid UTF-8: %q", c.in, c.maxBytes, got)
			}
			// Output never exceeds the byte cap (when one is set).
			if c.maxBytes > 0 && len(got) > c.maxBytes {
				t.Errorf("TruncateBytes(%q, %d) overflowed cap: len(out)=%d > %d",
					c.in, c.maxBytes, len(got), c.maxBytes)
			}
		})
	}
}

// TestTruncateBytesNoMarker pins the marker-less variant. Same
// boundary handling as TruncateBytes but no ellipsis cost — the cut
// happens at maxBytes itself, walking back only if that lands
// mid-codepoint.
func TestTruncateBytesNoMarker(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"empty", "", 10, ""},
		{"under-cap ASCII", "hi", 10, "hi"},
		{"exactly-at-cap ASCII", "hello", 5, "hello"},
		{"over-cap ASCII", "abcdefghij", 5, "abcde"},

		// Over-cap CJK rune-aligned: "你好世界", maxBytes=6, byte 6 is start of 世.
		// s[:6]="你好" — perfect cut.
		{"over-cap CJK rune-aligned", "你好世界", 6, "你好"},

		// Over-cap CJK mid-codepoint: maxBytes=4, byte 4 is mid-好.
		// Walk back to byte 3 (start of 好), s[:3]="你".
		{"over-cap CJK mid-codepoint", "你好世界", 4, "你"},

		// Emoji: maxBytes=5, "😀😀" is bytes 0..3 then 4..7. byte 5 is mid-second-😀.
		// Walk back to byte 4 (start of second 😀), s[:4]="😀".
		{"over-cap emoji", "😀😀", 5, "😀"},

		// Edge: cap zero or negative → "".
		{"cap zero", "hello", 0, ""},
		{"cap negative", "hello", -1, ""},

		// Cap = 1 and first rune is multi-byte: walk-back to 0, return "".
		{"cap one with leading CJK", "你hello", 1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateBytesNoMarker(c.in, c.maxBytes)
			if got != c.want {
				t.Errorf("TruncateBytesNoMarker(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateBytesNoMarker(%q, %d) returned invalid UTF-8: %q", c.in, c.maxBytes, got)
			}
			if c.maxBytes > 0 && len(got) > c.maxBytes {
				t.Errorf("TruncateBytesNoMarker(%q, %d) overflowed cap: len(out)=%d > %d",
					c.in, c.maxBytes, len(got), c.maxBytes)
			}
		})
	}
}

// TestTruncateRunes pins the rune-cap variant. The key contract is
// that maxRunes counts user-visible characters (Go runes, which line
// up with Unicode codepoints), not bytes — so "你好世界" with
// maxRunes=2 returns "你好…", regardless of the resulting byte count.
func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{"empty", "", 5, ""},
		{"under-cap ASCII", "hi", 5, "hi"},
		{"exactly-at-cap ASCII", "hello", 5, "hello"},
		{"over-cap ASCII", "abcdefghij", 5, "abcde…"},

		{"under-cap CJK", "你好", 5, "你好"},
		{"exactly-at-cap CJK", "你好", 2, "你好"},

		// Over-cap CJK: maxRunes=3, expect first 3 runes + marker.
		{"over-cap CJK", "你好世界你好", 3, "你好世…"},

		// Emoji is one rune per glyph in Go (no ZWJ here).
		{"over-cap emoji", "😀😀😀😀😀", 2, "😀😀…"},

		// Mixed: maxRunes=3 of "ab你好世界" → "ab你…".
		{"mixed prefix", "ab你好世界", 3, "ab你…"},

		// Edge: maxRunes 0 / negative → "".
		{"cap zero", "hello", 0, ""},
		{"cap negative", "hello", -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TruncateRunes(c.in, c.maxRunes)
			if got != c.want {
				t.Errorf("TruncateRunes(%q, %d) = %q, want %q", c.in, c.maxRunes, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("TruncateRunes(%q, %d) returned invalid UTF-8: %q", c.in, c.maxRunes, got)
			}
		})
	}
}

// TestTruncate_FuzzInvariants stays as a property-style sanity check:
// for any rune-valid input and any cap, the output is rune-valid and
// (for byte-cap variants) within the cap. This catches off-by-one
// regressions in cuts that slip past the table-test cases above.
func TestTruncate_FuzzInvariants(t *testing.T) {
	inputs := []string{
		"",
		"a",
		"hello world",
		"你好世界",
		"😀😀😀",
		"ab你c好d世e界",
		"日本語の文字列",
		"🇺🇸🇯🇵", // flags: each is 2 codepoints (regional indicators)
	}
	for _, in := range inputs {
		for cap := -1; cap <= len(in)+5; cap++ {
			t.Run("", func(t *testing.T) {
				gotB := TruncateBytes(in, cap)
				if !utf8.ValidString(gotB) {
					t.Errorf("TruncateBytes(%q, %d) invalid UTF-8: %q", in, cap, gotB)
				}
				if cap > 0 && len(gotB) > cap {
					t.Errorf("TruncateBytes(%q, %d) overflowed: %q (%d bytes)", in, cap, gotB, len(gotB))
				}

				gotN := TruncateBytesNoMarker(in, cap)
				if !utf8.ValidString(gotN) {
					t.Errorf("TruncateBytesNoMarker(%q, %d) invalid UTF-8: %q", in, cap, gotN)
				}
				if cap > 0 && len(gotN) > cap {
					t.Errorf("TruncateBytesNoMarker(%q, %d) overflowed: %q (%d bytes)", in, cap, gotN, len(gotN))
				}

				gotR := TruncateRunes(in, cap)
				if !utf8.ValidString(gotR) {
					t.Errorf("TruncateRunes(%q, %d) invalid UTF-8: %q", in, cap, gotR)
				}
			})
		}
	}
}
