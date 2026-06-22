package handlers

import (
	"encoding/json"
	"strings"
)

// agentCardURL extracts the advertised "url" from a runtime-supplied agent_card
// blob, or "" if the card is absent/malformed or carries no url. It is the reach
// address an egress-only (Cloudflare-tunnel-fronted) workspace box advertises even
// when it registers with an empty top-level url, so the registration path can
// recover a push-deliverable URL from it. Pure function — unit-tested alongside
// reconcileAgentCardIdentity.
func agentCardURL(card json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(card, &m); err != nil || m == nil {
		return ""
	}
	u, _ := m["url"].(string)
	return strings.TrimSpace(u)
}

// agent_card_reconcile.go — server-side repair for the fleet-wide
// agent-card identity gap.
//
// Root cause: the runtime builds its AgentCard from config.name
// (workspace/main.py:198), and config.name is read from the
// CP-regenerated /configs/config.yaml whose `name:` field is the raw
// workspace UUID — NOT the friendly name the operator sees. The friendly
// name IS captured: POST /workspaces and PATCH /workspaces/:id (the
// canvas Details tab) write it to the trusted workspaces.name DB column.
// But /registry/register stores the runtime-supplied card verbatim
// (registry.go: `agent_card = EXCLUDED.agent_card`), so the stored card
// served at /.well-known/agent-card.json and returned to peers via
// agent_card_url ends up with name = UUID, description = "", role = null.
//
// Fix shape (deliberately minimal, no contract weakening): when the
// runtime-supplied card's `name` is empty or equals the workspace UUID
// (the placeholder the runtime had no better value for), the PLATFORM —
// not the agent — substitutes the friendly value from the trusted
// workspaces row. Identity stays platform-controlled: the agent never
// gains the ability to self-set its own name/role; the platform sources
// it from the operator-controlled DB column. We only ever FILL gaps
// (empty / UUID-placeholder); a card that already carries a real
// friendly name is never downgraded.
//
// list_peers / the /registry/:id/peers endpoint already resolve display
// names from workspaces.name directly (discovery.go / mcp_tools.go
// `SELECT w.id, w.name, ...`), so peer_name in delivered message tags
// was already correct — this fix closes the remaining surface: the
// agent_card blob itself (canvas Agent Card / Skills view, peer
// agent_card_url fetches, the well-known card).
//
// description / role degrade discovery the same way: an empty
// description and null role give peers nothing to reason about. We
// default description from the (now reconciled) name when blank and
// role from workspaces.role when the operator set one.

// reconcileAgentCardIdentity patches identity gaps in a runtime-supplied
// agent card from the trusted workspace DB row. It returns the
// (possibly rewritten) card bytes and whether anything changed. On any
// failure (malformed JSON, nothing to fill) it returns the input bytes
// unchanged with changed=false so the caller can store them verbatim —
// this is strictly no-worse-than-before, never a regression.
//
// Pure function: no DB / HTTP / globals, so it is exhaustively
// unit-testable (agent_card_reconcile_test.go) without booting the
// handler or a sqlmock.
func reconcileAgentCardIdentity(card json.RawMessage, workspaceID, dbName, dbRole string) (json.RawMessage, bool) {
	var m map[string]any
	if err := json.Unmarshal(card, &m); err != nil || m == nil {
		// Malformed card — not this function's job to reject it (the
		// upsert stores it as-is and downstream readers handle bad
		// JSON). Return verbatim so byte-for-byte behaviour is
		// preserved on the failure path.
		return card, false
	}

	changed := false

	// name: fill only when empty or the UUID placeholder. A dbName that
	// is itself the UUID is a placeholder row (registry.go INSERT seeds
	// name = id before the canvas sets a friendly one) — not a friendly
	// name, so it is not an eligible source.
	cardName, _ := m["name"].(string)
	if (cardName == "" || cardName == workspaceID) &&
		dbName != "" && dbName != workspaceID {
		m["name"] = dbName
		changed = true
	}

	// description: when blank, default to the (reconciled) name so peers
	// and the canvas Agent Card view have a non-empty human label
	// instead of "". Mirrors the runtime's own
	// `config.description or config.name` fallback (main.py:199) but
	// applied to the registry copy where the runtime's fallback was the
	// UUID.
	if desc, _ := m["description"].(string); desc == "" {
		if n, _ := m["name"].(string); n != "" && n != workspaceID {
			m["description"] = n
			changed = true
		}
	}

	// role: surface the operator-set workspaces.role when the card
	// carries none. Discovery (peer_role) and the canvas Role row read
	// workspaces.role directly; this just makes the standalone card
	// self-describing too. Never overwrite a role the card already has.
	if dbRole != "" {
		if r, ok := m["role"].(string); !ok || r == "" {
			m["role"] = dbRole
			changed = true
		}
	}

	if !changed {
		// No-op: return the original bytes untouched so callers that
		// compare/store get byte-identical input (re-marshalling would
		// reorder keys for no reason).
		return card, false
	}

	out, err := json.Marshal(m)
	if err != nil {
		// Re-marshal of a map we just unmarshalled should never fail;
		// if it somehow does, fall back to the verbatim input rather
		// than storing nothing.
		return card, false
	}
	return out, true
}
