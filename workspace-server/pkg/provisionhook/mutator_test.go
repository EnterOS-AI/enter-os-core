package provisionhook

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeMutator is a test stand-in. Records what MutateEnv received +
// optionally returns a configured error or modifies the env map.
type fakeMutator struct {
	name        string
	mu          sync.Mutex
	calls       int
	lastEnv     map[string]string
	lastWS      string
	returnErr   error
	envToInject map[string]string
}

func (f *fakeMutator) Name() string { return f.name }

func (f *fakeMutator) MutateEnv(ctx context.Context, workspaceID string, env map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastWS = workspaceID
	f.lastEnv = env
	for k, v := range f.envToInject {
		env[k] = v
	}
	return f.returnErr
}

func TestRegistry_RunsMutatorsInOrder(t *testing.T) {
	r := NewRegistry()
	a := &fakeMutator{name: "a", envToInject: map[string]string{"A": "1"}}
	b := &fakeMutator{name: "b", envToInject: map[string]string{"B": "2"}}
	r.Register(a)
	r.Register(b)

	env := map[string]string{}
	if err := r.Run(context.Background(), "ws-1", env); err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "2" {
		t.Errorf("env mutations not applied: %v", env)
	}
	if a.calls != 1 || b.calls != 1 {
		t.Errorf("call counts: a=%d b=%d", a.calls, b.calls)
	}
}

func TestRegistry_LaterMutatorSeesEarlierMutations(t *testing.T) {
	// Mutators run in chain — second mutator should see the env first
	// mutator added. This is the entire point of running them in order
	// (e.g. a secret-resolver plugin can depend on a tenant-config
	// plugin running first).
	r := NewRegistry()
	r.Register(&fakeMutator{name: "first", envToInject: map[string]string{"TENANT": "acme"}})
	saw := ""
	r.Register(&envInspector{onCall: func(env map[string]string) { saw = env["TENANT"] }})

	env := map[string]string{}
	_ = r.Run(context.Background(), "ws-1", env)
	if saw != "acme" {
		t.Errorf("second mutator should have seen TENANT=acme, saw %q", saw)
	}
}

func TestRegistry_FirstErrorAbortsChain(t *testing.T) {
	r := NewRegistry()
	a := &fakeMutator{name: "a", returnErr: errors.New("boom")}
	b := &fakeMutator{name: "b"}
	r.Register(a)
	r.Register(b)

	err := r.Run(context.Background(), "ws-1", map[string]string{})
	if err == nil {
		t.Fatal("expected error from first mutator to propagate")
	}
	if b.calls != 0 {
		t.Errorf("second mutator should not run after first errors; got %d calls", b.calls)
	}
	// Error should be wrapped with the mutator name so logs say which
	// plugin failed, not just the underlying error.
	if !contains(err.Error(), `provisionhook "a"`) {
		t.Errorf("error should name the failing mutator: %v", err)
	}
}

func TestRegistry_NilReceiverIsNoop(t *testing.T) {
	// The provisioner shouldn't have to nil-check before calling.
	var r *Registry
	if err := r.Run(context.Background(), "ws-1", map[string]string{}); err != nil {
		t.Errorf("nil registry should return nil error: %v", err)
	}
}

func TestRegistry_NilMutatorIsIgnored(t *testing.T) {
	r := NewRegistry()
	r.Register(nil)
	r.Register(&fakeMutator{name: "real"})
	if r.Len() != 1 {
		t.Errorf("nil mutator should have been dropped; len=%d", r.Len())
	}
}

func TestRegistry_NamesReturnsRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeMutator{name: "tenant-config"})
	r.Register(&fakeMutator{name: "github-app-auth"})
	r.Register(&fakeMutator{name: "vault-secrets"})
	got := r.Names()
	want := []string{"tenant-config", "github-app-auth", "vault-secrets"}
	if !equalSlices(got, want) {
		t.Errorf("names: got %v, want %v", got, want)
	}
}

func TestRegistry_ConcurrentRegisterAndRun(t *testing.T) {
	// Sanity: the mutex prevents data races between registration +
	// execution. Run with `go test -race`.
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.Register(&fakeMutator{name: "concurrent"})
		}(i)
		go func() {
			defer wg.Done()
			_ = r.Run(context.Background(), "ws-x", map[string]string{})
		}()
	}
	wg.Wait()
	if r.Len() != 50 {
		t.Errorf("expected 50 registered mutators, got %d", r.Len())
	}
}

// envInspector is a tiny mutator that calls a callback on each
// invocation. Used by TestRegistry_LaterMutatorSeesEarlierMutations.
type envInspector struct {
	onCall func(env map[string]string)
}

func (e *envInspector) Name() string { return "inspector" }
func (e *envInspector) MutateEnv(_ context.Context, _ string, env map[string]string) error {
	e.onCall(env)
	return nil
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func equalSlices(a, b []string) bool {
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
