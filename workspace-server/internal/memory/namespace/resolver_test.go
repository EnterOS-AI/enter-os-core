package namespace

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/memory/contract"
)

// chainQueryMatcher matches the recursive-CTE query loosely (substring
// match on the WITH RECURSIVE keyword + chain table). sqlmock's
// QueryMatcher is regex by default; using it directly forces brittle
// escaping so we use ExpectQuery with a stable substring instead.
const chainQuerySnippet = "WITH RECURSIVE chain"

// setupMockDB creates an *sql.DB backed by sqlmock and returns both.
// Helper makes per-test mock setup terser.
func setupMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// We use QueryMatcherEqual but with regex-based ExpectQuery elsewhere
	// for flexibility. Actually swap to regex for the recursive query:
	db, mock, err = sqlmock.New() // default = regex
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

// --- walkChain ---

func TestWalkChain_RootOnly(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	// Root workspace: parent_id is NULL, depth 0, single row.
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-root", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("ws-root", nil, 0))

	chain, err := r.walkChain(context.Background(), "ws-root")
	if err != nil {
		t.Fatalf("walkChain: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("len = %d, want 1", len(chain))
	}
	if chain[0].id != "ws-root" || chain[0].parentID != nil || chain[0].depth != 0 {
		t.Errorf("root row mismatch: %+v", chain[0])
	}
	mustExpectations(t, mock)
}

func TestWalkChain_ChildToParent(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-child", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("ws-child", "ws-root", 0).
			AddRow("ws-root", nil, 1))

	chain, err := r.walkChain(context.Background(), "ws-child")
	if err != nil {
		t.Fatalf("walkChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("len = %d, want 2", len(chain))
	}
	if chain[0].id != "ws-child" || *chain[0].parentID != "ws-root" {
		t.Errorf("self row: %+v", chain[0])
	}
	if chain[1].id != "ws-root" || chain[1].parentID != nil {
		t.Errorf("root row: %+v", chain[1])
	}
	mustExpectations(t, mock)
}

func TestWalkChain_DeepTreeRespectsMaxDepth(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	// Simulate a 51-deep chain: should be capped at maxChainDepth.
	rows := sqlmock.NewRows([]string{"id", "parent_id", "depth"})
	for i := 0; i <= maxChainDepth; i++ {
		var parent interface{}
		if i < maxChainDepth {
			parent = "ws-" + itoa(i+1)
		} else {
			parent = nil // would be the cap point
		}
		rows.AddRow("ws-"+itoa(i), parent, i)
	}
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-0", maxChainDepth).
		WillReturnRows(rows)

	chain, err := r.walkChain(context.Background(), "ws-0")
	if err != nil {
		t.Fatalf("walkChain: %v", err)
	}
	// Returns at most maxChainDepth+1 rows (the recursive CTE bound is
	// `c.depth < maxChainDepth`, allowing depth values 0..maxChainDepth
	// inclusive — so 51 rows for maxChainDepth=50). Exact count
	// validates we didn't accidentally double-cap.
	if len(chain) != maxChainDepth+1 {
		t.Errorf("chain len = %d, want %d", len(chain), maxChainDepth+1)
	}
	mustExpectations(t, mock)
}

func TestWalkChain_WorkspaceNotFound(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-missing", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}))

	_, err := r.walkChain(context.Background(), "ws-missing")
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound", err)
	}
	mustExpectations(t, mock)
}

func TestWalkChain_QueryError(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-x", maxChainDepth).
		WillReturnError(errors.New("conn dead"))

	_, err := r.walkChain(context.Background(), "ws-x")
	if err == nil || !strings.Contains(err.Error(), "conn dead") {
		t.Errorf("err = %v, want wrapped 'conn dead'", err)
	}
}

func TestWalkChain_ScanError(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	// Wrong row shape forces Scan to fail.
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-x", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id"}). // missing parent_id, depth
								AddRow("ws-x"))

	_, err := r.walkChain(context.Background(), "ws-x")
	if err == nil {
		t.Error("expected scan error, got nil")
	}
}

func TestWalkChain_RowsErr(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-x", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("ws-x", nil, 0).
			RowError(0, errors.New("mid-iteration")))

	_, err := r.walkChain(context.Background(), "ws-x")
	if err == nil || !strings.Contains(err.Error(), "mid-iteration") {
		t.Errorf("err = %v, want wrapped 'mid-iteration'", err)
	}
}

// --- derive ---

func TestDerive(t *testing.T) {
	cases := []struct {
		name              string
		chain             []chainNode
		wantWS, wantTeam, wantOrg string
	}{
		{
			name:     "root-only (degenerate)",
			chain:    []chainNode{{id: "root-1"}},
			wantWS:   "root-1",
			wantTeam: "root-1",
			wantOrg:  "root-1",
		},
		{
			name: "child of root",
			chain: []chainNode{
				{id: "child-1", parentID: ptr("root-1")},
				{id: "root-1"},
			},
			wantWS:   "child-1",
			wantTeam: "root-1",
			wantOrg:  "root-1",
		},
		{
			name: "grandchild (future-proof)",
			chain: []chainNode{
				{id: "gc-1", parentID: ptr("child-1")},
				{id: "child-1", parentID: ptr("root-1")},
				{id: "root-1"},
			},
			wantWS:   "gc-1",
			wantTeam: "child-1",
			wantOrg:  "root-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws, team, org := derive(tc.chain)
			if ws != tc.wantWS || team != tc.wantTeam || org != tc.wantOrg {
				t.Errorf("derive = (%s, %s, %s), want (%s, %s, %s)",
					ws, team, org, tc.wantWS, tc.wantTeam, tc.wantOrg)
			}
		})
	}
}

// --- ReadableNamespaces ---

func TestReadableNamespaces_Root(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("root-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("root-1", nil, 0))

	got, err := r.ReadableNamespaces(context.Background(), "root-1")
	if err != nil {
		t.Fatalf("ReadableNamespaces: %v", err)
	}
	wantNames := []string{"workspace:root-1", "team:root-1", "org:root-1"}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, ns := range got {
		if ns.Name != wantNames[i] {
			t.Errorf("[%d] name = %q, want %q", i, ns.Name, wantNames[i])
		}
		if !ns.Writable {
			t.Errorf("[%d] %q must be writable for root", i, ns.Name)
		}
	}
	if got[0].Kind != contract.NamespaceKindWorkspace {
		t.Errorf("[0] kind = %q, want workspace", got[0].Kind)
	}
	if got[1].Kind != contract.NamespaceKindTeam {
		t.Errorf("[1] kind = %q, want team", got[1].Kind)
	}
	if got[2].Kind != contract.NamespaceKindOrg {
		t.Errorf("[2] kind = %q, want org", got[2].Kind)
	}
}

func TestReadableNamespaces_Child(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("child-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("child-1", "root-1", 0).
			AddRow("root-1", nil, 1))

	got, err := r.ReadableNamespaces(context.Background(), "child-1")
	if err != nil {
		t.Fatalf("ReadableNamespaces: %v", err)
	}
	wantNames := []string{"workspace:child-1", "team:root-1", "org:root-1"}
	for i, ns := range got {
		if ns.Name != wantNames[i] {
			t.Errorf("[%d] name = %q, want %q", i, ns.Name, wantNames[i])
		}
	}
	// Child is NOT writable to org (preserves today's GLOBAL root-only rule).
	if !got[0].Writable || !got[1].Writable {
		t.Errorf("workspace + team must be writable for child")
	}
	if got[2].Writable {
		t.Errorf("child must NOT be able to write to org:root-1; was %v", got[2])
	}
}

func TestReadableNamespaces_NotFound(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ghost", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}))

	_, err := r.ReadableNamespaces(context.Background(), "ghost")
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound", err)
	}
}

// --- WritableNamespaces ---

func TestWritableNamespaces_RootSeesAll(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("root-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("root-1", nil, 0))

	got, err := r.WritableNamespaces(context.Background(), "root-1")
	if err != nil {
		t.Fatalf("WritableNamespaces: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("root must have 3 writable, got %d", len(got))
	}
}

func TestWritableNamespaces_ChildExcludesOrg(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("child-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("child-1", "root-1", 0).
			AddRow("root-1", nil, 1))

	got, err := r.WritableNamespaces(context.Background(), "child-1")
	if err != nil {
		t.Fatalf("WritableNamespaces: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("child must have 2 writable (workspace + team), got %d (%v)", len(got), got)
	}
	for _, ns := range got {
		if ns.Kind == contract.NamespaceKindOrg {
			t.Errorf("child must not have org in writable: %v", ns)
		}
	}
}

func TestWritableNamespaces_NotFound(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)

	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ghost", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}))

	_, err := r.WritableNamespaces(context.Background(), "ghost")
	if !errors.Is(err, ErrWorkspaceNotFound) {
		t.Errorf("err = %v, want ErrWorkspaceNotFound", err)
	}
}

// --- CanWrite ---

func TestCanWrite(t *testing.T) {
	cases := []struct {
		name      string
		isRoot    bool
		namespace string
		want      bool
	}{
		{"root writes own workspace", true, "workspace:root-1", true},
		{"root writes own team", true, "team:root-1", true},
		{"root writes own org", true, "org:root-1", true},
		{"root cannot write foreign workspace", true, "workspace:other", false},
		{"child writes own workspace", false, "workspace:child-1", true},
		{"child writes parent team", false, "team:root-1", true},
		{"child cannot write org", false, "org:root-1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := setupMockDB(t)
			r := New(db)
			rows := sqlmock.NewRows([]string{"id", "parent_id", "depth"})
			if tc.isRoot {
				rows.AddRow("root-1", nil, 0)
				mock.ExpectQuery(chainQuerySnippet).WithArgs("root-1", maxChainDepth).WillReturnRows(rows)
				ok, err := r.CanWrite(context.Background(), "root-1", tc.namespace)
				if err != nil {
					t.Fatalf("CanWrite: %v", err)
				}
				if ok != tc.want {
					t.Errorf("CanWrite(%q) = %v, want %v", tc.namespace, ok, tc.want)
				}
			} else {
				rows.AddRow("child-1", "root-1", 0).AddRow("root-1", nil, 1)
				mock.ExpectQuery(chainQuerySnippet).WithArgs("child-1", maxChainDepth).WillReturnRows(rows)
				ok, err := r.CanWrite(context.Background(), "child-1", tc.namespace)
				if err != nil {
					t.Fatalf("CanWrite: %v", err)
				}
				if ok != tc.want {
					t.Errorf("CanWrite(%q) = %v, want %v", tc.namespace, ok, tc.want)
				}
			}
		})
	}
}

func TestCanWrite_PropagatesError(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-x", maxChainDepth).
		WillReturnError(errors.New("dead db"))
	_, err := r.CanWrite(context.Background(), "ws-x", "workspace:ws-x")
	if err == nil || !strings.Contains(err.Error(), "dead db") {
		t.Errorf("err = %v, want wrapped 'dead db'", err)
	}
}

// --- IntersectReadable ---

func TestIntersectReadable_DefaultAll(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("child-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("child-1", "root-1", 0).
			AddRow("root-1", nil, 1))

	// Empty requested → return everything readable.
	got, err := r.IntersectReadable(context.Background(), "child-1", nil)
	if err != nil {
		t.Fatalf("IntersectReadable: %v", err)
	}
	want := []string{"workspace:child-1", "team:root-1", "org:root-1"}
	if !slicesEq(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestIntersectReadable_Filters(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("child-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("child-1", "root-1", 0).
			AddRow("root-1", nil, 1))

	// Requested: one allowed, one disallowed (foreign workspace), one allowed
	requested := []string{"workspace:child-1", "workspace:foreign", "team:root-1"}
	got, err := r.IntersectReadable(context.Background(), "child-1", requested)
	if err != nil {
		t.Fatalf("IntersectReadable: %v", err)
	}
	want := []string{"workspace:child-1", "team:root-1"}
	if !slicesEq(got, want) {
		t.Errorf("got %v, want %v (foreign should be filtered)", got, want)
	}
}

func TestIntersectReadable_AllFiltered(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-1", maxChainDepth).
		WillReturnRows(sqlmock.NewRows([]string{"id", "parent_id", "depth"}).
			AddRow("ws-1", nil, 0))

	// Request only namespaces the caller cannot read.
	got, err := r.IntersectReadable(context.Background(), "ws-1", []string{"workspace:other", "team:other"})
	if err != nil {
		t.Fatalf("IntersectReadable: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want []", got)
	}
}

func TestIntersectReadable_PropagatesError(t *testing.T) {
	db, mock := setupMockDB(t)
	r := New(db)
	mock.ExpectQuery(chainQuerySnippet).
		WithArgs("ws-x", maxChainDepth).
		WillReturnError(errors.New("dead db"))
	_, err := r.IntersectReadable(context.Background(), "ws-x", []string{"workspace:foo"})
	if err == nil || !strings.Contains(err.Error(), "dead db") {
		t.Errorf("err = %v, want wrapped 'dead db'", err)
	}
}

// --- helpers ---

func mustExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations not met: %v", err)
	}
}

func ptr(s string) *string { return &s }

func slicesEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// itoa is a small inlined int→string to avoid pulling in strconv just
// for the deep-tree test fixture.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
