# Hermes Adapter — Implementation Plan

**Author:** Dev Lead  
**Date:** 2026-04-13  
**Branch convention:** `feat/hermes-adapter-<step>` for each PR below  
**Target:** Ship a minimal but functional Hermes workspace adapter in 4 PRs, each ≤200 lines changed.

---

## PR Sequence

### PR 1 — Docker image shell

**Title:** `feat(hermes): add workspace-template:hermes Docker image`

**Files touched:**
- `workspace/adapters/hermes/Dockerfile` (new)
- `workspace/adapters/hermes/requirements.txt` (new)
- `workspace/adapters/hermes/__init__.py` (new)
- `workspace/build-all.sh` (1-line addition)

**Description:** Adds the Hermes Docker image layer. `Dockerfile` extends `workspace-template:base` and installs `hermes-agent` (and declared deps) via pip at build time. `build-all.sh` gains `hermes` in the adapter list so `bash build-all.sh` and `bash build-all.sh hermes` both work. No Python adapter logic yet — just proves the image builds and that `import hermes` succeeds inside the container. CI: add `hermes` to the docker-build matrix.

---

### PR 2 — Python adapter + A2A executor

**Title:** `feat(hermes): implement HermesAdapter and A2A executor`

**Files touched:**
- `workspace/adapters/hermes/adapter.py` (new, ~80 lines)
- `workspace/tests/test_adapters.py` (extend existing test file, ~30 lines)

**Description:** Implements `HermesAdapter(BaseAdapter)` with `name()`, `display_name()`, `description()`, `get_config_schema()`, `setup()`, and `create_executor()`. `setup()` calls `_common_setup()` to load plugins/skills/tools identically to other adapters, then validates that `NOUS_API_KEY` or `OPENROUTER_API_KEY` is present and initialises a Hermes SDK session. `create_executor()` wraps the session as an `AgentExecutor`. Tests cover: adapter name/display_name contract, `setup()` raises `RuntimeError` when both API keys are absent, executor is returned after valid setup.

---

### PR 3 — Platform RuntimeImages entry

**Title:** `fix(provisioner): add hermes to RuntimeImages map`

**Files touched:**
- `workspace-server/internal/provisioner/provisioner.go` (1-line addition)
- `workspace-server/internal/provisioner/provisioner_test.go` (1-line addition in RuntimeImages coverage test)

**Description:** Adds `"hermes": "workspace-template:hermes"` to the `RuntimeImages` map. Without this entry the platform falls back to `workspace-template:langgraph` (wrong deps, agent fails to start). Test: extend the existing table-driven test that asserts every declared runtime resolves to a non-empty image tag.

---

### PR 4 — Integration docs + org template entry

**Title:** `docs(hermes): adapter usage guide and org template example`

**Files touched:**
- `docs/adapters/hermes-adapter-design.md` (update status from Draft → Implemented)
- `workspace-configs-templates/hermes/config.yaml` (new, ~20 lines — minimal config template)
- `org-templates/molecule-worker-gemini/org.yaml` or a new `molecule-hermes/` org template (optional, ~30 lines)

**Description:** Marks the design doc as implemented, adds a `workspace-configs-templates/hermes/config.yaml` so operators can create a Hermes workspace from the UI template picker, and optionally adds a minimal org template showing a Hermes-runtime team. Documents the three env vars (`NOUS_API_KEY`, `OPENROUTER_API_KEY`, `HERMES_MODEL`) in the config template comments.

---

## Sequencing Notes

- PRs 1 and 2 can overlap in development but PR 2 must merge after PR 1 (image must exist before adapter tests run in CI).
- PR 3 is a single-line change and can merge any time after PR 1 lands.
- PR 4 has no code risk; it can be drafted alongside PR 2 and merged last.
- Total estimated diff: ~180 lines of new code across all 4 PRs; well within the ≤200 lines/PR budget.

## Open Questions (resolve before PR 2)

1. **Hermes SDK import path** — confirm the pip package name and the Python import path (`import hermes`? `from hermes_agent import ...`?). Check `NousResearch/hermes-agent` README before writing adapter.py.
2. **Session persistence** — Hermes has a learning loop that writes skill files. Decide at PR 2 time whether to mount `/workspace` as the Hermes skills root or suppress auto-write in v1.
3. **Model default** — confirm the correct model identifier string for Nous Portal (e.g. `nous-hermes-3-70b` vs `hermes-3`); hardcode a safe default in `get_config_schema()`.
