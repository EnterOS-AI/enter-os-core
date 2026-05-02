export function extractAgentText(task: Record<string, unknown>): string {
  try {
    const directTexts = extractTextsFromParts(task.parts);
    if (directTexts) return directTexts;

    const artifacts = task.artifacts as Array<Record<string, unknown>> | undefined;
    if (artifacts && artifacts.length > 0) {
      const texts = extractTextsFromParts(artifacts[0].parts);
      if (texts) return texts;
    }

    const status = task.status as Record<string, unknown> | undefined;
    if (status?.message) {
      const msg = status.message as Record<string, unknown>;
      const texts = extractTextsFromParts(msg.parts);
      if (texts) return texts;
    }

    if (typeof task === "string") return task;
    return "(Could not extract response text)";
  } catch {
    return "(Failed to parse response)";
  }
}

export function extractTextsFromParts(parts: unknown): string | null {
  if (!Array.isArray(parts)) return null;
  const texts = parts
    .filter((p: Record<string, unknown>) => p.type === "text" || p.kind === "text")
    .map((p: Record<string, unknown>) => String(p.text || ""))
    .filter(Boolean);
  return texts.length > 0 ? texts.join("\n") : null;
}

export interface ParsedFilePart {
  name: string;
  uri: string;
  mimeType?: string;
  size?: number;
}

/** Extract file parts from an A2A response. Walks parts[] + artifacts[].
 *
 *  Hot path: v0 Pydantic shape `{ kind: "file", file: { name, mimeType,
 *  uri } }` — what every current workspace runtime emits.
 *
 *  Defensive secondary path: v1 protobuf shape `{ url, filename,
 *  mediaType }` — flat, no `kind`, no nested `file`. Not currently
 *  observed on the wire (a2a-sdk's JSON-RPC layer still validates
 *  against v0), but kept so a future SDK release that flips the wire
 *  shape, or a third-party agent that round-trips through protobuf
 *  serialization, doesn't silently lose file chips.
 *
 *  We only surface parts that carry a URL — inline bytes would require
 *  a different renderer (data URL) and are out of scope for MVP. Names
 *  fall back to the URL's basename when absent. */
export function extractFilesFromTask(task: Record<string, unknown>): ParsedFilePart[] {
  const out: ParsedFilePart[] = [];
  const pushFromParts = (parts: unknown) => {
    if (!Array.isArray(parts)) return;
    for (const raw of parts as Array<Record<string, unknown>>) {
      const isV0File = raw.kind === "file" || raw.type === "file";
      const v1Url = typeof raw.url === "string" ? raw.url : "";
      if (!isV0File && !v1Url) continue;

      let uri = "";
      let name = "";
      let mimeType: string | undefined;
      let size: number | undefined;

      if (isV0File) {
        const file = (raw.file ?? raw) as Record<string, unknown>;
        uri = typeof file.uri === "string" ? file.uri : "";
        if (!uri) continue;
        name = (typeof file.name === "string" && file.name) || basename(uri);
        mimeType = typeof file.mimeType === "string" ? file.mimeType : undefined;
        size = typeof file.size === "number" ? file.size : undefined;
      } else {
        // v1 flat shape: url + filename + mediaType (camelCase from
        // protobuf JSON serialization of media_type).
        uri = v1Url;
        const v1Name = typeof raw.filename === "string" ? raw.filename : "";
        name = v1Name || basename(uri);
        mimeType = typeof raw.mediaType === "string" ? raw.mediaType : undefined;
      }

      out.push({ name, uri, mimeType, size });
    }
  };
  try {
    pushFromParts(task.parts);
    const artifacts = task.artifacts as Array<Record<string, unknown>> | undefined;
    if (artifacts) for (const a of artifacts) pushFromParts(a.parts);
    const status = task.status as Record<string, unknown> | undefined;
    if (status?.message) {
      const msg = status.message as Record<string, unknown>;
      pushFromParts(msg.parts);
    }
    // Some A2A servers wrap a non-task reply as
    // {result: {message: {parts: [...]}}} rather than {result: {parts}}.
    // Without this branch we'd silently drop file parts returned by
    // third-party implementations.
    const message = task.message as Record<string, unknown> | undefined;
    if (message) pushFromParts(message.parts);
  } catch {
    /* tolerate malformed shapes — chat falls through to text-only */
  }
  return out;
}

function basename(uri: string): string {
  const cleaned = uri.replace(/^workspace:/, "").replace(/^https?:\/\//, "");
  const slash = cleaned.lastIndexOf("/");
  return slash >= 0 ? cleaned.slice(slash + 1) : cleaned || "file";
}

/** Extract user message text from an activity log request_body */
export function extractRequestText(body: Record<string, unknown> | null): string {
  if (!body) return "";
  const params = body.params as Record<string, unknown> | undefined;
  const msg = params?.message as Record<string, unknown> | undefined;
  const parts = msg?.parts as Array<Record<string, unknown>> | undefined;
  return (parts?.[0]?.text as string) || "";
}

/** Extract text from an activity log response_body (multiple possible formats).
 *
 *  Collects from EVERY source — top-level `parts[].text`, `parts[].root.text`
 *  (older nested shape), and `artifacts[].parts[].text` (task-shaped
 *  replies) — and joins them with "\n". Two reasons to collect rather
 *  than early-return:
 *
 *    1. Claude Code and other long-reply runtimes emit multiple text
 *       parts in a single `parts` array. Returning just the first
 *       silently truncates 15k-char briefs to their leading line
 *       (observed UX A/B Lab Wave 1, 2026-04-25).
 *
 *    2. Some producers emit a summary in `parts[].text` AND details in
 *       `artifacts[].parts[].text` (Hermes does this for tool calls).
 *       The previous "first source wins" returned only the summary;
 *       artifacts dropped silently. */
export function extractResponseText(body: Record<string, unknown>): string {
  try {
    // {result: "text"} — from MCP server delegation logs
    if (typeof body.result === "string") return body.result;

    const result = body.result as Record<string, unknown> | undefined;
    if (result) {
      const collected: string[] = [];

      // A2A JSON-RPC: {result: {parts: [{kind: "text", text: "..."}]}}
      const fromParts = extractTextsFromParts(result.parts);
      if (fromParts) collected.push(fromParts);

      // Older nested shape: {parts: [{root: {text: "..."}}]}
      const parts = (result.parts || []) as Array<Record<string, unknown>>;
      const rootTexts: string[] = [];
      for (const p of parts) {
        const root = p.root as Record<string, unknown> | undefined;
        if (root?.text) rootTexts.push(root.text as string);
      }
      if (rootTexts.length > 0) collected.push(rootTexts.join("\n"));

      // Task shape: {result: {artifacts: [{parts: [...]}]}}
      const artifacts = result.artifacts as Array<Record<string, unknown>> | undefined;
      if (artifacts) {
        for (const a of artifacts) {
          const t = extractTextsFromParts(a.parts);
          if (t) collected.push(t);
        }
      }

      if (collected.length > 0) return collected.join("\n");
    }

    // {task: "text"} — request body format, shouldn't be in response but handle it
    if (typeof body.task === "string") return body.task;
  } catch { /* ignore */ }
  return "";
}
