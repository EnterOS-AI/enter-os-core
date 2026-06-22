package channels

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Lark / Feishu (ByteDance) channel adapter — outbound via Custom Bot
// webhooks, inbound via Event Subscriptions.
//
// Outbound shape: POST <webhook_url> {"msg_type":"text","content":{"text":"..."}}
// Inbound shape:  POST <your-registered-url> with one of:
//   {"type":"url_verification","challenge":"...","token":"..."}     (handshake)
//   {"schema":"2.0","header":{"token":"...","event_type":"im.message.receive_v1"},
//    "event":{"sender":{"sender_id":{"user_id":"..."}},
//             "message":{"message_id":"...","chat_id":"...","content":"{\"text\":\"hi\"}"}}}
//
// Two URL families are accepted: open.feishu.cn (China) and open.larksuite.com
// (international). Both speak the same payload format — only the host differs.
type LarkAdapter struct{}

const (
	larkFeishuPrefix    = "https://open.feishu.cn/open-apis/bot/v2/hook/"
	larkLarkSuitePrefix = "https://open.larksuite.com/open-apis/bot/v2/hook/"
	larkHTTPTimeout     = 10 * time.Second
)

func (l *LarkAdapter) Type() string        { return "lark" }
func (l *LarkAdapter) DisplayName() string { return "Lark / Feishu" }

// ConfigSchema — Lark Custom Bot webhook URL + optional Event Subscription
// verify token. The webhook URL already encodes the chat, so no separate
// chat_id field is needed (and StartPolling is a no-op for Lark — inbound
// is delivered by ParseWebhook from the Event Subscription callback).
func (l *LarkAdapter) ConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "webhook_url",
			Label:       "Custom Bot Webhook URL",
			Type:        "password", // last path component is a secret
			Required:    true,
			Sensitive:   true,
			Placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX",
			Help:        "From the Lark/Feishu bot page → Webhook settings. open.feishu.cn (China) and open.larksuite.com (international) both accepted.",
		},
		{
			Key:         "verify_token",
			Label:       "Event Subscription Verify Token",
			Type:        "password",
			Required:    false,
			Sensitive:   true,
			Placeholder: "optional — from Event Subscriptions page",
			Help:        "Only needed if you want to receive messages from Lark. Paste the \"Verification Token\" from your app's Event Subscriptions configuration.",
		},
	}
}

// ValidateConfig requires webhook_url to point at a Lark or Feishu Custom
// Bot endpoint. verify_token is optional — when set, inbound events with a
// mismatching token are rejected (use Lark's "Verification Token" from the
// app's Event Subscriptions page).
func (l *LarkAdapter) ValidateConfig(config map[string]interface{}) error {
	webhookURL, _ := config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("missing required field: webhook_url")
	}
	if !isLarkWebhookURL(webhookURL) {
		return fmt.Errorf("invalid Lark/Feishu webhook URL — must start with %s or %s",
			larkFeishuPrefix, larkLarkSuitePrefix)
	}
	return nil
}

func isLarkWebhookURL(u string) bool {
	return strings.HasPrefix(u, larkFeishuPrefix) || strings.HasPrefix(u, larkLarkSuitePrefix)
}

// SendMessage posts text to the configured Lark/Feishu Custom Bot webhook.
// chatID is ignored — the chat is encoded in the webhook URL itself.
//
// Lark Custom Bot has no rate-limit tier we can rely on for batched output;
// callers that fan out should add their own back-pressure.
func (l *LarkAdapter) SendMessage(ctx context.Context, config map[string]interface{}, _ string, text string) error {
	webhookURL, _ := config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("webhook_url not configured")
	}
	if !isLarkWebhookURL(webhookURL) {
		return fmt.Errorf("invalid Lark/Feishu webhook URL")
	}

	payload, err := json.Marshal(map[string]interface{}{
		"msg_type": "text",
		"content":  map[string]string{"text": text},
	})
	if err != nil {
		return fmt.Errorf("lark: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("lark: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: larkHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("lark: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return fmt.Errorf("lark: read response body: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lark: webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Lark returns 200 even for application errors — the body's `code` field
	// is the truth. code:0 means delivered; anything else is a failure we
	// must surface to the caller, otherwise outbound looks healthy while
	// nothing reaches the chat.
	var apiResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &apiResp); err == nil && apiResp.Code != 0 {
		return fmt.Errorf("lark: api error code=%d msg=%s", apiResp.Code, apiResp.Msg)
	}
	return nil
}

// ParseWebhook handles both the url_verification handshake and event_callback
// payloads from Lark Event Subscriptions.
//
// The handshake (`type: "url_verification"`) returns nil, nil; the calling
// HTTP handler is responsible for echoing the challenge back to Lark — this
// matches the Slack pattern in slack.go (kept consistent so the receiver
// layer can stay generic).
//
// For event_callback we currently only surface the v2 message receive event
// (im.message.receive_v1). Other event types (reactions, member changes)
// return nil, nil so the receiver responds 200 OK without dispatching.
func (l *LarkAdapter) ParseWebhook(c *gin.Context, config map[string]interface{}) (*InboundMessage, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("lark: read body: %w", err)
	}

	// Probe for a v1 url_verification handshake first — it has a top-level
	// `type` field instead of the v2 `schema`/`header` wrapper.
	var probe struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Type == "url_verification" {
		// Verify token if operator configured one. Constant-time compare —
		// see #337: any place we compare a user-supplied value against a
		// stored secret must use subtle.ConstantTimeCompare.
		if expected, _ := config["verify_token"].(string); expected != "" {
			if subtle.ConstantTimeCompare([]byte(expected), []byte(probe.Token)) != 1 {
				return nil, fmt.Errorf("lark: url_verification token mismatch")
			}
		}
		return nil, nil
	}

	// v2 event payload
	var payload struct {
		Schema string `json:"schema"`
		Header struct {
			EventType string `json:"event_type"`
			Token     string `json:"token"`
		} `json:"header"`
		Event struct {
			Sender struct {
				SenderID struct {
					UserID  string `json:"user_id"`
					OpenID  string `json:"open_id"`
					UnionID string `json:"union_id"`
				} `json:"sender_id"`
			} `json:"sender"`
			Message struct {
				MessageID  string `json:"message_id"`
				ChatID     string `json:"chat_id"`
				ChatType   string `json:"chat_type"`
				MessageType string `json:"message_type"`
				Content    string `json:"content"` // JSON-encoded string, e.g. {"text":"hi"}
			} `json:"message"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("lark: parse event: %w", err)
	}

	// Verify token on event_callback too — same constant-time rule.
	if expected, _ := config["verify_token"].(string); expected != "" {
		if subtle.ConstantTimeCompare([]byte(expected), []byte(payload.Header.Token)) != 1 {
			return nil, fmt.Errorf("lark: event token mismatch")
		}
	}

	if payload.Header.EventType != "im.message.receive_v1" {
		return nil, nil // ignore non-message events
	}
	if payload.Event.Message.MessageType != "text" {
		return nil, nil // unsupported message type (image / file / sticker / etc.)
	}

	// content is a JSON-encoded string; for text messages it parses to {"text": "..."}.
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(payload.Event.Message.Content), &content); err != nil {
		return nil, fmt.Errorf("lark: parse message content: %w", err)
	}
	if content.Text == "" {
		return nil, nil
	}

	// Pick the most identifying sender ID Lark gave us — open_id is always
	// present; user_id is only set when the bot has the contacts permission.
	userID := payload.Event.Sender.SenderID.OpenID
	if payload.Event.Sender.SenderID.UserID != "" {
		userID = payload.Event.Sender.SenderID.UserID
	}

	return &InboundMessage{
		ChatID:    payload.Event.Message.ChatID,
		UserID:    userID,
		Text:      content.Text,
		MessageID: payload.Event.Message.MessageID,
		Metadata: map[string]string{
			"platform":  "lark",
			"chat_type": payload.Event.Message.ChatType,
		},
	}, nil
}

// StartPolling returns nil immediately. Lark/Feishu Custom Bots are
// outbound-only at the webhook layer; inbound is delivered via the Event
// Subscription HTTP callback handled by ParseWebhook.
func (l *LarkAdapter) StartPolling(_ context.Context, _ map[string]interface{}, _ MessageHandler) error {
	return nil
}
