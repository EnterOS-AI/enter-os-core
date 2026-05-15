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

const PLATFORM_URL = process.env.E2E_PLATFORM_URL ?? "http://localhost:8080";

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
  // 1. Create external workspace (no URL — platform will mint an auth token).
  const runId = Math.random().toString(36).slice(2, 8);
  const wsName = `Chat E2E Agent ${runId}`;
  const createRes = await fetch(`${PLATFORM_URL}/workspaces`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name: wsName, tier: 1, external: true, runtime: "external" }),
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
  const authToken = ws.connection?.auth_token;
  if (!authToken) {
    throw new Error("Workspace created but no auth_token returned");
  }

  // 2. Direct DB update: mark online + point url at echo runtime.
  //    The platform blocks loopback URLs at the API layer (SSRF guard),
  //    so we bypass via psql for local E2E.
  const dbUrl = process.env.E2E_DATABASE_URL;
  if (!dbUrl) {
    throw new Error("E2E_DATABASE_URL must be set for DB seeding");
  }
  const pgRegex = /postgres:\/\/([^:]+):([^@]+)@([^:]+):(\d+)\/([^?]+)/;
  const m = dbUrl.match(pgRegex);
  if (!m) {
    throw new Error(`Cannot parse E2E_DATABASE_URL: ${dbUrl}`);
  }
  const [, user, pass, host, port, db] = m;

  // Pre-seed a platform_inbound_secret so chat file uploads don't trigger
  // the lazy-heal 503 "retry in 30 s" path on first use.
  const inboundSecret = Array.from({ length: 43 }, () =>
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"[
      Math.floor(Math.random() * 64)
    ],
  ).join("");

  const psql = [
    `PGPASSWORD=${pass} psql`,
    `-h ${host} -p ${port} -U ${user} -d ${db}`,
    `-c "UPDATE workspaces SET status = 'online', url = '${echoURL}', platform_inbound_secret = '${inboundSecret}' WHERE id = '${ws.id}'"`,
  ].join(" ");

  const { execSync } = await import("node:child_process");
  try {
    execSync(psql, { stdio: "pipe", timeout: 30_000 });
  } catch (err) {
    throw new Error(`DB update failed: ${err}`);
  }

  return { id: ws.id, name: wsName, agentURL: echoURL, authToken };
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
 */
export async function seedChatHistory(
  workspaceId: string,
  messages: Array<{ role: "user" | "agent"; content: string }>,
): Promise<void> {
  const dbUrl = process.env.E2E_DATABASE_URL;
  if (!dbUrl) return;

  const pgRegex = /postgres:\/\/([^:]+):([^@]+)@([^:]+):(\d+)\/([^?]+)/;
  const m = dbUrl.match(pgRegex);
  if (!m) return;
  const [, user, pass, host, port, db] = m;

  const values = messages
    .map(
      (msg, i) =>
        `('${randomUUID()}', '${workspaceId}', '${msg.role}', '${msg.content.replace(/'/g, "''")}', NOW() - INTERVAL '${messages.length - i} seconds')`,
    )
    .join(",");

  const sql = `INSERT INTO chat_messages (id, workspace_id, role, content, created_at) VALUES ${values};`;

  const { execSync } = await import("node:child_process");
  const psql = `PGPASSWORD=${pass} psql -h ${host} -p ${port} -U ${user} -d ${db} -c "${sql}"`;
  execSync(psql, { stdio: "pipe", timeout: 10_000 });
}

/**
 * Delete a seeded workspace row directly from the DB.
 * Uses psql (same credentials as seedWorkspace) so we bypass any
 * workspace-server side-effects (container stop, cascade cleanup, etc.)
 * that can race or 500 on external workspaces.
 */
export async function cleanupWorkspace(workspaceId: string): Promise<void> {
  const dbUrl = process.env.E2E_DATABASE_URL;
  if (!dbUrl) return;

  const pgRegex = /postgres:\/\/([^:]+):([^@]+)@([^:]+):(\d+)\/([^?]+)/;
  const m = dbUrl.match(pgRegex);
  if (!m) return;
  const [, user, pass, host, port, db] = m;

  const psql = `PGPASSWORD=${pass} psql -h ${host} -p ${port} -U ${user} -d ${db} -c "DELETE FROM workspaces WHERE id = '${workspaceId}'"`;

  const { execSync } = await import("node:child_process");
  try {
    execSync(psql, { stdio: "pipe", timeout: 30_000 });
  } catch {
    // Best-effort cleanup; don't fail the test suite if the row is already gone.
  }
}

/**
 * Mint a workspace auth token so the canvas can make authenticated API
 * calls (WorkspaceAuth middleware).
 */
export async function mintTestToken(workspaceId: string): Promise<string> {
  const res = await fetch(
    `${PLATFORM_URL}/admin/workspaces/${workspaceId}/test-token`,
  );
  if (!res.ok) {
    throw new Error(`Failed to mint test token: ${res.status}`);
  }
  const data = (await res.json()) as { auth_token: string };
  return data.auth_token;
}
