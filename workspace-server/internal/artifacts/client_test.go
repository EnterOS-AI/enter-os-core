package artifacts_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/artifacts"
)

// cfEnvelope wraps a result value in the Cloudflare v4 response envelope.
func cfEnvelope(t *testing.T, result interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("cfEnvelope: marshal result: %v", err)
	}
	env := map[string]interface{}{
		"success": true,
		"result":  json.RawMessage(b),
		"errors":  []interface{}{},
	}
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("cfEnvelope: marshal envelope: %v", err)
	}
	return out
}

// cfError returns a Cloudflare v4 error envelope.
func cfError(t *testing.T, statusCode, code int, message string) ([]byte, int) {
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

func newTestClient(t *testing.T, mux *http.ServeMux) *artifacts.Client {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return artifacts.NewWithBaseURL("fixture-token", "test-ns", srv.URL)
}

// ---- CreateRepo ----------------------------------------------------------

func TestCreateRepo_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Verify auth header
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Decode request body
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req["name"] != "my-workspace-repo" {
			http.Error(w, "unexpected name", http.StatusBadRequest)
			return
		}

		repo := artifacts.Repo{
			Name:      "my-workspace-repo",
			ID:        "repo-abc123",
			RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/repo-abc123.git",
			CreatedAt: time.Now(),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cfEnvelope(t, repo))
	})

	client := newTestClient(t, mux)
	repo, err := client.CreateRepo(context.Background(), artifacts.CreateRepoRequest{
		Name:        "my-workspace-repo",
		Description: "Molecule AI workspace snapshot",
	})
	if err != nil {
		t.Fatalf("CreateRepo: unexpected error: %v", err)
	}
	if repo.Name != "my-workspace-repo" {
		t.Errorf("repo.Name = %q, want %q", repo.Name, "my-workspace-repo")
	}
	if repo.ID != "repo-abc123" {
		t.Errorf("repo.ID = %q, want %q", repo.ID, "repo-abc123")
	}
	if repo.RemoteURL == "" {
		t.Error("repo.RemoteURL is empty, want non-empty")
	}
}

func TestCreateRepo_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos", func(w http.ResponseWriter, r *http.Request) {
		body, status := cfError(t, http.StatusConflict, 1009, "repo already exists")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})

	client := newTestClient(t, mux)
	_, err := client.CreateRepo(context.Background(), artifacts.CreateRepoRequest{Name: "dup"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*artifacts.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusConflict)
	}
	if apiErr.Message != "repo already exists" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "repo already exists")
	}
}

// ---- GetRepo -------------------------------------------------------------

func TestGetRepo_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos/my-repo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		repo := artifacts.Repo{
			Name:      "my-repo",
			ID:        "repo-xyz",
			RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/repo-xyz.git",
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cfEnvelope(t, repo))
	})

	client := newTestClient(t, mux)
	repo, err := client.GetRepo(context.Background(), "my-repo")
	if err != nil {
		t.Fatalf("GetRepo: unexpected error: %v", err)
	}
	if repo.Name != "my-repo" {
		t.Errorf("repo.Name = %q, want %q", repo.Name, "my-repo")
	}
}

func TestGetRepo_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos/missing", func(w http.ResponseWriter, r *http.Request) {
		body, status := cfError(t, http.StatusNotFound, 1004, "repo not found")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})

	client := newTestClient(t, mux)
	_, err := client.GetRepo(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*artifacts.APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusNotFound)
	}
}

// ---- ForkRepo ------------------------------------------------------------

func TestForkRepo_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos/source-repo/fork", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["name"] != "forked-repo" {
			http.Error(w, "unexpected fork name", http.StatusBadRequest)
			return
		}
		result := artifacts.ForkResult{
			Repo: artifacts.Repo{
				Name:      "forked-repo",
				ID:        "repo-fork-1",
				RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/repo-fork-1.git",
			},
			ObjectCount: 42,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cfEnvelope(t, result))
	})

	client := newTestClient(t, mux)
	result, err := client.ForkRepo(context.Background(), "source-repo", artifacts.ForkRepoRequest{
		Name: "forked-repo",
	})
	if err != nil {
		t.Fatalf("ForkRepo: unexpected error: %v", err)
	}
	if result.Repo.Name != "forked-repo" {
		t.Errorf("Repo.Name = %q, want %q", result.Repo.Name, "forked-repo")
	}
	if result.ObjectCount != 42 {
		t.Errorf("ObjectCount = %d, want 42", result.ObjectCount)
	}
}

// ---- ImportRepo ----------------------------------------------------------

func TestImportRepo_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos/imported/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["url"] == "" {
			http.Error(w, "url required", http.StatusBadRequest)
			return
		}
		repo := artifacts.Repo{
			Name:      "imported",
			ID:        "repo-imp-1",
			RemoteURL: "https://x:tok@hash.artifacts.cloudflare.net/git/repo-imp-1.git",
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cfEnvelope(t, repo))
	})

	client := newTestClient(t, mux)
	repo, err := client.ImportRepo(context.Background(), "imported", artifacts.ImportRepoRequest{
		URL:    "https://github.com/Molecule-AI/molecule-core.git",
		Branch: "main",
		Depth:  1,
	})
	if err != nil {
		t.Fatalf("ImportRepo: unexpected error: %v", err)
	}
	if repo.Name != "imported" {
		t.Errorf("repo.Name = %q, want %q", repo.Name, "imported")
	}
}

// ---- DeleteRepo ----------------------------------------------------------

func TestDeleteRepo_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos/to-delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		deleted := map[string]string{"id": "repo-del-1"}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(cfEnvelope(t, deleted))
	})

	client := newTestClient(t, mux)
	if err := client.DeleteRepo(context.Background(), "to-delete"); err != nil {
		t.Fatalf("DeleteRepo: unexpected error: %v", err)
	}
}

// ---- CreateToken ---------------------------------------------------------

func TestCreateToken_Success(t *testing.T) {
	expiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["repo"] != "my-repo" {
			http.Error(w, "unexpected repo", http.StatusBadRequest)
			return
		}
		tok := artifacts.RepoToken{
			ID:        "tok-123",
			Token:     "plaintext-secret-abc",
			Scope:     "write",
			ExpiresAt: expiry,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cfEnvelope(t, tok))
	})

	client := newTestClient(t, mux)
	tok, err := client.CreateToken(context.Background(), artifacts.CreateTokenRequest{
		Repo:  "my-repo",
		Scope: "write",
		TTL:   86400,
	})
	if err != nil {
		t.Fatalf("CreateToken: unexpected error: %v", err)
	}
	if tok.ID != "tok-123" {
		t.Errorf("ID = %q, want %q", tok.ID, "tok-123")
	}
	if tok.Token != "plaintext-secret-abc" {
		t.Errorf("Token = %q, want %q", tok.Token, "plaintext-secret-abc")
	}
	if tok.Scope != "write" {
		t.Errorf("Scope = %q, want %q", tok.Scope, "write")
	}
}

// ---- RevokeToken ---------------------------------------------------------

func TestRevokeToken_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/tokens/tok-456", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		deleted := map[string]string{"id": "tok-456"}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cfEnvelope(t, deleted))
	})

	client := newTestClient(t, mux)
	if err := client.RevokeToken(context.Background(), "tok-456"); err != nil {
		t.Fatalf("RevokeToken: unexpected error: %v", err)
	}
}

// ---- Context cancellation ------------------------------------------------

func TestCreateRepo_ContextCancelled(t *testing.T) {
	// Server that never responds (simulates a hung connection)
	mux := http.NewServeMux()
	mux.HandleFunc("/namespaces/test-ns/repos", func(w http.ResponseWriter, r *http.Request) {
		// Block until the client gives up
		<-r.Context().Done()
	})

	client := newTestClient(t, mux)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := client.CreateRepo(ctx, artifacts.CreateRepoRequest{Name: "x"})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}
