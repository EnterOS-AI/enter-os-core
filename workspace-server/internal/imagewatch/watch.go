// Package imagewatch closes the last manual step of the runtime CD chain
// (see docs/workspace-runtime-package.md): polling GHCR for digest changes
// on the workspace-template-* :latest tags and invoking the existing
// workspace-image refresh logic when one moves.
//
// Without this, an operator has to either SSH and run
// scripts/refresh-workspace-images.sh OR curl
// /admin/workspace-images/refresh after every runtime release. With it,
// the platform self-heals to the latest published runtime within one
// polling interval — fully hands-off from "merge PR" to "containers
// running new code".
//
// Opt-in via IMAGE_AUTO_REFRESH=true. SaaS deployments whose deploy
// pipeline pulls on every release should leave it disabled (would be
// redundant work).
package imagewatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Molecule-AI/molecule-monorepo/platform/internal/handlers"
)

// DefaultInterval is the polling cadence. Runtime publishes happen at most
// a handful of times per day; a 5-minute lag between PyPI publish + image
// rebuild and the platform pulling is well within the implicit SLA. Going
// shorter wastes GHCR rate budget for no real win.
const DefaultInterval = 5 * time.Minute

// Refresher is the subset of *handlers.WorkspaceImageService the watcher
// needs. Defined here so the watcher can be tested with a fake.
type Refresher interface {
	Refresh(ctx context.Context, runtimes []string, recreate bool) (handlers.RefreshResult, error)
}

// Watcher polls GHCR for digest changes and invokes Refresher when one
// moves. Tracks last-observed remote digest per runtime in memory; on a
// fresh boot, the first observation per runtime seeds the tracker without
// triggering a refresh (containers stay on whatever image they have until
// the NEXT upstream change moves the digest).
type Watcher struct {
	svc      Refresher
	runtimes []string
	interval time.Duration
	http     *http.Client
	seen     map[string]string // runtime → last-observed remote digest
}

// New returns a watcher configured with the canonical runtimes list. Pass
// the WorkspaceImageService from the handlers package as svc.
func New(svc Refresher) *Watcher {
	return &Watcher{
		svc:      svc,
		runtimes: append([]string(nil), handlers.AllRuntimes...),
		interval: DefaultInterval,
		http:     &http.Client{Timeout: 10 * time.Second},
		seen:     make(map[string]string, len(handlers.AllRuntimes)),
	}
}

// digestFetcher returns the current upstream digest for a given runtime.
// Pulled out of tick() so tests can substitute a deterministic fake
// without standing up an httptest server for the GHCR API.
type digestFetcher func(ctx context.Context, runtime string) (string, error)

// Run blocks until ctx is cancelled, polling once per interval.
func (w *Watcher) Run(ctx context.Context) {
	log.Printf("image-auto-refresh: started (interval=%s, runtimes=%d)", w.interval, len(w.runtimes))
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	// Run one tick immediately so digests get seeded without waiting a full
	// interval. The first tick is seed-only: no refresh fires.
	w.tick(ctx, w.remoteDigest)
	for {
		select {
		case <-ctx.Done():
			log.Printf("image-auto-refresh: stopping (%v)", ctx.Err())
			return
		case <-tick.C:
			w.tick(ctx, w.remoteDigest)
		}
	}
}

func (w *Watcher) tick(ctx context.Context, fetch digestFetcher) {
	for _, rt := range w.runtimes {
		remote, err := fetch(ctx, rt)
		if err != nil {
			log.Printf("image-auto-refresh: %s digest fetch failed: %v", rt, err)
			continue
		}
		prev, hadPrev := w.seen[rt]
		w.seen[rt] = remote
		if !hadPrev {
			// Seed-only — don't refresh on first observation. Server may
			// have just booted; the local image either matches this digest
			// or operator can manually refresh once at deploy time.
			continue
		}
		if prev == remote {
			continue
		}
		log.Printf("image-auto-refresh: %s digest moved %s → %s, refreshing",
			rt, shortDigest(prev), shortDigest(remote))
		res, err := w.svc.Refresh(ctx, []string{rt}, true)
		if err != nil {
			log.Printf("image-auto-refresh: %s refresh failed: %v (pulled=%v recreated=%v)",
				rt, err, res.Pulled, res.Recreated)
			// Roll back the seen-digest so the next tick retries — without
			// this, a transient Docker error during recreate would leave
			// the watcher convinced the work was done.
			w.seen[rt] = prev
			continue
		}
		log.Printf("image-auto-refresh: %s pulled=%v recreated=%v failed=%v",
			rt, res.Pulled, res.Recreated, res.Failed)
	}
}

// remoteDigest queries GHCR for the current manifest digest of the
// workspace-template-<runtime>:latest image. Uses the Docker Registry V2
// HTTP API: get a bearer token, then HEAD the manifest.
//
// Auth: if GHCR_USER+GHCR_TOKEN are set, basic-auth the token request
// (works for both public and private images). If unset, anonymous token
// (works for public images only — every workspace template is public).
func (w *Watcher) remoteDigest(ctx context.Context, runtime string) (string, error) {
	repo := "molecule-ai/workspace-template-" + runtime
	tok, err := w.fetchPullToken(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("pull token: %w", err)
	}
	manifestURL := fmt.Sprintf("https://ghcr.io/v2/%s/manifests/latest", repo)
	req, err := http.NewRequestWithContext(ctx, "HEAD", manifestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	// Accept manifest + index media types so GHCR returns the digest of
	// whatever the :latest tag points at without doing a content-negotiation
	// rewrite that would change the digest server-side.
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}, ","))
	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HEAD %s → %d", manifestURL, resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("no Docker-Content-Digest in %s response", manifestURL)
	}
	return digest, nil
}

// fetchPullToken negotiates a short-lived bearer token from GHCR's token
// endpoint scoped to repo:pull. GHCR requires a token even for anonymous
// pulls of public images.
func (w *Watcher) fetchPullToken(ctx context.Context, repo string) (string, error) {
	q := url.Values{}
	q.Set("service", "ghcr.io")
	q.Set("scope", "repository:"+repo+":pull")
	tokURL := "https://ghcr.io/token?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", tokURL, nil)
	if err != nil {
		return "", err
	}
	if user, tok := strings.TrimSpace(os.Getenv("GHCR_USER")), strings.TrimSpace(os.Getenv("GHCR_TOKEN")); user != "" && tok != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + tok))
		req.Header.Set("Authorization", "Basic "+auth)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint → %d", resp.StatusCode)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint returned empty token")
}

func shortDigest(d string) string {
	// Digests look like "sha256:abc123..." — show enough to be diff-readable
	// in logs without filling the line.
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) >= i+13 {
		return d[:i+13]
	}
	return d
}
