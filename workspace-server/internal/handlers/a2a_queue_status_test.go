package handlers

import (
	"testing"
)

// TestExtractExpiresInSeconds covers the JSON parser used at enqueue time
// to honor a caller-specified TTL. Zero return = "no TTL" — caller leaves
// expires_at NULL on the queue row.
func TestExtractExpiresInSeconds(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "absent",
			body: `{"params":{"message":{"messageId":"x"}}}`,
			want: 0,
		},
		{
			name: "positive",
			body: `{"params":{"expires_in_seconds":300,"message":{"messageId":"x"}}}`,
			want: 300,
		},
		{
			name: "zero",
			body: `{"params":{"expires_in_seconds":0,"message":{"messageId":"x"}}}`,
			want: 0,
		},
		{
			name: "negative coerced to zero",
			body: `{"params":{"expires_in_seconds":-30,"message":{"messageId":"x"}}}`,
			want: 0,
		},
		{
			name: "invalid JSON returns zero",
			body: `not json`,
			want: 0,
		},
		{
			name: "wrong type silently zero (json.Unmarshal returns err on type mismatch)",
			body: `{"params":{"expires_in_seconds":"not-a-number"}}`,
			want: 0,
		},
		{
			name: "params absent entirely",
			body: `{}`,
			want: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractExpiresInSeconds([]byte(tc.body))
			if got != tc.want {
				t.Errorf("extractExpiresInSeconds(%q) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}
