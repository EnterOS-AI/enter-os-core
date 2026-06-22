package provisioner

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/api/types/volume"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDockerClient is a minimal in-memory implementation of the dockerClient
// interface used by the provisioner. It records calls and lets tests simulate
// the legacy/full-ID naming transition without a real Docker daemon.
type fakeDockerClient struct {
	mu sync.Mutex

	volumes    map[string]volume.Volume
	containers map[string]container.InspectResponse

	renameErr         error
	migrationExitCode int64

	volumeInspectCalls    []string
	volumeCreateCalls     []volume.CreateOptions
	volumeRemoveCalls     []string
	containerInspectCalls []string
	containerRemoveCalls  []string
	containerRenameCalls  []struct{ Old, New string }
	containerCreateCalls  []string
	containerStartCalls   []string
}

func newFakeDockerClient() *fakeDockerClient {
	return &fakeDockerClient{
		volumes:    make(map[string]volume.Volume),
		containers: make(map[string]container.InspectResponse),
	}
}

func (f *fakeDockerClient) Close() error { return nil }

func (f *fakeDockerClient) ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerCreateCalls = append(f.containerCreateCalls, containerName)
	return container.CreateResponse{ID: "cid-" + containerName}, nil
}

func (f *fakeDockerClient) ContainerExecAttach(ctx context.Context, execID string, config container.ExecAttachOptions) (types.HijackedResponse, error) {
	return types.HijackedResponse{}, errors.New("not implemented")
}

func (f *fakeDockerClient) ContainerExecCreate(ctx context.Context, ctr string, config container.ExecOptions) (container.ExecCreateResponse, error) {
	return container.ExecCreateResponse{}, errors.New("not implemented")
}

func (f *fakeDockerClient) ContainerInspect(ctx context.Context, name string) (container.InspectResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerInspectCalls = append(f.containerInspectCalls, name)
	if c, ok := f.containers[name]; ok {
		return c, nil
	}
	return container.InspectResponse{}, errors.New("No such container: " + name)
}

func (f *fakeDockerClient) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeDockerClient) ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeDockerClient) ContainerRemove(ctx context.Context, name string, options container.RemoveOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerRemoveCalls = append(f.containerRemoveCalls, name)
	if _, ok := f.containers[name]; ok {
		delete(f.containers, name)
		return nil
	}
	return errors.New("No such container: " + name)
}

func (f *fakeDockerClient) ContainerRename(ctx context.Context, oldName, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerRenameCalls = append(f.containerRenameCalls, struct{ Old, New string }{oldName, newName})
	if f.renameErr != nil {
		return f.renameErr
	}
	if c, ok := f.containers[oldName]; ok {
		delete(f.containers, oldName)
		c.Name = newName
		f.containers[newName] = c
	}
	return nil
}

func (f *fakeDockerClient) ContainerStart(ctx context.Context, id string, options container.StartOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerStartCalls = append(f.containerStartCalls, id)
	return nil
}

func (f *fakeDockerClient) ContainerWait(ctx context.Context, id string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	waitCh := make(chan container.WaitResponse, 1)
	errCh := make(chan error)
	f.mu.Lock()
	code := f.migrationExitCode
	f.mu.Unlock()
	waitCh <- container.WaitResponse{StatusCode: code}
	close(waitCh)
	return waitCh, errCh
}

func (f *fakeDockerClient) CopyToContainer(ctx context.Context, container, path string, content io.Reader, options container.CopyToContainerOptions) error {
	return errors.New("not implemented")
}

func (f *fakeDockerClient) ImageInspect(ctx context.Context, img string, opts ...client.ImageInspectOption) (image.InspectResponse, error) {
	return image.InspectResponse{}, errors.New("not implemented")
}

func (f *fakeDockerClient) ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeDockerClient) VolumeCreate(ctx context.Context, options volume.CreateOptions) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeCreateCalls = append(f.volumeCreateCalls, options)
	v := volume.Volume{Name: options.Name, Labels: options.Labels}
	f.volumes[options.Name] = v
	return v, nil
}

func (f *fakeDockerClient) VolumeInspect(ctx context.Context, name string) (volume.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeInspectCalls = append(f.volumeInspectCalls, name)
	if v, ok := f.volumes[name]; ok {
		return v, nil
	}
	return volume.Volume{}, errors.New("No such volume: " + name)
}

func (f *fakeDockerClient) VolumeRemove(ctx context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumeRemoveCalls = append(f.volumeRemoveCalls, name)
	if _, ok := f.volumes[name]; ok {
		delete(f.volumes, name)
		return nil
	}
	return errors.New("No such volume: " + name)
}

func runningContainer(name string) container.InspectResponse {
	return container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			Name:  name,
			State: &container.State{Running: true},
		},
	}
}

const migrateTestWorkspaceID = "abcdef1234567890"

func TestResolveConfigVolumeName_LegacyExists_MigratesInPlace(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ConfigVolumeName(migrateTestWorkspaceID)
	legacyName := legacyConfigVolumeName(migrateTestWorkspaceID)
	cli.volumes[legacyName] = volume.Volume{Name: legacyName}

	got := p.resolveConfigVolumeName(ctx, migrateTestWorkspaceID)
	if got != newName {
		t.Fatalf("resolveConfigVolumeName: got %q, want %q", got, newName)
	}

	// Migration path must inspect both names, then create new volume and copy.
	if !strings.Contains(strings.Join(cli.volumeInspectCalls, ","), legacyName) {
		t.Fatalf("expected VolumeInspect(%s) for legacy presence check", legacyName)
	}
	if !strings.Contains(strings.Join(cli.volumeInspectCalls, ","), newName) {
		t.Fatalf("expected VolumeInspect(%s) to check for existing new volume", newName)
	}
	if len(cli.volumeCreateCalls) == 0 || cli.volumeCreateCalls[0].Name != newName {
		t.Fatalf("expected VolumeCreate(%s)", newName)
	}
	if len(cli.containerCreateCalls) == 0 {
		t.Fatal("expected migration container creation")
	}
	if len(cli.containerStartCalls) == 0 {
		t.Fatal("expected migration container start")
	}
	if len(cli.containerRemoveCalls) == 0 {
		t.Fatal("expected migration container removal")
	}

	// Legacy volume must be removed so it is not orphaned.
	removed := false
	for _, n := range cli.volumeRemoveCalls {
		if n == legacyName {
			removed = true
			break
		}
	}
	if !removed {
		t.Fatalf("legacy volume %s was not removed after migration — orphan risk", legacyName)
	}
	if _, ok := cli.volumes[legacyName]; ok {
		t.Fatalf("legacy volume %s still present in fake client after migration", legacyName)
	}
	if _, ok := cli.volumes[newName]; !ok {
		t.Fatalf("new volume %s must exist after migration", newName)
	}
}

func TestResolveConfigVolumeName_LegacyAbsent_NoMigration(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ConfigVolumeName(migrateTestWorkspaceID)
	legacyName := legacyConfigVolumeName(migrateTestWorkspaceID)

	got := p.resolveConfigVolumeName(ctx, migrateTestWorkspaceID)
	if got != newName {
		t.Fatalf("resolveConfigVolumeName: got %q, want %q", got, newName)
	}

	// Should check legacy once and short-circuit.
	if len(cli.volumeInspectCalls) != 1 || cli.volumeInspectCalls[0] != legacyName {
		t.Fatalf("expected exactly one VolumeInspect call for legacy name, got %v", cli.volumeInspectCalls)
	}
	if len(cli.volumeCreateCalls) != 0 {
		t.Fatalf("expected no VolumeCreate when legacy absent, got %v", cli.volumeCreateCalls)
	}
	if len(cli.volumeRemoveCalls) != 0 {
		t.Fatalf("expected no VolumeRemove when legacy absent, got %v", cli.volumeRemoveCalls)
	}
}

func TestResolveClaudeSessionVolumeName_LegacyExists_MigratesInPlace(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ClaudeSessionVolumeName(migrateTestWorkspaceID)
	legacyName := legacyClaudeSessionVolumeName(migrateTestWorkspaceID)
	cli.volumes[legacyName] = volume.Volume{Name: legacyName}

	got := p.resolveClaudeSessionVolumeName(ctx, migrateTestWorkspaceID)
	if got != newName {
		t.Fatalf("resolveClaudeSessionVolumeName: got %q, want %q", got, newName)
	}

	removed := false
	for _, n := range cli.volumeRemoveCalls {
		if n == legacyName {
			removed = true
			break
		}
	}
	if !removed {
		t.Fatalf("legacy session volume %s was not removed after migration — orphan risk", legacyName)
	}
	if _, ok := cli.volumes[newName]; !ok {
		t.Fatalf("new session volume %s must exist after migration", newName)
	}
}

func TestResolveClaudeSessionVolumeName_LegacyAbsent_NoMigration(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ClaudeSessionVolumeName(migrateTestWorkspaceID)
	legacyName := legacyClaudeSessionVolumeName(migrateTestWorkspaceID)

	got := p.resolveClaudeSessionVolumeName(ctx, migrateTestWorkspaceID)
	if got != newName {
		t.Fatalf("resolveClaudeSessionVolumeName: got %q, want %q", got, newName)
	}
	if len(cli.volumeInspectCalls) != 1 || cli.volumeInspectCalls[0] != legacyName {
		t.Fatalf("expected exactly one VolumeInspect call for legacy session name, got %v", cli.volumeInspectCalls)
	}
	if len(cli.volumeCreateCalls) != 0 {
		t.Fatalf("expected no VolumeCreate when legacy absent, got %v", cli.volumeCreateCalls)
	}
}

func TestMigrateVolumeIfNeeded_CopyFails_PreservesLegacy(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ConfigVolumeName(migrateTestWorkspaceID)
	legacyName := legacyConfigVolumeName(migrateTestWorkspaceID)
	cli.volumes[legacyName] = volume.Volume{Name: legacyName}
	cli.migrationExitCode = 1

	if err := p.migrateVolumeIfNeeded(ctx, newName, legacyName); err == nil {
		t.Fatal("expected migration error when copy container exits non-zero")
	}

	// Legacy volume must survive a failed copy so no data is lost.
	if _, ok := cli.volumes[legacyName]; !ok {
		t.Fatal("legacy volume must be preserved when migration copy fails (data-loss guard)")
	}
}

func TestStop_FullIDAbsent_LegacyRemoved(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ContainerName(migrateTestWorkspaceID)
	legacyName := legacyContainerName(migrateTestWorkspaceID)
	cli.containers[legacyName] = runningContainer(legacyName)

	if err := p.Stop(ctx, migrateTestWorkspaceID); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if len(cli.containerRemoveCalls) < 2 {
		t.Fatalf("expected Remove on full-id then legacy, got %v", cli.containerRemoveCalls)
	}
	if cli.containerRemoveCalls[0] != newName {
		t.Fatalf("expected first remove target %q, got %q", newName, cli.containerRemoveCalls[0])
	}
	if cli.containerRemoveCalls[1] != legacyName {
		t.Fatalf("expected second remove target %q, got %q", legacyName, cli.containerRemoveCalls[1])
	}
	if _, ok := cli.containers[legacyName]; ok {
		t.Fatal("legacy container still present after Stop")
	}
}

func TestStop_BothAbsent_IsNoOp(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()
	p := &Provisioner{cli: cli, alpineImage: "alpine"}

	newName := ContainerName(migrateTestWorkspaceID)
	legacyName := legacyContainerName(migrateTestWorkspaceID)

	if err := p.Stop(ctx, migrateTestWorkspaceID); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if len(cli.containerRemoveCalls) != 2 {
		t.Fatalf("expected 2 remove attempts, got %v", cli.containerRemoveCalls)
	}
	if cli.containerRemoveCalls[0] != newName || cli.containerRemoveCalls[1] != legacyName {
		t.Fatalf("expected remove order [%q, %q], got %v", newName, legacyName, cli.containerRemoveCalls)
	}
}

func TestRunningContainerName_LegacyRunning_RenameFails_FallsBackToLegacy(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()

	newName := ContainerName(migrateTestWorkspaceID)
	legacyName := legacyContainerName(migrateTestWorkspaceID)
	cli.containers[legacyName] = runningContainer(legacyName)
	cli.renameErr = errors.New("daemon rename failed")

	got, err := RunningContainerName(ctx, cli, migrateTestWorkspaceID)
	if err != nil {
		t.Fatalf("RunningContainerName returned error: %v", err)
	}
	if got != legacyName {
		t.Fatalf("expected fallback to legacy name %q, got %q", legacyName, got)
	}

	if len(cli.containerInspectCalls) < 2 {
		t.Fatalf("expected inspect of legacy and new names, got %v", cli.containerInspectCalls)
	}
	if cli.containerInspectCalls[0] != legacyName {
		t.Fatalf("expected legacy inspect first, got %v", cli.containerInspectCalls)
	}

	renamed := false
	for _, r := range cli.containerRenameCalls {
		if r.Old == legacyName && r.New == newName {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatalf("expected rename attempt %q -> %q, got %v", legacyName, newName, cli.containerRenameCalls)
	}
}

func TestRunningContainerName_LegacyRunning_RenameSucceeds(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()

	newName := ContainerName(migrateTestWorkspaceID)
	legacyName := legacyContainerName(migrateTestWorkspaceID)
	cli.containers[legacyName] = runningContainer(legacyName)

	got, err := RunningContainerName(ctx, cli, migrateTestWorkspaceID)
	if err != nil {
		t.Fatalf("RunningContainerName returned error: %v", err)
	}
	if got != newName {
		t.Fatalf("expected new name %q after rename, got %q", newName, got)
	}
	if _, ok := cli.containers[legacyName]; ok {
		t.Fatal("legacy container should have been renamed away")
	}
	if _, ok := cli.containers[newName]; !ok {
		t.Fatal("new container name should exist after rename")
	}
}

func TestRunningContainerName_NewRunning_ReturnsNew(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()

	newName := ContainerName(migrateTestWorkspaceID)
	cli.containers[newName] = runningContainer(newName)

	got, err := RunningContainerName(ctx, cli, migrateTestWorkspaceID)
	if err != nil {
		t.Fatalf("RunningContainerName returned error: %v", err)
	}
	if got != newName {
		t.Fatalf("expected new name %q, got %q", newName, got)
	}
}

func TestRunningContainerName_BothAbsent_ReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	cli := newFakeDockerClient()

	got, err := RunningContainerName(ctx, cli, migrateTestWorkspaceID)
	if err != nil {
		t.Fatalf("RunningContainerName returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty name when neither container exists, got %q", got)
	}
}
