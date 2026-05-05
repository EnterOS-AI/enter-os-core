// Package provlog emits structured, single-line JSON log records for
// provisioning-lifecycle boundaries (workspace create, EC2 start/stop,
// restart, idempotency skips). Records share a stable `evt:` prefix and
// JSON payload so a future grep|jq pipeline (or a Loki/Datadog ingest)
// can reconstruct the per-workspace timeline without parsing the
// human-prose log lines that already exist.
//
// Existing log.Printf lines are intentionally NOT replaced — they
// remain the operator-facing message. Event() emits a paired structured
// record alongside, additive only.
//
// Event taxonomy (extend by appending; never rename):
//
//	provision.start         — workspace row inserted, EC2 about to launch
//	provision.skip_existing — idempotency hit, no new EC2
//	provision.ec2_started   — RunInstances returned an instance id
//	provision.ec2_stopped   — TerminateInstances acknowledged
//	restart.pre_stop        — Restart handler about to call Stop
//
// Required fields per event are documented at each call site.
package provlog

import (
	"encoding/json"
	"log"
)

// Event writes a single line of the form:
//
//	evt: <name> {"k":"v",...}
//
// to the standard logger. JSON encoding errors are silently swallowed —
// a logging helper must never panic the request path. fields may be
// nil; the empty payload `{}` is still useful to mark an event boundary.
func Event(name string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		// Fall back to a static payload so the event boundary still
		// appears in the log. The marshal error itself is recorded
		// on a best-effort basis.
		log.Printf("evt: %s {\"_marshal_err\":%q}", name, err.Error())
		return
	}
	log.Printf("evt: %s %s", name, payload)
}
