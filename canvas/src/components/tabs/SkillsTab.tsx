"use client";

import { useMemo, useState, useEffect, useCallback, useRef } from "react";
import { api } from "@/lib/api";
import { useCanvasStore, summarizeWorkspaceCapabilities, type WorkspaceNodeData } from "@/store/canvas";
import { showToast } from "../Toaster";

interface Props {
  // The workspace's id is NOT a field on WorkspaceNodeData — that
  // interface is the React Flow `node.data` blob, while the id lives
  // on `node.id`. Pass it explicitly (matches every other tab in
  // SidePanel) so the install/uninstall API calls don't end up
  // POSTing to /workspaces/undefined/plugins. The interface extending
  // Record<string, unknown> meant TypeScript silently typed
  // `data.id` as `unknown` instead of erroring — easy to miss.
  workspaceId: string;
  data: WorkspaceNodeData;
}

interface SkillEntry {
  id: string;
  name: string;
  description: string;
  tags: string[];
  examples: string[];
}

interface PluginInfo {
  name: string;
  version: string;
  description: string;
  author: string;
  tags: string[];
  skills: string[];
  // Declared supported runtimes (e.g. ["claude_code", "deepagents"]).
  // Empty / absent = "unspecified, try it".
  runtimes?: string[];
  // Only present on /workspaces/:id/plugins responses — true if the
  // plugin declared support for the workspace's current runtime (or
  // declared no runtimes at all). Lets us grey out inert installs.
  supported_on_runtime?: boolean;
}

interface SourceSchemesResponse {
  schemes: string[];
}

// Delay before reloading installed plugins after install/uninstall (workspace restarts)
const PLUGIN_RELOAD_DELAY_MS = 15_000;

export function SkillsTab({ workspaceId, data }: Props) {
  const capability = summarizeWorkspaceCapabilities(data);
  const skills = useMemo(() => extractSkills(data.agentCard), [data.agentCard]);
  const setPanelTab = useCanvasStore((s) => s.setPanelTab);
  const promotionTask = data.currentTask.startsWith("Skill promotion:");

  const [registry, setRegistry] = useState<PluginInfo[]>([]);
  const [installed, setInstalled] = useState<PluginInfo[]>([]);
  const [sourceSchemes, setSourceSchemes] = useState<string[]>([]);
  const [installing, setInstalling] = useState<string | null>(null);
  const [uninstalling, setUninstalling] = useState<string | null>(null);
  const [showRegistry, setShowRegistry] = useState(false);
  const [customSource, setCustomSource] = useState("");
  const mountedRef = useRef(true);
  const reloadTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);

  useEffect(() => {
    // Re-init `mountedRef.current = true` on every mount. React 18
    // StrictMode (Next.js dev) double-invokes effects: mount →
    // cleanup → mount. Without this re-init, the first cleanup sets
    // mountedRef.current = false, the re-mount runs the effect body
    // again but never restores the flag, so every subsequent
    // `if (mountedRef.current) setX(...)` guard skips and the
    // component appears wedged: fetches complete, state never
    // updates, "Loading…" sits forever. Production doesn't double-
    // invoke so the bug only surfaces in dev — but dev is where we
    // see it, and the cost of being explicit is one assignment.
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      clearTimeout(reloadTimerRef.current);
    };
  }, []);

  // Tracks whether loadInstalled has completed at least once (success
  // or empty-array success — NOT failure). Without this the auto-
  // expand effect below would fire on the initial render where
  // `installed.length === 0` simply because the fetch hasn't returned
  // yet, and worse, would also fire if the fetch throws (network
  // blip, auth failure) — both cases falsely look like "no plugins
  // installed". Gating on a separate "loaded" flag avoids the false
  // positive.
  const [installedLoaded, setInstalledLoaded] = useState(false);

  const loadInstalled = useCallback(async () => {
    try {
      const result = await api.get<PluginInfo[]>(`/workspaces/${workspaceId}/plugins`);
      if (mountedRef.current) {
        setInstalled(Array.isArray(result) ? result : []);
        setInstalledLoaded(true);
      }
    } catch (e) {
      console.warn("SkillsTab: installed plugins load failed", e);
    }
  }, [workspaceId]);

  // registry-load lifecycle so the UI can show "Loading…" / error /
  // retry instead of an indistinguishable "No plugins in registry"
  // banner whether the fetch is in-flight, errored, or genuinely
  // returned []. The previous silent console.warn-only path made
  // an auth failure or CORS blip look identical to an empty
  // registry — exactly the diagnosis dead-end observed when the
  // server returned 20 plugins via curl but the canvas showed 0.
  const [registryLoading, setRegistryLoading] = useState(false);
  const [registryError, setRegistryError] = useState<string | null>(null);

  // Synchronous gate against concurrent loadRegistry runs. Refs survive
  // Fast Refresh re-renders (ref objects persist across re-runs of
  // the function body), so a previously-stranded fetch can pin this
  // ref at true and block every subsequent loadRegistry call. The
  // `force` parameter on loadRegistry below provides the user-driven
  // escape hatch for that wedge.
  const registryFetchInFlight = useRef(false);

  // Reset the in-flight gate on unmount so a Fast Refresh that
  // tears down + recreates the component without a full page reload
  // doesn't carry the stuck-true value into the new instance via
  // dev-server-preserved module state.
  useEffect(() => {
    return () => {
      registryFetchInFlight.current = false;
    };
  }, []);

  const loadRegistry = useCallback(async (force = false) => {
    // Default callers (mount effect, button while not loading) honour
    // the gate. Explicit force=true callers (Retry button) bypass it
    // — the user is signalling "forget whatever you thought was in
    // flight, fetch again now".
    if (!force && registryFetchInFlight.current) return;
    registryFetchInFlight.current = true;
    setRegistryLoading(true);
    setRegistryError(null);
    try {
      // 10s timeout — tighter than the 15s default. Plugin registry
      // is local-disk-backed on the platform host (server reads
      // pluginsDir entries) so a 10s budget is generous. Without
      // an explicit timeout the UI's "Loading registry…" can sit
      // for the full 15s + any browser hop time when a Fast
      // Refresh strands an in-flight promise.
      const result = await api.get<PluginInfo[]>("/plugins", { timeoutMs: 10_000 });
      if (mountedRef.current) setRegistry(Array.isArray(result) ? result : []);
    } catch (e) {
      console.warn("SkillsTab: registry load failed", e);
      if (mountedRef.current) {
        // Detect timeout/abort by DOMException.name first — that's
        // the canonical signal across browsers. Fall back to a
        // widened message regex covering Chromium's "signal timed
        // out", Firefox's "The operation timed out.", Safari's
        // "Aborted". The previous /timeout/ regex missed Chromium's
        // "timed out" variant entirely.
        const name = (e as { name?: string })?.name ?? "";
        const msg = e instanceof Error ? e.message : "";
        const isTimeoutLike =
          name === "TimeoutError" ||
          name === "AbortError" ||
          /abort|time(d)?\s*out/i.test(msg);
        setRegistryError(
          isTimeoutLike
            ? "Registry fetch timed out (10s). The platform server may be slow or unreachable."
            : msg || "Failed to load registry",
        );
      }
    } finally {
      registryFetchInFlight.current = false;
      if (mountedRef.current) setRegistryLoading(false);
    }
  }, []);

  const loadSourceSchemes = useCallback(async () => {
    try {
      const result = await api.get<SourceSchemesResponse>("/plugins/sources");
      if (mountedRef.current) setSourceSchemes(result.schemes ?? []);
    } catch (e) {
      console.warn("SkillsTab: plugin sources load failed", e);
      // Falls back to "local only" UX — non-fatal.
    }
  }, []);

  useEffect(() => {
    loadInstalled();
    loadRegistry();
    loadSourceSchemes();
  }, [loadInstalled, loadRegistry, loadSourceSchemes]);

  // First-time experience: if the workspace has zero plugins
  // installed but the platform's registry has options to choose
  // from, expand the registry by default so the user sees what's
  // available without an extra click. Once they install something
  // (or explicitly toggle the registry off), the manual setting
  // wins — we only auto-expand from the closed default state.
  const hasAutoExpandedRef = useRef(false);
  useEffect(() => {
    if (hasAutoExpandedRef.current) return;
    if (installedLoaded && installed.length === 0 && registry.length > 0) {
      setShowRegistry(true);
      hasAutoExpandedRef.current = true;
    }
  }, [installedLoaded, installed.length, registry.length]);

  const installedNames = useMemo(() => new Set(installed.map((p) => p.name)), [installed]);

  // Install always goes through the source-based API. For registry
  // plugins we build the local:// source on the fly; custom sources
  // (github://, clawhub://, …) are typed into the input below.
  //
  // Optional `optimistic` parameter mirrors the uninstall flow's local
  // state mutation. Without it, the user sees the button revert from
  // "Installing..." → "Install" the instant the POST returns, and the
  // green "Installed" tag doesn't appear for ~15s while we wait out
  // PLUGIN_RELOAD_DELAY_MS for the workspace restart before refetching.
  // 15s of staring at the same button feels broken. Pushing the
  // registry entry into `installed` immediately makes the UI reflect
  // the install instantly; the delayed loadInstalled() reconciles
  // anything we got wrong (or any server-side filtering we don't
  // know about locally).
  const installFromSource = async (
    source: string,
    labelOverride?: string,
    optimistic?: PluginInfo,
  ) => {
    const label = labelOverride ?? source;
    setInstalling(label);
    try {
      await api.post(`/workspaces/${workspaceId}/plugins`, { source });
      showToast(`Installed ${label} — restarting workspace`, "success");
      if (optimistic && mountedRef.current) {
        // Push with `supported_on_runtime` left undefined — the
        // server's ListInstalled annotates the real value (true /
        // false) at refetch time. Forcing `true` here would hide the
        // "inert on this runtime" badge for 15s if the user
        // installed a plugin that doesn't actually support the
        // workspace's runtime; the badge only renders on `=== false`,
        // so undefined keeps it neutral until reconciliation arrives.
        setInstalled((prev) =>
          prev.some((p) => p.name === optimistic.name)
            ? prev
            : [...prev, { ...optimistic, supported_on_runtime: undefined }],
        );
        // Note: we intentionally do NOT set `installedLoaded` here.
        // That flag means "the initial GET has succeeded at least
        // once" and gates the auto-expand-registry effect. A fast
        // optimistic install BEFORE the initial fetch returns must
        // not flip the gate, or the auto-expand never fires and a
        // followup loadInstalled racing with the optimistic write
        // could overwrite our entry with [] mid-restart.
      }
      // Drop any prior reload timer before scheduling a new one —
      // back-to-back installs within PLUGIN_RELOAD_DELAY_MS would
      // otherwise queue multiple loadInstalled() calls and the
      // unmount cleanup only clears the latest handle.
      clearTimeout(reloadTimerRef.current);
      reloadTimerRef.current = setTimeout(() => loadInstalled(), PLUGIN_RELOAD_DELAY_MS);
    } catch (e) {
      showToast(e instanceof Error ? e.message : "Install failed", "error");
    } finally {
      setInstalling(null);
    }
  };

  const handleInstall = (pluginName: string) => {
    const entry = registry.find((p) => p.name === pluginName);
    return installFromSource(`local://${pluginName}`, pluginName, entry);
  };

  const handleInstallCustom = async () => {
    const source = customSource.trim();
    if (!source) return;
    await installFromSource(source);
    setCustomSource("");
  };

  const handleUninstall = async (pluginName: string) => {
    setUninstalling(pluginName);
    try {
      await api.del(`/workspaces/${workspaceId}/plugins/${pluginName}`);
      showToast(`Removed ${pluginName} — restarting workspace`, "success");
      setInstalled((prev) => prev.filter((p) => p.name !== pluginName));
      // Drop any prior reload timer (see installFromSource for the
      // back-to-back-action leak rationale).
      clearTimeout(reloadTimerRef.current);
      reloadTimerRef.current = setTimeout(() => loadInstalled(), PLUGIN_RELOAD_DELAY_MS);
    } catch (e) {
      showToast(e instanceof Error ? e.message : "Uninstall failed", "error");
    } finally {
      setUninstalling(null);
    }
  };

  return (
    <div className="p-4 space-y-4">
      {/* Plugins section */}
      <div className="rounded-xl border border-line bg-surface-sunken/70 p-3">
        <div className="flex items-center justify-between gap-3">
          <div>
            <div className="text-[10px] uppercase tracking-[0.22em] text-ink-soft">Plugins</div>
            <h3 className="mt-1 text-sm font-semibold text-ink">
              {installed.length} installed
            </h3>
          </div>
          <button
            onClick={() => setShowRegistry(!showRegistry)}
            className="rounded-full border border-violet-700/50 bg-violet-950/30 px-3 py-1 text-[10px] text-violet-200 hover:bg-violet-900/40 transition-colors"
          >
            {showRegistry ? "Hide Registry" : "+ Install Plugin"}
          </button>
        </div>

        {/* Installed plugins */}
        {installed.length > 0 && (
          <div className="mt-3 space-y-1.5">
            {installed.map((p) => {
              // Plugin was installed but does NOT declare support for
              // the workspace's current runtime — grey it out so users
              // see it's inert. Happens after a runtime change or when
              // someone installs a runtime-specific plugin on a wrong
              // workspace.
              const inert = p.supported_on_runtime === false;
              return (
                <div
                  key={p.name}
                  className={`flex items-center justify-between gap-2 rounded-lg border px-3 py-2 ${
                    inert
                      ? "border-amber-800/40 bg-amber-950/10 opacity-70"
                      : "border-line/60 bg-surface/40"
                  }`}
                >
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="text-[11px] font-medium text-ink">{p.name}</span>
                      {p.version && <span className="text-[10px] text-ink-soft">v{p.version}</span>}
                      {inert && (
                        <span className="rounded-full border border-amber-700/50 bg-amber-950/30 px-1.5 py-0.5 text-[10px] text-warm">
                          inert on this runtime
                        </span>
                      )}
                    </div>
                    {p.description && <div className="text-[10px] text-ink-soft truncate">{p.description}</div>}
                    {p.skills && p.skills.length > 0 && (
                      <div className="mt-1 flex flex-wrap gap-1">
                        {p.skills.slice(0, 4).map((s) => (
                          <span key={s} className="rounded-full bg-surface-card/60 px-1.5 py-0.5 text-[10px] text-ink-mid">{s}</span>
                        ))}
                        {p.skills.length > 4 && (
                          <span className="text-[10px] text-ink-soft">+{p.skills.length - 4}</span>
                        )}
                      </div>
                    )}
                  </div>
                  <button
                    onClick={() => handleUninstall(p.name)}
                    disabled={uninstalling === p.name}
                    className="shrink-0 rounded-full border border-red-800/40 bg-red-950/20 px-2 py-0.5 text-[11px] text-bad hover:bg-red-900/30 disabled:opacity-30"
                  >
                    {uninstalling === p.name ? "..." : "Remove"}
                  </button>
                </div>
              );
            })}
          </div>
        )}

        {/* Plugin registry (expandable) */}
        {showRegistry && (
          <div className="mt-3 border-t border-line/40 pt-3">
            {/* Install from any source (github://, clawhub://, …) */}
            <div className="mb-3 rounded-lg border border-line/60 bg-surface/40 p-2.5">
              <div className="flex items-center justify-between gap-2 mb-1.5">
                <div className="text-[10px] uppercase tracking-[0.2em] text-ink-soft">
                  Install from source
                </div>
                {sourceSchemes.length > 0 && (
                  <div className="flex flex-wrap gap-1">
                    {sourceSchemes.map((s) => (
                      <span
                        key={s}
                        className="rounded-full border border-line/50 bg-surface-sunken/50 px-1.5 py-0.5 text-[10px] text-ink-soft"
                      >
                        {s}://
                      </span>
                    ))}
                  </div>
                )}
              </div>
              <div className="flex items-center gap-1.5">
                <input
                  type="text"
                  aria-label="Install from source URL"
                  value={customSource}
                  onChange={(e) => setCustomSource(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" && !installing) handleInstallCustom();
                  }}
                  placeholder="e.g. github://owner/repo#v1.0"
                  spellCheck={false}
                  className="flex-1 rounded border border-line bg-surface px-2 py-1 text-[10px] text-ink placeholder:text-ink-soft focus:outline-none focus:border-violet-600 focus-visible:ring-2 focus-visible:ring-violet-600/50"
                />
                <button
                  onClick={handleInstallCustom}
                  disabled={!customSource.trim() || installing !== null}
                  className="shrink-0 rounded-full border border-violet-700/50 bg-violet-950/30 px-2.5 py-1 text-[11px] text-violet-300 hover:bg-violet-900/40 disabled:opacity-30"
                >
                  {installing === customSource.trim() ? "Installing..." : "Install"}
                </button>
              </div>
              <div className="mt-1 text-[10px] text-ink-soft">
                Local registry plugins below; paste any scheme URL above for GitHub or other sources.
              </div>
            </div>
            <div className="flex items-center justify-between mb-2">
              <div className="text-[10px] uppercase tracking-[0.2em] text-ink-soft">Available plugins</div>
              {/* Retry visible whenever registry is empty — including
                  the loading state — so a stuck fetch (Fast Refresh
                  stranded promise, slow server, browser quirk) has a
                  user-driven escape hatch. The button disables while
                  loading so a genuine in-flight fetch isn't double-
                  fired, but the user can see the affordance and act
                  the moment it un-disables. */}
              {registry.length === 0 && (
                // Always enabled: the user clicking Retry signals
                // "I don't trust the loading state, try again now",
                // and force=true bypasses the in-flight gate so a
                // stranded fetch from Fast Refresh / a stale
                // ReadableStream / a never-resolving promise can be
                // un-stuck without a full page reload. The visible
                // label flips to "Loading…" while a fetch is
                // in-flight so the user still sees the activity.
                <button
                  type="button"
                  onClick={() => loadRegistry(true)}
                  className="text-[10px] text-violet-300 hover:text-violet-200 underline-offset-2 hover:underline"
                >
                  {registryLoading ? "Loading… click to retry" : "Retry"}
                </button>
              )}
            </div>
            {registryLoading && registry.length === 0 ? (
              <div className="text-[10px] text-ink-soft">Loading registry…</div>
            ) : registryError ? (
              <div className="rounded-lg border border-red-800/40 bg-red-950/20 px-2 py-1.5">
                <div className="text-[10px] text-bad font-semibold mb-0.5">
                  Couldn't load the plugin registry
                </div>
                <div className="text-[10px] text-bad/80">{registryError}</div>
                <div className="mt-1 text-[10px] text-ink-soft">
                  Check the platform server is reachable at /plugins. The Retry button is in the header above.
                </div>
              </div>
            ) : registry.length === 0 ? (
              <div className="rounded-lg border border-line/40 bg-surface/40 px-2 py-1.5">
                <div className="text-[10px] text-ink-mid mb-0.5">Registry returned 0 plugins.</div>
                <div className="text-[10px] text-ink-soft">
                  This usually means the platform's plugins/ directory is empty.
                  Run scripts/clone-manifest.sh to populate it from the standalone repos.
                </div>
              </div>
            ) : (
              <div className="space-y-1.5">
                {registry.map((p) => {
                  const isInstalled = installedNames.has(p.name);
                  return (
                    <div key={p.name} className="flex items-center justify-between gap-2 rounded-lg border border-line/40 bg-surface/30 px-3 py-2">
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span className="text-[11px] text-ink-mid">{p.name}</span>
                          {p.version && <span className="text-[10px] text-ink-soft">v{p.version}</span>}
                        </div>
                        {p.description && <div className="text-[10px] text-ink-soft truncate">{p.description}</div>}
                        {p.tags && p.tags.length > 0 && (
                          <div className="mt-1 flex flex-wrap gap-1">
                            {p.tags.map((t) => (
                              <span key={t} className="rounded-full border border-line/40 px-1.5 py-0.5 text-[10px] text-ink-soft">{t}</span>
                            ))}
                          </div>
                        )}
                        {p.runtimes && p.runtimes.length > 0 && (
                          <div className="mt-1 flex flex-wrap gap-1">
                            {p.runtimes.map((r) => (
                              <span key={r} className="rounded-full border border-blue-800/40 bg-blue-950/20 px-1.5 py-0.5 text-[10px] text-accent">{r}</span>
                            ))}
                          </div>
                        )}
                      </div>
                      {isInstalled ? (
                        <span className="shrink-0 text-[10px] text-good">Installed</span>
                      ) : (
                        <button
                          onClick={() => handleInstall(p.name)}
                          disabled={installing === p.name}
                          className="shrink-0 rounded-full border border-violet-700/50 bg-violet-950/30 px-2.5 py-0.5 text-[11px] text-violet-300 hover:bg-violet-900/40 disabled:opacity-30"
                        >
                          {installing === p.name ? "Installing..." : "Install"}
                        </button>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        )}
      </div>

      {/* Skills section */}
      <div className="rounded-xl border border-line bg-surface-sunken/70 p-3">
        <div className="flex items-center justify-between gap-3">
          <div>
            <div className="text-[10px] uppercase tracking-[0.22em] text-ink-soft">Workspace skills</div>
            <h3 className="mt-1 text-sm font-semibold text-ink">Installed skills</h3>
          </div>
          <div className="flex flex-wrap gap-2">
            <MetaPill label="Count" value={String(capability.skillCount)} />
            <MetaPill label="Runtime" value={capability.runtime || "unknown"} />
          </div>
        </div>
        <p className="mt-2 text-[11px] leading-5 text-ink-soft">
          Live skill directory from the Agent Card — updates when the workspace hot-reloads skills.
        </p>
        <div className="mt-3 flex flex-wrap gap-2">
          <button
            onClick={() => setPanelTab("config")}
            className="rounded-full border border-line bg-surface px-3 py-1 text-[10px] text-ink-mid hover:bg-surface-sunken"
          >
            Open Config
          </button>
          <button
            onClick={() => setPanelTab("files")}
            className="rounded-full border border-line bg-surface px-3 py-1 text-[10px] text-ink-mid hover:bg-surface-sunken"
          >
            Open Files
          </button>
        </div>
      </div>

      {promotionTask && (
        <div className="rounded-xl border border-violet-800/30 bg-violet-950/20 p-3 text-[11px] text-violet-200/90">
          A skill promotion is currently in flight. The workspace is compressing a repeatable workflow into
          a new skill package.
        </div>
      )}

      {skills.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line bg-surface-sunken/40 p-6 text-center">
          <div className="text-sm text-ink">No skills loaded</div>
          <p className="mt-2 text-[11px] leading-5 text-ink-soft">
            Add skills from the Config tab, install a plugin above, or let the runtime hot-load them.
          </p>
        </div>
      ) : (
        <div className="space-y-2">
          {skills.map((skill) => (
            <div key={skill.id} className="rounded-xl border border-line bg-surface-sunken/60 p-3">
              <div className="flex items-start justify-between gap-3">
                <div>
                  <div className="text-xs font-semibold text-ink">{skill.name}</div>
                  <div className="mt-0.5 text-[10px] font-mono text-ink-soft">{skill.id}</div>
                </div>
                {skill.tags.length > 0 && (
                  <div className="flex flex-wrap justify-end gap-1.5">
                    {skill.tags.slice(0, 4).map((tag) => (
                      <span
                        key={tag}
                        className="rounded-full border border-line bg-surface-sunken px-2 py-0.5 text-[9px] text-ink-mid"
                      >
                        {tag}
                      </span>
                    ))}
                  </div>
                )}
              </div>

              {skill.description && (
                <p className="mt-2 text-[11px] leading-5 text-ink-mid">{skill.description}</p>
              )}

              {skill.examples.length > 0 && (
                <div className="mt-2">
                  <div className="text-[9px] uppercase tracking-[0.2em] text-ink-soft">Examples</div>
                  <div className="mt-1 space-y-1">
                    {skill.examples.slice(0, 2).map((example, index) => (
                      <div
                        key={`${skill.id}-${index}`}
                        className="rounded-md border border-line bg-surface/60 px-2 py-1 text-[10px] text-ink-mid"
                      >
                        {example}
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function extractSkills(agentCard: Record<string, unknown> | null): SkillEntry[] {
  if (!agentCard) return [];
  const rawSkills = agentCard.skills;
  if (!Array.isArray(rawSkills)) return [];

  return rawSkills
    .map((skill: Record<string, unknown>) => ({
      id: String(skill.id || skill.name || ""),
      name: String(skill.name || skill.id || "Unnamed skill"),
      description: String(skill.description || ""),
      tags: Array.isArray(skill.tags) ? skill.tags.map((tag) => String(tag)) : [],
      examples: Array.isArray(skill.examples) ? skill.examples.map((example) => String(example)) : [],
    }))
    .filter((skill) => skill.id.length > 0);
}

function MetaPill({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1 rounded-full border border-line/60 bg-surface/60 px-2 py-1 text-[9px] text-ink-mid">
      <span className="uppercase tracking-[0.18em] text-[8px] text-ink-soft">{label}</span>
      <span className="font-medium">{value}</span>
    </span>
  );
}
