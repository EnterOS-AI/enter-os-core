package pgplugin

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/memory/contract"
)

// SchemaVersion is what the plugin reports on /v1/health. Pinned to
// the contract package so a contract bump auto-bumps the plugin.
var SchemaVersion = contract.SchemaVersion

// Capabilities the built-in postgres plugin advertises. workspace-
// server's MCP layer keys feature exposure off this list; bumping
// any item here is a behavior change.
var Capabilities = []string{
	contract.CapabilityFTS,
	contract.CapabilityEmbedding,
	contract.CapabilityTTL,
	contract.CapabilityPin,
	contract.CapabilityPropagation,
}

// Handler is the HTTP layer for the plugin. Wires URL routing in its
// ServeHTTP method (no third-party router — keeps the plugin's
// dependency surface minimal). The route table is small enough that a
// single switch reads better than a mux.
type Handler struct {
	store    *Store
	pingDB   func() error // injectable for /v1/health degraded probe
	versionFn func() string
	capsFn    func() []string
}

// NewHandler wires up an HTTP handler against the given store. The
// pingDB callback is hit on every /v1/health to confirm the backing
// store is alive — a cached "ok" would mask connection-pool failures.
func NewHandler(store *Store, pingDB func() error) *Handler {
	return &Handler{
		store:     store,
		pingDB:    pingDB,
		versionFn: func() string { return SchemaVersion },
		capsFn:    func() []string { return Capabilities },
	}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/health" && r.Method == http.MethodGet:
		h.health(w, r)
	case r.URL.Path == "/v1/search" && r.Method == http.MethodPost:
		h.search(w, r)

	case strings.HasPrefix(r.URL.Path, "/v1/memories/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/v1/memories/")
		h.forget(w, r, id)

	case strings.HasPrefix(r.URL.Path, "/v1/namespaces/"):
		h.namespaceRoutes(w, r)

	default:
		writeError(w, http.StatusNotFound, contract.ErrorCodeNotFound, "no route", nil)
	}
}

func (h *Handler) namespaceRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/namespaces/")
	if rest == "" {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "namespace name missing", nil)
		return
	}
	// "{name}/memories" → memories endpoint
	if i := strings.Index(rest, "/"); i >= 0 {
		name := rest[:i]
		sub := rest[i+1:]
		if sub == "memories" && r.Method == http.MethodPost {
			h.commit(w, r, name)
			return
		}
		writeError(w, http.StatusNotFound, contract.ErrorCodeNotFound, "no route", nil)
		return
	}
	// "{name}" → namespace CRUD
	name := rest
	switch r.Method {
	case http.MethodPut:
		h.upsertNamespace(w, r, name)
	case http.MethodPatch:
		h.patchNamespace(w, r, name)
	case http.MethodDelete:
		h.deleteNamespace(w, r, name)
	default:
		writeError(w, http.StatusMethodNotAllowed, contract.ErrorCodeBadRequest, "method not allowed", nil)
	}
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	status := "ok"
	if h.pingDB != nil {
		if err := h.pingDB(); err != nil {
			status = "degraded"
			writeJSON(w, http.StatusServiceUnavailable, contract.HealthResponse{
				Status: status, Version: h.versionFn(), Capabilities: h.capsFn(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, contract.HealthResponse{
		Status: status, Version: h.versionFn(), Capabilities: h.capsFn(),
	})
}

func (h *Handler) upsertNamespace(w http.ResponseWriter, r *http.Request, name string) {
	if err := contract.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	var body contract.NamespaceUpsert
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "invalid JSON", nil)
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	ns, err := h.store.UpsertNamespace(r.Context(), name, body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, contract.ErrorCodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (h *Handler) patchNamespace(w http.ResponseWriter, r *http.Request, name string) {
	if err := contract.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	var body contract.NamespacePatch
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "invalid JSON", nil)
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	ns, err := h.store.PatchNamespace(r.Context(), name, body)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, contract.ErrorCodeNotFound, "namespace not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, contract.ErrorCodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, ns)
}

func (h *Handler) deleteNamespace(w http.ResponseWriter, r *http.Request, name string) {
	if err := contract.ValidateNamespaceName(name); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	if err := h.store.DeleteNamespace(r.Context(), name); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, contract.ErrorCodeNotFound, "namespace not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, contract.ErrorCodeInternal, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) commit(w http.ResponseWriter, r *http.Request, namespace string) {
	if err := contract.ValidateNamespaceName(namespace); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	var body contract.MemoryWrite
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "invalid JSON", nil)
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	resp, err := h.store.CommitMemory(r.Context(), namespace, body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, contract.ErrorCodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	var body contract.SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "invalid JSON", nil)
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	resp, err := h.store.Search(r.Context(), body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, contract.ErrorCodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) forget(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "memory id missing", nil)
		return
	}
	var body contract.ForgetRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, "invalid JSON", nil)
		return
	}
	if err := body.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, contract.ErrorCodeBadRequest, err.Error(), nil)
		return
	}
	if err := h.store.ForgetMemory(r.Context(), id, body.RequestedByNamespace); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, contract.ErrorCodeNotFound, "memory not found in namespace", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, contract.ErrorCodeInternal, err.Error(), nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("pgplugin: JSON encode error: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, code contract.ErrorCode, message string, details map[string]interface{}) {
	writeJSON(w, status, contract.Error{Code: code, Message: message, Details: details})
}
