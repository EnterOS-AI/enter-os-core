package provisioner

import (
	"os"
	"runtime"
	"testing"
)

// Tests for defaultImagePlatform + parseOCIPlatform.
//
// The platform-forcing helper unblocks Apple Silicon dev boxes — see
// issue #1875. SaaS production (linux/amd64 EC2) must NOT hit the
// forced-platform branch, which is what the "no override + linux host"
// and the explicit-empty-override tests lock in.

func TestDefaultImagePlatform_EnvOverride_ExplicitValue(t *testing.T) {
	t.Setenv("MOLECULE_IMAGE_PLATFORM", "linux/arm64")
	got := defaultImagePlatform()
	if got != "linux/arm64" {
		t.Errorf("expected env override to win, got %q", got)
	}
}

func TestDefaultImagePlatform_EnvOverride_EmptyValue(t *testing.T) {
	// An explicitly empty env var disables the auto-force. This is the
	// escape hatch for operators who don't want the fallback but also
	// haven't pinned an alternate platform.
	t.Setenv("MOLECULE_IMAGE_PLATFORM", "")
	got := defaultImagePlatform()
	if got != "" {
		t.Errorf("expected empty override to suppress auto-force, got %q", got)
	}
}

func TestDefaultImagePlatform_AutoDetect(t *testing.T) {
	// Clear any override the test runner inherited so we see pure
	// auto-detect behaviour.
	t.Setenv("MOLECULE_IMAGE_PLATFORM", "")
	// Re-run without the env var at all — t.Setenv already backs up,
	// but we need to Unsetenv for the LookupEnv branch to miss.
	if err := unsetEnvForTest(t, "MOLECULE_IMAGE_PLATFORM"); err != nil {
		t.Fatalf("unset env: %v", err)
	}

	got := defaultImagePlatform()
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		if got != "linux/amd64" {
			t.Errorf("Apple Silicon: expected linux/amd64 auto-force, got %q", got)
		}
	default:
		if got != "" {
			t.Errorf("non-Apple-Silicon host: expected no auto-force, got %q", got)
		}
	}
}

func TestParseOCIPlatform(t *testing.T) {
	cases := []struct {
		in     string
		wantOS string
		wantCPU string
		wantNil bool
	}{
		{"", "", "", true},
		{"linux/amd64", "linux", "amd64", false},
		{"linux/arm64", "linux", "arm64", false},
		// Malformed inputs must return nil so ContainerCreate falls back
		// to "no preference" instead of getting a half-populated struct.
		{"linux", "", "", true},
		{"linux/", "", "", true},
		{"/amd64", "", "", true},
		{"linux/amd64/v8", "linux", "amd64/v8", false}, // current parser: everything after first "/" is arch
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := parseOCIPlatform(tc.in)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("unexpected nil for %q", tc.in)
			}
			if got.OS != tc.wantOS || got.Architecture != tc.wantCPU {
				t.Errorf("parse %q = %+v, want OS=%q Arch=%q",
					tc.in, got, tc.wantOS, tc.wantCPU)
			}
		})
	}
}

// unsetEnvForTest removes an env var for the duration of the test and
// restores it on cleanup. t.Setenv only supports setting, not removing;
// we need the unset path to test the "no override" branch.
func unsetEnvForTest(t *testing.T, key string) error {
	t.Helper()
	prev, existed := os.LookupEnv(key)
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
	return os.Unsetenv(key)
}
