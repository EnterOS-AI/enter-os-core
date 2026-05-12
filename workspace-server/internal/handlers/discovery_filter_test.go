package handlers

import (
	"testing"
)

// filterPeersByQuery tests — nil-safe role/name filtering for peer discovery.

func TestFilterPeersByQuery_EmptyQueryNoOp(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "foo", "role": "bar"},
		{"name": "baz", "role": "qux"},
	}
	result := filterPeersByQuery(peers, "")
	if len(result) != 2 {
		t.Errorf("empty query: expected 2, got %d", len(result))
	}
}

func TestFilterPeersByQuery_WhitespaceQueryNoOp(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "foo", "role": "bar"},
	}
	result := filterPeersByQuery(peers, "   ")
	if len(result) != 1 {
		t.Errorf("whitespace-only query: expected 1, got %d", len(result))
	}
}

func TestFilterPeersByQuery_MatchName(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "backend-agent", "role": "sre"},
		{"name": "frontend-agent", "role": "ui"},
	}
	result := filterPeersByQuery(peers, "backend")
	if len(result) != 1 || result[0]["name"] != "backend-agent" {
		t.Errorf("expected backend-agent, got %v", result)
	}
}

func TestFilterPeersByQuery_MatchRole(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "agent-alpha", "role": "security engineer"},
		{"name": "agent-beta", "role": "devops"},
	}
	result := filterPeersByQuery(peers, "engineer")
	if len(result) != 1 || result[0]["name"] != "agent-alpha" {
		t.Errorf("expected agent-alpha, got %v", result)
	}
}

func TestFilterPeersByQuery_CaseInsensitive(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "AgentX", "role": "SRE"},
	}
	result := filterPeersByQuery(peers, "AGENTx")
	if len(result) != 1 {
		t.Errorf("expected 1 match (case-insensitive), got %d", len(result))
	}
}

func TestFilterPeersByQuery_NilRoleNoPanic(t *testing.T) {
	// This is the regression case for #730: queryPeerMaps explicitly sets
	// peer["role"] = nil when the DB role is empty string. Before the fix,
	// p["role"].(string) panics on nil. After the fix, it returns "" and
	// no match occurs — which is the correct behaviour.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("filterPeersByQuery panicked on nil role: %v", r)
		}
	}()
	peers := []map[string]interface{}{
		{"name": "some-agent", "role": nil},
	}
	result := filterPeersByQuery(peers, "some-agent")
	if len(result) != 1 {
		t.Errorf("expected 1 match by name, got %d", len(result))
	}
}

func TestFilterPeersByQuery_NilRoleQueryNoMatch(t *testing.T) {
	// When role is nil and query does not match name, nothing matches.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("filterPeersByQuery panicked on nil role: %v", r)
		}
	}()
	peers := []map[string]interface{}{
		{"name": "agent-alpha", "role": nil},
	}
	result := filterPeersByQuery(peers, "no-match")
	if len(result) != 0 {
		t.Errorf("expected 0 matches, got %d", len(result))
	}
}

func TestFilterPeersByQuery_NilNameNoPanic(t *testing.T) {
	// Defensive check: name could also theoretically be nil.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("filterPeersByQuery panicked on nil name: %v", r)
		}
	}()
	peers := []map[string]interface{}{
		{"name": nil, "role": "sre"},
	}
	result := filterPeersByQuery(peers, "sre")
	if len(result) != 1 {
		t.Errorf("expected 1 match by role, got %d", len(result))
	}
}

func TestFilterPeersByQuery_BothNilNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("filterPeersByQuery panicked on nil name+role: %v", r)
		}
	}()
	peers := []map[string]interface{}{
		{"name": nil, "role": nil},
	}
	result := filterPeersByQuery(peers, "")
	if len(result) != 1 {
		t.Errorf("empty query with nil name/role: expected 1, got %d", len(result))
	}
	result = filterPeersByQuery(peers, "anything")
	if len(result) != 0 {
		t.Errorf("non-empty query with nil name/role: expected 0, got %d", len(result))
	}
}

func TestFilterPeersByQuery_NoMatches(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "alpha", "role": "beta"},
		{"name": "gamma", "role": "delta"},
	}
	result := filterPeersByQuery(peers, "zzz")
	if len(result) != 0 {
		t.Errorf("expected 0, got %d", len(result))
	}
}

func TestFilterPeersByQuery_EmptyPeers(t *testing.T) {
	result := filterPeersByQuery([]map[string]interface{}{}, "query")
	if len(result) != 0 {
		t.Errorf("empty peers: expected 0, got %d", len(result))
	}
}

func TestFilterPeersByQuery_MultipleMatches(t *testing.T) {
	peers := []map[string]interface{}{
		{"name": "backend-alpha", "role": "eng"},
		{"name": "backend-beta", "role": "eng"},
		{"name": "frontend", "role": "ui"},
	}
	result := filterPeersByQuery(peers, "backend")
	if len(result) != 2 {
		t.Errorf("expected 2 backend matches, got %d", len(result))
	}
}
