package handlers

// mergeSystemMessages collapses consecutive leading system messages into a
// single system message before the payload is forwarded to a Hermes/vLLM
// endpoint.
//
// Background
// ----------
// The OpenAI-compatible vLLM server (used by Nous Hermes and similar models)
// accepts only ONE system message.  When the platform constructs a messages
// array from multiple sources — e.g. a base system prompt, a workspace-level
// config block, and a per-session user override — and these are all emitted as
// consecutive {"role":"system","content":"..."} entries, vLLM either rejects
// the request or silently drops all but the first.
//
// This function is a stateless pre-flight transform that resolves the
// collision before any HTTP call is made.
//
// Rules
// -----
//  1. Scan from the front of the slice.
//  2. Collect every consecutive {"role":"system"} entry.
//  3. Join their "content" strings with "\n\n" into one system message.
//  4. Prepend the merged message to the remaining (non-system) messages.
//  5. If there is only one leading system message, the slice is returned
//     unchanged (no allocation, no copy).
//  6. Non-system messages that appear BETWEEN two system messages are NOT
//     considered — the merge only applies to the uninterrupted leading run.
//  7. If there are no system messages at all, the slice is returned as-is.
//
// Content types
// -------------
// "content" may be a string (the common case) or any other JSON-decoded type
// (e.g. []interface{} for multi-modal content arrays).  Only string values
// are merged textually; non-string values are skipped during concatenation.
//
// Example
//
//	In:  [{system,"A"}, {system,"B"}, {user,"Q"}]
//	Out: [{system,"A\n\nB"}, {user,"Q"}]
func mergeSystemMessages(messages []map[string]interface{}) []map[string]interface{} {
	// Find the end of the leading system-message run.
	end := 0
	for end < len(messages) {
		role, _ := messages[end]["role"].(string)
		if role != "system" {
			break
		}
		end++
	}

	// Zero or one system message — nothing to merge.
	if end <= 1 {
		return messages
	}

	// Concatenate content strings from the leading system messages.
	var merged string
	for i := 0; i < end; i++ {
		content, _ := messages[i]["content"].(string)
		if i == 0 {
			merged = content
		} else {
			merged += "\n\n" + content
		}
	}

	// Build result: one merged system message + the remaining messages.
	result := make([]map[string]interface{}, 0, 1+len(messages)-end)
	result = append(result, map[string]interface{}{
		"role":    "system",
		"content": merged,
	})
	result = append(result, messages[end:]...)
	return result
}
