'use client';

import { useState, useEffect, useCallback, useId } from "react";
import { api } from "@/lib/api";
import { ConfirmDialog } from "@/components/ConfirmDialog";

// ConfigField mirrors the Go struct returned by GET /channels/adapters —
// the UI renders one input per field in the order the adapter returns
// them, so per-platform form shape stays server-owned.
interface ConfigField {
  key: string;
  label: string;
  type: "text" | "password" | "textarea";
  required: boolean;
  sensitive?: boolean;
  placeholder?: string;
  help?: string;
}

interface ChannelAdapter {
  type: string;
  display_name: string;
  config_schema?: ConfigField[];
}

interface Channel {
  id: string;
  workspace_id: string;
  channel_type: string;
  config: Record<string, string>;
  enabled: boolean;
  allowed_users: string[];
  message_count: number;
  last_message_at?: string;
  created_at: string;
}

interface Props {
  workspaceId: string;
}

// Telegram is the only platform that supports "Detect Chats" via
// getUpdates. Every other platform uses a webhook URL that already
// encodes the chat, so the button is only offered when useful.
const SUPPORTS_DETECT_CHATS = new Set(["telegram"]);

function relativeTime(iso: string | null | undefined): string {
  if (!iso) return "never";
  const diff = Date.now() - new Date(iso).getTime();
  if (diff < 60000) return `${Math.round(diff / 1000)}s ago`;
  if (diff < 3600000) return `${Math.round(diff / 60000)}m ago`;
  if (diff < 86400000) return `${Math.round(diff / 3600000)}h ago`;
  return `${Math.round(diff / 86400000)}d ago`;
}

export function ChannelsTab({ workspaceId }: Props) {
  const [channels, setChannels] = useState<Channel[]>([]);
  const [adapters, setAdapters] = useState<ChannelAdapter[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [testing, setTesting] = useState<string | null>(null);
  const [pendingDelete, setPendingDelete] = useState<Channel | null>(null);
  const [error, setError] = useState("");

  // Form state — schema-driven: formValues holds the typed-in config for
  // whichever adapter is currently selected, keyed by ConfigField.key.
  const [formType, setFormType] = useState("telegram");
  const [formValues, setFormValues] = useState<Record<string, string>>({});
  const [formAllowedUsers, setFormAllowedUsers] = useState("");
  const [formError, setFormError] = useState("");
  const [discovering, setDiscovering] = useState(false);
  const [discoveredChats, setDiscoveredChats] = useState<{ chat_id: string; name: string; type: string }[]>([]);
  const [selectedChats, setSelectedChats] = useState<Set<string>>(new Set());
  const [showManualInput, setShowManualInput] = useState(false);

  const platformId = useId();
  const allowedUsersId = useId();

  const currentAdapter = adapters.find((a) => a.type === formType);
  const currentSchema: ConfigField[] = currentAdapter?.config_schema || [];

  const load = useCallback(async () => {
    const [chResult, adResult] = await Promise.allSettled([
      api.get<Channel[]>(`/workspaces/${workspaceId}/channels`),
      api.get<ChannelAdapter[]>(`/channels/adapters`),
    ]);
    const errors: string[] = [];
    if (chResult.status === "fulfilled") {
      setChannels(Array.isArray(chResult.value) ? chResult.value : []);
    } else {
      console.warn("ChannelsTab: channels load failed", chResult.reason);
      errors.push("connected channels");
    }
    if (adResult.status === "fulfilled") {
      setAdapters(Array.isArray(adResult.value) ? adResult.value : []);
    } else {
      console.warn("ChannelsTab: adapters load failed", adResult.reason);
      errors.push("platforms");
    }
    if (errors.length > 0) {
      setError(`Failed to load ${errors.join(" and ")} — try refreshing`);
    } else {
      setError("");
    }
    setLoading(false);
  }, [workspaceId]);

  useEffect(() => { load(); }, [load]);

  // Auto-refresh every 15s
  useEffect(() => {
    const interval = setInterval(load, 15000);
    return () => clearInterval(interval);
  }, [load]);

  // Reset form values when the selected platform changes — each platform
  // has a different field set, so reusing old values would leak stale
  // data across platforms.
  useEffect(() => {
    setFormValues({});
    setDiscoveredChats([]);
    setSelectedChats(new Set());
    setShowManualInput(false);
    setFormError("");
  }, [formType]);

  const setFieldValue = (key: string, value: string) => {
    setFormValues((prev) => ({ ...prev, [key]: value }));
  };

  const handleDiscover = async () => {
    const botToken = formValues["bot_token"] || "";
    if (!botToken) {
      setFormError("Enter a bot token first");
      return;
    }
    setDiscovering(true);
    setFormError("");
    setDiscoveredChats([]);
    try {
      const res = await api.post<{ chats: { chat_id: string; name: string; type: string }[]; hint: string }>(
        `/channels/discover`,
        { channel_type: formType, bot_token: botToken, workspace_id: workspaceId }
      );
      const chats = res.chats || [];
      setDiscoveredChats(chats);
      if (chats.length === 0) {
        setFormError("No chats found. For groups: add the bot and send a message. For DMs: send /start to the bot first. Then retry.");
      } else {
        setSelectedChats(new Set(chats.map((c) => c.chat_id)));
        setFieldValue("chat_id", chats.map((c) => c.chat_id).join(", "));
      }
    } catch (e) {
      setFormError(String(e));
    } finally {
      setDiscovering(false);
    }
  };

  const toggleChat = (chatId: string) => {
    setSelectedChats((prev) => {
      const next = new Set(prev);
      if (next.has(chatId)) next.delete(chatId);
      else next.add(chatId);
      setFieldValue("chat_id", Array.from(next).join(", "));
      return next;
    });
  };

  const handleCreate = async () => {
    setFormError("");
    // Client-side required-field check so the user sees the gap before
    // we round-trip to the server. ValidateConfig on the backend remains
    // authoritative — adapter-specific rules like "bot_token OR webhook_url"
    // for Slack aren't expressible in required-flag alone.
    const missing = currentSchema
      .filter((f) => f.required && !(formValues[f.key] || "").trim())
      .map((f) => f.label);
    if (missing.length > 0) {
      setFormError(`Required: ${missing.join(", ")}`);
      return;
    }
    try {
      const allowed = formAllowedUsers
        .split(",")
        .map((s) => s.trim())
        .filter(Boolean);
      // Only send keys the schema knows about — avoids accidentally
      // persisting stale values when the user switched platforms mid-edit.
      const config: Record<string, string> = {};
      for (const f of currentSchema) {
        const v = (formValues[f.key] || "").trim();
        if (v) config[f.key] = v;
      }
      await api.post(`/workspaces/${workspaceId}/channels`, {
        channel_type: formType,
        config,
        allowed_users: allowed,
      });
      setShowForm(false);
      setFormValues({});
      setFormAllowedUsers("");
      load();
    } catch (e) {
      setFormError(String(e));
    }
  };

  const handleToggle = async (ch: Channel) => {
    try {
      await api.patch(`/workspaces/${workspaceId}/channels/${ch.id}`, {
        enabled: !ch.enabled,
      });
      load();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to toggle channel");
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    const ch = pendingDelete;
    setPendingDelete(null);
    try {
      await api.del(`/workspaces/${workspaceId}/channels/${ch.id}`);
      load();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to delete channel");
    }
  };

  const handleTest = async (ch: Channel) => {
    setTesting(ch.id);
    try {
      await api.post(`/workspaces/${workspaceId}/channels/${ch.id}/test`, {});
    } catch {
      /* ignore — error shown on platform side */
    } finally {
      setTimeout(() => setTesting(null), 2000);
    }
  };

  if (loading) {
    return (
      <div className="p-4 text-ink-soft text-xs">Loading channels...</div>
    );
  }

  return (
    <div className="p-4 space-y-4">
      {/* Header */}
      <div className="flex items-center justify-between">
        <h3 className="text-xs font-semibold text-ink-mid tracking-wide uppercase">
          Channels
        </h3>
        <button
          onClick={() => setShowForm(!showForm)}
          className="text-[10px] px-2.5 py-1 rounded bg-accent-strong/20 text-accent hover:bg-accent-strong/30 transition"
        >
          {showForm ? "Cancel" : "+ Connect"}
        </button>
      </div>

      {error && (
        <div className="px-3 py-1.5 bg-red-900/30 border border-red-800 rounded text-xs text-bad">
          {error}
        </div>
      )}

      {/* Create form — schema-driven */}
      {showForm && (
        <div className="space-y-2 p-3 bg-surface-card/40 rounded border border-line/50">
          <div>
            <label htmlFor={platformId} className="text-[10px] text-ink-soft block mb-1">Platform</label>
            <select
              id={platformId}
              value={formType}
              onChange={(e) => setFormType(e.target.value)}
              className="w-full text-xs bg-surface-sunken border border-line rounded px-2 py-1.5 text-ink-mid"
            >
              {adapters.map((a) => (
                <option key={a.type} value={a.type}>{a.display_name}</option>
              ))}
            </select>
          </div>

          {/* Render one input per schema field. Fallback path: if the
              backend didn't return a schema (older platform version) show
              a single bot_token + chat_id pair to preserve the old UX. */}
          {currentSchema.length === 0 ? (
            <div className="text-[10px] text-yellow-500">
              Platform exposes no config schema — upgrade the platform to pick up first-class support.
            </div>
          ) : (
            currentSchema.map((field) => (
              <SchemaField
                key={field.key}
                field={field}
                value={formValues[field.key] || ""}
                onChange={(v) => setFieldValue(field.key, v)}
                // Detect Chats button lives next to the chat_id input on
                // Telegram only (the only platform with getUpdates).
                renderExtras={
                  field.key === "chat_id" && SUPPORTS_DETECT_CHATS.has(formType)
                    ? () => (
                        <>
                          <div className="flex items-center justify-end mb-1 -mt-1">
                            <button
                              onClick={handleDiscover}
                              disabled={discovering || !formValues["bot_token"]}
                              className="text-[10px] px-2 py-0.5 rounded bg-accent-strong/20 text-accent hover:bg-accent-strong/30 transition disabled:opacity-40"
                            >
                              {discovering ? "Detecting..." : "Detect Chats"}
                            </button>
                          </div>
                          {discoveredChats.length > 0 && (
                            <div className="space-y-1 mb-2">
                              {discoveredChats.map((chat) => (
                                <label
                                  key={chat.chat_id}
                                  className="flex items-center gap-2 px-2 py-1.5 bg-surface-sunken/50 rounded border border-line/50 cursor-pointer hover:bg-surface-card/50"
                                >
                                  <input
                                    type="checkbox"
                                    checked={selectedChats.has(chat.chat_id)}
                                    onChange={() => toggleChat(chat.chat_id)}
                                    className="rounded border-line"
                                  />
                                  <span className="text-xs text-ink-mid">{chat.name || "Unknown"}</span>
                                  <span className="text-[10px] text-ink-soft ml-auto">{chat.type} {chat.chat_id}</span>
                                </label>
                              ))}
                              <button
                                onClick={() => setShowManualInput(!showManualInput)}
                                className="text-[10px] text-accent hover:underline"
                              >
                                {showManualInput ? "hide manual input" : "edit manually"}
                              </button>
                            </div>
                          )}
                        </>
                      )
                    : undefined
                }
              />
            ))
          )}

          <div>
            <label htmlFor={allowedUsersId} className="text-[10px] text-ink-soft block mb-1">
              Allowed Users <span className="text-ink-soft">(optional, comma-separated)</span>
            </label>
            <input
              id={allowedUsersId}
              value={formAllowedUsers}
              onChange={(e) => setFormAllowedUsers(e.target.value)}
              placeholder="123456789, 987654321"
              className="w-full text-xs bg-surface-sunken border border-line rounded px-2 py-1.5 text-ink-mid placeholder-zinc-600"
            />
            <p className="text-[11px] text-ink-soft mt-0.5">
              Platform-specific user IDs. Leave empty to allow everyone.
            </p>
          </div>
          {formError && (
            <p className="text-[10px] text-bad">{formError}</p>
          )}
          <button
            onClick={handleCreate}
            className="w-full text-xs py-1.5 rounded bg-accent-strong hover:bg-accent text-white transition"
          >
            Connect Channel
          </button>
        </div>
      )}

      {/* Channel list */}
      {channels.length === 0 && !showForm && (
        <div className="text-center py-8">
          <p className="text-ink-soft text-xs">No channels connected</p>
          <p className="text-ink-soft text-[10px] mt-1">
            Connect Telegram, Slack, Discord, or Lark / Feishu to chat with this agent from social platforms.
          </p>
        </div>
      )}

      {channels.map((ch) => (
        <div
          key={ch.id}
          className="p-3 bg-surface-card/30 rounded border border-line/40 space-y-2"
        >
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span
                className={`w-2 h-2 rounded-full ${
                  ch.enabled ? "bg-emerald-500" : "bg-surface-card"
                }`}
              />
              <span className="text-xs font-medium text-ink">
                {ch.channel_type.charAt(0).toUpperCase() + ch.channel_type.slice(1)}
              </span>
              <span className="text-[10px] text-ink-soft">
                {ch.config.chat_id || ch.config.channel_id || ""}
              </span>
            </div>
            <div className="flex items-center gap-1.5">
              <button
                onClick={() => handleTest(ch)}
                disabled={testing === ch.id}
                className="text-[10px] px-2 py-0.5 rounded bg-surface-card/50 text-ink-mid hover:text-ink transition disabled:opacity-50"
              >
                {testing === ch.id ? "Sent!" : "Test"}
              </button>
              <button
                onClick={() => handleToggle(ch)}
                className={`text-[10px] px-2 py-0.5 rounded transition ${
                  ch.enabled
                    ? "bg-emerald-900/30 text-good hover:bg-emerald-900/50"
                    : "bg-surface-card/50 text-ink-soft hover:text-ink-mid"
                }`}
              >
                {ch.enabled ? "On" : "Off"}
              </button>
              <button
                onClick={() => setPendingDelete(ch)}
                className="text-[10px] px-2 py-0.5 rounded bg-red-900/20 text-bad hover:bg-red-900/40 transition"
              >
                Remove
              </button>
            </div>
          </div>
          <div className="flex items-center gap-4 text-[10px] text-ink-soft">
            <span>{ch.message_count} messages</span>
            <span>Last: {relativeTime(ch.last_message_at)}</span>
            {ch.allowed_users.length > 0 && (
              <span>{ch.allowed_users.length} allowed user(s)</span>
            )}
          </div>
        </div>
      ))}

      <ConfirmDialog
        open={!!pendingDelete}
        title="Remove channel"
        message={`Delete ${pendingDelete?.channel_type ?? ""} channel? This will stop messages flowing through this integration.`}
        confirmLabel="Remove"
        confirmVariant="danger"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </div>
  );
}

// SchemaField renders one ConfigField as a label + input. Kept inline in
// this file so the ChannelsTab stays self-contained; promote to its own
// module if another tab ever needs it.
function SchemaField({
  field,
  value,
  onChange,
  renderExtras,
}: {
  field: ConfigField;
  value: string;
  onChange: (v: string) => void;
  renderExtras?: () => React.ReactNode;
}) {
  const inputId = useId();
  const common =
    "w-full text-xs bg-surface-sunken border border-line rounded px-2 py-1.5 text-ink-mid placeholder-zinc-600";
  return (
    <div>
      <label htmlFor={inputId} className="text-[10px] text-ink-soft block mb-1">
        {field.label}
        {!field.required && <span className="text-ink-soft"> (optional)</span>}
      </label>
      {field.type === "textarea" ? (
        <textarea
          id={inputId}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={field.placeholder}
          rows={3}
          className={common}
        />
      ) : (
        <input
          id={inputId}
          type={field.type === "password" ? "password" : "text"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={field.placeholder}
          className={common}
        />
      )}
      {renderExtras?.()}
      {field.help && (
        <p className="text-[11px] text-ink-soft mt-0.5">{field.help}</p>
      )}
    </div>
  );
}
