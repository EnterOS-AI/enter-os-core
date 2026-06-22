// Package channels provides a pluggable adapter system for social channel
// integrations (Telegram, Slack, Discord, etc.). Each platform implements
// the ChannelAdapter interface and registers itself in the adapter registry.
package channels

import (
	"context"

	"github.com/gin-gonic/gin"
)

// ChannelAdapter is the interface every social channel must implement.
type ChannelAdapter interface {
	// Type returns the channel type identifier (e.g. "telegram", "slack").
	Type() string

	// DisplayName returns the human-readable name (e.g. "Telegram").
	DisplayName() string

	// ConfigSchema describes the config fields each adapter needs. The UI
	// renders the connect-channel form from this list, so each platform's
	// field set (Telegram bot_token+chat_id, Lark webhook_url+verify_token,
	// Slack bot_token+channel_id, Discord webhook_url) can be captured
	// correctly without per-platform UI branching. Adapters must return the
	// same schema on every call — the order is the rendering order.
	ConfigSchema() []ConfigField

	// ValidateConfig checks that channel_config JSONB has required fields.
	ValidateConfig(config map[string]interface{}) error

	// SendMessage sends a text message to the social platform.
	SendMessage(ctx context.Context, config map[string]interface{}, chatID string, text string) error

	// ParseWebhook extracts message info from an incoming webhook request.
	ParseWebhook(c *gin.Context, config map[string]interface{}) (*InboundMessage, error)

	// StartPolling begins long-polling for platforms that support it.
	// Returns nil immediately if the platform only supports webhooks.
	StartPolling(ctx context.Context, config map[string]interface{}, onMessage MessageHandler) error
}

// ConfigField describes a single config field for the channels connect-form UI.
// Canvas renders one input per field in order. Values are strings in
// channel_config JSONB — this struct carries only presentation + validation
// hints; ValidateConfig on the adapter is still the source of truth for
// acceptance.
type ConfigField struct {
	// Key is the channel_config map key (e.g. "webhook_url").
	Key string `json:"key"`
	// Label is the human-readable field name (e.g. "Webhook URL").
	Label string `json:"label"`
	// Type controls the HTML input type: "text" | "password" | "textarea".
	Type string `json:"type"`
	// Required marks the field as non-optional in the UI. Still enforced
	// server-side via ValidateConfig regardless of this flag.
	Required bool `json:"required"`
	// Sensitive means the value must not be logged or shown unmasked in
	// read APIs after creation. Canvas uses this to redact the value in
	// list responses; server-side encryption is governed by sensitiveFields
	// in secret.go (today: bot_token + webhook_secret only — this flag is
	// forward-looking until that list is widened).
	Sensitive bool `json:"sensitive"`
	// Placeholder is rendered as the input's placeholder attribute.
	Placeholder string `json:"placeholder,omitempty"`
	// Help is a short one-liner shown below the input.
	Help string `json:"help,omitempty"`
}

// InboundMessage is the standardized message from any social platform.
type InboundMessage struct {
	ChatID    string            // Platform-specific chat/channel ID
	UserID    string            // Platform-specific user ID
	Username  string            // Human-readable username
	Text      string            // Message text
	MessageID string            // Platform-specific message ID (for threading)
	Metadata  map[string]string // Extra platform-specific data
}

// MessageHandler is called by polling adapters when a message arrives.
type MessageHandler func(ctx context.Context, channelID string, msg *InboundMessage) error

// ChannelRow represents a row from the workspace_channels table.
type ChannelRow struct {
	ID           string
	WorkspaceID  string
	ChannelType  string
	Config       map[string]interface{}
	Enabled      bool
	AllowedUsers []string
}
