#!/usr/bin/env bash
# GATING E2E for the social-channels outbound + discover + data-prune paths
# (core#2332 P1.10). Closes two coverage gaps that were previously only
# unit-mocked, so a regression in any of them goes RED in the required
# `E2E API Smoke Test` lane instead of slipping through:
#
#  (1) Channel SEND end-to-end. Every adapter's SendMessage was only ever
#      asserted by unit tests that reconstruct the payload by hand and POST
#      it themselves (see internal/channels/lark_test.go's "we can't change
#      the prefix const" comment) — nothing proved that a message submitted
#      through the LIVE platform API actually serializes and POSTs to a
#      provider endpoint. Here we stand up a local mock-upstream, point a
#      Slack Incoming-Webhook channel at it, send via
#      POST /channels/:id/send, and assert the MOCK RECEIVED the correctly
#      serialized {"text":"..."} body. Real serialize+POST, real HTTP stack,
#      no real Slack account.
#
#  (2) Channel DISCOVER (POST /channels/discover). Had no test at all. We
#      point the Telegram discover path at a mock Bot API that serves
#      getMe + getUpdates and assert the discovered bot username + chat
#      round-trip back through the handler.
#
#  (3) Workspace data-prune (RFC #734). The user-requested permanent delete
#      with ?purge=true prunes a workspace's durable child data (channels,
#      secrets, config, …). We create prunable data on a target workspace
#      AND a sibling, purge the target, then assert the target's child rows
#      are GONE while the sibling's SURVIVE.
#
# ── Test seam (production-inert) ────────────────────────────────────────
# Adapters pin their outbound host to the real vendor (hooks.slack.com /
# api.telegram.org). Two env-gated overrides — set ONLY by this lane, never
# in any prod/staging deploy — let the live send/discover path target a
# local mock so the round-trip is provable in CI:
#
#   MOLECULE_CHANNELS_TEST_WEBHOOK_BASE       (Slack webhook accept-prefix)
#   MOLECULE_CHANNELS_TEST_TELEGRAM_API_BASE  (Telegram Bot API base)
#
# These must be present in the PLATFORM process env (the workflow exports
# them via $GITHUB_ENV before "Start platform"), pointing at the fixed
# loopback ports this script binds its mocks on. If they are absent the
# platform rejects the mock URLs; under E2E_REQUIRE_LIVE=1 that is a hard
# RED (the seam regressed / the workflow wiring broke), otherwise a LOUD
# SKIP for ad-hoc local runs that didn't export them.
#
# NEVER fail-open: a missing assertion target fails the script.
#
# Required env (defaults shown):
#   BASE                       http://127.0.0.1:8080
#   MOLECULE_ADMIN_TOKEN       (admin bearer; matches the platform's ADMIN_TOKEN)
#   E2E_CHANNELS_WEBHOOK_PORT  18099   (mock Slack webhook upstream)
#   E2E_CHANNELS_TELEGRAM_PORT 18098   (mock Telegram Bot API upstream)
#   E2E_REQUIRE_LIVE           0        (1 = seam-absent is RED, not skip)

set -uo pipefail

# shellcheck disable=SC1091
source "$(dirname "$0")/_lib.sh"   # sets BASE default + admin/token helpers

WEBHOOK_PORT="${E2E_CHANNELS_WEBHOOK_PORT:-18099}"
TELEGRAM_PORT="${E2E_CHANNELS_TELEGRAM_PORT:-18098}"
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"

# The base prefixes the PLATFORM must have been started with. We assert the
# adapter accepted a URL under these — proving the platform's env matches.
WEBHOOK_BASE="http://127.0.0.1:${WEBHOOK_PORT}/"
TELEGRAM_BASE="http://127.0.0.1:${TELEGRAM_PORT}"

PASS=0
FAIL=0
WORK_DIR="$(mktemp -d)"
WS_TARGET=""
WS_SIBLING=""
WS_PARENT=""
WS_TARGET_TOK=""
WS_SIBLING_TOK=""
WS_PARENT_TOK=""
MOCK_PID=""

ADMIN_BEARER="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
ADMIN_AUTH=()
[ -n "$ADMIN_BEARER" ] && ADMIN_AUTH=(-H "Authorization: Bearer $ADMIN_BEARER")

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "  $2"; FAIL=$((FAIL + 1)); }

# loud_skip records a SKIP and exits according to E2E_REQUIRE_LIVE. NEVER
# silently passes — it either hard-fails (require-live) or exits 0 with a
# loud banner (ad-hoc local). Mirrors the require-live gate pattern used by
# test_priority_runtimes_e2e.sh.
loud_skip() {
  local reason="$1"
  echo
  echo "============================================================"
  if [ "$REQUIRE_LIVE" = "1" ]; then
    echo "E2E_REQUIRE_LIVE=1 but channels e2e seam is unavailable:"
    echo "  $reason"
    echo "This is a HARD FAILURE — the platform was not started with the"
    echo "channels test seam env (MOLECULE_CHANNELS_TEST_WEBHOOK_BASE /"
    echo "MOLECULE_CHANNELS_TEST_TELEGRAM_API_BASE) on the fixed loopback"
    echo "ports, or the seam regressed. Fix the workflow wiring or the seam."
    echo "============================================================"
    cleanup
    exit 1
  fi
  echo "SKIP (loud): $reason"
  echo "Set MOLECULE_CHANNELS_TEST_WEBHOOK_BASE=$WEBHOOK_BASE and"
  echo "MOLECULE_CHANNELS_TEST_TELEGRAM_API_BASE=$TELEGRAM_BASE in the"
  echo "PLATFORM env before starting it, then re-run. (CI sets these.)"
  echo "============================================================"
  cleanup
  exit 0
}

cleanup() {
  set +e
  if [ -n "$MOCK_PID" ]; then
    kill "$MOCK_PID" 2>/dev/null
    wait "$MOCK_PID" 2>/dev/null
  fi
  # Hard-purge any workspaces we created so repeat runs are deterministic.
  for pair in "$WS_TARGET|$WS_TARGET_TOK|e2e-chan-target-$$" \
              "$WS_SIBLING|$WS_SIBLING_TOK|e2e-chan-sibling-$$" \
              "$WS_PARENT|$WS_PARENT_TOK|e2e-chan-parent-$$"; do
    local wid tok name
    wid="${pair%%|*}"; pair="${pair#*|}"
    tok="${pair%%|*}"; name="${pair#*|}"
    [ -z "$wid" ] && continue
    e2e_gated_admin_op "$wid" curl -s -X DELETE "$BASE/workspaces/$wid?confirm=true&purge=true" \
      -H "X-Confirm-Name: $name" "${ADMIN_AUTH[@]}" >/dev/null 2>&1
  done
  rm -rf "$WORK_DIR" 2>/dev/null
}
trap cleanup EXIT INT TERM

# ── mock upstream ───────────────────────────────────────────────────────
# One Python process serves BOTH mocks (different ports). It records the
# Slack webhook request body to $WORK_DIR/slack_body.json and answers the
# Telegram getMe/getUpdates calls with a deterministic bot+chat fixture.
start_mock() {
  cat > "$WORK_DIR/mock.py" <<'PY'
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

WORK_DIR = os.environ["MOCK_WORK_DIR"]
WEBHOOK_PORT = int(os.environ["MOCK_WEBHOOK_PORT"])
TELEGRAM_PORT = int(os.environ["MOCK_TELEGRAM_PORT"])

BOT_USERNAME = "e2e_mock_bot"
CHAT_ID = -1009876543210
CHAT_NAME = "E2E Mock Group"


class SlackHandler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # silence
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(n)
        # Persist EXACTLY what the live Slack send path POSTed so the bash
        # side can assert the serialized payload.
        with open(os.path.join(WORK_DIR, "slack_body.json"), "wb") as f:
            f.write(body)
        with open(os.path.join(WORK_DIR, "slack_meta.json"), "w") as f:
            json.dump({"path": self.path,
                       "content_type": self.headers.get("Content-Type", "")}, f)
        # Real Slack Incoming Webhooks reply 200 "ok".
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")


class TelegramHandler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, obj):
        payload = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _route(self):
        # tgbotapi calls <base>/bot<token>/<method>
        method = self.path.rsplit("/", 1)[-1]
        if method == "getMe":
            return self._send({"ok": True, "result": {
                "id": 4242, "is_bot": True, "first_name": "E2E Mock",
                "username": BOT_USERNAME, "can_read_all_group_messages": True}})
        if method == "setMyCommands":
            return self._send({"ok": True, "result": True})
        if method == "deleteWebhook":
            return self._send({"ok": True, "result": True})
        if method == "getUpdates":
            # One my_chat_member update so the bot "discovers" a group.
            return self._send({"ok": True, "result": [{
                "update_id": 1,
                "my_chat_member": {
                    "chat": {"id": CHAT_ID, "title": CHAT_NAME, "type": "supergroup"},
                    "from": {"id": 1, "is_bot": False, "first_name": "Op"},
                    "date": 0,
                    "old_chat_member": {"user": {"id": 4242, "is_bot": True,
                                                 "first_name": "E2E Mock"},
                                        "status": "left"},
                    "new_chat_member": {"user": {"id": 4242, "is_bot": True,
                                                 "first_name": "E2E Mock"},
                                        "status": "member"},
                }}]})
        # Default OK for any other bot method tgbotapi may probe.
        return self._send({"ok": True, "result": True})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or "0")
        if n:
            self.rfile.read(n)
        self._route()

    def do_GET(self):
        self._route()


def serve(port, handler):
    ThreadingHTTPServer(("127.0.0.1", port), handler).serve_forever()


t = threading.Thread(target=serve, args=(TELEGRAM_PORT, TelegramHandler), daemon=True)
t.start()
serve(WEBHOOK_PORT, SlackHandler)
PY
  MOCK_WORK_DIR="$WORK_DIR" MOCK_WEBHOOK_PORT="$WEBHOOK_PORT" \
    MOCK_TELEGRAM_PORT="$TELEGRAM_PORT" \
    python3 "$WORK_DIR/mock.py" &
  MOCK_PID=$!
  # Wait for both ports to accept connections (fail loudly if they never do).
  local up=0
  for _ in $(seq 1 50); do
    if curl -s -o /dev/null "http://127.0.0.1:${WEBHOOK_PORT}/" \
       && curl -s -o /dev/null "http://127.0.0.1:${TELEGRAM_PORT}/botX/getMe"; then
      up=1; break
    fi
    sleep 0.1
  done
  if [ "$up" != "1" ]; then
    echo "FATAL: mock upstream did not come up on ports $WEBHOOK_PORT/$TELEGRAM_PORT" >&2
    cleanup
    exit 2
  fi
}

json_field() { python3 -c "import sys,json; print(json.load(sys.stdin).get('$1',''))"; }

create_external_ws() {
  local name="$1" parent="${2:-}" resp wid parent_field=""
  # core#2697: when no explicit parent is given, the server now defaults a new
  # workspace's parent to the org's platform-agent root, or — absent one — the
  # SOLE plain root. This test needs target + sibling to be genuine SIBLINGS
  # (so purging the target must NOT cascade to the sibling), so callers pass an
  # explicit shared parent. Without it the 2nd no-parent create would nest
  # under the 1st (the sole root) and the purge-over-reach assertion would
  # spuriously fail on the new default-parent behavior.
  [ -n "$parent" ] && parent_field=",\"parent_id\":\"$parent\""
  resp=$(curl -s -X POST "$BASE/workspaces" "${ADMIN_AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$name\",\"runtime\":\"external\",\"external\":true,\"tier\":1$parent_field}")
  wid=$(printf '%s' "$resp" | json_field id)
  if [ -z "$wid" ]; then
    echo "FATAL: could not create workspace $name: $resp" >&2
    cleanup
    exit 1
  fi
  local tok
  tok=$(printf '%s' "$resp" | e2e_extract_token)
  [ -z "$tok" ] && tok=$(e2e_mint_workspace_token "$wid" 2>/dev/null || true)
  printf '%s\t%s\n' "$wid" "$tok"
}

# ════════════════════════════════════════════════════════════════════════
echo "=== Channels + data-prune E2E (core#2332 P1.10) ==="
echo "BASE=$BASE  webhook_mock=$WEBHOOK_BASE  telegram_mock=$TELEGRAM_BASE"

if ! curl -sf "$BASE/health" >/dev/null 2>&1; then
  echo "FATAL: platform not reachable at $BASE/health" >&2
  exit 2
fi

start_mock

# ── workspaces ──────────────────────────────────────────────────────────
# Create a common parent first, then nest target + sibling under it as genuine
# siblings. This keeps the purge-over-reach invariant (purging target must not
# touch sibling) independent of the core#2697 default-parent behavior, which
# would otherwise nest the 2nd no-parent create under the 1st (the sole root).
IFS=$'\t' read -r WS_PARENT WS_PARENT_TOK < <(create_external_ws "e2e-chan-parent-$$")
IFS=$'\t' read -r WS_TARGET WS_TARGET_TOK < <(create_external_ws "e2e-chan-target-$$" "$WS_PARENT")
IFS=$'\t' read -r WS_SIBLING WS_SIBLING_TOK < <(create_external_ws "e2e-chan-sibling-$$" "$WS_PARENT")
echo "parent=$WS_PARENT target=$WS_TARGET sibling=$WS_SIBLING"

WS_AUTH=("${ADMIN_AUTH[@]}")
[ -n "$WS_TARGET_TOK" ] && WS_AUTH=(-H "Authorization: Bearer $WS_TARGET_TOK")
SIB_AUTH=("${ADMIN_AUTH[@]}")
[ -n "$WS_SIBLING_TOK" ] && SIB_AUTH=(-H "Authorization: Bearer $WS_SIBLING_TOK")

# ── (1) SEND end-to-end via a Slack Incoming-Webhook channel ────────────
echo
echo "--- (1) channel SEND → mock upstream receives serialized payload ---"

# Create a slack channel whose webhook_url points at our mock. If the
# platform wasn't started with the webhook test-base, ValidateConfig
# rejects this URL → loud_skip / RED. chat_id is required by SendOutbound.
SLACK_CFG=$(python3 -c "import json,sys; print(json.dumps({
  'webhook_url': sys.argv[1] + 'services/T000/B000/e2e',
  'chat_id': 'mock-chat'}))" "$WEBHOOK_BASE")
CREATE=$(curl -s -X POST "$BASE/workspaces/$WS_TARGET/channels" "${WS_AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d "{\"channel_type\":\"slack\",\"config\":$SLACK_CFG,\"enabled\":true}")
CH_ID=$(printf '%s' "$CREATE" | json_field id)
if [ -z "$CH_ID" ]; then
  case "$CREATE" in
    *"invalid channel config"*)
      loud_skip "platform rejected mock webhook_url (MOLECULE_CHANNELS_TEST_WEBHOOK_BASE not set on platform): $CREATE" ;;
    *)
      fail "create slack channel" "$CREATE" ;;
  esac
else
  pass "create slack channel pointed at mock upstream (id=$CH_ID)"

  SEND_TEXT="hello from e2e $$"
  # Send route: wsAuth.POST /workspaces/:id/channels/:channelId/send (the
  # handler keys off :channelId; :id scopes the workspace bearer).
  SEND=$(curl -s -w $'\n%{http_code}' -X POST \
    "$BASE/workspaces/$WS_TARGET/channels/$CH_ID/send" "${WS_AUTH[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"text\":\"$SEND_TEXT\"}")
  SEND_CODE=$(printf '%s' "$SEND" | tail -n1)
  if [ "$SEND_CODE" = "200" ]; then
    pass "POST /channels/:id/send returned 200"
  else
    fail "POST /channels/:id/send" "code=$SEND_CODE body=$(printf '%s' "$SEND" | sed '$d')"
  fi

  # Give the async-free SendOutbound a beat to land at the mock.
  RECEIVED=""
  for _ in $(seq 1 30); do
    if [ -s "$WORK_DIR/slack_body.json" ]; then RECEIVED=1; break; fi
    sleep 0.1
  done
  if [ -n "$RECEIVED" ]; then
    pass "mock upstream RECEIVED an outbound POST"
    GOT_TEXT=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('text',''))" \
      "$WORK_DIR/slack_body.json" 2>/dev/null || true)
    if [ "$GOT_TEXT" = "$SEND_TEXT" ]; then
      pass "mock received correctly-serialized {\"text\":...} payload (text matches end-to-end)"
    else
      fail "serialized payload mismatch" "want=[$SEND_TEXT] got=[$GOT_TEXT] raw=$(cat "$WORK_DIR/slack_body.json")"
    fi
  else
    fail "mock upstream never received the outbound POST" "send path did not serialize+POST to the configured endpoint"
  fi
fi

# ── (2) DISCOVER via the Telegram mock Bot API ──────────────────────────
echo
echo "--- (2) POST /channels/discover (telegram) → mock Bot API ---"
# A token matching the telegramTokenRegex (\d+:[A-Za-z0-9_-]{30,}).
DISC_TOKEN="424242:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
DISC=$(curl -s -w $'\n%{http_code}' -X POST "$BASE/channels/discover" \
  "${ADMIN_AUTH[@]}" -H "Content-Type: application/json" \
  -d "{\"channel_type\":\"telegram\",\"bot_token\":\"$DISC_TOKEN\",\"workspace_id\":\"$WS_TARGET\"}")
DISC_CODE=$(printf '%s' "$DISC" | tail -n1)
DISC_BODY=$(printf '%s' "$DISC" | sed '$d')
if [ "$DISC_CODE" = "200" ]; then
  pass "POST /channels/discover returned 200"
  if printf '%s' "$DISC_BODY" | grep -qF '"bot_username":"e2e_mock_bot"'; then
    pass "discover round-tripped the mock bot username"
  else
    fail "discover bot_username" "$DISC_BODY"
  fi
  if printf '%s' "$DISC_BODY" | grep -qF '"chat_id":"-1009876543210"'; then
    pass "discover round-tripped the mock chat id"
  else
    fail "discover chat list" "$DISC_BODY"
  fi
else
  case "$DISC_BODY" in
    *"Cannot reach Telegram"*|*"Invalid bot token"*|*"Failed to connect"*)
      # Platform reached the REAL api.telegram.org (seam not set) → can't prove.
      loud_skip "discover hit real Telegram, not the mock (MOLECULE_CHANNELS_TEST_TELEGRAM_API_BASE not set on platform): code=$DISC_CODE $DISC_BODY" ;;
    *)
      fail "POST /channels/discover" "code=$DISC_CODE body=$DISC_BODY" ;;
  esac
fi

# ── (3) Data-prune (RFC #734): purge removes prunable data, sibling survives
echo
echo "--- (3) data-prune: purge target's child data, sibling survives ---"

# Seed prunable child data on BOTH workspaces: a channel (already on target)
# + a secret on each. We assert via GET /channels which lists workspace_channels.
seed_secret() {
  local wid="$1"; shift
  curl -s -o /dev/null -X POST "$BASE/workspaces/$wid/secrets" "$@" \
    -H "Content-Type: application/json" \
    -d '{"key":"E2E_PRUNE_PROBE","value":"v"}'
}
seed_secret "$WS_TARGET" "${WS_AUTH[@]}"
# Sibling gets its OWN channel so we can prove its rows survive the target purge.
SIB_SLACK_CFG=$(python3 -c "import json,sys; print(json.dumps({
  'webhook_url': sys.argv[1] + 'services/T111/B111/sib',
  'chat_id': 'sib-chat'}))" "$WEBHOOK_BASE")
SIB_CH=$(curl -s -X POST "$BASE/workspaces/$WS_SIBLING/channels" "${SIB_AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d "{\"channel_type\":\"slack\",\"config\":$SIB_SLACK_CFG,\"enabled\":true}")
SIB_CH_ID=$(printf '%s' "$SIB_CH" | json_field id)

# Pre-purge: confirm both workspaces have >=1 channel row.
TGT_CH_PRE=$(curl -s "$BASE/workspaces/$WS_TARGET/channels" "${WS_AUTH[@]}")
SIB_CH_PRE=$(curl -s "$BASE/workspaces/$WS_SIBLING/channels" "${SIB_AUTH[@]}")
TGT_PRE_N=$(printf '%s' "$TGT_CH_PRE" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
SIB_PRE_N=$(printf '%s' "$SIB_CH_PRE" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
if [ "${TGT_PRE_N:-0}" -ge 1 ] && [ "${SIB_PRE_N:-0}" -ge 1 ]; then
  pass "pre-purge: target ($TGT_PRE_N) and sibling ($SIB_PRE_N) both have channel data"
else
  fail "pre-purge seed" "target=$TGT_PRE_N sibling=$SIB_PRE_N (need >=1 each)"
fi

# Permanent delete WITH purge — the RFC #734 prune of durable child data.
# DELETE /workspaces/:id is AdminAuth-gated (router.go:167); Tier-2b rejects a
# workspace bearer when ADMIN_TOKEN is set, so this MUST use the admin bearer.
# X-Confirm-Name must equal the workspace name (the destructive-delete guard).
PURGE=$(e2e_gated_admin_op "$WS_TARGET" curl -s -X DELETE \
  "$BASE/workspaces/$WS_TARGET?confirm=true&purge=true" \
  -H "X-Confirm-Name: e2e-chan-target-$$" "${ADMIN_AUTH[@]}")
if printf '%s' "$PURGE" | grep -qF '"status":"purged"'; then
  pass "DELETE ?purge=true returned purged"
else
  fail "DELETE ?purge=true" "body=$PURGE"
fi
# Target was purged → its token is revoked; query its channels with admin
# bearer. The purge hard-deletes workspace_channels rows for the target.
TGT_CH_POST=$(curl -s "$BASE/workspaces/$WS_TARGET/channels" "${ADMIN_AUTH[@]}")
TGT_POST_N=$(printf '%s' "$TGT_CH_POST" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else -1)
except Exception:
  print(-1)" 2>/dev/null || echo -1)
if [ "${TGT_POST_N:-1}" = "0" ]; then
  pass "post-purge: target's prunable channel data is GONE (0 rows)"
else
  fail "prune did not remove target channel data" "post-purge target rows=$TGT_POST_N body=$(printf '%s' "$TGT_CH_POST" | head -c 200)"
fi
WS_TARGET=""  # purged; don't re-delete in cleanup

# Sibling (NON-prunable relative to the target purge) must be untouched.
SIB_CH_POST=$(curl -s "$BASE/workspaces/$WS_SIBLING/channels" "${SIB_AUTH[@]}")
SIB_POST_N=$(printf '%s' "$SIB_CH_POST" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null || echo -1)
if [ "${SIB_POST_N:-0}" -ge 1 ] && printf '%s' "$SIB_CH_POST" | grep -qF "$SIB_CH_ID"; then
  pass "post-purge: sibling's non-prunable data SURVIVED ($SIB_POST_N rows, channel $SIB_CH_ID intact)"
else
  fail "purge over-reached: sibling data did not survive" "sibling rows=$SIB_POST_N body=$(printf '%s' "$SIB_CH_POST" | head -c 200)"
fi

# ── verdict ─────────────────────────────────────────────────────────────
echo
echo "=== channels+prune e2e: $PASS passed, $FAIL failed ==="
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi
# Guard against a vacuous green: every section must have produced asserts.
if [ "$PASS" -lt 9 ]; then
  echo "FATAL: only $PASS assertions ran — expected >=9 (send + discover + prune). Refusing to report green." >&2
  exit 1
fi
echo "ALL CHANNELS + PRUNE E2E CHECKS PASSED"
