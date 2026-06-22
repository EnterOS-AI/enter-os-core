/**
 * E2E seed fixture for chat tests.
 *
 * Creates an external workspace via the workspace-server API, extracts the
 * auto-minted auth token, then overrides the DB row so it appears "online"
 * with an echo-runtime URL.  External runtime is used because the health
 * sweep skips Docker checks for external workspaces; we keep the workspace
 * alive with periodic heartbeats.
 */

import { randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";

const PLATFORM_URL = process.env.E2E_PLATFORM_URL ?? "http://localhost:8080";

interface PgCredentials {
  user: string;
  pass: string;
  host: string;
  port: string;
  db: string;
}

export function parseDbUrl(): PgCredentials | null {
  const dbUrl = process.env.E2E_DATABASE_URL;
  if (!dbUrl) return null;
  const pgRegex = /postgres:\/\/([^:]+):([^@]+)@([^:]+):(\d+)\/([^?]+)/;
  const m = dbUrl.match(pgRegex);
  if (!m) return null;
  const [, user, pass, host, port, db] = m;
  return { user, pass, host, port, db };
}

export function runPsql(sql: string, timeoutMs = 30_000): string {
  const creds = parseDbUrl();
  if (!creds) {
    throw new Error("E2E_DATABASE_URL must be set for DB seeding");
  }
  const { user, pass, host, port, db } = creds;
  const out = execFileSync(
    "psql",
    ["-h", host, "-p", port, "-U", user, "-d", db, "-c", sql],
    {
      env: { ...process.env, PGPASSWORD: pass },
      stdio: "pipe",
      timeout: timeoutMs,
      encoding: "utf8",
    },
  );
  return out.toString();
}

/**
 * Execute a read-only psql query and return each row parsed as JSON.
 * The caller is responsible for making the query return exactly one JSON
 * value per output line (e.g. with `row_to_json` or `jsonb_agg`).
 */
export function queryPsql<T>(sql: string, timeoutMs = 30_000): T[] {
  const creds = parseDbUrl();
  if (!creds) {
    throw new Error("E2E_DATABASE_URL must be set for DB seeding");
  }
  const { user, pass, host, port, db } = creds;
  const out = execFileSync(
    "psql",
    ["-h", host, "-p", port, "-U", user, "-d", db, "-tA", "-c", sql],
    {
      env: { ...process.env, PGPASSWORD: pass },
      stdio: "pipe",
      timeout: timeoutMs,
      encoding: "utf8",
    },
  );
  return out
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.length > 0)
    .map((line) => JSON.parse(line) as T);
}

export interface SeededWorkspace {
  id: string;
  name: string;
  agentURL: string;
  authToken: string;
}

/**
 * Create an external workspace and wire it to the echo runtime.
 */
export async function seedWorkspace(echoURL: string): Promise<SeededWorkspace> {
  // 1. Create external workspace pointing at the in-process echo runtime.
  const runId = Math.random().toString(36).slice(2, 8);
  const wsName = `Chat E2E Agent ${runId}`;
  const adminToken = process.env.E2E_ADMIN_TOKEN ?? process.env.ADMIN_TOKEN;
  const createRes = await fetch(`${PLATFORM_URL}/workspaces`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...(adminToken ? { Authorization: `Bearer ${adminToken}` } : {}),
    },
    body: JSON.stringify({
      name: wsName,
      tier: 1,
      external: true,
      runtime: "external",
      url: echoURL,
      delivery_mode: "push",
    }),
  });
  if (!createRes.ok) {
    const text = await createRes.text();
    throw new Error(`Failed to create workspace: ${createRes.status} ${text}`);
  }
  const ws = (await createRes.json()) as {
    id: string;
    name: string;
    connection?: { auth_token?: string };
  };
  let authToken = ws.connection?.auth_token;
  if (!authToken) {
    authToken = await mintWorkspaceToken(ws.id);
  }
  if (!authToken) {
    throw new Error("Workspace created but no auth_token returned");
  }

  // 2. Direct DB update: mark online + point url at echo runtime.
  //    The platform blocks loopback URLs at the API layer (SSRF guard),
  //    so we bypass via psql for local E2E.
  if (!process.env.E2E_DATABASE_URL) {
    throw new Error("E2E_DATABASE_URL must be set for DB seeding");
  }

  // Pre-seed a platform_inbound_secret so chat file uploads don't trigger
  // the lazy-heal 503 "retry in 30 s" path on first use.
  const inboundSecret = Array.from({ length: 43 }, () =>
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[
      Math.floor(Math.random() * 64)
    ],
  ).join("");

  runPsql(
    `UPDATE workspaces SET status = 'online', url = '${echoURL}', platform_inbound_secret = '${inboundSecret}', delivery_mode = 'push' WHERE id = '${ws.id}'`,
  );

  cacheWorkspaceURL(ws.id, echoURL);

  return { id: ws.id, name: wsName, agentURL: echoURL, authToken };
}

function cacheWorkspaceURL(workspaceId: string, agentURL: string): void {
  const redisContainer = process.env.REDIS_CONTAINER;
  if (!redisContainer) return;

  const keys = [`ws:${workspaceId}:url`, `ws:${workspaceId}:internal_url`];
  for (const key of keys) {
    try {
      execFileSync(
        "docker",
        ["exec", redisContainer, "redis-cli", "SET", key, agentURL],
        { stdio: "pipe", timeout: 10_000 },
      );
    } catch (err) {
      throw new Error(`Redis URL cache update failed for ${key}: ${err}`);
    }
  }
}

/**
 * Start a heartbeat interval that keeps an external workspace alive.
 * Returns a stop function.
 */
export function startHeartbeat(
  workspaceId: string,
  authToken: string,
  intervalMs = 30_000,
): () => void {
  const send = () => {
    fetch(`${PLATFORM_URL}/registry/heartbeat`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${authToken}`,
      },
      body: JSON.stringify({
        workspace_id: workspaceId,
        error_rate: 0,
        sample_error: "",
        active_tasks: 0,
        current_task: "",
        uptime_seconds: 0,
      }),
    }).catch(() => {});
  };

  // Send immediately so the first heartbeat lands before the stale sweep.
  send();
  const timer = setInterval(send, intervalMs);

  return () => clearInterval(timer);
}

/**
 * Seed chat-history rows for a workspace.
 *
 * Chat history is read from activity_logs via messagestore.MessageStore
 * (workspace-server/internal/messagestore/postgres_store.go). We insert
 * a2a_receive rows with source_id NULL (canvas-origin) so the
 * /chat-history hydrator picks them up. Each message becomes its own row
 * so arbitrary user/agent sequences can be seeded.
 *
 * The JSON payloads are passed through psql as dollar-quoted literals so
 * message text containing quotes, backslashes, or other special characters
 * is preserved exactly and cannot break the SQL string escaping.
 */
export async function seedChatHistory(
  workspaceId: string,
  messages: Array<{ role: "user" | "agent"; content: string }>,
): Promise<void> {
  // Fail-closed: this is a setup helper, not a test. Silently returning when
  // the DB is unavailable would make downstream assertions pass vacuously
  // (false-green) — the spec must fail if it cannot seed its fixtures.
  if (!process.env.E2E_DATABASE_URL) {
    throw new Error("E2E_DATABASE_URL must be set for chat-history seeding");
  }

  const rows = messages
    .map((msg, i) => {
      const offsetSec = messages.length - i;
      const requestBody =
        msg.role === "user"
          ? JSON.stringify({
              params: {
                message: {
                  parts: [{ kind: "text", text: msg.content }],
                },
              },
            })
          : "{}";
      const responseBody =
        msg.role === "agent"
          ? JSON.stringify({
              result: {
                parts: [{ kind: "text", text: msg.content }],
              },
            })
          : "{}";

      // Use a per-row random dollar-quoting tag so the message content
      // cannot accidentally close the literal.
      const tag = `J${randomUUID().replace(/[^A-Za-z0-9]/g, "")}`;
      const reqLit = `$${tag}$${requestBody}$${tag}$`;
      const respLit = `$${tag}$${responseBody}$${tag}$`;

      return `('${randomUUID()}', '${workspaceId}', 'a2a_receive', NULL, NULL, 'message/send', NULL, ${reqLit}::jsonb, ${respLit}::jsonb, 0, 'ok', NOW() - INTERVAL '${offsetSec} seconds')`;
    })
    .join(",");

  const sql = `INSERT INTO activity_logs (id, workspace_id, activity_type, source_id, target_id, method, summary, request_body, response_body, duration_ms, status, created_at) VALUES ${rows};`;

  runPsql(sql, 10_000);
}

/**
 * Delete a seeded workspace row directly from the DB.
 * Uses psql (same credentials as seedWorkspace) so we bypass any
 * workspace-server side-effects (container stop, cascade cleanup, etc.)
 * that can race or 500 on external workspaces.
 */
export async function cleanupWorkspace(workspaceId: string): Promise<void> {
  if (!process.env.E2E_DATABASE_URL) return;

  try {
    runPsql(`DELETE FROM workspaces WHERE id = '${workspaceId}'`);
  } catch {
    // Best-effort cleanup; don't fail the test suite if the row is already gone.
  }
}

/**
 * Mint a workspace auth token so the canvas can make authenticated API
 * calls (WorkspaceAuth middleware).
 */
export async function mintWorkspaceToken(workspaceId: string): Promise<string> {
  const headers: Record<string, string> = {};
  const adminToken = process.env.E2E_ADMIN_TOKEN ?? process.env.ADMIN_TOKEN;
  if (adminToken) {
    headers.Authorization = `Bearer ${adminToken}`;
  }
  const res = await fetch(`${PLATFORM_URL}/admin/workspaces/${workspaceId}/tokens`, {
    method: "POST",
    headers,
  });
  if (!res.ok) {
    throw new Error(`Failed to mint workspace token: ${res.status}`);
  }
  const data = (await res.json()) as { auth_token: string };
  return data.auth_token;
}
