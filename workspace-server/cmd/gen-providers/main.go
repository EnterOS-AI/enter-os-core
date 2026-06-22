// Command gen-providers is the codegen half of the provider-registry SSOT
// machinery on the molecule-core side (internal#718 P2-A, CTO 2026-05-27
// "Distribution = SDK via codegen + verify-CI"). It is the byte-for-byte mirror
// of molecule-controlplane's cmd/gen-providers (the canonical generator). It
// reads core's SYNCED COPY of the schema — internal/providers/providers.yaml
// (via the providers loader, so it shares the SAME parse + validation as the
// runtime) — and emits a checked-in Go artifact:
//
//	internal/providers/gen/registry_gen.go
//
// The artifact is a deterministic projection of the merged registry: the
// provider catalog + per-runtime native sets as Go literals, plus the schema
// version and a content fingerprint. It is core's leaf of the multi-language SDK
// layer the RFC calls for (Go(CP+core)/TS(canvas)/Python(adapters)).
//
// CONTRACT for P2-A (zero behavior change): the generated artifact is
// checked-in + drift-gated ONLY. NO production code path imports
// internal/providers/gen — the gen-import-boundary test pins that. P2-B wires
// the billing/credential decision onto the LOADER (DeriveProvider/IsPlatform),
// not the raw gen literals. The generator is the build-time half;
// verify-providers-gen.yml is the CI half that regenerates and fails RED on any
// diff (drift or hand-edit); sync-providers-yaml.yml gates the synced copy
// against the controlplane canonical.
//
// Usage:
//
//	go run ./cmd/gen-providers            # write the artifact in place
//	go run ./cmd/gen-providers -check     # exit non-zero if the on-disk
//	                                      # artifact differs from a fresh gen
//	                                      # (the CI drift gate)
//	go run ./cmd/gen-providers -o PATH    # write to a specific path
//
//go:generate go run ../gen-providers -o ../../internal/providers/gen/registry_gen.go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strconv"
	"text/template"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/providers"
)

// defaultOutPath is the checked-in artifact location, relative to the repo
// root (the directory `go run ./cmd/gen-providers` is invoked from).
const defaultOutPath = "internal/providers/gen/registry_gen.go"

func main() {
	var (
		outPath string
		check   bool
	)
	flag.StringVar(&outPath, "o", defaultOutPath, "output path for the generated artifact")
	flag.BoolVar(&check, "check", false, "verify the on-disk artifact matches a fresh generation; exit 1 on drift")
	flag.Parse()

	generated, err := render()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-providers: %v\n", err)
		os.Exit(1)
	}

	if check {
		existing, err := os.ReadFile(outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen-providers -check: cannot read %s: %v\n", outPath, err)
			fmt.Fprintln(os.Stderr, "Run `go generate ./...` (or `go run ./cmd/gen-providers`) and commit the result.")
			os.Exit(1)
		}
		if !bytes.Equal(existing, generated) {
			fmt.Fprintf(os.Stderr, "gen-providers -check: DRIFT — %s is out of sync with providers.yaml.\n", outPath)
			fmt.Fprintln(os.Stderr, "The generated artifact was hand-edited or providers.yaml changed without regen.")
			fmt.Fprintln(os.Stderr, "Fix: run `go generate ./...` (or `go run ./cmd/gen-providers`) and commit.")
			os.Exit(1)
		}
		fmt.Println("gen-providers -check: OK — artifact in sync with providers.yaml")
		return
	}

	if err := os.WriteFile(outPath, generated, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen-providers: write %s: %v\n", outPath, err)
		os.Exit(1)
	}
	fmt.Printf("gen-providers: wrote %s\n", outPath)
}

// render loads the manifest and produces the gofmt'd artifact bytes.
func render() ([]byte, error) {
	m, err := providers.LoadManifest()
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}

	// Deterministic ordering: providers in catalog order is already stable
	// (slice). Runtimes is a map — sort its keys so the artifact is
	// reproducible regardless of Go map iteration order.
	runtimeNames := make([]string, 0, len(m.Runtimes))
	for rt := range m.Runtimes {
		runtimeNames = append(runtimeNames, rt)
	}
	sort.Strings(runtimeNames)

	type genProvider struct {
		Name             string
		DisplayName      string
		Protocol         string
		AuthMode         string
		AuthEnv          []string
		ModelPrefixMatch string
		IsPlatform       bool
		// UpstreamVendor is the proxy's upstream-vendor key for this entry
		// (internal#718 P1, CONVERGED) — empty for entries the proxy does not
		// route to an upstream. A plain scalar (no pointer), so both the rendered
		// literal and the fingerprint stay deterministic.
		UpstreamVendor string
	}
	type genRef struct {
		Name   string
		Models []string
	}
	type genRuntime struct {
		Name      string
		Providers []genRef
	}

	data := struct {
		SchemaVersion int
		Fingerprint   string
		Providers     []genProvider
		Runtimes      []genRuntime
	}{
		SchemaVersion: providers.SchemaVersion(),
	}

	for _, p := range m.Providers {
		gp := genProvider{
			Name:             p.Name,
			DisplayName:      p.DisplayName,
			Protocol:         string(p.Protocol),
			AuthMode:         p.AuthMode,
			AuthEnv:          p.AuthEnv,
			ModelPrefixMatch: p.ModelPrefixMatch,
			IsPlatform:       p.IsPlatform(),
			UpstreamVendor:   p.UpstreamVendor,
		}
		data.Providers = append(data.Providers, gp)
	}
	for _, rt := range runtimeNames {
		native := m.Runtimes[rt]
		gr := genRuntime{Name: rt}
		for _, ref := range native.Providers {
			gr.Providers = append(gr.Providers, genRef{Name: ref.Name, Models: ref.Models})
		}
		data.Runtimes = append(data.Runtimes, gr)
	}

	// Fingerprint pins the artifact to the data it was generated from. It is
	// derived from the structured projection (schema version + providers +
	// runtimes), NOT the raw YAML bytes, so a comment-only YAML edit does not
	// churn the artifact while any data change does.
	data.Fingerprint = fingerprint(data.SchemaVersion, data.Providers, data.Runtimes)

	var buf bytes.Buffer
	if err := artifactTmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt generated source: %w\n----\n%s", err, buf.String())
	}
	return formatted, nil
}

// fingerprint is a stable content hash of the structured projection. Any
// fields below this function references must be kept in sync with the
// template's emitted data so the hash and the literals never diverge.
func fingerprint(schema int, provs any, runtimes any) string {
	h := sha256.New()
	fmt.Fprintf(h, "schema=%d\n", schema)
	fmt.Fprintf(h, "%#v\n%#v\n", provs, runtimes)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func quote(s string) string { return strconv.Quote(s) }

func quoteSlice(ss []string) string {
	var b bytes.Buffer
	b.WriteString("[]string{")
	for i, s := range ss {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(s))
	}
	b.WriteString("}")
	return b.String()
}

var artifactTmpl = template.Must(template.New("artifact").Funcs(template.FuncMap{
	"quote":      quote,
	"quoteSlice": quoteSlice,
}).Parse(`// Code generated by cmd/gen-providers; DO NOT EDIT.
//
// Source of truth: internal/providers/providers.yaml (schema_version {{.SchemaVersion}}).
// Regenerate with: go generate ./...   (or: go run ./cmd/gen-providers)
// The verify-providers-gen CI workflow fails RED if this file drifts from
// providers.yaml or is hand-edited. internal#718 P0 — checked-in + drift-
// gated ONLY; no production path imports this package yet (that is P1+).

package gen

// SchemaVersion is the providers.yaml schema this artifact was generated
// against. It is the semver'd contract version (the MAJOR component for the
// public extension contract; see internal/providers/README.md).
const SchemaVersion = {{.SchemaVersion}}

// Fingerprint is a stable content hash of the generated projection (schema
// version + provider catalog + runtime native sets). It changes iff the
// registry DATA changes (comment-only YAML edits do not churn it).
const Fingerprint = {{quote .Fingerprint}}

// GenProvider is the generated projection of one provider catalog entry —
// the subset a downstream consumer needs to derive + display a provider.
type GenProvider struct {
	Name             string
	DisplayName      string
	Protocol         string
	AuthMode         string
	AuthEnv          []string
	ModelPrefixMatch string
	// IsPlatform marks the closed, core-only platform-managed provider.
	IsPlatform bool
	// UpstreamVendor is the proxy's upstream-vendor key for this entry
	// (internal#718 P1, CONVERGED); empty for providers the proxy does not
	// route to an upstream vendor. ResolveUpstream maps a model id's namespace
	// token to the entry whose UpstreamVendor equals it.
	UpstreamVendor string
}

// GenRuntimeRef is one native provider a runtime supports + its exact models.
type GenRuntimeRef struct {
	Name   string
	Models []string
}

// Providers is the full provider catalog, in providers.yaml declaration order.
var Providers = []GenProvider{
{{- range .Providers}}
	{Name: {{quote .Name}}, DisplayName: {{quote .DisplayName}}, Protocol: {{quote .Protocol}}, AuthMode: {{quote .AuthMode}}, AuthEnv: {{quoteSlice .AuthEnv}}, ModelPrefixMatch: {{quote .ModelPrefixMatch}}, IsPlatform: {{.IsPlatform}}{{if .UpstreamVendor}}, UpstreamVendor: {{quote .UpstreamVendor}}{{end}}},
{{- end}}
}

// Runtimes maps each runtime to its native provider+model set, runtime names
// sorted for a deterministic artifact.
var Runtimes = map[string][]GenRuntimeRef{
{{- range .Runtimes}}
	{{quote .Name}}: {
{{- range .Providers}}
		{Name: {{quote .Name}}, Models: {{quoteSlice .Models}}},
{{- end}}
	},
{{- end}}
}
`))
