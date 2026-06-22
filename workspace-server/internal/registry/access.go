package registry

import (
	"database/sql"
	"log"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
)

// maxAncestorWalk caps the depth of the parent-chain walk in
// CanCommunicate. Org trees are realistically 3-5 deep
// (PM → Dev Lead → Backend Engineer is depth 3); 32 is a safety
// ceiling so a malformed cycle in the workspaces table can't loop
// forever.
const maxAncestorWalk = 32

type workspaceRef struct {
	ID       string
	ParentID *string
}

func getWorkspaceRef(id string) (*workspaceRef, error) {
	var ws workspaceRef
	var parentID sql.NullString
	err := db.DB.QueryRow(`SELECT id, parent_id FROM workspaces WHERE id = $1`, id).
		Scan(&ws.ID, &parentID)
	if err != nil {
		return nil, err
	}
	if parentID.Valid {
		ws.ParentID = &parentID.String
	}
	return &ws, nil
}

// isAncestorOf returns true if `ancestorID` is found anywhere on the
// parent-chain walk starting from `childID`. Walks at most maxAncestorWalk
// steps so a corrupt parent-cycle cannot loop forever. Returns false on any
// DB lookup error (logged) — fail-secure.
func isAncestorOf(ancestorID, childID string) bool {
	current := childID
	for i := 0; i < maxAncestorWalk; i++ {
		ref, err := getWorkspaceRef(current)
		if err != nil {
			log.Printf("isAncestorOf: walk lookup %s: %v", current, err)
			return false
		}
		if ref.ParentID == nil {
			return false
		}
		if *ref.ParentID == ancestorID {
			return true
		}
		current = *ref.ParentID
	}
	log.Printf("isAncestorOf: walk exceeded maxAncestorWalk=%d from %s — corrupt parent chain?",
		maxAncestorWalk, childID)
	return false
}

// CanCommunicate checks if two workspaces can talk to each other based on
// the org hierarchy. The rules:
//
//   - self → self
//   - siblings (same parent, including both root-level)
//   - any ancestor → any descendant (e.g. PM → Backend Engineer)
//   - any descendant → any ancestor (e.g. Security Auditor → PM)
//
// The third and fourth rules generalise the previous "direct parent ↔
// child" check. Originally this was strict 1-step parent/child only,
// which broke the audit-routing contract: Security Auditor (under Dev
// Lead, under PM) could not call delegate_task on PM to deliver an
// audit_summary, so it fell back to delegating to Dev Lead — bypassing
// PM's category_routing entirely.
//
// The relaxation preserves the hierarchy intent (no horizontal cross-team
// chatter — Frontend Engineer cannot directly message Backend Engineer
// unless they share a parent, which they do under Dev Lead) while
// unblocking the leadership-chain pattern that is fundamental to how
// audit summaries fan out across the org.
func CanCommunicate(callerID, targetID string) bool {
	if callerID == targetID {
		return true
	}

	caller, err := getWorkspaceRef(callerID)
	if err != nil {
		log.Printf("CanCommunicate: lookup caller %s: %v", callerID, err)
		return false
	}
	target, err := getWorkspaceRef(targetID)
	if err != nil {
		log.Printf("CanCommunicate: lookup target %s: %v", targetID, err)
		return false
	}

	// Siblings — same parent (including root-level where both have no parent)
	if caller.ParentID != nil && target.ParentID != nil &&
		*caller.ParentID == *target.ParentID {
		return true
	}
	// Direct parent → child (fast path; avoids the ancestor walk)
	if target.ParentID != nil && caller.ID == *target.ParentID {
		return true
	}

	// Direct child → parent (fast path)
	if caller.ParentID != nil && target.ID == *caller.ParentID {
		return true
	}

	// Distant ancestor → descendant: caller is somewhere up target's chain.
	// Triggers extra DB lookups, only reached when the fast paths above didn't match.
	if target.ParentID != nil && isAncestorOf(callerID, *target.ParentID) {
		return true
	}

	// Distant descendant → ancestor: target is somewhere up caller's chain.
	// (e.g. Security Auditor → PM, where Security Auditor's parent is Dev Lead.)
	if caller.ParentID != nil && isAncestorOf(targetID, *caller.ParentID) {
		return true
	}

	return false
}
