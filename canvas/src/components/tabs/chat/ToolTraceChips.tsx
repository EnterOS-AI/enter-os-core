import { useState } from "react";
import type { ToolTraceEntry } from "./types";

/** Render a persisted agent tool-use chain (core#2636) as a collapsible
 *  list under the agent bubble — the rehydrated twin of the live progress
 *  feed, so the chain that scrolled by during the turn is still there
 *  after a reload. Collapsed by default (a long turn can run dozens of
 *  tools); the header shows the count and toggles.
 */
export function ToolTraceChips({ trace }: { trace: ToolTraceEntry[] }) {
  const [open, setOpen] = useState(false);
  if (!trace.length) return null;
  const n = trace.length;
  return (
    <div className="mt-1.5 border-t border-line/60 dark:border-zinc-600/60 pt-1">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-1 text-[10px] text-ink-mid hover:text-ink transition-colors"
        aria-expanded={open}
      >
        <span className="font-mono">{open ? "▾" : "▸"}</span>
        <span>
          {n} tool{n === 1 ? "" : "s"} used
        </span>
      </button>
      {open && (
        <ul className="mt-1 space-y-0.5">
          {trace.map((t, i) => (
            <li
              key={`${t.tool}-${i}`}
              className="font-mono text-[10px] text-ink-mid leading-snug break-all"
            >
              🛠 {formatTool(t)}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/** Mirror the live feed's "🛠 <tool>(…)" shape. The runtime already
 *  truncates `input` to 500 chars at capture; we trim it further for the
 *  inline chip and never render raw secrets (input is a stringified
 *  preview, not the live arg values). */
function formatTool(t: ToolTraceEntry): string {
  const tool = t.tool.trim();
  const input = (t.input ?? "").trim();
  // Reconstructed entries (agent_log source) already carry the full
  // "name(…)" summary and no input — render as-is. Tool-trace-column
  // entries carry a bare tool name + an input preview.
  if (!input) return tool;
  const preview = input.length > 60 ? `${input.slice(0, 60)}…` : input;
  return `${tool}(${preview})`;
}
