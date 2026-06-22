package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// ==================== DiscordAdapter unit tests ====================

// fatalClient is a deterministic httpClient stub that always returns a
// fixed error. Used to test that error messages from SendMessage do not
// contain the Discord webhook token.
type fatalClient struct {
	err error
}

func (c *fatalClient) Do(*http.Request) (*http.Response, error) {
	return nil, c.err
}

func TestDiscordAdapter_Type(t *testing.T) {
	a := &DiscordAdapter{}
	if a.Type() != "discord" {
		t.Errorf("expected 'discord', got %q", a.Type())
	}
}

func TestDiscordAdapter_DisplayName(t *testing.T) {
	a := &DiscordAdapter{}
	if a.DisplayName() != "Discord" {
		t.Errorf("expected 'Discord', got %q", a.DisplayName())
	}
}

func TestDiscordAdapter_ValidateConfig_Valid(t *testing.T) {
	a := &DiscordAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"webhook_url": "https://discord.com/api/webhooks/1234567890/abcdefghijk",
	})
	if err != nil {
		t.Errorf("expected no error for valid webhook URL, got %v", err)
	}
}

func TestDiscordAdapter_ValidateConfig_MissingWebhookURL(t *testing.T) {
	a := &DiscordAdapter{}
	err := a.ValidateConfig(map[string]interface{}{})
	if err == nil {
		t.Error("expected error for missing webhook_url")
	}
}

func TestDiscordAdapter_ValidateConfig_EmptyWebhookURL(t *testing.T) {
	a := &DiscordAdapter{}
	err := a.ValidateConfig(map[string]interface{}{"webhook_url": ""})
	if err == nil {
		t.Error("expected error for empty webhook_url")
	}
}

func TestDiscordAdapter_ValidateConfig_InvalidPrefix(t *testing.T) {
	a := &DiscordAdapter{}
	cases := []string{
		"http://discord.com/api/webhooks/1/abc",            // wrong scheme
		"https://evil.example.com/discord-hook",           // wrong host
		"https://discord.com.evil.com/api/webhooks/1/abc", // SSRF lookalike
		"not-a-url",
		"",
	}
	for _, u := range cases {
		config := map[string]interface{}{"webhook_url": u}
		err := a.ValidateConfig(config)
		if err == nil {
			t.Errorf("expected error for webhook_url %q, got nil", u)
		}
	}
}

func TestDiscordAdapter_SendMessage_EmptyWebhookURL(t *testing.T) {
	a := &DiscordAdapter{}
	err := a.SendMessage(context.Background(), map[string]interface{}{}, "ignored-chat", "hello")
	if err == nil {
		t.Error("expected error for missing webhook_url")
	}
}

func TestDiscordAdapter_SendMessage_InvalidPrefix(t *testing.T) {
	a := &DiscordAdapter{}
	err := a.SendMessage(context.Background(), map[string]interface{}{
		"webhook_url": "https://evil.example.com/hook",
	}, "ignored", "hello")
	if err == nil {
		t.Error("expected error for invalid webhook URL prefix in SendMessage")
	}
}

func TestDiscordAdapter_ParseWebhook_Ping(t *testing.T) {
	a := &DiscordAdapter{}
	body := `{"type":1,"id":"ping-id"}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))

	msg, err := a.ParseWebhook(c, nil)
	if err != nil {
		t.Errorf("expected no error for PING, got %v", err)
	}
	if msg != nil {
		t.Errorf("expected nil message for PING (type 1), got %+v", msg)
	}
}

func TestDiscordAdapter_ParseWebhook_SlashCommand(t *testing.T) {
	a := &DiscordAdapter{}
	payload := map[string]interface{}{
		"type":       2,
		"id":         "interaction-id",
		"channel_id": "chan-123",
		"token":      "interaction-token",
		"member": map[string]interface{}{
			"user": map[string]interface{}{
				"id":       "user-456",
				"username": "testuser",
			},
		},
		"data": map[string]interface{}{
			"name": "ask",
			"options": []interface{}{
				map[string]interface{}{"name": "query", "value": "what is the status?"},
			},
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(bodyBytes)))

	msg, err := a.ParseWebhook(c, nil)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message for slash command")
	}
	if msg.UserID != "user-456" {
		t.Errorf("expected UserID 'user-456', got %q", msg.UserID)
	}
	if msg.Username != "testuser" {
		t.Errorf("expected Username 'testuser', got %q", msg.Username)
	}
	if msg.ChatID != "chan-123" {
		t.Errorf("expected ChatID 'chan-123', got %q", msg.ChatID)
	}
	if !strings.Contains(msg.Text, "/ask") {
		t.Errorf("expected text to contain '/ask', got %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "what is the status?") {
		t.Errorf("expected text to contain option value, got %q", msg.Text)
	}
	if msg.Metadata["platform"] != "discord" {
		t.Errorf("expected platform metadata 'discord', got %q", msg.Metadata["platform"])
	}
}

func TestDiscordAdapter_ParseWebhook_SlashCommand_DMUser(t *testing.T) {
	// In DMs, "user" field is set instead of "member.user".
	a := &DiscordAdapter{}
	payload := map[string]interface{}{
		"type":       2,
		"id":         "dm-interaction-id",
		"channel_id": "dm-chan",
		"token":      "dm-token",
		"user": map[string]interface{}{
			"id":       "dm-user-789",
			"username": "dmuser",
		},
		"data": map[string]interface{}{
			"name":    "help",
			"options": []interface{}{},
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(bodyBytes)))

	msg, err := a.ParseWebhook(c, nil)
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if msg == nil {
		t.Fatal("expected non-nil message for DM slash command")
	}
	if msg.UserID != "dm-user-789" {
		t.Errorf("expected UserID 'dm-user-789', got %q", msg.UserID)
	}
	if msg.Username != "dmuser" {
		t.Errorf("expected Username 'dmuser', got %q", msg.Username)
	}
}

func TestDiscordAdapter_ParseWebhook_UnknownType(t *testing.T) {
	a := &DiscordAdapter{}
	body := `{"type":99}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))

	msg, err := a.ParseWebhook(c, nil)
	if err != nil {
		t.Errorf("expected no error for unknown type, got %v", err)
	}
	if msg != nil {
		t.Errorf("expected nil message for unknown type, got %+v", msg)
	}
}

func TestDiscordAdapter_ParseWebhook_InvalidJSON(t *testing.T) {
	a := &DiscordAdapter{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("{bad json"))

	_, err := a.ParseWebhook(c, nil)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestDiscordAdapter_StartPolling_ReturnsNil(t *testing.T) {
	a := &DiscordAdapter{}
	err := a.StartPolling(context.Background(), map[string]interface{}{}, nil)
	if err != nil {
		t.Errorf("expected nil from StartPolling, got %v", err)
	}
}

func TestGetAdapter_Discord(t *testing.T) {
	a, ok := GetAdapter("discord")
	if !ok || a == nil {
		t.Error("expected discord adapter to be registered")
	}
	if a.Type() != "discord" {
		t.Errorf("expected type 'discord', got %q", a.Type())
	}
}

func TestListAdapters_IncludesDiscord(t *testing.T) {
	list := ListAdapters()
	found := false
	for _, a := range list {
		if a.Type == "discord" {
			found = true
			if a.DisplayName != "Discord" {
				t.Errorf("expected display_name 'Discord', got %q", a.DisplayName)
			}
		}
	}
	if !found {
		t.Error("discord not found in ListAdapters")
	}
}

// ==================== splitMessage helper tests ====================

func TestSplitMessage_Short(t *testing.T) {
	chunks := splitMessage("hello world", 2000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk for short message, got %d", len(chunks))
	}
	if chunks[0] != "hello world" {
		t.Errorf("expected 'hello world', got %q", chunks[0])
	}
}

func TestSplitMessage_ExactlyMaxLen(t *testing.T) {
	text := strings.Repeat("a", 2000)
	chunks := splitMessage(text, 2000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}
}

func TestSplitMessage_LongMessage(t *testing.T) {
	// Build a 4100-character message — should split into at least 2 chunks.
	text := strings.Repeat("x", 4100)
	chunks := splitMessage(text, 2000)
	if len(chunks) < 2 {
		t.Errorf("expected at least 2 chunks for 4100-char message, got %d", len(chunks))
	}
	// Reassembled content must equal original.
	reassembled := strings.Join(chunks, "")
	if reassembled != text {
		t.Error("reassembled chunks do not match original text")
	}
}

// TestDiscordAdapter_SendMessage_ErrorDoesNotLeakToken verifies that when the
// HTTP call to the Discord webhook fails (network error), the returned error
// message does NOT contain the webhook URL — which embeds the Discord token.
// Regression test for the MEDIUM security finding in PR #659.
//
// This test uses a deterministic httptest.Server (connection refused) rather
// than a live network call, so it always exercises the error path regardless
// of environment routing.
func TestDiscordAdapter_SendMessage_ErrorDoesNotLeakToken(t *testing.T) {
	fakeToken := "SUPER_SECRET_DISCORD_TOKEN_12345"
	webhookURL := discordWebhookPrefix + "123456789/" + fakeToken

	// httptest.Server with no handler → connection refused / immediate close.
	// Deterministic in all environments; no skip condition.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server handler called — should have been unreachable")
	}))
	defer ts.Close()

	// Point the webhook URL at the test server so DiscordAdapter sends there.
	// We intercept the *request* (not the URL) by swapping the client's base URL.
	// The adapter always resolves webhookURL from config, so we set up a
	// test server that refuses connections on the real discord.com domain
	// by having the adapter's HTTP client hit an unreachable host.
	//
	// Simpler: construct a URL with the fake token that won't route anywhere,
	// but use a mock httpClient to control the error exactly.
	a := &DiscordAdapter{
		client: &fatalClient{err: fmt.Errorf("connection refused")},
	}

	err := a.SendMessage(
		context.Background(),
		map[string]interface{}{"webhook_url": webhookURL},
		"ignored",
		"hello",
	)

	if err == nil {
		t.Fatal("expected error from fatalClient")
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Errorf("error message leaks Discord webhook token: %q", err.Error())
	}
	if strings.Contains(err.Error(), "123456789") {
		t.Errorf("error message leaks webhook ID: %q", err.Error())
	}
}

func TestSplitMessage_SplitsAtNewline(t *testing.T) {
	// Build a message where a newline falls within the split window.
	line1 := strings.Repeat("a", 1500) + "\n"
	line2 := strings.Repeat("b", 1500)
	text := line1 + line2
	chunks := splitMessage(text, 2000)
	if len(chunks) < 2 {
		t.Errorf("expected at least 2 chunks, got %d", len(chunks))
	}
	// Reassembled content must equal original.
	reassembled := strings.Join(chunks, "")
	if reassembled != text {
		t.Error("reassembled chunks do not match original text")
	}
}
