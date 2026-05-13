package handlers

import (
	"encoding/json"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// extractA2AText tests
// ─────────────────────────────────────────────────────────────────────────────

func TestExtractA2AText_InvalidJSON(t *testing.T) {
	// When JSON unmarshal fails, fall back to raw body.
	body := []byte("not json at all")
	got := extractA2AText(body)
	if got != "not json at all" {
		t.Errorf("invalid JSON: got %q, want raw body", got)
	}
}

func TestExtractA2AText_A2AError(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"code":    -32600,
			"message": "workspace not found",
		},
	})
	got := extractA2AText(body)
	want := "[error] workspace not found"
	if got != want {
		t.Errorf("A2A error: got %q, want %q", got, want)
	}
}

func TestExtractA2AText_A2AErrorMissingMessage(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"code": -32600,
		},
	})
	got := extractA2AText(body)
	// No message key → falls through to result check, then fallback
	if got == "" {
		t.Errorf("A2A error without message: got empty string")
	}
}

func TestExtractA2AText_ArtifactsText(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{
							"text": "Hello from the artifact",
						},
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	want := "Hello from the artifact"
	if got != want {
		t.Errorf("artifacts text: got %q, want %q", got, want)
	}
}

func TestExtractA2AText_ArtifactsEmptyArray(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{},
		},
	})
	got := extractA2AText(body)
	// Empty artifacts → falls through to message check, then fallback
	if got == "" {
		t.Errorf("empty artifacts: got empty string")
	}
}

func TestExtractA2AText_MessageText(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"message": map[string]interface{}{
				"parts": []interface{}{
					map[string]interface{}{
						"text": "Hello from message",
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	want := "Hello from message"
	if got != want {
		t.Errorf("message text: got %q, want %q", got, want)
	}
}

func TestExtractA2AText_MessageNoParts(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"message": map[string]interface{}{},
		},
	})
	got := extractA2AText(body)
	// No parts → falls through to fallback (JSON marshal of result)
	if got == "" {
		t.Errorf("message with no parts: got empty string")
	}
}

func TestExtractA2AText_EmptyTextInPart(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{
							"text": "",
						},
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	// Empty text → falls through to message check, then fallback
	if got == "" {
		t.Errorf("empty text in part: got empty string")
	}
}

func TestExtractA2AText_NoResult(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"id": 1,
	})
	got := extractA2AText(body)
	// No result key → falls through to fallback
	if got == "" {
		t.Errorf("no result: got empty string")
	}
}

func TestExtractA2AText_FallbackMarshalsResult(t *testing.T) {
	// result is not artifacts or message → fallback to JSON marshal.
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"status": "ok",
			"count":  42,
		},
	})
	got := extractA2AText(body)
	// Fallback: json.Marshal(result) → {"count":42,"status":"ok"}
	if got == "" {
		t.Errorf("fallback marshal: got empty string")
	}
	// Verify it's valid JSON (marshaled result)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Errorf("fallback should produce valid JSON: got %q, error: %v", got, err)
	}
}

func TestExtractA2AText_PriorityArtifactsOverMessage(t *testing.T) {
	// Both artifacts and message present → artifacts takes priority (checked first).
	body, _ := json.Marshal(map[string]interface{}{
		"result": map[string]interface{}{
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{
							"text": "from artifacts",
						},
					},
				},
			},
			"message": map[string]interface{}{
				"parts": []interface{}{
					map[string]interface{}{
						"text": "from message",
					},
				},
			},
		},
	})
	got := extractA2AText(body)
	want := "from artifacts"
	if got != want {
		t.Errorf("artifacts should take priority: got %q, want %q", got, want)
	}
}
