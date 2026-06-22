package channels

// Field-level encryption for sensitive channel_config values (#319).
//
// workspace_channels.channel_config is a JSONB column holding adapter-specific
// settings. Some fields are secret — Telegram bot tokens, webhook shared
// secrets — and must not sit in cleartext at the database layer where a
// backup leak or read-replica mis-grant would expose them. workspace_secrets
// already encrypts values with AES-256-GCM; this file mirrors that posture
// for channel_config so the security stance is consistent.
//
// Strategy: lazy field-level encryption with a version prefix.
//
//   plaintext    "123456:AA..."                (legacy / pre-migration)
//   ciphertext   "ec1:<base64-GCM-ciphertext>" (new writes)
//
// On read, a missing "ec1:" prefix means the row predates the encryption
// rollout — return the value as-is (pass-through). On write, always encrypt.
// Rows upgrade lazily on the next PATCH/Create. An operator wishing to
// force-upgrade everything can re-save each channel via the Canvas Update
// button.
//
// Only `bot_token` and `webhook_secret` are considered secret. Other fields
// (chat_id, channel_name, enable_polling, etc.) stay in cleartext so the
// SQL-level `channel_config->>'chat_id'` lookups in the webhook receiver
// remain efficient.

import (
	"encoding/base64"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
)

// sensitiveFields is the set of channel_config keys that get encrypted at
// rest. Add a new key here to extend coverage — do NOT widen this to the
// whole config: it would break SQL field-access for non-secret keys like
// `chat_id` that the webhook receiver queries.
var sensitiveFields = []string{"bot_token", "webhook_secret"}

// ciphertextPrefix marks values encrypted by EncryptSensitiveFields so
// DecryptSensitiveFields can tell "new encrypted value" from a legacy
// plaintext row. The string is intentionally distinctive — no real bot
// token begins with "ec1:".
const ciphertextPrefix = "ec1:"

// EncryptSensitiveFields encrypts every known-sensitive value in config in
// place. Values that are already prefixed (already encrypted) are left
// untouched so a no-op re-save won't double-encrypt. Non-string values,
// empty strings, and unknown fields pass through unchanged.
//
// When SECRETS_ENCRYPTION_KEY is not configured (dev default), values are
// stored as plaintext — consistent with workspace_secrets' dev fallback.
func EncryptSensitiveFields(config map[string]interface{}) error {
	if config == nil {
		return nil
	}
	for _, field := range sensitiveFields {
		raw, ok := config[field]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok || s == "" {
			continue
		}
		if strings.HasPrefix(s, ciphertextPrefix) {
			// already encrypted (idempotent re-save)
			continue
		}
		if !crypto.IsEnabled() {
			// Dev fallback: leave plaintext so local test setups without a
			// key keep working. Prod boots with crypto.InitStrict which
			// refuses to start without a key, so this branch is dev-only.
			continue
		}
		ct, err := crypto.Encrypt([]byte(s))
		if err != nil {
			return err
		}
		config[field] = ciphertextPrefix + base64.StdEncoding.EncodeToString(ct)
	}
	return nil
}

// DecryptSensitiveFields is the inverse of EncryptSensitiveFields. Values
// without the ciphertext prefix are returned as-is (legacy plaintext rows).
// Values with the prefix are base64-decoded and run through AES-256-GCM.
//
// When SECRETS_ENCRYPTION_KEY is not configured but a prefixed value is
// encountered, that's an operator error (enabled encryption then disabled
// the key). Return the raw prefixed string in that case — the adapter will
// fail to authenticate with Telegram/Slack and the operator will see a
// clear "invalid bot token" message rather than a silent mis-decrypt.
func DecryptSensitiveFields(config map[string]interface{}) error {
	if config == nil {
		return nil
	}
	for _, field := range sensitiveFields {
		raw, ok := config[field]
		if !ok {
			continue
		}
		s, ok := raw.(string)
		if !ok || s == "" {
			continue
		}
		if !strings.HasPrefix(s, ciphertextPrefix) {
			// legacy plaintext row — pass through
			continue
		}
		if !crypto.IsEnabled() {
			// encryption-expected row but no key — leave encoded so callers
			// fail loudly at Telegram rather than mis-decrypt.
			continue
		}
		encoded := strings.TrimPrefix(s, ciphertextPrefix)
		ct, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return err
		}
		pt, err := crypto.Decrypt(ct)
		if err != nil {
			return err
		}
		config[field] = string(pt)
	}
	return nil
}
