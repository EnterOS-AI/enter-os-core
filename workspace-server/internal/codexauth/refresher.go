// Package codexauth owns the SINGLE, platform-side refresh of the global
// codex (ChatGPT/Codex subscription) OAuth credential stored in the
// global_secrets table under key CODEX_AUTH_JSON.
//
// THE PROBLEM IT FIXES (agents-team prod, 2026-05-31)
//
// Multiple codex workspaces share ONE ChatGPT-Pro OAuth token (the global
// secret CODEX_AUTH_JSON). OpenAI's refresh_token is SINGLE-USE: every refresh
// rotates it and invalidates the prior one. When each per-agent codex
// app-server refreshed independently on a 401, the siblings' in-flight tokens
// were invalidated within seconds — a refresh storm that burned the seed and
// wedged every codex agent.
//
// THE FIX (two halves; this is the core half)
//
//  1. The per-workspace codex app-server NO LONGER refreshes (the template's
//     OAuth POST is gated off by default — see the codex template's
//     codex_auth_sync.sh / CODEX_AUTH_REFRESH_OWNER gate). Workspaces only ever
//     GET the current token and write it to auth.json.
//  2. ONE owner refreshes the rotating refresh_token: this background goroutine
//     in the platform. It is structurally single-flight (one goroutine + a
//     package mutex), refreshes ONLY when the access_token is within a safety
//     margin of expiry, POSTs the refresh_token at most ONCE per due cycle, and
//     writes the rotated blob back to global_secrets. On a permanent failure
//     (the seed was already burned by an out-of-band login) it logs ONCE and
//     backs off — it never hot-loops a dead refresh_token.
//
// Billing-mode resolution and the byok strip are UNTOUCHED by this package.
package codexauth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/crypto"
)

const (
	// CodexAuthSecretKey is the global_secrets key holding the shared codex
	// ChatGPT/Codex subscription OAuth blob (auth.json contents).
	CodexAuthSecretKey = "CODEX_AUTH_JSON"

	// oauthTokenURL is OpenAI's OAuth token endpoint. The ONLY endpoint this
	// package ever POSTs to, and only for a due refresh.
	oauthTokenURL = "https://auth.openai.com/oauth/token"

	// codexOAuthClientID is the public Codex CLI OAuth client id (the same id
	// the codex CLI sends). Not a secret.
	codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// refreshSafetyMargin is how far ahead of access_token expiry a refresh is
	// considered DUE. A token expiring within this window is refreshed now; one
	// expiring later is left untouched (skip-when-fresh). Generous so a slow
	// tick can never let the shared token lapse for the fleet.
	refreshSafetyMargin = 15 * time.Minute

	// defaultInterval is how often the loop wakes to check due-ness. The check
	// is cheap (decrypt + JWT exp parse) and only POSTs when actually due.
	defaultInterval = 5 * time.Minute

	// permanentFailureBackoff is how long the loop waits after a PERMANENT
	// refresh failure (invalid_grant / "refresh token already used"). The seed
	// is burned until a human re-seeds a fresh login; there is nothing to retry,
	// so we back off hard rather than hammer the dead token.
	permanentFailureBackoff = 1 * time.Hour
)

// SecretStore is the minimal global_secrets surface the refresher needs. The
// production implementation (postgresStore) is backed by *sql.DB; tests inject
// a fake. It is deliberately tiny — read one key, write one key — so the test
// double is trivial and the refresher never reaches for the package-global DB.
type SecretStore interface {
	// Get returns the decrypted secret value and true, or ("", false) when the
	// key is absent. A non-nil error is a real read failure (not absence).
	Get(ctx context.Context, key string) (value string, found bool, err error)
	// Put encrypts and upserts value under key, bumping the row's updated_at
	// (the "last_refresh" timestamp). It is the rotated-blob write-back.
	Put(ctx context.Context, key, value string) error
}

// httpDoer is the http client seam (real *http.Client in prod, fake transport
// in tests). Tests NEVER hit the network.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// refresher is the single-owner refresh engine. The package-level mutex makes
// the refresh structurally single-flight: even if two refreshOnce calls raced
// (they cannot in prod — one goroutine drives it — but a test or a future
// caller might), only one POSTs at a time, and the access-token freshness
// re-check inside the lock means the second sees a freshly-rotated token and
// skips. One goroutine + this mutex = single-flight by construction.
type refresher struct {
	store  SecretStore
	client httpDoer
	now    func() time.Time

	// permanentlyFailed records that the current seed's refresh_token was
	// rejected as already-used/invalid. While set, refreshOnce is INERT (it
	// will not re-POST the dead token) until the secret value CHANGES (a human
	// re-seed), detected by comparing the stored blob. This is the anti-storm
	// latch — it lives on the struct, not globally, so it resets if the seed is
	// replaced out of band.
	failedSeed string // the auth-json blob that failed; "" = no known failure
}

// mu serializes refreshOnce across the process. Package-level so the
// single-flight guarantee holds regardless of how many refresher values exist
// (in prod there is exactly one).
var mu sync.Mutex

// oauthTokens is the token trio inside auth.json (and the OAuth response).
type oauthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
}

// StartCodexAuthRefresher launches the single background refresher goroutine.
// It returns immediately; the loop runs until ctx is cancelled. Wire it under
// supervised.RunWithRecover in main.go like the other Start* sweeps.
//
// db may be nil only in tests that drive refreshOnce directly; in prod it is
// the server's *sql.DB. The loop is INERT (logs once, keeps ticking) whenever
// CODEX_AUTH_JSON is absent — a deployment with no shared codex seed pays only
// a cheap periodic read.
func StartCodexAuthRefresher(ctx context.Context, db *sql.DB) {
	r := &refresher{
		store:  &postgresStore{db: db},
		client: &http.Client{Timeout: 30 * time.Second},
		now:    time.Now,
	}
	r.run(ctx, defaultInterval)
}

// run is the tick loop. It checks due-ness every interval and on a permanent
// failure waits permanentFailureBackoff before the next check (never a tight
// retry of a burned token).
func (r *refresher) run(ctx context.Context, interval time.Duration) {
	// Check once promptly on boot, then on the interval.
	for {
		wait := interval
		if perm := r.refreshOnce(ctx); perm {
			// Permanent failure this cycle — the seed is burned. Back off hard;
			// a human must re-seed. We keep ticking (a re-seed CHANGES the blob,
			// which clears the latch) but slowly.
			wait = permanentFailureBackoff
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Printf("codexauth: context done; stopping refresher")
			return
		case <-timer.C:
		}
	}
}

// refreshOnce performs ONE due-check + at most one refresh POST. It returns
// permanentFailure=true iff the refresh_token was permanently rejected this
// cycle (the caller backs off). All other outcomes (inert/skip/rotated/transient
// error) return false.
//
// It is single-flight: the package mutex is held for the whole read→decide→
// POST→write-back so two callers cannot both POST the (single-use) refresh_token.
func (r *refresher) refreshOnce(ctx context.Context) (permanentFailure bool) {
	mu.Lock()
	defer mu.Unlock()

	blob, found, err := r.store.Get(ctx, CodexAuthSecretKey)
	if err != nil {
		log.Printf("codexauth: read CODEX_AUTH_JSON failed: %v (skipping this cycle)", err)
		return false
	}
	if !found || strings.TrimSpace(blob) == "" {
		// INERT: no shared codex seed in this deployment. Cheap no-op.
		log.Printf("codexauth: no CODEX_AUTH_JSON in global_secrets — refresher inert")
		// A previously-failed seed that has since been DELETED clears the latch.
		r.failedSeed = ""
		return false
	}

	// Anti-storm latch: if THIS exact blob already failed permanently, do not
	// re-POST its dead refresh_token. A re-seed changes the blob and clears it.
	if r.failedSeed != "" && r.failedSeed == blob {
		return false
	}
	if r.failedSeed != "" && r.failedSeed != blob {
		// The seed changed out of band (human re-login) — give it a fresh chance.
		r.failedSeed = ""
	}

	tokens, err := parseTokens(blob)
	if err != nil {
		log.Printf("codexauth: CODEX_AUTH_JSON is not parseable codex auth json: %v (skipping)", err)
		return false
	}
	if tokens.RefreshToken == "" {
		log.Printf("codexauth: CODEX_AUTH_JSON carries no refresh_token (skipping)")
		return false
	}

	// Skip-when-fresh: only refresh within the safety margin of expiry. A blob
	// with an unparseable/absent access_token exp is treated as DUE (better to
	// refresh a token we cannot date than let the fleet lapse).
	exp, haveExp := jwtExp(tokens.AccessToken)
	if haveExp {
		remaining := exp.Sub(r.now())
		if remaining > refreshSafetyMargin {
			// Fresh — nothing to do. No POST.
			return false
		}
	}

	// DUE: POST the refresh_token ONCE.
	newTokens, perm, err := r.doRefresh(ctx, tokens.RefreshToken)
	if err != nil {
		if perm {
			// Permanent: the seed is burned. Latch it so we don't re-POST, log
			// ONCE, and DO NOT write anything back.
			log.Printf("codexauth: PERMANENT refresh failure (refresh_token rejected): %v — "+
				"NOT writing back; the shared CODEX_AUTH_JSON seed is burned and must be re-seeded "+
				"via a fresh codex login. Backing off.", err)
			r.failedSeed = blob
			return true
		}
		// Transient (network/5xx): no write-back, retry next cycle (no backoff).
		log.Printf("codexauth: transient refresh error: %v (will retry next cycle)", err)
		return false
	}

	// Success: merge the rotated trio into the blob (preserving every other
	// field) and write it back encrypted, bumping updated_at (last_refresh).
	rotated, err := mergeTokens(blob, newTokens)
	if err != nil {
		log.Printf("codexauth: failed to merge rotated tokens into auth json: %v (NOT writing back)", err)
		return false
	}
	if err := r.store.Put(ctx, CodexAuthSecretKey, rotated); err != nil {
		log.Printf("codexauth: write-back of rotated CODEX_AUTH_JSON failed: %v", err)
		return false
	}
	r.failedSeed = "" // success clears any stale latch
	log.Printf("codexauth: rotated shared CODEX_AUTH_JSON (single-owner refresh)")
	return false
}

// doRefresh POSTs the refresh_token to OpenAI's OAuth endpoint exactly once and
// returns the rotated trio. permanent=true marks an unrecoverable rejection
// (HTTP 400 invalid_grant / "refresh token already used") so the caller latches
// and backs off instead of retrying.
func (r *refresher) doRefresh(ctx context.Context, refreshToken string) (tokens oauthTokens, permanent bool, err error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     codexOAuthClientID,
		"refresh_token": refreshToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(string(body)))
	if err != nil {
		return oauthTokens{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return oauthTokens{}, false, err // transient: network
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusOK {
		var t oauthTokens
		if err := json.Unmarshal(respBody, &t); err != nil {
			return oauthTokens{}, false, fmt.Errorf("decode token response: %w", err)
		}
		if t.AccessToken == "" {
			return oauthTokens{}, false, fmt.Errorf("token response missing access_token")
		}
		return t, false, nil
	}

	// Non-200. A 400 (and any body naming invalid_grant / already-used) is a
	// PERMANENT rejection of the refresh_token. 401/403 likewise mean the seed
	// is no good. Everything else (429/5xx/network-shaped) is transient.
	lowerBody := strings.ToLower(string(respBody))
	isInvalidGrant := strings.Contains(lowerBody, "invalid_grant") ||
		strings.Contains(lowerBody, "refresh token already used") ||
		strings.Contains(lowerBody, "already been used") ||
		strings.Contains(lowerBody, "token has been revoked")
	switch {
	case resp.StatusCode == http.StatusBadRequest && isInvalidGrant:
		return oauthTokens{}, true, fmt.Errorf("oauth %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return oauthTokens{}, true, fmt.Errorf("oauth %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	default:
		return oauthTokens{}, false, fmt.Errorf("oauth %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// parseTokens extracts the OAuth trio from an auth.json blob, accepting both
// the nested `{"tokens":{...}}` shape the codex CLI writes and a flat top-level
// shape some seeds use.
func parseTokens(blob string) (oauthTokens, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &top); err != nil {
		return oauthTokens{}, err
	}
	if nested, ok := top["tokens"]; ok {
		var t oauthTokens
		if err := json.Unmarshal(nested, &t); err != nil {
			return oauthTokens{}, fmt.Errorf("decode nested tokens: %w", err)
		}
		return t, nil
	}
	var t oauthTokens
	if err := json.Unmarshal([]byte(blob), &t); err != nil {
		return oauthTokens{}, err
	}
	return t, nil
}

// mergeTokens writes the rotated trio back into the original blob in-place,
// preserving the blob's shape (nested-vs-flat) and every other field. A field
// in the OAuth response that is empty (e.g. id_token omitted) does NOT clobber
// the existing value.
func mergeTokens(blob string, rotated oauthTokens) (string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &top); err != nil {
		return "", err
	}

	applyTo := func(m map[string]json.RawMessage) error {
		setStr := func(key, val string) error {
			if val == "" {
				return nil // don't clobber an existing value with an empty one
			}
			b, err := json.Marshal(val)
			if err != nil {
				return err
			}
			m[key] = b
			return nil
		}
		if err := setStr("access_token", rotated.AccessToken); err != nil {
			return err
		}
		if err := setStr("refresh_token", rotated.RefreshToken); err != nil {
			return err
		}
		if err := setStr("id_token", rotated.IDToken); err != nil {
			return err
		}
		return nil
	}

	if nestedRaw, ok := top["tokens"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(nestedRaw, &nested); err != nil {
			return "", fmt.Errorf("decode nested tokens for merge: %w", err)
		}
		if err := applyTo(nested); err != nil {
			return "", err
		}
		nb, err := json.Marshal(nested)
		if err != nil {
			return "", err
		}
		top["tokens"] = nb
	} else {
		if err := applyTo(top); err != nil {
			return "", err
		}
	}

	out, err := json.Marshal(top)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// jwtExp decodes the `exp` claim (Unix seconds) from a JWT access token WITHOUT
// verifying the signature (we only need the expiry to decide due-ness; the
// token's validity is OpenAI's to enforce). Returns ok=false when the token is
// not a parseable 3-part JWT or carries no numeric exp.
func jwtExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders pad; tolerate standard base64url with padding too.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	secs, err := claims.Exp.Int64()
	if err != nil || secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0), true
}

// postgresStore is the production SecretStore backed by global_secrets, using
// the SAME crypto path the secrets handler uses (DecryptVersioned on read,
// Encrypt + CurrentEncryptionVersion on write).
type postgresStore struct {
	db *sql.DB
}

func (s *postgresStore) Get(ctx context.Context, key string) (string, bool, error) {
	var enc []byte
	var ver int
	err := s.db.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM global_secrets WHERE key = $1`, key).
		Scan(&enc, &ver)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	plain, err := crypto.DecryptVersioned(enc, ver)
	if err != nil {
		return "", false, err
	}
	return string(plain), true, nil
}

func (s *postgresStore) Put(ctx context.Context, key, value string) error {
	enc, err := crypto.Encrypt([]byte(value))
	if err != nil {
		return err
	}
	ver := crypto.CurrentEncryptionVersion()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO global_secrets (key, encrypted_value, encryption_version)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, key, enc, ver)
	return err
}
