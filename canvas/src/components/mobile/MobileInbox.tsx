// MobileInbox — Tasks + Approvals on mobile (core#2697 Phase 1).
//
// The desktop home flow has Tasks + Approvals as first-class destinations
// (ConciergeShell sidebar, RequestsInbox); mobile had NO way to review or
// action pending requests — decision-on-the-go, the canonical mobile job, was
// missing entirely. This is the mobile-native equivalent: same data layer
// (GET /requests/pending?kind=… + POST /requests/{id}/respond) the desktop
// RequestsInbox uses, with a touch-first list + sub-tabs, live WS refresh, and
// optimistic decisions. (More-Info threads are desktop-only for v1 — flagged.)

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { api } from "@/lib/api";
import { fetchSession } from "@/lib/auth";
import { useSocketEvent } from "@/hooks/useSocketEvent";
import type { RequestRow } from "@/components/concierge/RequestsInbox";
import { MOBILE_FONT_SANS, usePalette } from "./palette";

type InboxKind = "task" | "approval";

// WS events that mutate the pending set — re-fetch on any of them so the
// inbox stays live (mirrors RequestsInbox's refresh triggers).
const REFRESH_EVENTS = new Set([
  "REQUEST_CREATED",
  "REQUEST_RESPONDED",
  "REQUEST_MESSAGE",
  "REQUEST_UPDATED",
]);

export function MobileInbox({ dark }: { dark: boolean }) {
  const p = usePalette(dark);
  const [kind, setKind] = useState<InboxKind>("approval");
  const [items, setItems] = useState<RequestRow[]>([]);
  const [loading, setLoading] = useState(true);
  const [acting, setActing] = useState<string | null>(null);
  // Audit F1: track backend fetch failure distinctly so the list can render
  // a retry affordance instead of the silent "No pending approvals" empty
  // state. A genuine backend outage during a destructive-approvals review
  // must NOT look like a clean inbox.
  const [loadError, setLoadError] = useState<string | null>(null);

  // Responder identity — the same resolution RequestsInbox uses (session
  // user_id, "admin" placeholder when unauthenticated).
  const responderIdRef = useRef<string>("admin");
  useEffect(() => {
    let cancelled = false;
    fetchSession()
      .then((s) => { if (!cancelled && s?.user_id) responderIdRef.current = s.user_id; })
      .catch(() => {});
    return () => { cancelled = true; };
  }, []);

  // Guard against stale fetches: when the user switches tabs, a previous
  // in-flight fetch for the old kind must not overwrite the list after the
  // new tab's load clears it (CR2 #11478).
  const loadSeqRef = useRef(0);
  // Audit F3: track rows the user has just optimistically acted on. A
  // REQUEST_RESPONDED WS event fired by OUR server (in response to our own
  // POST) can race with the optimistic removal — the WS-triggered load()
  // may re-fetch a list that still contains the row (the POST hasn't
  // landed server-side yet), and a naive setItems would briefly re-render
  // the just-approved row (the "flicker" report). Filtering the just-acted
  // set on load() return closes the race; the set is cleared on POST
  // completion (success drops it because the server no longer returns the
  // row; failure drops it because the catch re-loads from server truth).
  const justActedRef = useRef<Set<string>>(new Set());
  const load = useCallback(() => {
    const seq = ++loadSeqRef.current;
    // core#2766 / CR2 #11478: clear stale rows the moment the kind changes
    // so the user never sees approval cards under the Tasks tab (or
    // vice-versa) while the new list is still fetching.
    setItems([]);
    setLoadError(null);
    setLoading(true);
    api
      .get<RequestRow[]>(`/requests/pending?kind=${kind}`)
      .then((rows) => {
        if (seq !== loadSeqRef.current) return; // stale response
        const arr = Array.isArray(rows) ? rows : [];
        // F3: filter out rows the user has just acted on so a racing
        // WS-triggered load doesn't re-surface them in the brief window
        // between optimistic-removal and POST completion.
        setItems(arr.filter((r) => !justActedRef.current.has(r.id)));
      })
      .catch(() => {
        if (seq !== loadSeqRef.current) return; // stale response
        setItems([]);
        // F1: surface the failure distinctly. The render path below
        // shows a retry affordance + role="alert" instead of the
        // "No pending approvals" copy that would falsely suggest a
        // clean inbox during a backend outage.
        setLoadError("Could not load pending requests.");
      })
      .finally(() => {
        if (seq !== loadSeqRef.current) return; // stale response
        setLoading(false);
      });
  }, [kind]);

  useEffect(() => { load(); }, [load]);

  useSocketEvent((msg) => {
    if (msg?.event && REFRESH_EVENTS.has(msg.event)) load();
  });

  const respond = useCallback(
    async (r: RequestRow, action: "done" | "rejected" | "approved") => {
      if (acting) return;
      setActing(r.id);
      // Optimistic: drop the row immediately. The server is the source of
      // truth, so on failure we re-load() rather than restoring a stale
      // `items` snapshot (Audit F2: a `setItems(prev)` snapshot would wipe
      // any rows that arrived via WS during the in-flight POST).
      justActedRef.current.add(r.id);
      setItems((cur) => cur.filter((x) => x.id !== r.id));
      try {
        await api.post(`/requests/${r.id}/respond`, {
          action,
          responder_type: "user",
          responder_id: responderIdRef.current,
        });
        // F3: POST succeeded — the server now treats r as responded, so
        // future loads won't return it. Drop from justActedRef.
        justActedRef.current.delete(r.id);
      } catch {
        // F2: re-fetch from server truth (preserves any rows that arrived
        // via WS during the in-flight POST — restoring a stale `items`
        // snapshot here would wipe them). Also drops the row from
        // justActedRef so the server's re-fetched list (with the row
        // still pending) is rendered as-is.
        justActedRef.current.delete(r.id);
        load();
      } finally {
        setActing(null);
      }
    },
    [acting, load],
  );

  const subTabs: { id: InboxKind; label: string }[] = useMemo(
    () => [
      { id: "approval", label: "Approvals" },
      { id: "task", label: "Tasks" },
    ],
    [],
  );

  return (
    <div
      style={{
        height: "100%",
        display: "flex",
        flexDirection: "column",
        background: p.bg,
        color: p.text,
        fontFamily: MOBILE_FONT_SANS,
      }}
    >
      {/* Header + sub-tabs */}
      <div style={{ padding: "14px 16px 0", borderBottom: `0.5px solid ${p.divider}` }}>
        <div style={{ fontSize: 17, fontWeight: 600, marginBottom: 10 }}>Inbox</div>
        <div role="tablist" aria-label="Inbox kind" style={{ display: "flex", gap: 18 }}>
          {subTabs.map((t) => {
            const on = kind === t.id;
            return (
              <button
                key={t.id}
                role="tab"
                aria-selected={on}
                onClick={() => setKind(t.id)}
                style={{
                  background: "none",
                  border: "none",
                  padding: "0 0 10px",
                  fontSize: 14,
                  fontWeight: on ? 600 : 500,
                  color: on ? p.text : p.text3,
                  borderBottom: on ? `2px solid ${p.accent}` : "2px solid transparent",
                  cursor: "pointer",
                  fontFamily: MOBILE_FONT_SANS,
                }}
              >
                {t.label}
              </button>
            );
          })}
        </div>
      </div>

      {/* List */}
      <div style={{ flex: 1, overflowY: "auto", padding: "12px 16px", display: "flex", flexDirection: "column", gap: 10 }}>
        {loading && items.length === 0 ? (
          <div style={{ color: p.text3, fontSize: 13, textAlign: "center", marginTop: 40 }}>Loading…</div>
        ) : loadError && items.length === 0 ? (
          // F1: backend fetch failure renders a distinct error/retry state
          // (NOT the silent "No pending approvals" empty copy). Critical for
          // destructive-approvals review — a clean-looking inbox during a
          // backend outage would let destructive actions go unreviewed.
          <div
            role="alert"
            data-testid="inbox-load-error"
            style={{
              display: "flex",
              flexDirection: "column",
              alignItems: "center",
              gap: 10,
              marginTop: 40,
              padding: "0 16px",
            }}
          >
            <div style={{ color: p.failed, fontSize: 13, textAlign: "center" }}>{loadError}</div>
            <button
              type="button"
              onClick={load}
              aria-label="Retry loading pending requests"
              data-testid="inbox-retry"
              style={{
                padding: "6px 14px",
                borderRadius: 14,
                border: `0.5px solid ${p.failed}`,
                background: "transparent",
                color: p.failed,
                fontSize: 12,
                fontWeight: 600,
                cursor: "pointer",
                fontFamily: MOBILE_FONT_SANS,
              }}
            >
              Retry
            </button>
          </div>
        ) : items.length === 0 ? (
          <div style={{ color: p.text3, fontSize: 13, textAlign: "center", marginTop: 40 }}>
            {kind === "approval"
              ? "No pending approvals. Destructive actions await sign-off here."
              : "No pending tasks."}
          </div>
        ) : (
          items.map((r) => (
            <div
              key={r.id}
              data-testid="inbox-row"
              style={{
                background: p.surface,
                border: `0.5px solid ${p.border}`,
                borderRadius: 14,
                padding: 14,
                display: "flex",
                flexDirection: "column",
                gap: 8,
              }}
            >
              <div style={{ fontSize: 14, fontWeight: 600, color: p.text }}>{r.title}</div>
              {r.detail ? (
                <div style={{ fontSize: 12.5, color: p.text2, lineHeight: 1.4 }}>{r.detail}</div>
              ) : null}
              <div style={{ fontSize: 11, color: p.text3 }}>
                {(r.workspace_name || r.requester_id || "agent")}
                {r.status && r.status !== "pending" ? ` · ${r.status.replace(/_/g, " ")}` : ""}
              </div>
              <div style={{ display: "flex", gap: 8, marginTop: 4 }}>
                <button
                  type="button"
                  disabled={acting === r.id}
                  onClick={() => respond(r, r.kind === "approval" ? "approved" : "done")}
                  style={{
                    flex: 1,
                    padding: "9px 0",
                    borderRadius: 10,
                    border: "none",
                    background: p.green,
                    color: "#fff",
                    fontSize: 13,
                    fontWeight: 600,
                    cursor: acting === r.id ? "not-allowed" : "pointer",
                    opacity: acting === r.id ? 0.5 : 1,
                    fontFamily: MOBILE_FONT_SANS,
                  }}
                >
                  {r.kind === "approval" ? "Approve" : "Done"}
                </button>
                <button
                  type="button"
                  disabled={acting === r.id}
                  onClick={() => respond(r, "rejected")}
                  style={{
                    flex: 1,
                    padding: "9px 0",
                    borderRadius: 10,
                    border: `0.5px solid ${p.failed}`,
                    background: "transparent",
                    color: p.failed,
                    fontSize: 13,
                    fontWeight: 600,
                    cursor: acting === r.id ? "not-allowed" : "pointer",
                    opacity: acting === r.id ? 0.5 : 1,
                    fontFamily: MOBILE_FONT_SANS,
                  }}
                >
                  Reject
                </button>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
