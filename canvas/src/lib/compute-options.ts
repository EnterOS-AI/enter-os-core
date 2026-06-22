// Cloud-provider + instance-type metadata (core#2489).
//
// SSOT lives in the workspace-server (workspace_compute.go allowlist + defaults)
// and is fetched at runtime from GET /compute/metadata (public, workspace-
// independent endpoint — the data is platform constraints, not org secrets), so
// the UI can never offer a (provider, instance-type) the PATCH validation then
// rejects with a 400. The constants below are ONLY a minimal offline fallback
// used until the fetch resolves (or if it fails) — they mirror the server SSOT
// but are not the source of truth. When the fetch succeeds, its data replaces
// them entirely.
//
// Response shape (workspace-server GET /compute/metadata):
//   {
//     providers: ["aws", "gcp", "hetzner"],
//     instanceTypes: { aws: ["t3.medium", ...], ... },
//     defaults: { aws: "t3.medium", ... },
//     display_defaults: { aws: "t3.xlarge", ... }
//   }

export type ComputeOptions = {
  providers: string[];
  instanceTypes: Record<string, string[]>;
  defaults: Record<string, string>;
  displayDefaults: Record<string, string>;
};

export const FALLBACK_COMPUTE_OPTIONS: ComputeOptions = {
  providers: ["aws", "hetzner", "gcp"],
  instanceTypes: {
    aws: ["t3.medium", "t3.large", "t3.xlarge", "t3.2xlarge", "m6i.large", "m6i.xlarge", "c6i.xlarge"],
    hetzner: ["cpx11", "cpx21", "cpx31", "cpx41", "cpx51", "cax11", "cax21", "cax31", "cax41"],
    gcp: ["e2-small", "e2-medium", "e2-standard-2", "e2-standard-4", "e2-standard-8"],
  },
  defaults: { aws: "t3.medium", hetzner: "cpx31", gcp: "e2-standard-2" },
  displayDefaults: { aws: "t3.xlarge", hetzner: "cpx41", gcp: "e2-standard-4" },
};

export const normalizeProvider = (p?: string): string =>
  p === "gcp" || p === "hetzner" ? p : "aws";

export const instanceTypesForProvider = (opts: ComputeOptions, p?: string): string[] =>
  opts.instanceTypes[normalizeProvider(p)] ??
  opts.instanceTypes.aws ??
  FALLBACK_COMPUTE_OPTIONS.instanceTypes.aws;

export const defaultInstanceForProvider = (opts: ComputeOptions, p?: string): string =>
  opts.defaults[normalizeProvider(p)] ?? FALLBACK_COMPUTE_OPTIONS.defaults.aws;

export const displayDefaultForProvider = (opts: ComputeOptions, p?: string): string =>
  opts.displayDefaults[normalizeProvider(p)] ?? FALLBACK_COMPUTE_OPTIONS.displayDefaults.aws;

// Build ComputeOptions from the workspace-server /compute/metadata response.
// Returns null when the payload is not well-formed, so callers can keep the
// fallback.
export function parseComputeOptions(resp: unknown): ComputeOptions | null {
  if (!resp || typeof resp !== "object") return null;
  const {
    providers,
    instanceTypes,
    defaults,
    display_defaults,
  } = resp as {
    providers?: unknown;
    instanceTypes?: unknown;
    defaults?: unknown;
    display_defaults?: unknown;
  };

  if (!Array.isArray(providers) || providers.length === 0) return null;
  if (!instanceTypes || typeof instanceTypes !== "object") return null;
  if (!defaults || typeof defaults !== "object") return null;
  if (!display_defaults || typeof display_defaults !== "object") return null;

  const providerIds: string[] = [];
  for (const p of providers) {
    if (typeof p === "string" && p) providerIds.push(p);
  }
  if (providerIds.length === 0) return null;

  const pickStrings = (obj: unknown): Record<string, string> => {
    if (!obj || typeof obj !== "object") return {};
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(obj as Record<string, unknown>)) {
      if (typeof v === "string" && v) out[k] = v;
    }
    return out;
  };

  const pickStringArrays = (obj: unknown): Record<string, string[]> => {
    if (!obj || typeof obj !== "object") return {};
    const out: Record<string, string[]> = {};
    for (const [k, v] of Object.entries(obj as Record<string, unknown>)) {
      if (Array.isArray(v)) {
        const filtered = v.filter((item): item is string => typeof item === "string" && Boolean(item));
        if (filtered.length > 0) out[k] = filtered;
      }
    }
    return out;
  };

  return {
    providers: providerIds,
    instanceTypes: pickStringArrays(instanceTypes),
    defaults: pickStrings(defaults),
    displayDefaults: pickStrings(display_defaults),
  };
}
