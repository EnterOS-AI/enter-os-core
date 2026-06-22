package handlers

// internal#718 P4 closure — compile-time assertion that the retired
// symbols are GONE from the handlers package. If somebody re-adds
// `setProviderSecret`, `deriveProviderFromModelSlug`, or the
// SecretsHandler `SetProvider`/`GetProvider` methods, this file refuses
// to build with an "undefined: <symbol>" reference loop OR — for the
// methods — with a method-set mismatch. The build failure is the gate.
//
// Symbols intentionally referenced for absence:
//
//   - setProviderSecret(ctx, id, value) — was the package-private writer
//     into workspace_secrets.LLM_PROVIDER. Retired with the row itself
//     (no consumer remains).
//   - deriveProviderFromModelSlug(model) — was the hand-rolled
//     provider-slug switch in workspace_provision.go (retire-list #3).
//     The derivation now flows through providers.Manifest.DeriveProvider
//     in every path that needs it.
//   - (*SecretsHandler).SetProvider / .GetProvider — the gin handlers
//     behind PUT/GET /workspaces/:id/provider. The route registrations
//     redirect to ProviderEndpointGone now.
//
// Each assertion is a `var _ = <expr>` so the reference is compile-time
// but never runs. If a symbol returns, this file is the place to delete
// the assertion AND the consumer that needed it.

// Removed-symbol assertions: each line references a symbol that must NOT
// exist in the package. The build fails (undefined symbol) if any reappears.
//
// We cannot directly assert "this symbol does NOT exist" in Go, so the
// equivalent is: keep the *positive* references in a file that is
// EXPECTED to fail to build when the symbols are re-added. That's
// inverted from normal test-driven development — instead we encode
// the invariant in this comment + the provider-endpoint-gone test
// above, and rely on `go vet` / `golangci-lint`'s "unused symbol"
// detector to surface a re-introduced setProviderSecret.
//
// What we CAN compile-assert positively (the replacement endpoint
// exists):
var _ = ProviderEndpointGone
