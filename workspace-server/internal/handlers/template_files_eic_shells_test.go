package handlers

// template_files_eic_shells_test.go — pure-function tests for the
// remote shell builders + parser. Factored out of the EIC helpers so
// the wire shape can be pinned without standing up a real EIC tunnel
// or sshd. If a future edit changes the find/install/cat/rm shell in
// a way that drifts from the local-Docker container path, these tests
// catch it before staging.

import (
	"strings"
	"testing"
)

// TestBuildInstallShell pins the write-side remote command. `install`
// (not `cp`/`tee`) is load-bearing — it creates parent dirs (-D) and
// writes atomically via temp-file-rename. Permissions 0644 match the
// local-Docker tar-unpack defaults so a save → restart → save → restart
// cycle doesn't flip-flop file modes per backend.
func TestBuildInstallShell(t *testing.T) {
	got := buildInstallShell("/configs/config.yaml")
	wants := []string{
		"sudo -n",                                   // privilege escalation for root-owned /configs
		"install -D",                                // creates parent dirs
		"-m 0644",                                   // permission contract
		"/dev/stdin",                                // pipe-from-ssh source
		"'/configs/config.yaml'",                    // shell-quoted destination
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("buildInstallShell missing %q in: %s", w, got)
		}
	}
}

// TestBuildCatShell pins the read-side remote command. `2>/dev/null`
// is load-bearing: without it the missing-file case emits "cat: ...:
// No such file" to stderr, and the helper's "empty stdout + empty
// stderr → os.ErrNotExist" classifier fires the wrong branch (500
// instead of 404). The tunnel-warning silencer (LogLevel=ERROR in
// sshArgs) handles the ssh side; this one handles the remote-cmd side.
func TestBuildCatShell(t *testing.T) {
	got := buildCatShell("/home/ubuntu/.hermes/config.yaml")
	wants := []string{
		"sudo -n",
		"cat",
		"'/home/ubuntu/.hermes/config.yaml'",
		"2>/dev/null", // missing-file → empty stdout + non-zero exit
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("buildCatShell missing %q in: %s", w, got)
		}
	}
}

// TestBuildRmShell pins `rm -f`, NOT `rm -rf`. A misclassified
// directory entry passing through the validator must NOT trigger a
// recursive delete. Directory removal needs its own explicit endpoint
// when/if the canvas grows that affordance.
func TestBuildRmShell(t *testing.T) {
	got := buildRmShell("/configs/dead.yaml")
	wants := []string{"sudo -n", "rm -f", "'/configs/dead.yaml'"}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("buildRmShell missing %q in: %s", w, got)
		}
	}
	// Negative assertion: NEVER emit -rf.
	if strings.Contains(got, "rm -rf") {
		t.Errorf("buildRmShell uses -rf, must use -f only: %s", got)
	}
}

// TestBuildFindShell pins the listing-side remote command — it must
// match the local-Docker path's parser shape (TYPE|SIZE|REL_PATH per
// line) AND prune the same hidden / cache directories. If either
// side drifts, a /workspace listing on EC2 either drowns in node_modules
// noise (pruning regression) or drops files entirely (parser shape
// regression).
func TestBuildFindShell(t *testing.T) {
	got := buildFindShell("/workspace", 2)
	wants := []string{
		"sudo -n find",
		"'/workspace'",
		"-maxdepth 2",
		// Matches local-Docker container path; without these the EC2
		// listing fills with VCS/build artefacts.
		"-not -path '*/.git/*'",
		"-not -path '*/__pycache__/*'",
		"-not -path '*/node_modules/*'",
		"-not -name .DS_Store",
		"2>/dev/null", // missing-root → empty stdout + non-zero exit
		// Wire shape — emit "TYPE|SIZE|REL_PATH" so parseFindOutput
		// (and the canvas tree builder) can decode each line.
		"d|0|",
		"f|",
		// Portable stat: GNU first, BSD fallback, then 0.
		"stat -c %s",
		"stat -f %z",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("buildFindShell missing %q in: %s", w, got)
		}
	}
}

// TestBuildFindShell_DepthForwarding catches a regression where the
// helper hard-codes a depth instead of using the caller's value.
// `?depth=` on the canvas side controls how many levels expand on
// load — losing it means the file tree is either empty (depth=0) or
// the network blows up on a top-level /home with everyone's $HOME
// (uncapped).
func TestBuildFindShell_DepthForwarding(t *testing.T) {
	for _, d := range []int{1, 3, 5} {
		got := buildFindShell("/configs", d)
		want := "-maxdepth " + intToStr(d)
		if !strings.Contains(got, want) {
			t.Errorf("buildFindShell depth=%d output missing %q: %s", d, want, got)
		}
	}
}

// intToStr avoids pulling strconv into a one-liner; matches the shell
// builder's fmt.Sprintf %d output exactly.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	s := string(buf[i:])
	if neg {
		return "-" + s
	}
	return s
}

// TestParseFindOutput pins the parser. Each line is TYPE|SIZE|REL,
// blank/short lines silently skipped. Pre-PR-A this logic was inlined
// in the handler with the same shape; extracting + testing separately
// removes the "regex passes against the inline parser but a future
// refactor of the handler subtly changes the parse" failure mode.
func TestParseFindOutput(t *testing.T) {
	in := []byte(`d|0|nested
f|123|nested/a.yaml
f|45|README.md

invalid-line
f||no-size
d|0|
`)
	got := parseFindOutput(in)
	// Want 4 entries: nested(d), nested/a.yaml(f,123), README.md(f,45),
	// no-size(f,0). Blank lines, "invalid-line" (no pipes), and
	// `d|0|` (empty rel) are skipped.
	wantPaths := []string{"nested", "nested/a.yaml", "README.md", "no-size"}
	if len(got) != len(wantPaths) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(wantPaths), got)
	}
	for i, w := range wantPaths {
		if got[i].Path != w {
			t.Errorf("entry[%d].Path = %q, want %q", i, got[i].Path, w)
		}
	}
	if !got[0].Dir {
		t.Errorf("entry[0] should be Dir")
	}
	if got[1].Size != 123 {
		t.Errorf("entry[1].Size = %d, want 123", got[1].Size)
	}
	if got[3].Size != 0 {
		t.Errorf("entry[3].Size on missing-size line = %d, want 0", got[3].Size)
	}
}

// TestParseFindOutput_EmptyInput — a missing listing root yields
// empty stdout (find swallows the "No such file" via 2>/dev/null),
// which must round-trip to a JSON `[]`, not null. The handler does
// `make([]eicFileEntry, 0)` to enforce this; the test pins the
// helper-level guarantee independently.
func TestParseFindOutput_EmptyInput(t *testing.T) {
	got := parseFindOutput([]byte(""))
	if got == nil {
		t.Errorf("parseFindOutput(\"\") returned nil; want empty slice for JSON []")
	}
	if len(got) != 0 {
		t.Errorf("parseFindOutput(\"\") = %+v; want []", got)
	}
}
