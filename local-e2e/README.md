# local-e2e — session-continuity canary harness

Self-contained Docker-Compose harness that gates RFC#600-class template
changes (session continuity, file-only messages, multimodal prompts,
cross-session memory) **before** they reach customer canary.

Per CTO standing directive "fully tested + separate CI": this is a
dedicated, *fast* (target <3 min), *small-surface* harness that uses a
Python tenant-CP simulator (not the full `workspace-server` Go service)
to exercise the runtime image end-to-end against canonical canary turns.

See [`feedback_no_single_source_of_truth`] — the harness IS the canonical
session-continuity validator. Per-runtime unit tests still cover their
own guard logic; the harness covers the live conversational behaviour
that those unit tests cannot prove.

See [`feedback_image_promote_is_not_user_live`] — every assertion reads
state back from the *running container*, never from a publish-pipeline
ack.

## What it tests (the 4 canaries)

| # | Scenario | Asserts |
|---|----------|---------|
| 1 | 2-turn name canary | turn 2 reply contains "Hongming" → SessionStore continuity |
| 2 | File-only message (no caption) | NOT "(empty prompt — nothing to do)" + reply references filename or asks for clarification |
| 3 | File + caption ("summarize this") | reply addresses attachment + caption |
| 4 | Cross-session memory recall | new session pulls "blue" via memory tool |

Each scenario re-uses the same A2A wire-shape that the production
`workspace-server` POSTs to runtime `:8000` (canvas-thread-id semantics
via `context_id`).

## Architecture

```
local-e2e/
  docker-compose.yml           # runtime under test + cp_sim
  cp_sim/                      # ≈300 LoC Python A2A poster + file uploader
    cp_sim.py
    Dockerfile
    requirements.txt
  canary/
    conftest.py
    test_session_continuity.py # 4 canary scenarios
    test_layer_diagnostics.py  # SessionStore state probe + key derivation
  scripts/
    run-canary.sh              # one-shot orchestration entrypoint
```

The CP simulator emits the **exact** JSON-RPC `message/send` envelope
that `workspace-server` produces (verified against
`tests/e2e/test_chat_attachments_e2e.sh`). No Go service is in the loop —
this keeps the harness lean per the CTO directive.

## Run locally

```bash
# from enter-os-core repo root:
export TEMPLATE_IMAGE=ghcr.io/molecule-ai/workspace-template-hermes:latest
./local-e2e/scripts/run-canary.sh
```

Exit code 0 = all 4 canaries pass. Non-zero = at least one canary failed
and the harness dumped SessionStore state + last 200 log lines from the
runtime container into `./local-e2e/artifacts/`.

## How it integrates into CI

Each template repo's `.gitea/workflows/session-continuity-e2e.yml` calls
`run-canary.sh` with its own freshly-built `TEMPLATE_IMAGE`. The
template repo's Gitea branch-protection lists
`session-continuity-e2e (pull_request)` as a required context.

Rollout order (deliberate — per `feedback_image_promote_is_not_user_live`
we bake before we cascade):

1. `molecule-ai-workspace-template-hermes` — highest-traffic + most
   recent RFC#600-class fixes — REQUIRED gate
2. Bake for 5 business days
3. Cascade to claude-code, langgraph, autogen, openclaw, smolagents,
   google-adk (one PR per template — see `scripts/onboard-template.sh`)

## Future extensions (out of scope for the initial PR)

- Multi-session memory consistency (3+ sessions deep)
- Tool-use canary (workspace seeded with skills/, agent must invoke)
- Streaming-cancellation canary (mid-stream client disconnect)
- Cross-runtime A2A peer call (currently covered by `e2e-peer-visibility`)

## Why a thin Python simulator and not the real `workspace-server`?

`workspace-server` is a 60+ MB Go binary that requires Postgres, Redis,
admin-token wiring, registry plumbing, and a 30+ second cold-boot. None
of that touches session-continuity behaviour, which is fully owned by
the runtime container's `a2a_executor.py`. Per CTO directive "separate
CI as possible" + the <3 min target, we excise the platform-tenant Go
service from the loop and emit identical wire-shape envelopes from a
single Python file.

If the simulator diverges from `workspace-server` wire shape, the gate
goes red — fix the simulator to match production. The wire shape is
asserted in `tests/e2e/test_chat_attachments_e2e.sh` and the runtime's
`workspace/a2a_executor.py:_core_execute`.
