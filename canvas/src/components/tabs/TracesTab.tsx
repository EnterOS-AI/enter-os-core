"use client";

import { useState, useEffect, useCallback } from "react";
import { api } from "@/lib/api";

interface Props {
  workspaceId: string;
}

interface Trace {
  id: string;
  name: string;
  timestamp: string;
  latency?: number;
  input?: Record<string, unknown> | string;
  output?: Record<string, unknown> | string;
  status?: string;
  totalCost?: number;
  usage?: {
    input?: number;
    output?: number;
    total?: number;
  };
}

export function TracesTab({ workspaceId }: Props) {
  const [traces, setTraces] = useState<Trace[]>([]);
  const [loading, setLoading] = useState(true);
  const [expanded, setExpanded] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  const loadTraces = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await api.get<{ data?: Trace[] }>(`/workspaces/${workspaceId}/traces`);
      setTraces(res.data || []);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load traces");
      setTraces([]);
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    loadTraces();
  }, [loadTraces]);

  if (loading) {
    return <div className="p-4 text-xs text-ink-soft">Loading traces...</div>;
  }

  return (
    <div className="p-4 space-y-2">
      <div className="flex items-center justify-between mb-2">
        <span className="text-xs text-ink-mid">{traces.length} traces</span>
        <button type="button" onClick={loadTraces} className="text-[10px] text-ink-soft hover:text-ink-mid">
          Refresh
        </button>
      </div>

      {error && (
        <div className="px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
          {error}
        </div>
      )}

      {traces.length === 0 && !error ? (
        <div className="text-center py-8">
          <div className="text-2xl opacity-20 mb-2" aria-hidden="true">--</div>
          <p className="text-xs text-ink-soft">No traces yet</p>
          <details className="mt-2 text-[10px] text-ink-soft">
            <summary className="cursor-pointer text-ink-soft hover:text-ink-mid">How to enable tracing</summary>
            <p className="mt-1">
              Set <code className="font-mono text-ink-mid">LANGFUSE_HOST</code>, <code className="font-mono text-ink-mid">LANGFUSE_PUBLIC_KEY</code>, <code className="font-mono text-ink-mid">LANGFUSE_SECRET_KEY</code> as workspace secrets to enable tracing.
            </p>
          </details>
        </div>
      ) : (
        <div className="space-y-1">
          {traces.map((trace) => (
            <div key={trace.id} className="bg-surface-card/40 border border-line/40 rounded-lg overflow-hidden">
              <button
                onClick={() => setExpanded(expanded === trace.id ? null : trace.id)}
                className="w-full px-3 py-2 flex items-center gap-2 text-left hover:bg-surface-card/60 transition-colors"
              >
                <div className={`w-1.5 h-1.5 rounded-full shrink-0 ${
                  trace.status === "ERROR" ? "bg-red-400" : "bg-emerald-400"
                }`} />
                <div className="flex-1 min-w-0">
                  <div className="text-[11px] text-ink truncate">{trace.name || "trace"}</div>
                  <div className="text-[9px] text-ink-soft">{formatTime(trace.timestamp)}</div>
                </div>
                <div className="flex items-center gap-2 shrink-0">
                  {trace.latency != null && (
                    <span className="text-[9px] text-ink-soft tabular-nums">
                      {trace.latency > 1000 ? `${(trace.latency / 1000).toFixed(1)}s` : `${trace.latency}ms`}
                    </span>
                  )}
                  {trace.usage?.total != null && (
                    <span className="text-[9px] text-ink-soft tabular-nums">
                      {trace.usage.total} tok
                    </span>
                  )}
                  <span className="text-[9px] text-ink-soft">
                    {expanded === trace.id ? "▼" : "▶"}
                  </span>
                </div>
              </button>

              {expanded === trace.id && (
                <div className="px-3 pb-2 space-y-2 border-t border-line/30">
                  {trace.input && (
                    <div>
                      <div className="text-[9px] text-ink-soft uppercase tracking-wider mt-2 mb-1">Input</div>
                      <pre className="text-[9px] text-ink-mid bg-surface-sunken rounded p-2 overflow-x-auto max-h-32">
                        {String(typeof trace.input === "string" ? trace.input : JSON.stringify(trace.input, null, 2))}
                      </pre>
                    </div>
                  )}
                  {trace.output && (
                    <div>
                      <div className="text-[9px] text-ink-soft uppercase tracking-wider mb-1">Output</div>
                      <pre className="text-[9px] text-ink-mid bg-surface-sunken rounded p-2 overflow-x-auto max-h-32">
                        {String(typeof trace.output === "string" ? trace.output : JSON.stringify(trace.output, null, 2))}
                      </pre>
                    </div>
                  )}
                  {trace.totalCost != null && (
                    <div className="text-[9px] text-ink-soft">
                      Cost: ${trace.totalCost.toFixed(6)}
                    </div>
                  )}
                  <div className="text-[8px] text-ink-soft font-mono select-all">
                    {trace.id}
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

function formatTime(iso: string): string {
  try {
    const d = new Date(iso);
    const now = new Date();
    const diff = now.getTime() - d.getTime();
    if (diff < 60_000) return `${Math.floor(diff / 1000)}s ago`;
    if (diff < 3_600_000) return `${Math.floor(diff / 60_000)}m ago`;
    if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)}h ago`;
    return d.toLocaleDateString();
  } catch {
    return iso;
  }
}
