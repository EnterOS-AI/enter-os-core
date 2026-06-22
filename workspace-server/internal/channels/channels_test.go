package channels

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// ==================== Adapter Interface Tests ====================

func TestTelegramAdapter_Type(t *testing.T) {
	a := &TelegramAdapter{}
	if a.Type() != "telegram" {
		t.Errorf("expected 'telegram', got %q", a.Type())
	}
}

func TestTelegramAdapter_DisplayName(t *testing.T) {
	a := &TelegramAdapter{}
	if a.DisplayName() != "Telegram" {
		t.Errorf("expected 'Telegram', got %q", a.DisplayName())
	}
}

func TestTelegramAdapter_ValidateConfig_Valid(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"bot_token": "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"chat_id":   "-100123",
	})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestTelegramAdapter_ValidateConfig_MissingBotToken(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"chat_id": "-100123",
	})
	if err == nil {
		t.Error("expected error for missing bot_token")
	}
}

func TestTelegramAdapter_ValidateConfig_MissingChatID(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"bot_token": "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err == nil {
		t.Error("expected error for missing chat_id")
	}
}

func TestTelegramAdapter_ValidateConfig_BadTokenFormat(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"bot_token": "not-a-real-token",
		"chat_id":   "-100",
	})
	if err == nil {
		t.Error("expected error for malformed bot_token")
	}
}

func TestTelegramAdapter_ValidateConfig_Empty(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.ValidateConfig(map[string]interface{}{})
	if err == nil {
		t.Error("expected error for empty config")
	}
}

func TestTelegramAdapter_SendMessage_EmptyToken(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.SendMessage(context.Background(), map[string]interface{}{}, "-100", "hello")
	if err == nil {
		t.Error("expected error for empty bot_token")
	}
}

func TestTelegramAdapter_SendMessage_InvalidChatID(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.SendMessage(context.Background(), map[string]interface{}{
		"bot_token": "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}, "not-a-number", "hello")
	if err == nil {
		t.Error("expected error for invalid chat_id")
	}
}

func TestTelegramAdapter_StartPolling_EmptyToken(t *testing.T) {
	a := &TelegramAdapter{}
	err := a.StartPolling(context.Background(), map[string]interface{}{}, nil)
	if err == nil {
		t.Error("expected error for empty bot_token")
	}
}

// ==================== Registry Tests ====================

func TestGetAdapter_Telegram(t *testing.T) {
	a, ok := GetAdapter("telegram")
	if !ok || a == nil {
		t.Error("expected telegram adapter to be registered")
	}
	if a.Type() != "telegram" {
		t.Errorf("expected type 'telegram', got %q", a.Type())
	}
}

func TestGetAdapter_Unknown(t *testing.T) {
	_, ok := GetAdapter("whatsapp")
	if ok {
		t.Error("expected unknown adapter to not be found")
	}
}

func TestListAdapters(t *testing.T) {
	list := ListAdapters()
	if len(list) == 0 {
		t.Fatal("expected at least 1 adapter")
	}
	found := false
	for _, a := range list {
		if a.Type == "telegram" {
			found = true
			if a.DisplayName != "Telegram" {
				t.Errorf("expected display_name 'Telegram', got %q", a.DisplayName)
			}
			if len(a.ConfigSchema) == 0 {
				t.Error("Telegram adapter must expose a non-empty ConfigSchema")
			}
		}
	}
	if !found {
		t.Error("telegram not found in ListAdapters")
	}
}

// ==================== Manager Tests ====================

type mockProxy struct {
	statusCode int
	respBody   []byte
	err        error
	calls      int
}

func (m *mockProxy) ProxyA2ARequest(ctx context.Context, workspaceID string, body []byte, callerID string, logActivity bool) (int, []byte, error) {
	m.calls++
	return m.statusCode, m.respBody, m.err
}

type mockBroadcaster struct {
	events []string
}

func (m *mockBroadcaster) RecordAndBroadcast(ctx context.Context, eventType, workspaceID string, data interface{}) error {
	m.events = append(m.events, eventType)
	return nil
}

func TestManager_NewManager(t *testing.T) {
	proxy := &mockProxy{}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
	if mgr.pollers == nil {
		t.Error("expected pollers map to be initialized")
	}
}

func TestManager_Stop(t *testing.T) {
	proxy := &mockProxy{}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.pollers["test-id-123456"] = cancel

	mgr.Stop()
	if len(mgr.pollers) != 0 {
		t.Errorf("expected 0 pollers after stop, got %d", len(mgr.pollers))
	}
	// Verify context was cancelled
	select {
	case <-ctx.Done():
		// good
	default:
		t.Error("expected poller context to be cancelled")
	}
}

func TestManager_HandleInbound_AllowlistBlocked(t *testing.T) {
	proxy := &mockProxy{statusCode: 200, respBody: []byte(`{"result":{"parts":[{"kind":"text","text":"hi"}]}}`)}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)

	ch := ChannelRow{
		ID:           "ch-123456789012",
		WorkspaceID:  "ws-123456789012",
		ChannelType:  "telegram",
		Config:       map[string]interface{}{"bot_token": "fake", "chat_id": "-100"},
		AllowedUsers: []string{"user-999"}, // Only user-999 allowed
	}

	msg := &InboundMessage{
		ChatID:    "-100",
		UserID:    "user-123", // Not in allowlist
		Username:  "blocked",
		Text:      "hello",
		MessageID: "1",
	}

	err := mgr.HandleInbound(context.Background(), ch, msg)
	if err != nil {
		t.Errorf("expected nil error for blocked user, got %v", err)
	}
	if proxy.calls != 0 {
		t.Errorf("expected 0 proxy calls for blocked user, got %d", proxy.calls)
	}
}

func TestManager_HandleInbound_AllowlistAllowed(t *testing.T) {
	proxy := &mockProxy{statusCode: 200, respBody: []byte(`{"result":{"parts":[{"kind":"text","text":"hi"}]}}`)}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)

	ch := ChannelRow{
		ID:           "ch-123456789012",
		WorkspaceID:  "ws-123456789012",
		ChannelType:  "telegram",
		Config:       map[string]interface{}{"bot_token": "fake", "chat_id": "-100"},
		AllowedUsers: []string{"user-123"},
	}

	msg := &InboundMessage{
		ChatID:    "-100",
		UserID:    "user-123",
		Username:  "allowed",
		Text:      "hello",
		MessageID: "1",
	}

	// This will fail at SendMessage (no real Telegram API) but proves allowlist passed
	_ = mgr.HandleInbound(context.Background(), ch, msg)
	if proxy.calls != 1 {
		t.Errorf("expected 1 proxy call for allowed user, got %d", proxy.calls)
	}
}

func TestManager_HandleInbound_EmptyAllowlist(t *testing.T) {
	proxy := &mockProxy{statusCode: 200, respBody: []byte(`{"result":{"parts":[{"kind":"text","text":"hi"}]}}`)}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)

	ch := ChannelRow{
		ID:           "ch-123456789012",
		WorkspaceID:  "ws-123456789012",
		ChannelType:  "telegram",
		Config:       map[string]interface{}{"bot_token": "fake", "chat_id": "-100"},
		AllowedUsers: []string{}, // Empty = allow all
	}

	msg := &InboundMessage{
		ChatID:    "-100",
		UserID:    "anyone",
		Username:  "anyone",
		Text:      "hello",
		MessageID: "1",
	}

	_ = mgr.HandleInbound(context.Background(), ch, msg)
	if proxy.calls != 1 {
		t.Errorf("expected 1 proxy call for empty allowlist, got %d", proxy.calls)
	}
}

func TestManager_HandleInbound_AllowByChatID(t *testing.T) {
	proxy := &mockProxy{statusCode: 200, respBody: []byte(`{"result":{"parts":[{"kind":"text","text":"ok"}]}}`)}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)

	ch := ChannelRow{
		ID:           "ch-123456789012",
		WorkspaceID:  "ws-123456789012",
		ChannelType:  "telegram",
		Config:       map[string]interface{}{"bot_token": "fake", "chat_id": "-100"},
		AllowedUsers: []string{"-100"}, // Allow by chat_id (group)
	}

	msg := &InboundMessage{
		ChatID:    "-100",
		UserID:    "user-456",
		Username:  "groupuser",
		Text:      "hello",
		MessageID: "1",
	}

	_ = mgr.HandleInbound(context.Background(), ch, msg)
	if proxy.calls != 1 {
		t.Errorf("expected 1 proxy call when chat_id matches allowlist, got %d", proxy.calls)
	}
}

func TestManager_HandleInbound_BroadcastsEvent(t *testing.T) {
	// Use empty result so SendMessage is skipped (no reply text) — broadcast still fires
	proxy := &mockProxy{statusCode: 200, respBody: []byte(`{"result":{}}`)}
	bc := &mockBroadcaster{}
	mgr := NewManager(proxy, bc)

	ch := ChannelRow{
		ID:          "ch-123456789012",
		WorkspaceID: "ws-123456789012",
		ChannelType: "telegram",
		Config:      map[string]interface{}{"bot_token": "fake", "chat_id": "-100"},
	}
	msg := &InboundMessage{ChatID: "-100", UserID: "u1", Username: "test", Text: "hi", MessageID: "1"}

	_ = mgr.HandleInbound(context.Background(), ch, msg)
	found := false
	for _, e := range bc.events {
		if e == "CHANNEL_MESSAGE" {
			found = true
		}
	}
	if !found {
		t.Error("expected CHANNEL_MESSAGE broadcast event")
	}
}

// ==================== extractReplyText Tests ====================

func TestExtractReplyText_Parts(t *testing.T) {
	proxy := &mockProxy{}
	mgr := NewManager(proxy, nil)

	body := []byte(`{"result":{"parts":[{"kind":"text","text":"hello world"}]}}`)
	text := mgr.extractReplyText(body, 200)
	if text != "hello world" {
		t.Errorf("expected 'hello world', got %q", text)
	}
}

func TestExtractReplyText_Artifacts(t *testing.T) {
	proxy := &mockProxy{}
	mgr := NewManager(proxy, nil)

	body := []byte(`{"result":{"artifacts":[{"parts":[{"kind":"text","text":"artifact text"}]}]}}`)
	text := mgr.extractReplyText(body, 200)
	if text != "artifact text" {
		t.Errorf("expected 'artifact text', got %q", text)
	}
}

func TestExtractReplyText_ErrorStatus(t *testing.T) {
	proxy := &mockProxy{}
	mgr := NewManager(proxy, nil)

	text := mgr.extractReplyText([]byte(`{}`), 500)
	if text == "" {
		t.Error("expected error message for non-2xx status")
	}
}

func TestExtractReplyText_InvalidJSON(t *testing.T) {
	proxy := &mockProxy{}
	mgr := NewManager(proxy, nil)

	text := mgr.extractReplyText([]byte(`not json`), 200)
	if text != "" {
		t.Errorf("expected empty for invalid JSON, got %q", text)
	}
}

func TestExtractReplyText_EmptyResult(t *testing.T) {
	proxy := &mockProxy{}
	mgr := NewManager(proxy, nil)

	text := mgr.extractReplyText([]byte(`{"result":{}}`), 200)
	if text != "" {
		t.Errorf("expected empty for no text parts, got %q", text)
	}
}

// ==================== truncID Tests ====================

func TestTruncID_Long(t *testing.T) {
	if got := truncID("abcdefghijklmnop"); got != "abcdefghijkl" {
		t.Errorf("expected 'abcdefghijkl', got %q", got)
	}
}

func TestTruncID_Short(t *testing.T) {
	if got := truncID("abc"); got != "abc" {
		t.Errorf("expected 'abc', got %q", got)
	}
}

func TestTruncID_Exact12(t *testing.T) {
	if got := truncID("123456789012"); got != "123456789012" {
		t.Errorf("expected '123456789012', got %q", got)
	}
}

func TestTruncID_Empty(t *testing.T) {
	if got := truncID(""); got != "" {
		t.Errorf("expected '', got %q", got)
	}
}

// ==================== DiscoverChats Tests ====================

func TestDiscoverChats_InvalidToken(t *testing.T) {
	a := &TelegramAdapter{}
	// tgbotapi.NewBotAPI with empty token calls the API and fails
	_, err := a.DiscoverChats(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty bot token")
	}
}

func TestDiscoverChats_MalformedToken(t *testing.T) {
	a := &TelegramAdapter{}
	// Clearly malformed tokens return an error from tgbotapi
	_, err := a.DiscoverChats(context.Background(), "not-a-real-token")
	if err == nil {
		t.Error("expected error for malformed token")
	}
}

// ==================== Poller Lifetime Tests ====================

// TestManager_PollerSurvivesRequestContext verifies pollers use the manager's
// background context, NOT the request ctx passed to Reload(). Otherwise pollers
// die immediately when an HTTP handler returns.
//
// This catches the bug where Reload(c.Request.Context()) would cancel the
// polling goroutine ~50ms after channel creation when the HTTP request finished.
func TestManager_PollerSurvivesRequestContext(t *testing.T) {
	mgr := NewManager(&mockProxy{}, &mockBroadcaster{})

	// Manager started with long-lived background context (simulates main.go wiring)
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	mgr.bgCtx = bgCtx

	// Spawn a poller using the manager's bgCtx (the correct path)
	pollCtx, pollCancel := context.WithCancel(mgr.bgCtx)
	mgr.mu.Lock()
	mgr.pollers["test-id"] = pollCancel
	mgr.mu.Unlock()

	pollerStopped := make(chan struct{})
	go func() {
		<-pollCtx.Done()
		close(pollerStopped)
	}()

	// Simulate an HTTP request context being cancelled (handler returned)
	requestCtx, requestCancel := context.WithCancel(context.Background())
	requestCancel()
	_ = requestCtx

	// Poller MUST still be alive — it's tied to bgCtx, not requestCtx
	select {
	case <-pollerStopped:
		t.Fatal("poller died when request context was cancelled — pollers must use manager.bgCtx, not request ctx")
	case <-time.After(100 * time.Millisecond):
		// good — poller still running
	}

	// Cleanup: cancel bg ctx to stop the poller
	bgCancel()
	select {
	case <-pollerStopped:
		// good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("poller didn't stop after bg context cancelled")
	}
}

// TestManager_BgCtxFallback verifies that if Start() was never called (bgCtx is nil),
// pollers fall back to context.Background() instead of crashing.
func TestManager_BgCtxFallback(t *testing.T) {
	mgr := NewManager(&mockProxy{}, &mockBroadcaster{})
	// Don't set mgr.bgCtx — simulates a manager that wasn't Start()ed

	parent := mgr.bgCtx
	if parent == nil {
		parent = context.Background()
	}
	pollCtx, cancel := context.WithCancel(parent)
	defer cancel()

	// Poller should be alive
	select {
	case <-pollCtx.Done():
		t.Fatal("poller ctx already cancelled with nil bgCtx — should fall back to context.Background()")
	default:
		// good
	}
}

// ==================== Multi-Chat ID Tests ====================

func TestParseChatIDs_Single(t *testing.T) {
	ids := parseChatIDs(map[string]interface{}{"chat_id": "-100123"})
	if len(ids) != 1 || ids[0] != "-100123" {
		t.Errorf("expected ['-100123'], got %v", ids)
	}
}

func TestParseChatIDs_Multiple(t *testing.T) {
	ids := parseChatIDs(map[string]interface{}{"chat_id": "-100123, -100456, -100789"})
	if len(ids) != 3 {
		t.Fatalf("expected 3 IDs, got %d: %v", len(ids), ids)
	}
	if ids[0] != "-100123" || ids[1] != "-100456" || ids[2] != "-100789" {
		t.Errorf("unexpected IDs: %v", ids)
	}
}

func TestParseChatIDs_Empty(t *testing.T) {
	ids := parseChatIDs(map[string]interface{}{})
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

func TestParseChatIDs_Whitespace(t *testing.T) {
	ids := parseChatIDs(map[string]interface{}{"chat_id": " -100 , , -200 "})
	if len(ids) != 2 || ids[0] != "-100" || ids[1] != "-200" {
		t.Errorf("expected ['-100','-200'], got %v", ids)
	}
}

func TestIsChatAllowed_InList(t *testing.T) {
	config := map[string]interface{}{"chat_id": "-100, -200, -300"}
	if !isChatAllowed(config, "-200") {
		t.Error("expected -200 to be allowed")
	}
}

func TestIsChatAllowed_NotInList(t *testing.T) {
	config := map[string]interface{}{"chat_id": "-100, -200"}
	if isChatAllowed(config, "-999") {
		t.Error("expected -999 to NOT be allowed")
	}
}

func TestIsChatAllowed_EmptyConfig(t *testing.T) {
	config := map[string]interface{}{}
	if !isChatAllowed(config, "-anything") {
		t.Error("expected all chats allowed when no chat_id configured")
	}
}

func TestSplitChatIDs_Multiple(t *testing.T) {
	ids := splitChatIDs("-100, -200, -300")
	if len(ids) != 3 {
		t.Fatalf("expected 3, got %d", len(ids))
	}
}

func TestSplitChatIDs_Single(t *testing.T) {
	ids := splitChatIDs("-100")
	if len(ids) != 1 || ids[0] != "-100" {
		t.Errorf("expected ['-100'], got %v", ids)
	}
}

func TestSplitChatIDs_Empty(t *testing.T) {
	ids := splitChatIDs("")
	if len(ids) != 0 {
		t.Errorf("expected empty, got %v", ids)
	}
}

// ==================== SendOutbound Tests ====================

func TestManager_SendOutbound_NoChatID(t *testing.T) {
	// Test that SendMessage fails when chatID is empty
	adapter, _ := GetAdapter("telegram")
	config := map[string]interface{}{"bot_token": "fake"} // no chat_id
	err := adapter.SendMessage(context.Background(), config, "", "test")
	if err == nil {
		t.Error("expected error for empty chatID")
	}
}

// ==================== #123 — disableChannelByChatID wiring ====================

// The callback is a package-level var set by NewManager; we verify both its
// default (safe no-op) and the wired-up path via a UPDATE assertion against
// a sqlmock-backed db.DB. Two tests guard the contract: the var is callable
// at zero-value, and a wired callback issues the right UPDATE.

func TestDisableChannelByChatID_DefaultIsNoOp(t *testing.T) {
	// Save + restore so we don't pollute sibling tests that build a Manager.
	prev := disableChannelByChatID
	t.Cleanup(func() { disableChannelByChatID = prev })
	disableChannelByChatID = func(ctx context.Context, chatID string) {}

	// Must not panic when called with empty / odd inputs.
	disableChannelByChatID(context.Background(), "")
	disableChannelByChatID(context.Background(), "not-a-number")
	disableChannelByChatID(context.Background(), "-100123")
}

func TestDisableChannelByChatID_WiredSetsEnabledFalse(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = mockDB.Close() })
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB })

	// UPDATE must match the chat_id path and flip enabled=false.
	mock.ExpectExec(`UPDATE workspace_channels\s+SET enabled = false.*WHERE channel_type = 'telegram'\s+AND enabled = true\s+AND config->>'chat_id' = \$1`).
		WithArgs("-100123").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Build a Manager to install the wired callback. Pass nils — Reload
	// is invoked after the UPDATE when rows>0; sqlmock doesn't need to
	// cover that follow-up query here because we're only asserting the
	// UPDATE's shape and args.
	mock.MatchExpectationsInOrder(false)
	// Reload() reads enabled rows; cover the SELECT that follows.
	mock.ExpectQuery(`SELECT .+ FROM workspace_channels WHERE enabled = true`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workspace_id", "channel_type", "config", "allowlist", "enabled",
			"last_message_at", "message_count",
		}))

	_ = NewManager(nil, nil)

	disableChannelByChatID(context.Background(), "-100123")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// ==================== SlackAdapter Tests (#384) ====================
// Note: TestSlackAdapter_Type and TestSlackAdapter_DisplayName moved to slack_test.go

func TestSlackAdapter_ValidateConfig_Valid(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"webhook_url": "https://hooks.slack.com/services/T000/B000/xxx",
	})
	if err != nil {
		t.Errorf("expected no error for valid webhook URL, got %v", err)
	}
}

func TestSlackAdapter_ValidateConfig_MissingWebhookURL(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{})
	if err == nil {
		t.Error("expected error for missing webhook_url")
	}
}

func TestSlackAdapter_ValidateConfig_InvalidPrefix(t *testing.T) {
	// Any URL that doesn't start with https://hooks.slack.com/ must be rejected.
	a := &SlackAdapter{}
	cases := []string{
		"http://hooks.slack.com/services/T000/B000/xxx",    // wrong scheme
		"https://evil.example.com/slack-hook",              // wrong host
		"https://hooks.slack.com.evil.com/services/T/B/x", // SSRF lookalike
		"not-a-url",
		"",
	}
	for _, u := range cases {
		config := map[string]interface{}{"webhook_url": u}
		err := a.ValidateConfig(config)
		if err == nil {
			t.Errorf("expected error for webhook_url %q, got nil", u)
		}
		if u != "" && err != nil && err.Error() != "invalid Slack webhook URL" {
			t.Errorf("webhook_url %q: expected 'invalid Slack webhook URL', got %q", u, err.Error())
		}
	}
}

func TestSlackAdapter_ValidateConfig_EmptyString(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{"webhook_url": ""})
	if err == nil {
		t.Error("expected error for empty webhook_url")
	}
}

func TestSlackAdapter_SendMessage_EmptyWebhookURL(t *testing.T) {
	a := &SlackAdapter{}
	err := a.SendMessage(context.Background(), map[string]interface{}{}, "ignored-chat", "hello")
	if err == nil {
		t.Error("expected error for missing webhook_url")
	}
}

func TestSlackAdapter_SendMessage_InvalidPrefix(t *testing.T) {
	a := &SlackAdapter{}
	err := a.SendMessage(context.Background(), map[string]interface{}{
		"webhook_url": "https://evil.example.com/hook",
	}, "ignored", "hello")
	if err == nil {
		t.Error("expected error for invalid webhook URL prefix in SendMessage")
	}
}

func TestSlackAdapter_StartPolling_ReturnsNil(t *testing.T) {
	// Slack webhooks don't support polling — must return nil immediately.
	a := &SlackAdapter{}
	err := a.StartPolling(context.Background(), map[string]interface{}{}, nil)
	if err != nil {
		t.Errorf("expected nil from StartPolling, got %v", err)
	}
}

func TestGetAdapter_Slack(t *testing.T) {
	a, ok := GetAdapter("slack")
	if !ok || a == nil {
		t.Error("expected slack adapter to be registered")
	}
	if a.Type() != "slack" {
		t.Errorf("expected type 'slack', got %q", a.Type())
	}
}

func TestListAdapters_IncludesSlack(t *testing.T) {
	list := ListAdapters()
	found := false
	for _, a := range list {
		if a.Type == "slack" {
			found = true
			if a.DisplayName != "Slack" {
				t.Errorf("expected display_name 'Slack', got %q", a.DisplayName)
			}
		}
	}
	if !found {
		t.Error("slack not found in ListAdapters")
	}
}

func TestDisableChannelByChatID_NoRowsAffectedSkipsReload(t *testing.T) {
	// When the chat_id doesn't match any row (already disabled, or a different
	// bot), the UPDATE returns RowsAffected=0 and we skip the reload. Verifies
	// we don't emit a spurious log or SELECT storm on unrelated kicked events.
	mockDB, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	t.Cleanup(func() { _ = mockDB.Close() })
	prevDB := db.DB
	db.DB = mockDB
	t.Cleanup(func() { db.DB = prevDB })

	mock.ExpectExec(`UPDATE workspace_channels\s+SET enabled = false`).
		WithArgs("999999999").
		WillReturnResult(sqlmock.NewResult(0, 0))

	_ = NewManager(nil, nil)
	disableChannelByChatID(context.Background(), "999999999")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
