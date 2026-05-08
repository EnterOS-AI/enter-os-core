package handlers

import "testing"

// TestValidateTrackedRef: pin the exact set of accepted track values
// the install endpoint stores. Drift detector reads this column; any
// value that slips through here without structural validation would
// silently fail at drift-check time.
func TestValidateTrackedRef(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		// Defaults
		{"", "none", false},
		{"   ", "none", false},
		{"none", "none", false},

		// Tag shape
		{"tag:v1.0.0", "tag:v1.0.0", false},
		{"tag:v0.4.0-gitea.1", "tag:v0.4.0-gitea.1", false},
		{"tag:latest", "tag:latest", false},

		// SHA shape
		{"sha:abc123", "sha:abc123", false},
		{"sha:0123456789abcdef0123456789abcdef01234567", "sha:0123456789abcdef0123456789abcdef01234567", false},

		// Reject malformed
		{"tag:", "", true},      // empty after prefix
		{"sha:", "", true},      // empty after prefix
		{"latest", "", true},    // bare 'latest' is ambiguous (tag? branch?)
		{"main", "", true},      // bare branch name not allowed
		{"v1.0.0", "", true},    // missing tag: prefix
		{"random", "", true},    // not in allowlist
		{"tag", "", true},       // prefix without separator
	}
	for _, tc := range cases {
		got, err := validateTrackedRef(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("validateTrackedRef(%q) = (%q, nil); want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("validateTrackedRef(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("validateTrackedRef(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
