package approvals

import "testing"

func TestIsGated(t *testing.T) {
	tests := []struct {
		name   string
		action Action
		want   bool
	}{
		{
			name:   "secret write is gated",
			action: ActionSecretWrite,
			want:   true,
		},
		{
			name:   "org token mint is gated",
			action: ActionOrgTokenMint,
			want:   true,
		},
		{
			name:   "workspace delete is not gated",
			action: ActionDeleteWorkspace,
			want:   false,
		},
		{
			name:   "deprovision is not gated",
			action: ActionDeprovision,
			want:   false,
		},
		{
			name:   "unknown action is not gated",
			action: Action("unknown_action"),
			want:   false,
		},
		{
			name:   "empty action is not gated",
			action: Action(""),
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsGated(tt.action); got != tt.want {
				t.Errorf("IsGated(%q) = %v, want %v", tt.action, got, tt.want)
			}
		})
	}
}

func TestGatedActions(t *testing.T) {
	// Sanity-check that the canonical gated actions map to the expected
	// constants. This catches accidental drift between the exported constants
	// and the policy map.
	for _, action := range []Action{ActionSecretWrite, ActionOrgTokenMint} {
		if !IsGated(action) {
			t.Errorf("expected %q to be gated", action)
		}
	}
}
