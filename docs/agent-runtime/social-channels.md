# Social Channels

Connect AI agent workspaces to social platforms (Telegram, Slack, Discord) so users can talk to agents from anywhere. Built on a pluggable adapter pattern — one channel per workspace, multiple chats per channel.

## Architecture

```
Telegram/Slack/Discord
    ↓ webhook or long-polling
Platform: ChannelAdapter.ParseWebhook() / StartPolling()
    ↓ allowlist check + Redis history lookup
ProxyA2ARequest(ctx, workspaceID, body, "channel:<type>", true)
    ↓ agent processes (existing A2A flow)
Reply text extracted from response
    ↓ ChannelAdapter.SendMessage()
Social chat ← reply (with typing indicator while waiting)
```

The `channel:<type>` caller prefix bypasses workspace hierarchy access checks (same pattern as `webhook:` and `system:`).

## Adapters

| Type | Status | Library |
|------|--------|---------|
| `telegram` | ✅ Implemented | `go-telegram-bot-api/v5` |
| `slack` | Planned | — |
| `discord` | ✅ Implemented (PR #656) | native `net/http` |
| `whatsapp` | Planned | — |

To add a new adapter: implement `ChannelAdapter` in `workspace-server/internal/channels/`, register in `registry.go`. Everything else (CRUD API, Canvas UI, MCP tools) works automatically.

## Telegram Setup

### 1. Create the bot
1. Talk to [@BotFather](https://t.me/BotFather) on Telegram → `/newbot`
2. Save the token (looks like `1234567890:ABCdefGHIjklMNOpqrSTUvwxYZ`)

### 2. Disable group privacy (recommended)
By default, Telegram bots in groups only see commands and @mentions. To let your bot see all group messages:
- @BotFather → `/mybots` → select your bot → **Bot Settings** → **Group Privacy** → **Turn off**
- Then **re-add the bot to the group** (privacy changes don't apply to existing memberships)

The Discover endpoint reports `can_read_all_group_messages` and surfaces a warning if privacy is on.

### 3. Connect via Canvas
1. Open the workspace in Canvas → **Channels** tab → **+ Connect**
2. Paste the bot token
3. Add the bot to your group(s) and send a message, OR send `/start` to it in DMs
4. Click **Detect Chats** → select the chats from the checklist
5. (Optional) Add **Allowed Users** for an allowlist
6. **Connect Channel**

### 4. Or connect via API
```bash
curl -X POST http://localhost:8080/workspaces/:id/channels \
  -H 'Content-Type: application/json' \
  -d '{
    "channel_type": "telegram",
    "config": {
      "bot_token": "1234567890:ABC...",
      "chat_id": "-100123, -100456"
    },
    "allowed_users": ["telegram_user_id_1"]
  }'
```

## Multi-chat IDs

A single channel entry serves multiple chats — `chat_id` is comma-separated:
```yaml
config:
  chat_id: "-100123, -100456, -100789"
```

The bot listens for messages from any of these chats and uses the same workspace agent for all of them. Outbound messages (e.g. agent-initiated notifications) are sent to all configured chats.

## Allowlist

Per-channel allowlist of user IDs (or chat IDs for groups). Empty = allow everyone.

```json
{ "allowed_users": ["123456789", "987654321"] }
```

When non-empty, messages from users not in the list are silently dropped (logged but no error).

## Bot Commands

The bot registers these commands via `setMyCommands`, so they appear in Telegram's command autocomplete:

| Command | Behavior |
|---------|----------|
| `/start` | Reply "Connected to EnterOS agent". Skipped if forwarded to agent. |
| `/help` | List all commands. |
| `/reset` | Clear conversation history (Redis key). |
| `/cancel` | Best-effort acknowledgment (no actual cancel plumbing yet). |

## Conversation History

Last 10 messages per chat stored in Redis at `channel:telegram:{chat_id}:history` with 24h TTL. Sent in A2A `metadata.history` so the agent has context. Same shape as Canvas chat history.

## Webhook Mode

Currently, channels run in long-polling mode by default. Webhook mode is implemented but requires:
1. Public URL pointing to `POST /webhooks/telegram` on your platform
2. Manual `setWebhook` call to Telegram with the URL + a `secret_token`
3. Storing the same `secret_token` in `channel_config.webhook_secret`

The platform verifies the `X-Telegram-Bot-Api-Secret-Token` header on every webhook request.

## Org Template Auto-Link

Channels can be defined in `org.yaml` so they're auto-created when the org is deployed. Config values support `${VAR}` expansion from `.env` files.

```yaml
workspaces:
  - name: PM
    files_dir: pm
    channels:
      - type: telegram
        config:
          bot_token: ${TELEGRAM_BOT_TOKEN}
          chat_id: ${TELEGRAM_CHAT_ID}
        allowed_users: []
        enabled: true
```

The vars are resolved from (in order): `pm/.env` → org root `.env` → platform process env. If any required var is unresolved, the channel is skipped with a clear log message and the skip reason is surfaced in the import response (`channels_skipped` field).

The platform calls `adapter.ValidateConfig()` upfront so unknown channel types or invalid configs fail fast. Insert is idempotent (`ON CONFLICT DO UPDATE`) so re-importing the same org refreshes the channel config.

## Discord Setup

### 1. Create a Discord Webhook
1. Open your Discord server → **Edit Channel** (or create a new one) → **Integrations** → **Webhooks**
2. Click **New Webhook** → name it → **Copy Webhook URL**
3. The URL looks like: `https://discord.com/api/webhooks/<id>/<token>`

### 2. Connect via Canvas
1. Open the workspace in Canvas → **Channels** tab → **+ Connect**
2. Paste the webhook URL
3. **Connect Channel**

### 3. Or connect via API
```bash
curl -X POST http://localhost:8080/workspaces/:id/channels \
  -H 'Content-Type: application/json' \
  -d '{
    "channel_type": "discord",
    "config": {
      "webhook_url": "https://discord.com/api/webhooks/YOUR_WEBHOOK_ID/YOUR_WEBHOOK_TOKEN"
    }
  }'
```

### 4. Register slash commands (for inbound)
Discord uses Application Commands (slash commands) for inbound messages. Register them in your Discord server's **Integration** page, or use Discord's developer portal to create global commands. The adapter parses `/<command> <options>` and passes them to the workspace agent.

### Inbound / Outbound
| Direction | Mechanism |
|---|---|
| **Inbound** | Discord Interactions endpoint (slash commands, message components) → `ParseWebhook()` |
| **Outbound** | Discord Incoming Webhooks → `SendMessage()` (2000-char chunking built in) |

**Note:** No Discord bot token is required for outbound-only use — the webhook URL encodes channel + token. For inbound slash commands, you need a Discord Application with an Interactions endpoint URL pointed at `POST /webhooks/discord` on your platform.

See `workspace-server/internal/channels/discord.go` for the full adapter implementation.

## Hot Reload

CRUD operations on `/workspaces/:id/channels` (POST, PATCH, DELETE) trigger `manager.Reload()`. Active polling goroutines are diffed against the desired DB state — new channels start, removed/disabled ones stop. No platform restart required.

The Discover endpoint also pauses any pollers using the same bot token to avoid Telegram's "only one `getUpdates` per bot" 409 Conflict, then resumes them after.

## Database

Migration `016_workspace_channels.sql`:
```sql
CREATE TABLE workspace_channels (
    id              UUID PRIMARY KEY,
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    channel_type    TEXT NOT NULL,          -- 'telegram', 'slack', etc.
    channel_config  JSONB NOT NULL,         -- adapter-specific (bot_token, chat_id, ...)
    enabled         BOOLEAN DEFAULT true,
    allowed_users   JSONB DEFAULT '[]',
    last_message_at TIMESTAMPTZ,
    message_count   INTEGER DEFAULT 0,
    ...
);
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/channels/adapters` | List available platforms |
| POST | `/channels/discover` | Detect chats for a bot token |
| GET | `/workspaces/:id/channels` | List channels (bot_token masked) |
| POST | `/workspaces/:id/channels` | Create channel (validates config) |
| PATCH | `/workspaces/:id/channels/:channelId` | Update config/enabled/allowlist |
| DELETE | `/workspaces/:id/channels/:channelId` | Remove channel |
| POST | `/workspaces/:id/channels/:channelId/send` | Outbound message |
| POST | `/workspaces/:id/channels/:channelId/test` | Send test message |
| POST | `/webhooks/:type` | Incoming webhook receiver |

## MCP Tools

```typescript
list_channel_adapters()                                              // list platforms
list_channels({ workspace_id })                                      // list channels
add_channel({ workspace_id, channel_type, config, allowed_users })   // hot reload
update_channel({ workspace_id, channel_id, config, enabled, allowed_users })
remove_channel({ workspace_id, channel_id })
send_channel_message({ workspace_id, channel_id, text })             // outbound
test_channel({ workspace_id, channel_id })                           // test connection
```

## Telegram-Specific Implementation Notes

- **Bot instance cache** (`sync.RWMutex`) avoids `getMe` API call on every send.
- **4096-char message splitting** at paragraph/line/word boundaries (Telegram's hard limit).
- **`sendChatAction("typing")`** goroutine re-sends every 4s during agent calls so the user sees "typing..." for the entire wait.
- **Markdown → plain text fallback** if the formatting fails (`ParseMode = "Markdown"` then retry without).
- **`my_chat_member` event handling** — when the bot is added to a chat, it auto-greets with the chat ID (no `/start` required).
- **Typed error handling**: 401 invalidates the bot cache; 403 returns a forbidden error; 429 honors `RetryAfter`.
- **Token format validation** via regex (`^\d+:[A-Za-z0-9_-]{30,}$`) before any API call.

## Files

| File | Purpose |
|------|---------|
| `workspace-server/internal/channels/adapter.go` | `ChannelAdapter` interface |
| `workspace-server/internal/channels/registry.go` | Adapter registry |
| `workspace-server/internal/channels/telegram.go` | Telegram implementation |
| `workspace-server/internal/channels/manager.go` | Orchestrator with hot reload |
| `workspace-server/internal/handlers/channels.go` | REST API + webhook |
| `workspace-server/migrations/016_workspace_channels.sql` | DB schema |
| `canvas/src/components/tabs/ChannelsTab.tsx` | Canvas UI |
| `mcp-server/src/index.ts` | 7 MCP tools |
