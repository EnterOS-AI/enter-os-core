package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/artifacts"
	"github.com/gin-gonic/gin"
)

// cfSuccessResponse wraps a result in the Cloudflare v4 success envelope.
func cfSuccessResponse(t *testing.T, result interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("cfSuccessResponse: marshal result: %v", err)
	}
	env := map[string]interface{}{
		"success": true,
		"result":  json.RawMessage(b),
		"errors":  []interface{}{},
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("cfSuccessResponse: marshal envelope: %v", err)
	}
	return out
}

// cfErrorResponse returns a Cloudflare v4 error envelope bytes and status code.
func cfErrorResponse(t *testing.T, statusCode, code int, message string) ([]byte, int) {
	t.Helper()
	env := map[string]interface{}{
		"success": false,
		"result":  nil,
		"errors": []map[string]interface{}{
			{"code": code, "message": message},
		},
	}
	b, _ := json.Marshal(env)
	return b, statusCode
}

// newArtifactsMockServer starts an httptest.Server with the given handler function
// registered at /namespaces/test-ns/<suffix>.
func newArtifactsMockCFServer(t *testing.T, suffix string, handler http.HandlerFunc) *artifacts.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns"+suffix, handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return artifacts.NewWithBaseURL("cf-fixture-token", "test-ns", srv.URL)
}

// ============================= Create =====================================

// TestArtifactsCreate_Success verifies the happy path: no existing link →
// CF API returns a repo → DB INSERT succeeds → 201 response.
func TestArtifactsCreate_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	cfClient := newArtifactsMockCFServer(t, "/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		repo := artifacts.Repo{
			Name:      "molecule-ws-ws-abc",
			ID:        "repo-001",
			RemoteURL: "https://x:tok123@hash.artifacts.cloudflare.net/git/repo-001.git",
			CreatedAt: time.Now(),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, repo))
	})

	// Existence probe — no existing link
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-abc").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	// DB INSERT — RETURNING row
	now := time.Now()
	mock.ExpectQuery(`INSERT INTO workspace_artifacts`).
		WithArgs("ws-abc", "molecule-ws-ws-abc", "test-ns",
			"https://hash.artifacts.cloudflare.net/git/repo-001.git", "").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "workspace_id", "cf_repo_name", "cf_namespace", "remote_url", "description", "created_at", "updated_at"}).
			AddRow("art-1", "ws-abc", "molecule-ws-ws-abc", "test-ns",
				"https://hash.artifacts.cloudflare.net/git/repo-001.git", "", now, now))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-abc"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-abc/artifacts",
		bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["cf_repo_name"] != "molecule-ws-ws-abc" {
		t.Errorf("cf_repo_name = %v, want molecule-ws-ws-abc", resp["cf_repo_name"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsCreate_AlreadyLinked verifies that a 409 is returned when the
// workspace already has a linked Artifacts repo.
func TestArtifactsCreate_AlreadyLinked(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Existence probe returns true
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-dup").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "ns", "http://unused"),
		"ns",
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-dup"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-dup/artifacts",
		bytes.NewBufferString(`{"name":"dup-repo"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Create(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsCreate_CFAPIError verifies that a CF API error (e.g. 409 conflict)
// is forwarded with the appropriate HTTP status.
func TestArtifactsCreate_CFAPIError(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	cfClient := newArtifactsMockCFServer(t, "/repos", func(w http.ResponseWriter, r *http.Request) {
		body, status := cfErrorResponse(t, http.StatusConflict, 1009, "repo name already taken")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	})

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-cfconflict").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfconflict"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-cfconflict/artifacts",
		bytes.NewBufferString(`{"name":"taken-name"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Create(c)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 from CF error, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsCreate_WithImportURL verifies that when import_url is set the
// handler hits the /import endpoint instead of plain /repos.
func TestArtifactsCreate_WithImportURL(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// Two paths: /repos/imported-repo/import
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos/imported-repo/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		if body["url"] != "https://github.com/Molecule-AI/molecule-core.git" {
			http.Error(w, "unexpected url", http.StatusBadRequest)
			return
		}
		repo := artifacts.Repo{
			Name:      "imported-repo",
			RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/imported.git",
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, repo))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cfClient := artifacts.NewWithBaseURL("tok", "test-ns", srv.URL)

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-import").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	now := time.Now()
	mock.ExpectQuery(`INSERT INTO workspace_artifacts`).
		WithArgs("ws-import", "imported-repo", "test-ns",
			"https://hash.artifacts.cloudflare.net/git/imported.git", "Imported from GitHub").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "workspace_id", "cf_repo_name", "cf_namespace", "remote_url", "description", "created_at", "updated_at"}).
			AddRow("art-imp", "ws-import", "imported-repo", "test-ns",
				"https://hash.artifacts.cloudflare.net/git/imported.git", "Imported from GitHub", now, now))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-import"}}
	body := `{"name":"imported-repo","description":"Imported from GitHub","import_url":"https://github.com/Molecule-AI/molecule-core.git","import_branch":"main","import_depth":1}`
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-import/artifacts",
		bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsCreate_NotConfigured verifies that missing env vars → 503.
func TestArtifactsCreate_NotConfigured(t *testing.T) {
	setupTestDB(t)
	setupTestRedis(t)

	// No CF client → nil
	h := newArtifactsHandlerWithClient(nil, "")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-uncfg"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-uncfg/artifacts",
		bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Create(c)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

// ============================= Get =======================================

// TestArtifactsGet_Success verifies the happy path: DB row found + CF API ok.
func TestArtifactsGet_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	cfClient := newArtifactsMockCFServer(t, "/repos/my-ws-repo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		repo := artifacts.Repo{
			Name:      "my-ws-repo",
			RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/my-ws-repo.git",
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, repo))
	})

	now := time.Now()
	mock.ExpectQuery(`SELECT id, workspace_id, cf_repo_name`).
		WithArgs("ws-get").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "workspace_id", "cf_repo_name", "cf_namespace", "remote_url", "description", "created_at", "updated_at"}).
			AddRow("art-get", "ws-get", "my-ws-repo", "test-ns",
				"https://hash.artifacts.cloudflare.net/git/my-ws-repo.git", "", now, now))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-get"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-get/artifacts", nil)

	h.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["cf_status"] != "ok" {
		t.Errorf("cf_status = %v, want ok", resp["cf_status"])
	}
	art, ok := resp["artifact"].(map[string]interface{})
	if !ok {
		t.Fatalf("artifact is not an object")
	}
	if art["cf_repo_name"] != "my-ws-repo" {
		t.Errorf("artifact.cf_repo_name = %v, want my-ws-repo", art["cf_repo_name"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsGet_NotFound verifies that 404 is returned when no row exists.
func TestArtifactsGet_NotFound(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT id, workspace_id, cf_repo_name`).
		WithArgs("ws-noart").
		WillReturnError(sql.ErrNoRows)

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "ns", "http://unused"),
		"ns",
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-noart"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-noart/artifacts", nil)

	h.Get(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsGet_CFUnavailable verifies that when CF API fails the handler
// still returns 200 with the cached DB row and cf_status="unavailable".
func TestArtifactsGet_CFUnavailable(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	// CF API server that always returns 500
	cfClient := newArtifactsMockCFServer(t, "/repos/cached-repo", func(w http.ResponseWriter, r *http.Request) {
		body, status := cfErrorResponse(t, http.StatusInternalServerError, 0, "service unavailable")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	})

	now := time.Now()
	mock.ExpectQuery(`SELECT id, workspace_id, cf_repo_name`).
		WithArgs("ws-cfdown").
		WillReturnRows(sqlmock.NewRows(
			[]string{"id", "workspace_id", "cf_repo_name", "cf_namespace", "remote_url", "description", "created_at", "updated_at"}).
			AddRow("art-cfdown", "ws-cfdown", "cached-repo", "test-ns",
				"https://hash.artifacts.cloudflare.net/git/cached-repo.git", "", now, now))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-cfdown"}}
	c.Request = httptest.NewRequest("GET", "/workspaces/ws-cfdown/artifacts", nil)

	h.Get(c)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (degraded), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["cf_status"] != "unavailable" {
		t.Errorf("cf_status = %v, want unavailable", resp["cf_status"])
	}
	if resp["artifact"] == nil {
		t.Error("artifact should still be present from DB cache")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// ============================= Fork ======================================

// TestArtifactsFork_Success verifies the fork happy path.
func TestArtifactsFork_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	cfClient := newArtifactsMockCFServer(t, "/repos/source-repo/fork", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		result := artifacts.ForkResult{
			Repo: artifacts.Repo{
				Name:      "forked-ws",
				ID:        "fork-1",
				RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/fork-1.git",
			},
			ObjectCount: 88,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, result))
	})

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-fork-src").
		WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("source-repo"))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-fork-src"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-fork-src/artifacts/fork",
		bytes.NewBufferString(`{"name":"forked-ws"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Fork(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["object_count"] != float64(88) {
		t.Errorf("object_count = %v, want 88", resp["object_count"])
	}
	fork, ok := resp["fork"].(map[string]interface{})
	if !ok {
		t.Fatalf("fork is not an object")
	}
	if fork["name"] != "forked-ws" {
		t.Errorf("fork.name = %v, want forked-ws", fork["name"])
	}
	// Embedded credentials must be stripped from clone_url
	if remote := resp["remote_url"].(string); len(remote) > 0 {
		if containsCredentials(remote) {
			t.Errorf("remote_url should not contain credentials: %s", remote)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsFork_NoLinkedRepo verifies 404 when workspace has no linked repo.
func TestArtifactsFork_NoLinkedRepo(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-norepo").
		WillReturnError(sql.ErrNoRows)

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "ns", "http://unused"),
		"ns",
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-norepo"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-norepo/artifacts/fork",
		bytes.NewBufferString(`{"name":"fork-name"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Fork(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsFork_MissingName verifies 400 when the fork name is missing.
func TestArtifactsFork_MissingName(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-fork-badname").
		WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("src-repo"))

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "test-ns", "http://unused"),
		"test-ns",
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-fork-badname"}}
	// name is required (binding:"required") but absent → 400
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-fork-badname/artifacts/fork",
		bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Fork(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing name, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// ============================= Token =====================================

// TestArtifactsToken_Success verifies the happy path: linked repo → CF returns token.
func TestArtifactsToken_Success(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	expiry := time.Now().Add(3600 * time.Second).UTC()
	cfClient := newArtifactsMockCFServer(t, "/tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "wrong method", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		if req["repo"] != "my-linked-repo" {
			http.Error(w, "unexpected repo", http.StatusBadRequest)
			return
		}
		tok := artifacts.RepoToken{
			ID:        "token-abc",
			Token:     "plaintext-git-token",
			Scope:     "write",
			ExpiresAt: expiry,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, tok))
	})

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-token").
		WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("my-linked-repo"))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-token"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-token/artifacts/token",
		bytes.NewBufferString(`{"scope":"write","ttl":3600}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Token(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["token_id"] != "token-abc" {
		t.Errorf("token_id = %v, want token-abc", resp["token_id"])
	}
	if resp["token"] != "plaintext-git-token" {
		t.Errorf("token = %v, want plaintext-git-token", resp["token"])
	}
	if resp["clone_url"] == nil || resp["clone_url"] == "" {
		t.Error("clone_url should be non-empty")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsToken_DefaultsApplied verifies that empty scope/ttl are defaulted
// to "write" / 3600.
func TestArtifactsToken_DefaultsApplied(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	expiry := time.Now().Add(3600 * time.Second).UTC()
	cfClient := newArtifactsMockCFServer(t, "/tokens", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		// scope should be "write" (default)
		if req["scope"] != "write" {
			http.Error(w, "expected default scope write", http.StatusBadRequest)
			return
		}
		// ttl should be 3600 (default), serialized as float64 from JSON
		if req["ttl"] != float64(3600) {
			http.Error(w, "expected default ttl 3600", http.StatusBadRequest)
			return
		}
		tok := artifacts.RepoToken{ID: "t1", Token: "tok-def", Scope: "write", ExpiresAt: expiry}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, tok))
	})

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-defaults").
		WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("my-repo"))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-defaults"}}
	// Empty body — all defaults
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-defaults/artifacts/token",
		bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Token(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsToken_InvalidScope verifies that an invalid scope returns 400.
func TestArtifactsToken_InvalidScope(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-badscope").
		WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("some-repo"))

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "ns", "http://unused"),
		"ns",
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-badscope"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-badscope/artifacts/token",
		bytes.NewBufferString(`{"scope":"admin"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Token(c)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid scope, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsToken_TTLCapped verifies that excessive TTL is silently capped
// to 7 days (604800 seconds) rather than returning an error.
func TestArtifactsToken_TTLCapped(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const maxTTL = 86400 * 7

	expiry := time.Now().Add(maxTTL * time.Second).UTC()
	cfClient := newArtifactsMockCFServer(t, "/tokens", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)
		if int(req["ttl"].(float64)) != maxTTL {
			http.Error(w, "expected capped ttl", http.StatusBadRequest)
			return
		}
		tok := artifacts.RepoToken{ID: "t-cap", Token: "capped-tok", Scope: "write", ExpiresAt: expiry}
		w.Header().Set("Content-Type", "application/json")
		w.Write(cfSuccessResponse(t, tok))
	})

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-ttlcap").
		WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("capped-repo"))

	h := newArtifactsHandlerWithClient(cfClient, "test-ns")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-ttlcap"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-ttlcap/artifacts/token",
		bytes.NewBufferString(`{"scope":"write","ttl":99999999}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Token(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// TestArtifactsToken_NoLinkedRepo verifies 404 when no repo is linked.
func TestArtifactsToken_NoLinkedRepo(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
		WithArgs("ws-tokennolink").
		WillReturnError(sql.ErrNoRows)

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "ns", "http://unused"),
		"ns",
	)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "ws-tokennolink"}}
	c.Request = httptest.NewRequest("POST", "/workspaces/ws-tokennolink/artifacts/token",
		bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.Token(c)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

// ============================= helper unit tests =========================

// TestStripCredentials verifies that stripCredentials removes user:token@ from URLs.
func TestStripCredentials(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{
			"https://x:tok123@hash.artifacts.cloudflare.net/git/repo.git",
			"https://hash.artifacts.cloudflare.net/git/repo.git",
		},
		{
			"https://hash.artifacts.cloudflare.net/git/repo.git",
			"https://hash.artifacts.cloudflare.net/git/repo.git",
		},
		{
			"http://user:pass@example.com/repo.git",
			"http://example.com/repo.git",
		},
		{"", ""},
	}
	for _, tc := range cases {
		got := stripCredentials(tc.input)
		if got != tc.want {
			t.Errorf("stripCredentials(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestCfErrToHTTP verifies the CF-error-to-HTTP-status mapping.
func TestCfErrToHTTP(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{&artifacts.APIError{StatusCode: http.StatusConflict}, http.StatusConflict},
		{&artifacts.APIError{StatusCode: http.StatusNotFound}, http.StatusNotFound},
		{&artifacts.APIError{StatusCode: http.StatusBadRequest}, http.StatusBadRequest},
		{&artifacts.APIError{StatusCode: http.StatusInternalServerError}, http.StatusBadGateway},
		{&artifacts.APIError{StatusCode: http.StatusBadGateway}, http.StatusBadGateway},
	}
	for _, tc := range cases {
		got := cfErrToHTTP(tc.err)
		if got != tc.want {
			t.Errorf("cfErrToHTTP(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// ============================= Security fix tests ============================

// TestCfErrMessage_5xxReturnsGeneric verifies that CF 5xx errors return a
// generic message instead of leaking CF internals.
func TestCfErrMessage_5xxReturnsGeneric(t *testing.T) {
	err := &artifacts.APIError{StatusCode: http.StatusInternalServerError, Message: "internal CF detail"}
	got := cfErrMessage(err)
	if got != "upstream service error" {
		t.Errorf("cfErrMessage(500) = %q, want %q", got, "upstream service error")
	}
}

// TestCfErrMessage_502ReturnsGeneric verifies that CF 502 (bad gateway) is also masked.
func TestCfErrMessage_502ReturnsGeneric(t *testing.T) {
	err := &artifacts.APIError{StatusCode: http.StatusBadGateway, Message: "gateway detail"}
	got := cfErrMessage(err)
	if got != "upstream service error" {
		t.Errorf("cfErrMessage(502) = %q, want %q", got, "upstream service error")
	}
}

// TestCfErrMessage_4xxPassesThrough verifies that CF 4xx messages are surfaced.
func TestCfErrMessage_4xxPassesThrough(t *testing.T) {
	msg := "repo name already taken"
	err := &artifacts.APIError{StatusCode: http.StatusConflict, Message: msg}
	got := cfErrMessage(err)
	if got != msg {
		t.Errorf("cfErrMessage(409) = %q, want %q", got, msg)
	}
}

// TestCfErrMessage_NonAPIErrorReturnsGeneric verifies that non-CF errors return generic message.
func TestCfErrMessage_NonAPIErrorReturnsGeneric(t *testing.T) {
	err := fmt.Errorf("some network error")
	got := cfErrMessage(err)
	if got != "upstream service error" {
		t.Errorf("cfErrMessage(non-API) = %q, want %q", got, "upstream service error")
	}
}

// TestArtifactsCreate_ImportURLNonHTTPS verifies that a non-HTTPS import_url
// is rejected with 400.
func TestArtifactsCreate_ImportURLNonHTTPS(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("ws-badurl").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "test-ns", "http://unused"),
		"test-ns",
	)

	cases := []string{
		"http://github.com/org/repo.git",
		"git://github.com/org/repo.git",
		"ssh://git@github.com/org/repo.git",
		"file:///etc/passwd",
	}
	for _, url := range cases {
		t.Run(url, func(t *testing.T) {
			// Re-register the EXISTS probe expectation for each sub-test case.
			mock.ExpectQuery(`SELECT EXISTS`).
				WithArgs("ws-badurl").
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-badurl"}}
			body, _ := json.Marshal(map[string]interface{}{
				"name":       "my-repo",
				"import_url": url,
			})
			c.Request = httptest.NewRequest("POST", "/workspaces/ws-badurl/artifacts",
				bytes.NewBuffer(body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Create(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("import_url=%q: expected 400, got %d: %s", url, w.Code, w.Body.String())
			}
		})
	}
}

// TestArtifactsCreate_InvalidRepoName verifies that invalid repo names return 400.
func TestArtifactsCreate_InvalidRepoName(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "test-ns", "http://unused"),
		"test-ns",
	)

	invalidNames := []string{
		"-starts-with-dash",
		"_starts-with-underscore",
		"has spaces",
		"has/slash",
		"has.dot",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 64 chars
	}
	for _, name := range invalidNames {
		t.Run(name, func(t *testing.T) {
			mock.ExpectQuery(`SELECT EXISTS`).
				WithArgs("ws-badname").
				WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-badname"}}
			body, _ := json.Marshal(map[string]interface{}{"name": name})
			c.Request = httptest.NewRequest("POST", "/workspaces/ws-badname/artifacts",
				bytes.NewBuffer(body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Create(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("name=%q: expected 400, got %d: %s", name, w.Code, w.Body.String())
			}
		})
	}
}

// TestArtifactsFork_InvalidRepoName verifies that invalid fork names return 400.
func TestArtifactsFork_InvalidRepoName(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	h := newArtifactsHandlerWithClient(
		artifacts.NewWithBaseURL("tok", "test-ns", "http://unused"),
		"test-ns",
	)

	invalidNames := []string{
		"-bad-start",
		"has spaces",
		"../traversal",
	}
	for _, name := range invalidNames {
		t.Run(name, func(t *testing.T) {
			mock.ExpectQuery(`SELECT cf_repo_name FROM workspace_artifacts WHERE workspace_id`).
				WithArgs("ws-forknm").
				WillReturnRows(sqlmock.NewRows([]string{"cf_repo_name"}).AddRow("src"))

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "ws-forknm"}}
			body, _ := json.Marshal(map[string]interface{}{"name": name})
			c.Request = httptest.NewRequest("POST", "/workspaces/ws-forknm/artifacts/fork",
				bytes.NewBuffer(body))
			c.Request.Header.Set("Content-Type", "application/json")

			h.Fork(c)

			if w.Code != http.StatusBadRequest {
				t.Errorf("fork name=%q: expected 400, got %d: %s", name, w.Code, w.Body.String())
			}
		})
	}
}

// containsCredentials is a test helper that checks whether a URL has embedded
// user:password@ credentials (should never appear in a stored remote URL).
func containsCredentials(u string) bool {
	// A URL with embedded creds has the form scheme://user:pass@host/...
	// We check for "@" after the scheme to detect this.
	for i := 0; i < len(u)-3; i++ {
		if u[i] == ':' && i > 0 && u[i-1] != '/' {
			// Found ":" that is not ":/" — could be user:pass pair
			if j := len(u); j > i {
				for k := i + 1; k < j; k++ {
					if u[k] == '@' {
						return true
					}
					if u[k] == '/' {
						break
					}
				}
			}
		}
	}
	return false
}
