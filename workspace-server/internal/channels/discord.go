package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	discordWebhookPrefix = "https://discord.com/api/webhooks/"
	discordHTTPTimeout   = 10 * time.Second
)

// httpClient abstracts http.Client for test injection.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// DiscordAdapter implements ChannelAdapter for Discord.
//
// Outbound messages are sent via Discord Incoming Webhooks. The webhook URL
// (https://discord.com/api/webhooks/{id}/{token}) is the only required config
// field — it encodes the channel and bot-token so no separate bot setup is
// needed for outbound-only use.
//
// Inbound messages are received via Discord's Interactions endpoint (slash
// commands and message components). Discord POSTs a signed JSON payload to the
// configured Interactions URL; ParseWebhook extracts the text and returns a
// standardized InboundMessage. Signature verification must be performed at
// the router layer before calling ParseWebhook.
//
// StartPolling returns nil immediately — Discord does not support long-polling;
// use the Interactions webhook route instead.
type DiscordAdapter struct {
	// client allows dependency injection for testing. If nil, the default
	// http.Client is used at call time (safe for production use).
	client httpClient
}

func (d *DiscordAdapter) Type() string        { return "discord" }
func (d *DiscordAdapter) DisplayName() string { return "Discord" }

// ConfigSchema — Discord only needs a webhook URL for outbound.
// public_key is the Ed25519 pubkey used to verify inbound Interactions
// signatures (stored hex-encoded); not required if you only do outbound.
func (d *DiscordAdapter) ConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "webhook_url",
			Label:       "Webhook URL",
			Type:        "password",
			Required:    true,
			Sensitive:   true,
			Placeholder: "https://discord.com/api/webhooks/{id}/{token}",
			Help:        "From Server Settings → Integrations → Webhooks → Copy URL.",
		},
		{
			Key:         "public_key",
			Label:       "Interactions Public Key (hex)",
			Type:        "password",
			Required:    false,
			Sensitive:   true,
			Placeholder: "optional — for inbound slash commands",
			Help:        "Ed25519 public key from the Discord Developer Portal → General Information. Only needed to receive slash commands.",
		},
	}
}

// ValidateConfig checks that the channel config contains a valid Discord
// Incoming Webhook URL. Returns a human-readable error for the Canvas UI.
func (d *DiscordAdapter) ValidateConfig(config map[string]interface{}) error {
	webhookURL, _ := config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("missing required field: webhook_url")
	}
	if !strings.HasPrefix(webhookURL, discordWebhookPrefix) {
		return fmt.Errorf("invalid Discord webhook URL (must start with %s)", discordWebhookPrefix)
	}
	return nil
}

// SendMessage posts a text message to the configured Discord webhook.
// chatID is ignored — the destination channel is encoded in the webhook URL.
// Messages longer than 2000 characters are split into 2000-char chunks because
// Discord enforces a hard 2000-character limit per message.
func (d *DiscordAdapter) SendMessage(ctx context.Context, config map[string]interface{}, _ string, text string) error {
	webhookURL, _ := config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("discord: webhook_url not configured")
	}
	if !strings.HasPrefix(webhookURL, discordWebhookPrefix) {
		return fmt.Errorf("discord: invalid webhook URL")
	}

	const maxLen = 2000

	// Split long messages into chunks at word boundaries where possible.
	chunks := splitMessage(text, maxLen)

	client := d.client
	if client == nil {
		client = &http.Client{Timeout: discordHTTPTimeout}
	}
	for _, chunk := range chunks {
		payload, err := json.Marshal(map[string]string{"content": chunk})
		if err != nil {
			return fmt.Errorf("discord: marshal payload: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("discord: create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			// Do NOT wrap err — the *url.Error from http.Client.Do includes the
			// full request URL, which contains the Discord webhook token
			// (https://discord.com/api/webhooks/{id}/{token}). Wrapping with %w
			// would propagate that token into logs and error responses (#659).
			return fmt.Errorf("discord: HTTP request failed")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("discord: read response body: %w", readErr)
		}

		// Discord returns 204 No Content on success.
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			return fmt.Errorf("discord: webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}
	return nil
}

// ParseWebhook handles a Discord Interactions POST.
// Discord sends two types of payloads: type 1 (PING) and type 2 (APPLICATION_COMMAND / slash command).
// Returns nil, nil for PING payloads — the handler layer must respond with `{"type":1}` to pass
// Discord's endpoint verification. Returns an InboundMessage for APPLICATION_COMMAND payloads.
func (d *DiscordAdapter) ParseWebhook(c *gin.Context, _ map[string]interface{}) (*InboundMessage, error) {
	// Cap incoming webhook bodies at 1 MiB. Discord's Interactions API
	// payloads are well under 10 KiB in practice; the cap is a DoS
	// guard, not a functional limit.
	const maxDiscordWebhook = 1 << 20
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxDiscordWebhook))
	if err != nil {
		return nil, fmt.Errorf("discord: read body: %w", err)
	}

	var payload struct {
		Type int    `json:"type"` // 1=PING, 2=APPLICATION_COMMAND, 3=MESSAGE_COMPONENT
		ID   string `json:"id"`
		Data struct {
			Name    string `json:"name"` // slash command name
			Options []struct {
				Name  string      `json:"name"`
				Value interface{} `json:"value"`
			} `json:"options"`
		} `json:"data"`
		Member struct {
			User struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
		} `json:"member"`
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
		ChannelID string `json:"channel_id"`
		Token     string `json:"token"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("discord: parse interaction: %w", err)
	}

	// Type 1: PING from Discord during endpoint verification — let the handler layer respond.
	if payload.Type == 1 {
		return nil, nil
	}

	// Type 2 or 3: extract text from slash command name + options.
	if payload.Type != 2 && payload.Type != 3 {
		return nil, nil
	}

	// Reconstruct the invocation as text: "/command option1 option2"
	var parts []string
	if payload.Data.Name != "" {
		parts = append(parts, "/"+payload.Data.Name)
	}
	for _, opt := range payload.Data.Options {
		parts = append(parts, fmt.Sprintf("%v", opt.Value))
	}
	text := strings.TrimSpace(strings.Join(parts, " "))
	if text == "" {
		return nil, nil
	}

	// Prefer member.user (in guilds) over user (in DMs).
	userID := payload.Member.User.ID
	username := payload.Member.User.Username
	if userID == "" {
		userID = payload.User.ID
		username = payload.User.Username
	}

	return &InboundMessage{
		ChatID:    payload.ChannelID,
		UserID:    userID,
		Username:  username,
		Text:      text,
		MessageID: payload.ID,
		Metadata: map[string]string{
			"platform":          "discord",
			"interaction_token": payload.Token,
		},
	}, nil
}

// StartPolling returns nil immediately. Discord uses the Interactions endpoint
// (webhook-based) rather than long-polling for inbound messages.
func (d *DiscordAdapter) StartPolling(_ context.Context, _ map[string]interface{}, _ MessageHandler) error {
	return nil
}

// splitMessage splits text into chunks of at most maxLen characters.
// It tries to break at the last newline or space within the window to avoid
// cutting words in the middle, but hard-splits if no boundary is found.
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		cut := maxLen
		// Walk back from cut looking for a newline or space.
		for i := cut - 1; i > maxLen/2; i-- {
			if text[i] == '\n' || text[i] == ' ' {
				cut = i + 1
				break
			}
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}
