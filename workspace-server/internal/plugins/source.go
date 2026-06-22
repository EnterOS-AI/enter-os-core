// Package plugins owns the plugin-install source layer.
//
// A plugin "source" is where the platform fetches plugin files from before
// it hands them to a workspace container. Sources are pluggable by scheme
// so new registries (ClawHub, enterprise private registries, direct HTTP
// tarballs, etc.) can be added without touching install handlers.
//
// The on-disk SHAPE of a plugin (agentskills.io format, MCP server,
// DeepAgents sub-agent, custom) is a separate concern handled by the
// per-runtime adapter layer inside the workspace — see
// workspace-template/plugins_registry.
package plugins

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ErrPluginNotFound is returned by a SourceResolver when the requested
// plugin does not exist at the source (e.g. local dir missing, GitHub
// repo 404). Handlers use errors.Is to map this to HTTP 404 rather than
// relying on fragile string matching of the error message.
var ErrPluginNotFound = errors.New("plugin not found")

// SourceResolver fetches a plugin from a remote or local source into a
// local directory that the install handler can then tar+copy into the
// workspace container.
//
// Implementations MUST:
//   - Return an absolute path to a directory containing the plugin's
//     top-level files (plugin.yaml, adapters/, rules/, skills/, etc.).
//   - Clean up any intermediate state on error.
//   - Honour ctx cancellation for long-running fetches (git clone, http
//     download, …).
//
// Registered at router wiring time via Registry.Register.
type SourceResolver interface {
	// Scheme is the URL scheme this resolver handles ("local", "github",
	// "clawhub", "https", …). Must be unique per platform.
	Scheme() string

	// Fetch retrieves the plugin identified by `spec` (scheme-specific
	// path, e.g. "org/repo#v1.0" for github) and writes its contents to
	// `dst`, which the caller creates and owns. Returns the resolved
	// plugin name (used for /configs/plugins/<name>/).
	Fetch(ctx context.Context, spec string, dst string) (pluginName string, err error)
}

// Source is a parsed plugin spec of the form "<scheme>://<spec>". Bare
// names are treated as "local://<name>".
type Source struct {
	Scheme string
	Spec   string
}

// Raw returns the normalized string form ("scheme://spec"). Note this
// is the normalized form: `ParseSource("foo")` → `{local, foo}` → Raw
// returns `"local://foo"`, NOT the original input.
func (s Source) Raw() string {
	return s.Scheme + "://" + s.Spec
}

// String is Raw so Source satisfies fmt.Stringer and logs cleanly.
func (s Source) String() string { return s.Raw() }

// schemeRE matches "<scheme>://" where scheme is the usual URL-scheme
// grammar (ASCII letters, digits, +, -, .). The body match is `.*` (can
// be empty) so we can emit a targeted error for `local://` rather than
// letting it fall through to the bare-name branch and produce a
// nonsensical Source{Scheme:"local", Spec:"local://"}.
var schemeRE = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+\-.]*)://(.*)$`)

// ParseSource parses a plugin source spec.
//
// Accepted forms:
//
//	"my-plugin"              → Source{Scheme: "local", Spec: "my-plugin"}
//	"local://my-plugin"      → Source{Scheme: "local", Spec: "my-plugin"}
//	"github://foo/bar"       → Source{Scheme: "github", Spec: "foo/bar"}
//	"github://foo/bar#v1.0"  → Source{Scheme: "github", Spec: "foo/bar#v1.0"}
//	"clawhub://sonoscli@1.2" → Source{Scheme: "clawhub", Spec: "sonoscli@1.2"}
//
// An empty input returns an error.
func ParseSource(input string) (Source, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return Source{}, fmt.Errorf("empty source spec")
	}
	m := schemeRE.FindStringSubmatch(input)
	if m == nil {
		// Bare name → local.
		return Source{Scheme: "local", Spec: input}, nil
	}
	scheme, spec := m[1], strings.TrimSpace(m[2])
	if spec == "" {
		return Source{}, fmt.Errorf("source %q has empty spec after %q scheme", input, scheme)
	}
	return Source{Scheme: scheme, Spec: spec}, nil
}

// PluginNameFromSource derives the install name (/configs/plugins/<name>/
// directory key) a source spec WILL resolve to, WITHOUT fetching. The
// post-online reconcile (RFC#2843) needs this at declaration time — before
// the box is online and a real Fetch is possible — to (a) record the declared
// row's plugin_name and (b) diff declared-vs-installed.
//
// The derivation MUST match each resolver's Fetch return value:
//   - local://<name>                       → <name>
//   - github://<owner>/<repo>[#ref]        → <repo>
//   - gitea://<owner>/<repo>[/<sub...>][#ref]
//     → last subpath segment, or <repo> when no subpath
//   - bare "<name>"                         → <name>  (treated as local)
//
// Returns an error for unparseable input or a scheme whose naming rule isn't
// known here (so a new resolver can't silently get a wrong declared name).
func PluginNameFromSource(input string) (string, error) {
	src, err := ParseSource(input)
	if err != nil {
		return "", err
	}
	switch src.Scheme {
	case "local":
		return src.Spec, nil
	case "github":
		// "<owner>/<repo>[#ref]" → repo segment.
		spec := src.Spec
		if i := strings.Index(spec, "#"); i >= 0 {
			spec = spec[:i]
		}
		parts := strings.Split(strings.Trim(spec, "/"), "/")
		if len(parts) < 2 || parts[1] == "" {
			return "", fmt.Errorf("github source %q: cannot derive plugin name", input)
		}
		return parts[1], nil
	case "gitea":
		p, err := parseGiteaSpec(src.Spec)
		if err != nil {
			return "", err
		}
		if p.subpath != "" {
			seg := strings.Split(p.subpath, "/")
			return seg[len(seg)-1], nil
		}
		return p.repo, nil
	default:
		return "", fmt.Errorf("source %q: cannot derive plugin name for scheme %q", input, src.Scheme)
	}
}

// Registry holds the set of registered SourceResolvers keyed by scheme.
//
// Writes (Register) should happen at startup on a single goroutine, but
// the RWMutex makes concurrent Resolve/Schemes + Register combinations
// safe should a future deployment register resolvers dynamically (e.g.
// an enterprise control-plane that enables new schemes at runtime).
type Registry struct {
	mu        sync.RWMutex
	resolvers map[string]SourceResolver
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{resolvers: map[string]SourceResolver{}}
}

// Register adds a resolver. Overwrites any existing resolver for the
// same scheme; a log line in the router surface is the right place to
// warn on accidental double-registration.
func (r *Registry) Register(resolver SourceResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvers[resolver.Scheme()] = resolver
}

// Resolve returns the resolver for a source's scheme, or an error if
// no resolver has been registered for that scheme.
func (r *Registry) Resolve(source Source) (SourceResolver, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	resolver, ok := r.resolvers[source.Scheme]
	if !ok {
		return nil, fmt.Errorf("no resolver registered for scheme %q", source.Scheme)
	}
	return resolver, nil
}

// Schemes returns the sorted list of registered schemes — useful for
// surfacing supported sources via the API.
func (r *Registry) Schemes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.resolvers))
	for s := range r.resolvers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
