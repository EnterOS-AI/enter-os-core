# Chrome DevTools MCP Quickstart

> **Prerequisites:** A Molecule AI workspace with an active agent · MCP bridge configured · Chrome DevTools MCP server installed
> **Runtime:** `claude-code` (or any MCP-compatible runtime)
> **Related:** [MCP Server Setup Guide](../guides/mcp-server-setup) · [Org API Keys](../guides/org-api-keys) · [Chrome DevTools MCP blog post](../blog/chrome-devtools-mcp)

---

## What You Get

Chrome DevTools MCP adds browser control tools to any MCP-compatible agent. Once configured, the agent can:

- **Navigate** pages via URL
- **Screenshot** any viewport — full-page or element-specific
- **Read** DOM content, cookies, network requests
- **Evaluate** JavaScript — run snippets or full scripts inside the page
- **Fill** forms, click elements, submit

Combined with Molecule AI's governance layer, every action is logged with your workspace token and org API key prefix. You can revoke, audit, and trace everything.

---

## Setup

### 1. Install the Chrome DevTools MCP server

```bash
npm install -g @modelcontextprotocol/server-chrome-devtools
```

### 2. Add to your workspace's MCP config

Edit `.mcp.json` in your project:

```json
{
  "mcpServers": {
    "chrome-devtools": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-chrome-devtools"]
    }
  }
}
```

Or use Molecule AI's canvas MCP bridge (recommended for platform deployments — gives you plugin allowlisting and security scanning):

```json
{
  "mcpServers": {
    "molecule": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@molecule-ai/mcp-server"]
    },
    "chrome-devtools": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-chrome-devtools"]
    }
  }
}
```

### 3. Verify the tools are available

Ask your agent:
```
What Chrome DevTools MCP tools are available?
```

Expected response — the agent should see: `browser_navigate`, `browser_screenshot`, `browser_evaluate`, `browser_dom_snapshot`, etc.

---

## Demo 1: Screenshot-Based Visual Regression

The agent navigates a staging URL, takes a screenshot, and compares it to a baseline. If the diff crosses a pixel threshold, it opens a ticket.

### Running the demo

```
Open https://staging.yourapp.com/dashboard and take a full-page screenshot.
```

Agent response (example):
```
Navigated to staging.yourapp.com/dashboard
Full-page screenshot saved to /tmp/screenshot-2026-04-22.png
Diff against baseline: 0.3% — within tolerance (threshold: 1%)
No regression detected.
```

### How it works (code equivalent)

```python
import subprocess, base64, json

def screenshot_page(url: str, path: str) -> str:
    """Use CDP via the MCP server to take a screenshot."""
    # The MCP tool definition maps to:
    # browser_navigate(url=url)
    # browser_screenshot(full_page=true)
    result = subprocess.run([
        "npx", "-y", "@modelcontextprotocol/server-chrome-devtools",
        "--tool", "screenshot",
        "--url", url
    ], capture_output=True, text=True)
    img_data = base64.b64decode(result.stdout)
    with open(path, "wb") as f:
        f.write(img_data)
    return path

# Baseline is stored in workspace files
BASELINE = "/workspace/.visual-baselines/dashboard.png"
CURRENT = screenshot_page("https://staging.yourapp.com/dashboard", "/tmp/current.png")
# Compare using imagehash or pixel-diff tool
```

### Governance notes

- Each screenshot action is logged as `browser_navigate + browser_screenshot` in the workspace activity log
- The org API key prefix (`ci-pipeline-key`, `qa-agent`, etc.) appears in the audit trail
- Revoke the key → agent's browser sessions close within 30 seconds

---

## Demo 2: Authenticated Session Scraping

Attach an existing logged-in session cookie to the agent's browser context, then let the agent navigate and extract data from behind the login wall.

### Setup

1. Store the session cookie as a workspace secret:
   ```
   set_secret key="session_cookie" value=" SID=sid_value_here; Domain=.yourapp.com; Path=/; HttpOnly "
   ```

2. Configure the browser to use that cookie on a target domain:

   ```
   Set cookie domain to yourapp.com with the session_cookie secret value
   Navigate to https://yourapp.com/admin/reports
   Read the table rows from .report-table and return them as JSON
   ```

3. Agent navigates, reads DOM, returns structured data:

   ```json
   [
     { "date": "2026-04-21", "users": 1423, "conversions": 87 },
     { "date": "2026-04-22", "users": 1389, "conversions": 91 }
   ]
   ```

### Security properties

| Property | How Molecule AI handles it |
|---|---|
| Credential isolation | Session cookie stored as a workspace secret — not in env vars or source code |
| Agent scope | Agent B cannot read Agent A's session — browser context is workspace-scoped |
| Revocation | Delete the workspace secret → next heartbeat the agent picks up the deletion |
| Audit | Every navigation + DOM read logged with org API key prefix + workspace ID |

> **SSRF protection:** The browser context only loads `https://` URLs. `http://`, `file://`, and internal ranges (cloud metadata IPs, link-local) are blocked by the platform router before the CDP request executes. This is enforced in `workspace-server/internal/handlers/chrome_devtools.go`.

---

## Demo 3: Automated Lighthouse Audit on Every PR

The agent runs a Lighthouse audit against your staging environment, reports the score to the PR, and flags regressions if the score drops below your threshold.

### Prompt to the agent

```
Run a Lighthouse performance audit against https://staging.yourapp.com.
Report: Performance score, FCP, LCP, CLS, TBT.
If Performance < 70, open a GitHub issue on molecule-core with the label "performance-regression" and assign @your-team.
```

### Expected output (success case)

```
Lighthouse audit against staging.yourapp.com:
  Performance: 84
  FCP: 1.2s | LCP: 2.1s | CLS: 0.03 | TBT: 180ms

Score above threshold (70). No regression.
Audit log: org-token:ci-pipeline-key → POST /workspaces/ws-dev-01/transcript
```

### Expected output (regression case)

```
Lighthouse audit against staging.yourapp.com:
  Performance: 61 ⚠️ BELOW THRESHOLD
  FCP: 2.4s | LCP: 5.2s | CLS: 0.18 | TBT: 620ms

Performance regression detected — opening GitHub issue.
Issue: https://git.moleculesai.app/molecule-ai/molecule-core/issues/1527
Label: performance-regression | Assignees: @your-team
```

### How the agent runs the audit

```javascript
// browser_evaluate runs this inside the page
const url = arguments[0];
const lighthouse = require('lighthouse');
const report = await lighthouse(url, {
  onlyCategories: ['performance'],
  settings: { onlyAudits: ['performance'] }
});
const score = report.lhr.categories.performance.score * 100;
// → 84
```

---

## Governance Configuration

### Enable plugin allowlisting (canvas)

1. **Settings → Security → Plugin Allowlist**
2. Add `molecule-security-scan` as a pre-install reviewer
3. Chrome DevTools MCP will surface its tool definitions for admin approval before the agent boots

This means: before the agent can `browser_navigate` anywhere, your org's admin sees the permission request and approves it once.

### Set token-scoped session limits

```
POST /workspaces/:id/config
{
  "browserSessionScope": "token",
  "sessionTimeoutSeconds": 3600,
  "allowedDomains": ["staging.yourapp.com", "yourapp.com"]
}
```

- `browserSessionScope: "token"` — each org API key gets its own browser session. Key A and Key B cannot see each other's cookies.
- `sessionTimeoutSeconds` — auto-close the browser after N seconds of inactivity
- `allowedDomains` — block navigation to domains outside your control

---

## Full End-to-End Workflow

```
1. You assign org API key "ci-pipeline-key" to the CI agent workspace
2. CI agent boots → Chrome DevTools MCP connects → admin approves via canvas
3. On PR open: CI agent navigates staging URL, runs Lighthouse, reports score
4. On regression: CI agent opens GitHub issue with audit results
5. Audit log shows: org-token:ci-pipeline-key → browser_navigate → browser_evaluate → POST /issues
6. If key is compromised: DELETE /org/tokens/ci-pipeline-key → 401 on next heartbeat
```

---

## Troubleshooting

| Issue | Fix |
|---|---|
| Agent says "no browser available" | Install `@modelcontextprotocol/server-chrome-devtools` · check `.mcp.json` |
| Navigation blocked (403/redirect) | Check `allowedDomains` in workspace config · verify org API key has access |
| Cookie not persisting | Store cookie as workspace secret (not env var) · use `set_secret` before session start |
| Screenshot blank | Page may be SPA — add `wait_for_selector` before screenshot |
| Org API key returns 401 | Key revoked or expired · mint a new one via Canvas → Settings → Org API Keys |

---

## Code Reference

| File | Description |
|---|---|
| `workspace-server/internal/handlers/chrome_devtools.go` | Chrome DevTools MCP handler, SSRF validation, session scoping |
| `workspace-server/internal/handlers/mcp_tools.go` | MCP tool registry and routing |
| `canvas/src/components/workspace/MCPSettings.tsx` | Canvas MCP plugin allowlist UI |
| `docs/guides/mcp-server-setup.md` | Full MCP tool reference |
| `docs/blog/2026-04-20-chrome-devtools-mcp/index.md` | Governance layer deep-dive |

---

*Quickstart prepared by DevRel Engineer. MCP bridge and plugin allowlisting managed via Molecule AI canvas.*