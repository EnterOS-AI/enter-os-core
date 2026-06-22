/** useA2AFlights — turns the org's live A2A activity stream into transient
 *  "flights" (one per delegate / message event, source → target) that an
 *  overlay can animate as an envelope travelling between two agents.
 *
 *  This hook owns ONLY the event→flight lifecycle: it subscribes to the same
 *  ACTIVITY_LOGGED WS bus the CommunicationOverlay uses, keeps a small bounded
 *  list of in-flight envelopes, and expires each after the animation window.
 *  The caller resolves positions and renders the envelope, so the exact same
 *  flight data drives both the spatial canvas (flow coords) and the concierge
 *  home (DOM row rects).
 *
 *  Honours `prefers-reduced-motion`: when the user opts out of motion the hook
 *  emits no flights at all, so no envelope ever animates. */
import { useEffect, useRef, useState } from "react";
import { useSocketEvent } from "@/hooks/useSocketEvent";

export type A2AFlightKind = "send" | "receive" | "task";

export interface A2AFlight {
  /** unique per flight instance (not per pair) so a burst renders distinct envelopes */
  key: string;
  sourceId: string;
  targetId: string;
  kind: A2AFlightKind;
}

/** Total time an envelope is alive (ms). Kept in sync with the overlay's
 *  Web-Animations duration; the extra tail gives the fade-out room to finish
 *  before the element unmounts. */
export const FLIGHT_DURATION_MS = 1200;
// Endpoint-bounce timing (FlightEnvelope's EndpointBounce): the sender bounce
// fires at launch; the receiver bounce fires on the final approach. Living
// here (not FlightEnvelope.tsx) keeps the import direction one-way.
export const BOUNCE_DURATION_MS = 420;
export const RECEIVE_BOUNCE_DELAY_MS = Math.round(FLIGHT_DURATION_MS * 0.82);
// TTL must outlive the LAST animation on the flight — the receiver's landing
// bounce — not just the envelope traversal, or the layer unmounts mid-catch.
const FLIGHT_TTL_MS = RECEIVE_BOUNCE_DELAY_MS + BOUNCE_DURATION_MS + 120;

/** Cap concurrent envelopes so a delegation storm can't spawn unbounded DOM. */
const MAX_CONCURRENT = 12;

function reducedMotionNow(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches
  );
}

export function useA2AFlights(enabled = true): A2AFlight[] {
  const [flights, setFlights] = useState<A2AFlight[]>([]);
  const reduced = useRef<boolean>(reducedMotionNow());
  const timers = useRef<number[]>([]);

  // Track reduced-motion preference changes live (a user can toggle it mid-session).
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia("(prefers-reduced-motion: reduce)");
    const onChange = () => {
      reduced.current = mq.matches;
      if (mq.matches) setFlights([]); // drop any in-flight envelopes immediately
    };
    mq.addEventListener?.("change", onChange);
    return () => mq.removeEventListener?.("change", onChange);
  }, []);

  // Clear pending expiry timers on unmount.
  useEffect(() => {
    const t = timers.current;
    return () => {
      t.forEach((id) => window.clearTimeout(id));
    };
  }, []);

  useSocketEvent((msg) => {
    if (!enabled || reduced.current) return;
    if (msg.event !== "ACTIVITY_LOGGED") return;

    const p = (msg.payload || {}) as {
      activity_type?: string;
      source_id?: string | null;
      target_id?: string | null;
    };
    const t = p.activity_type;
    if (t !== "a2a_send" && t !== "a2a_receive" && t !== "task_update") return;

    const sourceId = p.source_id || msg.workspace_id;
    const targetId = p.target_id || "";
    // A flight needs two distinct endpoints; a self-loop or missing peer has
    // nowhere to fly, so skip it.
    if (!sourceId || !targetId || sourceId === targetId) return;

    const kind: A2AFlightKind =
      t === "a2a_receive" ? "receive" : t === "task_update" ? "task" : "send";
    const key = `${msg.timestamp || Date.now()}:${sourceId}:${targetId}:${Math.random()
      .toString(36)
      .slice(2, 8)}`;

    setFlights((prev) => [...prev.slice(-(MAX_CONCURRENT - 1)), { key, sourceId, targetId, kind }]);

    const id = window.setTimeout(() => {
      setFlights((prev) => prev.filter((f) => f.key !== key));
      timers.current = timers.current.filter((x) => x !== id);
    }, FLIGHT_TTL_MS);
    timers.current.push(id);
  });

  return flights;
}
