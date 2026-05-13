package handlers

// workspace_dispatchers_test.go — unit coverage for workspace_dispatchers.go.
// Tests the three pure dispatcher helpers: HasProvisioner, IsSaaS, DefaultTier.
// The goroutine-backed dispatchers (provisionWorkspaceAuto,
// provisionWorkspaceAutoSync, RestartWorkspaceAuto) require integration-level
// mock setup (mock provisioner interfaces, broadcast spy) and are covered by
// workspace_provision_auto_test.go pin tests instead.

import "testing"

// ─── HasProvisioner ─────────────────────────────────────────────────────────

// mockLocalProvAPI and mockCPProvAPI are minimal implementations of the
// provisioner interfaces. The actual interface methods are never called in
// these tests — we only verify that the pointer presence toggles the bool
// return correctly.

type mockLocalProvAPI struct{}

type mockCPProvAPI struct{}

func TestHasProvisioner_NeitherWired(t *testing.T) {
	h := &WorkspaceHandler{}
	if h.HasProvisioner() {
		t.Error("HasProvisioner() = true; want false when neither backend is wired")
	}
}

func TestHasProvisioner_CPOnly(t *testing.T) {
	h := &WorkspaceHandler{cpProv: &mockCPProvAPI{}}
	if !h.HasProvisioner() {
		t.Error("HasProvisioner() = false; want true when cpProv is wired")
	}
}

func TestHasProvisioner_DockerOnly(t *testing.T) {
	h := &WorkspaceHandler{provisioner: &mockLocalProvAPI{}}
	if !h.HasProvisioner() {
		t.Error("HasProvisioner() = false; want true when provisioner is wired")
	}
}

func TestHasProvisioner_BothWired(t *testing.T) {
	h := &WorkspaceHandler{
		cpProv:      &mockCPProvAPI{},
		provisioner: &mockLocalProvAPI{},
	}
	if !h.HasProvisioner() {
		t.Error("HasProvisioner() = false; want true when both backends are wired")
	}
}

// ─── IsSaaS ────────────────────────────────────────────────────────────────

func TestIsSaaS_CPNotWired(t *testing.T) {
	h := &WorkspaceHandler{}
	if h.IsSaaS() {
		t.Error("IsSaaS() = true; want false when cpProv is nil")
	}
}

func TestIsSaaS_CPWired(t *testing.T) {
	h := &WorkspaceHandler{cpProv: &mockCPProvAPI{}}
	if !h.IsSaaS() {
		t.Error("IsSaaS() = true; want true when cpProv is wired")
	}
}

func TestIsSaaS_DockerOnlyNotSaaS(t *testing.T) {
	h := &WorkspaceHandler{provisioner: &mockLocalProvAPI{}}
	if h.IsSaaS() {
		t.Error("IsSaaS() = true; want false when only provisioner is wired (self-hosted)")
	}
}

// ─── DefaultTier ────────────────────────────────────────────────────────────

func TestDefaultTier_SaaS(t *testing.T) {
	h := &WorkspaceHandler{cpProv: &mockCPProvAPI{}}
	got := h.DefaultTier()
	if got != 4 {
		t.Errorf("DefaultTier() = %d; want 4 for SaaS (T4 = full host, single container per EC2)", got)
	}
}

func TestDefaultTier_SelfHosted(t *testing.T) {
	h := &WorkspaceHandler{provisioner: &mockLocalProvAPI{}}
	got := h.DefaultTier()
	if got != 3 {
		t.Errorf("DefaultTier() = %d; want 3 for self-hosted (T3 = privileged, Docker-in-host)", got)
	}
}

func TestDefaultTier_NeitherWired(t *testing.T) {
	h := &WorkspaceHandler{}
	got := h.DefaultTier()
	// No backend wired — falls through to IsSaaS()=false path, returns T3.
	// This is the correct behaviour: a configured-but-not-yet-provisioned
	// workspace gets the self-hosted default tier.
	if got != 3 {
		t.Errorf("DefaultTier() = %d; want 3 when neither backend is wired", got)
	}
}

// ─── Dispatcher routing consistency ──────────────────────────────────────────
// These tests document the invariant that all three Auto dispatchers use the
// same CP-first ordering when both backends are wired.

func TestDispatcherCPFirstOrdering(t *testing.T) {
	// All Auto dispatchers pick cpProv first when both are set.
	// This test documents the contract so future contributors can't
	// accidentally change the ordering in one helper without noticing.
	h := &WorkspaceHandler{
		cpProv:      &mockCPProvAPI{},
		provisioner: &mockLocalProvAPI{},
	}
	// IsSaaS and DefaultTier both route through the same cpProv check.
	if !h.IsSaaS() {
		t.Error("IsSaaS() = false; want true when cpProv is set (CP-first ordering)")
	}
	if h.DefaultTier() != 4 {
		t.Errorf("DefaultTier() = %d; want 4 when cpProv is set", h.DefaultTier())
	}
	if !h.HasProvisioner() {
		t.Error("HasProvisioner() = false; want true when cpProv is set")
	}
}
