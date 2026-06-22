package channels

import (
	"context"
	"strings"
	"testing"
)

func TestSlackSplitMessage_Short(t *testing.T) {
	chunks := slackSplitMessage("hello", 3000)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Errorf("expected 1 chunk 'hello', got %v", chunks)
	}
}

func TestSlackSplitMessage_Long(t *testing.T) {
	long := strings.Repeat("a", 6000)
	chunks := slackSplitMessage(long, 3000)
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 3000 {
			t.Errorf("chunk exceeds max: %d", len(c))
		}
	}
}

func TestSlackSplitMessage_SplitAtNewline(t *testing.T) {
	text := strings.Repeat("x", 2900) + "\n" + strings.Repeat("y", 200)
	chunks := slackSplitMessage(text, 3000)
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "\n") {
		t.Error("first chunk should end at newline boundary")
	}
}

func TestSlackValidateConfig_BotToken(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"bot_token":  "xoxb-test",
		"channel_id": "C123",
	})
	if err != nil {
		t.Errorf("expected valid, got %v", err)
	}
}

func TestSlackValidateConfig_BotTokenMissingChannel(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"bot_token": "xoxb-test",
	})
	if err == nil {
		t.Error("expected error for missing channel_id")
	}
}

func TestSlackValidateConfig_WebhookURL(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"webhook_url": "https://hooks.slack.com/services/T000/B000/xxx",
	})
	if err != nil {
		t.Errorf("expected valid, got %v", err)
	}
}

func TestSlackValidateConfig_InvalidWebhook(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{
		"webhook_url": "https://evil.com/steal",
	})
	if err == nil {
		t.Error("expected error for invalid webhook URL")
	}
}

func TestSlackValidateConfig_NeitherSet(t *testing.T) {
	a := &SlackAdapter{}
	err := a.ValidateConfig(map[string]interface{}{})
	if err == nil {
		t.Error("expected error when neither bot_token nor webhook_url set")
	}
}

func TestFetchChannelHistory_EmptyToken(t *testing.T) {
	msgs, err := FetchChannelHistory(context.Background(), "", "C123", 10)
	if err != nil || msgs != nil {
		t.Errorf("expected nil,nil for empty token, got %v,%v", msgs, err)
	}
}

func TestFetchChannelHistory_EmptyChannel(t *testing.T) {
	msgs, err := FetchChannelHistory(context.Background(), "xoxb-test", "", 10)
	if err != nil || msgs != nil {
		t.Errorf("expected nil,nil for empty channel, got %v,%v", msgs, err)
	}
}

func TestSlackAdapter_Type(t *testing.T) {
	a := &SlackAdapter{}
	if a.Type() != "slack" {
		t.Errorf("expected 'slack', got %q", a.Type())
	}
}

func TestSlackAdapter_DisplayName(t *testing.T) {
	a := &SlackAdapter{}
	if a.DisplayName() != "Slack" {
		t.Errorf("expected 'Slack', got %q", a.DisplayName())
	}
}

func TestMarkdownToMrkdwn_Bold(t *testing.T) {
	got := markdownToMrkdwn("This is **bold** text")
	if got != "This is *bold* text" {
		t.Errorf("expected *bold*, got %q", got)
	}
}

func TestMarkdownToMrkdwn_Heading(t *testing.T) {
	got := markdownToMrkdwn("### Security Findings")
	if got != "*Security Findings*" {
		t.Errorf("expected *Security Findings*, got %q", got)
	}
}

func TestMarkdownToMrkdwn_Link(t *testing.T) {
	got := markdownToMrkdwn("See [PR #800](https://github.com/org/repo/pull/800)")
	if got != "See <https://github.com/org/repo/pull/800|PR #800>" {
		t.Errorf("expected Slack link, got %q", got)
	}
}

func TestMarkdownToMrkdwn_HorizontalRule(t *testing.T) {
	got := markdownToMrkdwn("above\n---\nbelow")
	if got != "above\n----------\nbelow" {
		t.Errorf("expected dashes, got %q", got)
	}
}

func TestMarkdownToMrkdwn_CodeBlockUntouched(t *testing.T) {
	input := "```go\nfunc main() {}\n```"
	got := markdownToMrkdwn(input)
	if got != input {
		t.Errorf("code block should be untouched, got %q", got)
	}
}

func TestMarkdownToMrkdwn_Mixed(t *testing.T) {
	input := "## Summary\n\n**3 PRs** merged. See [details](https://example.com).\n\n---\n\nDone."
	got := markdownToMrkdwn(input)
	if !strings.Contains(got, "*Summary*") {
		t.Error("heading not converted")
	}
	if !strings.Contains(got, "*3 PRs*") {
		t.Error("bold not converted")
	}
	if !strings.Contains(got, "<https://example.com|details>") {
		t.Error("link not converted")
	}
	if !strings.Contains(got, "----------") {
		t.Error("hr not converted")
	}
}
