// Package envx holds tiny helpers for reading tunable values from
// environment variables with a safe default. Named `envx` rather than
// `env` to avoid collision with Go's net/http and common third-party
// packages that use `env` as a var/import name.
//
// Rules of thumb for the helpers:
//   - Unset variable  → default
//   - Unparseable     → default (never crash startup)
//   - Parsed but ≤ 0  → default (a "disabled" override is almost always
//     a misconfiguration; use a feature flag instead)
package envx

import (
	"os"
	"strconv"
	"time"
)

// Duration reads `name` as a time.Duration string (e.g. "30s", "5m").
// Returns `def` when unset, unparseable, or non-positive.
func Duration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// Int64 reads `name` as a base-10 int64. Returns `def` when unset,
// unparseable, or non-positive.
func Int64(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Bool reads `name` as a boolean. Returns `def` when unset or unparseable.
// Truthy values per strconv.ParseBool: 1, t, T, TRUE, true, True.
// Falsy: 0, f, F, FALSE, false, False, empty.
// All other values (including "yes", "on", "y", "n") are unparseable and
// return def. Use Bool for feature flags where the operator's mental
// model is "set truthy to enable" — a misconfigured value falls through
// to the safe default rather than silently enabling a feature.
func Bool(name string, def bool) bool {
	if v := os.Getenv(name); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
