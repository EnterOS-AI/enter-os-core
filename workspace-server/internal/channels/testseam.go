package channels

import "os"

// Test seams for the GATING channels e2e (tests/e2e/test_channels_e2e.sh).
//
// Every adapter pins its outbound destination to the real vendor host
// (hooks.slack.com, discord.com, api.telegram.org) in both ValidateConfig and
// SendMessage. That host pin is correct for production, but it means a real
// end-to-end test cannot point the LIVE send/discover path at a local mock
// upstream — so today the outbound serialize+POST is only ever asserted by
// unit tests that reconstruct the payload by hand (see lark_test.go's
// "we can't change the prefix const" comment) and never proven through the
// running platform.
//
// These two env-gated overrides close that gap WITHOUT changing any
// production behaviour:
//
//   - MOLECULE_CHANNELS_TEST_WEBHOOK_BASE — when set, Slack Incoming Webhook
//     URLs with this prefix are accepted as send destinations (in addition to
//     the real hooks.slack.com host). Lets the e2e create a slack channel whose
//     webhook_url points at a local httptest mock and assert the mock RECEIVED
//     the serialized {"text":...} payload.
//
//   - MOLECULE_CHANNELS_TEST_TELEGRAM_API_BASE — when set, TelegramAdapter.
//     DiscoverChats builds its bot client against this API base instead of
//     api.telegram.org, so POST /channels/discover can be exercised against a
//     mock that serves getMe/getUpdates and the e2e can assert the discovered
//     chats round-trip.
//
// Both vars are NEVER set in any production or staging deploy. The helpers
// return "" there, so the real vendor-host pins are the only thing that
// passes — production behaviour is byte-for-byte unchanged. Reading os.Getenv
// on each call (not caching) keeps the seam honest: a process that never sets
// the var can never accidentally enable it.

// channelsTestWebhookBase returns the test-only accepted webhook base prefix,
// or "" in production. See package doc above.
func channelsTestWebhookBase() string {
	return os.Getenv("MOLECULE_CHANNELS_TEST_WEBHOOK_BASE")
}

// channelsTestTelegramAPIBase returns the test-only Telegram Bot API base
// (a printf format string "<base>/bot%s/%s"), or "" in production.
func channelsTestTelegramAPIBase() string {
	return os.Getenv("MOLECULE_CHANNELS_TEST_TELEGRAM_API_BASE")
}
