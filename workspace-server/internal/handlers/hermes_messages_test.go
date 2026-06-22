package handlers

import (
	"reflect"
	"testing"
)

// msg is a shorthand constructor for test messages.
func msg(role, content string) map[string]interface{} {
	return map[string]interface{}{"role": role, "content": content}
}

// ============================================================
// mergeSystemMessages — acceptance criteria from issue #499
// ============================================================

// TestMergeSystemMessages_StackedMerged verifies that two consecutive leading
// system messages are collapsed into one, joined by "\n\n".
//
// Acceptance criterion 3:
//
//	input  [{system,"A"}, {system,"B"}, {user,"Q"}]
//	output [{system,"A\n\nB"}, {user,"Q"}]
func TestMergeSystemMessages_StackedMerged(t *testing.T) {
	input := []map[string]interface{}{
		msg("system", "A"),
		msg("system", "B"),
		msg("user", "Q"),
	}
	got := mergeSystemMessages(input)

	want := []map[string]interface{}{
		msg("system", "A\n\nB"),
		msg("user", "Q"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stacked merge: got %v, want %v", got, want)
	}
}

// TestMergeSystemMessages_SingleUnchanged verifies that a single leading system
// message is passed through without modification or reallocation.
//
// Acceptance criterion 4: single system message unchanged.
func TestMergeSystemMessages_SingleUnchanged(t *testing.T) {
	input := []map[string]interface{}{
		msg("system", "only"),
		msg("user", "hello"),
	}
	got := mergeSystemMessages(input)

	// Pointer equality: same underlying slice (no copy made).
	if &got[0] != &input[0] {
		t.Error("single system: expected same slice to be returned, got a copy")
	}
	if len(got) != 2 {
		t.Errorf("single system: got len %d, want 2", len(got))
	}
}

// TestMergeSystemMessages_NoSystem verifies that a messages array with no system
// messages at all is returned unchanged.
//
// Acceptance criterion 5: no system message → messages passed through unchanged.
func TestMergeSystemMessages_NoSystem(t *testing.T) {
	input := []map[string]interface{}{
		msg("user", "hello"),
		msg("assistant", "hi"),
	}
	got := mergeSystemMessages(input)

	if &got[0] != &input[0] {
		t.Error("no system: expected same slice to be returned, got a copy")
	}
	if len(got) != 2 {
		t.Errorf("no system: got len %d, want 2", len(got))
	}
}

// TestMergeSystemMessages_ThreeSystem verifies three consecutive system messages
// are collapsed into one, with "\n\n" between each pair.
func TestMergeSystemMessages_ThreeSystem(t *testing.T) {
	input := []map[string]interface{}{
		msg("system", "base"),
		msg("system", "workspace config"),
		msg("system", "user override"),
		msg("user", "go"),
	}
	got := mergeSystemMessages(input)

	want := []map[string]interface{}{
		msg("system", "base\n\nworkspace config\n\nuser override"),
		msg("user", "go"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("three system: got %v, want %v", got, want)
	}
}

// TestMergeSystemMessages_OnlySystemMessages verifies an array of only system
// messages (no user turn) is collapsed correctly.
func TestMergeSystemMessages_OnlySystemMessages(t *testing.T) {
	input := []map[string]interface{}{
		msg("system", "first"),
		msg("system", "second"),
	}
	got := mergeSystemMessages(input)

	want := []map[string]interface{}{
		msg("system", "first\n\nsecond"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("only system: got %v, want %v", got, want)
	}
}

// TestMergeSystemMessages_InterlevedUserNotMerged verifies that only the leading
// run of system messages is collapsed — a system message that appears AFTER a
// user turn is NOT merged into the leading block.
func TestMergeSystemMessages_InterleavedUserNotMerged(t *testing.T) {
	input := []map[string]interface{}{
		msg("system", "A"),
		msg("system", "B"),
		msg("user", "Q1"),
		msg("system", "C"), // NOT part of leading run
		msg("user", "Q2"),
	}
	got := mergeSystemMessages(input)

	want := []map[string]interface{}{
		msg("system", "A\n\nB"),
		msg("user", "Q1"),
		msg("system", "C"), // untouched
		msg("user", "Q2"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("interleaved: got %v, want %v", got, want)
	}
}

// TestMergeSystemMessages_EmptySlice verifies that an empty input is
// returned as-is without panicking.
func TestMergeSystemMessages_EmptySlice(t *testing.T) {
	input := []map[string]interface{}{}
	got := mergeSystemMessages(input)
	if len(got) != 0 {
		t.Errorf("empty: got len %d, want 0", len(got))
	}
}

// TestMergeSystemMessages_NilSlice verifies that a nil input is handled
// without panicking.
func TestMergeSystemMessages_NilSlice(t *testing.T) {
	var input []map[string]interface{}
	got := mergeSystemMessages(input)
	if len(got) != 0 {
		t.Errorf("nil: got %v, want nil/empty", got)
	}
}

// TestMergeSystemMessages_NonStringContentSkipped verifies that a system message
// whose "content" is not a string (e.g. a []interface{} multi-modal block) is
// treated as an empty string during concatenation so the merge still succeeds
// without panicking.
func TestMergeSystemMessages_NonStringContentSkipped(t *testing.T) {
	input := []map[string]interface{}{
		{"role": "system", "content": "text part"},
		{"role": "system", "content": []interface{}{"block1", "block2"}}, // non-string
		msg("user", "hi"),
	}
	got := mergeSystemMessages(input)

	// Non-string treated as "": "text part\n\n"
	wantContent := "text part\n\n"
	if len(got) != 2 {
		t.Fatalf("non-string content: got len %d, want 2", len(got))
	}
	gotContent, _ := got[0]["content"].(string)
	if gotContent != wantContent {
		t.Errorf("non-string content: got content %q, want %q", gotContent, wantContent)
	}
}

// TestMergeSystemMessages_AssistantLeadingNotMerged verifies that an assistant
// message at the front (unusual but possible) is not treated as a system
// message and the slice is returned as-is.
func TestMergeSystemMessages_AssistantLeadingNotMerged(t *testing.T) {
	input := []map[string]interface{}{
		msg("assistant", "hello"),
		msg("user", "hi"),
	}
	got := mergeSystemMessages(input)
	if &got[0] != &input[0] {
		t.Error("assistant leading: expected same slice to be returned")
	}
}
