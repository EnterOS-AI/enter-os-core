package handlers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// extractResponseText tests — walks A2A JSON-RPC response bodies and
// returns the first text part, falling back to raw body on parse failures.

func TestExtractResponseText_PartsWithTextKind(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{
				map[string]interface{}{"kind": "text", "text": "hello world"},
				map[string]interface{}{"kind": "text", "text": "second part"},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "hello world", extractResponseText(body))
}

func TestExtractResponseText_PartNotTextKind(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{
				map[string]interface{}{"kind": "image", "data": "base64..."},
				map[string]interface{}{"kind": "text", "text": "visible"},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "visible", extractResponseText(body))
}

func TestExtractResponseText_PartsEmpty(t *testing.T) {
	// Empty parts array — falls through to artifacts, then raw body
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts":     []interface{}{},
			"artifacts": []interface{}{},
		},
	}
	body, _ := json.Marshal(resp)
	// Falls through to raw body (which is the JSON string)
	result := extractResponseText(body)
	assert.NotEmpty(t, result)
}

func TestExtractResponseText_ArtifactPartsWithText(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{},
			"artifacts": []interface{}{
				map[string]interface{}{
					"kind": "file",
					"parts": []interface{}{
						map[string]interface{}{"kind": "text", "text": "artifact text"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "artifact text", extractResponseText(body))
}

func TestExtractResponseText_ArtifactPartNotTextKind(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{},
			"artifacts": []interface{}{
				map[string]interface{}{
					"kind": "code",
					"parts": []interface{}{
						map[string]interface{}{"kind": "image", "data": "..."},
						map[string]interface{}{"kind": "text", "text": "code comment"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "code comment", extractResponseText(body))
}

func TestExtractResponseText_ArtifactsEmpty(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts":     []interface{}{},
			"artifacts": []interface{}{},
		},
	}
	body, _ := json.Marshal(resp)
	result := extractResponseText(body)
	// Falls back to raw body
	assert.Equal(t, string(body), result)
}

func TestExtractResponseText_NoResult(t *testing.T) {
	// No "result" key at all — falls back to raw body
	body := []byte(`{"error": {"code": -32600, "message": "Invalid Request"}}`)
	result := extractResponseText(body)
	assert.Equal(t, string(body), result)
}

func TestExtractResponseText_ResultNotMap(t *testing.T) {
	// result is a string, not a map — falls back to raw body
	body := []byte(`{"result": "just a string"}`)
	result := extractResponseText(body)
	assert.Equal(t, string(body), result)
}

func TestExtractResponseText_NonJSONBody(t *testing.T) {
	// Non-JSON bytes — returns the raw string
	body := []byte("plain text response, not JSON at all")
	result := extractResponseText(body)
	assert.Equal(t, "plain text response, not JSON at all", result)
}

func TestExtractResponseText_PartWithNilText(t *testing.T) {
	// Text field is nil — kind is "text" but text is nil, should skip
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{
				map[string]interface{}{"kind": "text", "text": nil},
				map[string]interface{}{"kind": "text", "text": "found"},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "found", extractResponseText(body))
}

func TestExtractResponseText_ArtifactPartWithNilText(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{},
			"artifacts": []interface{}{
				map[string]interface{}{
					"parts": []interface{}{
						map[string]interface{}{"kind": "text", "text": nil},
						map[string]interface{}{"kind": "text", "text": "artifact-found"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "artifact-found", extractResponseText(body))
}

func TestExtractResponseText_PartsWithNonMapElement(t *testing.T) {
	// parts contains a non-map element — should be skipped gracefully
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{
				"not a map",
				123,
				nil,
				map[string]interface{}{"kind": "text", "text": "parsed"},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "parsed", extractResponseText(body))
}

func TestExtractResponseText_ArtifactWithNonMapElement(t *testing.T) {
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{},
			"artifacts": []interface{}{
				"not a map",
				nil,
				map[string]interface{}{
					"parts": []interface{}{
						"not a map",
						map[string]interface{}{"kind": "text", "text": "safe"},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "safe", extractResponseText(body))
}

func TestExtractResponseText_PartKindNotString(t *testing.T) {
	// kind is an integer, not a string — should be skipped
	resp := map[string]interface{}{
		"result": map[string]interface{}{
			"parts": []interface{}{
				map[string]interface{}{"kind": 123, "text": "ignored"},
				map[string]interface{}{"kind": "text", "text": "found"},
			},
		},
	}
	body, _ := json.Marshal(resp)
	assert.Equal(t, "found", extractResponseText(body))
}

func TestExtractResponseText_EmptyResponse(t *testing.T) {
	body := []byte("{}")
	result := extractResponseText(body)
	// Falls back to raw "{}"
	assert.Equal(t, "{}", result)
}

func TestExtractResponseText_NilBody(t *testing.T) {
	// nil byte slice — string(nil) = ""
	result := extractResponseText(nil)
	assert.Equal(t, "", result)
}

func TestExtractResponseText_WhitespaceBody(t *testing.T) {
	body := []byte("   \n\t  ")
	result := extractResponseText(body)
	// Unmarshals to empty map, no result, returns raw string
	assert.Equal(t, "   \n\t  ", result)
}
