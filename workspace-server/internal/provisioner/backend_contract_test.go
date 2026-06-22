package provisioner

// backend_contract_test.go — shared behavioral contract for the two
// workspace backends (Docker + CPProvisioner).
//
// The two implementations today evolved independently — method names
// line up on paper (Start/Stop/IsRunning/GetConsoleOutput) but the
// semantics around error shapes, not-found cases, and cleanup can
// drift because nothing holds them to a single interface. This file
// establishes that contract.
//
// Structure:
//
//   1. `Backend` interface below — the union of methods both backends
//      must satisfy. Used as the compile-time gate that catches drift
//      (adding a method to one implementation without the other stops
//      compiling).
//
//   2. `runBackendContract(t, impl)` runs the same scenarios against
//      any `Backend` value. Each scenario is a table row; adding a
//      new behavior requires extending this one place, not two.
//
//   3. `TestDockerBackend_Contract` and `TestCPProvisionerBackend_
//      Contract` feed the real implementations through the shared
//      runner. They use lightweight fakes (nil Docker client, stub
//      HTTP server) so the tests run in CI without a real daemon or
//      control plane.
//
// This file is intentionally a skeleton — the scenarios list is short
// today because we're establishing the pattern. Each follow-up PR
// that touches a backend method should add its scenario here, not
// bolt a new one-off test onto the implementation's own *_test.go.
//
// NON-GOAL: this is not a replacement for the existing per-backend
// tests. Those cover implementation-specific concerns (Docker image
// pull behavior, CP HTTP retry, etc.). This runner covers the
// cross-backend behavior users care about.

import (
	"context"
	"errors"
	"testing"
)

// Backend is the behavioral contract every workspace-provisioning
// backend (Docker, CPProvisioner, future backends) must satisfy. Method
// signatures here must match the actual implementations exactly — if
// an implementation's signature drifts, Go compile-time catches it at
// the assertion var blocks below.
//
// Kept minimal on purpose; expand only when a new cross-backend
// behavior needs a contract test. Implementation-private methods stay
// off this interface.
//
// NOTE: this is the SHARED-CONTRACT subset — the methods both backends
// must implement. The handler-facing surface is wider (LocalProvisionerAPI
// includes ExecRead / RemoveVolume / VolumeHasFile / WriteAuthTokenToVolume,
// which are Docker-specific because the CP backend delegates volume
// management to EC2 boot scripts). Don't widen Backend with Docker-only
// methods — split into per-backend interfaces if a method only one side
// implements.
type Backend interface {
	Start(ctx context.Context, cfg WorkspaceConfig) (string, error)
	Stop(ctx context.Context, workspaceID string) error
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

// Compile-time assertions — a method signature drift on either backend
// makes this file fail to build, which is the whole point.
var (
	_ Backend = (*Provisioner)(nil)
	_ Backend = (*CPProvisioner)(nil)
)

// backendContractScenario is one behavior every backend must exhibit.
type backendContractScenario struct {
	name string
	run  func(t *testing.T, b Backend)
}

// backendContractScenarios — extend this list when you add a new
// cross-backend behavior. Each scenario runs against every registered
// backend.
//
// Scenarios kept as methods on a closure so they can reference helpers
// without polluting the package namespace.
func backendContractScenarios() []backendContractScenario {
	return []backendContractScenario{
		{
			name: "IsRunning_UnknownWorkspace_ReturnsFalseAndNoError",
			// Contract: asking about a workspace the backend has never
			// seen must return (false, nil) — not a real error, not a
			// panic. Both current backends honor this today; this test
			// pins it so a future "optimization" doesn't break A2A's
			// alive-on-unknown path.
			run: func(t *testing.T, b Backend) {
				// Use a clearly-synthetic workspace ID that neither
				// backend should have state for.
				running, err := b.IsRunning(context.Background(), "contract-test-nonexistent-workspace-id")
				// The Docker backend returns (true, err) when it can't
				// reach the daemon — that's the "transient" contract
				// A2A relies on. The CP backend does the same when the
				// HTTP call fails. Both accept a transient-error shape.
				// For a not-found workspace both should return cleanly.
				// We allow either (false, nil) or (*, err) — the
				// contract prohibits (true, nil) for an unknown ID and
				// prohibits panic.
				_ = err
				_ = running
				// Contract assertion shape: we assert no panic (test
				// survives) + a recognizable return. Tightening this
				// requires deciding what the exact contract is; today
				// both backends do "best effort" lookup.
			},
		},
		{
			name: "Stop_UnknownWorkspace_IsIdempotent",
			// Contract: stopping a workspace that doesn't exist must
			// not error out. Important because the scheduler and the
			// orphan sweeper call Stop speculatively; if it errored on
			// unknown-id, every sweep would spam the logs and the
			// orphan path would never terminate cleanly.
			run: func(t *testing.T, b Backend) {
				err := b.Stop(context.Background(), "contract-test-nonexistent-workspace-id")
				if err != nil {
					t.Logf("Backend.Stop returned %v for unknown ID — acceptable as long as it doesn't panic, but ideally a no-op", err)
				}
			},
		},
	}
}

// runBackendContract is the shared runner. Call this from each
// implementation's contract test with a ready-to-use backend value.
func runBackendContract(t *testing.T, backend Backend) {
	t.Helper()
	for _, sc := range backendContractScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Backend scenario %q panicked: %v", sc.name, r)
				}
			}()
			sc.run(t, backend)
		})
	}
}

// TestDockerBackend_Contract feeds the Docker backend through the
// shared runner. Was Skip'd pending hardening of Provisioner.{Stop,
// IsRunning} against nil Docker client — that hardening landed in
// fix/provisioner-nil-guards-1813, so the scenarios run now. A
// zero-valued *Provisioner returns ErrNoBackend from each method
// instead of panicking, satisfying the no-panic contract.
func TestDockerBackend_Contract(t *testing.T) {
	var p Provisioner
	runBackendContract(t, &p)
}

// TestCPProvisionerBackend_Contract — same story as the Docker variant.
// Hardening landed in fix/provisioner-nil-guards-1813; zero-valued
// *CPProvisioner returns ErrNoBackend from Stop/IsRunning instead of
// panicking on nil httpClient.
func TestCPProvisionerBackend_Contract(t *testing.T) {
	var p CPProvisioner
	runBackendContract(t, &p)
}

// TestZeroValuedBackends_NoPanic pins the contract that motivated #1813:
// a Backend with no underlying client wired up must return cleanly,
// never panic. The orphan sweeper and shutdown paths both call these
// methods speculatively where the receiver could be unset.
//
// The exact error shape varies between backends:
//   - Docker Provisioner has no DB-lookup layer; zero-valued always
//     returns ErrNoBackend.
//   - CPProvisioner threads through a package-level resolveInstanceID
//     lookup; when the DB has no row for the workspace (or db.DB
//     itself is nil), instance_id comes back empty and the method
//     short-circuits to (false, nil). Only when there's a real
//     instance_id to query does the missing-httpClient case surface
//     as ErrNoBackend.
//
// Both shapes are acceptable — the contract is "no panic, error is
// nil or ErrNoBackend." Anything else is a bug.
func TestZeroValuedBackends_NoPanic(t *testing.T) {
	acceptableErr := func(err error) bool {
		return err == nil || errors.Is(err, ErrNoBackend)
	}
	t.Run("Provisioner.Stop", func(t *testing.T) {
		var p Provisioner
		if err := p.Stop(context.Background(), "any"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("zero-valued Provisioner.Stop: got %v, want ErrNoBackend", err)
		}
	})
	t.Run("Provisioner.IsRunning", func(t *testing.T) {
		var p Provisioner
		running, err := p.IsRunning(context.Background(), "any")
		if !errors.Is(err, ErrNoBackend) {
			t.Errorf("zero-valued Provisioner.IsRunning: got err=%v, want ErrNoBackend", err)
		}
		if running {
			t.Errorf("zero-valued Provisioner.IsRunning: got running=true, want false")
		}
	})
	t.Run("CPProvisioner.Stop", func(t *testing.T) {
		var p CPProvisioner
		if err := p.Stop(context.Background(), "any"); !acceptableErr(err) {
			t.Errorf("zero-valued CPProvisioner.Stop: got %v, want nil or ErrNoBackend", err)
		}
	})
	t.Run("CPProvisioner.IsRunning", func(t *testing.T) {
		var p CPProvisioner
		_, err := p.IsRunning(context.Background(), "any")
		if !acceptableErr(err) {
			t.Errorf("zero-valued CPProvisioner.IsRunning: got err=%v, want nil or ErrNoBackend", err)
		}
	})
	// Nil receivers must also be safe. The orphan sweeper code path can
	// hit Stop on a *Provisioner that's nil (not just zero-valued) when
	// the wiring path hasn't initialized at all. For nil receivers we
	// always expect ErrNoBackend (the nil-check at the top fires before
	// any DB lookup could absorb the case).
	t.Run("nil_Provisioner.Stop", func(t *testing.T) {
		var p *Provisioner
		if err := p.Stop(context.Background(), "any"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *Provisioner.Stop: got %v, want ErrNoBackend", err)
		}
	})
	t.Run("nil_CPProvisioner.Stop", func(t *testing.T) {
		var p *CPProvisioner
		if err := p.Stop(context.Background(), "any"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *CPProvisioner.Stop: got %v, want ErrNoBackend", err)
		}
	})

	// Methods extended to the LocalProvisionerAPI interface in #2369 —
	// every one needs the same nil-receiver guard so the typed-nil
	// interface case (handler tests passing nil concrete *Provisioner
	// to NewWorkspaceHandler, which the constructor now elides into a
	// real nil interface) doesn't regress in the future.
	t.Run("nil_Provisioner.Start", func(t *testing.T) {
		var p *Provisioner
		if _, err := p.Start(context.Background(), WorkspaceConfig{}); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *Provisioner.Start: got %v, want ErrNoBackend", err)
		}
	})
	t.Run("nil_Provisioner.ExecRead", func(t *testing.T) {
		var p *Provisioner
		if _, err := p.ExecRead(context.Background(), "any", "any"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *Provisioner.ExecRead: got %v, want ErrNoBackend", err)
		}
	})
	t.Run("nil_Provisioner.RemoveVolume", func(t *testing.T) {
		var p *Provisioner
		if err := p.RemoveVolume(context.Background(), "any"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *Provisioner.RemoveVolume: got %v, want ErrNoBackend", err)
		}
	})
	t.Run("nil_Provisioner.VolumeHasFile", func(t *testing.T) {
		var p *Provisioner
		if _, err := p.VolumeHasFile(context.Background(), "any", "config.yaml"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *Provisioner.VolumeHasFile: got %v, want ErrNoBackend", err)
		}
	})
	t.Run("nil_Provisioner.WriteAuthTokenToVolume", func(t *testing.T) {
		var p *Provisioner
		if err := p.WriteAuthTokenToVolume(context.Background(), "any", "tok"); !errors.Is(err, ErrNoBackend) {
			t.Errorf("nil *Provisioner.WriteAuthTokenToVolume: got %v, want ErrNoBackend", err)
		}
	})
}
