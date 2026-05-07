package main

import "testing"

// TestResolveBindHost pins the precedence: BIND_ADDR explicit > dev-mode
// fail-open default of 127.0.0.1 > production-shape empty (all interfaces).
//
// Mutation-test invariant: removing the IsDevModeFailOpen() branch makes
// "no_bindaddr_devmode_unset_admin" fail (returns "" instead of "127.0.0.1").
// Removing the BIND_ADDR branch makes "explicit_bindaddr_*" cases fail.
func TestResolveBindHost(t *testing.T) {
	cases := []struct {
		name       string
		bindAddr   string
		adminToken string
		molEnv     string
		want       string
	}{
		{
			name:       "no_bindaddr_devmode_unset_admin",
			bindAddr:   "",
			adminToken: "",
			molEnv:     "dev",
			want:       "127.0.0.1",
		},
		{
			name:       "no_bindaddr_devmode_unset_admin_full_word",
			bindAddr:   "",
			adminToken: "",
			molEnv:     "development",
			want:       "127.0.0.1",
		},
		{
			name:       "no_bindaddr_admin_set_in_dev_env",
			bindAddr:   "",
			adminToken: "secret",
			molEnv:     "dev",
			want:       "", // ADMIN_TOKEN flips IsDevModeFailOpen to false → all interfaces
		},
		{
			name:       "no_bindaddr_production_env",
			bindAddr:   "",
			adminToken: "",
			molEnv:     "production",
			want:       "", // production is not a dev value → all interfaces
		},
		{
			name:       "no_bindaddr_unset_env",
			bindAddr:   "",
			adminToken: "",
			molEnv:     "",
			want:       "", // unset MOLECULE_ENV → not dev → all interfaces
		},
		{
			name:       "explicit_bindaddr_loopback_overrides_devmode",
			bindAddr:   "127.0.0.1",
			adminToken: "",
			molEnv:     "dev",
			want:       "127.0.0.1",
		},
		{
			name:       "explicit_bindaddr_wildcard_overrides_devmode_default",
			bindAddr:   "0.0.0.0",
			adminToken: "",
			molEnv:     "dev",
			want:       "0.0.0.0",
		},
		{
			name:       "explicit_bindaddr_in_production",
			bindAddr:   "10.0.5.7",
			adminToken: "secret",
			molEnv:     "production",
			want:       "10.0.5.7",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BIND_ADDR", tc.bindAddr)
			t.Setenv("ADMIN_TOKEN", tc.adminToken)
			t.Setenv("MOLECULE_ENV", tc.molEnv)
			got := resolveBindHost()
			if got != tc.want {
				t.Errorf("resolveBindHost() = %q, want %q (BIND_ADDR=%q ADMIN_TOKEN=%q MOLECULE_ENV=%q)",
					got, tc.want, tc.bindAddr, tc.adminToken, tc.molEnv)
			}
		})
	}
}
