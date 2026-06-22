package middleware

import (
	"testing"
)

// Unit tests for the isLocalDevEnv predicate.
//
// (harden/no-fail-open-auth) This predicate replaced the old
// isDevModeFailOpen() auth escape hatch. It carries NO authentication
// semantics and does NOT consult ADMIN_TOKEN — it reports ONLY whether
// MOLECULE_ENV names a local-dev environment. It gates non-security knobs
// (rate-limit relaxation, loopback bind default). The fail-CLOSED auth
// behaviour is enforced by no_fail_open_test.go.

func TestIsLocalDevEnv_Development_True(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "development")
	if !isLocalDevEnv() {
		t.Error("expected MOLECULE_ENV=development to be local dev")
	}
}

func TestIsLocalDevEnv_ShortAlias_True(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "dev")
	if !isLocalDevEnv() {
		t.Error("expected MOLECULE_ENV=dev to be treated as local dev")
	}
}

func TestIsLocalDevEnv_IgnoresAdminToken(t *testing.T) {
	// Decoupled from ADMIN_TOKEN: dev now provisions one, but the bind /
	// rate-limit knobs still treat the env as local dev. Crucially this
	// predicate grants no access, so the coupling no longer matters.
	t.Setenv("MOLECULE_ENV", "development")
	t.Setenv("ADMIN_TOKEN", "operator-set-this")
	if !isLocalDevEnv() {
		t.Error("ADMIN_TOKEN must not affect isLocalDevEnv (env-only predicate)")
	}
}

func TestIsLocalDevEnv_Production_False(t *testing.T) {
	t.Setenv("MOLECULE_ENV", "production")
	if isLocalDevEnv() {
		t.Error("production must not count as local dev")
	}
}

func TestIsLocalDevEnv_CaseInsensitive(t *testing.T) {
	cases := []string{"Development", "DEVELOPMENT", "Dev", "DEV", "  dev  "}
	for _, env := range cases {
		t.Run(env, func(t *testing.T) {
			t.Setenv("MOLECULE_ENV", env)
			if !isLocalDevEnv() {
				t.Errorf("MOLECULE_ENV=%q should count as local dev", env)
			}
		})
	}
}

func TestIsLocalDevEnv_UnknownEnv_False(t *testing.T) {
	cases := []string{"", "staging", "local", "preview", "test", "devel"}
	for _, env := range cases {
		t.Run(env, func(t *testing.T) {
			t.Setenv("MOLECULE_ENV", env)
			if isLocalDevEnv() {
				t.Errorf("MOLECULE_ENV=%q must not count as local dev", env)
			}
		})
	}
}
