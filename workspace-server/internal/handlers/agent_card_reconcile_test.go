package handlers

import (
	"encoding/json"
	"testing"
)

// TestReconcileAgentCardIdentity covers the server-side backfill that
// repairs the fleet-wide agent-card identity gap (internal#XXX): the
// runtime POSTs /registry/register with agent_card.name = the workspace
// UUID (because the CP-regenerated /configs/config.yaml sets name: <uuid>)
// while the trusted workspaces.name DB column — the value the canvas
// Details tab shows and lets the operator edit — holds the friendly
// name ("Claude Code Agent"). The platform reconciles them from the DB
// row (NOT from the agent — identity stays platform-controlled, not
// self-mutable).
func TestReconcileAgentCardIdentity(t *testing.T) {
	const wsID = "3b81321b-1ec7-488c-96f7-72c42a968da6"

	tests := []struct {
		name        string
		card        string
		dbName      string
		dbRole      string
		wantName    string
		wantDesc    string
		wantRole    string
		wantChanged bool
	}{
		{
			name:        "name is the workspace UUID — backfill from DB",
			card:        `{"name":"3b81321b-1ec7-488c-96f7-72c42a968da6","description":"","capabilities":{"streaming":true}}`,
			dbName:      "Claude Code Agent",
			dbRole:      "",
			wantName:    "Claude Code Agent",
			wantDesc:    "Claude Code Agent",
			wantRole:    "",
			wantChanged: true,
		},
		{
			name:        "empty name — backfill from DB",
			card:        `{"name":"","description":"x"}`,
			dbName:      "ops-agent",
			dbRole:      "sre",
			wantName:    "ops-agent",
			wantDesc:    "x",
			wantRole:    "sre",
			wantChanged: true,
		},
		{
			name:        "role null in card, DB has role — backfill role only",
			card:        `{"name":"Reviewer","description":"Senior reviewer"}`,
			dbName:      "Reviewer",
			dbRole:      "code-reviewer",
			wantName:    "Reviewer",
			wantDesc:    "Senior reviewer",
			wantRole:    "code-reviewer",
			wantChanged: true,
		},
		{
			name: "card already has a real friendly name — do NOT clobber it",
			// A richer card (e.g. an external channel agent) must win;
			// the platform only fills gaps, never downgrades.
			card:        `{"name":"Claude Code (channel)","description":"Local Claude Code session bridged","role":"assistant"}`,
			dbName:      "hongming-pc",
			dbRole:      "operator",
			wantName:    "Claude Code (channel)",
			wantDesc:    "Local Claude Code session bridged",
			wantRole:    "assistant",
			wantChanged: false,
		},
		{
			name:        "no DB name available — leave UUID name untouched (no worse than before)",
			card:        `{"name":"3b81321b-1ec7-488c-96f7-72c42a968da6","description":""}`,
			dbName:      "",
			dbRole:      "",
			wantName:    "3b81321b-1ec7-488c-96f7-72c42a968da6",
			wantDesc:    "",
			wantRole:    "",
			wantChanged: false,
		},
		{
			name:        "dbName equals UUID (placeholder row) — not a friendly name, leave untouched",
			card:        `{"name":"3b81321b-1ec7-488c-96f7-72c42a968da6"}`,
			dbName:      "3b81321b-1ec7-488c-96f7-72c42a968da6",
			dbRole:      "",
			wantName:    "3b81321b-1ec7-488c-96f7-72c42a968da6",
			wantDesc:    "",
			wantRole:    "",
			wantChanged: false,
		},
		{
			name:        "malformed card JSON — return unchanged, no panic",
			card:        `{not json`,
			dbName:      "Claude Code Agent",
			dbRole:      "",
			wantChanged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, changed := reconcileAgentCardIdentity(
				json.RawMessage(tc.card), wsID, tc.dbName, tc.dbRole,
			)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if !tc.wantChanged {
				// Unchanged path must return the input bytes verbatim.
				if string(out) != tc.card {
					t.Fatalf("unchanged path mutated bytes:\n got  %s\n want %s", out, tc.card)
				}
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output not valid JSON: %v (%s)", err, out)
			}
			if g, _ := got["name"].(string); g != tc.wantName {
				t.Errorf("name = %q, want %q", g, tc.wantName)
			}
			if g, _ := got["description"].(string); g != tc.wantDesc {
				t.Errorf("description = %q, want %q", g, tc.wantDesc)
			}
			if tc.wantRole != "" {
				if g, _ := got["role"].(string); g != tc.wantRole {
					t.Errorf("role = %q, want %q", g, tc.wantRole)
				}
			}
		})
	}
}

// TestReconcileAgentCardIdentity_PreservesOtherFields ensures the
// reconcile is a minimal in-place patch — capabilities, version,
// skills and any unknown future fields survive untouched.
func TestReconcileAgentCardIdentity_PreservesOtherFields(t *testing.T) {
	card := `{"name":"ws-uuid","description":"","version":"1.0.0",` +
		`"capabilities":{"streaming":true,"pushNotifications":true},` +
		`"skills":[{"id":"a","name":"a"}],"configuration_status":"ready"}`
	out, changed := reconcileAgentCardIdentity(
		json.RawMessage(card), "ws-uuid", "Friendly Name", "",
	)
	if !changed {
		t.Fatal("expected changed = true")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["version"] != "1.0.0" {
		t.Errorf("version not preserved: %v", got["version"])
	}
	if got["configuration_status"] != "ready" {
		t.Errorf("configuration_status not preserved: %v", got["configuration_status"])
	}
	caps, ok := got["capabilities"].(map[string]any)
	if !ok || caps["streaming"] != true {
		t.Errorf("capabilities not preserved: %v", got["capabilities"])
	}
	skills, ok := got["skills"].([]any)
	if !ok || len(skills) != 1 {
		t.Errorf("skills not preserved: %v", got["skills"])
	}
}

func TestAgentCardURL(t *testing.T) {
	cases := []struct {
		name string
		card string
		want string
	}{
		{"tunnel url", `{"name":"x","url":"https://ws-abc.moleculesai.app"}`, "https://ws-abc.moleculesai.app"},
		{"private ip url", `{"url":"http://ip-172-31-1-1:8000"}`, "http://ip-172-31-1-1:8000"},
		{"trims space", `{"url":"  https://x.example  "}`, "https://x.example"},
		{"no url key", `{"name":"x"}`, ""},
		{"empty url", `{"url":""}`, ""},
		{"malformed", `not json`, ""},
		{"null", `null`, ""},
		{"url not a string", `{"url":123}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCardURL([]byte(tc.card)); got != tc.want {
				t.Errorf("agentCardURL(%s) = %q, want %q", tc.card, got, tc.want)
			}
		})
	}
}
