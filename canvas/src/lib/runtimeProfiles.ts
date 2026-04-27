/**
 * Runtime profiles — per-runtime UX metadata.
 *
 * Scaling target: hundreds of runtimes (plugin-architecture-v2 roadmap).
 * This module is the single source of truth for runtime-specific UI knobs
 * on the canvas side. Each runtime can declare:
 *
 *   - provisionTimeoutMs: when to show the "taking longer than expected"
 *     banner. Fast docker runtimes = 2min; slow source-build runtimes = 12min.
 *   - (future) label, icon, color, helpUrl, capabilities — add as needed.
 *
 * Resolution order (most specific wins):
 *
 *   1. Server-provided override on the workspace data (e.g.
 *      `workspace.data.provisionTimeoutMs` set from a template manifest).
 *      Lets operators tune without a canvas release once server-side
 *      declarative config lands.
 *   2. Per-runtime entry in RUNTIME_PROFILES.
 *   3. DEFAULT_RUNTIME_PROFILE.
 *
 * Adding a new runtime:
 *   - If it's fast (≤ 2min cold boot): do nothing, the default catches it.
 *   - If it's slow: add one entry to RUNTIME_PROFILES below.
 *   - Long-term: move runtime profiles server-side so this file can shrink.
 *
 * Architectural note: this deliberately lives under /lib, NOT
 * /components/ProvisioningTimeout. Other components (e.g. a
 * "create workspace" dialog that needs to know the runtime's expected
 * cold-boot time) should import from here too — avoids duplicating the
 * runtime-name knowledge across the codebase.
 */

/**
 * Structural shape of a runtime profile. Add fields as new UX knobs
 * become runtime-specific. Every field should be optional so new runtimes
 * can partially fill the profile without breaking older code that reads
 * only some fields.
 */
export interface RuntimeProfile {
  /** Milliseconds before the canvas shows the "taking too long" banner.
   *  Base value — the ProvisioningTimeout component still scales this by
   *  concurrent-provisioning count. */
  provisionTimeoutMs?: number;
  // Future extensions (kept commented until used):
  // label?: string;
  // icon?: string;
  // color?: string;
  // helpUrl?: string;
}

/** The floor every runtime inherits unless it overrides. Calibrated for
 *  docker-local fast runtimes (claude-code, langgraph, crewai) where cold
 *  boot is 30-90s. */
export const DEFAULT_RUNTIME_PROFILE: Required<
  Pick<RuntimeProfile, "provisionTimeoutMs">
> = {
  provisionTimeoutMs: 120_000, // 2 min
};

/**
 * Named per-runtime overrides. Keep this map small and explicit —
 * each entry is a deliberate statement that this runtime's cold-boot
 * behavior differs materially from the default AND that the runtime's
 * template manifest hasn't yet declared a server-side
 * `provision_timeout_seconds` (the preferred path post-#2054).
 *
 * Each override must also ship with a comment explaining WHY the default
 * is wrong for this runtime. Unexplained numbers rot.
 *
 * Empty today — `hermes` previously lived here at 720_000ms, but
 * Molecule-AI/molecule-ai-workspace-template-hermes now declares the
 * value in its config.yaml manifest, so the value flows through the
 * server (workspace API → WorkspaceData.provision_timeout_ms → resolver
 * overrides) instead of being canvas-hardcoded. New runtimes that need
 * a non-default cold-boot threshold should follow the same pattern:
 * declare `runtime_config.provision_timeout_seconds` in their template
 * manifest, NOT add an entry here.
 */
export const RUNTIME_PROFILES: Record<string, RuntimeProfile> = {};

/**
 * Data fields the canvas can consult for per-workspace overrides. These
 * let the backend (via workspace data on the socket payload) override
 * profile values without a canvas release.
 *
 * Intentionally loose typing — if a field isn't present on the node, we
 * fall through to the runtime profile.
 */
export interface WorkspaceRuntimeOverrides {
  provisionTimeoutMs?: number;
}

/**
 * Resolve a runtime profile for a given runtime name, optionally merging
 * server-provided per-workspace overrides on top.
 *
 * Resolution (most-specific wins):
 *   overrides.provisionTimeoutMs
 *   → RUNTIME_PROFILES[runtime].provisionTimeoutMs
 *   → DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs
 */
export function getRuntimeProfile(
  runtime: string | undefined,
  overrides?: WorkspaceRuntimeOverrides,
): Required<Pick<RuntimeProfile, "provisionTimeoutMs">> {
  const profile = runtime ? RUNTIME_PROFILES[runtime] : undefined;
  return {
    provisionTimeoutMs:
      overrides?.provisionTimeoutMs ??
      profile?.provisionTimeoutMs ??
      DEFAULT_RUNTIME_PROFILE.provisionTimeoutMs,
  };
}

/** Convenience: just the provisionTimeoutMs. Equivalent to
 *  `getRuntimeProfile(runtime, overrides).provisionTimeoutMs`. */
export function provisionTimeoutForRuntime(
  runtime: string | undefined,
  overrides?: WorkspaceRuntimeOverrides,
): number {
  return getRuntimeProfile(runtime, overrides).provisionTimeoutMs;
}
