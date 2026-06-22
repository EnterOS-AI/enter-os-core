"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import { api } from "@/lib/api";
import { fetchSession } from "@/lib/auth";
import { showToast } from "@/components/Toaster";
import { subscribeSocketEvents } from "@/store/socket-events";
import { isRequestEvent } from "@/lib/ws-events";
import s from "./Concierge.module.css";
import { IcClock, IcCheck, IcTrash, IcChat, IcSend } from "./icons";

/**
 * RequestsInbox renders the Home sidebar's Tasks + Approvals tabs on the
 * unified `requests` model (RFC unified-requests-inbox, P3 canvas). It
 * replaces the old split /approvals/pending + /user-tasks/pending sources
 * with the single /requests/pending?kind=… endpoint that P1 backfilled, and
 * adds the full action set (Done/Reject for tasks, Approve/Reject for
 * approvals) plus an inline More-Info thread per item.
 *
 * It is a self-contained, unit-testable unit (mirroring ApprovalBanner) that
 * ConciergeShell embeds inside its existing `.sbBody`; the visual language is
 * the existing Concierge.module.css card/button classes — no restyle.
 */

/** One row of GET /requests/pending — matches the Go RequestRow JSON shape
 *  (workspace-server/internal/handlers/request_store.go RequestRow). */
export interface RequestRow {
  id: string;
  kind: string;
  requester_type: string;
  requester_id: string;
  org_id: string | null;
  recipient_type: string;
  recipient_id: string;
  title: string;
  detail: string | null;
  status: string;
  responder_type: string | null;
  responder_id: string | null;
  priority: number | null;
  created_at: string;
  updated_at: string;
  responded_at: string | null;
  // Non-empty only when the requester party is an agent (LEFT JOIN workspaces).
  workspace_name?: string;
}

/** One row of a request's More-Info thread — matches Go RequestMessageRow. */
export interface RequestMessageRow {
  id: string;
  request_id: string;
  author_type: string;
  author_id: string;
  body: string;
  created_at: string;
}

/** GET /requests/{id} envelope. */
interface RequestWithThread {
  request: RequestRow;
  messages: RequestMessageRow[];
}

export type RequestKind = "task" | "approval";

/** ISO timestamp → "9:05 PM" (local). Empty string on a bad/missing value.
 *  Duplicated tiny formatter (also in ConciergeShell) — kept local so this
 *  unit has no coupling back to the shell. */
function clockTime(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  return d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

/** Human label for who raised a request: the joined workspace name when the
 *  requester is an agent, else a generic party label. */
function requesterLabel(r: RequestRow): string {
  if (r.workspace_name) return r.workspace_name;
  if (r.requester_type === "user") return "You";
  return r.requester_id || "agent";
}

/** A short, human status badge label. info_requested is the More-Info state. */
function statusLabel(status: string): string {
  switch (status) {
    case "info_requested": return "info requested";
    case "pending": return "pending";
    default: return status.replace(/_/g, " ");
  }
}

interface RequestsInboxProps {
  kind: RequestKind;
  /** Lifted setter so the parent (ConciergeShell) can show the tab count.
   *  Called with the current pending-list length on every load. */
  onCountChange?: (n: number) => void;
}

export function RequestsInbox({ kind, onCountChange }: RequestsInboxProps) {
  const [items, setItems] = useState<RequestRow[]>([]);
  // Guards double-submit while a respond POST is in flight (per-item id).
  const [acting, setActing] = useState<string | null>(null);
  // Which item's More-Info thread is expanded inline (null = none).
  const [openThread, setOpenThread] = useState<string | null>(null);

  // Responder identity. The canvas is effectively single-user today; the
  // real responder is the logged-in session user (GET /cp/auth/me → user_id).
  // We resolve it once and reuse it; if no session is reachable we fall back
  // to a clear placeholder so an action is never blocked on auth.
  // TODO(multi-user): when the canvas grows real per-action attribution, plumb
  // the acting user through props instead of a single module-level resolve.
  const responderIdRef = useRef<string>("admin");
  // core#2766: monotonic generation counter so a stale `/requests/pending`
  // response from a previous `kind` cannot overwrite the list after a tab
  // switch (or after a later live-refresh load).
  const loadGenRef = useRef(0);

  useEffect(() => {
    let cancelled = false;
    fetchSession()
      .then((sess) => {
        if (!cancelled && sess?.user_id) responderIdRef.current = sess.user_id;
      })
      .catch(() => {
        // No session reachable — keep the "admin" placeholder.
      });
    return () => { cancelled = true; };
  }, []);

  // Increment the generation on unmount so any in-flight load cannot call
  // setItems after this component instance is gone.
  useEffect(() => {
    return () => {
      loadGenRef.current += 1;
    };
  }, []);

  const load = useCallback(() => {
    const gen = ++loadGenRef.current;
    // core#2766: clear stale rows the moment the kind changes so the user
    // never sees approval cards under the Tasks tab (or vice-versa) while the
    // new list is still fetching. This prevents the wrong-action race where a
    // stale approval row could be actioned with action="done".
    setItems([]);
    onCountChange?.(0);
    api.get<RequestRow[]>(`/requests/pending?kind=${kind}`)
      .then((r) => {
        if (gen !== loadGenRef.current) return; // stale response
        const list = r ?? [];
        setItems(list);
        onCountChange?.(list.length);
      })
      .catch(() => {
        if (gen !== loadGenRef.current) return; // stale response
        setItems([]);
        onCountChange?.(0);
      });
  }, [kind, onCountChange]);

  useEffect(() => { load(); }, [load]);

  // Live refresh: the Go side emits REQUEST_CREATED / REQUEST_RESPONDED /
  // REQUEST_MESSAGE over the shared WS bus. Re-fetch on any of them so a
  // request raised/answered elsewhere (another tab, an agent) reflects here
  // without a manual refresh — same "subscribe to the global bus" pattern the
  // rest of the canvas uses.
  useEffect(() => {
    const unsub = subscribeSocketEvents((msg) => {
      if (isRequestEvent(msg.event)) load();
    });
    return unsub;
  }, [load]);

  /** Terminal action: POST /requests/{id}/respond. Optimistically drops the
   *  item from the pending list on success (it leaves the pending view). */
  const respond = useCallback(
    async (r: RequestRow, action: "done" | "rejected" | "approved") => {
      if (acting) return;
      setActing(r.id);
      try {
        await api.post(`/requests/${r.id}/respond`, {
          action,
          responder_type: "user",
          responder_id: responderIdRef.current,
        });
        const verb =
          action === "approved" ? "Approved" :
          action === "done" ? "Marked done" : "Rejected";
        showToast(verb, action === "rejected" ? "info" : "success");
        setItems((prev) => {
          const next = prev.filter((x) => x.id !== r.id);
          onCountChange?.(next.length);
          return next;
        });
        if (openThread === r.id) setOpenThread(null);
      } catch {
        showToast("Failed to record response", "error");
      } finally {
        setActing(null);
      }
    },
    [acting, openThread, onCountChange],
  );

  const toggleThread = useCallback((id: string) => {
    setOpenThread((cur) => (cur === id ? null : id));
  }, []);

  const emptyCopy =
    kind === "task"
      ? "Nothing needs you right now. When an agent needs you to do something, it shows up here."
      : "No pending approvals. Destructive actions await sign-off here.";

  return (
    <>
      {items.length === 0 && <div className={s.empty}>{emptyCopy}</div>}
      {items.map((r) => (
        <RequestItem
          key={r.id}
          row={r}
          acting={acting === r.id}
          threadOpen={openThread === r.id}
          onRespond={respond}
          onToggleThread={() => toggleThread(r.id)}
          responderId={responderIdRef.current}
        />
      ))}
    </>
  );
}

interface RequestItemProps {
  row: RequestRow;
  acting: boolean;
  threadOpen: boolean;
  onRespond: (r: RequestRow, action: "done" | "rejected" | "approved") => void;
  onToggleThread: () => void;
  responderId: string;
}

/** A single Tasks/Approvals card. Task layout reuses the .task classes;
 *  approval layout reuses the .apprCard classes. Both share the inline
 *  More-Info thread panel. */
function RequestItem({
  row, acting, threadOpen, onRespond, onToggleThread, responderId,
}: RequestItemProps) {
  const badge = statusLabel(row.status);
  // core#2766: derive the card layout and primary action from the ROW'S kind,
  // not from the selected tab. During a tab switch the old tab's rows are
  // cleared, but if any stale render slips through it must not expose a
  // task "Done" button on an approval row.
  const isApproval = row.kind === "approval";

  // "Responder identity" on a resolved row (shows only if a resolved item
  // ever renders in this pending view — defensive, since pending excludes
  // resolved, but info_requested rows carry a responder in some flows).
  const resolvedBy =
    row.status !== "pending" && row.responder_id
      ? `${isApproval ? "Approved by" : "Done by"} ${row.responder_id}`
      : null;

  const actions = isApproval ? (
    <div className={s.apprActions}>
      <button
        type="button"
        className={`${s.btn} ${s.approve} ${s.flex}`}
        disabled={acting}
        onClick={() => onRespond(row, "approved")}
      >
        {acting ? "…" : "Approve"}
      </button>
      <button
        type="button"
        className={`${s.btn} ${s.deny} ${s.flex}`}
        disabled={acting}
        onClick={() => onRespond(row, "rejected")}
      >
        {acting ? "…" : "Reject"}
      </button>
      <button
        type="button"
        className={s.btn}
        disabled={acting}
        onClick={onToggleThread}
        aria-expanded={threadOpen}
      >
        <IcChat /> More Info
      </button>
    </div>
  ) : (
    <div className={s.taskActions}>
      <button
        type="button"
        className={`${s.tbtn} ${s.done}`}
        disabled={acting}
        onClick={() => onRespond(row, "done")}
      >
        <IcCheck />Done
      </button>
      <button
        type="button"
        className={s.tbtn}
        disabled={acting}
        onClick={() => onRespond(row, "rejected")}
      >
        Reject
      </button>
      <button
        type="button"
        className={s.tbtn}
        disabled={acting}
        onClick={onToggleThread}
        aria-expanded={threadOpen}
      >
        <IcChat />More Info
      </button>
    </div>
  );

  if (isApproval) {
    return (
      <div className={s.apprCard} style={{ marginBottom: 7 }} data-testid="request-item" data-kind={row.kind}>
        <div className={s.apprRow}>
          <div className={s.apprIc}><IcTrash /></div>
          <div className={s.apprMeta}>
            <div className={s.apprT}>
              {row.title} <code>{requesterLabel(row)}</code>
            </div>
            <div className={s.apprS}>
              <span data-testid="request-status">{badge}</span>
              {" · asked "}{clockTime(row.created_at)}
            </div>
            {row.detail && <RequestBody body={row.detail} />}
            {resolvedBy && <div className={s.apprS} style={{ marginTop: 4 }}>{resolvedBy}</div>}
          </div>
        </div>
        {actions}
        {threadOpen && (
          <MoreInfoThread requestId={row.id} responderId={responderId} />
        )}
      </div>
    );
  }

  return (
    <div className={s.task} data-testid="request-item" data-kind={row.kind}>
      <div className={s.taskRow}>
        <div className={`${s.taskIc} ${s.run}`}><IcClock /></div>
        <div className={s.taskMeta}>
          <div className={s.taskT}>{row.title}</div>
          <div className={s.taskS}>
            {requesterLabel(row)}<span className={s.pip} />
            <span data-testid="request-status">{badge}</span>
            {" · asked "}{clockTime(row.created_at)}
          </div>
          {row.detail && (
            <RequestBody body={row.detail} />
          )}
          {resolvedBy && (
            <div style={{ fontSize: 12, color: "var(--tx-3)", marginTop: 6 }}>{resolvedBy}</div>
          )}
        </div>
      </div>
      {actions}
      {threadOpen && (
        <MoreInfoThread requestId={row.id} responderId={responderId} />
      )}
    </div>
  );
}

/** Approximate characters that fit in one line inside the 296 px sidebar card. */
const BODY_CHARS_PER_LINE = 48;
const BODY_MAX_LINES = 4;

function bodyNeedsClamp(body: string): boolean {
  const lines = body.split("\n").length;
  return lines > BODY_MAX_LINES || body.length > BODY_CHARS_PER_LINE * BODY_MAX_LINES;
}

/** Open links from agent-authored markdown in a new tab so the canvas stays put. */
function SafeLink({ href, children, ...rest }: React.AnchorHTMLAttributes<HTMLAnchorElement>) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" {...rest}>
      {children}
    </a>
  );
}

/** Shared markdown body for Task + Approval cards.
 *
 * - Renders markdown (bold, inline/fenced code, lists, headers, links) using the
 *   same react-markdown + remark-gfm stack as chat messages.
 * - HTML is escaped by default (no raw HTML / XSS).
 * - Long bodies are clamped to ~4 lines with a Show more / Show less toggle.
 */
function RequestBody({ body }: { body: string | null | undefined }) {
  const [expanded, setExpanded] = useState(false);
  if (!body) return null;

  const clamp = !expanded && bodyNeedsClamp(body);

  return (
    <div className={s.reqBodyWrap} data-testid="request-body">
      <hr className={s.reqDivider} />
      <div className={`${s.reqBody} ${clamp ? s.reqBodyClamped : ""}`}>
        <ReactMarkdown remarkPlugins={[remarkGfm]} components={{ a: SafeLink }}>
          {body}
        </ReactMarkdown>
      </div>
      {bodyNeedsClamp(body) && (
        <button
          type="button"
          className={s.reqShowMore}
          onClick={() => setExpanded((v) => !v)}
          aria-expanded={expanded}
        >
          {expanded ? "Show less" : "Show more"}
        </button>
      )}
    </div>
  );
}

interface MoreInfoThreadProps {
  requestId: string;
  responderId: string;
}

/**
 * Inline "chat about this" panel: loads GET /requests/{id} for the message
 * thread and posts replies to POST /requests/{id}/messages (which flips the
 * request to info_requested server-side). Rendered inside the existing card,
 * styled with the existing card/button vars — no new visual language.
 */
function MoreInfoThread({ requestId, responderId }: MoreInfoThreadProps) {
  const [messages, setMessages] = useState<RequestMessageRow[]>([]);
  const [draft, setDraft] = useState("");
  const [sending, setSending] = useState(false);
  const [loaded, setLoaded] = useState(false);

  const load = useCallback(() => {
    api.get<RequestWithThread>(`/requests/${requestId}`)
      .then((r) => {
        setMessages(r?.messages ?? []);
        setLoaded(true);
      })
      .catch(() => {
        setMessages([]);
        setLoaded(true);
      });
  }, [requestId]);

  useEffect(() => { load(); }, [load]);

  const send = useCallback(async () => {
    const body = draft.trim();
    if (!body || sending) return;
    setSending(true);
    try {
      await api.post(`/requests/${requestId}/messages`, {
        body,
        author_type: "user",
        author_id: responderId,
      });
      setDraft("");
      load(); // re-fetch so the new message (and flipped status) shows
    } catch {
      showToast("Failed to send message", "error");
    } finally {
      setSending(false);
    }
  }, [draft, sending, requestId, responderId, load]);

  return (
    <div
      data-testid="more-info-thread"
      style={{
        borderTop: "1px solid var(--hair)",
        padding: "11px 13px",
        background: "var(--card-2)",
      }}
    >
      <div style={{ display: "flex", flexDirection: "column", gap: 8, maxHeight: 180, overflowY: "auto" }}>
        {loaded && messages.length === 0 && (
          <div style={{ fontSize: 11.5, color: "var(--tx-3)" }}>
            No messages yet. Ask the agent for more detail.
          </div>
        )}
        {messages.map((m) => (
          <div key={m.id} data-testid="thread-message" style={{ fontSize: 12, lineHeight: 1.45 }}>
            <span style={{ fontWeight: 600, color: "var(--tx-2)" }}>
              {m.author_type === "user" ? "You" : m.author_id || "agent"}
            </span>
            <span style={{ color: "var(--tx-3)", marginLeft: 6, fontSize: 10.5 }}>
              {clockTime(m.created_at)}
            </span>
            <div style={{ color: "var(--tx-2)", marginTop: 2 }}>{m.body}</div>
          </div>
        ))}
      </div>
      <div style={{ display: "flex", gap: 7, marginTop: 9 }}>
        <input
          aria-label="More info message"
          data-testid="more-info-input"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              send();
            }
          }}
          placeholder="Ask about this…"
          style={{
            flex: 1,
            fontFamily: "var(--sans)",
            fontSize: 12,
            padding: "6px 10px",
            borderRadius: 8,
            border: "1px solid var(--hair-2)",
            background: "var(--card)",
            color: "var(--tx)",
          }}
        />
        <button
          type="button"
          className={`${s.tbtn}`}
          data-testid="more-info-send"
          disabled={sending || draft.trim().length === 0}
          onClick={send}
        >
          <IcSend />{sending ? "…" : "Send"}
        </button>
      </div>
    </div>
  );
}
