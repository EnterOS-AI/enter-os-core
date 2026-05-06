// Package textutil provides string-handling helpers that respect UTF-8
// rune boundaries.
//
// Why this package exists
// -----------------------
// `s[:max]` truncates by BYTES; for any string with a multi-byte
// codepoint at byte `max` (CJK, emoji, accented Latin), the slice
// produces invalid UTF-8. Postgres `text` and `jsonb` columns reject
// invalid UTF-8 with `invalid byte sequence for encoding "UTF8"`,
// which silently fails the INSERT and holds the surrounding tx open
// — a class of audit-gap that has bitten this codebase three times
// (scheduler.go #2026, agent_message_writer.go #2959,
// delegation_ledger.go #2962). Six per-package helpers had
// independently re-implemented this logic with varying correctness;
// this package is the single source of truth.
//
// Use sites
// ---------
//   - DB writes whose column is bytes-bounded (jsonb preview field,
//     varchar(N)): TruncateBytes / TruncateBytesNoMarker.
//   - UI summaries whose cap is in display chars, not bytes:
//     TruncateRunes.
//
// All functions guarantee `utf8.ValidString(out) == true` for any
// `s` where `utf8.ValidString(s) == true`. Inputs that are already
// invalid UTF-8 should be sanitized at the trust boundary (e.g. via
// `strings.ToValidUTF8`); this package does not silently fix
// upstream invalid input.
package textutil

import "unicode/utf8"

// ellipsis is the truncation marker. U+2026 HORIZONTAL ELLIPSIS —
// 3 bytes in UTF-8, 1 rune, 1 display column. Standardized across
// the codebase to avoid the "..." (3 ASCII chars) vs "…" (1 char)
// inconsistency the per-package helpers had drifted into.
const ellipsis = "…"

// TruncateBytes returns s if `len(s) <= maxBytes`, otherwise returns
// the longest rune-aligned prefix of s that fits in `maxBytes - 3`
// bytes followed by the ellipsis marker. The returned string is
// always at most `maxBytes` bytes long.
//
// Example: TruncateBytes("你好世界你好", 10) returns "你好世…" (9 bytes)
// — three "你好" runes (each 3 bytes = 9 bytes) plus "…" (3 bytes)
// would be 12 bytes, so we walk back to "你好" (6 bytes) + "…" (3) = 9.
//
// Edge cases:
//   - maxBytes <= 0: returns "" (no room even for input or marker)
//   - maxBytes < len(ellipsis): returns "" (can't add marker without
//     exceeding cap, and we won't return a marker-less truncation
//     here — caller wanted a marker; use TruncateBytesNoMarker if
//     they don't)
//   - s contains invalid UTF-8: continuation bytes are walked over
//     same as valid runes; the result preserves the (invalid) input
//     bytes up to the truncation point. Caller is responsible for
//     pre-sanitizing if Postgres validity is required.
func TruncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes < len(ellipsis) {
		return ""
	}
	// Reserve room for the marker, then walk back to the nearest
	// rune boundary at or below the cut point.
	cut := maxBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// TruncateBytesNoMarker returns s if `len(s) <= maxBytes`, otherwise
// returns the longest rune-aligned prefix of s that fits in
// `maxBytes` bytes. No marker is appended — useful when the caller's
// storage already conveys "preview" / "snippet" semantics and an
// extra ellipsis would push the result over a hard column cap.
//
// Example: TruncateBytesNoMarker("hello world", 5) returns "hello".
//
// Edge case: maxBytes <= 0 returns "".
func TruncateBytesNoMarker(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// TruncateRunes returns s if it has at most maxRunes runes, otherwise
// returns the first maxRunes runes followed by the ellipsis marker.
// Use this when the cap is in user-visible characters (UI summary,
// activity feed line) rather than bytes (DB column).
//
// Example: TruncateRunes("你好世界你好", 3) returns "你好世…" — three
// runes plus the marker, regardless of the resulting byte count.
//
// Edge case: maxRunes <= 0 returns "" (caller asked for no content).
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	// Fast path: if every byte is a single-byte rune, the byte-length
	// upper-bounds the rune count. This avoids a runes alloc for the
	// common ASCII case where the input fits.
	if len(s) <= maxRunes {
		return s
	}
	// Walk by rune boundaries; stop at the (maxRunes+1)-th rune so we
	// know the cut point and that truncation is needed.
	count := 0
	for i := range s {
		if count == maxRunes {
			return s[:i] + ellipsis
		}
		count++
	}
	// Reachable when the byte count exceeded maxRunes but the actual
	// rune count didn't (e.g. all single-byte runes that just happen
	// to be more than maxRunes). The fast path catches len(s) <=
	// maxRunes; this catches maxRunes < runeCount(s) <= len(s).
	return s
}
