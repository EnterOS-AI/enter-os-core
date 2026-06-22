package handlers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{45 * time.Second, "45s"},
		{3 * time.Minute, "3m0s"},
		{2*time.Hour + 14*time.Minute, "2h14m"},
		{25 * time.Hour, "25h0m"},
	}
	for _, c := range cases {
		got := humanDuration(c.in)
		if got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildRestartContextMessage_NoPriorSession(t *testing.T) {
	d := restartContextData{
		RestartAt:     time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC),
		PrevSessionAt: time.Time{},
		EnvKeys:       nil,
	}
	msg := buildRestartContextMessage(d)

	mustContain(t, msg, "=== WORKSPACE RESTART CONTEXT ===")
	mustContain(t, msg, "Restart at: 2026-04-13T12:00:00Z")
	mustContain(t, msg, "Previous session ended: (no prior session on record)")
	mustContain(t, msg, "Env vars now available: (none)")
	mustContain(t, msg, "=== END RESTART CONTEXT ===")
}

func TestBuildRestartContextMessage_WithDataRendersKeysAndDelta(t *testing.T) {
	prev := time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC)
	d := restartContextData{
		RestartAt:     prev.Add(2*time.Hour + 14*time.Minute),
		PrevSessionAt: prev,
		EnvKeys:       []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
	}
	msg := buildRestartContextMessage(d)

	mustContain(t, msg, "Previous session ended: 2026-04-13T10:00:00Z (2h14m ago)")
	mustContain(t, msg, "Env vars now available: ANTHROPIC_API_KEY, OPENAI_API_KEY")
	if strings.Contains(msg, "sk-") || strings.Contains(msg, "secret-value") {
		t.Errorf("rendered message leaked secret-shaped content: %q", msg)
	}
}

func TestBuildRestartA2APayload_ShapeIsJSONRPCMessageSend(t *testing.T) {
	body, err := buildRestartA2APayload("hello world")
	if err != nil {
		t.Fatalf("buildRestartA2APayload: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	if parsed["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v, want 2.0", parsed["jsonrpc"])
	}
	if parsed["method"] != "message/send" {
		t.Errorf("method = %v, want message/send", parsed["method"])
	}

	params, ok := parsed["params"].(map[string]any)
	if !ok {
		t.Fatalf("params missing or wrong type: %v", parsed["params"])
	}
	message, ok := params["message"].(map[string]any)
	if !ok {
		t.Fatalf("params.message missing: %v", params)
	}
	if message["role"] != "user" {
		t.Errorf("role = %v, want user", message["role"])
	}
	if message["messageId"] == nil || message["messageId"] == "" {
		t.Errorf("messageId missing")
	}

	meta, ok := message["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing: %v", message)
	}
	if meta["kind"] != "restart_context" {
		t.Errorf("metadata.kind = %v, want restart_context", meta["kind"])
	}
	if meta["source"] != "platform" {
		t.Errorf("metadata.source = %v, want platform", meta["source"])
	}

	parts, ok := message["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("parts wrong shape: %v", message["parts"])
	}
	firstPart, _ := parts[0].(map[string]any)
	if firstPart["text"] != "hello world" {
		t.Errorf("parts[0].text = %v, want hello world", firstPart["text"])
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}
