package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseDotEnvLine(t *testing.T) {
	cases := []struct {
		in      string
		k, v    string
		ok      bool
		comment string
	}{
		{in: "", ok: false, comment: "empty line"},
		{in: "   ", ok: false, comment: "whitespace-only"},
		{in: "# top-level comment", ok: false, comment: "full-line comment"},
		{in: "  #  indented comment", ok: false, comment: "indented full-line comment"},
		{in: "FOO", ok: false, comment: "no equals"},
		{in: "=BAR", ok: false, comment: "missing key"},

		{in: "FOO=bar", k: "FOO", v: "bar", ok: true, comment: "plain"},
		{in: "  FOO=bar", k: "FOO", v: "bar", ok: true, comment: "leading whitespace"},
		{in: "FOO=bar   ", k: "FOO", v: "bar", ok: true, comment: "trailing whitespace stripped"},
		{in: "FOO  =bar", k: "FOO", v: "bar", ok: true, comment: "whitespace before equals"},

		{in: "FOO=bar # comment", k: "FOO", v: "bar", ok: true, comment: "inline space-hash comment"},
		{in: "FOO=bar\t# comment", k: "FOO", v: "bar", ok: true, comment: "inline tab-hash comment"},
		{in: "FOO=bar    # lots of spaces", k: "FOO", v: "bar", ok: true, comment: "multiple spaces before hash"},

		{in: "FOO=bar#nocomment", k: "FOO", v: "bar#nocomment", ok: true, comment: "bare hash inside value preserved"},
		{in: "URL=postgres://u:p@h:5432/db?sslmode=disable", k: "URL", v: "postgres://u:p@h:5432/db?sslmode=disable", ok: true, comment: "url with embedded equals"},
		{in: "TOKEN=eyJhbGciOiJIUzI1NiJ9.payload.sig=", k: "TOKEN", v: "eyJhbGciOiJIUzI1NiJ9.payload.sig=", ok: true, comment: "base64 padding preserved"},

		{in: "FOO=", k: "FOO", v: "", ok: true, comment: "empty value"},
		{in: "ADMIN_TOKEN=", k: "ADMIN_TOKEN", v: "", ok: true, comment: "empty value (production gate sentinel)"},

		// Regression: the repo's own .env contains lines like
		// `CONFIGS_DIR=                   # Path to ...` where the value
		// is empty + an inline comment. Pre-fix parser stripped leading
		// whitespace BEFORE detecting the comment, leaving `#` at v[0]
		// with nothing preceding it, so the inline-comment check missed
		// it and the comment text was returned as the value. Server
		// then tried to use the comment as a directory path and template
		// loading silently failed (GET /templates returned []).
		{in: "CONFIGS_DIR=                   # Path to /var/foo (auto-discovered if empty)", k: "CONFIGS_DIR", v: "", ok: true, comment: "empty value with leading whitespace + inline comment"},
		{in: "FOO=    # comment", k: "FOO", v: "", ok: true, comment: "spaces-only value with inline comment"},
		{in: "FOO=\t# comment", k: "FOO", v: "", ok: true, comment: "tab-only value with inline comment"},

		// `export` prefix: shell-friendly .env files (direnv, .envrc-style)
		// — the prefix must be stripped, NOT folded into the key.
		{in: "export FOO=bar", k: "FOO", v: "bar", ok: true, comment: "export prefix stripped"},
		{in: "  export FOO=bar", k: "FOO", v: "bar", ok: true, comment: "leading whitespace + export"},
		{in: "export DATABASE_URL=postgres://u:p@h/db", k: "DATABASE_URL", v: "postgres://u:p@h/db", ok: true, comment: "export with URL value"},

		// Quoted values: one matched pair of surrounding quotes is
		// stripped; embedded `#` survives because it isn't an inline
		// comment inside a quote.
		{in: `FOO="hello world"`, k: "FOO", v: "hello world", ok: true, comment: "double-quoted value"},
		{in: `FOO='hello world'`, k: "FOO", v: "hello world", ok: true, comment: "single-quoted value"},
		{in: `FOO="value # not a comment"`, k: "FOO", v: "value # not a comment", ok: true, comment: "hash inside quotes is part of value"},
		{in: `FOO=  "padded"`, k: "FOO", v: "padded", ok: true, comment: "whitespace before opening quote"},
		{in: `FOO="unterminated`, k: "FOO", v: `"unterminated`, ok: true, comment: "unterminated quote stays as bare value"},

		// CRLF endings: bufio.Scanner strips \n; \r is left and stripped
		// by the value-side TrimSpace. Locking this in so a future
		// refactor doesn't accidentally feed \r into os.Setenv.
		{in: "FOO=bar\r", k: "FOO", v: "bar", ok: true, comment: "CRLF trailing carriage return stripped"},

		// UTF-8 BOM at file start: a Windows-edited .env begins with
		// \xEF\xBB\xBF; without explicit stripping the first key would
		// be "\ufeffFOO".
		{in: "\ufeffFOO=bar", k: "FOO", v: "bar", ok: true, comment: "UTF-8 BOM stripped"},
	}

	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			k, v, ok := parseDotEnvLine(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (input=%q)", ok, tc.ok, tc.in)
			}
			if !tc.ok {
				return
			}
			if k != tc.k || v != tc.v {
				t.Fatalf("got (%q, %q), want (%q, %q)", k, v, tc.k, tc.v)
			}
		})
	}
}

// makeFakeMonorepo creates a temp dir that satisfies isMonorepoRoot()
// (i.e., contains workspace-server/go.mod) plus a .env file with the
// given body. Returns the dir so the caller can chdir into it.
func makeFakeMonorepo(t *testing.T, envBody string) string {
	t.Helper()
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "workspace-server")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, "go.mod"), []byte("module fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envBody), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return dir
}

func TestLoadDotEnvIfPresent_PreservesExisting(t *testing.T) {
	dir := makeFakeMonorepo(t, "DOTENV_TEST_NEW=from_file\nDOTENV_TEST_EXISTING=from_file\n")

	// Pre-set one of the keys — file value must NOT clobber it.
	t.Setenv("DOTENV_TEST_EXISTING", "from_real_env")
	// Ensure the other key starts unset.
	os.Unsetenv("DOTENV_TEST_NEW")
	t.Cleanup(func() { os.Unsetenv("DOTENV_TEST_NEW") })

	// Run from the temp dir so findDotEnv picks our fixture.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	loadDotEnvIfPresent()

	if got := os.Getenv("DOTENV_TEST_NEW"); got != "from_file" {
		t.Errorf("DOTENV_TEST_NEW = %q, want %q", got, "from_file")
	}
	if got := os.Getenv("DOTENV_TEST_EXISTING"); got != "from_real_env" {
		t.Errorf("existing env clobbered: got %q, want %q", got, "from_real_env")
	}
}

func TestLoadDotEnvIfPresent_NoFile_NoOp(t *testing.T) {
	dir := t.TempDir() // empty — no .env at this level
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	// Should not panic, log loud errors, or set anything. Best-effort
	// silent miss is the contract.
	loadDotEnvIfPresent()
}

func TestFindDotEnv_WalksUpward(t *testing.T) {
	root := makeFakeMonorepo(t, "X=1\n")
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	got, ok := findDotEnv()
	if !ok {
		t.Fatal("expected to find .env walking upward")
	}
	want := filepath.Join(root, ".env")
	// macOS resolves /var → /private/var on TempDir, so compare via
	// EvalSymlinks for both sides to dodge that.
	gotR, _ := filepath.EvalSymlinks(got)
	wantR, _ := filepath.EvalSymlinks(want)
	if gotR != wantR {
		t.Errorf("findDotEnv() = %q, want %q", got, want)
	}
}

func TestFindDotEnv_RejectsUnrelatedDotEnv(t *testing.T) {
	// Simulates a developer running the binary from inside an
	// unrelated project tree that happens to have its own .env (or
	// from $HOME with a personal ~/.env). Without the monorepo
	// sentinel, findDotEnv would happily load it and clobber env
	// with arbitrary values — a real foot-gun this regression test
	// guards against.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LEAKY=value\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	if got, ok := findDotEnv(); ok {
		t.Errorf("findDotEnv() = %q, ok=true; want ok=false (no workspace-server sibling)", got)
	}
}
