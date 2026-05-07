# ADR-002: Local-build mode signalled by `MOLECULE_IMAGE_REGISTRY` presence

* Status: Accepted (2026-05-07)
* Issue: #63 (closes Task #194)
* Decision: Hongming (CTO) + Claude Opus 4.7 (implementation)

## Context

Pre-2026-05-06, every Molecule deployment — both production tenants and OSS contributor laptops — pulled workspace-template-* container images from `ghcr.io/molecule-ai/`. Production tenants additionally set `MOLECULE_IMAGE_REGISTRY` to an AWS ECR mirror via Railway env / EC2 user-data, but the OSS default was the upstream GHCR org.

On 2026-05-06 the `Molecule-AI` GitHub org was suspended (saved memory: `feedback_github_botring_fingerprint`). GHCR now returns **403 Forbidden** for every `molecule-ai/workspace-template-*` manifest. OSS contributors who clone `molecule-core` and run `go run ./workspace-server/cmd/server` cannot provision a workspace — every first provision fails with:

```
docker image "ghcr.io/molecule-ai/workspace-template-claude-code:latest" not found after pull attempt
```

Production tenants are unaffected (their `MOLECULE_IMAGE_REGISTRY` points at ECR, which we still control), but OSS onboarding is broken. Workspace template repos are intentionally separate from `molecule-core` (each runtime is OSS-shape and forkable), and they are mirrored to Gitea (`https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-<runtime>`) — but the provisioner has no path that consumes Gitea source directly.

## Decision

When `MOLECULE_IMAGE_REGISTRY` is **unset** (or empty), the provisioner switches to a **local-build mode** that:

1. Looks up the workspace-template repo's HEAD sha on Gitea via a single API call.
2. Checks whether a SHA-pinned local image (`molecule-local/workspace-template-<runtime>:<sha12>`) already exists; if so, reuses it.
3. Otherwise shallow-clones the repo into `~/.cache/molecule/workspace-template-build/<runtime>/<sha12>/` and runs `docker build --platform=linux/amd64 -t <tag> .`.
4. Hands the SHA-pinned tag to Docker for ContainerCreate, bypassing the registry-pull path entirely.

When `MOLECULE_IMAGE_REGISTRY` is **set**, behavior is unchanged: pull the image from that registry. Existing prod tenants and self-hosters who mirror to a private registry are not affected.

## Consequences

### Positive

* **Zero-config OSS onboarding** — `git clone molecule-core && go run ./workspace-server/cmd/server` boots end-to-end without any registry credentials.
* **Production tenants protected** — same env var, same semantics in SaaS-mode. Migration is a no-op.
* **No new env var** — extending an existing var's semantics ("where to pull, OR build locally if absent") rather than introducing `MOLECULE_LOCAL_BUILD=1` keeps the surface small.
* **SHA-pinned cache** — repeat builds are O(API-call); only template-repo HEAD changes invalidate.
* **Production-parity image** — amd64 emulation on Apple Silicon honours `feedback_local_must_mimic_production`. The provisioner's existing `defaultImagePlatform()` already forces amd64 for parity; building amd64 locally lets that decision stay consistent.

### Negative

* **Conflates two concerns** — `MOLECULE_IMAGE_REGISTRY` now signals BOTH "where to pull" AND "build locally if absent." A future operator who unsets it expecting a hard error will instead get a slow first-provision. Documented in the runbook.
* **First-provision is slow on Apple Silicon** — 5–10 min via QEMU emulation on the cold path. Mitigated by SHA-cache (subsequent runs are <1s lookup + 0s build).
* **Coverage gap** — only 4 of 9 runtimes are mirrored to Gitea today (`claude-code`, `hermes`, `langgraph`, `autogen`). The other 5 fail with an actionable "not mirrored" error. Mirroring those repos is a separate task.
* **Implicit trust boundary** — operator running `go run` implicitly trusts `molecule-ai/molecule-ai-workspace-template-*` repos on Gitea. This is the same trust they would extend to the GHCR images today; not a new attack surface.

## Alternatives considered

1. **New env var `MOLECULE_LOCAL_BUILD=1`** — explicit, but requires OSS contributors to know it exists. Violates the zero-config goal.
2. **Push pre-built images to a Gitea container registry, mirror tag from upstream** — operationally cleaner but: (a) Gitea's container-registry add-on isn't deployed on the operator host, (b) defeats the OSS-contributor goal of "hack on the source, see your changes," since they'd still pull a stale image.
3. **Embed Dockerfiles in molecule-core itself, drop the standalone template repos** — would work but breaks the OSS-shape principle; templates are intentionally separable, anyone-can-fork artifacts.
4. **Build native arch on Apple Silicon (arm64) and drop the platform pin in local-mode** — fast, but creates `linux/arm64` images that diverge from the amd64-only prod runtime. Local-vs-prod debug behavior would diverge. Rejected per `feedback_local_must_mimic_production`.

## Security review

* **Gitea repo URL allowlist** — runtime name must be in the `knownRuntimes` allowlist (defence-in-depth against a future code path that lets cfg.Runtime carry untrusted input). Repo prefix is hardcoded to `https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-template-`; forks can override via `MOLECULE_LOCAL_TEMPLATE_REPO_PREFIX` (opt-in, default off).
* **Token handling** — clones are anonymous over HTTPS by default (templates are public). `MOLECULE_GITEA_TOKEN`, if set, is passed via URL userinfo for the clone and as `Authorization: token` for the API call. The token is **masked in every log line** via `maskTokenInURL` / `maskTokenInString` and never appears in the cache dir path.
* **No silent fallback** — if Gitea is unreachable or the runtime isn't mirrored, we return a clear error mentioning the repo URL and the missing runtime. We **never** fall back to GHCR/ECR (that would be a confusing bug for an OSS contributor who happened to have stale ECR creds in their docker config).
* **Build-arg injection** — `docker build` is invoked with NO `--build-arg` from external input. Dockerfile is consumed as-is.
* **Cache poisoning** — cache key is the Gitea HEAD sha + Dockerfile content; a force-push to the template repo's main branch regenerates the key on next run. Cache dir is per-user (`$HOME/.cache`), so cross-user attacks aren't relevant in single-user dev mode.

## Versioning + back-compat

* Existing prod tenants set `MOLECULE_IMAGE_REGISTRY=<ECR url>` → unchanged behavior.
* Existing local installs that set the var → unchanged behavior.
* Existing local installs that don't set it → switch to local-build path. Migration: none required (additive); first provision will take 5–10 min instead of failing.
* No deprecations.

## References

* Issue #63 — feat(workspace-server): local-dev provisioner builds from Gitea source
* Saved memory `feedback_local_must_mimic_production` — local docker must mimic prod, no bypasses
* Saved memory `reference_post_suspension_pipeline` — full post-2026-05-06 stack shape
* Saved memory `feedback_github_botring_fingerprint` — what got the org suspended
