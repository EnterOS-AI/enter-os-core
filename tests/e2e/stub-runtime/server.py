#!/usr/bin/env python3
"""Minimal stub runtime for the local Docker-provisioner lifecycle e2e.

This is NOT a real agent — it carries no LLM, no claude-code SDK, no plugin
host. Its only job is to satisfy the platform's runtime<->platform contract so
the `test_local_provision_lifecycle_e2e.sh` harness can prove the LOCAL Docker
provisioner can provision a workspace, bring it online, SURVIVE A RESTART
(reusing the config volume), and route an A2A `message/send` through the
platform proxy — all WITHOUT building/booting the 2.5GB real claude-code image.

Contract it replicates (discovered from workspace-server):

  * registration is done BY the runtime container on boot (NOT the provisioner).
    The provisioner only sets status=provisioning + pre-stores the host URL; the
    container must POST /registry/register itself, and the heartbeat loop is what
    transitions provisioning -> online (registry.go evaluateStatus, #1784).

  * env vars the real entrypoint reads, injected by buildContainerEnv():
        WORKSPACE_ID   - this workspace's UUID
        PLATFORM_URL   - canonical platform base URL (e.g. http://platform:8080)
    We read exactly those (with WORKSPACE_CONFIG_PATH for the config.yaml probe).

  * POST {PLATFORM_URL}/registry/register
        body: {"id", "url", "agent_card":{"name","skills":[]}}
        - url MUST be push-routable. The provisioner runs the platform inside
          Docker, so it rewrites a stored http://127.0.0.1:<port> URL to the
          container-DNS form http://ws-<id[:12]>:8000 before proxying
          (a2a_proxy.go resolveAgentURL). We register our OWN container-DNS URL
          (http://<hostname>:8000) so SSRF validation passes in SaaS mode AND the
          proxy can reach us; in self-hosted (non-saas) mode RFC-1918 is blocked,
          so we fall back to registering by the ws-<id> alias hostname which
          resolves on molecule-core-net.
        - first register returns {"auth_token": ...}; we keep it for heartbeats.

  * POST {PLATFORM_URL}/registry/heartbeat   (every ~10s)
        header: Authorization: Bearer <auth_token>
        body: {"workspace_id","error_rate","sample_error","active_tasks",
               "uptime_seconds","current_task"}
        This is what lifts the workspace provisioning -> online and keeps the
        Redis liveness TTL fresh (so the restart re-online assertion can pass).

  * listen on :8000 and answer the A2A JSON-RPC the proxy forwards:
        POST /  {"jsonrpc","id","method":"message/send","params":{...}}
        -> 200 {"jsonrpc":"2.0","id":<echoed>,
                "result":{"kind":"message","role":"agent",
                          "parts":[{"kind":"text","text":"STUB OK"}],
                          "messageId":<uuid>}}
    The result envelope matches what test_a2a_e2e.sh asserts on
    (result.parts[0].text, role=agent, kind=text). A health path (/health and
    GET /) returns 200 so any probe sees the container as alive.
"""

import json
import os
import sys
import threading
import time
import urllib.request
import urllib.error
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8000

WORKSPACE_ID = os.environ.get("WORKSPACE_ID", "").strip()
PLATFORM_URL = (os.environ.get("PLATFORM_URL") or os.environ.get("MOLECULE_URL") or "").rstrip("/")
HOSTNAME = os.environ.get("HOSTNAME", "").strip()  # docker sets this to the container id; ws-<id> alias also resolves

# URL we register with. Two hard constraints, discovered from workspace-server:
#
#   * validateAgentURL (registry.go) blocks RFC-1918 ranges in NON-saas mode
#     (this dev stack sets neither MOLECULE_DEPLOY_MODE=saas nor MOLECULE_ORG_ID
#     -> strict mode). The molecule-core-net bridge is 172.18.0.0/16, INSIDE the
#     blocked 172.16/12 — so registering our own ws-<id>:8000 DNS name (which
#     resolves to a 172.18.x bridge IP) would be REJECTED and we'd never get an
#     auth_token. "localhost" is explicitly allowed BY NAME (no DNS lookup).
#
#   * the proxy doesn't use the URL we register anyway: the provisioner
#     pre-stores http://127.0.0.1:<host-port>, the register upsert PRESERVES any
#     existing 127.0.0.1 URL (CASE WHEN url LIKE 'http://127.0.0.1%'), and when
#     the platform runs in Docker resolveAgentURL rewrites that to the container
#     -DNS form http://ws-<id[:12]>:8000 before forwarding. So our listen
#     address (0.0.0.0:8000, reachable as ws-<id>:8000 on the bridge) is what
#     the proxy actually hits — independent of the URL string we register.
#
# Net: register a host-reachable localhost URL. The provisioner injects the
# correct host-mapped port via MOLECULE_WORKSPACE_URL (#2851); prefer that and
# fall back to the legacy STUB_REGISTER_URL or the internal listen port.
_short = WORKSPACE_ID[:12] if len(WORKSPACE_ID) > 12 else WORKSPACE_ID
SELF_URL = (
    os.environ.get("STUB_REGISTER_URL")
    or os.environ.get("MOLECULE_WORKSPACE_URL")
    or f"http://localhost:{PORT}"
)

CONFIG_PATH = (os.environ.get("WORKSPACE_CONFIG_PATH") or "/configs").rstrip("/")
AUTH_TOKEN_FILE = f"{CONFIG_PATH}/.auth_token"

AUTH_TOKEN = None
_started = time.time()


def _log(msg):
    print(f"[stub-runtime {_short}] {msg}", flush=True)


def read_volume_token():
    """The provisioner pre-writes the CURRENT workspace bearer to
    /configs/.auth_token before every container start (issueAndInjectToken,
    #1877), and ROTATES it on every (re)provision (RevokeAllForWorkspace +
    IssueToken). So the volume file — NOT the register-response token — is the
    authoritative, rotation-proof bearer. Reading it on each heartbeat means a
    provision-time token rotation never wedges our heartbeat at 401 (which is
    what kept the workspace stuck in 'provisioning' instead of flipping online).
    """
    try:
        with open(AUTH_TOKEN_FILE, "r") as f:
            tok = f.read().strip()
            return tok or None
    except Exception:
        return None


def _post_json(path, payload, token=None):
    url = f"{PLATFORM_URL}{path}"
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = resp.read().decode()
        return resp.status, body


def register():
    """POST /registry/register. Returns the issued auth_token (first register).

    C18 hijack guard: once the workspace has ANY live token on file (the
    provisioner mints+injects one into /configs/.auth_token before start), a
    register MUST carry that workspace's bearer or it 401s. So we send the
    volume token (if present). First-ever boot has no live token yet → bootstrap
    register (no bearer) is allowed and returns the freshly-issued auth_token.
    """
    global AUTH_TOKEN
    payload = {
        "id": WORKSPACE_ID,
        "url": SELF_URL,
        "delivery_mode": "push",
        "agent_card": {
            "name": WORKSPACE_ID,
            "description": "stub runtime (e2e lifecycle)",
            "skills": [],
        },
    }
    status, body = _post_json("/registry/register", payload, token=read_volume_token())
    _log(f"register -> {status} {body[:200]}")
    try:
        parsed = json.loads(body)
    except Exception:
        parsed = {}
    tok = parsed.get("auth_token")
    if tok:
        AUTH_TOKEN = tok
        _log("captured auth_token from register response")
    return status


def current_token():
    # Volume file is authoritative (rotation-proof); fall back to the token we
    # captured from the register response if the file isn't there yet.
    return read_volume_token() or AUTH_TOKEN


def heartbeat():
    payload = {
        "workspace_id": WORKSPACE_ID,
        "error_rate": 0.0,
        "sample_error": "",
        "active_tasks": 0,
        "uptime_seconds": int(time.time() - _started),
        "current_task": "",
    }
    status, body = _post_json("/registry/heartbeat", payload, token=current_token())
    return status, body


def register_with_retry():
    # The platform may still be wiring the row when we boot; retry a few times.
    # Register is best-effort for the e2e (heartbeat drives online); a sticky
    # 401 just means the workspace already has a live token and our volume token
    # is momentarily stale — the heartbeat path re-reads the volume each beat.
    for attempt in range(1, 11):
        try:
            status = register()
            if status == 200:
                return True
            _log(f"register attempt {attempt}: HTTP {status}, retrying")
        except urllib.error.HTTPError as e:
            _log(f"register attempt {attempt}: HTTPError {e.code} {e.read().decode()[:200]}")
        except Exception as e:
            _log(f"register attempt {attempt}: {e}")
        time.sleep(2)
    return False


def heartbeat_loop():
    # Fire the FIRST heartbeat immediately (no initial 5s wait) — the
    # provisioning->online transition is driven by the heartbeat handler
    # (registry.go evaluateStatus, #1784), so an eager first beat minimises the
    # provision->online latency the e2e polls on.
    while True:
        try:
            status, body = heartbeat()
            if status != 200:
                _log(f"heartbeat -> {status} {body[:160]}")
                # A 401 means our token was rotated (every provision rotates the
                # workspace token, issueAndInjectToken -> RevokeAllForWorkspace).
                # Re-register to mint a fresh one. This is what lets the SAME
                # container process survive a platform-side token rotation.
                if status == 401:
                    _log("heartbeat 401 — re-registering to refresh token")
                    register_with_retry()
        except urllib.error.HTTPError as e:
            if e.code == 401:
                _log("heartbeat 401 (HTTPError) — re-registering")
                register_with_retry()
            else:
                _log(f"heartbeat HTTPError {e.code}")
        except Exception as e:
            _log(f"heartbeat error: {e}")
        time.sleep(5)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # silence default access logging
        pass

    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        # Health: any GET returns 200 so probes see us as alive.
        self._send(200, {"status": "ok", "stub": True, "workspace_id": WORKSPACE_ID})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}

        method = req.get("method", "")
        req_id = req.get("id", str(uuid.uuid4()))

        if method and method != "message/send":
            # Match the proxy's -32601 method-not-found contract for unknowns.
            self._send(200, {
                "jsonrpc": "2.0",
                "id": req_id,
                "error": {"code": -32601, "message": f"method not found: {method}"},
            })
            return

        # Canned A2A reply — exact envelope the canvas/proxy + test_a2a_e2e.sh
        # assert on: result.role=agent, result.parts[0].kind=text/text.
        self._send(200, {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "kind": "message",
                "role": "agent",
                "parts": [{"kind": "text", "text": "STUB OK"}],
                "messageId": str(uuid.uuid4()),
            },
        })


def main():
    if not WORKSPACE_ID or not PLATFORM_URL:
        _log(f"FATAL: WORKSPACE_ID={WORKSPACE_ID!r} PLATFORM_URL={PLATFORM_URL!r} — both required")
        sys.exit(1)

    _log(f"booting: platform={PLATFORM_URL} self_url={SELF_URL} hostname={HOSTNAME}")

    # Start the HTTP server FIRST so the platform can reach us the instant we
    # register (avoids a race where the proxy forwards before we're listening).
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    _log(f"listening on :{PORT}")

    # Try to register, but do NOT make heartbeating contingent on it. The
    # provisioning->online transition is driven by the HEARTBEAT handler
    # (registry.go evaluateStatus, #1784), and heartbeats authenticate with the
    # volume token (rotation-proof). If register transiently 401s (e.g. a token
    # rotation mid-boot), we must still heartbeat so the workspace can come
    # online — blocking the heartbeat loop on register success is exactly what
    # kept the workspace stuck in 'provisioning'. register_with_retry runs in a
    # background thread; the foreground heartbeat loop starts immediately.
    threading.Thread(target=register_with_retry, daemon=True).start()
    heartbeat_loop()


if __name__ == "__main__":
    main()
