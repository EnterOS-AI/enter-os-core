/**
 * React Flow className helpers shared across the store and canvas
 * hooks. React Flow's Node.className / Edge.className is a single
 * space-separated string, so every call site was previously doing
 * the same `.split/.filter/.join` dance — centralise it here so
 * any future class manipulation follows one policy.
 */

/** Add `cls` to the existing className, de-duplicating. Returns
 *  the (possibly new) string; undefined/empty input → just `cls`. */
export function appendClass(existing: string | undefined, cls: string): string {
  if (!existing) return cls;
  const parts = existing.split(/\s+/).filter(Boolean);
  if (parts.includes(cls)) return existing;
  parts.push(cls);
  return parts.join(" ");
}

/** Remove `cls` if present. Returns the (possibly empty) string. */
export function removeClass(existing: string | undefined, cls: string): string {
  if (!existing) return "";
  return existing
    .split(/\s+/)
    .filter((c) => c && c !== cls)
    .join(" ");
}

/** Schedule `removeClass(nodeId, cls)` on the `nodes` slice after
 *  `delayMs`. The callers used to inline this twice — once for
 *  parent-pulse cleanup, once for spawn-class cleanup — and now
 *  share the same impl so future one-shot animation classes land
 *  consistently.
 *
 *  No-ops when `window` is undefined (SSR). Accepts the store's
 *  get/set pair directly rather than a store reference so it
 *  composes with the existing handleCanvasEvent signature. */
export function scheduleNodeClassRemoval(
  nodeId: string,
  cls: string,
  delayMs: number,
  get: () => { nodes: Array<{ id: string; className?: string }> },
  set: (partial: Record<string, unknown>) => void,
): void {
  if (typeof window === "undefined") return;
  window.setTimeout(() => {
    const state = get();
    set({
      nodes: state.nodes.map((n) =>
        n.id === nodeId ? { ...n, className: removeClass(n.className, cls) } : n,
      ),
    });
  }, delayMs);
}
