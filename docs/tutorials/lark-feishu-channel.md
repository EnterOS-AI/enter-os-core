# Connecting an AI Agent to Lark / Feishu

Molecule AI's Lark channel adapter (shipped in #480) lets any workspace agent
receive messages from Lark or Feishu chats and reply through the same thread.
This tutorial gets you from zero to a live bot in about ten minutes.

> **Lark vs Feishu** — same payload format, different host.
> Lark is the international product; Feishu is the China edition.
> The adapter detects which host to use from your webhook URL.

## Prerequisites

- A Molecule AI workspace already running (any runtime)
- A Lark tenant with permission to create a **Custom Bot**
- `PLATFORM_URL` and a workspace bearer token

## Setup

```bash
# 1. Create a Lark Custom Bot in your group chat
#    Settings → Bots → Add Bot → Custom Bot
#    Copy the Webhook URL — looks like:
#    https://open.larksuite.com/open-apis/bot/v2/hook/<token>

# 2. (Recommended) Set a Verification Token in the Lark bot settings.
#    This lets the platform reject spoofed inbound messages.

# 3. Register the channel on your workspace
curl -s -X POST "${PLATFORM_URL}/workspaces/${WORKSPACE_ID}/channel" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "type": "lark",
    "config": {
      "webhook_url": "https://open.larksuite.com/open-apis/bot/v2/hook/YOUR_TOKEN",
      "verify_token": "YOUR_VERIFY_TOKEN"
    }
  }'

# 4. Point Lark Event Subscriptions at your platform
#    Lark Developer Console → Event Subscriptions → Request URL:
#    https://YOUR_PLATFORM/channels/lark/webhook

# 5. Subscribe to the im.message.receive_v1 event in Lark console.

# 6. Send a message in the Lark group — your agent replies.
#    Watch the round-trip:
molecule workspace logs ${WORKSPACE_ID} --follow
```

## The 200-OK-but-failed gotcha

Lark's outbound API **always** returns HTTP 200, even on delivery failure.
The real result is in the JSON body:

```json
{ "code": 99, "msg": "...", "data": {} }
```

Molecule AI surfaces `code != 0` as a hard Go error so your agent sees an
actual failure instead of silent data loss. Check `last_sample_error` on the
workspace if messages seem to disappear.

## Expected output

After step 6, `workspace logs` shows:

```
[channel:lark] inbound  user_id=ou_xxx  text="Hello agent"
[agent]        reply    "Hi! How can I help you today?"
[channel:lark] outbound code=0  ok
```

## How it works

When a Lark user sends a message the platform receives a `v2 event_callback`
(`im.message.receive_v1`). It validates the optional `verify_token` with a
constant-time compare (timing-attack safe), then proxies the text through the
standard A2A flow — the same path used by Telegram and Slack. The agent never
knows which channel it's talking to. Replies go back via the Custom Bot
webhook; the adapter prefers `user_id` over `open_id` so replies land in the
correct DM thread when the bot has contacts permission.

## Multi-channel teams

You can attach different channels to different workspaces in the same org.
Route customer-facing chats to a triage agent on Lark while your eng team
talks to the DevOps agent on Slack — all coordinated through the same
Molecule AI canvas without code changes.

## Related

- PR #480: [feat(channels): Lark / Feishu channel adapter](https://git.moleculesai.app/molecule-ai/molecule-core/pull/480)
- [Social channels architecture](../agent-runtime/social-channels.md)
- [Channel adapter reference](../api-reference.md#channels)