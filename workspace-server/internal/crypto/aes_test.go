package crypto

import (
	"encoding/base64"
	"errors"
	"os"
	"testing"
)

func resetKey() {
	ResetForTesting()
}

func TestInit_NoEnvVar(t *testing.T) {
	resetKey()
	os.Unsetenv("SECRETS_ENCRYPTION_KEY")
	Init()
	if IsEnabled() {
		t.Error("expected encryption disabled when env var not set")
	}
}

func TestInit_ValidBase64Key(t *testing.T) {
	resetKey()
	// 32-byte key encoded as base64
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	os.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")

	Init()

	if !IsEnabled() {
		t.Error("expected encryption enabled with valid 32-byte base64 key")
	}
	resetKey()
}

func TestInit_Valid32ByteRawKey(t *testing.T) {
	resetKey()
	// 32-byte key that is NOT valid base64 — forces raw byte path
	rawKey := "abcdefghijklmnopqrstuvwxyz!@#$%^" // 32 chars, not valid base64
	os.Setenv("SECRETS_ENCRYPTION_KEY", rawKey)
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")

	Init()

	if !IsEnabled() {
		t.Error("expected encryption enabled with 32-byte raw key")
	}
	resetKey()
}

func TestInit_InvalidKey_TooShort(t *testing.T) {
	resetKey()
	os.Setenv("SECRETS_ENCRYPTION_KEY", "tooshort")
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")

	Init()

	if IsEnabled() {
		t.Error("expected encryption disabled for short invalid key")
	}
}

func TestInit_Base64Key_Wrong_Length(t *testing.T) {
	resetKey()
	// Valid base64 but decodes to 16 bytes, not 32
	key := make([]byte, 16)
	os.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")

	Init()

	if IsEnabled() {
		t.Error("expected encryption disabled for 16-byte base64 key")
	}
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	resetKey()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 42)
	}
	os.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")
	Init()
	defer resetKey()

	plaintext := []byte("super-secret-api-key-value")

	ciphertext, err := Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if string(ciphertext) == string(plaintext) {
		t.Error("ciphertext should differ from plaintext")
	}

	decrypted, err := Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if string(decrypted) != string(plaintext) {
		t.Errorf("expected %q, got %q", plaintext, decrypted)
	}
}

func TestEncrypt_Disabled_ReturnsPlaintext(t *testing.T) {
	resetKey()
	// No key set → passthrough mode

	input := []byte("not encrypted")
	out, err := Encrypt(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(input) {
		t.Errorf("expected passthrough, got %q", out)
	}
}

func TestDecrypt_Disabled_ReturnsInput(t *testing.T) {
	resetKey()

	input := []byte("raw value")
	out, err := Decrypt(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != string(input) {
		t.Errorf("expected passthrough, got %q", out)
	}
}

func TestDecrypt_TooShort_ReturnsError(t *testing.T) {
	resetKey()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	os.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")
	Init()
	defer resetKey()

	// Ciphertext shorter than nonce (12 bytes for GCM)
	_, err := Decrypt([]byte("short"))
	if err == nil {
		t.Error("expected error for ciphertext shorter than nonce size")
	}
}

func TestEncryptDecrypt_EmptyInput(t *testing.T) {
	resetKey()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 10)
	}
	os.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	defer os.Unsetenv("SECRETS_ENCRYPTION_KEY")
	Init()
	defer resetKey()

	ciphertext, err := Encrypt([]byte(""))
	if err != nil {
		t.Fatalf("Encrypt empty failed: %v", err)
	}

	decrypted, err := Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt empty failed: %v", err)
	}

	if string(decrypted) != "" {
		t.Errorf("expected empty string, got %q", decrypted)
	}
}

func TestIsEnabled_FalseByDefault(t *testing.T) {
	resetKey()
	if IsEnabled() {
		t.Error("expected IsEnabled to return false when key is nil")
	}
}

// -------- InitStrict: fail-secure in production (Top-5 #5) ---------

func TestInitStrict_FailsInProdWhenKeyMissing(t *testing.T) {
	resetKey()
	t.Setenv("MOLECULE_ENV", "prod")
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")

	err := InitStrict()
	if err == nil {
		t.Fatalf("InitStrict must return an error when MOLECULE_ENV=prod and key is missing")
	}
	if !errors.Is(err, ErrEncryptionKeyMissing) {
		t.Errorf("error must wrap ErrEncryptionKeyMissing, got: %v", err)
	}
	if IsEnabled() {
		t.Errorf("encryption must not be enabled when key was never loaded")
	}
}

func TestInitStrict_FailsInProdOnWrongLengthKey(t *testing.T) {
	resetKey()
	t.Setenv("MOLECULE_ENV", "production")
	// 24-char raw string — decodes as 18 bytes of base64, not 32.
	t.Setenv("SECRETS_ENCRYPTION_KEY", "not-thirty-two-bytes-aaa")

	err := InitStrict()
	if err == nil {
		t.Fatalf("InitStrict must return an error when the key has the wrong length in production")
	}
	if !errors.Is(err, ErrEncryptionKeyMissing) {
		t.Errorf("error must wrap ErrEncryptionKeyMissing, got: %v", err)
	}
}

func TestInitStrict_SucceedsInProdWithValidKey(t *testing.T) {
	resetKey()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("MOLECULE_ENV", "prod")
	t.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))

	if err := InitStrict(); err != nil {
		t.Fatalf("InitStrict must succeed with a valid 32-byte key in production: %v", err)
	}
	if !IsEnabled() {
		t.Error("encryption must be enabled after a successful InitStrict")
	}
}

func TestInitStrict_AllowsDevModeWithoutKey(t *testing.T) {
	resetKey()
	t.Setenv("MOLECULE_ENV", "") // unset → dev
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")

	if err := InitStrict(); err != nil {
		t.Errorf("InitStrict must NOT fail in dev mode when key is missing: %v", err)
	}
	if IsEnabled() {
		t.Error("encryption must be disabled when key is unset in dev mode")
	}
}

func TestInitStrict_AllowsStagingWithoutKey(t *testing.T) {
	resetKey()
	t.Setenv("MOLECULE_ENV", "staging")
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")

	if err := InitStrict(); err != nil {
		t.Errorf("InitStrict must NOT fail for non-prod environments: %v", err)
	}
}

func TestIsProdEnv_CaseInsensitive(t *testing.T) {
	cases := map[string]bool{
		"prod":       true,
		"PROD":       true,
		"Prod":       true,
		"production": true,
		"PRODUCTION": true,
		"  prod  ":   true, // trim
		"staging":    false,
		"dev":        false,
		"":           false,
	}
	for env, want := range cases {
		t.Setenv("MOLECULE_ENV", env)
		if got := isProdEnv(); got != want {
			t.Errorf("isProdEnv() with MOLECULE_ENV=%q = %v, want %v", env, got, want)
		}
	}
}

// -------- DecryptVersioned + CurrentEncryptionVersion (#85) ---------

func TestDecryptVersioned_PlaintextPassesThrough(t *testing.T) {
	// Simulates the historical-row case: rows written when encryption was
	// disabled. Current platform has a key but the row doesn't.
	resetKey()
	key := make([]byte, 32)
	t.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	initKey()

	plaintext := []byte("fake-token-representing-historical-plaintext")
	out, err := DecryptVersioned(plaintext, EncryptionVersionPlaintext)
	if err != nil {
		t.Fatalf("DecryptVersioned on plaintext version must not error, got: %v", err)
	}
	if string(out) != string(plaintext) {
		t.Errorf("plaintext bytes must pass through unchanged, got %q", out)
	}
}

func TestDecryptVersioned_AESGCMRoundTrip(t *testing.T) {
	resetKey()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	initKey()

	plain := []byte("fake-plaintext-for-round-trip-test")
	ct, err := Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}
	out, err := DecryptVersioned(ct, EncryptionVersionAESGCM)
	if err != nil {
		t.Fatalf("DecryptVersioned(v=1) failed: %v", err)
	}
	if string(out) != string(plain) {
		t.Errorf("round-trip mismatch: want %q got %q", plain, out)
	}
}

func TestDecryptVersioned_AESGCMRequiresEnabledKey(t *testing.T) {
	resetKey()
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")

	out, err := DecryptVersioned([]byte("opaque"), EncryptionVersionAESGCM)
	if err == nil {
		t.Fatal("DecryptVersioned(v=1) must error when IsEnabled() is false")
	}
	if out != nil {
		t.Errorf("expected nil bytes on error, got %q", out)
	}
}

func TestDecryptVersioned_UnknownVersionRejected(t *testing.T) {
	resetKey()
	_, err := DecryptVersioned([]byte("any"), 999)
	if err == nil {
		t.Fatal("DecryptVersioned must reject unknown versions")
	}
}

func TestCurrentEncryptionVersion_TracksKeyState(t *testing.T) {
	resetKey()
	t.Setenv("SECRETS_ENCRYPTION_KEY", "")
	if v := CurrentEncryptionVersion(); v != EncryptionVersionPlaintext {
		t.Errorf("key unset → plaintext version; got %d", v)
	}

	resetKey()
	key := make([]byte, 32)
	t.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	initKey()
	if v := CurrentEncryptionVersion(); v != EncryptionVersionAESGCM {
		t.Errorf("key set → AES-GCM version; got %d", v)
	}
}

func TestDecryptVersioned_HistoricalPlaintextAfterKeyEnabled(t *testing.T) {
	// The exact #85 scenario: platform ran without a key, secrets were
	// stored as plaintext (version=0), then a key was added. Old rows
	// MUST still decrypt via the version=0 path, even though the current
	// platform could run GCM.
	resetKey()
	key := make([]byte, 32)
	for i := range key {
		key[i] = 1
	}
	t.Setenv("SECRETS_ENCRYPTION_KEY", base64.StdEncoding.EncodeToString(key))
	initKey()
	if !IsEnabled() {
		t.Fatal("test setup: expected IsEnabled() to be true")
	}

	// Simulate a historical plaintext row — bytes are literal token,
	// version column stored the old default (0).
	historicalPlaintext := []byte("fake-historical-plaintext-abcdef0123456789")
	out, err := DecryptVersioned(historicalPlaintext, EncryptionVersionPlaintext)
	if err != nil {
		t.Fatalf("historical plaintext row must decrypt post-key-enable, got: %v", err)
	}
	if string(out) != string(historicalPlaintext) {
		t.Errorf("historical plaintext must round-trip identically; got %q", out)
	}
}
