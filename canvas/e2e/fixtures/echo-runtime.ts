/**
 * Minimal A2A echo runtime for E2E tests.
 *
 * Listens on an ephemeral port, receives A2A JSON-RPC `message/send`
 * requests, and returns a response with the original text echoed back.
 * Also implements the workspace-side chat upload ingest endpoint so
 * file-attachment E2E can exercise the full upload → send → echo
 * round-trip.
 *
 * Usage (inside test fixture):
 *   const echo = await startEchoRuntime();
 *   // ... seed workspace with agent_url pointing to echo.baseURL ...
 *   echo.stop();
 */

import { createServer, type Server } from "node:http";

export interface EchoRuntime {
  baseURL: string;
  stop: () => Promise<void>;
  lastRequest: { method: string; text: string; files: unknown[] } | null;
}

/** Parse a minimal multipart body and extract the first file's name + content. */
function parseMultipart(body: Buffer): { name: string; mimeType: string; content: Buffer } | null {
  // Find the boundary line (first line starting with "--").
  const str = body.toString("binary");
  const firstDash = str.indexOf("--");
  if (firstDash === -1) return null;
  const eol = str.indexOf("\r\n", firstDash);
  if (eol === -1) return null;
  const boundary = str.slice(firstDash + 2, eol);
  const boundaryMarker = "\r\n--" + boundary;

  // Find the first part that has a filename in Content-Disposition.
  let pos = eol + 2;
  while (pos < str.length) {
    const nextBoundary = str.indexOf(boundaryMarker, pos);
    if (nextBoundary === -1) break;
    const part = str.slice(pos, nextBoundary);

    const cdMatch = part.match(/Content-Disposition:[^\r\n]*filename="([^"]+)"/i);
    if (cdMatch) {
      const name = cdMatch[1];
      const ctMatch = part.match(/Content-Type:\s*([^\r\n]+)/i);
      const mimeType = ctMatch ? ctMatch[1].trim() : "application/octet-stream";
      // Body starts after the first double-CRLF in the part.
      const bodyStart = part.indexOf("\r\n\r\n");
      if (bodyStart !== -1) {
        // Extract the raw bytes (not the string) so binary is safe.
        const headerBytes = Buffer.byteLength(part.slice(0, bodyStart + 4), "binary");
        const partStartInBody = Buffer.byteLength(str.slice(0, pos + bodyStart + 4), "binary");
        const partEndInBody = Buffer.byteLength(str.slice(0, nextBoundary), "binary");
        const content = body.subarray(partStartInBody, partEndInBody);
        return { name, mimeType, content };
      }
    }
    pos = nextBoundary + boundaryMarker.length;
    // Skip trailing "--" (end marker) or CRLF.
    if (str.slice(pos, pos + 2) === "--") break;
    if (str.slice(pos, pos + 2) === "\r\n") pos += 2;
  }
  return null;
}

export async function startEchoRuntime(): Promise<EchoRuntime> {
  let lastRequest: EchoRuntime["lastRequest"] = null;

  const server = createServer((req, res) => {
    // CORS: allow the canvas origin (localhost:3000) to call us.
    res.setHeader("Access-Control-Allow-Origin", "*");
    res.setHeader("Access-Control-Allow-Methods", "POST, GET, OPTIONS");
    res.setHeader("Access-Control-Allow-Headers", "Content-Type, Authorization");

    if (req.method === "OPTIONS") {
      res.writeHead(204);
      res.end();
      return;
    }

    const url = req.url ?? "/";

    // Workspace-side chat upload ingest (RFC #2312).
    if (url === "/internal/chat/uploads/ingest" && req.method === "POST") {
      const chunks: Buffer[] = [];
      req.on("data", (chunk: Buffer) => chunks.push(chunk));
      req.on("end", () => {
        const body = Buffer.concat(chunks);
        const file = parseMultipart(body);
        if (!file) {
          res.writeHead(400);
          res.end(JSON.stringify({ error: "no files field" }));
          return;
        }
        const sanitized = file.name.replace(/[^a-zA-Z0-9._\-]/g, "_").replace(/ /g, "_");
        const prefix = Array.from({ length: 32 }, () =>
          Math.floor(Math.random() * 16).toString(16),
        ).join("");
        const response = {
          files: [
            {
              uri: `workspace:/workspace/.molecule/chat-uploads/${prefix}-${sanitized}`,
              name: sanitized,
              mimeType: file.mimeType,
              size: file.content.length,
            },
          ],
        };
        res.setHeader("Content-Type", "application/json");
        res.writeHead(200);
        res.end(JSON.stringify(response));
      });
      return;
    }

    // Default: A2A JSON-RPC handler.
    let body = "";
    req.setEncoding("utf8");
    req.on("data", (chunk: string) => {
      body += chunk;
    });
    req.on("end", () => {
      res.setHeader("Content-Type", "application/json");
      try {
        const rpc = JSON.parse(body);
        const msg = rpc.params?.message;
        const textParts =
          msg?.parts
            ?.filter((p: { kind?: string; text?: string }) => p.kind === "text")
            .map((p: { text?: string }) => p.text)
            .filter(Boolean) ?? [];
        const fileParts =
          msg?.parts?.filter((p: { kind?: string }) => p.kind === "file") ?? [];
        const text = textParts.join("\n");

        lastRequest = {
          method: rpc.method ?? "unknown",
          text,
          files: fileParts,
        };

        const replyText = text
          ? `Echo: ${text}`
          : fileParts.length > 0
            ? "Echo: received your file(s)."
            : "Echo: hello";

        const response = {
          jsonrpc: "2.0",
          id: rpc.id ?? null,
          result: {
            parts: [{ kind: "text", text: replyText }],
          },
        };

        res.writeHead(200);
        res.end(JSON.stringify(response));
      } catch {
        res.writeHead(400);
        res.end(JSON.stringify({ error: "invalid json" }));
      }
    });
  });

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  const port = typeof address === "object" && address ? address.port : 0;
  const baseURL = `http://127.0.0.1:${port}`;

  return {
    baseURL,
    stop: () =>
      new Promise((resolve) => {
        server.close(() => resolve(undefined));
      }),
    get lastRequest() {
      return lastRequest;
    },
  };
}
