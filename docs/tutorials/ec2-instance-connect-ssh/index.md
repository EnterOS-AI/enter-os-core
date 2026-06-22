# SSH into Cloud Agent Workspaces via EC2 Instance Connect

EC2 Instance Connect Endpoint lets you open a shell in a CP-provisioned workspace — no SSH keys, no IP hunting, no security group configuration. The platform handles the EIC call under the hood; you just click Terminal.

SSH access to a cloud agent workspace sounds like it should be simple. The instance exists in your AWS account, you have the `instance_id` — surely there's a direct path. There isn't, by default. Instance IPs change on restart, security groups need per-account rules, and long-lived SSH keys are a provenance problem the moment more than one person needs access.

AWS EC2 Instance Connect (EIC) Endpoint solves all of this. Instead of managing keys yourself, you delegate to AWS — the platform calls `aws ec2-instance-connect ssh` on your behalf, AWS pushes a short-lived key through the EIC Endpoint, and a PTY bridges straight into the Canvas Terminal tab. The access is attributable (EIC logs which principal opened the tunnel), temporary (key expires automatically), and requires no inbound security group rules (the tunnel opens outbound from the instance).

> **Prerequisites:** CP-managed workspace in your AWS account (provisioned with `controlplane` backend and `MOLECULE_ORG_ID` set). Your IAM role must have `ec2-instance-connect:SendSSHPublicKey` + `ec2-instance-connect:OpenTunnel` (condition `Role=workspace`). An EIC Endpoint must exist in the workspace VPC. See `docs/infra/workspace-terminal.md` for the one-time infra setup.

## How it works

```
Canvas (browser) ──WebSocket──► Platform (Go)
                                 │
                                 ▼ spawns
                     aws ec2-instance-connect ssh \
                       --connection-type eice \
                       --instance-id <instance_id> \
                       --os-user ec2-user \
                       -- docker exec -it <container_id> /bin/bash
                                 │
                                 ▼
                  EIC Endpoint ──► EC2 Instance (PTY bridge)
```

The platform stores the `instance_id` returned by AWS during provisioning (PR #1531). When you click Terminal, the Go handler looks up the instance, calls `aws ec2-instance-connect ssh`, and bridges the PTY to the Canvas WebSocket.

## Run it

```bash
# 1. Create a CP-managed workspace (requires controlplane backend + MOLECULE_ORG_ID)
WS=$(curl -s -X POST https://acme.moleculesai.app/workspaces \
  -H "Authorization: Bearer $ORG_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name": "prod-agent", "runtime": "hermes", "tier": 2}' \
  | jq -r '.id')

# 2. Wait for it to be running (~20-40s)
until curl -s https://acme.moleculesai.app/workspaces/$WS \
  | jq -r '.status' | grep -q ready; do sleep 5; done
echo "Workspace $WS is ready"

# 3. In Canvas: open the workspace → Terminal tab
#    The platform calls EIC on your behalf and opens a shell.
#    No SSH keys, no IP lookup — it just works.

# 4. Verify the PTY works by running a command
whoami          # should return: root (inside the container)
df -h /         # disk usage inside the workspace container
echo $MOLECULE_WS_ID  # confirm you're in the right workspace

# 5. Inspect the EIC tunnel via CloudWatch (AWS console)
#    Filter: eventName=OpenTunnel, eventSource=ec2-instance-connect
#    Principal: your IAM role ARN
#    Target: the instance_id of the workspace
```

## What you need on the AWS side

| Requirement | Details |
|---|---|
| IAM policy | `ec2-instance-connect:SendSSHPublicKey` + `ec2-instance-connect:OpenTunnel` on `*` with condition `aws:ResourceTag/Role=workspace` |
| EIC Endpoint | One per workspace VPC, reachable from the platform |
| AWS CLI | `aws-cli` + `openssh-client` installed in the tenant image (alpine: `apk add openssh-client aws-cli`) |
| Instance | Must be Nitro-based (T3, M5, C5, etc. — virtually all modern instance types) |

## Design notes

- The EIC call is a **subprocess** (`aws ec2-instance-connect ssh`) rather than a native SDK call. EIC Endpoint uses a signed WebSocket with specific framing that `aws-cli v2` implements correctly. Reimplementing it in Go is ~500 lines of crypto + protocol work.
- `sshCommandFactory` is a **var** (injectable) so tests can stub the command without spawning real aws-cli processes.
- Context cancellation is **bidirectional**: WS close kills the SSH process; SSH exit closes the WebSocket cleanly.
- If Terminal shows "EIC wiring incomplete," the EIC Endpoint or IAM policy isn't set up yet — see `docs/infra/workspace-terminal.md`.

## Teardown

Close the Terminal tab in Canvas, or the process exits automatically when the browser disconnects. No manual teardown needed.

*EC2 Instance Connect SSH shipped in PRs #1531 + #1533. For the social launch copy, see `docs/marketing/social/2026-04-22-ec2-instance-connect-ssh/`.*
