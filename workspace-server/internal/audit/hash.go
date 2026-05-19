package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha256Hex returns the lowercase hex digest of s. Kept in its own
// file so the import of crypto/sha256 is co-located with its only
// caller (HashValuePrefix in emit.go) — easier to audit when reviewing
// changes to secret handling.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
