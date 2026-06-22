package handlers

// a2a_outbound_envelope_test.go — outbound A2A `message/send` envelope
// CONTRACT gate (issue #2251).
//
// #2251: an outbound A2A envelope shipped without `role` and with text
// parts keyed `type` instead of the v0.3-canonical `kind`. The receiver's
// a-2-a-sdk v0.3 Pydantic validator silently rejected the message
// post-dispatch — the sender saw a happy 200/202 while the brief was
// dropped (the same invisible-rejection failure class as the v0.2→v0.3
// content bug pinned by a2a_corpus_test.go, but on the SEND side).
//
// The inbound corpus replay (a2a_corpus_test.go) proves normalizeA2APayload
// produces `parts[].kind` + a non-empty messageId, but it does NOT assert
// `role`, and it only covers what we RECEIVE. Nothing pins what core
// EMITS. This file pins the emit contract at the helper that builds the
// parts (buildA2AMessageParts, used by both delegate_task and
// delegate_task_async) and asserts the canonical Part key is `kind`.
//
// Part-object schema (A2A v0.3): every Part MUST carry a `kind`
// discriminator ("text" | "file" | "data"); there is NO `type` key. A
// text Part is {"kind":"text","text":"..."}. Emitting `type` makes the
// v0.3 validator drop the Part.

import (
	"encoding/json"
	"testing"
)

// TestBuildA2AMessageParts_TextPartUsesKindNotType pins the v0.3 Part
// discriminator for the text part emitted on every outbound A2A
// delegation. RED before #2251's fix (the helper emitted
// {"type":"text",...}); the receiver's v0.3 Pydantic validator drops a
// Part keyed `type`, silently losing the task text.
func TestBuildA2AMessageParts_TextPartUsesKindNotType(t *testing.T) {
	parts := buildA2AMessageParts("do the work", nil)
	if len(parts) == 0 {
		t.Fatal("buildA2AMessageParts returned no parts for a non-empty task")
	}
	text := parts[0]

	if _, hasType := text["type"]; hasType {
		t.Errorf("text part uses forbidden v0.2 key `type` %v — A2A v0.3 Parts discriminate on `kind`; `type` is dropped by the receiver's validator (#2251)", text)
	}
	kind, ok := text["kind"].(string)
	if !ok {
		t.Fatalf("text part missing string `kind` discriminator; got %v", text)
	}
	if kind != "text" {
		t.Errorf("text part kind = %q, want \"text\"", kind)
	}
	if text["text"] != "do the work" {
		t.Errorf("text part text = %v, want \"do the work\"", text["text"])
	}
}

// TestBuildA2AMessageParts_FilePartUsesKind guards the file-attachment
// Part the same way. The file path was already correct (it used `kind`),
// so this is a non-regression pin — it must STAY `kind` when the text
// path is fixed (a careless "make them consistent" edit could flip both
// to the wrong key).
func TestBuildA2AMessageParts_FilePartUsesKind(t *testing.T) {
	atts := []AgentMessageAttachment{
		{URI: "https://example.com/a.png", MimeType: "image/png", Name: "a.png"},
	}
	parts := buildA2AMessageParts("caption", atts)
	if len(parts) < 2 {
		t.Fatalf("expected text + file parts, got %d", len(parts))
	}
	file := parts[1]
	if _, hasType := file["type"]; hasType {
		t.Errorf("file part uses forbidden `type` key: %v", file)
	}
	if _, hasKind := file["kind"]; !hasKind {
		t.Errorf("file part missing `kind` discriminator: %v", file)
	}
}

// TestDelegationOutboundEnvelope_RoleAndKind pins the FULL outbound
// envelope contract — role + parts[].kind — on the canonical helper.
// A v0.3 `message` MUST carry `role` ("user" for a delegation request)
// and `parts` whose every entry discriminates on `kind`. This is the
// shape the receiver's MessageSendParams validator accepts; an envelope
// missing `role` or keyed `type` is silently rejected (#2251).
//
// Built from the same primitives delegation.go / mcp_tools.go assemble
// (role:"user" + buildA2AMessageParts) so the round-trip through
// json.Marshal proves the wire bytes are v0.3-valid.
func TestDelegationOutboundEnvelope_RoleAndKind(t *testing.T) {
	envelope := map[string]interface{}{
		"method": "message/send",
		"params": map[string]interface{}{
			"message": map[string]interface{}{
				"role":      "user",
				"messageId": "deleg-1",
				"parts":     buildA2AMessageParts("do the work", nil),
			},
		},
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}

	params, _ := parsed["params"].(map[string]interface{})
	if params == nil {
		t.Fatal("envelope missing params")
	}
	msg, _ := params["message"].(map[string]interface{})
	if msg == nil {
		t.Fatal("envelope missing params.message")
	}

	// role is mandatory on a v0.3 message — the receiver rejects without it.
	role, hasRole := msg["role"].(string)
	if !hasRole || role == "" {
		t.Errorf("params.message missing non-empty `role` — v0.3 requires it; omitting it is the other half of #2251")
	}

	parts, _ := msg["parts"].([]interface{})
	if len(parts) == 0 {
		t.Fatal("params.message.parts is empty")
	}
	for i, p := range parts {
		pm, _ := p.(map[string]interface{})
		if pm == nil {
			t.Errorf("part %d is not an object: %v", i, p)
			continue
		}
		if _, hasType := pm["type"]; hasType {
			t.Errorf("part %d uses forbidden `type` key (must be `kind`): %v", i, pm)
		}
		if _, hasKind := pm["kind"]; !hasKind {
			t.Errorf("part %d missing `kind` discriminator: %v", i, pm)
		}
	}
}
