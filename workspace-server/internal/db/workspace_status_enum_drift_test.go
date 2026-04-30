package db_test

// Static drift gate: every workspaces.status literal written in the Go
// tree must exist in the workspace_status enum defined by the migrations.
//
// Why this exists: the `workspace_status` enum (migrations 043 + 046)
// shipped without 'awaiting_agent' even though application code wrote
// that value, and every UPDATE silently failed in production for five
// days before the gap surfaced (see 046_workspace_status_awaiting_agent.up.sql).
// The unit tests passed because sqlmock matches SQL by regex, not against
// a live enum constraint.
//
// Approach: extract every Go string literal whose body matches
// (?i)workspaces[^a-z_].*status (so "UPDATE workspaces SET status",
// "FROM workspaces WHERE ... status", "INSERT INTO workspaces ... status",
// CTEs that reference workspaces, etc.). For each such SQL fragment,
// pull the single-quoted status values out of `status =`, `status IN`,
// `THEN`, and `ELSE`. Every value must be in the union of CREATE TYPE +
// ALTER TYPE ADD VALUE across all migrations.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestWorkspaceStatusEnum_NoLiteralDrift(t *testing.T) {
	t.Parallel()

	repoRoot := findRepoRoot(t)
	migrationsDir := filepath.Join(repoRoot, "workspace-server", "migrations")
	internalDir := filepath.Join(repoRoot, "workspace-server", "internal")

	enum := loadWorkspaceStatusEnum(t, migrationsDir)
	if len(enum) == 0 {
		t.Fatalf("could not parse workspace_status enum from %s — gate is non-functional", migrationsDir)
	}

	literals := collectWorkspacesStatusLiterals(t, internalDir)
	if len(literals) == 0 {
		t.Fatalf("found zero workspaces.status literals under %s — gate is non-functional", internalDir)
	}

	var rogue []string
	for lit := range literals {
		if _, ok := enum[lit]; ok {
			continue
		}
		rogue = append(rogue, lit)
	}
	if len(rogue) > 0 {
		sort.Strings(rogue)
		t.Errorf(
			"workspaces.status literal(s) %v are written by Go code but not in the workspace_status enum.\n"+
				"Add a migration `ALTER TYPE workspace_status ADD VALUE 'X';` (see 046 for shape).\n"+
				"Enum currently is: %v",
			rogue, sortedKeys(enum),
		)
	}
}

// loadWorkspaceStatusEnum scans every *.up.sql file for either:
//
//	CREATE TYPE workspace_status AS ENUM ('a', 'b', ...)
//	ALTER TYPE workspace_status ADD VALUE [IF NOT EXISTS] 'X' [BEFORE|AFTER 'Y']
//
// and returns the union of every value the enum will hold after all
// migrations apply.
func loadWorkspaceStatusEnum(t *testing.T, migrationsDir string) map[string]struct{} {
	t.Helper()

	out := make(map[string]struct{})

	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files)

	createRE := regexp.MustCompile(`(?is)CREATE\s+TYPE\s+workspace_status\s+AS\s+ENUM\s*\(([^)]+)\)`)
	addValueRE := regexp.MustCompile(`(?i)ALTER\s+TYPE\s+workspace_status\s+ADD\s+VALUE(?:\s+IF\s+NOT\s+EXISTS)?\s+'([^']+)'`)
	literalRE := regexp.MustCompile(`'([^']+)'`)

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range createRE.FindAllStringSubmatch(string(body), -1) {
			for _, lit := range literalRE.FindAllStringSubmatch(m[1], -1) {
				out[lit[1]] = struct{}{}
			}
		}
		for _, m := range addValueRE.FindAllStringSubmatch(string(body), -1) {
			out[m[1]] = struct{}{}
		}
	}
	return out
}

// collectWorkspacesStatusLiterals walks every non-test .go file under
// root, finds Go string literals that contain `UPDATE workspaces` or
// `INSERT INTO workspaces`, and extracts the status literals appearing
// inside the matching SQL statement.
//
// Why this scope: any UPDATE/INSERT against `workspaces` is the moment
// a status literal hits the column constrained by the enum. Read-side
// SQL (SELECT ... WHERE status = 'X') cannot fail on enum drift, so it's
// out of scope. JOINs to `workspaces` from other tables (e.g. approvals
// joining workspaces for display) write to a different table's status —
// also out of scope. Anchoring on the leading `UPDATE workspaces` /
// `INSERT INTO workspaces` keyword unambiguously identifies the writes
// we care about.
func collectWorkspacesStatusLiterals(t *testing.T, root string) map[string]struct{} {
	t.Helper()

	// Match raw-string and double-quoted Go string literals. Backtick
	// strings can span multiple lines. Both forms are extracted via the
	// same DOTALL regex over the whole file body.
	rawRE := regexp.MustCompile("(?s)`([^`]*?)`")
	dquoteRE := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

	// A SQL string is in scope if it begins (after optional leading
	// whitespace) with UPDATE workspaces or INSERT INTO workspaces.
	// `(?i)` is case-insensitive; `\s*` allows the format-friendly
	// leading newline and indent that the codebase uses.
	updateWorkspacesRE := regexp.MustCompile(`(?is)^\s*UPDATE\s+workspaces\b`)
	insertWorkspacesRE := regexp.MustCompile(`(?is)^\s*INSERT\s+INTO\s+workspaces\b`)

	// Inside a scoped SQL fragment, status literals appear in:
	//   status = 'X'           — assignment in SET (or filter in WHERE)
	//   status IN ('X', ...)   — filter
	//   status NOT IN ('X')    — filter
	//   THEN 'X'               — CASE arm
	//   ELSE 'X'               — CASE default
	statusEqRE := regexp.MustCompile(`(?i)status\s*(?:=|!=|<>)\s*'([a-z_]+)'`)
	statusInRE := regexp.MustCompile(`(?i)status\s+(?:NOT\s+)?IN\s*\(([^)]*)\)`)
	thenRE := regexp.MustCompile(`(?i)THEN\s+'([a-z_]+)'`)
	elseRE := regexp.MustCompile(`(?i)ELSE\s+'([a-z_]+)'`)
	inListLiteralRE := regexp.MustCompile(`'([a-z_]+)'`)

	out := make(map[string]struct{})

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)

		harvest := func(fragment string) {
			if !updateWorkspacesRE.MatchString(fragment) && !insertWorkspacesRE.MatchString(fragment) {
				return
			}
			for _, m := range statusEqRE.FindAllStringSubmatch(fragment, -1) {
				out[m[1]] = struct{}{}
			}
			for _, m := range statusInRE.FindAllStringSubmatch(fragment, -1) {
				for _, lit := range inListLiteralRE.FindAllStringSubmatch(m[1], -1) {
					out[lit[1]] = struct{}{}
				}
			}
			for _, m := range thenRE.FindAllStringSubmatch(fragment, -1) {
				out[m[1]] = struct{}{}
			}
			for _, m := range elseRE.FindAllStringSubmatch(fragment, -1) {
				out[m[1]] = struct{}{}
			}
		}

		for _, m := range rawRE.FindAllStringSubmatch(text, -1) {
			harvest(m[1])
		}
		for _, m := range dquoteRE.FindAllStringSubmatch(text, -1) {
			harvest(m[1])
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	return out
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "workspace-server", "migrations")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not locate repo root with workspace-server/migrations from %s", dir)
	return ""
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
