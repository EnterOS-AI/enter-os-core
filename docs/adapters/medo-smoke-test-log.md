# MeDo Smoke Test Log — 2026-04-13 (Run 4)

**Tester:** PM (direct execution)  
**Goal:** Install Miaoda App Builder skill → build "Hello EnterOS" landing page → publish → URL.  
**Credits spent:** 0 across all four runs.

---

## Run Summary

| Run | Blocker | Resolution |
|-----|---------|------------|
| 1 | `workspace-template:openclaw` image not built | ✅ Operator rebuilt image |
| 2 | Adapter key lookup ignores `AISTUDIO_API_KEY` / `QIANFAN_API_KEY` | ✅ Code fix committed (d779e16) |
| 3 | Executor creates fresh OpenClaw session per A2A message | ✅ Code fix committed (9466943) |
| 4 | `payloads: []` on every response — agent never returns text via `--json` mode | ❌ Root cause below |

---

## Run 4 — Detailed Findings

### Environment — all green
| Check | Result |
|-------|--------|
| Platform health | ✅ |
| `workspace-template:openclaw` image | ✅ boots in 31s |
| AISTUDIO_API_KEY + gemini-2.0-flash | ✅ confirmed in every response meta |
| Stable session ID (workspace ID) | ✅ `sessionKey: agent:main:explicit:a507780d-...` consistent across all calls |

### Messages Sent and Responses

| Message | Response | Duration |
|---------|----------|----------|
| Install skill | `payloads: [], livenessState: working` | 1.7s |
| Build Hello EnterOS | `payloads: [], livenessState: working` | 0.8s |
| Check status (sessions_list) | `LLM request failed: provider rejected request schema/payload` | — |
| Reply with exactly: STATUS_OK | `payloads: [], livenessState: working` (after restart) | 1.8s |

The "Reply with exactly: STATUS_OK" response is decisive. A vanilla LLM call with no tool use should produce a text payload. It didn't. This rules out skill complexity or message ambiguity as the cause.

### Root Cause — `openclaw agent --json` Does Not Surface Agent Text in `payloads`

The OpenClaw agent processes messages using background session dispatch (`sessions_spawn` / `sessions_yield`). In this mode:
1. Main session receives message → immediately spawns background session → calls `sessions_yield`
2. `openclaw agent --json` exits with `payloads: [], livenessState: 'working'`
3. Background session processes the actual work and produces text — but only visible in interactive/streaming mode, not in the `--json` subprocess call

**Evidence:** Even "Reply with exactly: STATUS_OK" returns `payloads: []`. The agent is using background sessions for everything, including trivial echo requests.

**Likely cause:** OpenClaw's default `SOUL.md` / `BOOTSTRAP.md` workspace config instructs the agent to always use async session patterns. In a terminal session these background responses appear naturally; via subprocess `--json`, only the main session's synchronous output is captured.

### Transient issue: LLM request failed
After 3+ rapid A2A calls (install → build → status check), the Gemini AI Studio API returned a schema/payload rejection. Resolved by restarting the workspace (`POST /workspaces/:id/restart`). Likely a rate-limit or context-size rejection from Gemini. Restarted in 30s, normal on next call.

---

## 4. Required Fix — OpenClawA2AExecutor Response Capture

The executor must retrieve the agent's text response from session history **after** the main session yields. The `sessions_history` CLI command (exposed as `session_history` tool) retrieves past messages.

**Proposed change** to `workspace/adapters/openclaw/adapter.py` (`execute()` method):

```python
# After proc.communicate() returns with payloads=[]:
if not reply or reply.startswith("{'payloads': []"):
    # Agent yielded without responding — fetch last message from session history
    await asyncio.sleep(2)  # brief wait for background session to complete short tasks
    hist_proc = await asyncio.create_subprocess_exec(
        "openclaw", "sessions", "history",
        "--session-id", self._session_id,
        "--limit", "1", "--json",
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
        env={**os.environ, "PATH": f"{os.path.expanduser('~/.local/bin')}:{os.environ.get('PATH', '')}"}
    )
    hist_stdout, _ = await asyncio.wait_for(hist_proc.communicate(), timeout=15)
    hist_data = json.loads(hist_stdout.decode().strip() or "{}")
    last_msg = (hist_data.get("messages") or [{}])[-1]
    reply = last_msg.get("content", reply)  # fall back to original if no history
```

**Note on long tasks (5–8 min builds):** Session history won't have the build result until it completes. For Miaoda App Builder, PM must poll: send a follow-up "What is the status of the Hello EnterOS app build?" message every 60s until the response contains a URL or error.

---

## 5. Open Questions Status

### 5-C — Rate limits
**UNKNOWN.** Never reached skill invocation.  
*New data:* Gemini AI Studio hit a schema/payload rejection after 3 rapid calls. This may be a Gemini-specific issue with large tool schemas (OpenClaw's `cron` schema is 6311 chars). Worth filing separately.

### 5-D — Failure recovery
**UNKNOWN.** Never reached app generation.

---

## 6. Issues to File

| # | Issue | Status | Location |
|---|-------|--------|----------|
| A | `fix(openclaw): use stable workspace session ID` | ✅ fixed in 9466943 | adapter.py |
| B | `fix(openclaw): extend key lookup for AISTUDIO/QIANFAN` | ✅ fixed in d779e16 | adapter.py |
| C | `fix(provisioner): surface Docker errors in last_sample_error` | ❌ open | provisioner.go |
| **D** | **`fix(openclaw): capture agent response via session history when payloads=[]`** | ❌ open — see §4 | adapter.py |
| **E** | **`fix(openclaw): Gemini rejects request after N rapid calls with large tool schema`** | ❌ open — investigate cron schema size | adapter.py |

---

## 7. Next Steps (before Run 5)

- [ ] **Dev Lead:** Implement §4 session-history fallback in `OpenClawA2AExecutor.execute()`
- [ ] **Dev Lead (optional):** Trim `cron` tool schema to reduce Gemini schema-size rejection risk
- [ ] **Operator:** Rebuild image: `bash workspace/build-all.sh openclaw`
- [ ] **PM (Run 5):** Re-run smoke test — expected to finally reach skill install confirmation
