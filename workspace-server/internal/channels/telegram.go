package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	telegramPollInterval    = 2 * time.Second
	telegramDiscoverTimeout = 5 // seconds — for getUpdates long-poll during discovery
	telegramMaxMessageLen   = 4096
	telegramTypingInterval  = 4 * time.Second // re-send typing every 4s during long agent calls
)

var telegramTokenRegex = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{30,}$`)

// Bot instance cache — avoids `getMe` API call on every send.
// Keyed by bot token. Each NewBotAPI call hits Telegram's getMe endpoint.
var (
	botCacheMu sync.RWMutex
	botCache   = map[string]*tgbotapi.BotAPI{}
)

// TelegramAdapter implements ChannelAdapter for Telegram Bot API.
type TelegramAdapter struct{}

func (t *TelegramAdapter) Type() string        { return "telegram" }
func (t *TelegramAdapter) DisplayName() string { return "Telegram" }

// ConfigSchema — Telegram uses Bot API long-polling. The bot token comes
// from @BotFather; chat_id is a comma-separated list discovered via the
// "Detect Chats" UI flow (calls Bot.getUpdates).
func (t *TelegramAdapter) ConfigSchema() []ConfigField {
	return []ConfigField{
		{
			Key:         "bot_token",
			Label:       "Bot Token",
			Type:        "password",
			Required:    true,
			Sensitive:   true,
			Placeholder: "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			Help:        "From @BotFather → /newbot (or /token on an existing bot).",
		},
		{
			Key:         "chat_id",
			Label:       "Chat IDs",
			Type:        "text",
			Required:    true,
			Placeholder: "-100123456789, -100987654321",
			Help:        "Comma-separated chat IDs. Use \"Detect Chats\" after adding the bot to groups or sending /start in DMs.",
		},
	}
}

func (t *TelegramAdapter) ValidateConfig(config map[string]interface{}) error {
	token, _ := config["bot_token"].(string)
	if token == "" {
		return fmt.Errorf("missing required field: bot_token")
	}
	if !telegramTokenRegex.MatchString(token) {
		return fmt.Errorf("bot_token format invalid (expected like '123456789:ABCdefGHIjkl...')")
	}
	if _, ok := config["chat_id"]; !ok {
		return fmt.Errorf("missing required field: chat_id")
	}
	return nil
}

// getBot returns a cached BotAPI for the given token, creating one if needed.
// Caching avoids the `getMe` API call that NewBotAPI makes on every invocation.
func getBot(token string) (*tgbotapi.BotAPI, error) {
	botCacheMu.RLock()
	bot, ok := botCache[token]
	botCacheMu.RUnlock()
	if ok {
		return bot, nil
	}

	botCacheMu.Lock()
	defer botCacheMu.Unlock()
	// Double-check after acquiring write lock
	if bot, ok = botCache[token]; ok {
		return bot, nil
	}
	bot, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	botCache[token] = bot
	return bot, nil
}

// invalidateBot removes a bot from the cache (used when token becomes invalid).
func invalidateBot(token string) {
	botCacheMu.Lock()
	delete(botCache, token)
	botCacheMu.Unlock()
}

// welcomeMessage is sent when a user sends /start during discovery.
const welcomeMessage = "✅ Bot connected and ready.\n\nYour chat ID: `%d`\n\nPaste this ID in Molecule AI to link this chat to an agent, or use 'Detect Chats' to auto-fill it."

// connectedMessage is sent when /start is received in an already-connected chat.
const connectedMessage = "✅ Connected to Molecule AI agent. Send a message and I'll forward it."

// helpMessage describes available commands.
const helpMessage = `*Molecule AI Bot Commands*

/help — Show this help
/reset — Clear conversation history
/cancel — Cancel current request (best-effort)

Just send any message and I'll forward it to the agent.`

// botCommands registered with Telegram so users see autocomplete.
var botCommands = []tgbotapi.BotCommand{
	{Command: "start", Description: "Connect this chat to the agent"},
	{Command: "help", Description: "Show available commands"},
	{Command: "reset", Description: "Clear conversation history"},
	{Command: "cancel", Description: "Cancel current request"},
}

// DiscoverResult is returned from DiscoverChats — includes bot info and detected chats.
type DiscoverResult struct {
	BotUsername             string
	Chats                   []map[string]interface{}
	CanReadAllGroupMessages bool // false = group privacy mode is ON (bot only sees commands/mentions)
}

// DiscoverChats calls Telegram getUpdates to find groups/chats the bot has been added to.
//
// SIDE EFFECT: Auto-replies to /start messages so the user gets immediate feedback.
// Also registers bot commands via setMyCommands for autocomplete.
func (t *TelegramAdapter) DiscoverChats(ctx context.Context, botToken string) (*DiscoverResult, error) {
	if !telegramTokenRegex.MatchString(botToken) {
		return nil, errors.New("invalid bot token format")
	}

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return nil, fmt.Errorf("invalid bot token: %w", err)
	}

	// Cache the bot for subsequent sends
	botCacheMu.Lock()
	botCache[botToken] = bot
	botCacheMu.Unlock()

	// Register bot commands (idempotent — Telegram replaces the list each time)
	if _, err := bot.Request(tgbotapi.NewSetMyCommands(botCommands...)); err != nil {
		log.Printf("Channels: Telegram setMyCommands failed (non-fatal): %v", err)
	}

	// Remove webhook + drop pending updates so getUpdates works cleanly
	dropConfig := tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false}
	if _, reqErr := bot.Request(dropConfig); reqErr != nil {
		log.Printf("Channels: Telegram discover — delete webhook failed (may be ok): %v", reqErr)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = telegramDiscoverTimeout
	u.Limit = 100
	// Include my_chat_member so we discover groups the bot was added to without messages
	u.AllowedUpdates = []string{"message", "channel_post", "my_chat_member"}

	updates, err := bot.GetUpdates(u)
	if err != nil {
		return nil, fmt.Errorf("failed to get updates: %w", err)
	}

	// Deduplicate by chat ID
	seen := map[int64]bool{}
	var chats []map[string]interface{}

	addChat := func(chat *tgbotapi.Chat) {
		if chat == nil {
			return
		}
		if seen[chat.ID] {
			return
		}
		seen[chat.ID] = true
		name := chat.Title
		if name == "" {
			name = chat.FirstName
			if chat.LastName != "" {
				name += " " + chat.LastName
			}
		}
		chats = append(chats, map[string]interface{}{
			"chat_id": strconv.FormatInt(chat.ID, 10),
			"name":    name,
			"type":    chat.Type,
		})
	}

	for _, update := range updates {
		// Discover via my_chat_member events (bot added/removed from a group)
		if update.MyChatMember != nil {
			addChat(&update.MyChatMember.Chat)
			continue
		}

		var msg *tgbotapi.Message
		switch {
		case update.Message != nil:
			msg = update.Message
		case update.ChannelPost != nil:
			msg = update.ChannelPost
		default:
			continue
		}

		// Auto-reply to /start so user knows the bot works
		if strings.HasPrefix(msg.Text, "/start") {
			sendWithFallback(bot, tgbotapi.NewMessage(msg.Chat.ID, fmt.Sprintf(welcomeMessage, msg.Chat.ID)))
		}

		addChat(msg.Chat)
	}

	return &DiscoverResult{
		BotUsername:             bot.Self.UserName,
		Chats:                   chats,
		CanReadAllGroupMessages: bot.Self.CanReadAllGroupMessages,
	}, nil
}

// sendWithFallback sends a message with Markdown, falling back to plain text on error.
func sendWithFallback(bot *tgbotapi.BotAPI, msg tgbotapi.MessageConfig) {
	if msg.ParseMode == "" {
		msg.ParseMode = "Markdown"
	}
	if _, err := bot.Send(msg); err != nil {
		msg.ParseMode = ""
		if _, fallbackErr := bot.Send(msg); fallbackErr != nil {
			log.Printf("Channels: Telegram send failed (markdown=%v plain=%v)", err, fallbackErr)
		}
	}
}

// parseChatIDs splits a comma-separated chat_id string into individual IDs.
func parseChatIDs(config map[string]interface{}) []string {
	raw, _ := config["chat_id"].(string)
	if raw == "" {
		return nil
	}
	var ids []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			ids = append(ids, s)
		}
	}
	return ids
}

// isChatAllowed checks if a chat ID is in the configured list.
func isChatAllowed(config map[string]interface{}, chatID string) bool {
	ids := parseChatIDs(config)
	if len(ids) == 0 {
		return true // no restriction
	}
	for _, id := range ids {
		if id == chatID {
			return true
		}
	}
	return false
}

// splitLongMessage splits a long message at sensible boundaries (paragraph > line > char).
// Telegram limits messages to 4096 chars.
func splitLongMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	remaining := text
	for len(remaining) > maxLen {
		// Try to split at the last paragraph break before maxLen
		split := strings.LastIndex(remaining[:maxLen], "\n\n")
		if split == -1 {
			split = strings.LastIndex(remaining[:maxLen], "\n")
		}
		if split == -1 {
			split = strings.LastIndex(remaining[:maxLen], " ")
		}
		if split == -1 || split == 0 {
			split = maxLen
		}
		chunks = append(chunks, strings.TrimSpace(remaining[:split]))
		remaining = strings.TrimSpace(remaining[split:])
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}

func (t *TelegramAdapter) SendMessage(ctx context.Context, config map[string]interface{}, chatID string, text string) error {
	token, _ := config["bot_token"].(string)
	if token == "" {
		return fmt.Errorf("bot_token not configured")
	}

	cid, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat_id %q: %w", chatID, err)
	}

	bot, err := getBot(token)
	if err != nil {
		return fmt.Errorf("telegram bot init: %w", err)
	}

	chunks := splitLongMessage(text, telegramMaxMessageLen)
	for i, chunk := range chunks {
		msg := tgbotapi.NewMessage(cid, chunk)
		msg.ParseMode = "Markdown"
		msg.DisableWebPagePreview = true

		_, err = bot.Send(msg)
		if err != nil {
			// Handle typed Telegram errors
			var apiErr *tgbotapi.Error
			if errors.As(err, &apiErr) {
				switch apiErr.Code {
				case 401:
					invalidateBot(token)
					return fmt.Errorf("unauthorized: bot token revoked")
				case 403:
					return fmt.Errorf("forbidden: bot was blocked or kicked from chat %s", chatID)
				case 429:
					retryAfter := time.Duration(apiErr.RetryAfter) * time.Second
					log.Printf("Channels: Telegram rate-limited, retry after %s", retryAfter)
					time.Sleep(retryAfter)
					if _, retryErr := bot.Send(msg); retryErr != nil {
						return fmt.Errorf("rate limited: %w", retryErr)
					}
					continue
				}
			}

			// Retry without Markdown for malformed formatting (BadRequest)
			msg.ParseMode = ""
			if _, retryErr := bot.Send(msg); retryErr != nil {
				if i == 0 {
					return retryErr
				}
				log.Printf("Channels: Telegram chunk %d/%d send failed: %v", i+1, len(chunks), retryErr)
			}
		}
	}
	return nil
}

// SendTyping sends a "typing..." chat action so the user knows the bot is working.
// Telegram clears it after ~5s, so callers should re-send periodically.
func (t *TelegramAdapter) SendTyping(config map[string]interface{}, chatID string) {
	token, _ := config["bot_token"].(string)
	if token == "" {
		return
	}
	cid, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return
	}
	bot, err := getBot(token)
	if err != nil {
		return
	}
	action := tgbotapi.NewChatAction(cid, tgbotapi.ChatTyping)
	if _, err := bot.Request(action); err != nil {
		log.Printf("Channels: Telegram sendChatAction failed for %s: %v", chatID, err)
	}
}

func (t *TelegramAdapter) ParseWebhook(c *gin.Context, config map[string]interface{}) (*InboundMessage, error) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var update tgbotapi.Update
	if err := json.Unmarshal(body, &update); err != nil {
		return nil, fmt.Errorf("parse telegram update: %w", err)
	}

	// Handle channel_post in addition to message
	var msg *tgbotapi.Message
	switch {
	case update.Message != nil:
		msg = update.Message
	case update.ChannelPost != nil:
		msg = update.ChannelPost
	default:
		return nil, nil // Not a message update
	}

	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	var userID, username, firstName, lastName string
	if msg.From != nil {
		userID = strconv.FormatInt(msg.From.ID, 10)
		username = msg.From.UserName
		firstName = msg.From.FirstName
		lastName = msg.From.LastName
		if username == "" {
			username = firstName
		}
	} else {
		username = msg.Chat.Title // channel posts don't have From
	}

	return &InboundMessage{
		ChatID:    chatID,
		UserID:    userID,
		Username:  username,
		Text:      msg.Text,
		MessageID: strconv.Itoa(msg.MessageID),
		Metadata: map[string]string{
			"chat_type":  msg.Chat.Type,
			"first_name": firstName,
			"last_name":  lastName,
		},
	}, nil
}

func (t *TelegramAdapter) StartPolling(ctx context.Context, config map[string]interface{}, onMessage MessageHandler) error {
	token, _ := config["bot_token"].(string)
	if token == "" {
		return fmt.Errorf("bot_token not configured")
	}

	channelID, _ := config["_channel_id"].(string) // injected by manager
	chatIDs := parseChatIDs(config)

	bot, err := getBot(token)
	if err != nil {
		return fmt.Errorf("telegram bot init: %w", err)
	}

	// Remove any existing webhook so polling works
	if _, err := bot.Request(tgbotapi.DeleteWebhookConfig{}); err != nil {
		log.Printf("Channels: Telegram failed to delete webhook (polling may not work): %v", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	u.AllowedUpdates = []string{"message", "channel_post", "my_chat_member"}

	u.AllowedUpdates = append(u.AllowedUpdates, "callback_query")

	log.Printf("Channels: Telegram polling started for chats %v (bot: @%s)", chatIDs, bot.Self.UserName)

	for {
		select {
		case <-ctx.Done():
			log.Printf("Channels: Telegram polling stopped for chats %v", chatIDs)
			return nil
		default:
		}

		updates, err := bot.GetUpdates(u)
		if err != nil {
			// Honor 429 retry_after
			var apiErr *tgbotapi.Error
			if errors.As(err, &apiErr) {
				if apiErr.Code == 429 {
					retryAfter := time.Duration(apiErr.RetryAfter) * time.Second
					log.Printf("Channels: Telegram poll rate-limited, sleeping %s", retryAfter)
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(retryAfter):
						continue
					}
				}
				if apiErr.Code == 401 {
					invalidateBot(token)
					return fmt.Errorf("unauthorized: bot token revoked")
				}
			}
			log.Printf("Channels: Telegram poll error: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(telegramPollInterval):
				continue
			}
		}

		for _, update := range updates {
			u.Offset = update.UpdateID + 1

			// Handle callback_query (inline keyboard button clicks)
			if update.CallbackQuery != nil {
				cb := update.CallbackQuery
				chatID := strconv.FormatInt(cb.Message.Chat.ID, 10)

				// Acknowledge the button press (removes loading spinner)
				ackCfg := tgbotapi.NewCallback(cb.ID, "Received")
				bot.Send(ackCfg)

				// Update the message to show what was clicked
				decision := "approved"
				if strings.HasPrefix(cb.Data, "reject") {
					decision = "rejected"
				}
				editMsg := tgbotapi.NewEditMessageText(
					cb.Message.Chat.ID,
					cb.Message.MessageID,
					cb.Message.Text+"\n\n✅ CEO "+decision,
				)
				bot.Send(editMsg)

				// Route the decision as an inbound message to the agent
				inbound := &InboundMessage{
					ChatID:    chatID,
					UserID:    strconv.FormatInt(cb.From.ID, 10),
					Username:  cb.From.UserName,
					Text:      "CEO_DECISION: " + cb.Data,
					MessageID: strconv.Itoa(cb.Message.MessageID),
					Metadata: map[string]string{
						"callback_data": cb.Data,
						"decision":      decision,
					},
				}
				if err := onMessage(ctx, channelID, inbound); err != nil {
					log.Printf("Channels: Telegram callback handler error: %v", err)
				}
				continue
			}

			// Handle my_chat_member: auto-greet when bot is added to a new chat
			if update.MyChatMember != nil {
				handleMyChatMember(bot, update.MyChatMember)
				continue
			}

			// Both message and channel_post
			var msg *tgbotapi.Message
			switch {
			case update.Message != nil:
				msg = update.Message
			case update.ChannelPost != nil:
				msg = update.ChannelPost
			default:
				continue
			}

			chatID := strconv.FormatInt(msg.Chat.ID, 10)

			// Only process messages from configured chats
			if !isChatAllowed(config, chatID) {
				continue
			}

			// Bot commands handled inline (don't forward to agent)
			if handleCommand(ctx, bot, msg, channelID) {
				continue
			}

			var userID, username, firstName, lastName string
			if msg.From != nil {
				userID = strconv.FormatInt(msg.From.ID, 10)
				username = msg.From.UserName
				firstName = msg.From.FirstName
				lastName = msg.From.LastName
				if username == "" {
					username = firstName
				}
			}

			inbound := &InboundMessage{
				ChatID:    chatID,
				UserID:    userID,
				Username:  username,
				Text:      msg.Text,
				MessageID: strconv.Itoa(msg.MessageID),
				Metadata: map[string]string{
					"chat_type":  msg.Chat.Type,
					"first_name": firstName,
					"last_name":  lastName,
				},
			}

			if err := onMessage(ctx, channelID, inbound); err != nil {
				log.Printf("Channels: Telegram message handler error: %v", err)
			}
		}
	}
}

// handleCommand processes /start, /help, /reset, /cancel inline.
// Returns true if the message was a command and should not be forwarded.
func handleCommand(ctx context.Context, bot *tgbotapi.BotAPI, msg *tgbotapi.Message, channelID string) bool {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return false
	}

	// Strip @botname suffix (Telegram appends it in groups)
	cmd := strings.SplitN(text, " ", 2)[0]
	if at := strings.Index(cmd, "@"); at != -1 {
		cmd = cmd[:at]
	}

	switch cmd {
	case "/start":
		sendWithFallback(bot, tgbotapi.NewMessage(msg.Chat.ID, connectedMessage))
		return true
	case "/help":
		reply := tgbotapi.NewMessage(msg.Chat.ID, helpMessage)
		reply.ParseMode = "Markdown"
		sendWithFallback(bot, reply)
		return true
	case "/reset":
		clearChatHistory(ctx, channelID, strconv.FormatInt(msg.Chat.ID, 10))
		sendWithFallback(bot, tgbotapi.NewMessage(msg.Chat.ID, "🧹 Conversation history cleared."))
		return true
	case "/cancel":
		// Best-effort acknowledgment — actual cancel requires A2A plumbing
		sendWithFallback(bot, tgbotapi.NewMessage(msg.Chat.ID, "⚠️ Cancellation requested (best-effort)."))
		return true
	}
	return false
}

// handleMyChatMember responds when the bot is added to or removed from a chat.
func handleMyChatMember(bot *tgbotapi.BotAPI, update *tgbotapi.ChatMemberUpdated) {
	newStatus := update.NewChatMember.Status
	chat := update.Chat

	switch newStatus {
	case "member", "administrator":
		// Bot was added — send a friendly greeting
		greet := fmt.Sprintf(
			"👋 Hi! I'm a Molecule AI agent bot.\n\nThis chat ID is `%d`. An admin should add me to a workspace in Molecule AI to start chatting.",
			chat.ID,
		)
		reply := tgbotapi.NewMessage(chat.ID, greet)
		reply.ParseMode = "Markdown"
		sendWithFallback(bot, reply)
		log.Printf("Channels: Telegram bot added to chat %d (%s)", chat.ID, chat.Title)
	case "left", "kicked":
		log.Printf("Channels: Telegram bot removed from chat %d (%s)", chat.ID, chat.Title)
		// #123 — persist the disabled state so the next scheduled outbound
		// message doesn't try to send to a chat the bot no longer belongs
		// to (Telegram 403 spam). manager.go wires disableChannelByChatID
		// to an UPDATE on workspace_channels + manager reload.
		disableChannelByChatID(context.Background(), strconv.FormatInt(chat.ID, 10))
	}
}

// clearChatHistory is a hook called by /reset. The actual Redis call lives in manager.go;
// here we just invoke a callback registered there. For now, it's a no-op placeholder
// since the manager owns Redis access.
var clearChatHistory = func(ctx context.Context, channelID, chatID string) {
	// Set by manager.go init
}

// disableChannelByChatID is wired by manager.go to UPDATE workspace_channels
// SET enabled=false WHERE config->>'chat_id' = chatID, then reload the manager.
// Called from onMyChatMember when the bot is removed from a chat (#123). The
// default is a no-op so tests and early boot don't crash if the channels
// manager isn't initialised yet.
var disableChannelByChatID = func(ctx context.Context, chatID string) {
	// Set by manager.go init
}
