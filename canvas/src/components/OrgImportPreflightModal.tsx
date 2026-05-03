"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { createSecret } from "@/lib/api/secrets";

/**
 * One entry from the server's preflight `required_env` / `recommended_env`.
 *
 *   - A plain string is a STRICT requirement: that exact env var must be
 *     configured.
 *   - A `{any_of: [...]}` object is an OR group: at least one member
 *     must be configured to satisfy it. Lets a template say "either
 *     ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN" without forcing
 *     both.
 *
 * Matches the Go `EnvRequirement` type's JSON shape (MarshalJSON in
 * workspace-server/internal/handlers/org.go). The union is written so
 * that a narrow check — `typeof e === "string"` — distinguishes cleanly.
 */
export type EnvRequirement = string | { any_of: string[] };

/** Flat member list for a requirement. */
export function envReqMembers(r: EnvRequirement): string[] {
  return typeof r === "string" ? [r] : r.any_of;
}

/** True if any member is present in `configured`. */
export function envReqSatisfied(r: EnvRequirement, configured: Set<string>): boolean {
  if (typeof r === "string") return configured.has(r);
  return r.any_of.some((m) => configured.has(m));
}

/** Stable react-key / dedup key for a requirement. Sorted for groups so
 *  reordered-member variants still collapse to one entry. */
export function envReqKey(r: EnvRequirement): string {
  if (typeof r === "string") return r;
  return [...r.any_of].sort().join("|");
}

interface Props {
  open: boolean;
  /** Display name of the org template — headline only. */
  orgName: string;
  /** Total workspace count so the header can read "12 workspaces". */
  workspaceCount: number;
  /** Env vars the server has declared MUST be set as global secrets.
   *  Import is disabled until every entry here is configured. Entries
   *  are either a single key name or an any-of group. */
  requiredEnv: EnvRequirement[];
  /** Env vars the server suggests — import can proceed without them,
   *  but the user sees them listed so they can decide. Same union
   *  shape as `requiredEnv`. */
  recommendedEnv: EnvRequirement[];
  /** Names of env vars already configured globally. Used to strike
   *  through entries the user has already set up in another
   *  session. Passed in rather than queried inside the modal so the
   *  parent can refresh after each save without prop-driven effects. */
  configuredKeys: Set<string>;
  /** Called after a successful secret save so the parent can refresh
   *  `configuredKeys`. */
  onSecretSaved: () => void;
  /** User clicked Import with all required envs satisfied. */
  onProceed: () => void;
  /** User dismissed the modal. Import is NOT fired. */
  onCancel: () => void;
}

interface DraftEntry {
  key: string;
  value: string;
  saving: boolean;
  error: string | null;
}

/**
 * OrgImportPreflightModal
 * -----------------------
 * Two-tier env preflight before POST /org/import:
 *
 *   - REQUIRED section (red, blocking) — every entry MUST be configured
 *     globally before the Import button enables. Matches the server-
 *     side preflight that would 412 the import anyway.
 *
 *   - RECOMMENDED section (yellow, non-blocking) — listed so the user
 *     can add them if they want the full experience, but the Import
 *     button stays enabled regardless.
 *
 * Saving goes to the GLOBAL secrets endpoint (PUT /settings/secrets)
 * because org-level templates deploy shared resources. Per-workspace
 * overrides still work via the Config tab on an individual node
 * after import. The modal does NOT enable Import the moment a key is
 * typed — only after it saves successfully (so a half-entered token
 * can't proceed and then fail at container-start time instead).
 */
export function OrgImportPreflightModal({
  open,
  orgName,
  workspaceCount,
  requiredEnv,
  recommendedEnv,
  configuredKeys,
  onSecretSaved,
  onProceed,
  onCancel,
}: Props) {
  const [drafts, setDrafts] = useState<Record<string, DraftEntry>>({});

  // Flatten the union-shaped requirement lists to the set of every key
  // that could ever appear as an input row. Used purely to seed the
  // drafts map — satisfaction semantics still read from the grouped
  // EnvRequirement entries (a group can be satisfied by any one
  // member).
  const allMemberKeys = useMemo(() => {
    const keys: string[] = [];
    for (const r of requiredEnv) keys.push(...envReqMembers(r));
    for (const r of recommendedEnv) keys.push(...envReqMembers(r));
    return keys;
  }, [requiredEnv, recommendedEnv]);

  // Seed a draft entry per declared key the first time the modal
  // opens. Entries persist across `configuredKeys` changes so a mid-
  // save recheck doesn't wipe what the user typed.
  //
  // Dep: derive a STABLE string from the env-name lists rather than
  // the array refs themselves. The parent computes
  // `preflight.org.required_env ?? []`, which produces a fresh []
  // identity on every re-render (e.g. when refreshConfiguredKeys
  // bumps state); depending on the array refs would re-fire the
  // effect on every parent render and mask any future edit that
  // drops the `if (!next[k])` guard as a silent input-reset bug.
  const envKeysSignature = useMemo(
    () => [...allMemberKeys].sort().join("|"),
    [allMemberKeys],
  );
  useEffect(() => {
    if (!open) return;
    setDrafts((prev) => {
      const next = { ...prev };
      for (const k of allMemberKeys) {
        if (!next[k]) {
          next[k] = { key: k, value: "", saving: false, error: null };
        }
      }
      return next;
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, envKeysSignature]);

  const missingRequired = useMemo(
    () => requiredEnv.filter((r) => !envReqSatisfied(r, configuredKeys)),
    [requiredEnv, configuredKeys],
  );
  const missingRecommended = useMemo(
    () => recommendedEnv.filter((r) => !envReqSatisfied(r, configuredKeys)),
    [recommendedEnv, configuredKeys],
  );
  const canProceed = missingRequired.length === 0;

  // Synchronous in-flight gate. A ref (not state) so two clicks
  // dispatched in the SAME microtask both see the gate flip — state
  // commits don't help here because setState is async. The previous
  // closure-based `current.saving` gate worked under React Testing
  // Library's act() flushing but failed for true microtask-level
  // double-fires (programmatic clicks, dblclick events, Enter-spam
  // before React commits). Set is keyed by env var name so different
  // rows can save concurrently.
  const inFlightRef = useRef<Set<string>>(new Set());

  // Latest-drafts ref so saveOne can read the current input value
  // without taking `drafts` as a useCallback dep — that dep would
  // re-create saveOne on every keystroke and re-bind every Save
  // button's onClick handler, churn that scales with row count.
  const draftsRef = useRef(drafts);
  useEffect(() => {
    draftsRef.current = drafts;
  }, [drafts]);

  const saveOne = useCallback(
    async (key: string) => {
      // Microtask-safe gate: claim the slot synchronously BEFORE any
      // await so a second click in the same tick bounces immediately.
      if (inFlightRef.current.has(key)) return;
      const current = draftsRef.current[key];
      if (!current || !current.value.trim()) return;
      inFlightRef.current.add(key);

      const startValue = current.value;
      setDrafts((d) => ({
        ...d,
        [key]: { ...d[key], saving: true, error: null },
      }));
      try {
        await createSecret("global", key, startValue);
        setDrafts((d) => ({
          ...d,
          [key]: { ...d[key], value: "", saving: false, error: null },
        }));
        // Let the parent refresh configuredKeys so the strike-through
        // updates and canProceed recomputes.
        onSecretSaved();
      } catch (e) {
        setDrafts((d) => ({
          ...d,
          [key]: {
            ...d[key],
            saving: false,
            error: e instanceof Error ? e.message : "Save failed",
          },
        }));
      } finally {
        inFlightRef.current.delete(key);
      }
    },
    [onSecretSaved],
  );

  if (!open) return null;

  // Portal the dialog to document.body so it escapes any ancestor
  // containing block. TemplatePalette renders this modal inside a
  // sidebar whose `fixed` container plus backdrop-filter together
  // re-anchor descendants' `position: fixed` to the sidebar's own
  // bounds instead of the viewport — the modal ends up glued to the
  // sidebar's scrollable region and only becomes visible after the
  // user scrolls the sidebar. Portal dodges that class of issue
  // once and for all, regardless of what future wrappers do.
  //
  // SSR-safe guard: `document` is undefined on the server. Since
  // the modal is gated by `if (!open) return null` above, this
  // effectively only runs after open flips true on the client.
  if (typeof document === "undefined") return null;

  return createPortal(
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="org-preflight-title"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70"
      onClick={onCancel}
    >
      <div
        className="w-[560px] max-h-[80vh] overflow-auto rounded-xl bg-surface-sunken border border-line shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <header className="px-5 py-4 border-b border-line">
          <h2 id="org-preflight-title" className="text-sm font-semibold text-ink">
            Deploy {orgName}
          </h2>
          <p className="mt-0.5 text-[11px] text-ink-soft">
            {workspaceCount} workspace{workspaceCount === 1 ? "" : "s"}.
            Review the credentials needed before import.
          </p>
        </header>

        <section className="p-5 space-y-5">
          {requiredEnv.length > 0 && (
            <EnvList
              tone="required"
              title="Required"
              subtitle="Import is blocked until every key below is saved globally."
              entries={requiredEnv}
              configuredKeys={configuredKeys}
              drafts={drafts}
              onChange={(key, value) =>
                setDrafts((d) => ({ ...d, [key]: { ...d[key], value } }))
              }
              onSave={saveOne}
            />
          )}
          {recommendedEnv.length > 0 && (
            <EnvList
              tone="recommended"
              title="Recommended"
              subtitle="Not required, but some features degrade without them. Add them now for the best experience."
              entries={recommendedEnv}
              configuredKeys={configuredKeys}
              drafts={drafts}
              onChange={(key, value) =>
                setDrafts((d) => ({ ...d, [key]: { ...d[key], value } }))
              }
              onSave={saveOne}
            />
          )}
          {requiredEnv.length === 0 && recommendedEnv.length === 0 && (
            <p className="text-[12px] text-ink-mid">
              No additional credentials required for this template.
            </p>
          )}
        </section>

        <footer className="px-5 py-3 border-t border-line flex items-center justify-between">
          <button
            type="button"
            onClick={onCancel}
            className="px-3 py-1.5 text-[11px] rounded bg-surface-card hover:bg-surface-card text-ink-mid"
          >
            Cancel
          </button>
          <div className="flex items-center gap-2">
            {missingRecommended.length > 0 && canProceed && (
              <span className="text-[10px] text-warm/90">
                {missingRecommended.length} recommended key
                {missingRecommended.length === 1 ? "" : "s"} still unset
              </span>
            )}
            <button
              type="button"
              onClick={onProceed}
              disabled={!canProceed}
              className="px-4 py-1.5 text-[11px] font-semibold rounded bg-accent-strong hover:bg-accent text-white disabled:bg-surface-card disabled:text-white-soft disabled:cursor-not-allowed"
            >
              Import
            </button>
          </div>
        </footer>
      </div>
    </div>,
    document.body,
  );
}

interface EnvListProps {
  tone: "required" | "recommended";
  title: string;
  subtitle: string;
  entries: EnvRequirement[];
  configuredKeys: Set<string>;
  drafts: Record<string, DraftEntry>;
  onChange: (key: string, value: string) => void;
  onSave: (key: string) => void;
}

function EnvList({
  tone,
  title,
  subtitle,
  entries,
  configuredKeys,
  drafts,
  onChange,
  onSave,
}: EnvListProps) {
  const accent =
    tone === "required"
      ? "border-red-800/60 bg-red-950/20"
      : "border-amber-800/50 bg-amber-950/15";
  const headerColor =
    tone === "required" ? "text-bad" : "text-warm";

  return (
    <div className={`rounded-lg border ${accent} p-3`}>
      <h3 className={`text-[11px] font-semibold uppercase tracking-wide ${headerColor}`}>
        {title}
      </h3>
      <p className="mt-0.5 mb-2 text-[10px] text-ink-mid">{subtitle}</p>
      <ul className="space-y-2">
        {entries.map((entry) =>
          typeof entry === "string" ? (
            <StrictEnvRow
              key={envReqKey(entry)}
              envKey={entry}
              configured={configuredKeys.has(entry)}
              draft={drafts[entry]}
              onChange={onChange}
              onSave={onSave}
            />
          ) : (
            <AnyOfEnvGroup
              key={envReqKey(entry)}
              members={entry.any_of}
              configuredKeys={configuredKeys}
              drafts={drafts}
              onChange={onChange}
              onSave={onSave}
            />
          ),
        )}
      </ul>
    </div>
  );
}

interface StrictEnvRowProps {
  envKey: string;
  configured: boolean;
  draft: DraftEntry | undefined;
  onChange: (key: string, value: string) => void;
  onSave: (key: string) => void;
}

function StrictEnvRow({
  envKey,
  configured,
  draft: d,
  onChange,
  onSave,
}: StrictEnvRowProps) {
  return (
    <li className="flex items-center gap-2 rounded bg-surface-sunken/70 border border-line px-2 py-1.5">
      <code
        className={`text-[11px] font-mono flex-1 ${
          configured ? "text-ink-soft line-through" : "text-ink"
        }`}
      >
        {envKey}
      </code>
      {configured ? (
        <span className="text-[10px] text-good">✓ set</span>
      ) : (
        <>
          <input
            type="password"
            aria-label={`Value for ${envKey}`}
            placeholder="paste value"
            value={d?.value ?? ""}
            onChange={(e) => onChange(envKey, e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                onSave(envKey);
              }
            }}
            disabled={d?.saving}
            className="flex-1 px-2 py-1 rounded bg-surface-card border border-line text-[11px] text-ink focus:outline-none focus:border-accent disabled:opacity-50"
          />
          <button
            type="button"
            onClick={() => onSave(envKey)}
            disabled={d?.saving || !d?.value.trim()}
            className="px-2 py-1 text-[10px] rounded bg-accent-strong hover:bg-accent text-white disabled:opacity-40 disabled:cursor-not-allowed"
          >
            {d?.saving ? "…" : "Save"}
          </button>
        </>
      )}
      {d?.error && (
        <span className="text-[9px] text-bad basis-full pl-1">
          {d.error}
        </span>
      )}
    </li>
  );
}

interface AnyOfEnvGroupProps {
  members: string[];
  configuredKeys: Set<string>;
  drafts: Record<string, DraftEntry>;
  onChange: (key: string, value: string) => void;
  onSave: (key: string) => void;
}

/**
 * Renders an OR group: the user only needs to configure ONE of the
 * members to satisfy the requirement. Once any member is configured
 * the group shows a green banner identifying the satisfying key; the
 * other inputs remain visible but muted so the user can still switch
 * providers if they want (uncommon but cheap to support).
 */
function AnyOfEnvGroup({
  members,
  configuredKeys,
  drafts,
  onChange,
  onSave,
}: AnyOfEnvGroupProps) {
  const satisfiedBy = members.find((m) => configuredKeys.has(m));
  return (
    <li className="rounded border border-line bg-surface-sunken/50 px-2.5 py-2">
      <div className="flex items-center justify-between mb-1.5">
        <span className="text-[10px] uppercase tracking-wide text-ink-mid">
          Configure any one
        </span>
        {satisfiedBy && (
          <span className="text-[10px] text-good">
            ✓ using <code className="font-mono">{satisfiedBy}</code>
          </span>
        )}
      </div>
      <ul className="space-y-1.5">
        {members.map((m) => {
          const isConfigured = configuredKeys.has(m);
          const d = drafts[m];
          const dimmed = !!satisfiedBy && !isConfigured;
          return (
            <li
              key={m}
              className={`flex items-center gap-2 rounded bg-surface-sunken/70 border border-line px-2 py-1 ${
                dimmed ? "opacity-50" : ""
              }`}
            >
              <code
                className={`text-[11px] font-mono flex-1 ${
                  isConfigured ? "text-ink-soft line-through" : "text-ink"
                }`}
              >
                {m}
              </code>
              {isConfigured ? (
                <span className="text-[10px] text-good">✓ set</span>
              ) : (
                <>
                  <input
                    type="password"
                    aria-label={`Value for ${m}`}
                    placeholder="paste value"
                    value={d?.value ?? ""}
                    onChange={(e) => onChange(m, e.target.value)}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") {
                        e.preventDefault();
                        onSave(m);
                      }
                    }}
                    disabled={d?.saving}
                    className="flex-1 px-2 py-1 rounded bg-surface-card border border-line text-[11px] text-ink focus:outline-none focus:border-accent disabled:opacity-50"
                  />
                  <button
                    type="button"
                    onClick={() => onSave(m)}
                    disabled={d?.saving || !d?.value.trim()}
                    className="px-2 py-1 text-[10px] rounded bg-accent-strong hover:bg-accent text-white disabled:opacity-40 disabled:cursor-not-allowed"
                  >
                    {d?.saving ? "…" : "Save"}
                  </button>
                </>
              )}
              {d?.error && (
                <span className="text-[9px] text-bad basis-full pl-1">
                  {d.error}
                </span>
              )}
            </li>
          );
        })}
      </ul>
    </li>
  );
}
