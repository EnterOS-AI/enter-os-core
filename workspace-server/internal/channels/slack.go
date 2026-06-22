package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	slackWebhookPrefix = "https://hooks.slack.com/"
	slackHTTPTimeout   = 10 * time.Second
)

var slackHTTPClient = &http.Client{Timeout: slackHTTPTimeout}

// slackWebhookAccepted reports whether a Slack Incoming Webhook URL is allowed
// as a send destination. Production accepts only the real hooks.slack.com host.
//
// TEST SEAM (gating e2e): when MOLECULE_CHANNELS_TEST_WEBHOOK_BASE is set, a
// URL with that prefix is ALSO accepted so tests/e2e/test_channels_e2e.sh can
// point the live Slack send path at a local mock-upstream and assert the mock
// actually received the serialized {"text":...} payload end-to-end (the unit
// tests can only assert the body shape — see lark_test.go's prefix-gate
// workaround comment). The env var is NEVER set in any production/staging
// deploy; channelsTestWebhookBase() returns "" there and only the real
// hooks.slack.com prefix passes, so this changes no production behaviour.
func slackWebhookAccepted(u string) bool {
	if strings.HasPrefix(u, slackWebhookPrefix) {
		return true
	}
	if base := channelsTestWebhookBase(); base != "" && strings.HasPrefix(u, base) {
		return true
	}
	return false
}

// SlackAdapter implements ChannelAdapter for Slack Incoming Webhooks.
//
// Outbound messages are sent via Slack Incoming Webhooks (the simple,
// no-OAuth path). Inbound messages require Slack Event API / slash command
// configuration on the Slack App side; ParseWebhook handles the JSON payload
// that Slack POSTs to the registered webhook URL.
type SlackAdapter struct{}

func (s *SlackAdapter) Type() string        { return "slack" }
func (s *SlackAdapter) DisplayName() string { return "Slack" }

// ConfigSchema — Slack supports two mutually-exclusive outbound modes:
// Bot API (bot_token + channel_id, supports per-message identity override)
// and Incoming Webhook (webhook_url, legacy, no identity override). The
// form exposes both; ValidateConfig enforces "one or the other".
func (s *SlackAdapter) ConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "bot_token",
			Label:       "Bot Token (xoxb-…)",
			Type:        "password",
			Required:    false,
			Sensitive:   true,
			Placeholder: "xoxb-1234-5678-abc...",
			Help:        "Bot API mode — supports per-agent identity override. Required scopes: chat:write, chat:write.customize. Leave empty to use Incoming Webhook mode instead.",
		},
		{
			Key:         "channel_id",
			Label:       "Channel ID",
			Type:        "text",
			Required:    false,
			Placeholder: "C01234ABCDE",
			Help:        "Required when using Bot Token mode. From the channel's \"View channel details\" dialog.",
		},
		{
			Key:         "webhook_url",
			Label:       "Incoming Webhook URL (legacy)",
			Type:        "password",
			Required:    false,
			Sensitive:   true,
			Placeholder: "https://hooks.slack.com/services/T.../B.../...",
			Help:        "Simpler mode — no per-agent identity. Either Bot Token OR Webhook URL is required.",
		},
		{
			Key:         "username",
			Label:       "Override Username",
			Type:        "text",
			Required:    false,
			Placeholder: "optional, Bot Token mode only",
			Help:        "Display name to use on outbound messages. Ignored in Webhook mode.",
		},
		{
			Key:         "icon_emoji",
			Label:       "Override Icon Emoji",
			Type:        "text",
			Required:    false,
			Placeholder: ":robot_face:",
			Help:        "Emoji shortcode for per-message avatar. Ignored in Webhook mode.",
		},
	}
}

// ValidateConfig checks that the channel config contains a valid Slack
// Incoming Webhook URL (must start with https://hooks.slack.com/).
// Returns an error whose message becomes part of the 400 response body so
// keep it human-readable for the canvas UI.
func (s *SlackAdapter) ValidateConfig(config map[string]interface{}) error {
	botToken, _ := config["bot_token"].(string)
	webhookURL, _ := config["webhook_url"].(string)
	if botToken == "" && webhookURL == "" {
		return fmt.Errorf("missing required field: bot_token or webhook_url")
	}
	if botToken != "" {
		if cid, _ := config["channel_id"].(string); cid == "" {
			return fmt.Errorf("bot_token mode requires channel_id")
		}
	}
	if webhookURL != "" && !slackWebhookAccepted(webhookURL) {
		return fmt.Errorf("invalid Slack webhook URL")
	}
	return nil
}

// SendMessage posts text to Slack. Supports two modes:
//
//   - Bot API (bot_token set): uses chat.postMessage with per-agent identity
//     via chat:write.customize scope. Supports username + icon_emoji overrides.
//   - Webhook (webhook_url set, legacy): simple POST, no identity override.
//
// chatID overrides channel_id from config if non-empty (for multi-channel routing).
func (s *SlackAdapter) SendMessage(ctx context.Context, config map[string]interface{}, chatID string, text string) error {
	botToken, _ := config["bot_token"].(string)
	if botToken != "" {
		return s.sendBotMessage(ctx, config, chatID, text)
	}
	return s.sendWebhookMessage(ctx, config, text)
}

func (s *SlackAdapter) sendBotMessage(ctx context.Context, config map[string]interface{}, chatID, text string) error {
	botToken, _ := config["bot_token"].(string)
	channelID := chatID
	if channelID == "" {
		channelID, _ = config["channel_id"].(string)
	}
	if channelID == "" {
		return fmt.Errorf("slack: no channel_id")
	}

	username, _ := config["username"].(string)
	iconEmoji, _ := config["icon_emoji"].(string)

	// Convert Markdown → Slack mrkdwn before sending
	text = markdownToMrkdwn(text)

	// Split long messages at newline boundaries
	chunks := slackSplitMessage(text, 3000)
	for _, chunk := range chunks {
		payload := map[string]interface{}{
			"channel": channelID,
			"text":    chunk,
			// Use blocks with mrkdwn type for rich formatting.
			// The "text" field is the fallback for notifications/previews.
			"blocks": []map[string]interface{}{
				{
					"type": "section",
					"text": map[string]interface{}{
						"type": "mrkdwn",
						"text": chunk,
					},
				},
			},
		}
		if username != "" {
			payload["username"] = username
		}
		if iconEmoji != "" {
			payload["icon_emoji"] = iconEmoji
		}

		body, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			log.Printf("slack SendMessage: json.Marshal payload failed: %v", marshalErr)
			return fmt.Errorf("slack: marshal payload: %w", marshalErr)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/chat.postMessage", bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("slack: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set("Authorization", "Bearer "+botToken)

		resp, err := slackHTTPClient.Do(req)
		if err != nil {
			return fmt.Errorf("slack: send: %w", err)
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("slack: read response body: %w", readErr)
		}
		var result struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &result) == nil && !result.OK {
			return fmt.Errorf("slack: API error: %s", result.Error)
		}
	}
	return nil
}

func (s *SlackAdapter) sendWebhookMessage(ctx context.Context, config map[string]interface{}, text string) error {
	webhookURL, _ := config["webhook_url"].(string)
	if webhookURL == "" {
		return fmt.Errorf("webhook_url not configured")
	}
	if !slackWebhookAccepted(webhookURL) {
		return fmt.Errorf("invalid Slack webhook URL")
	}

	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("slack: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("slack: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("slack: webhook returned %d (read body failed: %v)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("slack: webhook returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// markdownToMrkdwn converts standard Markdown to Slack's mrkdwn format.
// Agents output standard MD (Claude Code default); Slack renders mrkdwn.
//
//	MD **bold** → mrkdwn *bold*
//	MD __italic__ or *italic* (standalone) → mrkdwn _italic_
//	MD ### heading → mrkdwn *heading* (bold, no heading syntax in Slack)
//	MD [text](url) → mrkdwn <url|text>
//	MD --- → mrkdwn ———
//	MD > quote → mrkdwn > quote (same, works as-is)
//	MD `code` → mrkdwn `code` (same)
//	MD ```block``` → mrkdwn ```block``` (same)
func markdownToMrkdwn(text string) string {
	// First pass: convert markdown tables to aligned plain text.
	// Slack has no table support — render as monospace columns.
	text = convertTables(text)

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Headings: ### Text → *Text*
		if strings.HasPrefix(trimmed, "#") {
			heading := strings.TrimLeft(trimmed, "# ")
			if heading != "" {
				lines[i] = "*" + heading + "*"
				continue
			}
		}

		// Horizontal rules → simple dashes (no unicode em-dash)
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			lines[i] = "----------"
			continue
		}

		// Strikethrough: ~~text~~ → ~text~ (Slack uses single tilde)
		for strings.Contains(lines[i], "~~") {
			first := strings.Index(lines[i], "~~")
			second := strings.Index(lines[i][first+2:], "~~")
			if second < 0 {
				break
			}
			second += first + 2
			inner := lines[i][first+2 : second]
			lines[i] = lines[i][:first] + "~" + inner + "~" + lines[i][second+2:]
		}

		// Links: [text](url) → <url|text>
		for {
			start := strings.Index(lines[i], "[")
			if start < 0 {
				break
			}
			mid := strings.Index(lines[i][start:], "](")
			if mid < 0 {
				break
			}
			mid += start
			end := strings.Index(lines[i][mid+2:], ")")
			if end < 0 {
				break
			}
			end += mid + 2
			linkText := lines[i][start+1 : mid]
			url := lines[i][mid+2 : end]
			lines[i] = lines[i][:start] + "<" + url + "|" + linkText + ">" + lines[i][end+1:]
		}

		// Bold: **text** → *text* (Slack bold is single asterisk)
		for strings.Contains(lines[i], "**") {
			first := strings.Index(lines[i], "**")
			second := strings.Index(lines[i][first+2:], "**")
			if second < 0 {
				break
			}
			second += first + 2
			inner := lines[i][first+2 : second]
			lines[i] = lines[i][:first] + "*" + inner + "*" + lines[i][second+2:]
		}
	}
	return strings.Join(lines, "\n")
}

// convertTables finds markdown tables and renders them as monospace blocks.
// Input:  | Col A | Col B |
//         |-------|-------|
//         | val1  | val2  |
// Output: ```
//         Col A     Col B
//         val1      val2
//         ```
func convertTables(text string) string {
	lines := strings.Split(text, "\n")
	var result []string
	i := 0
	for i < len(lines) {
		// Detect table start: line with | and next line is separator |---|
		if strings.Contains(lines[i], "|") && i+1 < len(lines) && isTableSeparator(lines[i+1]) {
			// Collect all table rows
			var headers []string
			var rows [][]string

			headers = parseTableRow(lines[i])
			i += 2 // skip header + separator

			for i < len(lines) && strings.Contains(lines[i], "|") && !isTableSeparator(lines[i]) {
				rows = append(rows, parseTableRow(lines[i]))
				i++
			}

			// Calculate column widths
			colWidths := make([]int, len(headers))
			for j, h := range headers {
				if len(h) > colWidths[j] {
					colWidths[j] = len(h)
				}
			}
			for _, row := range rows {
				for j, cell := range row {
					if j < len(colWidths) && len(cell) > colWidths[j] {
						colWidths[j] = len(cell)
					}
				}
			}

			// Render as monospace block
			result = append(result, "```")
			headerLine := ""
			for j, h := range headers {
				headerLine += padRight(h, colWidths[j]) + "  "
			}
			result = append(result, strings.TrimRight(headerLine, " "))
			// Separator
			sepLine := ""
			for j := range headers {
				sepLine += strings.Repeat("-", colWidths[j]) + "  "
			}
			result = append(result, strings.TrimRight(sepLine, " "))
			for _, row := range rows {
				rowLine := ""
				for j, cell := range row {
					if j < len(colWidths) {
						rowLine += padRight(cell, colWidths[j]) + "  "
					}
				}
				result = append(result, strings.TrimRight(rowLine, " "))
			}
			result = append(result, "```")
		} else {
			result = append(result, lines[i])
			i++
		}
	}
	return strings.Join(result, "\n")
}

func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.Contains(trimmed, "|") && strings.Contains(trimmed, "---")
}

func parseTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "|")
	parts := strings.Split(line, "|")
	var cells []string
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

func padRight(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func slackSplitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		end := maxLen
		if end > len(text) {
			end = len(text)
		}
		if end < len(text) {
			if idx := strings.LastIndex(text[:end], "\n"); idx > 0 {
				end = idx + 1
			}
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks
}

// ParseWebhook handles a Slack slash command or event API POST.
// The payload is either URL-encoded (slash commands) or JSON (Events API).
// Returns nil, nil for non-message events (e.g. url_verification challenge).
func (s *SlackAdapter) ParseWebhook(c *gin.Context, _ map[string]interface{}) (*InboundMessage, error) {
	contentType := c.GetHeader("Content-Type")

	var text, userID, username, channelID, msgID string

	if strings.Contains(contentType, "application/x-www-form-urlencoded") {
		// Slack slash command payload
		if err := c.Request.ParseForm(); err != nil {
			return nil, fmt.Errorf("slack: parse form: %w", err)
		}
		text = c.Request.FormValue("text")
		userID = c.Request.FormValue("user_id")
		username = c.Request.FormValue("user_name")
		channelID = c.Request.FormValue("channel_id")
		msgID = c.Request.FormValue("trigger_id")
		// Slash command: prepend the command itself so agent sees the full invocation
		if cmd := c.Request.FormValue("command"); cmd != "" {
			text = cmd + " " + text
		}
	} else {
		// Slack Events API JSON payload
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return nil, fmt.Errorf("slack: read body: %w", err)
		}

		var payload struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
			Event     struct {
				Type    string `json:"type"`
				User    string `json:"user"`
				Text    string `json:"text"`
				Channel string `json:"channel"`
				Ts      string `json:"ts"`
				BotID   string `json:"bot_id"`
				Subtype string `json:"subtype"`
			} `json:"event"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("slack: parse event: %w", err)
		}

		// url_verification handshake — respond with challenge directly
		if payload.Type == "url_verification" {
			c.JSON(200, gin.H{"challenge": payload.Challenge})
			return nil, nil
		}

		// Ignore bot messages to prevent echo loops. Our own auto-posts
		// via chat.postMessage fire Events API callbacks with bot_id set.
		if payload.Event.BotID != "" || payload.Event.Subtype == "bot_message" {
			return nil, nil
		}
		if payload.Event.Type != "message" || payload.Event.Text == "" {
			return nil, nil
		}

		text = payload.Event.Text
		userID = payload.Event.User
		channelID = payload.Event.Channel
		msgID = payload.Event.Ts
	}

	if text == "" {
		return nil, nil
	}

	return &InboundMessage{
		ChatID:    channelID,
		UserID:    userID,
		Username:  username,
		Text:      text,
		MessageID: msgID,
		Metadata:  map[string]string{"platform": "slack"},
	}, nil
}

// SlackHistoryMessage represents a single message from conversations.history.
type SlackHistoryMessage struct {
	User     string `json:"user"`
	Username string `json:"username"`
	Text     string `json:"text"`
	Ts       string `json:"ts"`
	BotID    string `json:"bot_id"`
}

// FetchChannelHistory calls Slack conversations.history and returns the
// last N messages from the channel, filtering out raw bot messages.
func FetchChannelHistory(ctx context.Context, botToken, channelID string, limit int) ([]SlackHistoryMessage, error) {
	if botToken == "" || channelID == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://slack.com/api/conversations.history?channel=%s&limit=%d", channelID, limit*2),
		nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+botToken)

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 65536))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("slack: read history response: %w", readErr)
	}

	var result struct {
		OK       bool                  `json:"ok"`
		Messages []SlackHistoryMessage `json:"messages"`
	}
	if json.Unmarshal(body, &result) != nil || !result.OK {
		return nil, fmt.Errorf("slack history API error")
	}

	var filtered []SlackHistoryMessage
	for _, m := range result.Messages {
		if m.BotID != "" && m.Username == "" {
			continue
		}
		filtered = append(filtered, m)
		if len(filtered) >= limit {
			break
		}
	}
	// Reverse: oldest first
	for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
		filtered[i], filtered[j] = filtered[j], filtered[i]
	}
	return filtered, nil
}

// StartPolling returns nil immediately. Slack does not support long-polling
// for Incoming Webhooks — use the Slack Events API + webhook route instead.
func (s *SlackAdapter) StartPolling(_ context.Context, _ map[string]interface{}, _ MessageHandler) error {
	return nil
}
