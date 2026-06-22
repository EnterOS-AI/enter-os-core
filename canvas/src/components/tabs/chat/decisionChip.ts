import type { ChatMessage } from "./types";

/** The REQUEST_RESPONDED payload the decision chip consumes. */
export interface RequestRespondedPayload {
  status: string;
  responderType: string;
  responderId: string;
  title: string;
  kind: string;
}

/** Decide whether a REQUEST_RESPONDED event should render a decision chip in
 *  the CURRENT user's My Chat, and which decision it is. Returns null when no
 *  chip should show.
 *
 *  Gate (core#2636, CR2 fix): only the user's OWN responses — never an agent
 *  response, and never another user's response in a multi-user org (showing
 *  someone else's decision as "You …" is wrong + a confusion/privacy risk).
 *  currentUserId is resolved the SAME way RequestsInbox sets responder_id
 *  (session user_id, "admin" placeholder when no session), so the ids match
 *  on the single-user path and correctly diverge per-user otherwise.
 */
export function decisionForChip(
  p: RequestRespondedPayload,
  currentUserId: string,
): ChatMessage["decision"] | null {
  if (p.responderType !== "user") return null;
  if (!p.responderId || p.responderId !== currentUserId) return null;
  switch (p.status) {
    case "approved":
      return "approved";
    case "rejected":
      return "rejected";
    case "done":
      return "done";
    default:
      return null;
  }
}

/** The chip's human label for a decision + optional request title. */
export function decisionChipText(decision: NonNullable<ChatMessage["decision"]>, title: string): string {
  const verb = decision === "approved" ? "approved" : decision === "rejected" ? "rejected" : "completed";
  return `You ${verb}${title ? ` “${title}”` : " the request"}`;
}
