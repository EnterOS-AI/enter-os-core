package channels

import "context"

// Registry of all available channel adapters.
// To add a new platform: implement ChannelAdapter, register here.
var adapters = map[string]ChannelAdapter{
	"telegram": &TelegramAdapter{},
	"slack":    &SlackAdapter{},
	"lark":     &LarkAdapter{},
	"discord":  &DiscordAdapter{},
}

// SendAdapter is the subset of ChannelAdapter needed by SendOutbound.
// Extracted so tests can inject a no-op/mock adapter without hitting real
// platform APIs (Telegram Bot API, Slack API, etc.).
type SendAdapter interface {
	SendMessage(ctx context.Context, config map[string]interface{}, chatID string, text string) error
}

// getSendAdapter is the production implementation of GetSendAdapter —
// returns the real registered adapter's SendMessage method.
func getSendAdapter(channelType string) (SendAdapter, bool) {
	a, ok := adapters[channelType]
	if !ok {
		return nil, false
	}
	return a, true
}

// GetSendAdapter returns the SendAdapter for a channel type.
// Defaults to the real adapter; overridden by SetTestSendAdapter in tests.
var GetSendAdapter = getSendAdapter

// GetAdapter returns the adapter for a channel type.
func GetAdapter(channelType string) (ChannelAdapter, bool) {
	a, ok := adapters[channelType]
	return a, ok
}

// AdapterInfo is the metadata payload returned by ListAdapters — the Canvas
// connect-channel form renders its field list dynamically from config_schema.
type AdapterInfo struct {
	Type         string        `json:"type"`
	DisplayName  string        `json:"display_name"`
	ConfigSchema []ConfigField `json:"config_schema"`
}

// ListAdapters returns metadata about all available adapters, in a stable
// order (sorted by display name) so UI rendering + test assertions don't
// depend on Go's random map iteration.
func ListAdapters() []AdapterInfo {
	result := make([]AdapterInfo, 0, len(adapters))
	for _, a := range adapters {
		result = append(result, AdapterInfo{
			Type:         a.Type(),
			DisplayName:  a.DisplayName(),
			ConfigSchema: a.ConfigSchema(),
		})
	}
	// Sort by display name for deterministic ordering.
	for i := 1; i < len(result); i++ {
		for j := i; j > 0 && result[j-1].DisplayName > result[j].DisplayName; j-- {
			result[j-1], result[j] = result[j], result[j-1]
		}
	}
	return result
}
