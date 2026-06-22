package models

// Contract test: the EXACT request bodies the workspace runtime emits for
// POST /registry/register and POST /registry/heartbeat bind cleanly against
// the real RegisterPayload / HeartbeatPayload structs — and a body missing a
// binding:"required" field is REJECTED.
//
// Why this exists — the same blind-spot class as the #2251 A2A bug
// ----------------------------------------------------------------
// The existing registry_test.go binds HAND-WRITTEN JSON literals
// (`{"id":"ws-123","agent_card":{...}}`) that encode the *test author's*
// idea of the wire shape, not the bytes the runtime actually produces. The
// runtime's producer (molecule-ai-workspace-runtime main.py:484 /
// heartbeat.py:233) is a separate hand-rolled dict. Nothing pinned that the
// two agree on the required keys.
//
// These golden bodies are byte-for-byte the shapes the runtime emits (see the
// companion Python contract test test_registry_payload_contract.py, which
// asserts the runtime PRODUCES exactly these required keys). Together the two
// halves form a producer→consumer contract: if the runtime drops a required
// key, the Python test fails; if this struct adds/renames a required field,
// the Go test below fails — drift can't pass silently on either side.
//
// gin's ShouldBindJSON runs `binding.JSON.BindBody`, which is json.Unmarshal
// followed by the go-playground validator on the `binding` tags. We invoke
// that exact path here without standing up a gin.Context / DB / Redis.

import (
	"testing"

	"github.com/gin-gonic/gin/binding"
)

// bindJSON mirrors gin's ShouldBindJSON: decode + validate the `binding` tags.
func bindJSON(t *testing.T, body []byte, out any) error {
	t.Helper()
	return binding.JSON.BindBody(body, out)
}

// ---- /registry/register --------------------------------------------------

// The exact body main.py emits (workspace_id + workspace_url + the hand-rolled
// agent_card_dict). agent_card is json.RawMessage on the struct so its inner
// shape is opaque to the bind — only presence is required.
const runtimeRegisterBody = `{
  "id": "11111111-1111-1111-1111-111111111111",
  "url": "https://ws.example/a2a",
  "agent_card": {
    "name": "pm",
    "description": "team lead",
    "version": "1.0.0",
    "url": "https://ws.example/a2a",
    "skills": [{"id": "coding", "name": "coding", "description": "coding", "tags": []}],
    "capabilities": {"streaming": true, "pushNotifications": false},
    "configuration_status": "ready"
  },
  "mcp_server_present": false
}`

func TestRegisterPayload_RuntimeBodyBinds(t *testing.T) {
	var p RegisterPayload
	if err := bindJSON(t, []byte(runtimeRegisterBody), &p); err != nil {
		t.Fatalf("runtime register body must bind against RegisterPayload, got: %v", err)
	}
	if p.ID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("id not decoded: %q", p.ID)
	}
	if len(p.AgentCard) == 0 {
		t.Error("agent_card must be present (binding:required)")
	}
	if p.URL == "" {
		t.Error("url should round-trip from the runtime body")
	}
	if p.MCPServerPresent == nil {
		t.Error("mcp_server_present must decode (nil would be fail-closed)")
	}
}

func TestRegisterPayload_MissingID_Rejected(t *testing.T) {
	// The #2251-style regression: runtime drops the required `id` key.
	const noID = `{"url":"https://ws.example/a2a","agent_card":{"name":"pm"}}`
	var p RegisterPayload
	if err := bindJSON(t, []byte(noID), &p); err == nil {
		t.Fatal("a register body missing the required `id` MUST be rejected (would 400); got nil error")
	}
}

func TestRegisterPayload_MissingAgentCard_Rejected(t *testing.T) {
	const noCard = `{"id":"ws-1","url":"https://ws.example/a2a"}`
	var p RegisterPayload
	if err := bindJSON(t, []byte(noCard), &p); err == nil {
		t.Fatal("a register body missing the required `agent_card` MUST be rejected (would 400); got nil error")
	}
}

// ---- /registry/heartbeat -------------------------------------------------

// The exact body heartbeat.py:233 emits (no wedge/metadata, the healthy case).
const runtimeHeartbeatBody = `{
  "workspace_id": "00000000-0000-0000-0000-000000000688",
  "error_rate": 0.0,
  "sample_error": "",
  "active_tasks": 0,
  "current_task": "",
  "uptime_seconds": 42,
  "mcp_server_present": false
}`

func TestHeartbeatPayload_RuntimeBodyBinds(t *testing.T) {
	var p HeartbeatPayload
	if err := bindJSON(t, []byte(runtimeHeartbeatBody), &p); err != nil {
		t.Fatalf("runtime heartbeat body must bind against HeartbeatPayload, got: %v", err)
	}
	if p.WorkspaceID != "00000000-0000-0000-0000-000000000688" {
		t.Errorf("workspace_id not decoded: %q", p.WorkspaceID)
	}
	if p.UptimeSeconds != 42 {
		t.Errorf("uptime_seconds not decoded: %d", p.UptimeSeconds)
	}
	if p.MCPServerPresent == nil {
		t.Error("mcp_server_present must decode (nil would be fail-closed)")
	}
}

// The wedged-runtime heartbeat (heartbeat.py _runtime_state_payload +
// _runtime_metadata_payload layered on) must also bind — runtime_metadata is a
// pointer so a present block decodes, and an absent one stays nil.
const runtimeHeartbeatWedgedBody = `{
  "workspace_id": "00000000-0000-0000-0000-000000000688",
  "error_rate": 0.5,
  "active_tasks": 1,
  "current_task": "stuck",
  "uptime_seconds": 99,
  "runtime_state": "wedged",
  "sample_error": "Control request timeout: initialize",
  "runtime_metadata": {
    "capabilities": {"heartbeat": true, "scheduler": false},
    "idle_timeout_seconds": 600
  }
}`

func TestHeartbeatPayload_WedgedRuntimeBodyBinds(t *testing.T) {
	var p HeartbeatPayload
	if err := bindJSON(t, []byte(runtimeHeartbeatWedgedBody), &p); err != nil {
		t.Fatalf("wedged heartbeat body must bind, got: %v", err)
	}
	if p.RuntimeState != "wedged" {
		t.Errorf("runtime_state not decoded: %q", p.RuntimeState)
	}
	if p.RuntimeMetadata == nil {
		t.Fatal("runtime_metadata must decode to a non-nil pointer when present")
	}
	if got := p.RuntimeMetadata.Capabilities["heartbeat"]; !got {
		t.Error("runtime_metadata.capabilities[heartbeat] should be true")
	}
	if p.RuntimeMetadata.IdleTimeoutSeconds == nil || *p.RuntimeMetadata.IdleTimeoutSeconds != 600 {
		t.Error("runtime_metadata.idle_timeout_seconds should decode to 600")
	}
}

func TestHeartbeatPayload_MissingWorkspaceID_Rejected(t *testing.T) {
	// The drift the producer-side Python test guards: workspace_id renamed/dropped.
	const renamed = `{"id":"ws-688","error_rate":0.0,"active_tasks":0}`
	var p HeartbeatPayload
	if err := bindJSON(t, []byte(renamed), &p); err == nil {
		t.Fatal("a heartbeat body missing the required `workspace_id` MUST be rejected (would 400); got nil error")
	}
}
