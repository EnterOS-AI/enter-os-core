package handlers

// derive_provider_drift_test.go — behavior-based AST/text drift gate.
//
// Why this exists: PR #2535 introduced a Go port of derive-provider.sh
// (see deriveProviderFromModelSlug in workspace_provision.go) so the
// workspace-server can persist LLM_PROVIDER into workspace_secrets at
// provision time. That created two sources of truth:
//
//   1. molecule-ai-workspace-template-hermes/scripts/derive-provider.sh —
//      runs inside the container at boot, has the final say on which
//      provider hermes targets (writes ~/.hermes/config.yaml's
//      model.provider field). The shell script lives in a separate
//      OSS repo, so we vendor a snapshot at testdata/derive-provider.sh
//      to keep this gate hermetic.
//   2. workspace-server/internal/handlers/workspace_provision.go's
//      deriveProviderFromModelSlug — runs at provision time on the
//      platform side so LLM_PROVIDER lands in workspace_secrets and
//      survives Save+Restart.
//
// If a future PR adds a new provider prefix to one but not the other,
// the workspace-server's persisted LLM_PROVIDER silently disagrees
// with what the container's derive-provider.sh produces. The container
// wins (it writes the actual config.yaml), so the workspace-server's
// persisted value becomes stale and misleading without anything
// flipping red in CI.
//
// This gate pins the invariant that the *prefix set* the two functions
// know about is identical, modulo a small hardcoded acceptedDivergences
// map for the two intentional differences documented in
// deriveProviderFromModelSlug's doc comment (nousresearch/* and
// openai/* both fall back to "openrouter" at provision time because
// the runtime env that picks "nous" / "custom" isn't available yet).
//
// Pattern: the "behavior-based AST gate" from PR #2367 / memory
// feedback_behavior_based_ast_gates — pin invariants by what a
// function maps, not by what it's named. Walks the actual Go AST of
// deriveProviderFromModelSlug's switch statement so a rename or a
// duplicate function in another file can't sneak past the gate.
//
// Task: #242. Companion to the table-driven mapping test in
// workspace_provision_shared_test.go (TestDeriveProviderFromModelSlug)
// which pins the *values*; this test pins the *coverage* of the
// prefix set itself.
//
// Hermetic: reads two files (vendored shell script + Go source) from
// paths relative to the test package directory and parses them
// in-process. No network, no docker, no DB. The vendored shell script
// at testdata/derive-provider.sh is a snapshot of the upstream OSS
// template repo's script — refresh it via the cp command in that file's
// header when upstream changes.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// acceptedDivergences pins the prefixes where the Go port intentionally
// differs from derive-provider.sh. Each entry's value is the provider
// the Go function returns; the shell would (at runtime, with the right
// env keys present) return something else. Documented in
// deriveProviderFromModelSlug's doc comment in workspace_provision.go.
//
// If a NEW divergence appears, this test fails and the engineer must
// either (a) align the Go function with the shell, or (b) add the
// prefix here with a comment explaining why the divergence is
// intentional and safe at provision time.
var acceptedDivergences = map[string]string{
	// Shell: "nous" if HERMES_API_KEY/NOUS_API_KEY set, else "openrouter".
	// Go:    "openrouter" unconditionally — runtime keys aren't loaded at
	//        provision time. derive-provider.sh upgrades to "nous" at boot
	//        when the keys are present.
	"nousresearch": "openrouter",
	// Shell: "custom" if OPENAI_API_KEY set, "openrouter" if OPENROUTER_API_KEY
	//        set, else "openrouter" as a no-key fallback.
	// Go:    "openrouter" unconditionally — same reason as nousresearch/*.
	//        derive-provider.sh upgrades to "custom" at boot when
	//        OPENAI_API_KEY is present.
	"openai": "openrouter",
}

// TestDeriveProviderDrift_ShellAndGoStayInSync is the drift gate.
// It extracts the prefix→provider mapping from both sources and
// asserts:
//
//  1. Every prefix the shell knows about, the Go function also handles
//     (returning either the same provider OR the value pinned in
//     acceptedDivergences for that prefix).
//  2. Every prefix the Go function handles (extracted from its switch
//     statement via go/ast), the shell case statement also lists.
func TestDeriveProviderDrift_ShellAndGoStayInSync(t *testing.T) {
	t.Parallel()

	shellMap := loadShellPrefixMap(t)
	goMap := loadGoPrefixMap(t)

	if len(shellMap) == 0 {
		t.Fatalf("parsed zero prefixes from derive-provider.sh — regex likely broke; rebuild parser before trusting this gate")
	}
	if len(goMap) == 0 {
		t.Fatalf("parsed zero prefixes from deriveProviderFromModelSlug — AST walk likely broke; rebuild parser before trusting this gate")
	}

	// Direction 1: every shell prefix must be in the Go map (with the
	// same provider value, or with the documented divergence).
	for prefix, shellProvider := range shellMap {
		goProvider, ok := goMap[prefix]
		if !ok {
			t.Errorf(
				"DRIFT: derive-provider.sh has prefix %q -> %q but deriveProviderFromModelSlug doesn't handle it.\n"+
					"Fix: either add a case for %q to deriveProviderFromModelSlug in "+
					"workspace-server/internal/handlers/workspace_provision.go (returning %q to match the shell), "+
					"OR if this prefix is intentionally provision-time-divergent, add it to acceptedDivergences{} "+
					"in this test with a comment explaining why.",
				prefix, shellProvider, prefix, shellProvider,
			)
			continue
		}
		if goProvider == shellProvider {
			continue
		}
		// Mismatch — only acceptable if it's on the explicit divergence list
		// AND the Go side returns exactly the documented value.
		expected, divergenceAllowed := acceptedDivergences[prefix]
		if !divergenceAllowed {
			t.Errorf(
				"DRIFT: prefix %q maps to %q in derive-provider.sh but %q in deriveProviderFromModelSlug.\n"+
					"Fix: align the Go function with the shell (preferred — they should agree), "+
					"OR if the divergence is intentional and safe at provision time, "+
					"add %q: %q to acceptedDivergences{} in this test with a comment explaining why.",
				prefix, shellProvider, goProvider, prefix, goProvider,
			)
			continue
		}
		if goProvider != expected {
			t.Errorf(
				"DRIFT: prefix %q is on the acceptedDivergences list with expected Go value %q but "+
					"deriveProviderFromModelSlug now returns %q.\n"+
					"Fix: update acceptedDivergences[%q] in this test to %q (and update its comment), "+
					"OR revert the Go function to return %q.",
				prefix, expected, goProvider, prefix, goProvider, expected,
			)
		}
	}

	// Direction 2: every Go prefix must be in the shell map. Drift in
	// this direction is rarer (someone added a Go case without touching
	// the shell) but produces the same broken state — provision-time
	// LLM_PROVIDER disagrees with what the container actually uses.
	for prefix, goProvider := range goMap {
		if _, ok := shellMap[prefix]; ok {
			continue
		}
		t.Errorf(
			"DRIFT: deriveProviderFromModelSlug handles prefix %q -> %q but derive-provider.sh doesn't list it.\n"+
				"Fix: add a `%s/*) PROVIDER=%q ;;` case to "+
				"workspace-configs-templates/hermes/scripts/derive-provider.sh — the Go provision-time hint "+
				"is meaningless if the container's runtime script doesn't recognize the same prefix.",
			prefix, goProvider, prefix, goProvider,
		)
	}

	// Belt-and-braces: every entry in acceptedDivergences must actually
	// appear in BOTH maps. A stale divergence entry (prefix removed from
	// either source) silently weakens the gate.
	for prefix := range acceptedDivergences {
		if _, ok := shellMap[prefix]; !ok {
			t.Errorf(
				"acceptedDivergences contains prefix %q but derive-provider.sh no longer lists it. "+
					"Remove the entry from acceptedDivergences{} in this test.",
				prefix,
			)
		}
		if _, ok := goMap[prefix]; !ok {
			t.Errorf(
				"acceptedDivergences contains prefix %q but deriveProviderFromModelSlug no longer lists it. "+
					"Remove the entry from acceptedDivergences{} in this test.",
				prefix,
			)
		}
	}
}

// vendoredShellPath is the testdata snapshot of upstream
// derive-provider.sh. The path is relative to the test package
// directory (which is what `go test` sets as cwd). See the file's
// header for the refresh procedure when upstream changes.
const vendoredShellPath = "testdata/derive-provider.sh"

// goSourcePath is the file containing deriveProviderFromModelSlug.
// Relative to the test package directory.
const goSourcePath = "workspace_provision.go"

// loadShellPrefixMap parses derive-provider.sh and returns a
// map[prefix]provider for every case clause. Aliases inside a single
// `pat1/*|pat2/*)` clause expand to one map entry per alias, both
// pointing at the same provider.
//
// Stops at the first `*)` (the catch-all) and ignores it — the
// catch-all maps to PROVIDER="auto" which has no Go counterpart by
// design (deriveProviderFromModelSlug returns "" for unknowns and
// lets the shell's *=auto branch decide at runtime).
//
// Ambiguity: case clauses whose body branches on env vars (openai/*,
// nousresearch/*) are still extracted as the FIRST PROVIDER= literal
// inside the body. The shell's full conditional logic is documented
// via the acceptedDivergences map in this file rather than re-encoded
// in the parser, because re-encoding sh `if` semantics in regex is a
// fool's errand — the divergences are stable and small enough to
// hardcode.
func loadShellPrefixMap(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(vendoredShellPath)
	if err != nil {
		t.Fatalf("read %s: %v (refresh from upstream — see file header)", vendoredShellPath, err)
	}

	// Locate the case statement body so we don't accidentally match
	// PROVIDER= assignments above the case (the HERMES_INFERENCE_PROVIDER
	// override + the empty-model fallback both write PROVIDER= before
	// the case). Upstream renamed the case variable to ${_HERMES_MODEL}
	// in v0.12.0 (the resolved value of HERMES_INFERENCE_MODEL with a
	// HERMES_DEFAULT_MODEL legacy fallback); accept either spelling so
	// this test survives a future rename.
	caseStart := regexp.MustCompile(`(?m)^case\s+"\$\{(_?HERMES(?:_DEFAULT|_INFERENCE)?_MODEL)\}"\s+in\s*$`)
	startLoc := caseStart.FindIndex(raw)
	if startLoc == nil {
		t.Fatalf("could not locate `case \"${...HERMES...MODEL}\" in` in %s — shell file shape changed; rebuild parser", vendoredShellPath)
	}
	caseEnd := regexp.MustCompile(`(?m)^esac\s*$`)
	endLoc := caseEnd.FindIndex(raw[startLoc[1]:])
	if endLoc == nil {
		t.Fatalf("could not locate `esac` after the case statement in %s — shell file shape changed", vendoredShellPath)
	}
	body := string(raw[startLoc[1] : startLoc[1]+endLoc[0]])

	out := map[string]string{}

	// Pattern A: single-line clauses like
	//   minimax-cn/*)            PROVIDER="minimax-cn" ;;
	//   alibaba/*|dashscope/*|qwen/*) PROVIDER="alibaba" ;;
	// Capture group 1 is the patterns (e.g. `minimax-cn/*` or
	// `alibaba/*|dashscope/*|qwen/*`); group 2 is the provider literal.
	singleLine := regexp.MustCompile(`(?m)^\s*([a-zA-Z0-9_./*|\-]+)\)\s*PROVIDER="([^"]+)"\s*;;`)

	// Pattern B: multi-line clauses like
	//   openai/*)
	//     if [ -n "${OPENAI_API_KEY:-}" ]; then
	//       PROVIDER="custom"
	//     ...
	// We capture the patterns and the FIRST PROVIDER= that follows
	// (before the next `;;`). The acceptedDivergences map handles the
	// fact that the runtime branching can pick a different value.
	multiLine := regexp.MustCompile(`(?ms)^\s*([a-zA-Z0-9_./*|\-]+)\)\s*\n(.*?);;`)

	addEntry := func(patterns, provider string) {
		// Skip the `*)` catch-all — it has no Go counterpart by design.
		if strings.TrimSpace(patterns) == "*" {
			return
		}
		for _, alt := range strings.Split(patterns, "|") {
			alt = strings.TrimSpace(alt)
			// Each alternative is `<prefix>/*` — strip the trailing `/*`.
			alt = strings.TrimSuffix(alt, "/*")
			if alt == "" {
				continue
			}
			// First write wins — a single-line match outranks a multi-line
			// fallback for the same patterns block (defensive; the regexes
			// shouldn't overlap on the same line in practice).
			if _, exists := out[alt]; !exists {
				out[alt] = provider
			}
		}
	}

	// Run single-line first so it claims its lines before the multi-line
	// pass sees them.
	consumed := map[int]bool{}
	for _, m := range singleLine.FindAllStringSubmatchIndex(body, -1) {
		addEntry(body[m[2]:m[3]], body[m[4]:m[5]])
		// Mark every line touched so multi-line pass can skip it.
		for i := m[0]; i < m[1]; i++ {
			consumed[i] = true
		}
	}

	for _, m := range multiLine.FindAllStringSubmatchIndex(body, -1) {
		// Skip if the start of this match overlaps a single-line clause.
		if consumed[m[0]] {
			continue
		}
		patterns := body[m[2]:m[3]]
		clauseBody := body[m[4]:m[5]]
		// Extract the FIRST PROVIDER="..." from the clause body.
		firstProvider := regexp.MustCompile(`PROVIDER="([^"]+)"`).FindStringSubmatch(clauseBody)
		if firstProvider == nil {
			t.Errorf("multi-line case clause for %q has no PROVIDER= literal — shell file shape changed; rebuild parser", patterns)
			continue
		}
		addEntry(patterns, firstProvider[1])
	}

	return out
}

// loadGoPrefixMap parses workspace_provision.go and walks the AST to
// extract the prefix→provider mapping from deriveProviderFromModelSlug's
// switch statement.
//
// Each case clause's string-literal labels become map keys, all
// pointing at the provider returned by that case body's `return "..."`
// statement. A clause like `case "alibaba", "dashscope", "qwen":
// return "alibaba"` produces three map entries.
//
// Skips the default clause (returns ""). Skips any case clause whose
// body's first statement isn't a single `return STRING_LITERAL` — those
// would need their own divergence handling and don't currently exist
// in the function.
func loadGoPrefixMap(t *testing.T) map[string]string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, goSourcePath, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", goSourcePath, err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if f.Name.Name == "deriveProviderFromModelSlug" {
			fn = f
			break
		}
	}
	if fn == nil {
		t.Fatalf("could not find deriveProviderFromModelSlug in %s — function renamed/removed; this gate's invariant has been violated", goSourcePath)
	}

	// Walk the function body for the SwitchStmt.
	var sw *ast.SwitchStmt
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.SwitchStmt); ok {
			sw = s
			return false
		}
		return true
	})
	if sw == nil {
		t.Fatalf("no switch statement found in deriveProviderFromModelSlug — function shape changed; rebuild parser")
	}

	out := map[string]string{}
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		// Default clause has no list — skip.
		if len(clause.List) == 0 {
			continue
		}
		// Find the first return statement in the clause body.
		var ret *ast.ReturnStmt
		for _, bodyStmt := range clause.Body {
			if r, ok := bodyStmt.(*ast.ReturnStmt); ok {
				ret = r
				break
			}
		}
		if ret == nil || len(ret.Results) != 1 {
			t.Errorf("case clause at %s has no single-value return — function shape changed; gate may be incomplete",
				fset.Position(clause.Pos()))
			continue
		}
		lit, ok := ret.Results[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("case clause at %s returns a non-literal — gate cannot extract provider value",
				fset.Position(clause.Pos()))
			continue
		}
		provider, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("case clause at %s has unparseable string literal %q: %v",
				fset.Position(clause.Pos()), lit.Value, err)
			continue
		}

		for _, expr := range clause.List {
			lbl, ok := expr.(*ast.BasicLit)
			if !ok || lbl.Kind != token.STRING {
				t.Errorf("case clause at %s has a non-string-literal label — gate cannot extract prefix",
					fset.Position(clause.Pos()))
				continue
			}
			prefix, err := strconv.Unquote(lbl.Value)
			if err != nil {
				t.Errorf("case clause at %s has unparseable label literal %q: %v",
					fset.Position(clause.Pos()), lbl.Value, err)
				continue
			}
			out[prefix] = provider
		}
	}
	return out
}

// TestDeriveProviderDrift_ShellParserIsSane is a guard test: the shell
// parser is regex-based, so we sanity-check that it actually finds the
// well-known prefixes documented in derive-provider.sh's header
// comment. If this test passes but the main drift test reports
// missing prefixes, the bug is almost certainly in the regex (not in
// the production code).
func TestDeriveProviderDrift_ShellParserIsSane(t *testing.T) {
	t.Parallel()
	shellMap := loadShellPrefixMap(t)

	// Anchor prefixes — these have lived in derive-provider.sh since it
	// was first introduced. If the parser can't find them, it's broken.
	mustHave := map[string]string{
		"anthropic":    "anthropic",
		"minimax":      "minimax",
		"minimax-cn":   "minimax-cn",
		"openrouter":   "openrouter",
		"custom":       "custom",
		"alibaba":      "alibaba", // in an alias group with dashscope/qwen
		"dashscope":    "alibaba", // ditto
		"qwen":         "alibaba", // ditto
		"openai":       "custom",  // multi-line; first PROVIDER= is "custom"
		"nousresearch": "nous",    // multi-line; first PROVIDER= is "nous"
	}

	missing := []string{}
	wrong := []string{}
	for prefix, want := range mustHave {
		got, ok := shellMap[prefix]
		if !ok {
			missing = append(missing, prefix)
			continue
		}
		if got != want {
			wrong = append(wrong, prefix+" got="+got+" want="+want)
		}
	}
	sort.Strings(missing)
	sort.Strings(wrong)
	if len(missing) > 0 {
		t.Errorf("shell parser failed to extract anchor prefixes: %v", missing)
	}
	if len(wrong) > 0 {
		t.Errorf("shell parser extracted wrong values for anchor prefixes: %v", wrong)
	}
}
