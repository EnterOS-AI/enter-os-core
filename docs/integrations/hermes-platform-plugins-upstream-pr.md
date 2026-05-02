# Upstream PR draft: `register_platform_adapter` for hermes-agent plugins

**Status:** Draft — pre-submission review (REWRITTEN 2026-05-02 after deeper research)
**Target repo:** `NousResearch/hermes-agent`
**Owner:** Molecule AI (hongmingwang@moleculesai.app)
**Date drafted:** 2026-05-02 (rewrite of earlier draft)

---

## Background — what changed in this draft

The first draft proposed adding a `plugins/platforms/` discovery
directory + a `_create_adapter()` fallback chain. **That was wrong** —
it duplicated infrastructure that already exists.

Deeper research established (validated by hand-rolling a test plugin
under `~/.hermes/plugins/`):

- **`hermes_cli/plugins.py` already implements full plugin discovery
  across THREE sources:**
  - User dir: `~/.hermes/plugins/<name>/`
  - Project dir: `./.hermes/plugins/<name>/`
  - Pip entry_points group: `hermes_agent.plugins`
- The discovery loop is at `hermes_cli/plugins.py:433` and
  `_scan_entry_points()` at line 499.
- `PluginContext` (line 124) exposes a `register(ctx)` collector with:
  - `register_tool` (line 133)
  - `register_cli_command` (line 192)
  - `register_command` (line 217) — slash command
  - `register_context_engine` (line 295)
  - `register_hook` (line 327)
  - `register_skill` (line 346)
- **But NOT `register_platform_adapter`.** Platform adapters remain
  hardcoded in `gateway/run.py:_create_adapter()` (lines 2424-2578),
  the only major subsystem still closed to plugins.
- Memory providers have a parallel discovery system at
  `plugins/memory/__init__.py` for legacy reasons; the modern
  `hermes_cli/plugins.py` is the way forward for new plugin types.
- Hand-rolled test confirmed user-dir and entry_points discovery both
  work end-to-end. **Zero external plugins exist in the wild today**
  — the system is technically mature but socially unused.

This makes the PR much smaller and more obviously correct: extend the
existing plugin pattern by one method, mirror how memory providers
work, no novel infrastructure.

---

## Proposed PR title

> `feat(gateway): platform adapter plugins via PluginContext.register_platform_adapter`

Branch: `feat/platform-adapter-plugins` per
`CONTRIBUTING.md` branch convention.

---

## PR body

### Problem

Hermes ships 19 in-tree platform adapters (`gateway/run.py:2424-2578`).
Adding a new platform requires editing two files: append a `Platform`
enum value at `gateway/config.py:48-69`, then append an `elif platform
== Platform.X:` branch in `_create_adapter()`. For platforms with broad
demand (Telegram, Slack, Discord) this is fine. For narrower channels
— enterprise-internal protocols, agent-to-agent inbox bridges, niche
regional platforms — the only path is a fork of `gateway/run.py`.

This is the only major subsystem that's still closed. Tools, CLI
commands, slash commands, context engines, hooks, and skills all
already extend via `hermes_cli/plugins.py`'s `PluginContext`
collector, with three discovery paths (user dir / project dir / pip
entry_points). Platform adapters should follow the same pattern.

### Proposal

Add **one collector method** to `PluginContext` and **one fallback
branch** to `_create_adapter()`. That's the entire change.

**1. New collector method in `hermes_cli/plugins.py`**, beside the
existing `register_tool` / `register_hook` etc.:

```python
class PluginContext:
    # ...existing register_* methods...

    def register_platform_adapter(
        self,
        name: str,
        adapter_class: type,
        requirements_check: Callable[[], bool] | None = None,
    ) -> None:
        """Register a custom platform adapter.

        name              — unique platform identifier (matches
                            gateway.platforms.<name> in config.yaml)
        adapter_class     — subclass of BasePlatformAdapter
        requirements_check— optional, returns False if dependencies
                            missing (matches existing
                            check_telegram_requirements pattern).
        """
        self._registered_platform_adapters[name] = (adapter_class, requirements_check)
```

**2. Plugin-registered adapters in `_create_adapter()`** —
fall through to the plugin-registered map after the in-tree if/elif
chain returns None:

```python
# at gateway/run.py:2578, AFTER the existing chain
plugin_entry = self._plugin_manager.get_platform_adapter(platform.value)
if plugin_entry:
    adapter_class, req_check = plugin_entry
    if req_check and not req_check():
        logger.warning(f"{platform.value}: plugin requirements not met")
        return None
    return adapter_class(config)

return None  # existing return
```

**3. `Platform` enum stays closed** but accepts unknown values
through a small loosening: rather than refactor enum-vs-string,
introduce `Platform.from_string()` that returns either an existing
enum member OR a synthetic `Platform.PLUGIN(value)`-equivalent that
carries the plugin name through. `_create_adapter()` then dispatches
on the carried name. This is the smallest change preserving
backward compatibility — every existing `Platform.TELEGRAM` reference
keeps working unchanged.

### Why this is the right shape

- **Symmetric.** Mirrors `register_tool`, `register_hook`, etc. — same
  collector, same discovery, same lifecycle. No new mental model.
- **No new infrastructure.** Reuses `hermes_cli/plugins.py`'s existing
  three-source discovery (user dir / project dir / entry_points) —
  zero new code paths to test.
- **Backward compatible.** All 19 in-tree adapters keep their
  hardcoded path; precedence is in-tree first, plugin fallback. No
  behavior change for any existing user.
- **Discovery cost is zero.** Plugin lookup only fires if the
  platform name doesn't match an in-tree value.
- **Forward compatible.** When external plugins become commonplace
  (today: zero published, system technically mature but unused),
  platform adapters benefit from the same ecosystem growth as tools.

### What we'll ship as the first consumer

Molecule will publish `hermes-platform-molecule-a2a` on PyPI with the
appropriate `[project.entry-points."hermes_agent.plugins"]` entry. It
delivers Molecule platform A2A inbox messages into the same
`_handle_message` dispatch Telegram uses, with
`MessageEvent(internal=True)` to bypass user-auth (peer agents are
authenticated at the platform layer, not the Telegram-user level).
Implementation lives in our workspace template; this PR upstream is
the contract change that lets us register without forking.

### Backward compatibility

- All 19 in-tree adapters keep their hardcoded path. Precedence:
  in-tree wins on name collision (matches the memory plugin pattern).
- `gateway.platforms.telegram.enabled: true` etc. continue to work
  unchanged.
- No new mandatory config keys.
- Existing `Platform.X` Python references unchanged.
- Plugin discovery only adds latency on platforms that don't match
  an in-tree value — zero cost for existing users.

### Test plan

- **Unit:** Mock plugin registers an adapter via `register_platform_adapter`;
  `_create_adapter()` returns it for the corresponding platform name.
- **Unit:** In-tree precedence — when plugin AND in-tree both register
  `telegram`, in-tree wins.
- **Unit:** Duplicate plugin registration warns + skips, doesn't
  replace the original.
- **Integration:** Add `tests/plugins/platform_example/` (matching
  the existing `tests/plugins/` shape — see how `register_tool` is
  tested today). Smoke that hermes boot loads it.
- **Manual (already done locally):** `hermes-platform-molecule-a2a`
  scaffold validates against the patched fork end-to-end:
  - 11/11 unit tests on the adapter (lifecycle, inbound auth, outbound
    routing, plugin entry-point shape)
  - 7/7 production-path checkpoints (entry_points discovery → registry
    → `GatewayConfig.from_dict` → `_create_plugin_adapter` → live
    HTTP listener → `MessageEvent` dispatch → callback POST)
  - 9/9 user-dir-discovery validation against the patched
    `PluginContext` / `PluginManager`
- **Pre-existing test isolation issue (independent of this PR):**
  `tests/hermes_cli/test_plugins.py::test_discover_is_idempotent` and
  two siblings assert `len(list_plugins()) == 1` after creating one
  test plugin in a tmp_path. They fail on any dev box that has a
  hermes plugin pip-installed (entry_points discovery is global, not
  isolated by HERMES_HOME). Not caused by this patch but surfaced
  during validation. Worth fixing in a follow-up by either filtering
  entry-point plugins out of these specific tests, or adding a
  `discover_only_user_dir=True` test hook to `discover_and_load`.

### Documentation

- Extend `website/docs/developer-guide/build-a-hermes-plugin.md`'s
  capability list to mention platform adapters alongside tools, hooks,
  etc.
- One-paragraph note in `gateway/run.py` explaining the in-tree-first,
  plugin-fallback precedence.

### Out of scope

- Memory provider system migration (still uses
  `plugins/memory/__init__.py`'s separate discovery). Out of scope
  for this PR — orthogonal cleanup.
- A "Plugins Hub" analogous to Skills Hub. Independently useful but
  separate proposal; ship the contract first, build the
  distribution/discovery UX later.

---

## Open questions to put in the GitHub Discussion

Per `CONTRIBUTING.md`, design proposals go in **GitHub Discussions**
at `NousResearch/hermes-agent/discussions`, not Discord. Open one
titled "RFC: `PluginContext.register_platform_adapter`" before filing
the PR. Questions to surface:

1. **Naming.** `register_platform_adapter` matches existing
   `register_*` collector methods. Short forms (`register_platform`,
   `register_channel`) are also possible. Defaulting to the long form
   for consistency.
2. **Synthetic Platform value.** Is a `Platform.from_string()` helper
   (with synthetic plugin entries) acceptable, or do maintainers
   prefer a different shape — e.g., adding a `name: str` field to
   `PlatformConfig` so callers know the plugin name without going
   through the enum?
3. **Test fixture vs example plugin.** The `tests/plugins/`
   directory has fixture-only plugins. Should the platform adapter
   test plugin live there too, or as a real bundled adapter (matching
   how memory providers ship as real bundled implementations under
   `plugins/memory/<name>/`)?
4. **Multi-account plugins.** Existing platforms (Telegram, Slack)
   support multi-account via the `extra` config dict. Is the
   plugin-registered adapter expected to handle the same shape, or
   is single-account a reasonable v1 constraint?

---

## Status checklist (for our own tracking)

Per user's gating: "if the plugin works locally in our docker setup
and e2e testing works, yes [submit]". Validation prerequisites:

- [x] Build `hermes-platform-molecule-a2a` against a forked hermes
      with the proposed `register_platform_adapter` patch applied
      → `~/hermes-platform-molecule-a2a/`, 11/11 unit tests pass,
      7/7 production-path E2E checkpoints pass
- [x] Patched fork at `~/.hermes/hermes-agent` branch
      `feat/platform-adapter-plugins` (4 commits):
      1. `PluginContext.register_platform_adapter` + manager registry
         + `get_plugin_platform_adapter` accessor
      2. `GatewayConfig.plugin_platforms` + `_create_plugin_adapter`
         boot path
      3. `PluginPlatformIdentifier` helper for `BasePlatformAdapter`
         construction
      4. `resolve_platform_id` for plugin-platform-safe deserialization
         in `SessionSource.from_dict` / `SessionEntry.from_dict` /
         `HomeChannel.from_dict` (without this, daemon restart loses
         every plugin-platform session)
- [ ] Bake the forked hermes + plugin into a local copy of our
      `molecule-ai-workspace-template-hermes` Docker image
- [ ] E2E: boot the local image, send A2A messages from a peer agent,
      observe `_handle_message` dispatch + reply through A2A queue
- [ ] Confirm `PluginPlatformIdentifier` doesn't break any downstream
      `isinstance(self.platform, Platform)` check — grep for those
- [ ] Open GitHub Discussion for design validation; wait for maintainer
      feedback (≥1 week)
- [ ] Address Discussion feedback in the PR
- [ ] PR description: what/why + how-to-test + platforms tested per
      `CONTRIBUTING.md`
- [ ] Open PR against `NousResearch/hermes-agent` `main` (**requires
      user confirmation** — visible-to-others action)
- [ ] Track PR; bump cadence weekly; if stalled past 4 weeks, propose
      bundling our adapter directly under `gateway/platforms/molecule_a2a.py`
      as a fallback (smaller upstream maintenance footprint than fork)

---

## What changed from the first draft, in one paragraph

First draft proposed extending the memory-provider pattern to platforms
via a new `plugins/platforms/` directory and bespoke discovery code in
`_create_adapter()`. Research established that hermes's MODERN plugin
system is `hermes_cli/plugins.py` (not `plugins/memory/`), already
supports user-dir + entry_points discovery for tools/hooks/CLI/skills,
and just needs `register_platform_adapter` added to its collector to
cover platforms too. The new draft is ~60 lines of upstream code change
instead of ~200, with a tighter conceptual fit and better forward
compatibility.
