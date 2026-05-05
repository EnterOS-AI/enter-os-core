package provlog

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

// captureLog redirects the default logger to a buffer for the duration
// of fn and returns whatever was written.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevWriter := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0) // strip date/time so assertions stay deterministic
	t.Cleanup(func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

func TestEvent_EmitsEvtPrefixAndJSONPayload(t *testing.T) {
	out := captureLog(t, func() {
		Event("provision.start", map[string]any{
			"workspace_id": "ws-123",
			"tier":         4,
			"runtime":      "claude-code",
		})
	})
	out = strings.TrimSpace(out)
	if !strings.HasPrefix(out, "evt: provision.start ") {
		t.Fatalf("expected evt-prefixed line, got %q", out)
	}
	jsonPart := strings.TrimPrefix(out, "evt: provision.start ")
	var got map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &got); err != nil {
		t.Fatalf("payload not valid JSON: %v (raw=%q)", err, jsonPart)
	}
	if got["workspace_id"] != "ws-123" {
		t.Errorf("workspace_id field lost: %+v", got)
	}
	// JSON unmarshal turns numbers into float64 — exact-equal compare.
	if got["tier"].(float64) != 4 {
		t.Errorf("tier field lost: %+v", got)
	}
	if got["runtime"] != "claude-code" {
		t.Errorf("runtime field lost: %+v", got)
	}
}

func TestEvent_NilFieldsEmitsEmptyObject(t *testing.T) {
	out := captureLog(t, func() {
		Event("restart.pre_stop", nil)
	})
	if !strings.Contains(out, "evt: restart.pre_stop {}") {
		t.Fatalf("nil fields should emit empty object, got %q", out)
	}
}

func TestEvent_PreservesEventBoundaryOnUnmarshalableValue(t *testing.T) {
	// A channel cannot be marshaled by encoding/json — verify we still
	// emit the event boundary with a recorded marshal error. This is
	// the structural guarantee: the call site never sees a panic, and
	// the event name is always present in the log.
	out := captureLog(t, func() {
		Event("provision.ec2_started", map[string]any{
			"chan": make(chan int),
		})
	})
	if !strings.Contains(out, "evt: provision.ec2_started ") {
		t.Fatalf("event boundary missing on marshal error: %q", out)
	}
	if !strings.Contains(out, "_marshal_err") {
		t.Fatalf("expected _marshal_err sentinel, got %q", out)
	}
}

func TestEvent_SingleLineOutput(t *testing.T) {
	// Log aggregators line-split on \n. A multi-line emit would silently
	// fragment the JSON across two records — pin single-line shape.
	out := captureLog(t, func() {
		Event("provision.skip_existing", map[string]any{
			"existing_id": "ws-abc",
			"name":        "child-1",
		})
	})
	trimmed := strings.TrimRight(out, "\n")
	if strings.Contains(trimmed, "\n") {
		t.Fatalf("event line must be single-line, got %q", out)
	}
}
