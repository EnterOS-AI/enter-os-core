// Package rescue captures a fixed post-mortem "rescue bundle" off a
// workspace EC2 whose boot FAILED — before the platform's sweeper /
// control-plane reaps the instance — and ships it to obs/Loki so a
// wedged workspace (e.g. the codex provider-derivation failure that
// motivated RFC internal#742) is inspectable instead of an
// uninspectable wall.
//
// Design constraints (RFC internal#742, Part 2):
//
//   - BEST-EFFORT + NON-BLOCKING. Capture MUST NOT change boot-failure
//     semantics or add latency to the failure path. Callers fire
//     Capture in its own goroutine; Capture additionally bounds itself
//     with CaptureTimeout so a hung EIC tunnel can't wedge the
//     goroutine forever.
//   - FIRES ON THE BOOT-FAILURE VERDICT ONLY. The two hook points are
//     the provision-timeout sweep (registry.sweepStuckProvisioning) and
//     the out-of-band bootstrap-watcher signal
//     (handlers.WorkspaceHandler.BootstrapFailed). Normal teardown /
//     deprovision / recreate / billing-suspend / hibernate paths do NOT
//     call Capture — see the RFC's path enumeration.
//   - REDACT BEFORE ANYTHING LEAVES THE BOX. Every collected section is
//     run through the injected Redact func (wired to the existing
//     handlers.redactSecrets secret-scan) before it is shipped. Raw
//     tokens/keys never reach Loki.
//
// The package is a LEAF: it imports only internal/audit (the obs
// shipper) so it can be called from both handlers and registry without
// an import cycle (registry must not import handlers). The two heavy
// dependencies — the EIC/SSH remote-command runner and the redactor —
// are injected as package-level func vars, wired once at boot from the
// handlers package (which owns withEICTunnel + redactSecrets). Tests
// swap them for fakes.
package rescue

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/EnterOS-AI/enter-os-core/workspace-server/internal/audit"
)

// CaptureTimeout bounds the whole bundle collection. The sweeper runs
// every 30s and the CP reap follows the failure verdict; 45s gives the
// EIC dance (~3-5s) plus six short remote commands (<2s each) generous
// headroom while still finishing well before the instance is torn down.
// Distinct from the per-op eicFileOpTimeout so a slow box that already
// failed to boot can't hang the capture goroutine indefinitely.
const CaptureTimeout = 45 * time.Second

// LokiKind is the Loki stream label value that tags every rescue
// record. Queryable as `kind="rescue"` (RFC internal#742 §Loki labels).
const LokiKind = "rescue"

// RescueVolumeGrace is how long a boot-failed workspace's /configs data
// volume (and its still-running instance) must be RETAINED past the
// boot-failure verdict so a live rescue read is possible — distinct from
// the user-requested prune path (cp#415), which is an explicit erase.
//
// In molecule-core (the tenant platform) the boot-failure verdict only
// flips workspaces.status to `failed`; it never issues a terminate. The
// platform's two reapers (registry.StartCPOrphanSweeper +
// handlers deprovision) act ONLY on status='removed', so a `failed`
// workspace's instance + /configs volume are retained here by
// construction — see TestCPSweepOnce_DoesNotReapFailedWorkspace. The
// time-bounded reap of the failed instance is the control plane's
// bootstrap-watcher concern; this constant is the SSOT for the grace
// the CP must honour (24h covers an operator's next-business-day
// post-mortem without leaking the volume indefinitely).
const RescueVolumeGrace = 24 * time.Hour

// rescueEventType is the audit event_type carried in the shipped
// record. The obs shipper (internal/audit) already maps event_type to a
// low-cardinality Loki label; "rescue.bundle" keeps the rescue stream
// trivially filterable alongside the existing audit taxonomy.
const rescueEventType = "rescue.bundle"

// RunRemote runs a single shell command on the still-running (but
// unconfigured) workspace EC2 over EIC/SSH and returns its combined
// output. Wired at boot to the handlers EIC runner
// (rescueRunRemoteViaEIC). nil until wired — Capture degrades to a
// logged no-op rather than panicking, so an operator who hasn't wired
// the hook still gets a clear signal instead of a crash on the failure
// path.
var RunRemote func(ctx context.Context, instanceID, command string) (string, error)

// Redact scrubs secret-shaped substrings from a collected section
// before it leaves the box. Wired at boot to handlers.redactSecrets.
// nil until wired — Capture refuses to ship un-redacted content if the
// redactor is missing (fails closed: logs + aborts rather than leaking
// raw config).
var Redact func(workspaceID, content string) string

// section is one labelled chunk of the rescue bundle: a human-readable
// name + the remote command that produces it.
type section struct {
	name    string
	command string
}

// bundleSections is the FIXED set collected on every boot-failure
// rescue (RFC internal#742 §Build.1). Order is the post-mortem reading
// order: config first, then boot logs, then container state, then the
// resolved model/provider env that drove the codex derivation failure.
//
//   - /configs/config.yaml + system-prompt.md: the managed config the
//     runtime booted against (redacted; system-prompt can embed keys).
//   - cloud-init-output.log tail: the user-data execution trace — where
//     a wedged boot actually died.
//   - docker ps -a: container state (did the agent container even
//     start, exit-code, restart loop).
//   - agent container logs: the runtime's own stderr (the codex
//     provider-derivation panic lives here).
//   - MODEL|PROVIDER|RUNTIME env: the resolved routing that motivated
//     the RFC. `sudo cat` of the container env via docker inspect-style
//     grep — see the command.
//
// All commands use `sudo -n` (the box's /configs is root-owned; ubuntu
// has passwordless sudo) and swallow missing-target stderr so a section
// that can't be produced ships as a short marker instead of failing the
// whole bundle. Kept as data (not inlined) so the redaction + ship loop
// is uniform and the set is reviewable in one place.
var bundleSections = []section{
	{
		name:    "config.yaml",
		command: "sudo -n cat /configs/config.yaml 2>/dev/null || echo '(/configs/config.yaml absent)'",
	},
	{
		name:    "system-prompt.md",
		command: "sudo -n cat /configs/system-prompt.md 2>/dev/null || echo '(/configs/system-prompt.md absent)'",
	},
	{
		name:    "cloud-init-output.log.tail",
		command: "sudo -n tail -200 /var/log/cloud-init-output.log 2>/dev/null || echo '(cloud-init-output.log absent)'",
	},
	{
		name:    "docker-ps",
		command: "sudo -n docker ps -a 2>/dev/null || echo '(docker unavailable)'",
	},
	{
		// The agent container is the first non-infra container; grab the
		// most recently created one and tail its logs. `head -1` of
		// `docker ps -a -q` is creation-ordered newest-first, which is
		// the agent runtime on a workspace box.
		name:    "agent-container.logs.tail",
		command: "cid=$(sudo -n docker ps -a -q 2>/dev/null | head -1); [ -n \"$cid\" ] && sudo -n docker logs --tail 200 \"$cid\" 2>&1 || echo '(no agent container)'",
	},
	{
		// Resolved model/provider/runtime env from the agent container.
		// `docker inspect` the env array and grep the routing keys. This
		// is the field that pinpoints a provider-derivation failure.
		name:    "model-provider-runtime.env",
		command: "cid=$(sudo -n docker ps -a -q 2>/dev/null | head -1); [ -n \"$cid\" ] && sudo -n docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' \"$cid\" 2>/dev/null | grep -E 'MODEL|PROVIDER|RUNTIME' || echo '(no env)'",
	},
}

// Input is the identity of the failed workspace being rescued.
type Input struct {
	InstanceID  string // EC2 instance id of the still-running failed box
	WorkspaceID string
	OrgID       string
	// Reason is a short tag for WHY the rescue fired (e.g.
	// "provision_timeout_sweep" or "bootstrap_watcher") — carried into
	// the Loki record so an operator can correlate the bundle with the
	// failure verdict that triggered it.
	Reason string
}

// Section is one labelled, already-redacted chunk of the persisted
// rescue bundle. It mirrors what ship() emits to Loki per-section, but
// is the unit the queryable store (and the read endpoint) returns.
// `Redacted` is false only for collection-failure markers (the section
// command couldn't run); true sections passed through the secret-scan.
type Section struct {
	Name     string `json:"name"`
	Content  string `json:"content"`
	Redacted bool   `json:"redacted"`
}

// Bundle is the full, already-redacted post-mortem bundle for ONE
// boot-failure capture — the unit persisted to the queryable store on
// capture (RFC internal#742 Part 3) and served by
// GET /workspaces/:id/rescue. Sections are in fixed reading order
// (config → boot logs → container state → resolved routing env).
type Bundle struct {
	WorkspaceID string    `json:"workspace_id"`
	OrgID       string    `json:"org_id"`
	InstanceID  string    `json:"instance_id"`
	Reason      string    `json:"reason"`
	Sections    []Section `json:"sections"`
}

// PersistBundle writes the fully-collected, already-redacted bundle to
// the queryable per-tenant store (rescue_bundles table) so the rescue
// READ endpoint can serve it without obs/Loki read creds (RFC
// internal#742 Part 3 read-path decision — see the migration header).
//
// Wired at boot from the handlers package (which owns db.DB) to keep
// internal/rescue a leaf: it must NOT import internal/db, or registry —
// which imports rescue — would inherit a db dependency it can call
// without a cycle, but more importantly the leaf stays trivially
// testable with a fake. nil until wired: a capture with no store wired
// still ships to Loki (Part 2 behavior preserved) and logs that it
// skipped the DB persist, rather than failing the capture.
var PersistBundle func(ctx context.Context, b Bundle) error

// Capture collects the fixed rescue bundle off the failed instance,
// redacts each section, and ships it to Loki under
// {kind="rescue", org=<OrgID>, workspace_id=<WorkspaceID>}.
//
// BEST-EFFORT: every failure mode (missing wiring, EIC error, a single
// section that won't collect) is logged and does NOT propagate — Capture
// never returns an error and never panics, so the boot-failure handling
// at the call site is unaffected. The caller is expected to invoke this
// in its own goroutine; Capture additionally self-bounds with
// CaptureTimeout.
func Capture(ctx context.Context, in Input) {
	defer func() {
		// A logging helper on the failure path must never take the
		// process down. Recover defensively — the redactor / shipper are
		// injected and a future mis-wire shouldn't crash the sweeper.
		if r := recover(); r != nil {
			log.Printf("rescue: capture panicked for ws=%s instance=%s: %v", in.WorkspaceID, in.InstanceID, r)
		}
	}()

	if in.InstanceID == "" {
		// No live box to read — nothing to rescue (e.g. failure before
		// any EC2 was launched). Not an error; just skip.
		log.Printf("rescue: skip ws=%s — no instance_id (nothing to capture)", in.WorkspaceID)
		return
	}
	if RunRemote == nil {
		log.Printf("rescue: skip ws=%s instance=%s — RunRemote not wired (best-effort no-op)", in.WorkspaceID, in.InstanceID)
		return
	}
	if Redact == nil {
		// Fail CLOSED: without a redactor we could leak raw tokens to
		// Loki. Abort rather than ship unredacted.
		log.Printf("rescue: ABORT ws=%s instance=%s — Redact not wired; refusing to ship un-redacted bundle", in.WorkspaceID, in.InstanceID)
		return
	}

	ctx, cancel := context.WithTimeout(ctx, CaptureTimeout)
	defer cancel()

	log.Printf("rescue: capturing bundle ws=%s instance=%s reason=%s", in.WorkspaceID, in.InstanceID, in.Reason)

	collected := 0
	// Accumulate the per-section result alongside shipping each to Loki,
	// so the same already-redacted content is persisted to the queryable
	// store as one bundle row after the loop. Shipping stays per-section
	// (Part 2 Loki behavior unchanged); persistence is the single
	// bundle the read endpoint serves.
	bundle := Bundle{
		WorkspaceID: in.WorkspaceID,
		OrgID:       in.OrgID,
		InstanceID:  in.InstanceID,
		Reason:      in.Reason,
		Sections:    make([]Section, 0, len(bundleSections)),
	}
	for _, sec := range bundleSections {
		raw, err := RunRemote(ctx, in.InstanceID, sec.command)
		if err != nil {
			// One section failing (e.g. ssh blip mid-collection) must not
			// abort the rest — ship a marker for it and continue.
			log.Printf("rescue: section %q failed for ws=%s: %v", sec.name, in.WorkspaceID, err)
			marker := fmt.Sprintf("(rescue: section collection failed: %v)", err)
			ship(ctx, in, sec.name, marker, false)
			bundle.Sections = append(bundle.Sections, Section{Name: sec.name, Content: marker, Redacted: false})
			continue
		}
		redacted := Redact(in.WorkspaceID, raw)
		ship(ctx, in, sec.name, redacted, true)
		bundle.Sections = append(bundle.Sections, Section{Name: sec.name, Content: redacted, Redacted: true})
		collected++
	}

	log.Printf("rescue: shipped %d/%d sections ws=%s instance=%s kind=%s", collected, len(bundleSections), in.WorkspaceID, in.InstanceID, LokiKind)

	// Persist the redacted bundle to the queryable store so the rescue
	// READ endpoint can serve it without obs/Loki read creds. Best-effort
	// and last: a persist failure (or no store wired) must NOT undo the
	// Loki ship that already succeeded, and never panics the failure path.
	persistBundle(ctx, bundle)
}

// persistBundle writes the collected bundle to the queryable store if a
// store is wired. Best-effort: a nil store (operator hasn't wired the
// READ path) or a DB error is logged and swallowed — the Loki ship is
// the durable cross-tenant copy, and the failure path must never be
// disturbed by the post-mortem read store.
func persistBundle(ctx context.Context, b Bundle) {
	if PersistBundle == nil {
		log.Printf("rescue: store not wired — bundle for ws=%s shipped to Loki only (no queryable copy)", b.WorkspaceID)
		return
	}
	if err := PersistBundle(ctx, b); err != nil {
		log.Printf("rescue: persist bundle for ws=%s failed (shipped to Loki regardless): %v", b.WorkspaceID, err)
	}
}

// ship emits one rescue section to Loki via the audit shipper. The
// org / workspace_id / kind ride in the record body (queryable via
// LogQL `| json`); event_type ("rescue.bundle") is the low-cardinality
// Loki label the shipper already promotes. `redacted` records whether
// the content passed through the secret-scan, so an operator can tell a
// shipped-but-redacted section from a collection-failure marker.
func ship(ctx context.Context, in Input, name, content string, redacted bool) {
	audit.Emit(ctx, rescueEventType, map[string]any{
		"kind":         LokiKind,
		"org":          in.OrgID,
		"workspace_id": in.WorkspaceID,
		"instance_id":  in.InstanceID,
		"reason":       in.Reason,
		"section":      name,
		"redacted":     redacted,
		"content":      content,
	})
}
