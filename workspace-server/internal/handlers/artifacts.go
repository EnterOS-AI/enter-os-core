package handlers

// ArtifactsHandler exposes the Cloudflare Artifacts demo integration.
//
// Routes (all behind WorkspaceAuth middleware):
//
//	POST   /workspaces/:id/artifacts          — attach a CF Artifacts repo to this workspace
//	GET    /workspaces/:id/artifacts          — get the linked repo info
//	POST   /workspaces/:id/artifacts/fork     — fork this workspace's repo
//	POST   /workspaces/:id/artifacts/token    — mint a short-lived git credential
//
// Configuration (env vars, loaded once at platform startup):
//
//	CF_ARTIFACTS_API_TOKEN  — Cloudflare API token with Artifacts write permissions
//	CF_ARTIFACTS_NAMESPACE  — Cloudflare Artifacts namespace name
//
// When either env var is absent the handler returns 503 with a clear message so
// callers know the feature is not yet configured (private beta onboarding).
//
// See: https://developers.cloudflare.com/artifacts/

import (
	"database/sql"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/artifacts"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// repoNameRE validates CF Artifacts repo names: start with alphanumeric,
// then up to 62 alphanumeric/hyphen/underscore chars (63 total max).
var repoNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

// cfErrMessage returns a safe error message for CF API errors.
// For CF 5xx errors (or non-CF errors), returns a generic "upstream service error"
// to avoid leaking internal CF error details to clients.
func cfErrMessage(err error) string {
	apiErr, ok := err.(*artifacts.APIError)
	if !ok || apiErr.StatusCode >= 500 {
		return "upstream service error"
	}
	return apiErr.Message
}

// ArtifactsHandler holds a pre-built CF Artifacts client.
// The client is nil when CF_ARTIFACTS_API_TOKEN / CF_ARTIFACTS_NAMESPACE are unset.
type ArtifactsHandler struct {
	client    *artifacts.Client
	namespace string
}

// NewArtifactsHandler reads CF_ARTIFACTS_API_TOKEN and CF_ARTIFACTS_NAMESPACE
// from the environment and builds the client. If either is absent the handler
// still registers — every method simply returns 503.
func NewArtifactsHandler() *ArtifactsHandler {
	token := os.Getenv("CF_ARTIFACTS_API_TOKEN")
	ns := os.Getenv("CF_ARTIFACTS_NAMESPACE")
	if token == "" || ns == "" {
		log.Printf("artifacts: CF_ARTIFACTS_API_TOKEN or CF_ARTIFACTS_NAMESPACE not set — demo endpoints will return 503")
		return &ArtifactsHandler{}
	}
	return &ArtifactsHandler{
		client:    artifacts.New(token, ns),
		namespace: ns,
	}
}

// newArtifactsHandlerWithClient is the injectable constructor used in tests.
func newArtifactsHandlerWithClient(client *artifacts.Client, namespace string) *ArtifactsHandler {
	return &ArtifactsHandler{client: client, namespace: namespace}
}

// configured returns false (and writes a 503) when the CF client is missing.
func (h *ArtifactsHandler) configured(c *gin.Context) bool {
	if h.client == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Cloudflare Artifacts not configured — set CF_ARTIFACTS_API_TOKEN and CF_ARTIFACTS_NAMESPACE",
		})
		return false
	}
	return true
}

// ---- POST /workspaces/:id/artifacts ------------------------------------

// createArtifactsRepoRequest is the body for attaching/creating a CF Artifacts repo.
type createArtifactsRepoRequest struct {
	// Name is the desired CF repo name. Defaults to "molecule-ws-<workspace_id>" when empty.
	Name string `json:"name"`
	// Description is an optional label stored in CF and in the local DB.
	Description string `json:"description"`
	// ImportURL, when non-empty, bootstraps the repo from an existing Git URL
	// (e.g. "https://github.com/org/repo.git") instead of creating an empty repo.
	ImportURL string `json:"import_url"`
	// ImportBranch restricts the import to a single branch (only used with ImportURL).
	ImportBranch string `json:"import_branch"`
	// ImportDepth sets a shallow-clone depth for the import (0 = full history).
	ImportDepth int `json:"import_depth"`
	// ReadOnly marks the new repo as fetch/clone-only.
	ReadOnly bool `json:"read_only"`
}

// workspaceArtifactRow is the DB row shape returned by queries.
type workspaceArtifactRow struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	CFRepoName  string    `json:"cf_repo_name"`
	CFNamespace string    `json:"cf_namespace"`
	RemoteURL   string    `json:"remote_url,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Create handles POST /workspaces/:id/artifacts.
// Creates or imports a Cloudflare Artifacts repo and links it to the workspace.
// Returns 409 if a repo is already linked.
func (h *ArtifactsHandler) Create(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Reject if already linked.
	var exists bool
	db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspace_artifacts WHERE workspace_id = $1)`,
		workspaceID,
	).Scan(&exists)
	if exists {
		c.JSON(http.StatusConflict, gin.H{"error": "workspace already has a linked Artifacts repo — delete it first"})
		return
	}

	var req createArtifactsRepoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Default repo name: "molecule-ws-<workspace_id>" (truncated at 63 chars).
	repoName := req.Name
	if repoName == "" {
		repoName = "molecule-ws-" + workspaceID
		if len(repoName) > 63 {
			repoName = repoName[:63]
		}
	}

	// Validate explicit repo names; auto-generated names are always safe.
	if req.Name != "" && !repoNameRE.MatchString(req.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repo name must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"})
		return
	}

	var (
		repo *artifacts.Repo
		err  error
	)
	if req.ImportURL != "" {
		// Fix 1: require HTTPS for import URLs to prevent SSRF via non-HTTPS schemes.
		if !strings.HasPrefix(req.ImportURL, "https://") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "import_url must use https://"})
			return
		}
		repo, err = h.client.ImportRepo(ctx, repoName, artifacts.ImportRepoRequest{
			URL:      req.ImportURL,
			Branch:   req.ImportBranch,
			Depth:    req.ImportDepth,
			ReadOnly: req.ReadOnly,
		})
	} else {
		repo, err = h.client.CreateRepo(ctx, artifacts.CreateRepoRequest{
			Name:        repoName,
			Description: req.Description,
			ReadOnly:    req.ReadOnly,
		})
	}
	if err != nil {
		log.Printf("artifacts: CreateRepo/ImportRepo failed for workspace %s: %v", workspaceID, err)
		c.JSON(cfErrToHTTP(err), gin.H{"error": cfErrMessage(err)})
		return
	}

	// Strip the embedded credential from the URL before persisting.
	remoteURL := stripCredentials(repo.RemoteURL)

	var row workspaceArtifactRow
	err = db.DB.QueryRowContext(ctx, `
		INSERT INTO workspace_artifacts
			(workspace_id, cf_repo_name, cf_namespace, remote_url, description)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, workspace_id, cf_repo_name, cf_namespace, remote_url, description, created_at, updated_at
	`, workspaceID, repo.Name, h.namespace, remoteURL, req.Description).Scan(
		&row.ID, &row.WorkspaceID, &row.CFRepoName, &row.CFNamespace,
		&row.RemoteURL, &row.Description, &row.CreatedAt, &row.UpdatedAt,
	)
	if err != nil {
		log.Printf("artifacts: DB insert failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to persist artifact link"})
		return
	}

	c.JSON(http.StatusCreated, row)
}

// ---- GET /workspaces/:id/artifacts -------------------------------------

// Get handles GET /workspaces/:id/artifacts.
// Returns the linked Cloudflare Artifacts repo info from local DB and CF API.
func (h *ArtifactsHandler) Get(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	var row workspaceArtifactRow
	err := db.DB.QueryRowContext(ctx, `
		SELECT id, workspace_id, cf_repo_name, cf_namespace, remote_url, description, created_at, updated_at
		FROM workspace_artifacts
		WHERE workspace_id = $1
	`, workspaceID).Scan(
		&row.ID, &row.WorkspaceID, &row.CFRepoName, &row.CFNamespace,
		&row.RemoteURL, &row.Description, &row.CreatedAt, &row.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no Artifacts repo linked to this workspace"})
		return
	}
	if err != nil {
		log.Printf("artifacts: DB query failed for workspace %s: %v", workspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	// Augment with live info from CF API (remote URL may have changed, etc.).
	cfRepo, err := h.client.GetRepo(ctx, row.CFRepoName)
	if err != nil {
		// CF API unavailable — return cached DB row with a warning.
		log.Printf("artifacts: GetRepo from CF failed for %s: %v", row.CFRepoName, err)
		c.JSON(http.StatusOK, gin.H{
			"artifact": row,
			"cf_status": "unavailable",
			"cf_error":  cfErrMessage(err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"artifact":  row,
		"cf_repo":   cfRepo,
		"cf_status": "ok",
	})
}

// ---- POST /workspaces/:id/artifacts/fork -------------------------------

// forkArtifactsRepoRequest is the body for forking a workspace's repo.
type forkArtifactsRepoRequest struct {
	// Name is the desired name of the forked repo. Required.
	Name string `json:"name" binding:"required"`
	// Description is an optional label for the fork.
	Description string `json:"description"`
	// ReadOnly marks the fork as fetch/clone-only.
	ReadOnly bool `json:"read_only"`
	// DefaultBranchOnly limits the fork to the default branch.
	DefaultBranchOnly bool `json:"default_branch_only"`
}

// Fork handles POST /workspaces/:id/artifacts/fork.
// Creates an isolated copy of the workspace's primary Artifacts repo in CF.
// The fork is not recorded in the local DB — it is owned by the caller.
func (h *ArtifactsHandler) Fork(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Look up the source repo name.
	var cfRepoName string
	err := db.DB.QueryRowContext(ctx,
		`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&cfRepoName)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no Artifacts repo linked to this workspace"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	var req forkArtifactsRepoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	if req.Name != "" && !repoNameRE.MatchString(req.Name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repo name must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"})
		return
	}

	result, err := h.client.ForkRepo(ctx, cfRepoName, artifacts.ForkRepoRequest{
		Name:              req.Name,
		Description:       req.Description,
		ReadOnly:          req.ReadOnly,
		DefaultBranchOnly: req.DefaultBranchOnly,
	})
	if err != nil {
		log.Printf("artifacts: ForkRepo failed for workspace %s: %v", workspaceID, err)
		c.JSON(cfErrToHTTP(err), gin.H{"error": cfErrMessage(err)})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"fork":         result.Repo,
		"object_count": result.ObjectCount,
		"remote_url":   stripCredentials(result.Repo.RemoteURL),
	})
}

// ---- POST /workspaces/:id/artifacts/token ------------------------------

// artifactsTokenRequest is the body for minting a git credential.
type artifactsTokenRequest struct {
	// Scope is "read" or "write". Defaults to "write".
	Scope string `json:"scope"`
	// TTL is the credential lifetime in seconds. Defaults to 3600 (1h).
	TTL int `json:"ttl"`
}

// Token handles POST /workspaces/:id/artifacts/token.
// Returns a short-lived Git credential for the workspace's linked repo.
// The plaintext token value must be saved by the caller — it is not stored.
func (h *ArtifactsHandler) Token(c *gin.Context) {
	if !h.configured(c) {
		return
	}
	workspaceID := c.Param("id")
	ctx := c.Request.Context()

	// Look up the linked CF repo name.
	var cfRepoName string
	err := db.DB.QueryRowContext(ctx,
		`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&cfRepoName)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "no Artifacts repo linked to this workspace"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
		return
	}

	var req artifactsTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	scope := req.Scope
	if scope == "" {
		scope = "write"
	}
	if scope != "read" && scope != "write" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "scope must be \"read\" or \"write\""})
		return
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = 3600
	}
	const maxTTL = 86400 * 7 // 7 days
	if ttl > maxTTL {
		ttl = maxTTL
	}

	tok, err := h.client.CreateToken(ctx, artifacts.CreateTokenRequest{
		Repo:  cfRepoName,
		Scope: scope,
		TTL:   ttl,
	})
	if err != nil {
		log.Printf("artifacts: CreateToken failed for workspace %s: %v", workspaceID, err)
		c.JSON(cfErrToHTTP(err), gin.H{"error": cfErrMessage(err)})
		return
	}

	// Build the authenticated git remote URL inline so callers can use it
	// directly: git clone <clone_url>
	cloneURL := buildCloneURL(cfRepoName, tok.Token, h.namespace)

	c.JSON(http.StatusCreated, gin.H{
		"token_id":   tok.ID,
		"token":      tok.Token,
		"scope":      tok.Scope,
		"expires_at": tok.ExpiresAt,
		"clone_url":  cloneURL,
		"message":    "Save this token — it cannot be retrieved again.",
	})
}

// ---- helpers -------------------------------------------------------------

// cfErrToHTTP converts a CF API error to an appropriate HTTP status code.
// Passes through 4xx, maps everything else to 502 (bad gateway — upstream CF).
func cfErrToHTTP(err error) int {
	apiErr, ok := err.(*artifacts.APIError)
	if !ok {
		return http.StatusBadGateway
	}
	if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
		return apiErr.StatusCode
	}
	return http.StatusBadGateway
}

// stripCredentials removes "x:<token>@" from an authenticated git remote URL
// so we never persist credentials in the database.
// e.g. "https://x:tok@hash.artifacts.cloudflare.net/…" → "https://hash.artifacts.cloudflare.net/…"
func stripCredentials(remoteURL string) string {
	if i := strings.Index(remoteURL, "@"); i != -1 {
		scheme := "https://"
		if strings.HasPrefix(remoteURL, "http://") {
			scheme = "http://"
		}
		return scheme + remoteURL[i+1:]
	}
	return remoteURL
}

// buildCloneURL constructs an authenticated clone URL from the CF token.
// Format: https://x:<token>@<hash>.artifacts.cloudflare.net/git/repo-<name>.git
// When we only have the repo name (not the full hashed host), we use a stable
// URL pattern that the CF git endpoint resolves.
func buildCloneURL(repoName, token, _ string) string {
	// The CF git endpoint is the remote_url stored in the DB (minus the
	// credential prefix). We reconstruct the authenticated form here.
	// In production the remote URL is returned by CreateRepo/GetRepo;
	// this fallback covers cases where the DB row predates that field.
	return "https://x:" + token + "@artifacts.cloudflare.net/git/" + repoName + ".git"
}
