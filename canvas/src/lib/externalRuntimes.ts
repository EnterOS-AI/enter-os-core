/**
 * External-like (BYO-compute) runtime detection.
 *
 * Mirrors the backend's isExternalLikeRuntime() in
 * workspace-server/internal/handlers/runtime_registry.go.
 *
 * These runtimes have no platform-owned container — the operator installs
 * the agent CLI locally and calls /registry/register. They share UX
 * behaviour: no Files tab, no Terminal tab, no Docker config, and the
 * connection modal shows copy-paste snippets.
 */

const EXTERNAL_LIKE_RUNTIMES = new Set([
  "external",
  "kimi",
  "kimi-cli",
]);

export function isExternalLikeRuntime(runtime: string | undefined): boolean {
  return !!runtime && EXTERNAL_LIKE_RUNTIMES.has(runtime);
}
