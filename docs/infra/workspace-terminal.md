# Workspace Terminal

> **Full runbook moved to the internal repo on 2026-04-22.**
>
> The implementation-level content (EIC bootstrap script output,
> per-tenant SG backfill commands, tenant-specific identifiers) now
> lives at **`Molecule-AI/internal/runbooks/workspace-terminal.md`**
> (private — Molecule AI org members only).

## What this feature is (public summary)

The canvas Terminal tab opens an interactive shell on a workspace's
compute — locally this is a `docker exec` into the container; in the
SaaS tenant path it's an SSH session into the tenant EC2 (or the
workspace container running on it) over an [EC2 Instance Connect
Endpoint](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-connect-setup-ec2-instance-connect-endpoint.html).
End users see a terminal; no direct public SSH ingress is required.

Tracking: originally `molecule-core#1528` (resolved 2026-04-22). The
`molecule-core` repo has since been renamed to `molecule-monorepo` and no
longer accepts new issues under the old name; future terminal work is
tracked in `molecule-monorepo` issues (workspace-server scope) and in
`molecule-controlplane` issues for the EIC / per-tenant SG path.

## Where things are

- **Go handler:** [`workspace-server/internal/handlers/terminal.go`](../../workspace-server/internal/handlers/terminal.go)
- **CP provisioner (EIC endpoint, per-tenant SG):** `Molecule-AI/molecule-controlplane/internal/provisioner/ec2.go` — `EICEndpointSGID` field
- **Bootstrap script:** `Molecule-AI/molecule-controlplane/scripts/bootstrap-eic-terminal.sh`
- **Detailed ops runbook (internal):** `Molecule-AI/internal/runbooks/workspace-terminal.md`

Why the split: the bootstrap-script output + per-tenant SG ingress
backfill commands include AWS resource IDs and tenant slugs that
don't belong in a public repo, but the high-level design is useful
for external readers + self-hosters.
