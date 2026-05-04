'use client';

import { useState, useEffect, useCallback, useId } from "react";
import { api } from "@/lib/api";
import { ConfirmDialog } from "@/components/ConfirmDialog";

interface Schedule {
  id: string;
  workspace_id: string;
  name: string;
  cron_expr: string;
  timezone: string;
  prompt: string;
  enabled: boolean;
  last_run_at: string | null;
  next_run_at: string | null;
  run_count: number;
  last_status: string;
  last_error: string;
  created_at: string;
}

interface Props {
  workspaceId: string;
}

function cronToHuman(expr: string): string {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return expr;
  const [min, hour, dom, mon, dow] = parts;
  if (min === "*" && hour === "*") return `Every minute`;
  if (min.startsWith("*/")) return `Every ${min.slice(2)} minutes`;
  if (hour.startsWith("*/") && min === "0") return `Every ${hour.slice(2)} hours`;
  if (dom === "*" && mon === "*" && dow === "*" && !hour.startsWith("*/"))
    return `Daily at ${hour.padStart(2, "0")}:${min.padStart(2, "0")} UTC`;
  if (dom === "*" && mon === "*" && dow === "1-5" && !hour.startsWith("*/"))
    return `Weekdays at ${hour.padStart(2, "0")}:${min.padStart(2, "0")} UTC`;
  return expr;
}

function relativeTime(iso: string | null): string {
  if (!iso) return "never";
  const diff = Date.now() - new Date(iso).getTime();
  if (diff < 0) {
    const future = -diff;
    if (future < 60000) return `in ${Math.round(future / 1000)}s`;
    if (future < 3600000) return `in ${Math.round(future / 60000)}m`;
    if (future < 86400000) return `in ${Math.round(future / 3600000)}h`;
    return `in ${Math.round(future / 86400000)}d`;
  }
  if (diff < 60000) return `${Math.round(diff / 1000)}s ago`;
  if (diff < 3600000) return `${Math.round(diff / 60000)}m ago`;
  if (diff < 86400000) return `${Math.round(diff / 3600000)}h ago`;
  return `${Math.round(diff / 86400000)}d ago`;
}

export function ScheduleTab({ workspaceId }: Props) {
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [editId, setEditId] = useState<string | null>(null);
  const [formName, setFormName] = useState("");
  const [formCron, setFormCron] = useState("0 9 * * *");
  const [formTimezone, setFormTimezone] = useState("UTC");
  const [formPrompt, setFormPrompt] = useState("");
  const [formEnabled, setFormEnabled] = useState(true);
  const [error, setError] = useState("");
  const [pendingDelete, setPendingDelete] = useState<{ id: string; name: string } | null>(null);

  // Stable IDs for label↔input associations (WCAG 1.3.1)
  const cronId = useId();
  const timezoneId = useId();
  const promptId = useId();

  const fetchSchedules = useCallback(async () => {
    try {
      const data = await api.get<Schedule[]>(`/workspaces/${workspaceId}/schedules`);
      setSchedules(data);
    } catch {
      setSchedules([]);
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    fetchSchedules();
    const interval = setInterval(fetchSchedules, 10000);
    return () => clearInterval(interval);
  }, [fetchSchedules]);

  const resetForm = () => {
    setFormName("");
    setFormCron("0 9 * * *");
    setFormTimezone("UTC");
    setFormPrompt("");
    setFormEnabled(true);
    setEditId(null);
    setShowForm(false);
    setError("");
  };

  const handleSubmit = async () => {
    setError("");
    try {
      if (editId) {
        await api.patch(`/workspaces/${workspaceId}/schedules/${editId}`, {
          name: formName,
          cron_expr: formCron,
          timezone: formTimezone,
          prompt: formPrompt,
          enabled: formEnabled,
        });
      } else {
        await api.post(`/workspaces/${workspaceId}/schedules`, {
          name: formName,
          cron_expr: formCron,
          timezone: formTimezone,
          prompt: formPrompt,
          enabled: formEnabled,
        });
      }
      resetForm();
      fetchSchedules();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to save schedule");
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    const { id } = pendingDelete;
    setPendingDelete(null);
    try {
      await api.del(`/workspaces/${workspaceId}/schedules/${id}`);
      fetchSchedules();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to delete schedule");
    }
  };

  const handleToggle = async (sched: Schedule) => {
    try {
      await api.patch(`/workspaces/${workspaceId}/schedules/${sched.id}`, {
        enabled: !sched.enabled,
      });
      fetchSchedules();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to toggle schedule");
    }
  };

  const handleEdit = (sched: Schedule) => {
    setFormName(sched.name);
    setFormCron(sched.cron_expr);
    setFormTimezone(sched.timezone);
    setFormPrompt(sched.prompt);
    setFormEnabled(sched.enabled);
    setEditId(sched.id);
    setShowForm(true);
  };

  const handleRunNow = async (sched: Schedule) => {
    try {
      const result = await api.post<{ prompt: string }>(`/workspaces/${workspaceId}/schedules/${sched.id}/run`, {});
      await api.post(`/workspaces/${workspaceId}/a2a`, {
        method: "message/send",
        params: {
          message: {
            role: "user",
            messageId: `manual-cron-${Date.now()}`,
            parts: [{ kind: "text", text: result.prompt }],
          },
        },
      });
      fetchSchedules();
    } catch {
      setError("Failed to run schedule");
    }
  };

  if (loading) {
    return <div className="p-4 text-[10px] text-ink-soft">Loading schedules...</div>;
  }

  return (
    <div className="flex flex-col h-full">
      {/* Header */}
      <div className="flex items-center justify-between px-3 py-2 border-b border-line/50">
        <span className="text-[10px] font-semibold text-ink-mid uppercase tracking-wider">
          Schedules
        </span>
        <button
          onClick={() => { resetForm(); setShowForm(true); }}
          className="text-[11px] px-2 py-0.5 bg-accent-strong/20 text-accent rounded hover:bg-accent-strong/30 transition-colors"
        >
          + Add Schedule
        </button>
      </div>

      {/* Create/Edit Form */}
      {showForm && (
        <div className="p-3 border-b border-line/50 bg-surface-sunken/50 space-y-2">
          <input
            type="text"
            aria-label="Schedule name"
            placeholder="Schedule name (e.g., Daily security scan)"
            value={formName}
            onChange={(e) => setFormName(e.target.value)}
            className="w-full text-[10px] bg-surface-card border border-line rounded px-2 py-1 text-ink placeholder:text-ink-soft"
          />
          <div className="flex gap-2">
            <div className="flex-1">
              <label htmlFor={cronId} className="text-[10px] text-ink-soft block mb-0.5">Cron Expression</label>
              <input
                id={cronId}
                type="text"
                value={formCron}
                onChange={(e) => setFormCron(e.target.value)}
                className="w-full text-[10px] bg-surface-card border border-line rounded px-2 py-1 text-ink font-mono"
              />
              <div className="text-[10px] text-ink-soft mt-0.5">
                {cronToHuman(formCron)}
              </div>
            </div>
            <div className="w-24">
              <label htmlFor={timezoneId} className="text-[10px] text-ink-soft block mb-0.5">Timezone</label>
              <select
                id={timezoneId}
                value={formTimezone}
                onChange={(e) => setFormTimezone(e.target.value)}
                className="w-full text-[10px] bg-surface-card border border-line rounded px-1 py-1 text-ink"
              >
                <option value="UTC">UTC</option>
                <option value="America/New_York">US Eastern</option>
                <option value="America/Chicago">US Central</option>
                <option value="America/Denver">US Mountain</option>
                <option value="America/Los_Angeles">US Pacific</option>
                <option value="Europe/London">London</option>
                <option value="Europe/Berlin">Berlin</option>
                <option value="Asia/Tokyo">Tokyo</option>
                <option value="Asia/Shanghai">Shanghai</option>
                <option value="Australia/Sydney">Sydney</option>
              </select>
            </div>
          </div>
          <div>
            <label htmlFor={promptId} className="text-[10px] text-ink-soft block mb-0.5">Prompt / Task</label>
            <textarea
              id={promptId}
              value={formPrompt}
              onChange={(e) => setFormPrompt(e.target.value)}
              placeholder="What should the agent do on this schedule?"
              rows={3}
              className="w-full text-[10px] bg-surface-card border border-line rounded px-2 py-1 text-ink placeholder:text-ink-soft resize-y"
            />
          </div>
          <div className="flex items-center gap-2">
            <label className="flex items-center gap-1.5 text-[10px] text-ink-mid cursor-pointer">
              <input
                type="checkbox"
                checked={formEnabled}
                onChange={(e) => setFormEnabled(e.target.checked)}
                className="rounded border-line"
              />
              Enabled
            </label>
          </div>
          {error && <div className="text-[10px] text-bad">{error}</div>}
          <div className="flex gap-2">
            <button
              type="button"
              onClick={handleSubmit}
              disabled={!formCron || !formPrompt}
              // Was bg-accent-strong hover:bg-accent — accent is the
              // LIGHTER variant, so this hovered lighter on white text
              // and dropped contrast below AA. Same trap fixed in
              // OnboardingWizard, ConfirmDialog, ApprovalBanner.
              className="text-[11px] px-3 py-1 bg-accent text-white rounded hover:bg-accent-strong disabled:opacity-40 transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-1 focus-visible:ring-offset-surface"
            >
              {editId ? "Update" : "Create"}
            </button>
            <button
              type="button"
              onClick={resetForm}
              // Was hover:bg-surface-card on top of bg-surface-card —
              // silent no-op hover. Lift to surface-elevated.
              className="text-[11px] px-3 py-1 bg-surface-card text-ink-mid rounded hover:bg-surface-elevated hover:text-ink transition-colors focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 focus-visible:ring-offset-1 focus-visible:ring-offset-surface"
            >
              Cancel
            </button>
          </div>
          <div className="text-[10px] text-ink-soft space-y-0.5">
            <div>Common patterns:</div>
            <div className="font-mono">{"0 9 * * *"} — Daily at 9:00 AM</div>
            <div className="font-mono">{"*/30 * * * *"} — Every 30 minutes</div>
            <div className="font-mono">{"0 */4 * * *"} — Every 4 hours</div>
            <div className="font-mono">{"0 9 * * 1-5"} — Weekdays at 9:00 AM</div>
          </div>
        </div>
      )}

      {/* Schedule List */}
      <div className="flex-1 overflow-y-auto">
        {schedules.length === 0 && !showForm ? (
          <div className="p-6 text-center">
            <div className="text-2xl mb-2">⏲</div>
            <div className="text-[10px] text-ink-mid mb-1">No schedules yet</div>
            <div className="text-[9px] text-ink-soft">
              Add a schedule to run tasks automatically — daily scans, periodic reports, standup reminders.
            </div>
          </div>
        ) : (
          schedules.map((sched) => (
            <div
              key={sched.id}
              className={`px-3 py-2 border-b border-line/30 ${
                !sched.enabled ? "opacity-50" : ""
              }`}
            >
              <div className="flex items-start justify-between gap-2">
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-1.5">
                    <button
                      onClick={() => handleToggle(sched)}
                      className={`w-2 h-2 rounded-full flex-shrink-0 ${
                        sched.last_status === "error"
                          ? "bg-red-400"
                          : sched.last_status === "ok"
                          ? "bg-emerald-400"
                          : "bg-surface-card"
                      }`}
                      title={sched.enabled ? "Click to disable" : "Click to enable"}
                    />
                    <span className="text-[10px] font-medium text-ink truncate">
                      {sched.name || "Unnamed schedule"}
                    </span>
                  </div>
                  <div className="text-[9px] text-ink-soft mt-0.5 font-mono">
                    {cronToHuman(sched.cron_expr)}
                    {sched.timezone !== "UTC" && (
                      <span className="text-ink-soft"> ({sched.timezone})</span>
                    )}
                  </div>
                  <div className="text-[9px] text-ink-soft mt-0.5 truncate">
                    {sched.prompt.slice(0, 80)}{sched.prompt.length > 80 ? "..." : ""}
                  </div>
                  <div className="flex items-center gap-3 mt-1 text-[8px] text-ink-soft">
                    <span>Last: {relativeTime(sched.last_run_at)}</span>
                    <span>Next: {relativeTime(sched.next_run_at)}</span>
                    <span>Runs: {sched.run_count}</span>
                  </div>
                  {sched.last_error && (
                    <div className="text-[8px] text-bad/70 mt-0.5 truncate">
                      Error: {sched.last_error}
                    </div>
                  )}
                </div>
                <div className="flex items-center gap-1 flex-shrink-0">
                  <button
                    onClick={() => handleRunNow(sched)}
                    aria-label={`Run schedule ${sched.name} now`}
                    className="text-[11px] px-1.5 py-0.5 text-accent hover:bg-accent-strong/20 rounded transition-colors"
                    title="Run now"
                  >
                    ▶
                  </button>
                  <button
                    onClick={() => handleEdit(sched)}
                    aria-label={`Edit schedule ${sched.name}`}
                    className="text-[11px] px-1.5 py-0.5 text-ink-mid hover:bg-surface-card rounded transition-colors"
                    title="Edit"
                  >
                    ✎
                  </button>
                  <button
                    onClick={() => setPendingDelete({ id: sched.id, name: sched.name })}
                    aria-label={`Delete schedule ${sched.name}`}
                    className="text-[11px] px-1.5 py-0.5 text-bad hover:bg-red-600/20 rounded transition-colors"
                    title="Delete"
                  >
                    ✕
                  </button>
                </div>
              </div>
            </div>
          ))
        )}
      </div>

      <ConfirmDialog
        open={!!pendingDelete}
        title="Delete schedule"
        message={`Delete schedule "${pendingDelete?.name || "Unnamed"}"? This cannot be undone.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </div>
  );
}
