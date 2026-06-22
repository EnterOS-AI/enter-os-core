// Package crypto provides AES-256 encryption for workspace secrets.
//
// Production-mode fail-secure (Top-5 #5 from the ecosystem-research outcomes
// doc, Security Auditor's top proposal): when MOLECULE_ENV=prod, Init
// refuses to let the platform boot without a valid 32-byte key. Dev mode
// retains the historical "warn + store plaintext" fallback so local
// developers aren't forced to generate a key to run the test server.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// ErrEncryptionKeyMissing is returned by InitStrict when MOLECULE_ENV=prod
// and SECRETS_ENCRYPTION_KEY is unset, malformed, or the wrong length.
// Callers (cmd/server/main.go) must treat this as a fatal boot error.
var ErrEncryptionKeyMissing = errors.New(
	"SECRETS_ENCRYPTION_KEY is required in production (MOLECULE_ENV=prod) " +
		"and must be 32 bytes raw or base64-encoded",
)

var (
	encryptionKey []byte
	initOnce      sync.Once
)

// Init loads the encryption key from SECRETS_ENCRYPTION_KEY env var.
// Safe to call multiple times — only executes once in production.
//
// Legacy dev-mode entry point: logs a WARNING when the key is missing or
// invalid and continues with encryption disabled. Production code paths
// (cmd/server/main.go) should call InitStrict instead so that missing
// keys abort boot when MOLECULE_ENV=prod.
func Init() {
	initOnce.Do(initKey)
}

// InitStrict is the fail-secure variant of Init. When MOLECULE_ENV=prod
// (or any strconv.ParseBool-truthy value), missing / malformed / wrong-
// length keys return ErrEncryptionKeyMissing so the caller can refuse to
// boot. In all other environments the behaviour matches Init — warn and
// continue with encryption disabled for local dev ergonomics.
func InitStrict() error {
	var initErr error
	initOnce.Do(func() {
		initErr = initKeyStrict()
	})
	return initErr
}

// ResetForTesting clears the encryption key and allows re-initialization.
// Only for tests — not safe for concurrent use.
func ResetForTesting() {
	encryptionKey = nil
	initOnce = sync.Once{}
}

func initKey() {
	if err := loadKeyFromEnv(); err != nil {
		log.Printf("%s", err)
	}
}

func initKeyStrict() error {
	loadErr := loadKeyFromEnv()
	if isProdEnv() && !IsEnabled() {
		if loadErr != nil {
			log.Printf("FATAL: %s", loadErr)
		}
		return fmt.Errorf("%w: refusing to boot without encryption in production", ErrEncryptionKeyMissing)
	}
	if loadErr != nil {
		// Non-prod: match Init's historical warn-and-continue behaviour.
		log.Printf("%s", loadErr)
	}
	return nil
}

// loadKeyFromEnv parses SECRETS_ENCRYPTION_KEY into encryptionKey.
// Returns a non-nil error describing the problem when the env var is set
// but unusable (wrong length, unparseable). Returns nil when the var is
// unset OR successfully applied.
func loadKeyFromEnv() error {
	key := os.Getenv("SECRETS_ENCRYPTION_KEY")
	if key == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		// Try raw key (must be exactly 32 bytes)
		if len(key) == 32 {
			encryptionKey = []byte(key)
			return nil
		}
		return fmt.Errorf("SECRETS_ENCRYPTION_KEY is set but invalid (not base64 and not 32 bytes). Encryption disabled")
	}
	if len(decoded) == 32 {
		encryptionKey = decoded
		return nil
	}
	return fmt.Errorf("SECRETS_ENCRYPTION_KEY decoded to %d bytes (expected 32). Encryption disabled", len(decoded))
}

// isProdEnv returns true when MOLECULE_ENV matches one of the canonical
// production markers. Accepts case-insensitive "prod" / "production".
func isProdEnv() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MOLECULE_ENV")))
	return v == "prod" || v == "production"
}

// IsEnabled returns true if encryption is configured.
func IsEnabled() bool {
	return len(encryptionKey) == 32
}

// Encryption version tags. Stored in workspace_secrets.encryption_version +
// global_secrets.encryption_version (migration 018). DecryptVersioned reads
// the tag to decide whether to run GCM or pass bytes through. Prevents the
// "turn on encryption → all historical secrets become unreadable" trap
// documented in #85.
const (
	// EncryptionVersionPlaintext identifies rows written when the platform
	// was running without a key. encrypted_value column holds literal
	// plaintext bytes.
	EncryptionVersionPlaintext = 0
	// EncryptionVersionAESGCM identifies rows written with the current
	// scheme (AES-256-GCM, 12-byte prefix nonce).
	EncryptionVersionAESGCM = 1
)

// CurrentEncryptionVersion returns the version a new Encrypt call would
// produce given the current key state. Callers use this as the value to
// INSERT into encryption_version.
func CurrentEncryptionVersion() int {
	if IsEnabled() {
		return EncryptionVersionAESGCM
	}
	return EncryptionVersionPlaintext
}

// Encrypt encrypts plaintext with AES-256-GCM.
// If encryption is disabled, returns the plaintext as-is.
//
// Callers should persist CurrentEncryptionVersion() alongside the returned
// bytes so Decrypt / DecryptVersioned knows how to read them back.
func Encrypt(plaintext []byte) ([]byte, error) {
	if !IsEnabled() {
		return plaintext, nil
	}

	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts AES-256-GCM ciphertext.
// If encryption is disabled, returns the data as-is.
//
// **Prefer DecryptVersioned** when the caller has access to the
// encryption_version column — this function can't tell plaintext rows from
// ciphertext and will attempt GCM on both when a key is set, mangling
// plaintext-era secrets. Kept for backward compatibility with callers that
// haven't migrated to the version-aware helper yet.
func Decrypt(ciphertext []byte) ([]byte, error) {
	if !IsEnabled() {
		return ciphertext, nil
	}

	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// DecryptVersioned is the #85 replacement for Decrypt. It reads the
// encryption_version tag and picks the right path:
//
//   - EncryptionVersionPlaintext (0): return the bytes as-is. Correct both
//     on installs that never had a key, AND on installs where the operator
//     has since enabled encryption but existing rows predate the key.
//   - EncryptionVersionAESGCM (1): run AES-256-GCM decrypt as before.
//     Fails with a clear error if IsEnabled() is false — an operator who
//     enabled then disabled encryption without re-encrypting rows.
//
// Callers that store rows this cycle should write
// CurrentEncryptionVersion() into the encryption_version column alongside
// the bytes produced by Encrypt.
func DecryptVersioned(value []byte, version int) ([]byte, error) {
	switch version {
	case EncryptionVersionPlaintext:
		return value, nil
	case EncryptionVersionAESGCM:
		if !IsEnabled() {
			return nil, errors.New("row was encrypted (version=1) but SECRETS_ENCRYPTION_KEY is unset; cannot decrypt")
		}
		return Decrypt(value)
	default:
		return nil, fmt.Errorf("unknown encryption_version=%d; platform upgrade required", version)
	}
}
