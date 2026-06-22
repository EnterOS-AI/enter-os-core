package handlers

import (
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

// walkOrgWorkspaceNames tests — recursive collection of non-empty workspace names.

func TestWalkOrgWorkspaceNames_EmptySlice(t *testing.T) {
	var names []string
	walkOrgWorkspaceNames([]OrgWorkspace{}, &names)
	assert.Empty(t, names)
}

func TestWalkOrgWorkspaceNames_SingleNode(t *testing.T) {
	var names []string
	walkOrgWorkspaceNames([]OrgWorkspace{{Name: "my-workspace"}}, &names)
	assert.Equal(t, []string{"my-workspace"}, names)
}

func TestWalkOrgWorkspaceNames_SingleNodeEmptyName(t *testing.T) {
	var names []string
	walkOrgWorkspaceNames([]OrgWorkspace{{Name: ""}}, &names)
	assert.Empty(t, names)
}

func TestWalkOrgWorkspaceNames_NestedChildren(t *testing.T) {
	var names []string
	tree := []OrgWorkspace{
		{
			Name: "parent",
			Children: []OrgWorkspace{
				{Name: "child-a"},
				{Name: "child-b"},
			},
		},
	}
	walkOrgWorkspaceNames(tree, &names)
	assert.Equal(t, []string{"parent", "child-a", "child-b"}, names)
}

func TestWalkOrgWorkspaceNames_DeeplyNested(t *testing.T) {
	var names []string
	tree := []OrgWorkspace{
		{
			Name: "level0",
			Children: []OrgWorkspace{
				{
					Name: "level1",
					Children: []OrgWorkspace{
						{
							Name: "level2",
							Children: []OrgWorkspace{
								{Name: "level3"},
							},
						},
					},
				},
			},
		},
	}
	walkOrgWorkspaceNames(tree, &names)
	assert.Equal(t, []string{"level0", "level1", "level2", "level3"}, names)
}

func TestWalkOrgWorkspaceNames_SkipsEmptyNames(t *testing.T) {
	var names []string
	tree := []OrgWorkspace{
		{Name: "a"},
		{Name: ""},
		{Name: "b"},
	}
	walkOrgWorkspaceNames(tree, &names)
	assert.Equal(t, []string{"a", "b"}, names)
}

func TestWalkOrgWorkspaceNames_Siblings(t *testing.T) {
	var names []string
	tree := []OrgWorkspace{
		{Name: "team"},
		{Name: "alpha"},
		{Name: "beta"},
	}
	walkOrgWorkspaceNames(tree, &names)
	assert.Equal(t, []string{"team", "alpha", "beta"}, names)
}

func TestWalkOrgWorkspaceNames_MultipleRoots(t *testing.T) {
	var names []string
	tree := []OrgWorkspace{
		{Name: "root-a", Children: []OrgWorkspace{{Name: "child-a"}}},
		{Name: "root-b", Children: []OrgWorkspace{{Name: "child-b"}}},
	}
	walkOrgWorkspaceNames(tree, &names)
	assert.Equal(t, []string{"root-a", "child-a", "root-b", "child-b"}, names)
}

func TestWalkOrgWorkspaceNames_SpawningFalseStillWalks(t *testing.T) {
	// The comment in the source is explicit: spawning:false subtrees are
	// still walked. Empty names within those subtrees are still skipped.
	var names []string
	yes := true
	no := false
	tree := []OrgWorkspace{
		{
			Name: "parent",
			Children: []OrgWorkspace{
				{Name: "spawning-child", Spawning: &yes},
				{Name: "non-spawning-child", Spawning: &no},
				{Name: ""},
			},
		},
	}
	walkOrgWorkspaceNames(tree, &names)
	assert.Equal(t, []string{"parent", "spawning-child", "non-spawning-child"}, names)
}

// resolveProvisionConcurrency tests — env-var parsing with sensible fallback.

func TestResolveProvisionConcurrency_Default(t *testing.T) {
	os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	assert.Equal(t, defaultProvisionConcurrency, val)
}

func TestResolveProvisionConcurrency_ValidPositiveInt(t *testing.T) {
	os.Setenv("MOLECULE_PROVISION_CONCURRENCY", "5")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	assert.Equal(t, 5, val)
}

func TestResolveProvisionConcurrency_ZeroUnlimited(t *testing.T) {
	os.Setenv("MOLECULE_PROVISION_CONCURRENCY", "0")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	// Zero is mapped to 1<<20 (unlimited semantics with finite cap)
	assert.Equal(t, 1<<20, val)
}

func TestResolveProvisionConcurrency_NegativeFallsBack(t *testing.T) {
	os.Setenv("MOLECULE_PROVISION_CONCURRENCY", "-1")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	assert.Equal(t, defaultProvisionConcurrency, val)
}

func TestResolveProvisionConcurrency_NonIntegerFallsBack(t *testing.T) {
	os.Setenv("MOLECULE_PROVISION_CONCURRENCY", "not-a-number")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	assert.Equal(t, defaultProvisionConcurrency, val)
}

func TestResolveProvisionConcurrency_WhitespaceOnly(t *testing.T) {
	os.Setenv("MOLECULE_PROVISION_CONCURRENCY", "   ")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	assert.Equal(t, defaultProvisionConcurrency, val)
}

func TestResolveProvisionConcurrency_LargeValue(t *testing.T) {
	os.Setenv("MOLECULE_PROVISION_CONCURRENCY", "10000")
	defer os.Unsetenv("MOLECULE_PROVISION_CONCURRENCY")
	val := resolveProvisionConcurrency()
	assert.Equal(t, 10000, val)
}

// errString tests — nil-safe error-to-string wrapper.

func TestErrString_NilError(t *testing.T) {
	result := errString(nil)
	assert.Equal(t, "", result)
}

func TestErrString_WithError(t *testing.T) {
	err := errors.New("something went wrong")
	result := errString(err)
	assert.Equal(t, "something went wrong", result)
}

func TestErrString_EmptyError(t *testing.T) {
	err := errors.New("")
	result := errString(err)
	assert.Equal(t, "", result)
}
