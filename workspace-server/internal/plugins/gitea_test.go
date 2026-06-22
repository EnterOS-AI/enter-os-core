package plugins

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGiteaResolver_Scheme(t *testing.T) {
	if NewGiteaResolver().Scheme() != "gitea" {
		t.Errorf("scheme must be 'gitea'")
	}
}

func TestParseGiteaSpec(t *testing.T) {
	cases := []struct {
		in      string
		owner   string
		repo    string
		subpath string
		ref     string
		wantErr bool
	}{
		{in: "molecule-ai/repo#main", owner: "molecule-ai", repo: "repo", subpath: "", ref: "main"},
		{
			in:    "molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main",
			owner: "molecule-ai", repo: "molecule-ai-workspace-template-seo-agent",
			subpath: "agent-skills/seo-all", ref: "main",
		},
		{in: "o/r/sub#tag:v1.0.0", owner: "o", repo: "r", subpath: "sub", ref: "tag:v1.0.0"},
		{in: "o/r/sub#sha:abc123", owner: "o", repo: "r", subpath: "sub", ref: "sha:abc123"},
		{in: "o/r", owner: "o", repo: "r"}, // unpinned parse ok; Fetch enforces
		{in: "justone", wantErr: true},
		{in: "o/r/../etc#main", wantErr: true}, // traversal in subpath
		{in: "o/r/sub#-evil", wantErr: true},   // ref starting with -
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := parseGiteaSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseGiteaSpec(%q): expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseGiteaSpec(%q): unexpected err %v", c.in, err)
			continue
		}
		if got.owner != c.owner || got.repo != c.repo || got.subpath != c.subpath || got.ref != c.ref {
			t.Errorf("parseGiteaSpec(%q) = %+v, want owner=%q repo=%q subpath=%q ref=%q",
				c.in, got, c.owner, c.repo, c.subpath, c.ref)
		}
	}
}

func TestGiteaResolver_UnpinnedRejected(t *testing.T) {
	r := NewGiteaResolver()
	r.GitRunner = func(ctx context.Context, dir string, args ...string) error {
		t.Fatal("git must not run for an unpinned spec")
		return nil
	}
	_, err := r.Fetch(context.Background(), "molecule-ai/repo", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "pinned ref") {
		t.Errorf("expected pinned-ref rejection, got %v", err)
	}
}

func TestGiteaResolver_UnpinnedAllowedWithOverride(t *testing.T) {
	t.Setenv("PLUGIN_ALLOW_UNPINNED", "true")
	r := &GiteaResolver{
		GitRunner: stubGit(map[string]string{"plugin.yaml": "name: repo\n"}),
		BaseURL:   "file:///dev/null",
	}
	name, err := r.Fetch(context.Background(), "molecule-ai/repo", t.TempDir())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if name != "repo" {
		t.Errorf("whole-repo install name = %q, want repo", name)
	}
}

func TestGiteaResolver_TokenInjectedIntoCloneURL(t *testing.T) {
	t.Setenv("MY_PAT", "secret-token-xyz")
	r := &GiteaResolver{
		BaseURL:  "https://git.example.com",
		TokenEnv: "MY_PAT",
	}
	got, err := r.cloneURL("owner", "repo")
	if err != nil {
		t.Fatalf("cloneURL: %v", err)
	}
	if !strings.Contains(got, "secret-token-xyz:") || !strings.Contains(got, "@git.example.com") {
		t.Errorf("token not injected into clone URL userinfo: %q", got)
	}
	if !strings.HasSuffix(got, "/owner/repo.git") {
		t.Errorf("clone URL path wrong: %q", got)
	}
}

func TestGiteaResolver_NoTokenAnonymousURL(t *testing.T) {
	r := &GiteaResolver{BaseURL: "https://git.example.com", TokenEnv: "UNSET_PAT_VAR"}
	got, err := r.cloneURL("o", "r")
	if err != nil {
		t.Fatalf("cloneURL: %v", err)
	}
	if strings.Contains(got, "@") {
		t.Errorf("expected anonymous URL with no userinfo, got %q", got)
	}
}

// makeBareRepoWithSubpath builds a real git repo containing a subpath tree,
// commits it, and returns a bare clone the resolver can fetch via file://.
// Skips the test if git isn't on PATH.
func makeBareRepoWithSubpath(t *testing.T) (baseDir, owner, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root := t.TempDir()
	owner, repo = "owner", "repo"
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(work, "agent-skills", "seo-all", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Subpath plugin content.
	mustWrite(t, filepath.Join(work, "agent-skills", "seo-all", "plugin.yaml"),
		"name: seo-all\nversion: 1.0.0\nskills:\n  - seo-all\n")
	mustWrite(t, filepath.Join(work, "agent-skills", "seo-all", "SKILL.md"), "# SEO\n")
	mustWrite(t, filepath.Join(work, "agent-skills", "seo-all", "commands", "audit.md"), "# audit\n")
	// Sibling content that MUST NOT be copied (proves subpath isolation).
	mustWrite(t, filepath.Join(work, "config.yaml"), "name: template\n")
	mustWrite(t, filepath.Join(work, "agent-skills", "other", "SKILL.md"), "# other\n")

	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "t@t")
	runGit(t, work, "config", "user.name", "t")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "init")

	// Bare clone at <root>/owner/repo.git
	bareParent := filepath.Join(root, owner)
	if err := os.MkdirAll(bareParent, 0o755); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(bareParent, repo+".git")
	runGit(t, root, "clone", "-q", "--bare", work, bare)
	return root, owner, repo
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LANG=C", "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func TestGiteaResolver_SubpathExtraction_RealGit(t *testing.T) {
	root, owner, repo := makeBareRepoWithSubpath(t)
	r := &GiteaResolver{
		GitRunner: defaultGitRunner,
		BaseURL:   "file://" + root,
		TokenEnv:  "", // file:// — no auth
	}
	dst := t.TempDir()
	spec := owner + "/" + repo + "/agent-skills/seo-all#main"
	name, err := r.Fetch(context.Background(), spec, dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if name != "seo-all" {
		t.Errorf("plugin name = %q, want seo-all (last subpath segment)", name)
	}
	// The subpath contents are present at dst root (NOT nested under agent-skills/).
	for _, want := range []string{"plugin.yaml", "SKILL.md", "commands/audit.md"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("expected %q in dst, missing: %v", want, err)
		}
	}
	// Sibling / parent content MUST NOT have been copied.
	for _, notWant := range []string{"config.yaml", "agent-skills", ".git"} {
		if _, err := os.Stat(filepath.Join(dst, notWant)); !os.IsNotExist(err) {
			t.Errorf("subpath isolation violated: %q leaked into dst", notWant)
		}
	}
	// A SHA was captured for drift-seed parity.
	if r.LastSHA() == "" {
		t.Error("expected LastSHA to be set after a real clone")
	}
}

// TestGiteaResolver_SubpathSymlinkDoesNotLeakTarget (B2) commits a symlink
// inside the plugin subpath that points OUTSIDE the subpath (at a secret file
// holding sentinel content). copyTree must SKIP the symlink, so the staged
// output must contain neither the link nor the secret's content.
func TestGiteaResolver_SubpathSymlinkEscape(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const secret = "TOP-SECRET-LEAKED-CONTENT-9999"
	root := t.TempDir()
	owner, repo := "owner", "repo"
	work := filepath.Join(root, "work")
	skill := filepath.Join(work, "agent-skills", "seo-all")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	// Legit content inside the subpath.
	mustWrite(t, filepath.Join(skill, "plugin.yaml"), "name: seo-all\n")
	// A secret file at the repo ROOT — outside the subpath.
	mustWrite(t, filepath.Join(work, "SECRET"), secret)
	// A symlink INSIDE the subpath pointing up-and-out to the secret.
	if err := os.Symlink("../../SECRET", filepath.Join(skill, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	// A symlink to an absolute path outside the repo entirely.
	if err := os.Symlink("/etc/passwd", filepath.Join(skill, "passwd.txt")); err != nil {
		t.Fatal(err)
	}

	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "t@t")
	runGit(t, work, "config", "user.name", "t")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "init")
	bareParent := filepath.Join(root, owner)
	if err := os.MkdirAll(bareParent, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "clone", "-q", "--bare", work, filepath.Join(bareParent, repo+".git"))

	r := &GiteaResolver{GitRunner: defaultGitRunner, BaseURL: "file://" + root}
	dst := t.TempDir()
	if _, err := r.Fetch(context.Background(),
		owner+"/"+repo+"/agent-skills/seo-all#main", dst); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// The legit file is present.
	if _, err := os.Stat(filepath.Join(dst, "plugin.yaml")); err != nil {
		t.Errorf("expected plugin.yaml in dst: %v", err)
	}
	// The symlinks must NOT be present (skipped, not followed).
	for _, link := range []string{"leak.txt", "passwd.txt"} {
		if _, err := os.Lstat(filepath.Join(dst, link)); !os.IsNotExist(err) {
			t.Errorf("symlink %q must not be staged (err=%v)", link, err)
		}
	}
	// CRITICAL: the secret target content must not have leaked into ANY file
	// in the staged output.
	walkErr := filepath.Walk(dst, func(p string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), secret) {
			t.Errorf("symlink-target secret leaked into staged file %q", p)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk dst: %v", walkErr)
	}
}

func TestGiteaResolver_SubpathMissing_NotFound(t *testing.T) {
	root, owner, repo := makeBareRepoWithSubpath(t)
	r := &GiteaResolver{GitRunner: defaultGitRunner, BaseURL: "file://" + root}
	_, err := r.Fetch(context.Background(),
		owner+"/"+repo+"/agent-skills/does-not-exist#main", t.TempDir())
	if !errors.Is(err, ErrPluginNotFound) {
		t.Errorf("missing subpath: want ErrPluginNotFound, got %v", err)
	}
}

func TestGiteaResolver_ResolveRef_RealGit(t *testing.T) {
	root, owner, repo := makeBareRepoWithSubpath(t)
	r := &GiteaResolver{GitRunner: defaultGitRunner, BaseURL: "file://" + root}
	sha, err := r.ResolveRef(context.Background(), owner+"/"+repo+"/agent-skills/seo-all#main")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if len(sha) < 40 {
		t.Errorf("expected a full commit SHA, got %q", sha)
	}
}

func TestGiteaResolver_RegisteredScheme(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewGiteaResolver())
	src, err := ParseSource("gitea://o/r/sub#main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Resolve(src); err != nil {
		t.Errorf("gitea scheme must resolve: %v", err)
	}
}

// makePluginTarball returns a gzip-compressed tar archive containing a repo
// root directory named after repo, with files under the given relPaths.
func makePluginTarball(t *testing.T, repo string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	for rel, content := range files {
		name := repo + "/" + rel
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := io.WriteString(tw, content); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// writeTarball writes a tarball to dstDir, simulating defaultArchiveDownloader.
func writeTarball(t *testing.T, dstDir string, data []byte) {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		target := filepath.Join(dstDir, hdr.Name)
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("mkdir parent: %v", err)
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatalf("create file: %v", err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			t.Fatalf("copy: %v", err)
		}
		f.Close()
	}
}

func TestGiteaResolver_ArchiveFetch_PrivateRepo_FastFail(t *testing.T) {
	const tok = "SUPERSECRET-ARCHIVE-TOKEN-999"
	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", tok)

	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprintf("HTTP%d", code), func(t *testing.T) {
			r := &GiteaResolver{
				BaseURL:  "https://git.example.com",
				TokenEnv: "MOLECULE_TEMPLATE_REPO_TOKEN",
				ArchiveDownloader: func(ctx context.Context, archiveURL, token, dstDir string) error {
					if token != tok {
						t.Errorf("token not passed to downloader: got %q", token)
					}
					switch code {
					case http.StatusNotFound:
						return fmt.Errorf("gitea resolver: %s: %w", archiveURL, ErrPluginNotFound)
					default:
						return fmt.Errorf("gitea resolver: %s: repository not accessible (HTTP %d)", archiveURL, code)
					}
				},
			}

			start := time.Now()
			_, err := r.Fetch(context.Background(), "owner/repo#main", t.TempDir())
			if time.Since(start) > 2*time.Second {
				t.Errorf("Fetch took too long (%s); should fast-fail", time.Since(start))
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), tok) {
				t.Errorf("PAT leaked into error: %v", err)
			}
			if code == http.StatusNotFound {
				if !errors.Is(err, ErrPluginNotFound) {
					t.Errorf("expected ErrPluginNotFound for 404, got %v", err)
				}
			} else {
				if errors.Is(err, ErrPluginNotFound) {
					t.Errorf("did not expect ErrPluginNotFound for %d", code)
				}
			}
		})
	}
}

func TestGiteaResolver_ArchiveFetch_Timeout(t *testing.T) {
	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", "tok")
	r := &GiteaResolver{
		BaseURL:      "https://git.example.com",
		TokenEnv:     "MOLECULE_TEMPLATE_REPO_TOKEN",
		FetchTimeout: 500 * time.Millisecond,
		ArchiveDownloader: func(ctx context.Context, archiveURL, token, dstDir string) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Minute):
				return errors.New("should have been cancelled")
			}
		},
	}

	start := time.Now()
	_, err := r.Fetch(context.Background(), "owner/repo#main", t.TempDir())
	if time.Since(start) > 2*time.Second {
		t.Errorf("Fetch took too long (%s); timeout should fire around 500ms", time.Since(start))
	}
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("expected timeout/deadline error, got %v", err)
	}
}

func TestGiteaResolver_ArchiveFetch_Success(t *testing.T) {
	const tok = "SUPERSECRET-ARCHIVE-TOKEN-777"
	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", tok)

	archive := makePluginTarball(t, "repo", map[string]string{
		"plugin.yaml": "name: repo\nversion: 1.0.0\n",
		"README.md":   "# repo",
	})
	const wantSHA = "abc123def456abc123def456abc123def456abcd"

	commitsHandler := func(w http.ResponseWriter, req *http.Request) {
		if auth := req.Header.Get("Authorization"); auth != "token "+tok {
			t.Errorf("commits request auth header = %q, want token %s", auth, tok)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"sha":"%s"}]`, wantSHA)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/commits") {
			commitsHandler(w, req)
			return
		}
		http.NotFound(w, req)
	}))
	defer server.Close()

	r := &GiteaResolver{
		BaseURL:          server.URL,
		TokenEnv:         "MOLECULE_TEMPLATE_REPO_TOKEN",
		ResolveRefClient: server.Client(),
		ArchiveDownloader: func(ctx context.Context, archiveURL, token, dstDir string) error {
			if token != tok {
				t.Errorf("token not passed to downloader: got %q", token)
			}
			writeTarball(t, dstDir, archive)
			return nil
		},
	}

	dst := t.TempDir()
	name, err := r.Fetch(context.Background(), "owner/repo#main", dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if name != "repo" {
		t.Errorf("plugin name = %q, want repo", name)
	}
	for _, want := range []string{"plugin.yaml", "README.md"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("expected %q in dst: %v", want, err)
		}
	}
	if r.LastSHA() != wantSHA {
		t.Errorf("LastSHA = %q, want %q", r.LastSHA(), wantSHA)
	}
}

func TestGiteaResolver_ArchiveFetch_Subpath(t *testing.T) {
	const wantSHA = "abc123def456abc123def456abc123def456abcd"
	archive := makePluginTarball(t, "template", map[string]string{
		"agent-skills/seo-all/plugin.yaml": "name: seo-all\n",
		"agent-skills/seo-all/SKILL.md":    "# SEO",
		"config.yaml":                      "name: template",
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/commits") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"sha":"%s"}]`, wantSHA)
			return
		}
		http.NotFound(w, req)
	}))
	defer server.Close()

	r := &GiteaResolver{
		BaseURL:          server.URL,
		ResolveRefClient: server.Client(),
		ArchiveDownloader: func(ctx context.Context, archiveURL, token, dstDir string) error {
			writeTarball(t, dstDir, archive)
			return nil
		},
	}

	dst := t.TempDir()
	name, err := r.Fetch(context.Background(), "owner/template/agent-skills/seo-all#main", dst)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if name != "seo-all" {
		t.Errorf("plugin name = %q, want seo-all", name)
	}
	for _, want := range []string{"plugin.yaml", "SKILL.md"} {
		if _, err := os.Stat(filepath.Join(dst, want)); err != nil {
			t.Errorf("expected %q in dst: %v", want, err)
		}
	}
	for _, notWant := range []string{"config.yaml", "agent-skills"} {
		if _, err := os.Stat(filepath.Join(dst, notWant)); !os.IsNotExist(err) {
			t.Errorf("subpath isolation violated: %q leaked", notWant)
		}
	}
	if r.LastSHA() != wantSHA {
		t.Errorf("LastSHA = %q, want %q", r.LastSHA(), wantSHA)
	}
}

func TestGiteaResolver_ArchiveFetch_ResolveRef(t *testing.T) {
	const wantSHA = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if auth := req.Header.Get("Authorization"); auth != "token the-token" {
			t.Errorf("auth = %q, want token the-token", auth)
		}
		if strings.HasSuffix(req.URL.Path, "/commits") {
			q := req.URL.Query()
			if got := q.Get("sha"); got != "main" {
				t.Errorf("sha query = %q, want main", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"sha":"%s"}]`, wantSHA)
			return
		}
		http.NotFound(w, req)
	}))
	defer server.Close()

	t.Setenv("MOLECULE_TEMPLATE_REPO_TOKEN", "the-token")
	r := &GiteaResolver{
		BaseURL:          server.URL,
		TokenEnv:         "MOLECULE_TEMPLATE_REPO_TOKEN",
		ResolveRefClient: server.Client(),
	}

	sha, err := r.ResolveRef(context.Background(), "owner/repo#main")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if sha != wantSHA {
		t.Errorf("ResolveRef = %q, want %q", sha, wantSHA)
	}
}

func TestGiteaResolver_ArchiveFetch_ResolveRef_TagPrefix(t *testing.T) {
	const wantSHA = "1111111111111111111111111111111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/commits") {
			q := req.URL.Query()
			if got := q.Get("sha"); got != "v1.2.0" {
				t.Errorf("sha query = %q, want v1.2.0", got)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `[{"sha":"%s"}]`, wantSHA)
			return
		}
		http.NotFound(w, req)
	}))
	defer server.Close()

	r := &GiteaResolver{
		BaseURL:          server.URL,
		ResolveRefClient: server.Client(),
	}

	sha, err := r.ResolveRef(context.Background(), "owner/repo#tag:v1.2.0")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if sha != wantSHA {
		t.Errorf("ResolveRef = %q, want %q", sha, wantSHA)
	}
}
