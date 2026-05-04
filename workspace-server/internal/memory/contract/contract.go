// Package contract holds the typed Go bindings for the Memory Plugin v1
// HTTP contract defined at docs/api-protocol/memory-plugin-v1.yaml.
//
// These types are the wire shape between workspace-server (the only
// sanctioned client) and any memory plugin implementation. They are
// kept in their own package so the plugin client (PR-2) and the
// built-in postgres plugin server (PR-3) share a single source of
// truth for JSON tags and validation rules.
//
// Validation lives next to the types via the Validate() methods so
// every wire object self-checks; PR-2's HTTP client and PR-3's HTTP
// server both call Validate() at the boundary.
package contract

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// SchemaVersion pins the contract revision the workspace-server expects
// from /v1/health responses. Bump in lockstep with the OpenAPI spec.
const SchemaVersion = "1.0.0"

// Capability strings reported by /v1/health. Plugins MAY report any
// subset; workspace-server gates feature exposure on what's reported.
const (
	CapabilityEmbedding   = "embedding"
	CapabilityFTS         = "fts"
	CapabilityTTL         = "ttl"
	CapabilityPin         = "pin"
	CapabilityPropagation = "propagation"
)

// NamespaceKind enumerates the four namespace shapes workspace-server
// derives from the team tree. `custom` is reserved for operator-defined
// cross-workspace channels.
type NamespaceKind string

const (
	NamespaceKindWorkspace NamespaceKind = "workspace"
	NamespaceKindTeam      NamespaceKind = "team"
	NamespaceKindOrg       NamespaceKind = "org"
	NamespaceKindCustom    NamespaceKind = "custom"
)

// MemoryKind distinguishes facts (point-in-time observations), summaries
// (compressed multi-fact rollups), and checkpoints (durable state
// markers between sessions).
type MemoryKind string

const (
	MemoryKindFact       MemoryKind = "fact"
	MemoryKindSummary    MemoryKind = "summary"
	MemoryKindCheckpoint MemoryKind = "checkpoint"
)

// MemorySource records who wrote a memory: the agent itself, the
// workspace runtime (e.g., end-of-session auto-summary), or the user
// (canvas-side input).
type MemorySource string

const (
	MemorySourceAgent   MemorySource = "agent"
	MemorySourceRuntime MemorySource = "runtime"
	MemorySourceUser    MemorySource = "user"
)

// ErrorCode enumerates the wire error codes plugins return.
type ErrorCode string

const (
	ErrorCodeBadRequest  ErrorCode = "bad_request"
	ErrorCodeNotFound    ErrorCode = "not_found"
	ErrorCodeForbidden   ErrorCode = "forbidden"
	ErrorCodeInternal    ErrorCode = "internal"
	ErrorCodeUnavailable ErrorCode = "unavailable"
)

// HealthResponse is the body of GET /v1/health.
type HealthResponse struct {
	Status       string   `json:"status"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

// HasCapability reports whether the plugin advertises the named
// capability. Tolerant of nil receivers so callers can probe before
// the health check completes.
func (h *HealthResponse) HasCapability(c string) bool {
	if h == nil {
		return false
	}
	for _, cap := range h.Capabilities {
		if cap == c {
			return true
		}
	}
	return false
}

// Namespace is the persisted namespace state returned by upsert/patch
// and embedded in audit responses.
type Namespace struct {
	Name      string                 `json:"name"`
	Kind      NamespaceKind          `json:"kind"`
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt time.Time              `json:"created_at"`
}

// NamespaceUpsert is the body of PUT /v1/namespaces/{name}.
type NamespaceUpsert struct {
	Kind      NamespaceKind          `json:"kind"`
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// NamespacePatch is the body of PATCH /v1/namespaces/{name}.
type NamespacePatch struct {
	ExpiresAt *time.Time             `json:"expires_at,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// MemoryWrite is the body of POST /v1/namespaces/{name}/memories.
//
// `Content` MUST be pre-redacted by workspace-server (SAFE-T1201).
// Plugins do not run additional redaction; the workspace-server is the
// security perimeter.
type MemoryWrite struct {
	Content     string                 `json:"content"`
	Kind        MemoryKind             `json:"kind"`
	Source      MemorySource           `json:"source"`
	ExpiresAt   *time.Time             `json:"expires_at,omitempty"`
	Propagation map[string]interface{} `json:"propagation,omitempty"`
	Pin         bool                   `json:"pin,omitempty"`
	Embedding   []float32              `json:"embedding,omitempty"`
}

// MemoryWriteResponse is the body of 201 from POST .../memories.
type MemoryWriteResponse struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
}

// Memory is a stored memory record returned by search.
type Memory struct {
	ID          string                 `json:"id"`
	Namespace   string                 `json:"namespace"`
	Content     string                 `json:"content"`
	Kind        MemoryKind             `json:"kind"`
	Source      MemorySource           `json:"source"`
	ExpiresAt   *time.Time             `json:"expires_at,omitempty"`
	Propagation map[string]interface{} `json:"propagation,omitempty"`
	Pin         bool                   `json:"pin,omitempty"`
	CreatedAt   time.Time              `json:"created_at"`
	Score       *float64               `json:"score,omitempty"`
}

// SearchRequest is the body of POST /v1/search.
//
// `Namespaces` MUST already be intersected with the caller's readable
// set by workspace-server. The plugin treats it as authoritative.
type SearchRequest struct {
	Namespaces []string     `json:"namespaces"`
	Query      string       `json:"query,omitempty"`
	Kinds      []MemoryKind `json:"kinds,omitempty"`
	Limit      int          `json:"limit,omitempty"`
	Embedding  []float32    `json:"embedding,omitempty"`
}

// SearchResponse is the body of 200 from POST /v1/search.
type SearchResponse struct {
	Memories []Memory `json:"memories"`
}

// ForgetRequest is the body of DELETE /v1/memories/{id}.
type ForgetRequest struct {
	RequestedByNamespace string `json:"requested_by_namespace"`
}

// Error is the standard error envelope for non-2xx responses.
type Error struct {
	Code    ErrorCode              `json:"code"`
	Message string                 `json:"message"`
	Details map[string]interface{} `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil contract.Error>"
	}
	return fmt.Sprintf("memory-plugin: %s: %s", e.Code, e.Message)
}

// --- Validation ---

// Per the OpenAPI spec: lowercase prefix, colon, then alnum + a small
// set of separators. Caps the length at 256 to bound storage.
var namespacePattern = regexp.MustCompile(`^[a-z]+:[A-Za-z0-9_:.\-]+$`)

const maxNamespaceLen = 256

// ValidateNamespaceName enforces the wire-level namespace string
// format. Run by both client (before request) and server (on receive).
func ValidateNamespaceName(name string) error {
	if name == "" {
		return errors.New("namespace name is empty")
	}
	if len(name) > maxNamespaceLen {
		return fmt.Errorf("namespace name exceeds %d chars", maxNamespaceLen)
	}
	if !namespacePattern.MatchString(name) {
		return fmt.Errorf("namespace name %q does not match required pattern %s",
			name, namespacePattern.String())
	}
	return nil
}

// Validate checks NamespaceUpsert against the OpenAPI constraints.
func (u *NamespaceUpsert) Validate() error {
	if u == nil {
		return errors.New("nil NamespaceUpsert")
	}
	if !validNamespaceKind(u.Kind) {
		return fmt.Errorf("invalid namespace kind %q", u.Kind)
	}
	return nil
}

// Validate checks NamespacePatch is at least one mutation. An entirely
// empty patch is rejected so callers don't waste round-trips.
func (p *NamespacePatch) Validate() error {
	if p == nil {
		return errors.New("nil NamespacePatch")
	}
	if p.ExpiresAt == nil && p.Metadata == nil {
		return errors.New("patch has no fields set")
	}
	return nil
}

// Validate checks MemoryWrite. Empty content is rejected (zero-length
// memories are pure overhead). Both kind and source are required.
func (w *MemoryWrite) Validate() error {
	if w == nil {
		return errors.New("nil MemoryWrite")
	}
	if strings.TrimSpace(w.Content) == "" {
		return errors.New("content is empty")
	}
	if !validMemoryKind(w.Kind) {
		return fmt.Errorf("invalid memory kind %q", w.Kind)
	}
	if !validMemorySource(w.Source) {
		return fmt.Errorf("invalid memory source %q", w.Source)
	}
	return nil
}

// Validate checks SearchRequest. The namespace list must be non-empty
// because workspace-server is required to intersect server-side; an
// empty list at this layer is a bug, not a "search everything" intent.
func (s *SearchRequest) Validate() error {
	if s == nil {
		return errors.New("nil SearchRequest")
	}
	if len(s.Namespaces) == 0 {
		return errors.New("namespaces is empty (workspace-server must intersect, not the plugin)")
	}
	for i, ns := range s.Namespaces {
		if err := ValidateNamespaceName(ns); err != nil {
			return fmt.Errorf("namespaces[%d]: %w", i, err)
		}
	}
	if s.Limit < 0 || s.Limit > 100 {
		return fmt.Errorf("limit %d out of range [0,100]", s.Limit)
	}
	for i, k := range s.Kinds {
		if !validMemoryKind(k) {
			return fmt.Errorf("kinds[%d]: invalid memory kind %q", i, k)
		}
	}
	return nil
}

// Validate checks ForgetRequest.
func (f *ForgetRequest) Validate() error {
	if f == nil {
		return errors.New("nil ForgetRequest")
	}
	return ValidateNamespaceName(f.RequestedByNamespace)
}

func validNamespaceKind(k NamespaceKind) bool {
	switch k {
	case NamespaceKindWorkspace, NamespaceKindTeam, NamespaceKindOrg, NamespaceKindCustom:
		return true
	}
	return false
}

func validMemoryKind(k MemoryKind) bool {
	switch k {
	case MemoryKindFact, MemoryKindSummary, MemoryKindCheckpoint:
		return true
	}
	return false
}

func validMemorySource(s MemorySource) bool {
	switch s {
	case MemorySourceAgent, MemorySourceRuntime, MemorySourceUser:
		return true
	}
	return false
}
