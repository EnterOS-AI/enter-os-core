package handlers

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/channels"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// ChannelHandler manages workspace social channel integrations.
type ChannelHandler struct {
	manager *channels.Manager
}

// channelSlugRe matches valid agent slugs used in [slug] routing.
// Compiled once at init to avoid recompilation on every webhook call.
var channelSlugRe = regexp.MustCompile(`^[a-zA-Z0-9 _-]+$`)

// NewChannelHandler creates a channel handler with the given manager.
func NewChannelHandler(manager *channels.Manager) *ChannelHandler {
	return &ChannelHandler{manager: manager}
}

// ListAdapters returns all available channel adapter types.
func (h *ChannelHandler) ListAdapters(c *gin.Context) {
	c.JSON(http.StatusOK, channels.ListAdapters())
}

// List returns all channels for a workspace.
func (h *ChannelHandler) List(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users,
		       last_message_at, message_count, created_at, updated_at
		FROM workspace_channels WHERE workspace_id = $1
		ORDER BY created_at
	`, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}
	defer rows.Close()

	result := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id, wsID, chType string
		var configJSON, allowedJSON []byte
		var enabled bool
		var lastMsg sql.NullTime
		var msgCount int
		var createdAt, updatedAt sql.NullTime

		if err := rows.Scan(&id, &wsID, &chType, &configJSON, &enabled, &allowedJSON, &lastMsg, &msgCount, &createdAt, &updatedAt); err != nil {
			continue
		}

		var config map[string]interface{}
		if err := json.Unmarshal(configJSON, &config); err != nil {
			log.Printf("Channels: unmarshal config for channel %s: %v", id, err)
			config = map[string]interface{}{}
		}
		// #319: decrypt sensitive fields first so the mask operates on
		// plaintext (first-4 / last-4 of the real token, not the ciphertext
		// prefix). Decrypt errors are logged but non-fatal — List must keep
		// returning the rest of the channel even if one field is corrupt.
		if err := channels.DecryptSensitiveFields(config); err != nil {
			log.Printf("Channels: decrypt config on list for channel %s: %v", id, err)
		}
		// Mask bot_token in list response
		if _, ok := config["bot_token"]; ok {
			token, _ := config["bot_token"].(string)
			if len(token) > 8 {
				config["bot_token"] = token[:4] + "..." + token[len(token)-4:]
			} else {
				config["bot_token"] = "***"
			}
		}

		var allowed []string
		if err := json.Unmarshal(allowedJSON, &allowed); err != nil {
			log.Printf("Channels: unmarshal allowed_users for channel %s: %v", id, err)
			allowed = []string{}
		}

		entry := map[string]interface{}{
			"id":            id,
			"workspace_id":  wsID,
			"channel_type":  chType,
			"config":        config,
			"enabled":       enabled,
			"allowed_users": allowed,
			"message_count": msgCount,
			"created_at":    createdAt.Time,
			"updated_at":    updatedAt.Time,
		}
		if lastMsg.Valid {
			entry["last_message_at"] = lastMsg.Time
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("Channels: list rows.Err: %v", err)
	}

	c.JSON(http.StatusOK, result)
}

// Create adds a new channel to a workspace.
func (h *ChannelHandler) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		ChannelType  string                 `json:"channel_type"`
		Config       map[string]interface{} `json:"config"`
		AllowedUsers []string               `json:"allowed_users"`
		Enabled      *bool                  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	if body.ChannelType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "channel_type is required"})
		return
	}

	adapter, ok := channels.GetAdapter(body.ChannelType)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported channel_type: " + body.ChannelType})
		return
	}

	if err := adapter.ValidateConfig(body.Config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid channel config"})
		return
	}

	// #319: encrypt sensitive fields (bot_token, webhook_secret) before
	// persisting. Exactly one call here; duplicate removed in this PR.
	// Validation above ran against plaintext; storage is ciphertext.
	if err := channels.EncryptSensitiveFields(body.Config); err != nil {
		log.Printf("Channels: encrypt config failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt failed"})
		return
	}

	configJSON, marshalErr := json.Marshal(body.Config)
	if marshalErr != nil {
		log.Printf("Channels create %s: json.Marshal config failed: %v", workspaceID, marshalErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal config failed"})
		return
	}
	allowedJSON, marshalErr := json.Marshal(body.AllowedUsers)
	if marshalErr != nil {
		log.Printf("Channels create %s: json.Marshal allowed_users failed: %v", workspaceID, marshalErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal allowed_users failed"})
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	var id string
	err := db.DB.QueryRowContext(ctx, `
		INSERT INTO workspace_channels (workspace_id, channel_type, channel_config, enabled, allowed_users)
		VALUES ($1, $2, $3::jsonb, $4, $5::jsonb)
		RETURNING id
	`, workspaceID, body.ChannelType, string(configJSON), enabled, string(allowedJSON)).Scan(&id)
	if err != nil {
		log.Printf("Channels: create failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create channel"})
		return
	}

	// Hot reload
	h.manager.Reload(ctx)

	c.JSON(http.StatusCreated, gin.H{
		"id":           id,
		"channel_type": body.ChannelType,
		"enabled":      enabled,
	})
}

// Update modifies a channel's config, allowlist, or enabled state.
func (h *ChannelHandler) Update(c *gin.Context) {
	workspaceID := c.Param("id")
	channelID := c.Param("channelId")
	ctx := c.Request.Context()

	var body struct {
		Config       map[string]interface{} `json:"config"`
		AllowedUsers []string               `json:"allowed_users"`
		Enabled      *bool                  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	// COALESCE-based update
	var configArg, allowedArg interface{}
	if body.Config != nil {
		// #319: re-encrypt sensitive fields on every config update — the
		// PATCH body carries plaintext (client already had them plaintext in
		// List response's unmasked path or typed fresh).
		if err := channels.EncryptSensitiveFields(body.Config); err != nil {
			log.Printf("Channels: encrypt update for workspace %s: %v", workspaceID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt failed"})
			return
		}
		j, marshalErr := json.Marshal(body.Config)
		if marshalErr != nil {
			log.Printf("Channels update %s: json.Marshal config failed: %v", workspaceID, marshalErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal config failed"})
			return
		}
		configArg = string(j)
	}
	if body.AllowedUsers != nil {
		j, marshalErr := json.Marshal(body.AllowedUsers)
		if marshalErr != nil {
			log.Printf("Channels update %s: json.Marshal allowed_users failed: %v", workspaceID, marshalErr)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal allowed_users failed"})
			return
		}
		allowedArg = string(j)
	}

	result, err := db.DB.ExecContext(ctx, `
		UPDATE workspace_channels
		SET channel_config = COALESCE($3::jsonb, channel_config),
		    allowed_users = COALESCE($4::jsonb, allowed_users),
		    enabled = COALESCE($5, enabled),
		    updated_at = now()
		WHERE id = $1 AND workspace_id = $2
	`, channelID, workspaceID, configArg, allowedArg, body.Enabled)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
		return
	}

	n, err := result.RowsAffected()
	if err != nil {
		log.Printf("Channel update RowsAffected error channel=%s workspace=%s: %v", channelID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}

	// Hot reload
	h.manager.Reload(ctx)

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

// Delete removes a channel from a workspace.
func (h *ChannelHandler) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	channelID := c.Param("channelId")
	ctx := c.Request.Context()

	result, err := db.DB.ExecContext(ctx, `
		DELETE FROM workspace_channels WHERE id = $1 AND workspace_id = $2
	`, channelID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}

	n, err := result.RowsAffected()
	if err != nil {
		log.Printf("Channel delete RowsAffected error channel=%s workspace=%s: %v", channelID, workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
		return
	}
	if n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel not found"})
		return
	}

	// Hot reload
	h.manager.Reload(ctx)

	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

// Send sends an outbound message from a workspace to its social channel.
func (h *ChannelHandler) Send(c *gin.Context) {
	channelID := c.Param("channelId")
	ctx := c.Request.Context()

	var body struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Text == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "text is required"})
		return
	}

	// Per-channel budget enforcement (#368).
	// Reads message_count and channel_budget in one query. If channel_budget IS
	// NOT NULL and message_count has already reached it, reject with 429.
	// DB errors are logged and treated as fail-open (budget not enforced) so a
	// transient DB hiccup doesn't silently block outbound messages.
	var msgCount int
	var budget sql.NullInt64
	if err := db.DB.QueryRowContext(ctx,
		`SELECT message_count, channel_budget FROM workspace_channels WHERE id = $1`,
		channelID,
	).Scan(&msgCount, &budget); err != nil && err != sql.ErrNoRows {
		log.Printf("Channels: budget check failed for channel %s: %v", channelID, err)
	}
	if budget.Valid && int64(msgCount) >= budget.Int64 {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "channel budget exceeded"})
		return
	}

	if err := h.manager.SendOutbound(ctx, channelID, body.Text); err != nil {
		log.Printf("Channels: send outbound failed for channel %s: %v", channelID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "send failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "sent"})
}

// Test sends a test message to verify the channel is working.
func (h *ChannelHandler) Test(c *gin.Context) {
	channelID := c.Param("channelId")
	ctx := c.Request.Context()

	if err := h.manager.SendOutbound(ctx, channelID, "🔔 Molecule AI channel test — connection successful!"); err != nil {
		log.Printf("Channels: test message failed for channel %s: %v", channelID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "test message failed"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "test message sent"})
}

// matchesChatID returns true if chatID is in the channel's comma-separated chat_id list.
// Exact match — avoids the substring-match bug of SQL LIKE.
func matchesChatID(config map[string]interface{}, chatID string) bool {
	raw, _ := config["chat_id"].(string)
	if raw == "" {
		return false
	}
	for _, s := range strings.Split(raw, ",") {
		if strings.TrimSpace(s) == chatID {
			return true
		}
	}
	return false
}

// Discover auto-detects chats/groups a bot has been added to by calling the platform API.
// User flow: enter bot token → add bot to groups → send a message → click Detect → select groups.
func (h *ChannelHandler) Discover(c *gin.Context) {
	var body struct {
		ChannelType string `json:"channel_type"`
		BotToken    string `json:"bot_token"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.BotToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bot_token is required"})
		return
	}
	// #329: workspace_id is required so PausePollersForToken can scope the
	// decryption lookup to the caller's tenant. Legacy clients that omit
	// the field still work — they just won't be able to pause a previously-
	// saved poller sharing the same token, which fails loudly at Telegram
	// with a 409 Conflict rather than silently decrypting every tenant's
	// token.
	if body.WorkspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id is required"})
		return
	}

	adapter, ok := channels.GetAdapter(body.ChannelType)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported channel_type"})
		return
	}

	// Only Telegram supports discovery currently
	tg, ok := adapter.(*channels.TelegramAdapter)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "discovery not supported for " + body.ChannelType})
		return
	}

	// Pause any active poller in THIS workspace using this bot token to
	// avoid Telegram's "only one getUpdates at a time" 409 Conflict.
	// #329: scoped to workspace_id so we never decrypt other tenants' tokens.
	resumeFn := h.manager.PausePollersForToken(body.WorkspaceID, body.BotToken)
	defer resumeFn()

	result, err := tg.DiscoverChats(c.Request.Context(), body.BotToken)
	if err != nil {
		// Map known errors to user-friendly messages
		msg := err.Error()
		userMsg := "Failed to connect to Telegram. Check your bot token and try again."
		if strings.Contains(msg, "invalid bot token") || strings.Contains(msg, "Unauthorized") || strings.Contains(msg, "Not Found") {
			userMsg = "Invalid bot token. Check the token from @BotFather and try again."
		} else if strings.Contains(msg, "Conflict") || strings.Contains(msg, "terminated by other") {
			userMsg = "This bot is already connected to another channel. Disconnect the existing channel first, or wait 30 seconds and retry."
		} else if strings.Contains(msg, "no route to host") || strings.Contains(msg, "i/o timeout") {
			userMsg = "Cannot reach Telegram API. Check your network connection and try again."
		}
		log.Printf("Channels: discover error: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": userMsg})
		return
	}

	hint := "For groups: add bot and send a message. For DMs: send /start to the bot. Then retry."
	var warning string
	if !result.CanReadAllGroupMessages {
		warning = "⚠️ Group privacy mode is ON for this bot — it only sees commands and @mentions in groups. " +
			"To let it see all group messages: open @BotFather → /mybots → " + result.BotUsername +
			" → Bot Settings → Group Privacy → Turn off, then re-add the bot to the group."
	}

	c.JSON(http.StatusOK, gin.H{
		"bot_username":                  result.BotUsername,
		"chats":                         result.Chats,
		"can_read_all_group_messages":   result.CanReadAllGroupMessages,
		"privacy_warning":               warning,
		"hint":                          hint,
	})
}

// Webhook handles incoming webhooks from any social platform.
func (h *ChannelHandler) Webhook(c *gin.Context) {
	channelType := c.Param("type")
	ctx := c.Request.Context()

	adapter, ok := channels.GetAdapter(channelType)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown channel type"})
		return
	}

	// Discord: verify Ed25519 signature BEFORE the body is consumed by ParseWebhook.
	// The app_public_key is the Discord application's public key (not a secret —
	// it's a PUBLIC key and therefore stored in plaintext in channel_config).
	// We look it up from the DB (first enabled Discord channel with the field set)
	// and fall back to the DISCORD_APP_PUBLIC_KEY env var for self-hosted setups
	// that prefer global configuration. Fail closed: no key configured → 401.
	// verifyDiscordSignature restores r.Body after reading so ParseWebhook below
	// can still read the payload.
	if channelType == "discord" {
		pubKey := discordPublicKey(ctx)
		if pubKey == "" || !verifyDiscordSignature(c.Request, pubKey) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}
	}

	// For webhooks, we need to find the channel by type and match by chat_id in the message
	// Parse the webhook first to get the chat_id
	msg, err := adapter.ParseWebhook(c, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "webhook parse failed"})
		return
	}
	if msg == nil {
		c.JSON(http.StatusOK, gin.H{"status": "ignored"}) // Non-message update
		return
	}

	// [slug] routing: if the message starts with [word], extract it as
	// a target agent slug and match against the channel config's username
	// field (lowercased). This lets humans type "[backend] what's #800?"
	// in a shared channel and route to a specific agent.
	targetSlug := ""
	routedText := msg.Text
	if len(msg.Text) > 2 && msg.Text[0] == '[' {
		if idx := strings.Index(msg.Text, "]"); idx > 1 && idx < 40 {
			candidate := strings.ToLower(strings.TrimSpace(msg.Text[1:idx]))
			if channelSlugRe.MatchString(candidate) {
				targetSlug = candidate
				routedText = strings.TrimSpace(msg.Text[idx+1:])
				if routedText == "" {
					routedText = msg.Text
				}
			}
		}
	}

	// Look up channels by type and find one whose chat_id list contains msg.ChatID.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users
		FROM workspace_channels
		WHERE channel_type = $1 AND enabled = true
	`, channelType)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "channel lookup failed"})
		return
	}
	defer rows.Close()

	var ch channels.ChannelRow
	var candidates []channels.ChannelRow
	found := false
	for rows.Next() {
		var row channels.ChannelRow
		var configJSON, allowedJSON []byte
		if err := rows.Scan(&row.ID, &row.WorkspaceID, &row.ChannelType, &configJSON, &row.Enabled, &allowedJSON); err != nil {
			continue
		}
		if err := json.Unmarshal(configJSON, &row.Config); err != nil {
			log.Printf("Channels: unmarshal config for webhook row %s: %v", row.ID, err)
			row.Config = map[string]interface{}{}
		}
		if err := json.Unmarshal(allowedJSON, &row.AllowedUsers); err != nil {
			log.Printf("Channels: unmarshal allowed_users for webhook row %s: %v", row.ID, err)
			row.AllowedUsers = []string{}
		}
		if err := channels.DecryptSensitiveFields(row.Config); err != nil {
			log.Printf("Channels: decrypt webhook row %s: %v", row.ID, err)
			continue
		}

		if expectedSecret, _ := row.Config["webhook_secret"].(string); expectedSecret != "" {
			receivedSecret := c.GetHeader("X-Telegram-Bot-Api-Secret-Token")
			if subtle.ConstantTimeCompare([]byte(receivedSecret), []byte(expectedSecret)) != 1 {
				continue
			}
		}

		if matchesChatID(row.Config, msg.ChatID) {
			candidates = append(candidates, row)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Channels: telegram webhook rows.Err: %v", err)
	}

	if targetSlug != "" {
		// [slug] routing — match against config username (lowercased)
		for _, row := range candidates {
			username, _ := row.Config["username"].(string)
			usernameLC := strings.ToLower(username)
			// Match: [backend] → "Backend Engineer", [pm] → "PM", [dev lead] → "Dev Lead"
			if usernameLC == targetSlug ||
				strings.HasPrefix(strings.ReplaceAll(usernameLC, " ", "-"), targetSlug) ||
				strings.HasPrefix(strings.ReplaceAll(usernameLC, " ", ""), targetSlug) {
				ch = row
				found = true
				msg.Text = routedText // Strip the [slug] prefix before routing
				break
			}
		}
		if !found {
			// No match for slug — respond with available agents
			var names []string
			for _, row := range candidates {
				if u, _ := row.Config["username"].(string); u != "" {
					names = append(names, "["+strings.ToLower(strings.ReplaceAll(u, " ", "-"))+"]")
				}
			}
			c.JSON(http.StatusOK, gin.H{
				"status":          "unknown_agent",
				"requested_slug":  targetSlug,
				"available_slugs": names,
			})
			return
		}
	} else if len(candidates) > 0 {
		// No [slug] prefix — route to first matching channel (backward compat)
		ch = candidates[0]
		found = true
	}

	if !found {
		c.JSON(http.StatusOK, gin.H{"status": "no_channel"})
		return
	}

	// Process asynchronously — don't block the webhook response.
	// RFC internal#524 Layer 1: globalGoAsync — HandleInbound traverses
	// db.DB to resolve workspace + record the channel event; drained by
	// drainTestAsync before db.DB swap.
	globalGoAsync(func() {
		bgCtx := context.Background()
		if err := h.manager.HandleInbound(bgCtx, ch, msg); err != nil {
			log.Printf("Channels: async HandleInbound error for workspace %s: %v", ch.WorkspaceID[:12], err)
		}
	})

	c.JSON(http.StatusOK, gin.H{"status": "accepted"})
}

// discordPublicKey returns the Ed25519 public key to use for Discord request
// signature verification. It queries the DB for the first enabled Discord
// channel whose config contains a non-empty app_public_key (stored in
// plaintext — it is a PUBLIC key and is not in the sensitiveFields list),
// then falls back to the DISCORD_APP_PUBLIC_KEY environment variable.
//
// Returns "" when no key is configured, which causes the caller to reject
// the incoming request with 401 (fail-closed behaviour).
func discordPublicKey(ctx context.Context) string {
	var pubKey string
	row := db.DB.QueryRowContext(ctx, `
		SELECT COALESCE(channel_config->>'app_public_key', '')
		FROM workspace_channels
		WHERE channel_type = 'discord' AND enabled = true
		  AND channel_config->>'app_public_key' IS NOT NULL
		  AND channel_config->>'app_public_key' != ''
		LIMIT 1
	`)
	_ = row.Scan(&pubKey)
	if pubKey != "" {
		return pubKey
	}
	return os.Getenv("DISCORD_APP_PUBLIC_KEY")
}

// verifyDiscordSignature verifies a Discord Interactions request using the
// Ed25519 signature scheme described in Discord's Interactions documentation.
// Discord signs the concatenation of the X-Signature-Timestamp header and the
// raw request body with the application's private key; we verify with the
// public key stored in channel_config or DISCORD_APP_PUBLIC_KEY.
//
// The function reads r.Body in full and then replaces it with a bytes.Reader
// over the same bytes so that subsequent callers (adapter.ParseWebhook) can
// still read the body.
//
// Returns false when any required header is missing, when pubKeyHex cannot
// be hex-decoded to a 32-byte Ed25519 public key, when the signature header
// cannot be decoded, or when the Ed25519 verification itself fails.
func verifyDiscordSignature(r *http.Request, pubKeyHex string) bool {
	sig := r.Header.Get("X-Signature-Ed25519")
	ts := r.Header.Get("X-Signature-Timestamp")
	if sig == "" || ts == "" || pubKeyHex == "" {
		return false
	}

	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		return false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	// Restore body so adapter.ParseWebhook can read it.
	r.Body = io.NopCloser(bytes.NewReader(body))

	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}

	msg := append([]byte(ts), body...)
	return ed25519.Verify(pubKeyBytes, msg, sigBytes)
}
