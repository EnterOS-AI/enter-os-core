package handlers

import (
	"context"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectDesiredSet programs the two queries desiredPluginSources runs:
// listDeclaredPlugins (workspace_declared_plugins) then listInstalledPlugins
// (workspace_plugins, name + source_raw).
func expectDesiredSet(mock sqlmock.Sqlmock, declared, installed []DeclaredPlugin) {
	dRows := sqlmock.NewRows([]string{"plugin_name", "source_raw"})
	for _, d := range declared {
		dRows.AddRow(d.PluginName, d.SourceRaw)
	}
	mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_declared_plugins`).
		WillReturnRows(dRows)

	iRows := sqlmock.NewRows([]string{"plugin_name", "source_raw"})
	for _, i := range installed {
		iRows.AddRow(i.PluginName, i.SourceRaw)
	}
	mock.ExpectQuery(`SELECT plugin_name, source_raw\s+FROM workspace_plugins`).
		WillReturnRows(iRows)
}

// RFC#2843 #42: the boot-install desired-set is declared ∪ installed. A plugin
// the user installed at runtime (in workspace_plugins, not declared) must
// survive a restart; declared-only used to wipe it.
func TestDesiredPluginSources_UnionPreservesUserInstalled(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	expectDesiredSet(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "gitea://molecule-ai/seo-agent#tag:v1"}},
		[]DeclaredPlugin{
			{PluginName: "seo-all", SourceRaw: "gitea://molecule-ai/seo-agent#tag:v1"}, // declared+installed
			{PluginName: "user-tool", SourceRaw: "gitea://molecule-ai/user-tool#main"}, // user-installed only
		},
	)

	got, err := desiredPluginSources(context.Background(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by plugin_name: seo-all, then user-tool.
	want := []string{"gitea://molecule-ai/seo-agent#tag:v1", "gitea://molecule-ai/user-tool#main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desired set = %v, want %v (user-installed plugin must be present)", got, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A declared plugin not yet installed (first boot, before reconcile) still
// seeds the set — installed-only would drop it.
func TestDesiredPluginSources_DeclaredOnlySeedsFirstBoot(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	expectDesiredSet(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "gitea://molecule-ai/seo-agent#tag:v1"}},
		nil, // nothing installed yet
	)

	got, err := desiredPluginSources(context.Background(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gitea://molecule-ai/seo-agent#tag:v1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desired set = %v, want %v", got, want)
	}
}

// On a name collision the INSTALLED source wins — it reflects what is actually
// running (e.g. a ref the user re-pinned via install_plugin).
func TestDesiredPluginSources_InstalledSourceWinsOnCollision(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	expectDesiredSet(mock,
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "gitea://molecule-ai/seo-agent#tag:v1"}},
		[]DeclaredPlugin{{PluginName: "seo-all", SourceRaw: "gitea://molecule-ai/seo-agent#tag:v2"}},
	)

	got, err := desiredPluginSources(context.Background(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gitea://molecule-ai/seo-agent#tag:v2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desired set = %v, want %v (installed source must win)", got, want)
	}
}

// Rows with an empty source_raw are skipped — there is nothing to fetch.
func TestDesiredPluginSources_SkipsEmptySource(t *testing.T) {
	mock, cleanup := withMockDB(t)
	defer cleanup()

	expectDesiredSet(mock,
		[]DeclaredPlugin{{PluginName: "ghost", SourceRaw: ""}},
		[]DeclaredPlugin{{PluginName: "real", SourceRaw: "gitea://molecule-ai/real#main"}},
	)

	got, err := desiredPluginSources(context.Background(), "ws-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gitea://molecule-ai/real#main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("desired set = %v, want %v (empty-source row must be skipped)", got, want)
	}
}
