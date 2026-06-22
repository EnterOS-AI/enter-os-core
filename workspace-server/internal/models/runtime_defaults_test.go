package models

// runtime_defaults_test.go — previously pinned DefaultModel's contract
// (claude-code → "sonnet", everything else → "anthropic:claude-opus-4-7").
//
// DefaultModel was removed as a soft-fallback bug magnet (CTO 2026-05-22):
// model is REQUIRED user input; the platform must not provide a default.
// See runtime_defaults.go for the deletion rationale, and the new
// fail-closed gate in `handlers.WorkspaceHandler.Create` for the boundary
// enforcement. No test stub here — the contract is "this function does
// not exist", which the type-checker enforces at compile time.
