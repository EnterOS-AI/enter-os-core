# A2A protocol replay corpus

Captures every A2A JSON-RPC message shape the platform has ever
accepted, so a future PR that bumps a-2-a-sdk or modifies
`normalizeA2APayload` can be tested against historical inputs
before merging.

This is the gate that would have caught the 2026-04-29 v0.2 → v0.3
silent-drop bug (PR #2349). The bug shipped because the SDK bump
PR didn't replay v0.2-shaped inputs against the new code; the
shape-mismatch surfaced only in production when the receiver's
Pydantic validator silently rejected inbound messages.

## Layout

- `valid/` — every shape that has ever been accepted. Each PR that
  changes the protocol code OR bumps the SDK pin runs every entry
  through `normalizeA2APayload` and asserts it parses without
  error. Removing an entry from this directory is a breaking
  change and requires explicit operator approval.

- `invalid/` — shapes that MUST be rejected with the right error
  type. These pin the rejection contract — a future PR that
  silently accepts a malformed shape (because, say, it added a
  permissive default) breaks the gate.

## When to add a corpus entry

- A new SDK version is released and adds a shape we want to support.
  Capture a representative payload (PII-scrubbed) before bumping
  the SDK pin. The same PR that bumps the SDK adds the corpus
  entry AND any required compat shim in `normalizeA2APayload`.

- Production logs surface a shape we hadn't seen before. Capture
  it after PII-scrubbing. Even if it currently rejects, decide
  whether to support it (move to `valid/`) or to keep rejecting
  loudly (move to `invalid/`).

## When NOT to add an entry

- Test scaffolding. The corpus is the SHAPE record, not unit-test
  coverage. Use the regular handler tests for functional coverage.

- Hypothetical future shapes. Add only what we have evidence for
  — either a real payload or an SDK release note.

## Removal policy

Removing an entry from `valid/` is a breaking change for any sender
emitting that shape. Requires:

1. A migration plan (deprecation window, sender notification).
2. A separate PR with a single-line removal + a comment explaining
   the deprecation timeline.
3. Approval from someone outside the PR author's team.

Removing an entry from `invalid/` widens what we accept. Lower bar:
just verify the new behavior is intentional and the corpus entry
moves to `valid/`.

## Anatomy of a corpus entry

```json
{
  "_comment": "v0.2 string content — basic text message via message/send",
  "_added": "2026-04-30",
  "_source": "PR #2349 incident, real payload from sender workspace",
  "jsonrpc": "2.0",
  "method": "message/send",
  "id": "test-id",
  "params": {
    "message": {
      "content": "hello"
    }
  }
}
```

`_comment`, `_added`, `_source` are documentation that the test
loader strips before passing the payload to the parser. They are
required for every entry.
