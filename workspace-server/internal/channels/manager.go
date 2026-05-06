package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/db"
	"github.com/Molecule-AI/molecule-monorepo/platform/internal/events"
)

const (
	// A2A timeout for channel messages (shorter than workspace-to-workspace).
	channelA2ATimeout = 5 * time.Minute
	// Max conversation history entries stored in Redis per chat.
	maxHistoryEntries = 10
	// Redis TTL for conversation history.
	historyTTL = 24 * time.Hour
)

// A2AProxy sends messages to workspaces via the A2A protocol.
type A2AProxy interface {
	ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error)
}

// Broadcaster records events and pushes them to WebSocket clients.
type Broadcaster interface {
	RecordAndBroadcast(ctx context.Context, eventType, workspaceID string, data interface{}) error
}

// Manager orchestrates all channel adapters with hot-reload support.
// When channels are added/removed/updated via API, call Reload() to
// pick up changes without restarting the platform.
type Manager struct {
	proxy       A2AProxy
	broadcaster Broadcaster

	// bgCtx is the long-lived context for spawning poller goroutines.
	// Set exactly once by Start() via startOnce. Pollers must use this —
	// NOT the request context from HTTP handlers, which gets cancelled
	// when the request finishes.
	bgCtx     context.Context
	startOnce sync.Once

	mu      sync.RWMutex
	pollers map[string]context.CancelFunc // channelID → cancel func
}

// NewManager creates a channel manager.
func NewManager(proxy A2AProxy, broadcaster Broadcaster) *Manager {
	m := &Manager{
		proxy:       proxy,
		broadcaster: broadcaster,
		pollers:     make(map[string]context.CancelFunc),
	}
	// Wire up the /reset command in the Telegram adapter to clear Redis history
	clearChatHistory = func(ctx context.Context, channelID, chatID string) {
		key := fmt.Sprintf("channel:telegram:%s:history", chatID)
		if db.RDB != nil {
			db.RDB.Del(ctx, key)
		}
	}
	// #123 — wire kicked/left events to mark the matching workspace_channels
	// row disabled and reload in-memory manager state. Without this, outbound
	// messages keep trying the dead chat and log 403s forever.
	disableChannelByChatID = func(ctx context.Context, chatID string) {
		if db.DB == nil {
			return
		}
		res, err := db.DB.ExecContext(ctx, `
			UPDATE workspace_channels
			SET enabled = false, updated_at = now()
			WHERE channel_type = 'telegram'
			  AND enabled = true
			  AND config->>'chat_id' = $1
		`, chatID)
		if err != nil {
			log.Printf("Channels: failed to disable telegram chat_id=%s: %v", chatID, err)
			return
		}
		if rows, _ := res.RowsAffected(); rows > 0 {
			log.Printf("Channels: disabled %d telegram channel(s) for chat_id=%s (bot removed)", rows, chatID)
			// Reload so the in-memory poller map drops the now-disabled row.
			m.Reload(ctx)
		}
	}
	return m
}

// Start loads all enabled channels from DB and starts polling goroutines.
// The provided ctx is used as the parent for all poller goroutines, so they
// stay alive for the lifetime of the manager (not for individual requests).
// Calling Start multiple times is a no-op after the first.
func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		m.bgCtx = ctx
		log.Println("Channels: manager started")
	})
	m.Reload(ctx)
}

// PausePollersForToken stops any pollers in the given workspace that share
// the given bot token, then returns a resume function. Used during discovery
// to avoid Telegram's "only one getUpdates at a time" 409 Conflict.
//
// #319: bot_token is stored encrypted in channel_config so we cannot match
// with SQL `channel_config->>'bot_token' = $1` anymore. Load channels,
// decrypt each, and compare the plaintext in Go.
//
// #329: scope the lookup to the requesting workspace. The unscoped variant
// loaded plaintext tokens for every tenant into memory on each discovery
// call — blast-radius concern if a heap dump / profiler leaked process
// memory. Reload() keeps the unscoped query since it legitimately needs
// every workspace's pollers at startup; PausePollersForToken operates in
// the context of a single workspace's API request and does not.
func (m *Manager) PausePollersForToken(workspaceID, botToken string) func() {
	if botToken == "" || workspaceID == "" {
		return func() {}
	}

	rows, err := db.DB.QueryContext(context.Background(), `
		SELECT id, channel_config FROM workspace_channels
		WHERE enabled = true AND workspace_id = $1
	`, workspaceID)
	if err != nil {
		return func() {}
	}
	defer func() { _ = rows.Close() }()

	var pausedIDs []string
	m.mu.Lock()
	for rows.Next() {
		var id string
		var configJSON []byte
		if err := rows.Scan(&id, &configJSON); err != nil {
			continue
		}
		var config map[string]interface{}
		if err := json.Unmarshal(configJSON, &config); err != nil {
			continue
		}
		if err := DecryptSensitiveFields(config); err != nil {
			log.Printf("Channels: pause-pollers decrypt error for %s: %v", truncID(id), err)
			continue
		}
		if token, _ := config["bot_token"].(string); token == botToken {
			if cancel, ok := m.pollers[id]; ok {
				cancel()
				delete(m.pollers, id)
				pausedIDs = append(pausedIDs, id)
				log.Printf("Channels: paused poller %s for discovery", truncID(id))
			}
		}
	}
	m.mu.Unlock()

	if len(pausedIDs) == 0 {
		return func() {}
	}

	// Resume by reloading — Reload starts pollers for any enabled channels not currently running
	return func() {
		// Wait briefly so Telegram releases the long-poll connection
		time.Sleep(1 * time.Second)
		m.Reload(context.Background())
		log.Printf("Channels: resumed %d poller(s) after discovery", len(pausedIDs))
	}
}

// Stop cancels all running pollers.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cancel := range m.pollers {
		cancel()
		delete(m.pollers, id)
	}
	log.Println("Channels: manager stopped")
}

// Reload re-reads enabled channels from DB and diffs against running pollers.
// New channels get started, removed/disabled channels get stopped.
func (m *Manager) Reload(ctx context.Context) {
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users
		FROM workspace_channels
		WHERE enabled = true
	`)
	if err != nil {
		log.Printf("Channels: reload query error: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()

	desired := make(map[string]ChannelRow)
	for rows.Next() {
		var ch ChannelRow
		var configJSON, allowedJSON []byte
		if err := rows.Scan(&ch.ID, &ch.WorkspaceID, &ch.ChannelType, &configJSON, &ch.Enabled, &allowedJSON); err != nil {
			log.Printf("Channels: reload scan error: %v", err)
			continue
		}
		_ = json.Unmarshal(configJSON, &ch.Config)
		_ = json.Unmarshal(allowedJSON, &ch.AllowedUsers)
		// #319: decrypt at the boundary between DB (ciphertext) and the
		// in-memory config adapters consume. A decrypt failure logs and
		// skips the channel — downstream getUpdates would fail anyway
		// with a mangled token so fail-closed here is kinder to operators.
		if err := DecryptSensitiveFields(ch.Config); err != nil {
			log.Printf("Channels: reload decrypt error for %s: %v", truncID(ch.ID), err)
			continue
		}
		desired[ch.ID] = ch
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Stop pollers that are no longer in the desired set
	for id, cancel := range m.pollers {
		if _, exists := desired[id]; !exists {
			cancel()
			delete(m.pollers, id)
			log.Printf("Channels: stopped poller for %s", truncID(id))
		}
	}

	// Start pollers for new channels
	for id, ch := range desired {
		if _, running := m.pollers[id]; running {
			continue
		}

		adapter, ok := GetAdapter(ch.ChannelType)
		if !ok {
			log.Printf("Channels: unknown adapter type %q for channel %s", ch.ChannelType, truncID(id))
			continue
		}

		// Use the manager's long-lived background context, NOT the request ctx
		// (request ctx gets cancelled when the HTTP handler returns, killing the poller)
		parent := m.bgCtx
		if parent == nil {
			parent = context.Background()
		}
		pollCtx, cancel := context.WithCancel(parent)
		m.pollers[id] = cancel

		// Inject channel ID into config for the polling callback
		ch.Config["_channel_id"] = ch.ID

		go func(a ChannelAdapter, c ChannelRow, pCtx context.Context) {
			if err := a.StartPolling(pCtx, c.Config, m.onInboundMessage); err != nil {
				log.Printf("Channels: polling error for %s/%s: %v", c.ChannelType, truncID(c.ID), err)
			}
		}(adapter, ch, pollCtx)

		log.Printf("Channels: started poller for %s/%s (workspace %s)", ch.ChannelType, truncID(id), truncID(ch.WorkspaceID))
	}

	log.Printf("Channels: reload complete — %d active pollers", len(m.pollers))
}

// onInboundMessage is called by polling adapters when a message arrives.
func (m *Manager) onInboundMessage(ctx context.Context, channelID string, msg *InboundMessage) error {
	ch, err := m.loadChannel(ctx, channelID)
	if err != nil {
		return fmt.Errorf("load channel: %w", err)
	}
	return m.HandleInbound(ctx, ch, msg)
}

// HandleInbound processes an incoming message from any social channel.
func (m *Manager) HandleInbound(ctx context.Context, ch ChannelRow, msg *InboundMessage) error {
	// Check allowlist
	if len(ch.AllowedUsers) > 0 {
		allowed := false
		for _, uid := range ch.AllowedUsers {
			if uid == msg.UserID || uid == msg.ChatID {
				allowed = true
				break
			}
		}
		if !allowed {
			log.Printf("Channels: blocked message from unauthorized user %s (chat %s)", msg.UserID, msg.ChatID)
			return nil
		}
	}

	// Load conversation history from Redis
	historyKey := fmt.Sprintf("channel:%s:%s:history", ch.ChannelType, msg.ChatID)
	history := m.loadHistory(ctx, historyKey)

	// Build A2A JSON-RPC payload
	a2aBody, _ := json.Marshal(map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": fmt.Sprintf("channel-%s-%s", ch.ChannelType, msg.MessageID),
				"parts":     []map[string]interface{}{{"kind": "text", "text": msg.Text}},
			},
			"metadata": map[string]interface{}{
				"source":     ch.ChannelType,
				"channel_id": ch.ID,
				"chat_id":    msg.ChatID,
				"user_id":    msg.UserID,
				"username":   msg.Username,
				"message_id": msg.MessageID,
				"history":    history,
				"extra":      msg.Metadata,
			},
		},
	})

	callerID := "channel:" + ch.ChannelType

	log.Printf("Channels: %s message from @%s → workspace %s", ch.ChannelType, msg.Username, truncID(ch.WorkspaceID))

	fireCtx, cancel := context.WithTimeout(ctx, channelA2ATimeout)
	defer cancel()

	// Show typing indicator throughout the agent call so user knows we're working.
	// Telegram clears it after ~5s, so we re-send every 4s in a goroutine.
	if tg, ok := GetAdapter(ch.ChannelType); ok {
		if typer, ok := tg.(interface {
			SendTyping(config map[string]interface{}, chatID string)
		}); ok {
			typingCtx, typingCancel := context.WithCancel(fireCtx)
			defer typingCancel()
			go func() {
				typer.SendTyping(ch.Config, msg.ChatID)
				ticker := time.NewTicker(4 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-typingCtx.Done():
						return
					case <-ticker.C:
						typer.SendTyping(ch.Config, msg.ChatID)
					}
				}
			}()
		}
	}

	statusCode, respBody, err := m.proxy.ProxyA2ARequest(fireCtx, ch.WorkspaceID, a2aBody, callerID, true)
	if err != nil {
		log.Printf("Channels: A2A error for %s: %v", truncID(ch.WorkspaceID), err)
		return fmt.Errorf("a2a proxy: %w", err)
	}

	// Extract response text
	replyText := m.extractReplyText(respBody, statusCode)

	// Send reply back to social platform
	adapter, ok := GetAdapter(ch.ChannelType)
	if !ok {
		return fmt.Errorf("no adapter for %s", ch.ChannelType)
	}

	if replyText != "" {
		if err := adapter.SendMessage(ctx, ch.Config, msg.ChatID, replyText); err != nil {
			log.Printf("Channels: send reply error: %v", err)
			return fmt.Errorf("send reply: %w", err)
		}
	}

	// Update conversation history in Redis
	m.appendHistory(ctx, historyKey, msg.Username, msg.Text, replyText)

	// Update stats in DB
	if db.DB != nil {
		db.DB.ExecContext(ctx, `
			UPDATE workspace_channels
			SET last_message_at = now(), message_count = message_count + 1, updated_at = now()
			WHERE id = $1
		`, ch.ID)
	}

	// Broadcast event
	if m.broadcaster != nil {
		m.broadcaster.RecordAndBroadcast(ctx, string(events.EventChannelMessage), ch.WorkspaceID, map[string]interface{}{
			"channel_id":   ch.ID,
			"channel_type": ch.ChannelType,
			"username":     msg.Username,
			"direction":    "inbound",
		})
	}

	return nil
}

// SendOutbound sends a message from a workspace to its connected social channel.
func (m *Manager) SendOutbound(ctx context.Context, channelID string, text string) error {
	ch, err := m.loadChannel(ctx, channelID)
	if err != nil {
		return err
	}

	adapter, ok := GetAdapter(ch.ChannelType)
	if !ok {
		return fmt.Errorf("no adapter for %s", ch.ChannelType)
	}

	chatIDRaw, _ := ch.Config["chat_id"].(string)
	if chatIDRaw == "" {
		return fmt.Errorf("no chat_id configured for channel %s", channelID)
	}

	// Send to all configured chat IDs (comma-separated)
	for _, cid := range splitChatIDs(chatIDRaw) {
		if err := adapter.SendMessage(ctx, ch.Config, cid, text); err != nil {
			log.Printf("Channels: outbound send to %s failed: %v", cid, err)
		}
	}

	if db.DB != nil {
		db.DB.ExecContext(ctx, `
			UPDATE workspace_channels
			SET last_message_at = now(), message_count = message_count + 1, updated_at = now()
			WHERE id = $1
		`, channelID)
	}

	if m.broadcaster != nil {
		m.broadcaster.RecordAndBroadcast(ctx, string(events.EventChannelMessage), ch.WorkspaceID, map[string]interface{}{
			"channel_id":   ch.ID,
			"channel_type": ch.ChannelType,
			"direction":    "outbound",
		})
	}

	return nil
}

// BroadcastToWorkspaceChannels sends a message to ALL enabled channels
// configured for a workspace. Used by the scheduler to auto-post cron
// output summaries and by delegation handlers to post completion notices.
//
// Unlike SendOutbound (which targets a specific channel row by ID), this
// fans out to every enabled channel for the workspace — so a single cron
// completion posts to both #mol-engineering AND #mol-firehose if the
// workspace has both configured via chat_id comma-separation.
func (m *Manager) BroadcastToWorkspaceChannels(ctx context.Context, workspaceID, text string) {
	if text == "" || db.DB == nil {
		return
	}
	// Truncate to keep Slack messages digestible (rune-safe for CJK/emoji)
	runes := []rune(text)
	if len(runes) > 500 {
		text = string(runes[:497]) + "..."
	}
	// Only auto-post to Slack channels. Telegram is CEO-only — explicit
	// escalations via the agent's outbound call, never auto-post from crons.
	rows, err := db.DB.QueryContext(ctx, `
		SELECT id FROM workspace_channels
		WHERE workspace_id = $1 AND enabled = true AND channel_type = 'slack'
	`, workspaceID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var channelID string
		if rows.Scan(&channelID) == nil {
			if sendErr := m.SendOutbound(ctx, channelID, text); sendErr != nil {
				log.Printf("Channels: broadcast to %s failed: %v", channelID[:12], sendErr)
			}
		}
	}
}

// FetchWorkspaceChannelContext returns recent Slack channel messages formatted
// as ambient context for cron prompts (Level 3).
func (m *Manager) FetchWorkspaceChannelContext(ctx context.Context, workspaceID string) string {
	if db.DB == nil {
		return ""
	}
	rows, err := db.DB.QueryContext(ctx, `
		SELECT channel_config FROM workspace_channels
		WHERE workspace_id = $1 AND channel_type = 'slack' AND enabled = true
		LIMIT 1
	`, workspaceID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var configJSON []byte
	if rows.Scan(&configJSON) != nil {
		return ""
	}
	var config map[string]interface{}
	json.Unmarshal(configJSON, &config)
	if err := DecryptSensitiveFields(config); err != nil {
		return ""
	}
	botToken, _ := config["bot_token"].(string)
	channelID, _ := config["channel_id"].(string)
	if botToken == "" || channelID == "" {
		return ""
	}
	messages, err := FetchChannelHistory(ctx, botToken, channelID, 10)
	if err != nil || len(messages) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Slack channel context — recent team messages]\n")
	for _, msg := range messages {
		name := msg.Username
		if name == "" {
			name = msg.User
		}
		text := msg.Text
		if len(text) > 200 {
			text = text[:197] + "..."
		}
		sb.WriteString(fmt.Sprintf("- %s: %s\n", name, text))
	}
	return sb.String()
}

func splitChatIDs(raw string) []string {
	var ids []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

func truncID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (m *Manager) loadChannel(ctx context.Context, channelID string) (ChannelRow, error) {
	var ch ChannelRow
	var configJSON, allowedJSON []byte
	err := db.DB.QueryRowContext(ctx, `
		SELECT id, workspace_id, channel_type, channel_config, enabled, allowed_users
		FROM workspace_channels WHERE id = $1
	`, channelID).Scan(&ch.ID, &ch.WorkspaceID, &ch.ChannelType, &configJSON, &ch.Enabled, &allowedJSON)
	if err != nil {
		return ch, fmt.Errorf("channel %s not found: %w", channelID, err)
	}
	json.Unmarshal(configJSON, &ch.Config)
	json.Unmarshal(allowedJSON, &ch.AllowedUsers)
	// #319: decrypt bot_token / webhook_secret — SendOutbound and adapter
	// methods downstream read them as plaintext strings.
	if err := DecryptSensitiveFields(ch.Config); err != nil {
		return ch, fmt.Errorf("decrypt channel %s: %w", channelID, err)
	}
	return ch, nil
}

func (m *Manager) extractReplyText(respBody []byte, statusCode int) string {
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Sprintf("Error: agent returned HTTP %d", statusCode)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return ""
	}

	// Try result.parts[].text (standard A2A response)
	if result, ok := resp["result"].(map[string]interface{}); ok {
		if parts, ok := result["parts"].([]interface{}); ok {
			for _, p := range parts {
				if part, ok := p.(map[string]interface{}); ok {
					if text, ok := part["text"].(string); ok {
						return text
					}
				}
			}
		}
		// Try result.artifacts[].parts[].text
		if artifacts, ok := result["artifacts"].([]interface{}); ok {
			for _, a := range artifacts {
				if artifact, ok := a.(map[string]interface{}); ok {
					if parts, ok := artifact["parts"].([]interface{}); ok {
						for _, p := range parts {
							if part, ok := p.(map[string]interface{}); ok {
								if text, ok := part["text"].(string); ok {
									return text
								}
							}
						}
					}
				}
			}
		}
	}

	return ""
}

func (m *Manager) loadHistory(ctx context.Context, key string) []map[string]string {
	if db.RDB == nil {
		return nil
	}
	entries, err := db.RDB.LRange(ctx, key, 0, int64(maxHistoryEntries-1)).Result()
	if err != nil {
		return nil
	}
	history := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		var h map[string]string
		if json.Unmarshal([]byte(e), &h) == nil {
			history = append(history, h)
		}
	}
	return history
}

func (m *Manager) appendHistory(ctx context.Context, key string, username, userMsg, agentReply string) {
	if db.RDB == nil {
		return
	}
	entry, _ := json.Marshal(map[string]string{
		"user":    username,
		"message": userMsg,
		"reply":   agentReply,
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
	db.RDB.LPush(ctx, key, string(entry))
	db.RDB.LTrim(ctx, key, 0, int64(maxHistoryEntries-1))
	db.RDB.Expire(ctx, key, historyTTL)
}
