import { type ChatMessage, createMessage } from "./types";
import { extractResponseText, extractRequestText, extractFilesFromTask } from "./message-parser";

/** Activity row shape the chat history loader consumes. Only the fields
 *  it actually reads are listed — the platform sends more (id, target_id,
 *  method, summary, etc.) but the hydration is defined by these four. */
export interface ActivityRowForHydration {
  activity_type: string;
  status: string;
  created_at: string;
  request_body: Record<string, unknown> | null;
  response_body: Record<string, unknown> | null;
}

/** Map a single activity_logs row to the chat messages it represents.
 *
 *  An a2a_receive row can produce up to two messages:
 *    1. A user-side bubble derived from request_body (the message the
 *       user sent), unless the request was an internal self-message.
 *    2. An agent-side bubble derived from response_body (text +
 *       file attachments), with role=system when status=error.
 *
 *  CRITICAL: both messages MUST adopt `row.created_at` as their
 *  timestamp. createMessage() defaults to new Date() — appropriate for
 *  freshly-typed messages, wrong for hydrated history because every
 *  reload would re-stamp every bubble to the render moment. The
 *  regression that prompted extracting this helper showed up as every
 *  user message in the chat collapsing to the same "now" clock after
 *  reload (see test_user_messages_pin_timestamps_to_created_at).
 */
export function activityRowToMessages(
  row: ActivityRowForHydration,
  isInternalSelfMessage: (text: string) => boolean,
): ChatMessage[] {
  const out: ChatMessage[] = [];

  const userText = extractRequestText(row.request_body);
  // Hydrate user-side file attachments out of the same A2A envelope.
  // Without this, a chat reload after a session where the user dragged
  // in a file shows the text bubble but loses the download chip — the
  // pre-fix loader only walked text via extractRequestText. Mirrors
  // the agent branch below. Wire shape from ChatTab's outbound POST:
  //   request_body = {params: {message: {parts: [{kind:"text"}, {kind:"file", file:{...}}]}}}
  // extractFilesFromTask walks `task.parts`, so we feed it `params.message`.
  const userMsg = (row.request_body?.params as Record<string, unknown> | undefined)
    ?.message as Record<string, unknown> | undefined;
  const userAttachments = userMsg ? extractFilesFromTask(userMsg) : [];
  // Internal-self messages (e.g. heartbeat self-trigger) take precedence
  // — drop the row even if it carries attachments, since the heartbeat
  // path doesn't produce attachments anyway and keeping the bubble would
  // misattribute it to the user.
  const isInternal = !!userText && isInternalSelfMessage(userText);
  if (!isInternal && (userText || userAttachments.length > 0)) {
    out.push({
      ...createMessage("user", userText, userAttachments),
      timestamp: row.created_at,
    });
  }

  if (row.response_body) {
    const text = extractResponseText(row.response_body);
    // Pick the right object to feed extractFilesFromTask:
    //   - Task-shape:   {result: {parts: [...]}}        → unwrap result
    //   - Notify-shape: {result: "<text>", parts: [...]} → use the body
    // Naively doing `result ?? body` would pass the string "<text>" to
    // the file extractor for the notify case, returning [] and dropping
    // the file chips on reload. Only unwrap when result is an object.
    const filesSource: Record<string, unknown> =
      row.response_body.result && typeof row.response_body.result === "object"
        ? (row.response_body.result as Record<string, unknown>)
        : row.response_body;
    const attachments = extractFilesFromTask(filesSource);
    if (text || attachments.length > 0) {
      const role: ChatMessage["role"] =
        row.status === "error" || text.toLowerCase().startsWith("agent error")
          ? "system"
          : "agent";
      out.push({ ...createMessage(role, text, attachments), timestamp: row.created_at });
    }
  }

  return out;
}
