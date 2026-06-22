/** MessageFlightHome — the concierge-home counterpart of MessageFlightLayer.
 *  The home view is a vertical agent tree (not a spatial canvas), so an envelope
 *  flies between the source and target agent ROWS. It shares the exact same
 *  flight stream (useA2AFlights) as the canvas, and resolves endpoints from each
 *  row's DOM rect (rows carry data-ws-id). Reduced-motion is honoured by the
 *  shared hook (it emits no flights). */
import { useRef } from "react";
import { useA2AFlights, type A2AFlight } from "@/hooks/useA2AFlights";
import { FlightEnvelope, type Point } from "../FlightEnvelope";

function rowCenter(wsId: string): Point | null {
  if (typeof document === "undefined") return null;
  const sel =
    typeof CSS !== "undefined" && typeof CSS.escape === "function"
      ? CSS.escape(wsId)
      : wsId;
  const el = document.querySelector<HTMLElement>(`[data-ws-id="${sel}"]`);
  if (!el) return null;
  const r = el.getBoundingClientRect();
  return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
}

/** One flight. Captures the source/target row rects ONCE on mount (a ref, not
 *  per-render) so a later re-render or scroll mid-flight does not restart the
 *  animation. */
function HomeFlight({ flight }: { flight: A2AFlight }) {
  const pos = useRef<{ from: Point; to: Point } | null>(null);
  if (pos.current === null) {
    const from = rowCenter(flight.sourceId);
    const to = rowCenter(flight.targetId);
    if (from && to) pos.current = { from, to };
  }
  if (!pos.current) return null; // one or both agents not visible in the tree
  return <FlightEnvelope from={pos.current.from} to={pos.current.to} kind={flight.kind} />;
}

export function MessageFlightHome() {
  const flights = useA2AFlights();
  if (flights.length === 0) return null;
  return (
    <div
      aria-hidden="true"
      style={{ position: "fixed", inset: 0, pointerEvents: "none", zIndex: 50 }}
    >
      {flights.map((f) => (
        <HomeFlight key={f.key} flight={f} />
      ))}
    </div>
  );
}
