# Organization API Keys — User Guide

> Full-admin API keys for your EnterOS organization. Use these to
> let AI agents, scripts, or integrations manage your org without a
> browser session.

## TL;DR

1. Open your org's canvas UI (`https://<your-slug>.moleculesai.app`)
2. Settings (⌘,) → **Org API Keys** tab
3. Click **New Key**, give it a label (e.g. "zapier", "my-claude-agent")
4. **Copy the token immediately** — it will never be shown again
5. Hand it to whatever needs org-admin access:
   ```
   Authorization: Bearer <your-token>
   ```

Revoke from the same UI the moment anything looks wrong.

## What these keys can do

**Full organization admin.** A valid org API key is equivalent to
being logged in as an admin user. With it, a script or AI can:

- Create, delete, list workspaces
- Import a complete org definition (can wipe + recreate everything)
- Manage per-workspace secrets (your OpenAI/Anthropic/etc. keys)
- Register + install templates, bundles, plugins
- Approve or reject pending workspace approvals
- Configure channels (Slack, Discord, etc.)
- Mint more org API keys
- Revoke any org API key (including itself)

**What they cannot do:**

- Reach the control plane's admin API (`/cp/admin/*`) — CP admin
  lives on a separate allowlist.
- Touch other organizations — each org's keys work only on its own
  tenant.
- Edit the tenant's environment variables or restart the underlying
  EC2 instance — those are ops-only operations.

## Treat keys like passwords

- **Don't** commit keys to git. If you must have one in source,
  reference an env var and keep the var in your secret manager.
- **Don't** paste keys into Slack or email. Share via a password
  manager when you can.
- **Do** give each integration its own key with a descriptive name.
  If Zapier gets compromised, you revoke `zapier` and leave
  `github-action-deploy` untouched.
- **Do** revoke any key you stop using.

If you leak one, revoke it and mint a new one. Revocation is
immediate — the next request with the old key gets 401.

## Using a key

### curl

```bash
curl -H "Authorization: Bearer $MOLECULE_ORG_TOKEN" \
  https://acme.moleculesai.app/workspaces
```

### Python

```python
import os, requests

resp = requests.get(
    "https://acme.moleculesai.app/workspaces",
    headers={"Authorization": f"Bearer {os.environ['MOLECULE_ORG_TOKEN']}"},
)
resp.raise_for_status()
print(resp.json())
```

### TypeScript / Node

```ts
const resp = await fetch("https://acme.moleculesai.app/workspaces", {
  headers: { Authorization: `Bearer ${process.env.MOLECULE_ORG_TOKEN}` },
});
if (!resp.ok) throw new Error(`${resp.status}: ${await resp.text()}`);
console.log(await resp.json());
```

### Hand it to an AI agent

Add the key to the agent's environment or config, with clear
instructions about what routes it should touch. Claude Code, for
example, can use it to inspect the tenant's state programmatically:

```bash
export MOLECULE_ORG_TOKEN=...   # the key you just minted
```

Then tell the agent: "Using MOLECULE_ORG_TOKEN, list my workspaces
and tell me which ones are idle."

## Endpoints you'll hit most often

| Method | Path | What it does |
|---|---|---|
| GET | `/workspaces` | list all workspaces |
| POST | `/workspaces` | create a workspace |
| DELETE | `/workspaces/:id` | delete a workspace |
| GET | `/org/templates` | list registered templates |
| POST | `/org/import` | import a full org YAML |
| POST | `/bundles/import` | install a bundle |
| GET | `/approvals/pending` | list pending approvals |

Each workspace you create gets its own workspace-scoped token
returned in the create response. Use that token (not the org key)
for agent-to-platform calls inside that specific workspace — it
has a narrower blast radius if leaked.

Full API reference: `docs/api-reference.md`.

## Keys vs session cookies

| | Org API Key | WorkOS session cookie |
|---|---|---|
| Who holds it | Integrations, AI, CLI | Your browser |
| Where you see it | `/org/tokens` UI | Browser cookies |
| Revocation | One-click in UI | Log out / session expiry |
| Use from code | Yes | No (HttpOnly) |
| Blast radius | Full org admin | Full org admin |

Both unlock the same surface; the key is just the non-browser
equivalent.

## Current limits

Every key is full-admin. Scoped roles (read-only / workspace-
write / admin), per-workspace bindings, and expiry are not yet
supported — treat every key as equivalent to being logged in.
