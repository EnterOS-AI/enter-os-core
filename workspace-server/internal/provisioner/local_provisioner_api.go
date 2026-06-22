package provisioner

import "context"

// LocalProvisionerAPI is the contract WorkspaceHandler uses to talk to the
// local Docker provisioner. Mirrors CPProvisionerAPI so the handler types
// both backends symmetrically — no more "interface for SaaS, concrete for
// Docker" asymmetry that blocked easy mocking and made future
// pluggable-backend work harder than it needed to be (issue #2369).
//
// Method set is the union of methods WorkspaceHandler actually calls
// today. Adding a new handler call site that reaches into Provisioner
// means widening this interface explicitly — the same review-surfacing
// contract CPProvisionerAPI already enforces.
//
// Keep narrow: the *Provisioner type also exposes ListManagedContainerIDPrefixes,
// CopyTemplateToContainer, DockerClient, etc. Those are consumed by
// non-handler call sites (orphan sweeper, registry health watcher) which
// type their dependency as `*Provisioner` directly. Pulling them onto
// this interface would force every handler test to stub method bodies
// they never exercise.
type LocalProvisionerAPI interface {
	Start(ctx context.Context, cfg WorkspaceConfig) (string, error)
	Stop(ctx context.Context, workspaceID string) error
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
	ExecRead(ctx context.Context, containerName, filePath string) ([]byte, error)
	RemoveVolume(ctx context.Context, workspaceID string) error
	VolumeHasFile(ctx context.Context, workspaceID, relPath string) (bool, error)
	WriteAuthTokenToVolume(ctx context.Context, workspaceID, token string) error
}

// Compile-time assertion: *Provisioner satisfies LocalProvisionerAPI.
// Catches a future method-signature drift at build time instead of at
// the NewWorkspaceHandler call site. Mirror of the assertion in
// cp_provisioner.go for CPProvisionerAPI.
var _ LocalProvisionerAPI = (*Provisioner)(nil)
