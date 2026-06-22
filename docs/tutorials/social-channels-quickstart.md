# Social Channels Quickstart — Connect Your AI Agent to Discord or Telegram

Your EnterOS workspace can receive messages from and reply to Discord channels and Telegram chats — using the same A2A routing your canvas uses internally. This tutorial gets you from zero to a live slash-command bot in about ten minutes.

> **What you get:** agents that respond to `/ask`, `/status`, and `/help` commands in Discord or Telegram. One channel per workspace. Hot-reload config — no restart required.

---

## How the adapter system works

Both Discord and Telegram use the same `ChannelAdapter` interface under the hood. The workspace agent never knows which platform it's talking to — it receives plain text via A2A and replies the same way.

| Platform | Inbound method | Outbound method | Slash commands |
|---|---|---|---|
| **Discord** | Discord Interactions (webhook) | Incoming Webhook | ✅ via `/ask` |
| **Telegram** | Long-polling | Bot API | ✅ via `/ask` |

New platforms add one adapter implementation. The REST API, Canvas UI, and MCP tools work automatically.

---

## Discord — Setup

### Step 1 — Create a Discord webhook

1. Open your Discord server → **Channel Settings** → **Integrations** → **Webhooks**
2. Click **New Webhook** → name it (e.g. "EnterOS Agent")
3. Copy the webhook URL — it looks like:
   `https://discord.com/api/webhooks/123456789/abcdefghijklmnop`

The webhook URL encodes the channel and bot credentials. You don't need a separate bot account for outbound messages.

### Step 2 — Connect via Canvas

1. Open your workspace in Canvas → **Channels** tab → **+ Connect**
2. Select **Discord**
3. Paste the webhook URL
4. Click **Connect**

That's it. Your workspace can now send messages to that Discord channel.

### Step 3 — Add inbound slash commands

For Discord to route slash commands to your workspace:

1. Go to the [Discord Developer Portal](https://discord.com/developers/applications)
2. Create a new Application (or use an existing bot)
3. Under **General Information**, copy the **Application ID** and **Public Key**
4. Under **OAuth2** → **URL Generator**, add the scope: `bot`
5. Visit the generated URL to add the bot to your server
6. In your platform's Canvas → **Channels** → Discord channel → **Interactions Endpoint URL**:
   - Set it to `https://your-platform.com/webhooks/discord`
   - Discord requires HTTPS

7. Register the bot's slash commands with Discord:
   ```bash
   curl -X POST "https://discord.com/api/v10/applications/${APP_ID}/commands" \
     -H "Authorization: Bot ${BOT_TOKEN}" \
     -H "Content-Type: application/json" \
     -d '{
       "name": "ask",
       "type": 1,
       "description": "Ask the EnterOS agent"
     }'
   ```

### Step 4 — Verify

Send `/ask what's our deployment status?` in your Discord channel. Your agent replies.

If it doesn't respond, check:
- The Interactions Endpoint URL is set correctly in Canvas
- The bot is in the correct Discord server with permissions to read slash commands
- Your platform URL is publicly accessible (Discord needs to POST to it)

---

## Telegram — Setup

### Step 1 — Create a Telegram bot

1. Open a chat with [@BotFather](https://t.me/BotFather) on Telegram
2. Send `/newbot`
3. Follow the prompts — save the token (looks like `123456789:ABCdefGHI...`)

### Step 2 — Disable group privacy (recommended)

By default, Telegram bots only see slash commands and @mentions in groups. To let the bot see all messages (for a better experience):

1. `@BotFather` → `/mybots` → select your bot
2. **Bot Settings** → **Group Privacy** → **Turn off**
3. Re-add the bot to your group (privacy changes only apply to new memberships)

### Step 3 — Connect via Canvas

1. Open your workspace in Canvas → **Channels** tab → **+ Connect**
2. Select **Telegram**
3. Paste the bot token from Step 1
4. Click **Detect Chats** — this lists the chats the bot is currently in
5. Select the chats to connect → **Connect**

Or via API:
```bash
curl -X POST https://your-platform.com/workspaces/${WORKSPACE_ID}/channels \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "channel_type": "telegram",
    "config": {
      "bot_token": "123456789:ABCdefGHI..."
    },
    "allowed_users": []
  }'
```

### Step 4 — Verify

Send `/ask hello` to the bot in your Telegram chat. Your agent replies.

---

## What your agents can do

Once connected, your workspace agent handles:

| Command | What it does |
|---|---|
| `/ask <question>` | Routes the question to the agent, replies in the same chat |
| `/status` | Returns current agent status (idle / active tasks) |
| `/help` | Lists available commands |
| `/reset` | Clears conversation history (Telegram only) |

Slash commands are the interface. The agent decides what to do. Your Discord server or Telegram chat is the front-end your team already uses.

---

## Multi-chat setup

A single workspace channel serves multiple chats. Add chat IDs via Canvas or API:

```bash
# Via API — comma-separated chat IDs
curl -X PATCH https://your-platform.com/workspaces/${WORKSPACE_ID}/channels/${CHANNEL_ID} \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "config": {
      "bot_token": "123456789:ABCdefGHI...",
      "chat_id": "-100123456789, -100987654321"
    }
  }'
```

---

## Sending outbound messages

Agents can send messages to channels proactively — useful for notifications, summaries, or scheduled reports:

```python
# In your agent code
client.send_channel_message(
    channel_id="ch_abc123",
    text="Deployment complete. 47 tests passing."
)
```

Or via MCP tool:
```typescript
send_channel_message({ workspace_id, channel_id, text })
```

---

## Hot reload

Adding, updating, or removing a channel takes effect immediately — no platform restart required. Changes in Canvas → **Channels** tab are reflected within seconds.

---

## Security notes

- Discord webhook tokens are never logged. HTTP request errors surface as generic messages.
- Telegram bot tokens are stored server-side (SHA-256 hash in DB) and shown once at creation.
- Both adapters verify request signatures before processing (Discord: HMAC-SHA256, Telegram: token format validation).
- Add `allowed_users` to restrict access to specific Discord users or Telegram chat IDs.

---

## What's next

- [Discord adapter launch post](/docs/blog/discord-adapter-launch) — the full product story
- [Social channels architecture](/docs/agent-runtime/social-channels) — adapter interface, MCP tools, DB schema
- [Telegram tutorial](/docs/tutorials/lark-feishu-channel) — Lark/Feishu adapter (same pattern)
- [Remote agent tutorial](/docs/tutorials/register-remote-agent) — run your agent on a remote machine and connect it to a social channel

*EnterOS is open source. Discord adapter shipped in PR #656. Telegram adapter shipped in Phase 25.*
