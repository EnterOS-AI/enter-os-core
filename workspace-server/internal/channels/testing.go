package channels

import "context"

// MockSendAdapter implements SendAdapter for handler tests. It records every
// call and returns a configurable error (nil = success, non-nil = failure).
type MockSendAdapter struct {
	Calls    int
	Err      error
	SentText string
	SentChat string
}

func (m *MockSendAdapter) SendMessage(_ context.Context, _ map[string]interface{}, chatID string, text string) error {
	m.Calls++
	m.SentText = text
	m.SentChat = chatID
	return m.Err
}

// SetGetSendAdapter replaces the package-level GetSendAdapter variable.
// Tests MUST call ResetSendAdapters() in their t.Cleanup.
func SetGetSendAdapter(fn func(string) (SendAdapter, bool)) {
	GetSendAdapter = fn
}

// ResetSendAdapters restores GetSendAdapter to the production implementation.
func ResetSendAdapters() {
	GetSendAdapter = getSendAdapter
}
