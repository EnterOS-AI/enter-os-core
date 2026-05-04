export const STATUS_CONFIG: Record<string, { dot: string; glow: string; label: string; bar: string }> = {
  online: { dot: "bg-emerald-400", glow: "shadow-emerald-400/50", label: "Online", bar: "from-emerald-500/20 to-transparent" },
  offline: { dot: "bg-zinc-500", glow: "", label: "Offline", bar: "from-zinc-600/10 to-transparent" },
  paused: { dot: "bg-indigo-400", glow: "", label: "Paused", bar: "from-indigo-500/10 to-transparent" },
  degraded: { dot: "bg-amber-400", glow: "shadow-amber-400/50", label: "Degraded", bar: "from-amber-500/20 to-transparent" },
  failed: { dot: "bg-red-400", glow: "shadow-red-400/50", label: "Failed", bar: "from-red-500/20 to-transparent" },
  provisioning: { dot: "bg-sky-400 motion-safe:animate-pulse", glow: "shadow-sky-400/50", label: "Starting", bar: "from-sky-500/20 to-transparent" },
};

export function statusDotClass(status: string): string {
  return STATUS_CONFIG[status]?.dot ?? "bg-zinc-500";
}

export const TIER_CONFIG: Record<number, { label: string; color: string; border: string }> = {
  1: { label: "T1", color: "text-ink-mid bg-surface-card border border-line", border: "text-ink-mid border-line" },
  2: { label: "T2", color: "text-white bg-accent border border-accent-strong", border: "text-accent border-accent" },
  3: { label: "T3", color: "text-white bg-violet-600 border border-violet-700", border: "text-violet-600 border-violet-500" },
  4: { label: "T4", color: "text-white bg-warm border border-warm", border: "text-warm border-warm" },
};

export const COMM_TYPE_LABELS: Record<string, string> = {
  a2a_send: "sent",
  a2a_receive: "received",
  task_update: "task update",
};
