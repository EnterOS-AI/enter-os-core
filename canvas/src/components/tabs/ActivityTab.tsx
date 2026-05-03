"use client";

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";
import { ConversationTraceModal } from "@/components/ConversationTraceModal";
import { type ActivityEntry } from "@/types/activity";
import { useWorkspaceName } from "@/hooks/useWorkspaceName";
import { inferA2AErrorHint } from "./chat/a2aErrorHint";

interface Props {
  workspaceId: string;
}

type FilterType = "all" | "a2a_receive" | "a2a_send" | "task_update" | "agent_log" | "skill_promotion" | "error";

const FILTERS: { id: FilterType; label: string; icon: string }[] = [
  { id: "all", label: "All", icon: "●" },
  { id: "a2a_receive", label: "A2A In", icon: "↙" },
  { id: "a2a_send", label: "A2A Out", icon: "↗" },
  { id: "task_update", label: "Tasks", icon: "◆" },
  { id: "skill_promotion", label: "Skill Promo", icon: "★" },
  { id: "agent_log", label: "Logs", icon: "▸" },
  { id: "error", label: "Errors", icon: "!" },
];

const TYPE_COLORS: Record<string, { text: string; bg: string; border: string }> = {
  a2a_receive: { text: "text-accent", bg: "bg-blue-950/30", border: "border-blue-800/30" },
  a2a_send: { text: "text-cyan-400", bg: "bg-cyan-950/30", border: "border-cyan-800/30" },
  task_update: { text: "text-warm", bg: "bg-amber-950/30", border: "border-amber-800/30" },
  skill_promotion: { text: "text-violet-300", bg: "bg-violet-950/30", border: "border-violet-800/30" },
  agent_log: { text: "text-ink-mid", bg: "bg-surface-card/30", border: "border-line/30" },
  error: { text: "text-bad", bg: "bg-red-950/30", border: "border-red-800/30" },
};

const STATUS_ICONS: Record<string, { icon: string; color: string }> = {
  ok: { icon: "✓", color: "text-good" },
  error: { icon: "✕", color: "text-bad" },
  timeout: { icon: "⏱", color: "text-warm" },
};

export function ActivityTab({ workspaceId }: Props) {
  const [activities, setActivities] = useState<ActivityEntry[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<FilterType>("all");
  const [expanded, setExpanded] = useState<string | null>(null);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [traceOpen, setTraceOpen] = useState(false);
  const resolveName = useWorkspaceName();

  const loadActivities = useCallback(async () => {
    try {
      const typeParam = filter !== "all" ? `?type=${filter}` : "";
      const data = await api.get<ActivityEntry[]>(`/workspaces/${workspaceId}/activity${typeParam}`);
      setActivities(data);
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load activity");
    } finally {
      setLoading(false);
    }
  }, [workspaceId, filter]);

  useEffect(() => {
    setLoading(true);
    loadActivities();
  }, [loadActivities]);

  useEffect(() => {
    if (!autoRefresh) return;
    const interval = setInterval(loadActivities, 5000);
    return () => clearInterval(interval);
  }, [loadActivities, autoRefresh]);

  return (
    <div className="flex flex-col h-full">
      {/* Filter bar */}
      <div className="px-3 pt-3 pb-2 border-b border-line/40">
        <div className="flex items-center gap-1 flex-wrap">
          {FILTERS.map((f) => (
            <button
              key={f.id}
              onClick={() => setFilter(f.id)}
              aria-pressed={filter === f.id}
              className={`px-2 py-1 text-[11px] rounded-md font-medium transition-all ${
                filter === f.id
                  ? "bg-surface-card text-ink ring-1 ring-zinc-600"
                  : "text-ink-soft hover:text-ink-mid hover:bg-surface-card/60"
              }`}
            >
              <span className="mr-0.5 opacity-60">{f.icon}</span> {f.label}
            </button>
          ))}
          <div className="ml-auto flex items-center gap-2">
            <button
              onClick={() => setAutoRefresh(!autoRefresh)}
              aria-pressed={autoRefresh}
              className={`text-[11px] px-1.5 py-0.5 rounded ${
                autoRefresh ? "text-good bg-emerald-950/30" : "text-ink-soft"
              }`}
              title={autoRefresh ? "Auto-refresh ON" : "Auto-refresh OFF"}
            >
              {autoRefresh ? "⟳ Live" : "⟳ Paused"}
            </button>
            <button
              onClick={() => setTraceOpen(true)}
              className="px-2 py-1 bg-blue-900/40 hover:bg-blue-800/50 text-[11px] rounded text-accent border border-blue-800/30"
              title="View full conversation trace across all workspaces"
            >
              Full Trace
            </button>
            <button
              onClick={loadActivities}
              className="px-2 py-1 bg-surface-card hover:bg-surface-card text-[11px] rounded text-ink-mid"
            >
              Refresh
            </button>
          </div>
        </div>
        <div className="mt-1.5 text-[10px] text-ink-soft">
          {activities.length} {filter === "all" ? "activities" : filter.replace("_", " ") + " entries"}
        </div>
      </div>

      {/* Activity list */}
      <div className="flex-1 overflow-y-auto p-3 space-y-1.5">
        {loading && activities.length === 0 && (
          <div className="text-xs text-ink-soft text-center py-8">Loading activity...</div>
        )}

        {error && (
          <div className="px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
            {error}
          </div>
        )}

        {!loading && !error && activities.length === 0 && (
          <div className="text-center py-8">
            <div className="text-ink-soft text-xs">No activity recorded yet</div>
            <div className="text-ink-soft text-[9px] mt-1">
              Activity logs appear when agents communicate or perform tasks
            </div>
          </div>
        )}

        {activities.map((entry) => (
          <ActivityRow
            key={entry.id}
            entry={entry}
            expanded={expanded === entry.id}
            onToggle={() => setExpanded(expanded === entry.id ? null : entry.id)}
            resolveName={resolveName}
          />
        ))}
      </div>

      <ConversationTraceModal
        open={traceOpen}
        workspaceId={workspaceId}
        onClose={() => setTraceOpen(false)}
      />
    </div>
  );
}

function ActivityRow({
  entry,
  expanded,
  onToggle,
  resolveName,
}: {
  entry: ActivityEntry;
  expanded: boolean;
  onToggle: () => void;
  resolveName: (id: string | null) => string;
}) {
  const typeStyle = TYPE_COLORS[entry.activity_type] || TYPE_COLORS.agent_log;
  const statusStyle = STATUS_ICONS[entry.status] || STATUS_ICONS.ok;
  const isA2A = entry.activity_type.startsWith("a2a_");
  const isError = entry.status === "error";

  return (
    <div
      className={`rounded-lg border transition-colors ${
        isError
          ? "bg-red-950/20 border-red-900/30"
          : "bg-surface-card/60 border-line/40"
      }`}
    >
      <button type="button" onClick={onToggle} className="w-full text-left px-3 py-2">
        {/* Top row: type badge + method + time */}
        <div className="flex items-center gap-2">
          <span className={`text-[8px] font-mono px-1.5 py-0.5 rounded ${typeStyle.text} ${typeStyle.bg} border ${typeStyle.border}`}>
            {formatType(entry.activity_type)}
          </span>

          {entry.method && (
            <span className="text-[10px] font-mono text-ink-mid truncate">
              {entry.method}
            </span>
          )}

          <span className={`text-[9px] ml-auto shrink-0 ${statusStyle.color}`}>
            {statusStyle.icon}
          </span>

          {entry.duration_ms != null && (
            <span className="text-[8px] text-ink-soft font-mono tabular-nums shrink-0">
              {entry.duration_ms}ms
            </span>
          )}

          <span className="text-[8px] text-ink-soft shrink-0">
            {formatTime(entry.created_at)}
          </span>

          <span className="text-[9px] text-ink-soft">
            {expanded ? "▼" : "▶"}
          </span>
        </div>

        {/* Summary — replace raw IDs with workspace names */}
        {entry.summary && (
          <div className="text-[10px] text-ink-mid mt-1 truncate">
            {entry.summary
              .replace(entry.source_id || "", resolveName(entry.source_id))
              .replace(entry.target_id || "", resolveName(entry.target_id))}
          </div>
        )}

        {/* A2A flow indicator */}
        {isA2A && (entry.source_id || entry.target_id) && (
          <div className="flex items-center gap-1 mt-1">
            {entry.source_id && (
              <span className="text-[9px] text-cyan-400/80 truncate max-w-[140px]" title={entry.source_id}>
                {resolveName(entry.source_id)}
              </span>
            )}
            <span className="text-[9px] text-ink-soft">→</span>
            {entry.target_id && (
              <span className="text-[9px] text-accent/80 truncate max-w-[140px]" title={entry.target_id}>
                {resolveName(entry.target_id)}
              </span>
            )}
          </div>
        )}

        {/* Error detail */}
        {isError && entry.error_detail && (
          <div className="text-[9px] text-bad/80 mt-1 truncate">
            {entry.error_detail}
          </div>
        )}
      </button>

      {/* Expanded details */}
      {expanded && (
        <div className="px-3 pb-3 space-y-2 border-t border-line/30 mt-1 pt-2">
          {entry.source_id && (
            <Detail label="Source" value={`${resolveName(entry.source_id)} (${entry.source_id.slice(0, 8)})`} />
          )}
          {entry.target_id && (
            <Detail label="Target" value={`${resolveName(entry.target_id)} (${entry.target_id.slice(0, 8)})`} />
          )}
          {/* Message preview — extract text from A2A request/response */}
          {entry.request_body && (
            <MessagePreview label="Message Sent" body={entry.request_body} />
          )}
          {entry.response_body && (
            <MessagePreview label="Response" body={entry.response_body} />
          )}
          {entry.error_detail && (
            <Detail label="Error" value={entry.error_detail} error />
          )}
          {entry.request_body && (
            <JsonBlock label="Raw Request" data={entry.request_body} />
          )}
          {entry.response_body && (
            <JsonBlock label="Response" data={entry.response_body} />
          )}
          <div className="text-[8px] text-ink-soft font-mono select-all">
            ID: {entry.id}
          </div>
        </div>
      )}
    </div>
  );
}

const A2A_ERROR_PREFIX = "[A2A_ERROR]";

/** Render a [A2A_ERROR]-prefixed response as a structured error block
 *  with a stripped detail line + a cause hint. The previous raw render
 *  ("[A2A_ERROR] " literal in the response area) gave the user no
 *  signal to act on. */
function A2AErrorPreview({ label, raw }: { label: string; raw: string }) {
  const detail = raw.slice(A2A_ERROR_PREFIX.length).trim() || "(no detail provided)";
  const hint = inferA2AErrorHint(detail);
  return (
    <div>
      <div className="text-[8px] text-bad/80 uppercase tracking-wider mb-1">{label} — delivery failed</div>
      <div className="text-[10px] text-bad bg-red-950/30 border border-red-800/40 rounded p-2 space-y-1.5">
        <div className="font-mono whitespace-pre-wrap break-words max-h-32 overflow-y-auto">{detail}</div>
        <div className="text-[9px] text-bad/70 leading-relaxed border-t border-red-800/30 pt-1.5">{hint}</div>
      </div>
    </div>
  );
}

/** Extract human-readable text from A2A request/response JSON */
function MessagePreview({ label, body }: { label: string; body: Record<string, unknown> }) {
  // Try to extract text from A2A message parts
  let text = "";
  try {
    // Simple formats from MCP server: {task: "..."} or {result: "..."}
    if (body.task && typeof body.task === "string") { text = body.task; }
    if (!text && body.result && typeof body.result === "string") { text = body.result; }
    if (text) {
      // [A2A_ERROR]-prefixed responses get the structured error
      // treatment. Bare text fallthrough renders a bland gray block
      // — fine for normal replies, terrible for "[A2A_ERROR] " with
      // no further context. Detect at the top of the rendering path
      // so it short-circuits before the generic preview kicks in.
      if (text.trimStart().startsWith(A2A_ERROR_PREFIX)) {
        return <A2AErrorPreview label={label} raw={text.trimStart()} />;
      }
      return (
        <div>
          <div className="text-[8px] text-ink-soft uppercase tracking-wider mb-1">{label}</div>
          <div className="text-[10px] text-ink-mid bg-surface-sunken/60 rounded p-2 max-h-32 overflow-y-auto whitespace-pre-wrap break-words">
            {text.slice(0, 2000)}
          </div>
        </div>
      );
    }

    // Request: params.message.parts[].text
    const params = body.params as Record<string, unknown> | undefined;
    const message = params?.message as Record<string, unknown> | undefined;
    const parts = (message?.parts || []) as Array<Record<string, unknown>>;
    text = parts
      .map((p) => (p.text as string) || (p.kind === "text" ? (p.text as string) : ""))
      .filter(Boolean)
      .join("\n");

    // Response: result.parts[].text
    if (!text) {
      const result = body.result as Record<string, unknown> | undefined;
      const rParts = (result?.parts || []) as Array<Record<string, unknown>>;
      text = rParts
        .map((p) => {
          if (p.text) return p.text as string;
          const root = p.root as Record<string, unknown> | undefined;
          return (root?.text as string) || "";
        })
        .filter(Boolean)
        .join("\n");
    }

    // Fallback: result as string
    if (!text && typeof body.result === "string") {
      text = body.result;
    }
  } catch {
    return null;
  }

  if (!text) return null;

  return (
    <div>
      <div className="text-[8px] text-ink-soft uppercase tracking-wider mb-1">{label}</div>
      <div className="text-[10px] text-ink-mid bg-surface-sunken/60 rounded p-2 max-h-32 overflow-y-auto whitespace-pre-wrap break-words">
        {text.slice(0, 2000)}
      </div>
    </div>
  );
}

function Detail({ label, value, mono, error: isError }: { label: string; value: string; mono?: boolean; error?: boolean }) {
  return (
    <div className="flex items-start gap-2">
      <span className="text-[8px] text-ink-soft uppercase tracking-wider w-14 shrink-0 pt-0.5">{label}</span>
      <span className={`text-[9px] break-all ${isError ? "text-bad" : "text-ink-mid"} ${mono ? "font-mono" : ""}`}>
        {value}
      </span>
    </div>
  );
}

function JsonBlock({ label, data }: { label: string; data: Record<string, unknown> }) {
  return (
    <div>
      <div className="text-[8px] text-ink-soft uppercase tracking-wider mb-1">{label}</div>
      <pre className="text-[9px] text-ink-mid bg-surface-sunken/80 rounded p-2 overflow-x-auto max-h-48 font-mono">
        {JSON.stringify(data, null, 2)}
      </pre>
    </div>
  );
}

function formatType(type: string): string {
  switch (type) {
    case "a2a_receive": return "A2A IN";
    case "a2a_send": return "A2A OUT";
    case "task_update": return "TASK";
    case "skill_promotion": return "PROMO";
    case "agent_log": return "LOG";
    case "error": return "ERROR";
    default: return type.toUpperCase();
  }
}

function formatTime(iso: string): string {
  const d = new Date(iso);
  const now = new Date();
  const diff = now.getTime() - d.getTime();

  if (diff < 60_000) return `${Math.floor(diff / 1000)}s`;
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m`;
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h`;
  return d.toLocaleDateString();
}
