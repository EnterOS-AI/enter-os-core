package main

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// loadDotEnvIfPresent walks upward from CWD looking for a .env file and
// merges its KEY=VALUE pairs into the process environment. Already-set
// vars (e.g. from `docker run -e`, CI exports, or ad-hoc `KEY=val
// ./binary`) win over file values so operators can override without
// editing the file.
//
// Why walk upward: the binary may be launched from the monorepo root,
// the workspace-server subdir, or anywhere else the operator finds
// convenient. Walking upward from CWD finds the canonical .env
// (gitignored, lives at the monorepo root) regardless of cwd, so a
// fresh `go build -o /tmp/molecule-server ./cmd/server && /tmp/molecule-server`
// from any subdir picks up the same MOLECULE_ENV / DATABASE_URL / etc.
// the operator already has — without sourcing or `set -a`.
//
// Why no godotenv dep: the format we use is simple — KEY=VALUE with
// optional `#` comments and no interpolation — so a tiny in-tree parser
// is auditable, has no supply-chain surface, and avoids drift across
// repos where some teams configure godotenv differently.
//
// Why it's safe in production: the Dockerfile does not COPY .env into
// the image and `.env` is gitignored, so production containers have no
// .env on disk to load. If an operator goes out of their way to put one
// there, the explicit-env-wins rule above means container env still
// dominates.
func loadDotEnvIfPresent() {
	path, ok := findDotEnv()
	if !ok {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		log.Printf(".env: open %s: %v (skipping)", path, err)
		return
	}
	defer f.Close()

	loaded := 0
	skipped := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		k, v, ok := parseDotEnvLine(scanner.Text())
		if !ok {
			continue
		}
		// Existing env wins. NOTE: an explicitly-set empty string
		// (`KEY=` exported from a parent shell) counts as "set" — we
		// keep the empty value rather than backfilling from the file.
		// Matches Node's `process.env[k] !== undefined` check in the
		// canvas's next.config.ts loader so both processes treat the
		// same input identically. Operators who want the file value
		// to win must `unset KEY` in the launching shell.
		if _, exists := os.LookupEnv(k); exists {
			skipped++
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			log.Printf(".env: set %s: %v", k, err)
			continue
		}
		loaded++
	}
	if err := scanner.Err(); err != nil {
		log.Printf(".env: scan %s: %v", path, err)
	}
	log.Printf(".env: %s — loaded %d, %d already set in env", path, loaded, skipped)
}

// findDotEnv returns the path of the nearest .env file walking upward
// from CWD. Capped at 6 levels so a deeply-nested launch dir doesn't
// scan the entire filesystem.
//
// Sentinel gate: only accept a .env that sits next to `workspace-server/`
// (the monorepo marker). Without it, a developer running the binary from
// `~/Documents/other-project/` would walk up to `~/.env` and load
// arbitrary variables — a real foot-gun on shared dev machines and a
// possible information-leak vector on bare-metal deploys. Skipping the
// match falls through to "no .env found" which is identical to today's
// pre-fix behavior (the operator must export env explicitly).
func findDotEnv() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, ".env")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if isMonorepoRoot(dir) {
				return p, true
			}
			// .env exists here but the directory isn't the monorepo
			// root — keep walking. Loading it could clobber
			// environment with values from an unrelated project.
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// isMonorepoRoot returns true if `dir` looks like the molecule-core
// monorepo root — the directory that owns the .env we want to load.
// The marker is `workspace-server/go.mod`, which is the canonical
// in-tree go module and exists only in this monorepo. A simple
// `workspace-server/` directory check would false-positive on a fork
// that renamed the dir; the go.mod check is more precise.
func isMonorepoRoot(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "workspace-server", "go.mod"))
	return err == nil && !st.IsDir()
}

// parseDotEnvLine parses a single .env line. Returns (key, value, true)
// for KEY=VALUE pairs. Returns (_, _, false) for blanks, comments, and
// malformed lines. Handles:
//   - leading `export ` prefix (so shell-friendly .env files written
//     for `source .env` or direnv work without modification)
//   - leading UTF-8 BOM on the first line (Windows editors)
//   - inline `# comment` after a value when preceded by whitespace
//   - surrounding `"` or `'` quotes on the value (stripped one matched
//     pair); inside a quoted value, `#` is part of the value, not a
//     comment marker
func parseDotEnvLine(line string) (string, string, bool) {
	// Strip a UTF-8 BOM if present. bufio.Scanner doesn't filter it,
	// so the very first line of a Windows-edited .env would otherwise
	// produce a key like U+FEFF + "FOO" that os.Setenv silently accepts.
	line = strings.TrimPrefix(line, "\ufeff")
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	// Drop a leading `export ` (literal space — `export\tFOO=bar`
	// with a tab is intentionally rejected, matching the TS mirror in
	// canvas/next.config.ts. shells emit `export ` with a space; tabs
	// would only appear in hand-mangled files.) so lines like
	// `export FOO=bar` (the form direnv and many `.env` templates
	// emit) don't end up as a junk key with an embedded space.
	line = strings.TrimPrefix(line, "export ")
	line = strings.TrimLeft(line, " \t") // re-trim in case `export` itself had trailing space
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:eq])
	v := line[eq+1:]
	// Trim leading whitespace so a quoted value's opening quote is at
	// v[0]. The comment-detection loop below then treats the position
	// after the trim as "start of value" — `KEY=    # comment` has its
	// `#` at the new v[0] (preceded only by whitespace in the source)
	// and is correctly classified as an empty value followed by a
	// comment, not as a value of `# comment`.
	v = strings.TrimLeft(v, " \t")
	// Quoted value: strip one matched pair of surrounding quotes and
	// take the contents verbatim (no inline-comment splitting). Must
	// happen BEFORE comment detection so `KEY="value # not a comment"`
	// keeps the `#` as part of the value.
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') {
		quote := v[0]
		if end := strings.IndexByte(v[1:], quote); end >= 0 {
			return k, v[1 : 1+end], true
		}
		// Unterminated quote — fall through to bare-value handling
		// (treats the opening quote as a literal char in the value).
	}
	// Bare value: strip inline comment. A `#` is a comment marker iff
	// it's at the start of the (trimmed) value OR is preceded by
	// whitespace. `KEY=token#fragment` keeps the `#` as part of the
	// value because v[i-1] is alphanum.
	for i := 0; i < len(v); i++ {
		if v[i] != '#' {
			continue
		}
		if i == 0 || v[i-1] == ' ' || v[i-1] == '\t' {
			v = v[:i]
			break
		}
	}
	return k, strings.TrimSpace(v), true
}
