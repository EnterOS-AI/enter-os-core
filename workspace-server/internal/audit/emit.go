// Package audit emits structured, single-line JSON audit-log records for
// user-initiated actions on a workspace (secret set/delete, file
// create/delete, A2A send, chat turn, …). Records ship to Loki via the
// tenant Vector pipeline using two transports, in this order:
//
//  1. A `audit:` prefixed line on the standard logger. This is the
//     primary transport — tenant Vector already tails the
//     molecule-tenant container's stdout (see
//     /usr/local/bin/tenant-vector.yaml.tmpl on operator-host), so the
//     event reaches Loki with no Vector-side change.
//
//  2. A best-effort append to /var/log/molecule-audit.jsonl on the
//     tenant container's writable rootfs. This is the durable local
//     artifact for forensic queries when Loki is unreachable, and is
//     the future file-source target for Phase 2 (RFC internal#562 Step
//     1, dedicated audit shipping channel).
//
// Both transports are best-effort and run on the request goroutine.
// Per RFC: emit MUST NOT fail the user's request. Any I/O error is
// dropped to a single log.Printf line so an operator can detect the
// outage during a forensic search. The handler caller is decoupled —
// Emit returns nothing.
//
// # Event schema (stable contract — extend by appending; never rename)
//
//	{
//	  "ts":             "2026-05-19T20:00:00Z",   // RFC3339Nano UTC
//	  "event_type":     "secret.set",             // <noun>.<verb>; low-cardinality
//	  "workspace_id":   "<uuid>",                  // bounded ~1000
//	  "user_id":        "<uuid|empty>",            // unbounded — NOT a label
//	  "actor_kind":     "user|admin|agent|cron",
//	  "correlation_id": "<req-id|empty>",          // upstream request id
//	  "fields": { … }                              // event-specific payload
//	}
//
// `fields` MUST NEVER contain secret values. The convention for
// secret-touching events is to record `value_hash` (sha256(value), hex
// prefix of 8 chars) only.
//
// # Loki labels (cardinality budget — see RFC internal/rfcs/audit-log-to-loki.md §4)
//
//   - tenant         (already set by Vector)         ~10
//   - service        ("molecule-tenant")             1
//   - container      ("molecule-tenant")             1
//   - source         ("audit")                       1
//   - event_type     (low-cardinality, top-20)       ~20
//
// workspace_id, user_id, correlation_id stay INSIDE the JSON body —
// they are queryable via `| json` LogQL but are NOT labels. This keeps
// per-stream cardinality under Loki's 100k/stream chunk limit.
package audit

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// AuditLogPath is where the durable JSONL trail is written. Override
// via the MOLECULE_AUDIT_LOG_PATH env var (useful for tests + for the
// future Phase 2 file-source target).
const defaultAuditLogPath = "/var/log/molecule-audit.jsonl"

// ActorKind enumerates the categories of actor we tag every event
// with. Strings are stable wire values; do not rename.
type ActorKind string

const (
	ActorUser  ActorKind = "user"
	ActorAdmin ActorKind = "admin"
	ActorAgent ActorKind = "agent"
	ActorCron  ActorKind = "cron"
)

// Context-key type — unexported so callers must use the package-local
// setters to avoid string-key collisions across the binary.
type ctxKey int

const (
	ctxKeyUserID        ctxKey = iota // string
	ctxKeyActorKind                   // ActorKind
	ctxKeyCorrelationID               // string
	ctxKeyWorkspaceID                 // string
)

// WithUserID returns ctx with the user-id attached. Middleware that
// authenticates the caller should populate this so handlers can call
// Emit(ctx, ...) without re-discovering identity.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, ctxKeyUserID, userID)
}

// WithActorKind tags the actor category. Defaults to ActorUser when
// unset (see resolveActor).
func WithActorKind(ctx context.Context, k ActorKind) context.Context {
	return context.WithValue(ctx, ctxKeyActorKind, k)
}

// WithCorrelationID attaches an upstream request id (X-Request-Id or
// similar). The empty string is fine; downstream readers treat empty
// as "no upstream id provided".
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyCorrelationID, id)
}

// WithWorkspaceID attaches the workspace UUID — usually pulled from
// the gin URL parameter. Handlers may either pre-populate the context
// or pass it through the Fields map; the Fields map wins if both are
// set, so callers can override on a per-event basis.
func WithWorkspaceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyWorkspaceID, id)
}

func resolveUserID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyUserID).(string); ok {
		return v
	}
	return ""
}

func resolveActor(ctx context.Context) ActorKind {
	if v, ok := ctx.Value(ctxKeyActorKind).(ActorKind); ok && v != "" {
		return v
	}
	return ActorUser
}

func resolveCorrelationID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyCorrelationID).(string); ok {
		return v
	}
	return ""
}

func resolveWorkspaceID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyWorkspaceID).(string); ok {
		return v
	}
	return ""
}

// record is the on-wire shape. Keep field order stable so Loki
// `| json` queries against `event_type` etc. are predictable.
type record struct {
	TS            string         `json:"ts"`
	EventType     string         `json:"event_type"`
	WorkspaceID   string         `json:"workspace_id"`
	UserID        string         `json:"user_id"`
	ActorKind     ActorKind      `json:"actor_kind"`
	CorrelationID string         `json:"correlation_id"`
	Fields        map[string]any `json:"fields"`
}

// fileMu serializes JSONL appends so two goroutines can't interleave
// half-lines. Cheap; audit events are rare relative to request volume.
var fileMu sync.Mutex

// auditLogPath returns the destination path; respects the
// MOLECULE_AUDIT_LOG_PATH env var so tests + future shipping changes
// don't need to recompile.
func auditLogPath() string {
	if p := os.Getenv("MOLECULE_AUDIT_LOG_PATH"); p != "" {
		return p
	}
	return defaultAuditLogPath
}

// nowRFC3339Nano is var so tests can pin time.
var nowRFC3339Nano = func() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// Emit writes one audit record for eventType. Identity, actor, and
// correlation are pulled from ctx; workspaceID falls back to the ctx
// value if absent from fields. Emission is best-effort:
//
//   - The `audit:` log line (Loki transport) is written even if the
//     file append fails.
//   - The file append is wrapped in its own error branch; on failure
//     we drop a single warning and continue.
//
// This function MUST NOT panic and MUST NOT return an error — handlers
// in the request path call it inline.
func Emit(ctx context.Context, eventType string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}

	wsID := ""
	// Fields-supplied workspace_id wins (per-event override).
	if v, ok := fields["workspace_id"].(string); ok && v != "" {
		wsID = v
		// Remove from inner fields so it isn't duplicated — top-level
		// is the canonical position.
		delete(fields, "workspace_id")
	} else {
		wsID = resolveWorkspaceID(ctx)
	}

	rec := record{
		TS:            nowRFC3339Nano(),
		EventType:     eventType,
		WorkspaceID:   wsID,
		UserID:        resolveUserID(ctx),
		ActorKind:     resolveActor(ctx),
		CorrelationID: resolveCorrelationID(ctx),
		Fields:        fields,
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		// Marshal failure → emit a degraded marker so the event boundary
		// is still visible in Loki. Never lose the fact that *something*
		// happened.
		log.Printf("audit: %s {\"_marshal_err\":%q,\"event_type\":%q}", eventType, err.Error(), eventType)
		return
	}

	// Transport 1: stdout (Loki via tenant Vector docker-logs source).
	log.Printf("audit: %s", payload)

	// Transport 2: durable JSONL (forensic local copy, Phase-2
	// file-source target). Best effort.
	appendJSONL(payload)
}

// appendJSONL opens, appends one line, and closes. The open-per-write
// pattern is acceptable at audit-event rates (≪100/s); it survives
// log rotation without the package having to handle SIGHUP.
func appendJSONL(payload []byte) {
	fileMu.Lock()
	defer fileMu.Unlock()

	path := auditLogPath()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		// Don't spam: one warning per emit failure. The Loki transport
		// already captured the event so we are not losing observability.
		log.Printf("audit: append %s failed (event still in stdout): %v", path, err)
		return
	}
	defer func() { _ = f.Close() }()

	// Write payload + newline as one syscall to keep the JSONL invariant.
	if _, werr := f.Write(append(payload, '\n')); werr != nil {
		log.Printf("audit: write %s failed (event still in stdout): %v", path, werr)
	}
}

// HashValuePrefix returns the lowercase hex SHA-256 prefix of v, of
// length n. Use this when an event field needs to identify a secret
// value without exposing it. Returns "" for empty input. n is clamped
// to [4, 64].
func HashValuePrefix(v string, n int) string {
	if v == "" {
		return ""
	}
	if n < 4 {
		n = 4
	}
	if n > 64 {
		n = 64
	}
	return sha256Hex(v)[:n]
}
