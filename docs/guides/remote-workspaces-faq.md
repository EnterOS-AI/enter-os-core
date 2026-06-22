# Phase 30 Remote Workspaces — Customer FAQ

> **Cycle:** Marketing work cycle — offline content prep
> **Status:** Draft — needs review from Marketing Lead and Doc Specialist before publishing

Top customer and sales-engineer questions about Phase 30 Remote Workspaces, answered in a format ready to drop into the docs site or adapt for the support team.

---

## Product & Architecture

**Q: What's the difference between a "container" workspace and a "remote" workspace?**

A container workspace runs inside the EnterOS platform's infrastructure — fully managed, no SSH, no git. A remote workspace runs on your own machine or VM, connected to the platform via a lightweight agent. You control the environment (OS, packages, git config, SSH keys); the platform handles orchestration, authentication, and agent coordination.

**Q: Do remote workspaces still appear in the Canvas UI?**

Yes. Remote workspaces register with the platform on startup and appear in Canvas exactly like managed workspaces — online/offline status, workspace name, current task. The platform doesn't care where the agent runs, only that it's reachable.

**Q: Can I run both container and remote workspaces in the same org?**

Yes — in fact that's the primary pattern. A fleet might have 5 container workspaces for ephemeral tasks and 2 remote workspaces for long-running agents with persistent state. All of them show up in Canvas and can coordinate via A2A.

**Q: What does the remote runtime actually install on my machine?**

The agent binary (~30MB) plus a minimal bootstrap script. No root required. The agent connects to `wss://[your-org].moleculesai.app`, authenticates with your org token, and registers its A2A endpoint. That's it — no VPN, no firewall holes beyond outbound HTTPS.

---

## Security & Access Control

**Q: How does the platform authenticate a remote workspace?**

Remote workspaces authenticate with an org-scoped bearer token (not a personal token). The platform validates the token against the tenant and provisions a session-scoped credential for A2A communication. If the remote machine is revoked from the org, the token is invalidated and the workspace goes offline within one heartbeat cycle (~15s).

**Q: Can a remote workspace make outbound connections my firewall would block?**

The agent only makes outbound HTTPS/WSS connections to the platform. It does not accept inbound connections. Your firewall only needs to allow `*.moleculesai.app` outbound — same as a browser.

**Q: What happens to data if the remote workspace is disconnected or the machine is wiped?**

Workspace state lives in the platform unless explicitly persisted. For remote workspaces, you can attach a Cloudflare Artifacts repo to snapshot state to disk on your own infrastructure. If the agent reconnects, it re-registers and Canvas picks up where it left off.

**Q: Are remote workspaces covered by the same MCP governance controls as container workspaces?**

Yes. MCP plugin allowlists, org API key auditing, and workspace-level audit logs all apply to remote workspaces identically. The remote runtime is a transport layer — the platform's security model sits above it.

---

## Onboarding & Operations

**Q: How do I get started with a remote workspace?**

1. Install the agent: `curl -sSL https://get.moleculesai.app | bash`
2. Authenticate: `molecule login --org your-org`
3. Bootstrap: `molecule workspace init --name my-agent --runtime remote`
4. The workspace registers with the platform and appears in Canvas within ~10 seconds.

**Q: Can I use my existing SSH keys and git config with a remote workspace?**

Yes. The remote runtime does not virtualize or override your shell environment. SSH keys, git config, dotfiles — all persist across sessions and are available to the agent.

**Q: How do I update the remote agent when a new version ships?**

`molecule update` — pulls the latest agent binary from the platform, does a rolling restart. Zero downtime if the agent reconnects within the heartbeat window.

**Q: What's the latency like for A2A coordination between a remote workspace and a container workspace?**

A2A messages route through the platform's relay, so latency is essentially internet RTT between the remote machine and the platform's edge (~20–80ms depending on geography). For comparison, container workspaces on-platform have <5ms RTT. The practical difference for most coordination patterns is imperceptible.

**Q: Can I run a remote workspace on a machine that's behind NAT with no public IP?**

Yes. The agent initiates the outbound WebSocket connection to the platform — no inbound ports needed. This is the primary design reason remote workspaces use WSS rather than HTTP.

---

## Pricing & Limits

**Q: Do remote workspaces count toward my workspace limit?**

Yes. The workspace count limit is platform-wide regardless of runtime type. Remote workspaces are still platform workspaces — they just run externally. If you're at your limit, you can archive old workspaces or request an increase.

**Q: Is there a different price for remote vs. container workspaces?**

At launch, remote workspaces are priced identically to container workspaces. Future tiers may differentiate based on egress or storage, but that's not in the current release.

**Q: What's the maximum concurrent task throughput for a single remote workspace?**

Same as a container workspace — up to 5 concurrent delegated tasks. Remote runtime adds no throughput cap.

---

## Troubleshooting

**Q: Remote workspace shows offline in Canvas but the process is running on my machine.**

1. Check the agent log: `molecule logs --workspace my-agent`
2. Confirm the machine has outbound internet access: `curl -s https://[your-org].moleculesai.app/health`
3. Check token validity: `molecule auth status` — re-authenticate if expired
4. Restart the agent: `molecule restart --workspace my-agent`

**Q: A2A messages to my remote workspace are timing out.**

Remote workspaces must maintain the outbound WebSocket connection. If the machine sleeps or loses connectivity, the connection drops and A2A messages queue for up to 5 minutes before failing. The agent will re-register on reconnect — Canvas will show it back online.

**Q: My remote workspace is online but can't reach internal APIs.**

The remote runtime does not inherit VPN credentials from the machine by default. If internal APIs require VPN, you'll need to either configure the VPN on the host machine outside the agent, or use the platform's `/cp/*` reverse proxy for same-origin access (same-origin-canvas-fetches.md).

---

## Competitive

**Q: How is this different from connecting to a cloud IDE like Cursor or Copilot?**

Cursor and Copilot are individual developer tools. EnterOS is an agent orchestration platform. Remote workspaces are about running autonomous agents that coordinate with each other — not just one human and one AI pairing. The multi-agent coordination layer (A2A, Canvas, org-scoped auth) is what distinguishes the platform.

**Q: How does this compare to running agents on Modal or Railway?**

Modal and Railway are inference platforms — they run your code on their infrastructure. EnterOS remote workspaces run on *your* infrastructure. You own the compute, the data stays on your machine, and the platform handles coordination. For regulated industries or workloads with data residency requirements, this is a different category entirely.

---

*Needs review from: Marketing Lead (voice + accuracy), Doc Specialist (technical accuracy), possibly Support for the troubleshooting section.*
