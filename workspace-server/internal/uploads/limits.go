// Package uploads is the single source of truth for chat-upload sizing
// constraints across every layer of the platform.
//
// Before this package existed the same numbers were duplicated across at
// least five surfaces:
//
//  1. workspace-server Go const  — pendinguploads.MaxFileBytes
//  2. workspace-server Go const  — handlers.chatUploadMaxBytes
//  3. workspace Python module    — workspace/inbox_uploads.MAX_FILE_BYTES
//  4. workspace Python module    — workspace/internal_chat_uploads
//     .CHAT_UPLOAD_MAX_BYTES / .CHAT_UPLOAD_MAX_FILE_BYTES
//  5. canvas TypeScript const    — canvas/.../chat/uploads.ts MAX_UPLOAD_BYTES
//
// plus a sixth (the DB CHECK on pending_uploads.size_bytes) and a seventh
// (the nginx test-harness client_max_body_size).
//
// Every cap change required a coordinated edit across all of them. mc#1588
// raised push-mode (1, 2, 4, 5, 7) from 50 MB to 100 MB on 2026-05-20;
// the matching poll-mode + DB CHECK bumps (3, 6, parts of pendinguploads)
// were missed and shipped a day later as mc#1589 (drift window: one day,
// production confusion: "why does push work but poll reject the same
// file?"). The same drift class is guaranteed to recur on every future cap
// change unless the constants converge.
//
// This package + the GET /uploads/limits endpoint are the convergence
// point. The Go consumers reference DefaultUploadLimits() directly; the
// out-of-process consumers (workspace Python, canvas TS, python ingest)
// can fetch the limits via the public endpoint at startup and cache them.
// The migration that defines the DB CHECK references the same numerical
// constant via a -- comment so a reviewer can see at a glance whether a
// new migration is in sync with the Go default.
//
// Task tracking: molecule-ai/internal #320 + the legacy SSOT-follow-up
// markers in pendinguploads/storage.go, handlers/chat_files.go, and
// canvas/src/components/tabs/chat/uploads.ts.
package uploads

// UploadLimits is the wire shape returned by GET /uploads/limits and the
// in-process type read by every Go consumer. The JSON tags are part of
// the stable public contract — renaming or removing a field is a
// breaking change for the canvas + Python consumers.
//
// New fields MAY be added without a major bump (consumers ignore unknown
// keys), but every existing field must keep its name + units forever or
// roll out a v2 endpoint.
type UploadLimits struct {
	// PerFileBytes is the hard cap on a single uploaded file. Enforced
	// in three places: the platform-side handler in chat_files.go
	// (push + poll paths), the workspace-side ingest in
	// internal_chat_uploads.py (push) + inbox_uploads.py (poll), and
	// the canvas-side pre-flight gate before any network I/O. The DB
	// CHECK on pending_uploads.size_bytes also enforces this value for
	// the poll-mode staging table.
	PerFileBytes int64 `json:"per_file_bytes"`

	// PerRequestBytes is the hard cap on the full multipart request
	// body. With one attachment + minimal multipart framing this is
	// effectively equal to PerFileBytes; with N attachments it bounds
	// the sum. Today we keep them equal at 100 MB — a multi-file batch
	// must collectively fit under the same ceiling as a single large
	// file. If we ever decouple them (e.g. raise per-request to allow
	// a 200 MB batch of 50 MB files) this field is where that lands.
	PerRequestBytes int64 `json:"per_request_bytes"`

	// MaxAttachmentsPerMessage caps the count of files in a single
	// /chat/uploads request. Defends against a pathological client that
	// streams 10 000 1-byte files (which would each spawn a row in
	// pending_uploads, exhaust file descriptors on the workspace side,
	// and slow chat_files.uploadPollMode's per-file loop to a crawl).
	// Currently advisory only — consumers are free to read it but no
	// platform handler enforces it as of task #320 Phase 1. Will be
	// enforced once the canvas + workspace consumers have rolled.
	MaxAttachmentsPerMessage int `json:"max_attachments_per_message"`
}

// DefaultUploadLimits returns the production defaults. This is THE
// source: every other constant in the codebase that mentions an upload
// cap must derive from this function, NOT from a duplicated literal.
//
// Why a function and not a package-level var: a var would be mutable at
// runtime and create the "test modified it and forgot to reset it" class
// of flake. Callers that need a per-test override should pass a custom
// UploadLimits value through the handler/registration site, not mutate
// a global. (No such override exists today; if one is needed in the
// future, prefer a WithLimits(UploadLimits) wiring option over a
// SetDefault function.)
//
// Values pinned at 100 MB per CTO directive 2026-05-19, in lockstep
// with mc#1588 + mc#1589. Bumping the cap is a coordinated multi-PR
// dance: raise this default, ship a DB migration that loosens the
// pending_uploads.size_bytes CHECK, raise the nginx
// client_max_body_size in tests/harness/cf-proxy/nginx.conf, and
// confirm both push-mode + poll-mode E2E. The whole point of this
// package is that step 1 is now ONE edit instead of 5.
func DefaultUploadLimits() UploadLimits {
	return UploadLimits{
		PerFileBytes:             100 * 1024 * 1024, // 100 MB
		PerRequestBytes:          100 * 1024 * 1024, // 100 MB
		MaxAttachmentsPerMessage: 10,
	}
}
