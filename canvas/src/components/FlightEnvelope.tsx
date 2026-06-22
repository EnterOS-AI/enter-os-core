/** FlightEnvelope — a single envelope that animates from `from` to `to` and
 *  fades out, used by both the canvas (flow coords inside a ViewportPortal) and
 *  the concierge home (screen coords inside a fixed overlay). The parent owns
 *  the coordinate space; this component only animates the translate delta.
 *
 *  Uses the Web Animations API so the from/to delta can be dynamic per flight
 *  (a static CSS @keyframes can't translate to a runtime-computed point). */
import { useEffect, useRef } from "react";
import {
  BOUNCE_DURATION_MS,
  FLIGHT_DURATION_MS,
  RECEIVE_BOUNCE_DELAY_MS,
  type A2AFlightKind,
} from "@/hooks/useA2AFlights";

/** Stroke colour by activity kind — mirrors CommunicationOverlay's palette
 *  (send = cyan, receive = violet/accent, task = warm) so the two surfaces
 *  read as the same event. */
const KIND_COLOR: Record<A2AFlightKind, string> = {
  send: "#22d3ee",
  receive: "#8b5cf6",
  task: "#f5a623",
};

export interface Point {
  x: number;
  y: number;
}

/** EndpointBounce — a small bounce pulse rendered at a flight endpoint: the
 *  sender "flicks" the envelope on launch and the receiver "catches" it on
 *  landing (CTO ask, 2026-06-10 — both the canvas map and the concierge home
 *  render flights through this one component, so both surfaces get it).
 *
 *  A filled dot that overshoots (scale up past 1, settle, fade) with a ring
 *  expanding behind it — classic squash-free bounce that works at any zoom.
 *  Same WAAPI degrade posture as the envelope: no Element.animate → render
 *  nothing visible (opacity stays 0). */
function EndpointBounce({
  at,
  color,
  delayMs,
}: {
  at: Point;
  color: string;
  delayMs: number;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el || typeof el.animate !== "function") return;
    const dot = el.querySelector<SVGElement>("[data-bounce-dot]");
    const ring = el.querySelector<SVGElement>("[data-bounce-ring]");
    const anims: Animation[] = [];
    if (dot && typeof (dot as unknown as HTMLElement).animate === "function") {
      anims.push(
        (dot as unknown as HTMLElement).animate(
          [
            { transform: "scale(0.3)", opacity: 0 },
            { transform: "scale(1.45)", opacity: 0.95, offset: 0.35 }, // overshoot
            { transform: "scale(0.85)", opacity: 0.8, offset: 0.6 }, // bounce back
            { transform: "scale(1.1)", opacity: 0.5, offset: 0.8 }, // settle hop
            { transform: "scale(1)", opacity: 0 },
          ],
          { duration: BOUNCE_DURATION_MS, delay: delayMs, easing: "ease-out", fill: "both" },
        ),
      );
    }
    if (ring && typeof (ring as unknown as HTMLElement).animate === "function") {
      anims.push(
        (ring as unknown as HTMLElement).animate(
          [
            { transform: "scale(0.4)", opacity: 0.7 },
            { transform: "scale(1.9)", opacity: 0 },
          ],
          { duration: BOUNCE_DURATION_MS, delay: delayMs, easing: "ease-out", fill: "both" },
        ),
      );
    }
    return () => anims.forEach((a) => a.cancel());
  }, [delayMs]);

  return (
    <div
      ref={ref}
      data-testid="flight-endpoint-bounce"
      aria-hidden="true"
      style={{
        position: "absolute",
        left: at.x,
        top: at.y,
        width: 0,
        height: 0,
        pointerEvents: "none",
        zIndex: 5,
      }}
    >
      <svg
        width="28"
        height="28"
        viewBox="0 0 28 28"
        fill="none"
        aria-hidden="true"
        style={{ position: "absolute", left: -14, top: -14, overflow: "visible" }}
      >
        <circle
          data-bounce-ring
          cx="14"
          cy="14"
          r="9"
          stroke={color}
          strokeWidth="1.5"
          fill="none"
          opacity="0"
          style={{ transformOrigin: "14px 14px" }}
        />
        <circle
          data-bounce-dot
          cx="14"
          cy="14"
          r="4.5"
          fill={color}
          opacity="0"
          style={{ transformOrigin: "14px 14px" }}
        />
      </svg>
    </div>
  );
}

export function FlightEnvelope({
  from,
  to,
  kind,
}: {
  from: Point;
  to: Point;
  kind: A2AFlightKind;
}) {
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const el = ref.current;
    // Element.animate is unavailable in some test/SSR environments — degrade to
    // a static (instantly-finished) envelope rather than throw.
    if (!el || typeof el.animate !== "function") return;
    const dx = to.x - from.x;
    const dy = to.y - from.y;
    // Launch small from the source dot, GROW BIG as it crosses the gap (peak
    // mid-flight), then SHRINK small as it lands on the target dot — reads as an
    // envelope flung from one agent and received by the other. translate tracks
    // the straight path (fraction == keyframe offset); scale arcs independently.
    const at = (frac: number, scale: number, opacity: number, offset?: number) => ({
      transform: `translate(-50%,-50%) translate(${dx * frac}px,${dy * frac}px) scale(${scale})`,
      opacity,
      ...(offset === undefined ? {} : { offset }),
    });
    const anim = el.animate(
      [
        at(0, 0.5, 0),
        at(0.2, 1.25, 1, 0.2), // faded in + grown
        at(0.5, 1.7, 1, 0.5), // BIG at mid-flight
        at(0.82, 1.05, 1, 0.82), // shrinking on approach
        at(1, 0.5, 0), // small + faded out, arrived on the target dot
      ],
      { duration: FLIGHT_DURATION_MS, easing: "ease-in-out", fill: "forwards" },
    );
    return () => anim.cancel();
  }, [from.x, from.y, to.x, to.y]);

  const color = KIND_COLOR[kind];
  return (
    <>
      {/* sender flick: bounce at the launch point as the envelope departs */}
      <EndpointBounce at={from} color={color} delayMs={0} />
      {/* receiver catch: bounce at the landing point as the envelope arrives */}
      <EndpointBounce at={to} color={color} delayMs={RECEIVE_BOUNCE_DELAY_MS} />
      <div
        ref={ref}
        data-testid="flight-envelope"
      aria-hidden="true"
      style={{
        position: "absolute",
        left: from.x,
        top: from.y,
        pointerEvents: "none",
        willChange: "transform, opacity",
        filter: "drop-shadow(0 1px 3px rgba(0,0,0,0.45))",
        zIndex: 6,
      }}
    >
      <svg width="22" height="22" viewBox="0 0 24 24" fill="none" aria-hidden="true">
        <rect x="2.5" y="5.5" width="19" height="13" rx="2.5" fill="#0b0b0f" stroke={color} strokeWidth="1.6" />
        <path
          d="M3.5 7.5l8.5 6 8.5-6"
          stroke={color}
          strokeWidth="1.6"
          fill="none"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
        </svg>
      </div>
    </>
  );
}
