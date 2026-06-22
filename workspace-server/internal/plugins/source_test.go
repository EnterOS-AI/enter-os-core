package plugins

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// ---- ParseSource ----

func TestParseSource_BareNameBecomesLocal(t *testing.T) {
	s, err := ParseSource("my-plugin")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if s.Scheme != "local" || s.Spec != "my-plugin" {
		t.Errorf("got %+v", s)
	}
}

func TestParseSource_ExplicitScheme(t *testing.T) {
	cases := map[string]Source{
		"local://foo":            {Scheme: "local", Spec: "foo"},
		"github://org/repo":      {Scheme: "github", Spec: "org/repo"},
		"github://org/repo#v1.0": {Scheme: "github", Spec: "org/repo#v1.0"},
		"clawhub://name@1.2.3":   {Scheme: "clawhub", Spec: "name@1.2.3"},
		"https://example.com/x":  {Scheme: "https", Spec: "example.com/x"},
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := ParseSource(in)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ParseSource(%q) = %+v, want %+v", in, got, want)
			}
		})
	}
}

func TestParseSource_EmptyRejected(t *testing.T) {
	if _, err := ParseSource(""); err == nil {
		t.Error("expected error on empty input")
	}
	if _, err := ParseSource("   "); err == nil {
		t.Error("expected error on whitespace input")
	}
}

func TestParseSource_StripsWhitespace(t *testing.T) {
	s, err := ParseSource("  my-plugin  ")
	if err != nil || s.Spec != "my-plugin" {
		t.Errorf("got %+v, err=%v", s, err)
	}
}

func TestSource_Raw(t *testing.T) {
	s := Source{Scheme: "github", Spec: "foo/bar#v1"}
	if s.Raw() != "github://foo/bar#v1" {
		t.Errorf("got %q", s.Raw())
	}
}

// ---- Registry ----

type fakeResolver struct {
	scheme string
	calls  int
}

func (f *fakeResolver) Scheme() string { return f.scheme }
func (f *fakeResolver) Fetch(ctx context.Context, spec, dst string) (string, error) {
	f.calls++
	return spec, nil
}

func TestRegistry_RegisterAndResolve(t *testing.T) {
	reg := NewRegistry()
	local := &fakeResolver{scheme: "local"}
	gh := &fakeResolver{scheme: "github"}
	reg.Register(local)
	reg.Register(gh)

	r, err := reg.Resolve(Source{Scheme: "github", Spec: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if r != gh {
		t.Errorf("got wrong resolver: %+v", r)
	}
}

func TestRegistry_UnknownScheme(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Resolve(Source{Scheme: "mystery", Spec: "x"})
	if err == nil {
		t.Error("expected error for unknown scheme")
	}
	if !strings.Contains(err.Error(), "mystery") {
		t.Errorf("error should name the missing scheme: %v", err)
	}
}

func TestRegistry_OverwriteSameScheme(t *testing.T) {
	reg := NewRegistry()
	a := &fakeResolver{scheme: "local"}
	b := &fakeResolver{scheme: "local"}
	reg.Register(a)
	reg.Register(b)
	r, _ := reg.Resolve(Source{Scheme: "local", Spec: "x"})
	if r != b {
		t.Error("second registration should overwrite the first")
	}
}

func TestRegistry_SchemesSorted(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&fakeResolver{scheme: "local"})
	reg.Register(&fakeResolver{scheme: "clawhub"})
	reg.Register(&fakeResolver{scheme: "github"})
	got := reg.Schemes()
	want := []string{"clawhub", "github", "local"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Schemes() = %v, want %v", got, want)
	}
}

func TestRegistry_EmptyReturnsEmpty(t *testing.T) {
	reg := NewRegistry()
	if s := reg.Schemes(); len(s) != 0 {
		t.Errorf("empty registry should return empty slice, got %v", s)
	}
}

func TestErrPluginNotFound_IsMatchable(t *testing.T) {
	// Wrap + unwrap via fmt.Errorf to prove errors.Is works through
	// the fmt wrappers the resolvers use in their error returns.
	err := fmt.Errorf("local resolver: plugin \"x\": %w", ErrPluginNotFound)
	if !errors.Is(err, ErrPluginNotFound) {
		t.Error("errors.Is did not unwrap ErrPluginNotFound")
	}
}

func TestSource_StringEqualsRaw(t *testing.T) {
	s := Source{Scheme: "github", Spec: "foo/bar#v1"}
	if s.String() != s.Raw() {
		t.Errorf("String()=%q Raw()=%q must match", s.String(), s.Raw())
	}
}

func TestRegistry_ConcurrentRegisterResolve_NoRace(t *testing.T) {
	// Exercises the RWMutex: interleave Register / Resolve / Schemes
	// from multiple goroutines. `go test -race` fails loudly if the
	// locking is wrong.
	reg := NewRegistry()
	reg.Register(&fakeResolver{scheme: "local"})

	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(i int) {
			for j := 0; j < 50; j++ {
				reg.Register(&fakeResolver{scheme: fmt.Sprintf("s%d", i)})
				_, _ = reg.Resolve(Source{Scheme: "local"})
				_ = reg.Schemes()
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}

// ---- C1: empty spec after scheme ----

func TestParseSource_EmptySpecAfterSchemeRejected(t *testing.T) {
	for _, in := range []string{"local://", "github://", "https://", "local://   "} {
		t.Run(in, func(t *testing.T) {
			_, err := ParseSource(in)
			if err == nil {
				t.Errorf("ParseSource(%q) should reject empty spec", in)
			} else if !strings.Contains(err.Error(), "empty spec") {
				t.Errorf("error message should mention 'empty spec': %v", err)
			}
		})
	}
}

func TestParseSource_BareNameStillAccepted(t *testing.T) {
	// The empty-spec guard must not break back-compat for bare names.
	s, err := ParseSource("my-plugin")
	if err != nil || s.Scheme != "local" || s.Spec != "my-plugin" {
		t.Errorf("bare name broke: %+v err=%v", s, err)
	}
}

// ---- PluginNameFromSource (RFC#2843 declared-plugin name derivation) ----

func TestPluginNameFromSource(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "seo-all", want: "seo-all"}, // bare → local
		{in: "local://my-plugin", want: "my-plugin"},
		{in: "github://owner/repo#v1.0", want: "repo"},
		{in: "github://owner/repo", want: "repo"},
		{
			in:   "gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main",
			want: "seo-all",
		},
		{in: "gitea://o/r#main", want: "r"},        // gitea whole-repo
		{in: "gitea://o/r/a/b/c#main", want: "c"},  // deep subpath → last seg
		{in: "clawhub://thing@1.2", wantErr: true}, // unknown naming rule
		{in: "", wantErr: true},
	}
	for _, c := range cases {
		got, err := PluginNameFromSource(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("PluginNameFromSource(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("PluginNameFromSource(%q): unexpected err %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("PluginNameFromSource(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
