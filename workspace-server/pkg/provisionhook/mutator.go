// Package provisionhook is the public extension point that lets external
// plugins mutate the env map a workspace container will boot with, just
// before the provisioner calls Start(cfg).
//
// The package lives under pkg/ (not internal/) because plugins import it
// from outside this Go module. Anything outside pkg/ is core-only.
//
// # Why this exists
//
// Auth providers (GitHub App tokens, GitLab tokens, Bitbucket app
// passwords, internal PAT vaults), secret managers (Vault, AWS Secrets
// Manager, GCP Secret Manager), per-tenant config injectors, and
// observability sidecars all want to write env vars into the workspace
// container before it starts. Each is an OPTIONAL concern that only some
// deployments need. Hardcoding any of them in the platform binary
// violates the "core stays small, capabilities are plugins" principle
// (CEO 2026-04-16, after the monorepo → 44 sub-repos split).
//
// # Plugin shape
//
// A plugin implements EnvMutator and registers an instance with a
// Registry at platform startup. The provisioner calls Run(...) on the
// registry before each workspace container starts.
//
// Plugins live in their own Go modules + repos (e.g.
// github.com/Molecule-AI/molecule-ai-plugin-github-app-auth). Each
// plugin ships its own cmd/server/main.go that imports core's startup
// function + registers the plugin's mutator. Operators deploy the
// plugin binary instead of core's vanilla cmd/server when they want
// the plugin's behaviour.
//
// # Failure handling
//
// MutateEnv returning a non-nil error aborts the provision (workspace
// is marked 'failed', container never starts). Plugins should fail open
// on transient external-service errors (log + return nil) so a flaky
// upstream doesn't block agent provisioning. Reserve errors for hard
// config bugs that the operator must fix.
//
// # Concurrency
//
// Registry is safe for concurrent registration + execution. MutateEnv
// implementations should be safe to call from goroutines (the
// provisioner runs each workspace's provision in its own goroutine).
package provisionhook

import (
	"context"
	"fmt"
	"log"
	"reflect"
	"sync"
	"time"
)

// EnvMutator is implemented by plugins that want to inject env vars
// into a workspace container at provision time.
//
//   - Name returns a stable identifier for logging / metrics. Should
//     match the plugin's repo / module name (e.g. "github-app-auth").
//   - MutateEnv receives the workspace ID, the create payload, and a
//     mutable env map. It can read existing values, add new ones, or
//     overwrite as needed. Mutations are visible to subsequent
//     mutators in the chain (registration order).
type EnvMutator interface {
	Name() string
	MutateEnv(ctx context.Context, workspaceID string, env map[string]string) error
}

// TokenProvider is an optional interface that EnvMutator implementations
// may also satisfy. When a mutator implements TokenProvider the platform
// can serve GET /admin/github-installation-token, allowing long-running
// workspaces to fetch a fresh GitHub token without restarting.
//
// # Why a separate interface?
//
// EnvMutator.MutateEnv is called once at provision time and writes into
// an env map. Calling it again just to read the current token would be
// semantically wrong and potentially unsafe (the env map is a live
// workspace struct). TokenProvider cleanly separates "what do I inject
// at boot?" from "what is the live token right now?".
//
// # Plugin contract
//
// Token must return the current valid token and the time at which it
// will expire. If the plugin's internal cache is past its refresh
// threshold it must block until a new token is obtained before
// returning. Token should never return an expired token — callers rely
// on this guarantee and do not do their own expiry check.
//
// Returning a non-nil error causes the HTTP handler to respond 500 and
// log "[github] token refresh failed: <err>". The workspace will retry
// on its next credential-helper invocation.
type TokenProvider interface {
	Token(ctx context.Context) (token string, expiresAt time.Time, err error)
}

// Registry holds the ordered list of EnvMutator instances the
// provisioner runs before each workspace boot. Safe for concurrent
// registration + execution.
type Registry struct {
	mu       sync.RWMutex
	mutators []EnvMutator
}

// NewRegistry returns an empty registry. The platform creates one at
// startup; plugins call Register on it.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a mutator to the chain. Mutators run in registration
// order. Registering the same instance twice is allowed (it'll run
// twice) — the registry doesn't dedupe; that's the caller's
// responsibility if dedup matters.
func (r *Registry) Register(m EnvMutator) {
	if m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mutators = append(r.mutators, m)
}

// Len reports how many mutators are registered. Used by the platform's
// boot log so operators can see which extension hooks are wired.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.mutators)
}

// Names returns the names of registered mutators in registration order.
// Used by the boot log so operators can grep for which plugins are
// active.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, len(r.mutators))
	for i, m := range r.mutators {
		names[i] = m.Name()
	}
	return names
}

// Mutators returns a copy of the registered mutators in registration
// order. Used when multiple plugins build their own registries and need
// to merge onto a shared one at boot. Returns a copy so callers can't
// mutate internal state.
func (r *Registry) Mutators() []EnvMutator {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]EnvMutator, len(r.mutators))
	copy(out, r.mutators)
	return out
}

// FirstTokenProvider returns the first registered mutator that also
// implements TokenProvider, or nil if none do. Used to back the
// GET /admin/github-installation-token endpoint so long-running
// workspaces can refresh their GITHUB_TOKEN without a container restart.
//
// Uses both direct type assertion AND reflection fallback. The reflection
// path handles the case where the plugin was compiled against a different
// copy of the provisionhook package (Go module boundary issue #960) —
// the method signatures match but the interface types don't, so the
// direct assertion fails. The reflection adapter wraps the method call
// so the rest of the platform sees a normal TokenProvider.
//
// A nil registry returns nil (no provider configured).
func (r *Registry) FirstTokenProvider() TokenProvider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range r.mutators {
		// Direct type assertion (same module boundary)
		if tp, ok := m.(TokenProvider); ok {
			return tp
		}
		// Reflection fallback (cross-module boundary #960)
		if tp := reflectTokenProvider(m); tp != nil {
			return tp
		}
	}
	return nil
}

// Run calls every registered mutator in order. The first one to return
// a non-nil error aborts the chain — subsequent mutators do NOT run,
// and the error is returned to the caller (which marks the workspace
// failed).
//
// A nil registry is a no-op (returns nil) so the provisioner doesn't
// have to nil-check before calling.
func (r *Registry) Run(ctx context.Context, workspaceID string, env map[string]string) error {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	mutators := make([]EnvMutator, len(r.mutators))
	copy(mutators, r.mutators)
	r.mu.RUnlock()

	for _, m := range mutators {
		if err := m.MutateEnv(ctx, workspaceID, env); err != nil {
			return fmt.Errorf("provisionhook %q: %w", m.Name(), err)
		}
	}
	return nil
}

// reflectTokenProvider uses reflection to check if a mutator has a Token()
// method matching the TokenProvider signature. Returns a wrapper that calls
// the method via reflection, or nil if the method doesn't exist or has the
// wrong signature. This handles the Go module boundary case (#960) where
// the plugin satisfies TokenProvider structurally but the type assertion
// fails because the interface comes from a different package path.
func reflectTokenProvider(m EnvMutator) TokenProvider {
	v := reflect.ValueOf(m)
	t := v.Type()
	log.Printf("provisionhook: reflect check on %q (type=%s, kind=%s, numMethod=%d)", m.Name(), t, t.Kind(), t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		mt := t.Method(i)
		log.Printf("  method[%d]: %s %s", i, mt.Name, mt.Type)
	}
	method := v.MethodByName("Token")
	if !method.IsValid() {
		log.Printf("provisionhook: no Token method on %q", m.Name())
		return nil
	}
	// Verify signature: func(context.Context) (string, time.Time, error)
	mt := method.Type()
	if mt.NumIn() != 1 || mt.NumOut() != 3 {
		return nil
	}
	if mt.In(0) != reflect.TypeOf((*context.Context)(nil)).Elem() {
		return nil
	}
	if mt.Out(0).Kind() != reflect.String || mt.Out(2).String() != "error" {
		return nil
	}
	log.Printf("provisionhook: found Token() via reflection on %q (cross-module boundary fallback)", m.Name())
	return &reflectTokenAdapter{method: method}
}

// reflectTokenAdapter wraps a reflected Token() method as a TokenProvider.
type reflectTokenAdapter struct {
	method reflect.Value
}

func (a *reflectTokenAdapter) Token(ctx context.Context) (string, time.Time, error) {
	results := a.method.Call([]reflect.Value{reflect.ValueOf(ctx)})
	token := results[0].String()
	expiresAt := results[1].Interface().(time.Time)
	var err error
	if !results[2].IsNil() {
		err = results[2].Interface().(error)
	}
	return token, expiresAt, err
}
