package provisioner

import (
	"strings"
	"testing"
)

// Tests for the new mode-detection surface. The legacy RegistryPrefix()
// shim is covered by registry_test.go; these tests pin the explicit
// two-mode discriminated return from Resolve().

// TestResolve_LocalModeWhenRegistryUnset — the OSS-contributor default.
// Issue #63: with MOLECULE_IMAGE_REGISTRY unset, the provisioner must
// switch to the local-build path instead of trying to pull from a GHCR
// org that's been suspended.
func TestResolve_LocalModeWhenRegistryUnset(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	got := Resolve()
	if got.Mode != RegistryModeLocal {
		t.Errorf("Mode = %q, want %q (unset registry → local-build)", got.Mode, RegistryModeLocal)
	}
	if got.Prefix != localImagePrefix {
		t.Errorf("Prefix = %q, want %q", got.Prefix, localImagePrefix)
	}
}

// TestResolve_SaaSModeWhenRegistrySet — production tenants set the var
// to their ECR mirror; we must keep producing pull-style image refs.
func TestResolve_SaaSModeWhenRegistrySet(t *testing.T) {
	const ecr = "123456789012.dkr.ecr.us-east-2.amazonaws.com/molecule-ai"
	t.Setenv("MOLECULE_IMAGE_REGISTRY", ecr)
	got := Resolve()
	if got.Mode != RegistryModeSaaS {
		t.Errorf("Mode = %q, want %q (set registry → saas)", got.Mode, RegistryModeSaaS)
	}
	if got.Prefix != ecr {
		t.Errorf("Prefix = %q, want %q", got.Prefix, ecr)
	}
}

// TestResolve_EmptyEnvIsLocalMode — operator who set the var to "" via
// a misconfigured deploy must NOT silently produce malformed image refs;
// they get the local path which fails loudly if Docker is missing.
// This contract is the safer-blast-radius half of Issue #63.
func TestResolve_EmptyEnvIsLocalMode(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_REGISTRY", "")
	if Resolve().Mode != RegistryModeLocal {
		t.Fatalf("empty MOLECULE_IMAGE_REGISTRY should be local-mode, got %q", Resolve().Mode)
	}
}

// TestResolve_GarbageURL — a registry value that's syntactically malformed
// (e.g. `not-a-url`, `foo bar`) is still treated as SaaS-mode. The whole
// design of MOLECULE_IMAGE_REGISTRY is "operator-supplied trusted value";
// validating the URL here would be pretending we can prevent operator
// error. The downstream docker-pull will fail loudly with a registry-
// shaped error message, which is the right blast radius.
func TestResolve_GarbageURLStillSaaSMode(t *testing.T) {
	for _, garbage := range []string{
		"not-a-url",
		"http://",
		"ghcr.io/",
		"   ",
		"\thello\n",
	} {
		t.Run(garbage, func(t *testing.T) {
			t.Setenv("MOLECULE_IMAGE_REGISTRY", garbage)
			if Resolve().Mode != RegistryModeSaaS {
				t.Errorf("Mode = %q, want saas (any non-empty value is SaaS-mode by design)", Resolve().Mode)
			}
		})
	}
}

// TestRegistryPrefix_AlignedWithResolve — the back-compat shim must
// agree with Resolve().Prefix on every input the new code distinguishes.
func TestRegistryPrefix_AlignedWithResolve(t *testing.T) {
	cases := []struct {
		name string
		env  string
	}{
		{"unset", ""},
		{"ecr", "999999999999.dkr.ecr.us-east-2.amazonaws.com/molecule-ai"},
		{"harbor", "harbor.example.com/molecule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MOLECULE_IMAGE_REGISTRY", tc.env)
			gotPrefix := RegistryPrefix()
			gotResolve := Resolve().Prefix
			// Note: with the new design, RegistryPrefix() unset returns
			// the SaaS GHCR default (legacy back-compat) while
			// Resolve().Prefix returns the local-mode "molecule-local"
			// hostname. They DIVERGE on the unset path by design — that
			// divergence is what closes the GHCR-403 hole. Pin both so a
			// future refactor can't accidentally re-couple them.
			if tc.env == "" {
				if gotPrefix != defaultRegistryPrefix {
					t.Errorf("RegistryPrefix() = %q, want %q (legacy shim)", gotPrefix, defaultRegistryPrefix)
				}
				if gotResolve != localImagePrefix {
					t.Errorf("Resolve().Prefix = %q, want %q (local-build hostname)", gotResolve, localImagePrefix)
				}
			} else {
				if gotPrefix != tc.env {
					t.Errorf("RegistryPrefix() = %q, want %q", gotPrefix, tc.env)
				}
				if gotResolve != tc.env {
					t.Errorf("Resolve().Prefix = %q, want %q", gotResolve, tc.env)
				}
			}
		})
	}
}

// TestIsKnownRuntime — defence-in-depth guard for the local-build path.
// Must accept every entry in knownRuntimes and reject anything else.
func TestIsKnownRuntime(t *testing.T) {
	for _, rt := range knownRuntimes {
		if !IsKnownRuntime(rt) {
			t.Errorf("IsKnownRuntime(%q) = false, want true", rt)
		}
	}
	for _, bad := range []string{
		"", "unknown", "WORKSPACE-TEMPLATE-FAKE", "../../../etc/passwd",
		"langgraph;rm -rf /", "claude-code\n", " langgraph",
	} {
		if IsKnownRuntime(bad) {
			t.Errorf("IsKnownRuntime(%q) = true, want false (untrusted input)", bad)
		}
	}
}

// TestLocalImagePrefix_Stable — the synthetic prefix is part of the
// public surface; admin handlers and image-watch use it to short-circuit
// network calls. Pin the constant.
func TestLocalImagePrefix_Stable(t *testing.T) {
	if got := LocalImagePrefix(); got != "molecule-local" {
		t.Errorf("LocalImagePrefix() = %q, want %q", got, "molecule-local")
	}
}

// TestLocalImagePrefix_NoDots — the synthetic hostname must not contain
// a `.` because Docker's image-ref parser would interpret it as a real
// DNS-resolvable registry. With no dot, the daemon treats `molecule-local`
// as the registry hostname only when explicitly tagged that way locally,
// and never tries to resolve it via DNS for a pull.
func TestLocalImagePrefix_NoDots(t *testing.T) {
	if strings.Contains(LocalImagePrefix(), ".") {
		t.Errorf("LocalImagePrefix() = %q contains '.' — Docker would attempt DNS resolution", LocalImagePrefix())
	}
}
