package models

// runtime_defaults.go — DELETED helper. Intentionally empty.
//
// Previously held `DefaultModel(runtime string) string` which returned
// "sonnet" for claude-code and "anthropic:claude-opus-4-7" for everything
// else. That function was a SOFT-FALLBACK bug magnet:
//
//   - codex workspaces created without an explicit `model` silently
//     received `anthropic:claude-opus-4-7`. Codex adapter only supports
//     openai-* providers, so they wedged in `not_configured` with
//     `codex adapter: workspace config picks provider='anthropic' but
//     it is not in the providers registry`. The fallback never matched
//     a runtime that could actually use it (only claude-code + hermes
//     could even partially execute anthropic:claude-opus-4-7 without
//     extra credential plumbing). It existed as a "must return
//     something" placeholder that turned every silent miss into a
//     prod incident.
//
//   - The fallback hid the contract bug at every callsite: Create
//     handler, org_import, anywhere a stale CreateWorkspacePayload
//     bubbled through to provisionWorkspace.
//
// SSOT principle (CTO 2026-05-22T03:42Z, feedback_workspace_model_required_no_platform_default_dynamic_credential_intake):
// model / provider / provider-credential are REQUIRED user input at
// create time. The platform must not provide a default. The runtime
// must not fall back. Decision belongs to the user (or to the agent
// acting on the user's behalf), never to the platform.
//
// Callers that previously fell back to DefaultModel must now fail-closed
// when model is empty after template-resolution.
